#!/bin/sh
# Fault injection: stop PostgreSQL under traffic, show that the API reports
# itself degraded (503 on /health, errors instead of hangs), start the
# database again and show that the API recovers without a restart.
set -eu
API=${API:-http://localhost:8080}
COMPOSE=${COMPOSE:-docker compose}

health() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$API/health" || echo 000; }
wait_for() { # wait_for <code> <seconds>
  i=0
  while [ "$i" -lt "$2" ]; do
    [ "$(health)" = "$1" ] && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}

$COMPOSE up -d --wait db api
echo "1) baseline: /health = $(health)"
[ "$(health)" = 200 ] || { echo "FAIL: API not healthy at start"; exit 1; }

echo "2) stopping the database"
$COMPOSE stop db >/dev/null
if wait_for 503 30; then echo "   /health = 503 (degraded, detected)"; else echo "FAIL: API did not report the outage"; exit 1; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 -X POST "$API/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"musteri@sahnebilet.test","password":"Demo1234!"}' || echo 000)
echo "   login during outage -> HTTP $code (fails fast, no hang)"

echo "3) starting the database again"
$COMPOSE start db >/dev/null
if wait_for 200 60; then echo "   /health = 200 (recovered without restarting the API)"; else echo "FAIL: API did not recover"; exit 1; fi
code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$API/api/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"musteri@sahnebilet.test","password":"Demo1234!"}')
[ "$code" = 200 ] || { echo "FAIL: login after recovery -> $code"; exit 1; }
echo "   login after recovery -> HTTP 200"
echo "PASS: chaos scenario"
