// Frontend unit tests: node --test frontend/test (Node 20+, no dependencies).
'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { webcrypto } = require('node:crypto');

globalThis.crypto ??= webcrypto;
const lib = require('../public/lib.js');

test('esc escapes HTML so seat labels and titles cannot inject markup', () => {
  assert.equal(lib.esc('<img src=x onerror="alert(1)">'), '&lt;img src=x onerror=&quot;alert(1)&quot;&gt;');
  assert.equal(lib.esc("O'Neil & Co"), 'O&#39;Neil &amp; Co');
  assert.equal(lib.esc(null), '');
  assert.equal(lib.esc(undefined), '');
});

test('tl formats kuruş as Turkish lira', () => {
  const s = lib.tl(1234550);
  assert.match(s, /12\.345,50/);
  assert.match(s, /₺|TRY/);
  assert.match(lib.tl(0), /0,00/);
});

test('errText prefers the Turkish message for known API codes', () => {
  assert.match(lib.errText({ code: 'SEAT_TAKEN', message: 'x' }), /başka biri/);
  assert.equal(lib.errText({ code: 'SOMETHING_NEW', message: 'raw message' }), 'raw message');
  assert.equal(lib.errText({}), 'Beklenmeyen hata');
  assert.equal(lib.st('partially_refunded'), 'kısmi iade');
  assert.equal(lib.st('unknown'), 'unknown');
});

test('occupancy reads in words: free seats, last seats, sold out', () => {
  const plenty = lib.occupancy(120, 120, 0);
  assert.match(plenty, /120 koltuk boş/);
  assert.match(plenty, /henüz satış yok/);
  assert.doesNotMatch(plenty, /occ (low|full)/);

  const some = lib.occupancy(100, 200, 50);
  assert.match(some, /100 koltuk boş/);
  assert.match(some, /%50 dolu · 200 koltuk/);

  const last = lib.occupancy(8, 200, 96);
  assert.match(last, /Son 8 koltuk/);
  assert.match(last, /occ low/);

  const full = lib.occupancy(0, 120, 100);
  assert.match(full, /Tükendi/);
  assert.match(full, /occ full/);
  assert.match(full, /width:100%/);
});

test('occupancy uses 15% of the hall (at least 10 seats) as the "last seats" threshold', () => {
  assert.match(lib.occupancy(10, 40, 75), /Son 10 koltuk/); // small hall: floor of 10
  assert.match(lib.occupancy(11, 40, 72), /11 koltuk boş/);
  assert.match(lib.occupancy(42, 280, 85), /Son 42 koltuk/); // 15% of 280
  assert.match(lib.occupancy(43, 280, 84), /43 koltuk boş/);
});

test('b64u decodes base64url without padding', () => {
  const bytes = lib.b64u('SGVsbG8_-w');
  assert.deepEqual([...bytes], [72, 101, 108, 108, 111, 63, 251]);
});

test('icsFor builds a calendar event with a reminder', () => {
  const ics = lib.icsFor({ ticket_id: 't1', movie_title: 'Son Vapur', seat: 'F-8', hall_name: 'Salon 3',
    starts_at: '2026-10-30T15:00:00Z', ends_at: '2026-10-30T17:00:00Z' });
  assert.match(ics, /^BEGIN:VCALENDAR\r\n/);
  assert.match(ics, /DTSTART:20261030T150000Z/);
  assert.match(ics, /DTEND:20261030T170000Z/);
  assert.match(ics, /SUMMARY:Son Vapur \(F-8\)/);
  assert.match(ics, /TRIGGER:-PT1H/);
  assert.match(ics, /END:VCALENDAR$/);
});

// --- offline gate: same QR format as the Go signer (SB1.<payload>.<sig>)

const enc = (bytes) => Buffer.from(bytes).toString('base64url');

async function keyPair() {
  const kp = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']);
  const raw = await crypto.subtle.exportKey('raw', kp.publicKey);
  const pub = await crypto.subtle.importKey('raw', raw, { name: 'Ed25519' }, false, ['verify']);
  return { priv: kp.privateKey, pub };
}

async function sign(priv, claims) {
  const payload = enc(new TextEncoder().encode(JSON.stringify(claims)));
  const sig = await crypto.subtle.sign({ name: 'Ed25519' }, priv, new TextEncoder().encode('SB1.' + payload));
  return `SB1.${payload}.${enc(new Uint8Array(sig))}`;
}

const now = 1_800_000_000;
const claims = { t: 'ticket-1', s: 7, l: 'F-8', nb: now - 3600, na: now + 3600 };

test('offline check accepts a valid ticket once the lists allow it', async () => {
  const { priv, pub } = await keyPair();
  const code = await sign(priv, claims);
  const r = await lib.checkTicketOffline(code, pub, { nowSec: now });
  assert.equal(r.result, 'ACCEPTED');
  assert.equal(r.claims.l, 'F-8');
  assert.equal((await lib.checkTicketOffline(code, pub, { nowSec: now, used: ['ticket-1'] })).result, 'REJECTED_ALREADY_USED');
  assert.equal((await lib.checkTicketOffline(code, pub, { nowSec: now, revoked: ['ticket-1'] })).result, 'REJECTED_REFUNDED');
});

test('offline check rejects forged, tampered and malformed codes', async () => {
  const { priv, pub } = await keyPair();
  const other = await keyPair();
  const code = await sign(priv, claims);
  assert.equal((await lib.checkTicketOffline(code, other.pub, { nowSec: now })).result, 'REJECTED_SIGNATURE');

  const forged = await sign(priv, { ...claims, l: 'A-1' });
  const [p0, , p2] = code.split('.');
  const swapped = `${p0}.${forged.split('.')[1]}.${p2}`;
  assert.equal((await lib.checkTicketOffline(swapped, pub, { nowSec: now })).result, 'REJECTED_SIGNATURE');

  for (const bad of ['', 'SB1.x', 'XX.a.b', 'SB1.!!.??', 'garbage']) {
    assert.equal((await lib.checkTicketOffline(bad, pub, { nowSec: now })).result, 'REJECTED_MALFORMED', bad);
  }
});

test('offline check enforces the validity window', async () => {
  const { priv, pub } = await keyPair();
  const code = await sign(priv, claims);
  assert.equal((await lib.checkTicketOffline(code, pub, { nowSec: now - 7200 })).result, 'REJECTED_NOT_YET');
  assert.equal((await lib.checkTicketOffline(code, pub, { nowSec: now + 7200 })).result, 'REJECTED_EXPIRED');
});
