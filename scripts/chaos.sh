#!/bin/sh
# Fault injection, three scenarios. Each one breaks a dependency on a
# running stack and checks that the system degrades visibly and recovers
# without manual repair.
#
#   1. database stopped     -> /health 503, requests fail fast, recovers on start
#   2. API process killed   -> restarted container keeps every open cart
#                              (state lives in PostgreSQL, not in memory)
#   3. network partition    -> DB unreachable from the API, then reconnected
#
# Needs: docker compose, curl. Exits non-zero on the first failed check.
set -eu
API=${API:-http://localhost:8080}
COMPOSE=${COMPOSE:-docker compose}
PW='Demo1234!'

fail() { echo "FAIL: $*"; exit 1; }
health() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$API/health" || true; }
wait_for() { # wait_for <code> <seconds>
  i=0
  while [ "$i" -lt "$2" ]; do
    [ "$(health)" = "$1" ] && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}
json() { # json <field> : first string or number value of a field in stdin
  sed -n "s/.*\"$1\":\"\{0,1\}\([^\",}]*\).*/\1/p" | head -1
}
login() {
  curl -s --max-time 10 -X POST "$API/api/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$1\",\"password\":\"$PW\"}" | json access_token
}
timed_login() { # prints "<http code> <seconds>"
  curl -s -o /dev/null -w '%{http_code} %{time_total}' --max-time 20 -X POST "$API/api/auth/login" \
    -H 'Content-Type: application/json' -d "{\"email\":\"musteri@sahnebilet.test\",\"password\":\"$PW\"}" || echo "000 20"
}

$COMPOSE up -d --wait db api
[ "$(health)" = 200 ] || fail "API not healthy at start"
echo "baseline: /health = 200"

# --------------------------------------------------------------------------
echo
echo "== 1) database stopped"
$COMPOSE stop db >/dev/null
wait_for 503 30 || fail "API did not report the database outage"
echo "   /health = 503 (degraded, detected)"
set -- $(timed_login)
echo "   login during the outage -> HTTP $1 in ${2}s (fails fast, does not hang)"
[ "$1" != 200 ] || fail "login succeeded without a database"
$COMPOSE start db >/dev/null
wait_for 200 60 || fail "API did not recover after the database came back"
set -- $(timed_login)
[ "$1" = 200 ] || fail "login after recovery -> $1"
echo "   database back: /health = 200, login 200, no API restart needed"

# --------------------------------------------------------------------------
echo
echo "== 2) API process killed with an open cart"
TOKEN=$(login yuk199@sahnebilet.test)
[ -n "$TOKEN" ] || fail "login for scenario 2"
FROM=$(date -u -d '+2 day' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v+2d +%Y-%m-%dT%H:%M:%SZ)
SHOW=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/api/showtimes?limit=1&from=$FROM" | json id)
[ -n "$SHOW" ] || fail "no showtime"
SEATS=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/api/showtimes/$SHOW/suggest?n=2" | sed -n 's/.*"seat_ids":\(\[[0-9,]*\]\).*/\1/p' | head -1)
HOLD=$(curl -s -X POST "$API/api/holds" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: chaos-$(date +%s)-$$" -d "{\"showtime_id\":$SHOW,\"seat_ids\":$SEATS}" | json id)
[ -n "$HOLD" ] || fail "could not open a cart"
echo "   cart $HOLD holds seats $SEATS of showtime $SHOW"
$COMPOSE kill -s SIGKILL api >/dev/null
[ "$(health)" != 200 ] || fail "API still answering after SIGKILL"
echo "   API killed (SIGKILL, no graceful shutdown): /health = $(health)"
$COMPOSE up -d api >/dev/null   # what the orchestrator / restart policy does
wait_for 200 60 || fail "API did not come back"
STATE=$(curl -s -H "Authorization: Bearer $TOKEN" "$API/api/holds/$HOLD")
echo "$STATE" | grep -q '"status":"active"' || fail "cart lost after the crash: $STATE"
SEATS_AFTER=$(echo "$STATE" | sed -n 's/.*"seat_ids":\(\[[0-9,]*\]\).*/\1/p')
[ "$SEATS_AFTER" = "$SEATS" ] || fail "cart seats changed: $SEATS -> $SEATS_AFTER"
echo "   API back: cart still active with the same seats $SEATS_AFTER (the old token still works)"
curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" "$API/api/holds/$HOLD"

# --------------------------------------------------------------------------
echo
echo "== 3) network partition between API and database"
DB_ID=$($COMPOSE ps -q db)
NET=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$DB_ID" | awk '{print $1}')
docker network disconnect "$NET" "$DB_ID"
wait_for 503 30 || { docker network connect --alias db "$NET" "$DB_ID"; fail "API did not notice the partition"; }
echo "   database unreachable: /health = 503"
set -- $(timed_login)
echo "   login during the partition -> HTTP $1 in ${2}s (bounded by the request timeout)"
[ "$1" != 200 ] || { docker network connect --alias db "$NET" "$DB_ID"; fail "login succeeded during the partition"; }
docker network connect --alias db "$NET" "$DB_ID"
wait_for 200 60 || fail "API did not recover after the network came back"
set -- $(timed_login)
[ "$1" = 200 ] || fail "login after the partition -> $1"
echo "   network back: /health = 200, login 200, pool reconnected on its own"

echo
echo "PASS: chaos scenarios (database stop, API crash, network partition)"
