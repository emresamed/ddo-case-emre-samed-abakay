'use strict';
// Pure helpers shared by the app and its tests (node --test frontend/test).
// Loaded as a classic script before app.js; top-level declarations are
// globals there, and module.exports makes them importable in Node.

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const tl = (k) => (Number(k || 0) / 100).toLocaleString('tr-TR', { style: 'currency', currency: 'TRY' });

const ERRORS = {
  SEAT_TAKEN: 'Seçtiğiniz koltuklardan biri az önce başka biri tarafından alındı. Haritayı yeniledik, lütfen tekrar seçin.',
  SINGLE_SEAT_RULE: 'Bu seçim sırada tek boş koltuk bırakıyor. Yanındaki koltuğu da ekleyin ya da bir koltuk kaydırın.',
  HOLD_EXPIRED: 'Sepet süresi doldu, koltuklar serbest bırakıldı.',
  BUSY: 'Çok yoğun talep var, birkaç saniye sonra tekrar deneyin.',
  COUPON_EXHAUSTED: 'Bu kuponun kullanım hakkı bitti.',
  COUPON_INVALID: 'Kupon geçersiz.',
  COUPON_MIN_TOTAL: 'Kupon için sepet tutarı yetersiz.',
  GIFT_CARD_INVALID: 'Hediye çeki bulunamadı.',
  TOO_MANY_HOLDS: 'Aynı anda en fazla 3 açık sepetiniz olabilir.',
  EXTENSION_LIMIT: 'Sepet süresi en fazla iki kez uzatılabilir.',
  REFUND_WINDOW_CLOSED: 'İade süresi doldu (online iade seanstan 60 dk öncesine kadar).',
  TICKET_USED: 'Kullanılmış bilet iade edilemez.',
  INVALID_CREDENTIALS: 'E-posta veya parola hatalı.',
  FORBIDDEN: 'Bu işlem için yetkiniz yok.',
};
const STATUS = { valid: 'geçerli', used: 'kullanıldı', refunded: 'iade edildi', paid: 'ödendi', partially_refunded: 'kısmi iade' };
const st = (s) => STATUS[s] || s;
const errText = (e) => ERRORS[e.code] || e.message || 'Beklenmeyen hata';

// occupancy renders "how full is it" so it reads at a glance: a status line
// in words, a bar that fills as seats sell, and the exact numbers below.
function occupancy(free, capacity, pct) {
  let status;
  let level = '';
  if (free <= 0) {
    status = 'Tükendi';
    level = 'full';
  } else if (free <= Math.max(10, capacity * 0.15)) {
    status = `Son ${free} koltuk`;
    level = 'low';
  } else {
    status = `${free} koltuk boş`;
  }
  return `<span class="occ ${level}">
    <span class="occ-status">${status}</span>
    <span class="occ-bar" title="%${pct} dolu"><i style="width:${Math.max(pct, 0)}%"></i></span>
    <span class="occ-meta">${pct > 0 ? `%${pct} dolu` : 'henüz satış yok'} · ${capacity} koltuk</span>
  </span>`;
}

const b64u = (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/') + '==='.slice((s.length + 3) % 4)), (c) => c.charCodeAt(0));

function icsFor(p) {
  const f = (d) => new Date(d).toISOString().replace(/[-:]/g, '').replace(/\.\d{3}/, '');
  const lines = ['BEGIN:VCALENDAR', 'VERSION:2.0', 'PRODID:-//SahneBilet//TR', 'BEGIN:VEVENT',
    `UID:${p.ticket_id}@sahnebilet`, `DTSTAMP:${f(new Date())}`, `DTSTART:${f(p.starts_at)}`, `DTEND:${f(p.ends_at)}`,
    `SUMMARY:${p.movie_title} (${p.seat})`, `LOCATION:${p.hall_name}`, 'DESCRIPTION:Girişte bilet QR kodunu gösterin.',
    'BEGIN:VALARM', 'TRIGGER:-PT1H', 'ACTION:DISPLAY', 'DESCRIPTION:Seansa 1 saat', 'END:VALARM', 'END:VEVENT', 'END:VCALENDAR'];
  return lines.join('\r\n');
}

// checkTicketOffline is the gate decision without network: signature with
// the Ed25519 public key, validity window, then the device's refunded and
// used lists. It mirrors gate.Scanner.Scan on the server.
async function checkTicketOffline(code, publicKey, { used = [], revoked = [], nowSec = Date.now() / 1000 } = {}) {
  const parts = String(code).trim().split('.');
  if (parts.length !== 3 || parts[0] !== 'SB1') return { result: 'REJECTED_MALFORMED' };
  let claims;
  try {
    const ok = await crypto.subtle.verify({ name: 'Ed25519' }, publicKey, b64u(parts[2]), new TextEncoder().encode(parts[0] + '.' + parts[1]));
    if (!ok) return { result: 'REJECTED_SIGNATURE' };
    claims = JSON.parse(new TextDecoder().decode(b64u(parts[1])));
  } catch (e) {
    return { result: 'REJECTED_MALFORMED' };
  }
  if (!claims || !claims.t) return { result: 'REJECTED_MALFORMED' };
  if (nowSec < claims.nb) return { result: 'REJECTED_NOT_YET', claims };
  if (nowSec > claims.na) return { result: 'REJECTED_EXPIRED', claims };
  if (revoked.includes(claims.t)) return { result: 'REJECTED_REFUNDED', claims };
  if (used.includes(claims.t)) return { result: 'REJECTED_ALREADY_USED', claims };
  return { result: 'ACCEPTED', claims };
}

if (typeof module !== 'undefined') {
  module.exports = { esc, tl, STATUS, st, ERRORS, errText, occupancy, b64u, icsFor, checkTicketOffline };
}
