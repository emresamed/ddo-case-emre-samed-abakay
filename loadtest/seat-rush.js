// Premiere-morning rush (T-1): VUS virtual customers open the same showtime
// at the same moment, each picks 1-4 seats near the centre, holds them and
// buys. Afterwards the integrity endpoint must report no double sale.
//
//   make load                     # 2000 VUs (K6_VUS overrides)
import http from 'k6/http';
import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const API = __ENV.API_URL || 'http://localhost:8080';
const VUS = Number(__ENV.VUS || 2000);
const USERS = 200; // seeded customers yuk001..yuk200

const holdOK = new Counter('hold_ok');
const holdConflict = new Counter('hold_conflict');
const holdOther = new Counter('hold_other');
const orderOK = new Counter('order_ok');
const holdLatency = new Trend('hold_latency', true);

export const options = {
  scenarios: {
    rush: { executor: 'per-vu-iterations', vus: VUS, iterations: 1, maxDuration: '5m' },
  },
  thresholds: {
    // the only acceptable failures are "seat taken" conflicts
    hold_other: ['count==0'],
    'http_req_duration{name:hold}': ['p(95)<3000'],
  },
  setupTimeout: '3m',
};

const json = (body, token, key) => ({
  headers: Object.assign({ 'Content-Type': 'application/json' },
    token ? { Authorization: 'Bearer ' + token } : {}, key ? { 'Idempotency-Key': key } : {}),
});

export function setup() {
  const tokens = [];
  for (let i = 1; i <= USERS; i++) {
    const email = `yuk${String(i).padStart(3, '0')}@sahnebilet.test`;
    const r = http.post(`${API}/api/auth/login`, JSON.stringify({ email, password: 'Demo1234!' }), json());
    check(r, { 'login 200': (x) => x.status === 200 });
    tokens.push(r.json('access_token'));
  }
  const mgr = http.post(`${API}/api/auth/login`, JSON.stringify({ email: 'amir@sahnebilet.test', password: 'Demo1234!' }), json()).json('access_token');
  // the biggest hall, a show at least a day ahead with nothing sold yet
  const from = new Date(Date.now() + 864e5).toISOString();
  const to = new Date(Date.now() + 10 * 864e5).toISOString();
  const list = http.get(`${API}/api/showtimes?limit=200&hall_id=1&from=${from}&to=${to}`, json(null, tokens[0])).json('items');
  const show = list.find((s) => s.taken === 0) || list[0];
  const map = http.get(`${API}/api/showtimes/${show.id}/seats`, json(null, tokens[0])).json();
  const seats = map.seats.filter((s) => s.kind === 'standard' || s.kind === 'vip');
  console.log(`rush on showtime ${show.id} (${show.movie_title}, ${show.hall_name}), ${seats.length} sellable seats, ${VUS} customers`);
  return { tokens, mgr, show: show.id, seats };
}

export default function (data) {
  const token = data.tokens[(__VU - 1) % data.tokens.length];
  // everybody wants the middle: pick a random centre-weighted block
  const rows = [...new Set(data.seats.map((s) => s.row))];
  const row = rows[Math.min(rows.length - 1, Math.floor(rows.length * 0.4 + Math.random() * rows.length * 0.4))];
  const inRow = data.seats.filter((s) => s.row === row).sort((a, b) => a.col - b.col);
  const n = 1 + Math.floor(Math.random() * 4);
  const start = Math.floor(Math.random() * Math.max(1, inRow.length - n));
  const ids = inRow.slice(start, start + n).map((s) => s.id);

  const key = `k6-${__VU}-${__ITER}-${Date.now()}`;
  const r = http.post(`${API}/api/holds`, JSON.stringify({ showtime_id: data.show, seat_ids: ids }), Object.assign(json(null, token, key), { tags: { name: 'hold' } }));
  holdLatency.add(r.timings.duration);
  if (r.status === 201) {
    holdOK.add(1);
    const o = http.post(`${API}/api/orders`, JSON.stringify({ hold_id: r.json('id') }), Object.assign(json(null, token, key + '-o'), { tags: { name: 'order' } }));
    if (check(o, { 'order 201': (x) => x.status === 201 })) orderOK.add(1);
  } else if (r.status === 409 || r.status === 422) {
    holdConflict.add(1); // seat taken, busy, too many carts or single-seat rule
  } else {
    holdOther.add(1);
    console.error(`unexpected hold response ${r.status} ${r.body}`);
  }
}

export function teardown(data) {
  const r = http.get(`${API}/api/showtimes/${data.show}/integrity`, json(null, data.mgr));
  console.log(`integrity: ${r.body}`);
  check(r, { 'no double sale': (x) => x.json('ok') === true && x.json('duplicate_seats') === 0 });
}
