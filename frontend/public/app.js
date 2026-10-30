'use strict';
// SahneBilet frontend: plain ES2020, no build step. Each role sees only its
// own screens; the API enforces the same rules server side.

const $app = document.getElementById('app');
const TZ = 'Europe/Istanbul';
const ROLE_NAMES = {
  customer: 'Müşteri', box_office: 'Gişe', hall_manager: 'Salon amiri',
  programmer: 'Programcı', accounting: 'Muhasebe', gate: 'Giriş kontrol',
};
const MENUS = {
  customer: [['#/shows', 'Seanslar'], ['#/orders', 'Biletlerim'], ['#/wallet', 'Cüzdanım']],
  box_office: [['#/shows', 'Gişe satışı'], ['#/orders', 'Siparişler'], ['#/giftcards', 'Hediye çeki']],
  hall_manager: [['#/shows', 'Seanslar'], ['#/halls', 'Salon düzeni'], ['#/conflicts', 'Giriş çakışmaları'], ['#/orders', 'Siparişler'], ['#/audit', 'Denetim kaydı']],
  programmer: [['#/schedule', 'Seans programı'], ['#/movies', 'Filmler / oyunlar'], ['#/coupons', 'Kuponlar']],
  accounting: [['#/reports', 'Hasılat'], ['#/coupons', 'Kuponlar'], ['#/orders', 'Siparişler'], ['#/audit', 'Denetim kaydı']],
  gate: [['#/gate', 'Giriş tarayıcı'], ['#/conflicts', 'Çakışmalar']],
};
const HOME = { customer: '#/shows', box_office: '#/shows', hall_manager: '#/shows', programmer: '#/schedule', accounting: '#/reports', gate: '#/gate' };

// ---------- utilities

const fmtTime = (d) => new Date(d).toLocaleTimeString('tr-TR', { hour: '2-digit', minute: '2-digit', timeZone: TZ });
const fmtDay = (d) => new Date(d).toLocaleDateString('tr-TR', { weekday: 'long', day: 'numeric', month: 'long', timeZone: TZ });
const fmtDT = (d) => new Date(d).toLocaleString('tr-TR', { dateStyle: 'short', timeStyle: 'short', timeZone: TZ });
const isoDay = (d) => new Date(d).toLocaleDateString('sv-SE', { timeZone: TZ });
const key = () => (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random());
const h = (html) => { const t = document.createElement('template'); t.innerHTML = html.trim(); return t.content; };
const store = {
  get(k) { try { return JSON.parse(localStorage.getItem(k)); } catch (e) { console.warn('storage read', e); return null; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) { console.warn('storage write', e); } },
  del(k) { try { localStorage.removeItem(k); } catch (e) { console.warn('storage delete', e); } },
};


// ---------- API client with token refresh

const auth = {
  get: () => store.get('auth'),
  set: (a) => store.set('auth', a),
  clear: () => store.del('auth'),
};

class ApiError extends Error {
  constructor(status, code, message, details) { super(message); this.status = status; this.code = code; this.details = details; }
}

async function api(method, path, body, opts = {}) {
  const a = auth.get();
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (a) headers.Authorization = 'Bearer ' + a.access_token;
  if (opts.idem) headers['Idempotency-Key'] = opts.idem;
  let res;
  try {
    res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (e) {
    throw new ApiError(0, 'NETWORK', 'Sunucuya ulaşılamıyor');
  }
  if (res.status === 401 && a && !opts.retried && path !== '/api/auth/refresh') {
    if (await refresh()) return api(method, path, body, { ...opts, retried: true });
    logout();
    throw new ApiError(401, 'UNAUTHENTICATED', 'Oturum süresi doldu');
  }
  if (res.status === 204) return null;
  let data = null;
  try { data = await res.json(); } catch (e) { data = null; }
  if (!res.ok) {
    const err = (data && data.error) || {};
    throw new ApiError(res.status, err.code || 'HTTP_' + res.status, err.message || res.statusText, err.details);
  }
  return data;
}

async function refresh() {
  const a = auth.get();
  if (!a) return false;
  try {
    const t = await fetch('/api/auth/refresh', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ refresh_token: a.refresh_token }) });
    if (!t.ok) return false;
    auth.set(await t.json());
    return true;
  } catch (e) {
    console.warn('refresh failed', e);
    return false;
  }
}

function logout() {
  const a = auth.get();
  if (a) fetch('/api/auth/logout', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ refresh_token: a.refresh_token }) }).catch((e) => console.warn(e));
  auth.clear();
  location.hash = '#/login';
}

// ---------- rendering helpers

const loading = (what) => `<div class="state">${esc(what || 'Yükleniyor')}…</div>`;
const empty = (what) => `<div class="state">${esc(what)}</div>`;
const failure = (e) => `<div class="notice bad">${esc(errText(e))}${e.status ? ` <span class="small">(${esc(e.code)})</span>` : ''}</div>`;

// render runs an async view and shows loading / error states around it.
async function render(view) {
  $app.innerHTML = loading();
  try {
    await view();
  } catch (e) {
    console.error(e);
    $app.innerHTML = failure(e);
  }
}

function setNav(route) {
  const a = auth.get();
  const nav = document.getElementById('nav');
  const user = document.getElementById('user');
  const out = document.getElementById('logout');
  if (!a) { nav.innerHTML = ''; user.textContent = ''; out.hidden = true; return; }
  nav.innerHTML = (MENUS[a.user.role] || []).map(([href, label]) =>
    `<a href="${href}" class="${route.startsWith(href) ? 'active' : ''}">${esc(label)}</a>`).join('');
  user.textContent = `${a.user.email} · ${ROLE_NAMES[a.user.role] || a.user.role}`;
  out.hidden = false;
}

// ---------- live indicator (simulator traffic shows the system is alive)

async function pollLive() {
  const el = document.getElementById('live');
  if (!auth.get()) { el.textContent = ''; return; }
  try {
    const s = await api('GET', '/api/stats/live');
    const n = s.holds_last_minute + s.orders_last_minute + s.scans_last_minute;
    el.className = 'live' + (n > 0 ? ' on' : '');
    el.textContent = `Son 1 dk: ${s.holds_last_minute} sepet · ${s.orders_last_minute} satış · ${s.scans_last_minute} giriş`;
  } catch (e) {
    el.className = 'live';
    el.textContent = 'Bağlantı yok';
  }
}
setInterval(pollLive, 5000);

// ---------- views

function viewLogin() {
  $app.innerHTML = `
  <div class="grid grid-7-5">
    <div>
      <h1>SahneBilet</h1>
      <p class="lead">Çok salonlu sinema ve tiyatro zinciri için seans programı, koltuk satışı, kampanya ve salon girişi.</p>
      <div class="card">
        <form id="f" class="stack">
          <label class="field">E-posta<input name="email" type="email" autocomplete="username" required value="musteri@sahnebilet.test"></label>
          <label class="field">Parola<input name="password" type="password" autocomplete="current-password" required value="Demo1234!"></label>
          <div id="msg"></div>
          <button class="btn" type="submit">Giriş yap</button>
        </form>
      </div>
    </div>
    <div class="card">
      <div class="label">Demo hesapları · parola Demo1234!</div>
      <table><tbody>
        ${Object.entries({ musteri: 'customer', gise: 'box_office', amir: 'hall_manager', programci: 'programmer', muhasebe: 'accounting', kapi: 'gate' })
          .map(([u, r]) => `<tr class="click" data-email="${u}@sahnebilet.test"><td>${ROLE_NAMES[r]}</td><td class="mono">${u}@sahnebilet.test</td></tr>`).join('')}
      </tbody></table>
    </div>
  </div>`;
  $app.querySelectorAll('tr[data-email]').forEach((tr) => tr.addEventListener('click', () => {
    $app.querySelector('[name=email]').value = tr.dataset.email;
  }));
  $app.querySelector('#f').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const fd = new FormData(ev.target);
    const msg = $app.querySelector('#msg');
    msg.innerHTML = loading('Giriş yapılıyor');
    try {
      const t = await api('POST', '/api/auth/login', { email: fd.get('email'), password: fd.get('password') });
      auth.set(t);
      location.hash = HOME[t.user.role] || '#/';
      pollLive();
    } catch (e) {
      msg.innerHTML = failure(e);
    }
  });
}

async function viewShows() {
  const now = new Date();
  const to = new Date(now.getTime() + 3 * 864e5);
  const [movies, page] = await Promise.all([
    api('GET', '/api/movies'),
    api('GET', `/api/showtimes?limit=200&from=${encodeURIComponent(new Date(now - 30 * 6e4).toISOString())}&to=${encodeURIComponent(to.toISOString())}`),
  ]);
  const role = auth.get().user.role;
  const title = role === 'box_office' ? 'Gişe satışı' : 'Seanslar';
  const draw = (movieId) => {
    const items = page.items.filter((s) => !movieId || s.movie_id === Number(movieId));
    const days = {};
    items.forEach((s) => { (days[isoDay(s.starts_at)] ||= []).push(s); });
    return Object.keys(days).length === 0 ? empty('Bu filtrede önümüzdeki 3 gün için seans yok.') :
      Object.entries(days).map(([, list]) => `
        <section class="day">
          <div class="label" style="margin-bottom:6px">${esc(fmtDay(list[0].starts_at))}</div>
          ${list.map((s) => {
            const pct = Math.round((100 * s.taken) / s.capacity);
            const started = new Date(s.starts_at) < now;
            return `<a class="show-row" href="#/show/${s.id}">
              <span class="show-time">${fmtTime(s.starts_at)}</span>
              <span><strong>${esc(s.movie_title)}</strong><br><span class="muted small">${esc(s.hall_name)} · bitiş ${fmtTime(s.ends_at)}</span></span>
              ${started ? '<span class="occ"><span class="tag">Seans başladı</span></span>' : occupancy(s.capacity - s.taken, s.capacity, pct)}
            </a>`;
          }).join('')}
        </section>`).join('');
  };
  $app.innerHTML = `
    <div class="spread"><div><h1>${title}</h1><p class="lead">Önümüzdeki 3 günün seansları. Doluluk, sepetteki koltukları da içerir.</p></div>
      <label class="field" style="min-width:220px">Film / oyun
        <select id="movie"><option value="">Tümü</option>${movies.items.map((m) => `<option value="${m.id}">${esc(m.title)}</option>`).join('')}</select>
      </label>
    </div>
    <div class="card" id="list">${draw('')}</div>`;
  $app.querySelector('#movie').addEventListener('change', (e) => { $app.querySelector('#list').innerHTML = draw(e.target.value); });
}

// --- seat selection, cart and checkout

async function viewShow(id) {
  const role = auth.get().user.role;
  const state = { sel: new Set(), hold: store.get('hold:' + id), map: null, quote: null, timer: null, cats: {} };
  const canOverride = role === 'hall_manager';

  async function loadMap() {
    state.map = await api('GET', `/api/showtimes/${id}/seats`);
    if (state.hold) {
      try {
        state.hold = await api('GET', '/api/holds/' + state.hold.id);
        if (state.hold.status !== 'active') { store.del('hold:' + id); state.hold = null; }
      } catch (e) { store.del('hold:' + id); state.hold = null; }
    }
  }
  await loadMap();
  if (role === 'customer') {
    try { state.offer = await api('GET', '/api/offers/me'); } catch (e) { console.warn('offer', e); }
  }
  const st = state.map.showtime;
  $app.innerHTML = `
    <a href="#/shows" class="small muted">← Seanslar</a>
    <h1>${esc(st.movie_title)}</h1>
    <p class="lead">${esc(st.hall_name)} · ${esc(fmtDay(st.starts_at))} ${fmtTime(st.starts_at)} – ${fmtTime(st.ends_at)}</p>
    <div class="grid grid-8-4">
      <div class="card">
        <div class="spread" style="margin-bottom:6px"><span></span><span id="livebadge" class="live">Bağlanıyor</span></div>
        <div class="screen">PERDE</div>
        <div class="seatmap"><div id="seats" class="seat-grid"></div></div>
        <div class="legend">
          <span><button class="seat" disabled></button>Boş</span><span><button class="seat sel" disabled></button>Seçili</span>
          <span><button class="seat mine" disabled></button>Sepetinizde</span><span><button class="seat held" disabled></button>Başka sepette</span>
          <span><button class="seat sold" disabled></button>Satıldı</span><span><button class="seat vip" disabled></button>VIP</span>
          <span><button class="seat accessible" disabled></button>Engelli</span>
        </div>
        <hr class="sep">
        <div class="row">
          <label class="field" style="width:120px">Kişi sayısı<input id="n" type="number" min="1" max="10" value="2"></label>
          <label class="check" style="margin-top:18px"><input id="acc" type="checkbox">Engelli koltuğu dahil</label>
          <button id="suggest" class="btn btn-ghost" style="margin-top:18px">En iyi koltukları öner</button>
        </div>
        <div id="sugg" class="small" style="margin-top:10px"></div>
      </div>
      <div class="card stack" id="cart"></div>
    </div>`;

  const seatsEl = $app.querySelector('#seats');
  const cartEl = $app.querySelector('#cart');

  function drawSeats() {
    const seats = state.map.seats;
    const maxCol = Math.max(...seats.map((s) => s.col));
    seatsEl.style.gridTemplateColumns = `18px repeat(${maxCol}, auto) 18px`;
    const rows = {};
    seats.forEach((s) => { (rows[s.row] ||= []).push(s); });
    const holdSeats = new Set(state.hold ? state.hold.seat_ids : []);
    let html = '';
    Object.keys(rows).sort((a, b) => a - b).forEach((r) => {
      const letter = String.fromCharCode(64 + Number(r));
      html += `<span class="rowlab">${letter}</span>`;
      const byCol = {};
      rows[r].forEach((s) => { byCol[s.col] = s; });
      for (let c = 1; c <= maxCol; c++) {
        const s = byCol[c];
        if (!s) { html += '<span></span>'; continue; }
        let cls = 'seat ' + s.kind;
        if (holdSeats.has(s.id) || s.mine) cls += ' mine';
        else if (s.status === 'sold') cls += ' sold';
        else if (s.status === 'held') cls += ' held';
        else if (state.sel.has(s.id)) cls += ' sel';
        html += `<button class="${cls}" data-id="${s.id}" title="${esc(s.label)} ${s.kind !== 'standard' ? '(' + s.kind + ')' : ''}">${c}</button>`;
      }
      html += `<span class="rowlab">${letter}</span>`;
    });
    seatsEl.innerHTML = html;
  }

  seatsEl.addEventListener('click', (e) => {
    const b = e.target.closest('button[data-id]');
    if (!b || state.hold) return;
    const sid = Number(b.dataset.id);
    const s = state.map.seats.find((x) => x.id === sid);
    if (!s || s.status !== 'free' || s.kind === 'blocked') return;
    if (state.sel.has(sid)) state.sel.delete(sid);
    else if (state.sel.size < 10) state.sel.add(sid);
    drawSeats();
    drawCart();
  });

  const label = (sid) => (state.map.seats.find((s) => s.id === sid) || {}).label || sid;

  function drawCart(msg = '') {
    clearInterval(state.timer);
    if (!state.hold) {
      const n = state.sel.size;
      cartEl.innerHTML = `
        <h2>Sepet</h2>
        <div class="price-line"><span>Tam</span><span>${tl(state.map.prices.full)}</span></div>
        <div class="price-line"><span>Öğrenci</span><span>${tl(state.map.prices.student)}</span></div>
        <div class="price-line"><span>Mısır + içecek menüsü</span><span>${tl(state.map.prices.combo)}</span></div>
        <hr class="sep" style="margin:8px 0">
        <div>${n ? `Seçili: <strong>${[...state.sel].map(label).join(', ')}</strong>` : '<span class="muted">Haritadan koltuk seçin ya da öneri isteyin.</span>'}</div>
        ${canOverride ? '<label class="check"><input id="ovr" type="checkbox">Tekli koltuk kuralını aş (amir istisnası)</label>' : ''}
        <div id="msg">${msg}</div>
        <button id="hold" class="btn" ${n ? '' : 'disabled'}>Koltukları 10 dk tut</button>`;
      cartEl.querySelector('#hold').addEventListener('click', doHold);
      return;
    }
    const h0 = state.hold;
    cartEl.innerHTML = `
      <div class="spread"><h2>Sepet</h2><span class="timer" id="timer"></span></div>
      <table><tbody>${h0.seat_ids.map((sid) => `<tr><td>${esc(label(sid))}</td><td>
        <select data-cat="${sid}"><option value="full">Tam</option><option value="student" ${state.cats[sid] === 'student' ? 'selected' : ''}>Öğrenci</option></select></td></tr>`).join('')}
      </tbody></table>
      <label class="field">Mısır + içecek menüsü<input id="combos" type="number" min="0" max="10" value="${state.combos || 0}"></label>
      ${state.offer ? `<div class="notice ok small">Size özel: <strong class="mono">${esc(state.offer.variant.coupon_code)}</strong> · ${esc(state.offer.variant.description)}
        <button id="useOffer" class="btn btn-ghost btn-sm" style="margin-left:6px">Uygula</button></div>` : ''}
      <label class="field">Kupon kodu<input id="coupon" value="${esc(state.coupon || '')}" placeholder="HOSGELDIN10"></label>
      <label class="field">Hediye çeki<input id="gift" value="${esc(state.gift || '')}" placeholder="HC-DEMO-100"></label>
      <div id="quote" class="small"></div>
      <div id="msg">${msg}</div>
      <div class="row">
        <button id="buy" class="btn">${role === 'box_office' ? 'Gişeden sat' : 'Ödemeyi tamamla'}</button>
        <button id="extend" class="btn btn-ghost btn-sm">+10 dk</button>
        <button id="release" class="btn btn-ghost btn-sm">Bırak</button>
      </div>`;
    const tick = () => {
      const left = Math.max(0, new Date(state.hold.expires_at) - Date.now());
      const el = cartEl.querySelector('#timer');
      if (el) el.textContent = `${String(Math.floor(left / 6e4)).padStart(2, '0')}:${String(Math.floor((left % 6e4) / 1e3)).padStart(2, '0')}`;
      if (left <= 0) { store.del('hold:' + id); state.hold = null; refreshAll(errBox({ code: 'HOLD_EXPIRED' })); }
    };
    tick();
    state.timer = setInterval(tick, 1000);
    cartEl.querySelectorAll('select[data-cat]').forEach((s) => s.addEventListener('change', () => { state.cats[s.dataset.cat] = s.value; doQuote(); }));
    ['combos', 'coupon', 'gift'].forEach((k) => cartEl.querySelector('#' + k).addEventListener('change', (e) => { state[k] = e.target.value; doQuote(); }));
    cartEl.querySelector('#buy').addEventListener('click', doBuy);
    const useOffer = cartEl.querySelector('#useOffer');
    if (useOffer) useOffer.addEventListener('click', () => {
      state.coupon = state.offer.variant.coupon_code;
      cartEl.querySelector('#coupon').value = state.coupon;
      doQuote();
    });
    cartEl.querySelector('#extend').addEventListener('click', async () => {
      try { state.hold = await api('POST', `/api/holds/${state.hold.id}/extend`); store.set('hold:' + id, state.hold); drawCart('<div class="notice ok">Süre uzatıldı.</div>'); doQuote(); } catch (e) { cartEl.querySelector('#msg').innerHTML = failure(e); }
    });
    cartEl.querySelector('#release').addEventListener('click', async () => {
      try { await api('DELETE', '/api/holds/' + state.hold.id); } catch (e) { console.warn(e); }
      store.del('hold:' + id); state.hold = null; state.sel.clear(); refreshAll();
    });
    doQuote();
  }

  const errBox = (e) => `<div class="notice bad">${esc(errText(e))}</div>`;

  function orderBody() {
    return {
      hold_id: state.hold.id,
      tickets: state.hold.seat_ids.map((sid) => ({ seat_id: sid, category: state.cats[sid] || 'full' })),
      combos: Number(state.combos || 0),
      coupon_code: state.coupon || '',
      gift_card_code: state.gift || '',
    };
  }

  async function doQuote() {
    const el = cartEl.querySelector('#quote');
    if (!el) return;
    el.innerHTML = loading('Fiyat hesaplanıyor');
    try {
      const q = await api('POST', '/api/quote', orderBody());
      el.innerHTML = `
        <div class="price-line"><span>Ara toplam</span><span>${tl(q.subtotal)}</span></div>
        ${q.discount ? `<div class="price-line"><span>Kupon indirimi</span><span>−${tl(q.discount)}</span></div>` : ''}
        ${q.gift_applied ? `<div class="price-line"><span>Hediye çeki</span><span>−${tl(q.gift_applied)}</span></div>` : ''}
        <div class="price-line total"><span>Ödenecek</span><span>${tl(q.payable)}</span></div>
        <div class="muted">KDV ${tl(q.tax.vat)} · eğlence vergisi ${tl(q.tax.ent_tax)} dahildir</div>`;
    } catch (e) {
      el.innerHTML = failure(e);
    }
  }

  async function doHold() {
    const msg = cartEl.querySelector('#msg');
    msg.innerHTML = loading('Koltuklar tutuluyor');
    const ovr = cartEl.querySelector('#ovr');
    try {
      state.hold = await api('POST', '/api/holds', { showtime_id: Number(id), seat_ids: [...state.sel], override_single_seat: !!(ovr && ovr.checked) }, { idem: key() });
      store.set('hold:' + id, state.hold);
      state.sel.clear();
      await refreshAll();
    } catch (e) {
      if (e.code === 'SEAT_TAKEN') { state.sel.clear(); await refreshAll(errBox(e)); return; }
      let extra = '';
      if (e.code === 'SINGLE_SEAT_RULE' && e.details) extra = ' Boş kalacak koltuk: ' + e.details.map((v) => String.fromCharCode(64 + v.row) + '-' + v.col).join(', ');
      msg.innerHTML = `<div class="notice bad">${esc(errText(e) + extra)}</div>`;
    }
  }

  async function doBuy() {
    const btn = cartEl.querySelector('#buy');
    btn.disabled = true;
    const msg = cartEl.querySelector('#msg');
    msg.innerHTML = loading('Ödeme alınıyor');
    try {
      const o = await api('POST', '/api/orders', orderBody(), { idem: state.payKey ||= key() });
      store.del('hold:' + id);
      location.hash = '#/order/' + o.id;
    } catch (e) {
      btn.disabled = false;
      msg.innerHTML = failure(e);
      if (e.code === 'HOLD_EXPIRED') { store.del('hold:' + id); state.hold = null; await refreshAll(errBox(e)); }
    }
  }

  async function refreshAll(msg = '') {
    await loadMap();
    drawSeats();
    drawCart(msg);
  }

  $app.querySelector('#suggest').addEventListener('click', async () => {
    const out = $app.querySelector('#sugg');
    out.innerHTML = loading('Öneri hesaplanıyor');
    try {
      const n = Number($app.querySelector('#n').value || 2);
      const acc = $app.querySelector('#acc').checked;
      const r = await api('GET', `/api/showtimes/${id}/suggest?n=${n}&accessible=${acc}`);
      if (!r.items.length) { out.innerHTML = empty(`${n} kişilik, tek boş koltuk bırakmayan bitişik yer kalmadı.`); return; }
      out.innerHTML = r.items.map((s, i) => `<button class="btn btn-ghost btn-sm" data-i="${i}">${esc(s.seat_ids.map(label).join(' '))}</button>`).join(' ');
      out.querySelectorAll('button').forEach((b) => b.addEventListener('click', () => {
        if (state.hold) return;
        state.sel = new Set(r.items[b.dataset.i].seat_ids);
        drawSeats(); drawCart();
      }));
    } catch (e) {
      out.innerHTML = failure(e);
    }
  });

  drawSeats();
  drawCart();
  // live seat map: server-sent events over fetch() (the token stays in the
  // Authorization header); falls back to polling if the stream breaks
  const live = $app.querySelector('#livebadge');
  let refreshing = false;
  const refreshMap = async () => {
    if (refreshing) return;
    refreshing = true;
    try { state.map = await api('GET', `/api/showtimes/${id}/seats`); drawSeats(); } catch (e) { console.warn('seat refresh', e); }
    refreshing = false;
  };
  const onPage = () => location.hash.startsWith('#/show/' + id);
  const ctrl = new AbortController();
  let fallback = null;
  const startPolling = () => {
    if (fallback) return;
    live.textContent = 'Yoklama (4 sn)';
    fallback = setInterval(() => { if (!onPage()) { clearInterval(fallback); return; } refreshMap(); }, 4000);
  };
  (async function stream() {
    while (onPage()) {
      try {
        const a = auth.get();
        const res = await fetch(`/api/showtimes/${id}/events`, { headers: { Authorization: 'Bearer ' + a.access_token }, signal: ctrl.signal });
        if (res.status === 401 && await refresh()) continue;
        if (!res.ok || !res.body) throw new Error('stream ' + res.status);
        live.textContent = 'Canlı';
        live.className = 'live on';
        if (fallback) { clearInterval(fallback); fallback = null; }
        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let i;
          while ((i = buf.indexOf('\n\n')) >= 0) {
            const ev = buf.slice(0, i);
            buf = buf.slice(i + 2);
            if (ev.includes('event: seats')) refreshMap();
          }
        }
      } catch (e) {
        if (ctrl.signal.aborted) return;
        console.warn('seat stream', e);
        live.className = 'live';
        startPolling();
      }
      await new Promise((r) => setTimeout(r, 3000));
    }
  })();
  const stopWatch = setInterval(() => {
    if (!onPage()) { ctrl.abort(); clearInterval(state.timer); clearInterval(stopWatch); if (fallback) clearInterval(fallback); }
  }, 1000);
}

function drawQR(el, text, size = 168) {
  if (window.QRCode) {
    el.innerHTML = '';
    new window.QRCode(el, { text, width: size, height: size, correctLevel: window.QRCode.CorrectLevel.M });
  } else {
    el.innerHTML = `<div class="mono">${esc(text)}</div>`;
  }
}

async function viewOrder(oid) {
  const o = await api('GET', '/api/orders/' + oid);
  const role = auth.get().user.role;
  const canRefund = ['customer', 'box_office', 'hall_manager'].includes(role);
  const tix = o.lines.filter((l) => l.kind === 'ticket');
  $app.innerHTML = `
    <a href="#/orders" class="small muted">← Siparişler</a>
    <h1>${esc(o.movie_title)}</h1>
    <p class="lead">${esc(o.hall_name)} · ${esc(fmtDay(o.starts_at))} ${fmtTime(o.starts_at)} · <span class="tag">${esc(o.channel === 'online' ? 'online' : 'gişe')}</span> <span class="tag ${o.status === 'paid' ? 'ok' : 'warn'}">${esc(st(o.status))}</span></p>
    <div class="grid grid-7-5">
      <div class="card">
        <h2>Biletler</h2>
        <div id="tix" class="stack">${tix.map((t) => `
          <div class="spread" style="border-top:1px solid var(--border);padding-top:12px">
            <div><strong>${esc(t.seat_label)}</strong> · ${t.category === 'student' ? 'Öğrenci' : 'Tam'} · ${tl(t.paid)}
              <div><span class="tag ${t.status === 'valid' ? 'ok' : t.status === 'used' ? '' : 'bad'}">${esc(st(t.status))}</span></div>
              ${t.status === 'valid' && role === 'customer' ? `<a class="btn btn-sm" href="#/pass/${t.ticket_id}" style="margin-top:8px">Cüzdan bileti</a>` : ''}
              ${canRefund && t.status === 'valid' ? `<button class="btn btn-ghost btn-sm" data-refund="${t.ticket_id}" style="margin-top:8px">Bu bileti iade et</button>` : ''}
            </div>
            <div class="qr" data-qr="${t.ticket_id}" ${t.status !== 'valid' ? 'hidden' : ''}></div>
          </div>`).join('')}</div>
      </div>
      <div class="card">
        <h2>Ödeme</h2>
        <div class="price-line"><span>Ara toplam</span><span>${tl(o.subtotal)}</span></div>
        <div class="price-line"><span>İndirim</span><span>−${tl(o.discount)}</span></div>
        <div class="price-line"><span>Hediye çeki</span><span>−${tl(o.gift_paid)}</span></div>
        <div class="price-line total"><span>Kart / nakit</span><span>${tl(o.total - o.gift_paid)}</span></div>
        <table style="margin-top:14px"><thead><tr><th>Kalem</th><th class="num">Net</th><th class="num">Eğl.</th><th class="num">KDV</th></tr></thead><tbody>
          ${o.lines.map((l) => `<tr><td>${l.kind === 'ticket' ? esc(l.seat_label) : 'Menü'}${l.status === 'refunded' ? ' <span class="tag bad">iade</span>' : ''}</td>
            <td class="num">${tl(l.tax.net)}</td><td class="num">${tl(l.tax.ent_tax)}</td><td class="num">${tl(l.tax.vat)}</td></tr>`).join('')}
        </tbody></table>
        ${canRefund && tix.some((t) => t.status === 'valid') ? '<button id="refundAll" class="btn btn-danger" style="margin-top:14px">Tüm siparişi iade et</button>' : ''}
        <div id="msg" style="margin-top:10px"></div>
      </div>
    </div>`;
  if (role === 'customer' || role === 'box_office') {
    for (const el of $app.querySelectorAll('[data-qr]')) {
      if (el.hidden) continue;
      api('GET', `/api/tickets/${el.dataset.qr}/qr`).then((r) => { drawQR(el, r.qr); if (role === 'customer') walletStore.put(r); }).catch((e) => { el.innerHTML = failure(e); });
    }
  }
  const doRefund = async (ids) => {
    const msg = $app.querySelector('#msg');
    msg.innerHTML = loading('İade ediliyor');
    try {
      const r = await api('POST', `/api/orders/${oid}/refund`, { ticket_ids: ids }, { idem: key() });
      await viewOrder(oid);
      $app.querySelector('#msg').innerHTML = `<div class="notice ok">İade: ${tl(r.refund.amount)} (hediye çekine ${tl(r.refund.to_gift_card)}, karta ${tl(r.refund.to_original_payment)}). KDV ${tl(r.refund.tax.vat)}, eğlence vergisi ${tl(r.refund.tax.ent_tax)} geri alındı.${r.refund.restore_coupon ? ' Kupon hakkı iade edildi.' : ''}</div>`;
    } catch (e) {
      msg.innerHTML = failure(e);
    }
  };
  $app.querySelectorAll('[data-refund]').forEach((b) => b.addEventListener('click', () => doRefund([b.dataset.refund])));
  const all = $app.querySelector('#refundAll');
  if (all) all.addEventListener('click', () => doRefund([]));
}

async function viewOrders(cursor = '') {
  const p = await api('GET', '/api/orders?limit=20' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''));
  const role = auth.get().user.role;
  $app.innerHTML = `
    <h1>${role === 'customer' ? 'Biletlerim' : 'Siparişler'}</h1>
    <p class="lead">${role === 'customer' ? 'Satın aldığınız biletler ve QR kodları.' : 'Tüm kanallardan son siparişler, en yeni üstte.'}</p>
    <div class="card table-wrap">${p.items.length === 0 ? empty('Henüz sipariş yok.') : `
      <table><thead><tr><th>Tarih</th><th>Gösterim</th><th>Kanal</th><th>Durum</th><th class="num">Bilet</th><th class="num">Tutar</th></tr></thead><tbody>
      ${p.items.map((o) => `<tr class="click" data-id="${o.id}"><td>${fmtDT(o.created_at)}</td>
        <td><strong>${esc(o.movie_title)}</strong><br><span class="small muted">${esc(o.hall_name)} · ${fmtDT(o.starts_at)}</span></td>
        <td>${o.channel === 'online' ? 'online' : 'gişe'}</td><td><span class="tag ${o.status === 'paid' ? 'ok' : 'warn'}">${esc(st(o.status))}</span></td>
        <td class="num">${o.lines.filter((l) => l.kind === 'ticket').length}</td><td class="num">${tl(o.total)}</td></tr>`).join('')}
      </tbody></table>`}
      ${p.next_cursor ? '<button id="more" class="btn btn-ghost btn-sm" style="margin-top:12px">Daha eski</button>' : ''}
    </div>`;
  $app.querySelectorAll('tr[data-id]').forEach((tr) => tr.addEventListener('click', () => { location.hash = '#/order/' + tr.dataset.id; }));
  const more = $app.querySelector('#more');
  if (more) more.addEventListener('click', () => render(() => viewOrders(p.next_cursor)));
}

async function viewGiftCards() {
  $app.innerHTML = `
    <h1>Hediye çeki</h1>
    <p class="lead">Gişeden hediye çeki satışı ve bakiye sorgulama. Çek birden fazla siparişte kısmen kullanılabilir.</p>
    <div class="grid grid-7-5">
      <div class="card stack"><h2>Yeni çek sat</h2>
        <label class="field">Tutar (TL)<input id="amt" type="number" min="50" max="5000" value="250"></label>
        <button id="issue" class="btn">Çek oluştur</button><div id="out"></div></div>
      <div class="card stack"><h2>Bakiye sorgula</h2>
        <label class="field">Çek kodu<input id="code" placeholder="HC-DEMO-500"></label>
        <button id="check" class="btn btn-ghost">Sorgula</button><div id="bal"></div></div>
    </div>`;
  $app.querySelector('#issue').addEventListener('click', async () => {
    const out = $app.querySelector('#out');
    out.innerHTML = loading();
    try {
      const g = await api('POST', '/api/gift-cards', { amount_kurus: Math.round(Number($app.querySelector('#amt').value) * 100) }, { idem: key() });
      out.innerHTML = `<div class="notice ok">Kod: <strong class="mono">${esc(g.code)}</strong> · bakiye ${tl(g.balance_kurus)}</div>`;
    } catch (e) { out.innerHTML = failure(e); }
  });
  $app.querySelector('#check').addEventListener('click', async () => {
    const out = $app.querySelector('#bal');
    out.innerHTML = loading();
    try {
      const g = await api('GET', '/api/gift-cards/' + encodeURIComponent($app.querySelector('#code').value.trim()));
      out.innerHTML = `<div class="notice ok">${esc(g.code)}: ${tl(g.balance_kurus)} / ${tl(g.initial_kurus)}</div>`;
    } catch (e) { out.innerHTML = failure(e); }
  });
}

async function viewHalls(hallId) {
  const halls = await api('GET', '/api/halls');
  const current = Number(hallId) || halls.items[0].id;
  const seats = await api('GET', `/api/halls/${current}/seats`);
  $app.innerHTML = `
    <h1>Salon düzeni</h1>
    <p class="lead">Koltuğa tıklayarak türünü değiştirin: standart → VIP → engelli → kapalı (arızalı). Satılmış veya sepette olan koltuk kapatılamaz.</p>
    <div class="row" style="margin-bottom:14px">${halls.items.map((x) => `<a class="btn ${x.id === current ? '' : 'btn-ghost'} btn-sm" href="#/halls/${x.id}">${esc(x.name)} · ${x.capacity}</a>`).join('')}</div>
    <div class="card"><div class="screen">PERDE</div><div class="seatmap"><div id="seats" class="seat-grid"></div></div><div id="msg" style="margin-top:12px"></div></div>`;
  const order = ['standard', 'vip', 'accessible', 'blocked'];
  const draw = () => {
    const maxCol = Math.max(...seats.items.map((s) => s.col));
    const el = $app.querySelector('#seats');
    el.style.gridTemplateColumns = `18px repeat(${maxCol}, auto)`;
    const rows = {};
    seats.items.forEach((s) => { (rows[s.row] ||= {})[s.col] = s; });
    el.innerHTML = Object.keys(rows).sort((a, b) => a - b).map((r) => `<span class="rowlab">${String.fromCharCode(64 + Number(r))}</span>` +
      Array.from({ length: maxCol }, (_, i) => {
        const s = rows[r][i + 1];
        return s ? `<button class="seat ${s.kind} ${s.kind === 'blocked' ? 'edit-blocked' : ''}" data-id="${s.id}" title="${esc(s.label)} ${s.kind}">${i + 1}</button>` : '<span></span>';
      }).join('')).join('');
  };
  draw();
  $app.querySelector('#seats').addEventListener('click', async (e) => {
    const b = e.target.closest('button[data-id]');
    if (!b) return;
    const s = seats.items.find((x) => x.id === Number(b.dataset.id));
    const next = order[(order.indexOf(s.kind) + 1) % order.length];
    const msg = $app.querySelector('#msg');
    try {
      const u = await api('PATCH', '/api/seats/' + s.id, { kind: next });
      s.kind = u.kind;
      draw();
      msg.innerHTML = `<div class="notice ok">${esc(s.label)} artık ${esc(next)}. Denetim kaydına yazıldı.</div>`;
    } catch (err) { msg.innerHTML = failure(err); }
  });
}

async function viewConflicts() {
  const c = await api('GET', '/api/gate/conflicts');
  const names = { DUPLICATE_ENTRY: 'Aynı bilet iki kez girdi', ADMITTED_AFTER_REFUND: 'İade edilmiş bilet çevrimdışı kabul edildi', UNKNOWN_TICKET: 'Tanınmayan bilet' };
  $app.innerHTML = `
    <h1>Giriş çakışmaları</h1>
    <p class="lead">Ağ kesintisinde kapı tarayıcılarının yerel olarak kabul ettiği, senkronizasyonda çakışan girişler.</p>
    <div class="card table-wrap">${c.items.length === 0 ? empty('Çakışma yok.') : `
      <table><thead><tr><th>Tür</th><th>Bilet</th><th>Kapı</th><th>Zaman</th><th>İlk giriş</th></tr></thead><tbody>
      ${c.items.map((x) => `<tr><td>${esc(names[x.kind] || x.kind)}</td><td class="mono">${esc(x.ticket_id.slice(0, 8))}</td><td>${esc(x.gate_id)}</td>
        <td>${fmtDT(x.scanned_at)}</td><td>${x.first_gate ? esc(x.first_gate) + ' · ' + fmtDT(x.first_at) : '—'}</td></tr>`).join('')}
      </tbody></table>`}</div>`;
}

async function viewAudit(before = 0) {
  const a = await api('GET', '/api/audit?limit=50' + (before ? '&before=' + before : ''));
  $app.innerHTML = `
    <h1>Denetim kaydı</h1>
    <p class="lead">Kritik işlemler: kim, ne zaman, neyi, eski ve yeni değer. Kayıtlar değiştirilemez (veritabanı tetikleyicisi).</p>
    <div class="card table-wrap">${a.items.length === 0 ? empty('Kayıt yok.') : `
      <table><thead><tr><th>#</th><th>Zaman</th><th>Kullanıcı</th><th>İşlem</th><th>Kayıt</th><th>Eski → yeni</th></tr></thead><tbody>
      ${a.items.map((x) => `<tr><td>${x.id}</td><td>${fmtDT(x.at)}</td><td>${x.actor_id ?? 'sistem'}</td><td>${esc(x.action)}</td>
        <td>${esc(x.entity)} <span class="mono">${esc(String(x.entity_id).slice(0, 12))}</span></td>
        <td class="mono">${esc(JSON.stringify(x.old_value) || '')} → ${esc(JSON.stringify(x.new_value).slice(0, 160))}</td></tr>`).join('')}
      </tbody></table>`}
      ${a.next_before ? '<button id="more" class="btn btn-ghost btn-sm" style="margin-top:12px">Daha eski</button>' : ''}</div>`;
  const more = $app.querySelector('#more');
  if (more) more.addEventListener('click', () => render(() => viewAudit(a.next_before)));
}

async function viewSchedule() {
  const today = isoDay(new Date());
  $app.innerHTML = `
    <h1>Seans programı</h1>
    <p class="lead">Film süresi + 15 dk reklam + 20 dk temizlik ile salon başına çakışmasız program. Sözleşme kuralları: haftalık asgari seans ve prime-time (18:00–22:00) payı.</p>
    <div class="card stack">
      <div class="row">
        <label class="field" style="width:180px">Başlangıç<input id="from" type="date" value="${today}"></label>
        <label class="field" style="width:120px">Gün<input id="days" type="number" min="1" max="28" value="14"></label>
        <button id="validate" class="btn btn-ghost" style="margin-top:18px">Kayıtlı programı doğrula</button>
        <button id="gen" class="btn btn-ghost" style="margin-top:18px">Yeni program üret (önizleme)</button>
        <button id="save" class="btn" style="margin-top:18px">Üret ve kaydet</button>
      </div>
      <div id="out"></div>
    </div>`;
  const movies = (await api('GET', '/api/movies')).items;
  const halls = (await api('GET', '/api/halls')).items;
  const mname = (id) => (movies.find((m) => m.id === id) || {}).title || id;
  const hname = (id) => (halls.find((x) => x.id === id) || {}).name || id;
  const out = $app.querySelector('#out');
  const show = (r) => {
    const problems = r.problems.length === 0 ? '<div class="notice ok">Tüm kısıtlar sağlanıyor: çakışma yok, haftalık asgari seans ve prime-time payları tamam.</div>' :
      `<div class="notice bad">${r.problems.length} kısıt ihlali</div><table><tbody>${r.problems.map((p) => `<tr><td><span class="tag bad">${esc(p.code)}</span></td><td>${esc(p.message)}</td></tr>`).join('')}</tbody></table>`;
    let plan = '';
    if (r.plan) {
      const first = r.plan.filter((s) => isoDay(s.start) === isoDay(r.plan[0].start));
      plan = `<h3 style="margin-top:16px">İlk gün (${first.length} seans)</h3><div class="table-wrap"><table><thead><tr><th>Salon</th><th>Başlangıç</th><th>Bitiş</th><th>Film</th></tr></thead><tbody>
        ${first.map((s) => `<tr><td>${esc(hname(s.hall_id))}</td><td>${fmtTime(s.start)}</td><td>${fmtTime(s.end)}</td><td>${esc(mname(s.movie_id))}</td></tr>`).join('')}</tbody></table></div>`;
    }
    const imp = r.improvement ? `<p class="small muted">İyileştirme turu: ${r.improvement.swaps} film yer değiştirdi; kapasite × talep skoru ${r.improvement.objective_before.toLocaleString('tr-TR')} → ${r.improvement.objective_after.toLocaleString('tr-TR')} (+%${((r.improvement.objective_after / r.improvement.objective_before - 1) * 100).toFixed(1)}). Popüler filmler büyük salonlara, haftalık ve prime-time sayıları değişmeden taşındı.</p>` : '';
    out.innerHTML = `<div class="label">${r.shows} seans · ${r.days} gün${r.saved ? ' · kaydedildi' : ''}</div>${problems}${imp}${plan}`;
  };
  const params = () => ({ from: $app.querySelector('#from').value, days: Number($app.querySelector('#days').value) });
  $app.querySelector('#validate').addEventListener('click', async () => {
    out.innerHTML = loading('Doğrulanıyor');
    try { const p = params(); show(await api('GET', `/api/schedule/validate?from=${p.from}&days=${p.days}`)); } catch (e) { out.innerHTML = failure(e); }
  });
  $app.querySelector('#gen').addEventListener('click', async () => {
    out.innerHTML = loading('Program üretiliyor');
    try { show(await api('POST', '/api/schedule/generate', { ...params(), save: false })); } catch (e) { out.innerHTML = failure(e); }
  });
  $app.querySelector('#save').addEventListener('click', async () => {
    out.innerHTML = loading('Kaydediliyor');
    try { show(await api('POST', '/api/schedule/generate', { ...params(), save: true })); } catch (e) { out.innerHTML = failure(e); }
  });
}

async function viewMovies() {
  const m = await api('GET', '/api/movies');
  const pct = (bp) => (bp / 100).toFixed(0) + '%';
  $app.innerHTML = `
    <h1>Filmler ve oyunlar</h1>
    <p class="lead">Vizyon sözleşmesi: haftalık asgari seans, prime-time payı ve dağıtımcı hasılat payı (1., 2. ve sonraki haftalar).</p>
    <div class="grid grid-8-4">
      <div class="card table-wrap"><table><thead><tr><th>Ad</th><th>Süre</th><th>Dağıtımcı</th><th>Vizyon</th><th class="num">Asgari/hf</th><th class="num">Prime</th><th class="num">Pay 1/2/sonra</th></tr></thead><tbody>
        ${m.items.map((x) => `<tr><td><strong>${esc(x.title)}</strong> <span class="tag">${x.kind === 'play' ? 'oyun' : 'film'}</span></td><td>${x.duration_min} dk</td>
          <td>${esc(x.distributor)}</td><td>${esc(x.release_date)}</td><td class="num">${x.min_weekly_shows}</td><td class="num">${pct(x.min_prime_bp)}</td>
          <td class="num">${pct(x.share_week1_bp)} / ${pct(x.share_week2_bp)} / ${pct(x.share_later_bp)}</td></tr>`).join('')}
      </tbody></table></div>
      <form id="f" class="card stack"><h2>Yeni ekle</h2>
        <label class="field">Ad<input name="title" required maxlength="200"></label>
        <label class="field">Tür<select name="kind"><option value="film">Film</option><option value="play">Oyun</option></select></label>
        <label class="field">Süre (dk)<input name="duration_min" type="number" min="30" max="300" value="110" required></label>
        <label class="field">Dağıtımcı<input name="distributor" required></label>
        <label class="field">Vizyon tarihi<input name="release_date" type="date" value="${isoDay(new Date())}" required></label>
        <label class="field">Haftalık asgari seans<input name="min_weekly_shows" type="number" min="0" value="0"></label>
        <label class="field">Prime-time payı (%)<input name="min_prime" type="number" min="0" max="100" value="0"></label>
        <label class="field">Dağıtımcı payı 1. hafta / 2. hafta / sonra (%)<span class="row"><input name="w1" type="number" value="55" style="width:30%"><input name="w2" type="number" value="50" style="width:30%"><input name="wl" type="number" value="45" style="width:30%"></span></label>
        <div id="msg"></div><button class="btn">Kaydet</button></form>
    </div>`;
  $app.querySelector('#f').addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const f = new FormData(ev.target);
    const msg = $app.querySelector('#msg');
    try {
      await api('POST', '/api/movies', {
        title: f.get('title'), kind: f.get('kind'), duration_min: Number(f.get('duration_min')), distributor: f.get('distributor'),
        release_date: f.get('release_date'), min_weekly_shows: Number(f.get('min_weekly_shows')), min_prime_bp: Number(f.get('min_prime')) * 100,
        share_week1_bp: Number(f.get('w1')) * 100, share_week2_bp: Number(f.get('w2')) * 100, share_later_bp: Number(f.get('wl')) * 100,
      });
      await render(viewMovies);
    } catch (e) { msg.innerHTML = failure(e); }
  });
}

async function viewCoupons() {
  const c = await api('GET', '/api/coupons');
  const canCreate = auth.get().user.role === 'accounting';
  $app.innerHTML = `
    <h1>Kuponlar</h1>
    <p class="lead">Kuponlar yalnızca bilete uygulanır, menüye uygulanmaz. Tam iade kupon hakkını geri verir.</p>
    <div class="grid grid-8-4">
      <div class="card table-wrap">${c.items.length === 0 ? empty('Kupon yok.') : `<table><thead><tr><th>Kod</th><th>İndirim</th><th class="num">Asgari</th><th class="num">Kullanım</th><th>Geçerlilik</th></tr></thead><tbody>
        ${c.items.map((x) => `<tr><td class="mono">${esc(x.code)}</td><td>${x.percent ? '%' + x.value / 100 : tl(x.value)}</td><td class="num">${tl(x.min_total)}</td>
          <td class="num">${x.used_count} / ${x.max_uses}</td><td class="small">${fmtDT(x.valid_from)} – ${fmtDT(x.valid_to)}</td></tr>`).join('')}</tbody></table>`}</div>
      ${canCreate ? `<form id="f" class="card stack"><h2>Yeni kupon</h2>
        <label class="field">Kod<input name="code" required minlength="3" maxlength="32"></label>
        <label class="field">Tür<select name="percent"><option value="1">Yüzde</option><option value="0">Tutar (TL)</option></select></label>
        <label class="field">Değer<input name="value" type="number" min="1" value="15" required></label>
        <label class="field">Asgari sepet (TL)<input name="min" type="number" min="0" value="0"></label>
        <label class="field">Kullanım hakkı<input name="max" type="number" min="1" value="100"></label>
        <label class="field">Bitiş<input name="to" type="date" value="${isoDay(new Date(Date.now() + 30 * 864e5))}"></label>
        <div id="msg"></div><button class="btn">Oluştur</button></form>` : ''}
    </div>`;
  const f = $app.querySelector('#f');
  if (f) f.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const d = new FormData(f);
    const percent = d.get('percent') === '1';
    try {
      await api('POST', '/api/coupons', {
        code: d.get('code'), percent, value: Number(d.get('value')) * 100, min_total: Number(d.get('min')) * 100, max_uses: Number(d.get('max')),
        valid_from: new Date().toISOString(), valid_to: new Date(d.get('to') + 'T23:59:59+03:00').toISOString(),
      });
      await render(viewCoupons);
    } catch (e) { $app.querySelector('#msg').innerHTML = failure(e); }
  });
}

async function viewReports() {
  const today = isoDay(new Date());
  const weekAgo = isoDay(new Date(Date.now() - 6 * 864e5));
  $app.innerHTML = `
    <h1>Hasılat</h1>
    <p class="lead">Seans tarihine göre hasılat. İade edilen satırlar hasılattan düşülür, ayrıca gösterilir. Dağıtımcı payı net (KDV ve eğlence vergisi hariç) bilet hasılatı üzerinden hesaplanır.</p>
    <div class="row" style="margin-bottom:14px">
      <label class="field" style="width:170px">Başlangıç<input id="from" type="date" value="${weekAgo}"></label>
      <label class="field" style="width:170px">Bitiş<input id="to" type="date" value="${isoDay(new Date(Date.now() + 7 * 864e5))}"></label>
      <button id="go" class="btn" style="margin-top:18px">Raporla</button>
    </div>
    <div id="rev" class="card table-wrap"></div>
    <hr class="sep">
    <h2>Dağıtımcı hasılat payı</h2>
    <div id="dist" class="card table-wrap"></div>
    <hr class="sep">
    <h2>Kampanya A/B testi</h2>
    <div id="ab" class="card table-wrap"></div>`;
  const run = async () => {
    const from = $app.querySelector('#from').value;
    const to = $app.querySelector('#to').value;
    const rev = $app.querySelector('#rev');
    const dist = $app.querySelector('#dist');
    rev.innerHTML = loading(); dist.innerHTML = loading();
    try {
      const r = await api('GET', `/api/reports/revenue?from=${from}&to=${to}`);
      const row = (x, bold) => `<tr${bold ? ' style="font-weight:600"' : ''}><td>${bold ? 'Toplam' : esc(x.movie_title)}</td><td>${bold ? 'tüm kanallar' : esc(x.channel === 'box_office' ? 'gişe' : x.channel)}</td>
        <td class="num">${x.tickets}</td><td class="num">${x.combos}</td><td class="num">${tl(x.gross)}</td><td class="num">${tl(x.discount)}</td><td class="num">${tl(x.paid)}</td>
        <td class="num">${tl(x.net)}</td><td class="num">${tl(x.ent_tax)}</td><td class="num">${tl(x.vat)}</td><td class="num">${x.refunded_count} · ${tl(x.refunded_paid)}</td></tr>`;
      rev.innerHTML = r.rows.length === 0 ? empty('Bu aralıkta satış yok.') : `<table><thead><tr><th>Film</th><th>Kanal</th><th class="num">Bilet</th><th class="num">Menü</th><th class="num">Brüt</th>
        <th class="num">İndirim</th><th class="num">Tahsilat</th><th class="num">Net</th><th class="num">Eğl. vergisi</th><th class="num">KDV</th><th class="num">İade</th></tr></thead>
        <tbody>${r.rows.map((x) => row(x)).join('')}${row(r.totals, true)}</tbody></table>`;
    } catch (e) { rev.innerHTML = failure(e); }
    try {
      const d = await api('GET', `/api/reports/distributor?from=${from}&to=${to}`);
      dist.innerHTML = d.items.length === 0 ? empty('Bu aralıkta bilet hasılatı yok.') : `<table><thead><tr><th>Dağıtımcı</th><th>Film</th><th class="num">Hafta</th><th class="num">Bilet</th>
        <th class="num">Net hasılat</th><th class="num">Pay</th><th class="num">Ödenecek</th></tr></thead><tbody>
        ${d.items.map((x) => `<tr><td>${esc(x.distributor)}</td><td>${esc(x.movie_title)}</td><td class="num">${x.week}</td><td class="num">${x.tickets}</td>
          <td class="num">${tl(x.net_revenue)}</td><td class="num">%${x.share_bp / 100}</td><td class="num">${tl(x.share_amount)}</td></tr>`).join('')}</tbody></table>`;
    } catch (e) { dist.innerHTML = failure(e); }
    const ab = $app.querySelector('#ab');
    ab.innerHTML = loading();
    try {
      const r = await api('GET', `/api/reports/ab?from=${from}&to=${to}`);
      const [a, b] = r.arms;
      const pct = (x) => (x * 100).toFixed(1) + '%';
      const row = (label, f) => `<tr><td>${label}</td><td class="num">${f(a)}</td><td class="num">${f(b)}</td></tr>`;
      const c = r.comparison;
      const verdict = !c.enough_data ? '<div class="notice warn">Karar için veri yetersiz: her kolda en az 5 ödenen ve 5 terk edilen sepet gerekir.</div>'
        : c.significant ? `<div class="notice ok">Anlamlı fark: <strong>${esc(c.winner)}</strong> kolu kazanıyor (p = ${c.p_value}, z = ${c.z_score}).</div>`
          : `<div class="notice warn">Fark istatistiksel olarak anlamlı değil (p = ${c.p_value}, z = ${c.z_score}). Teste devam edin.</div>`;
      ab.innerHTML = `
        <div class="label" style="margin-bottom:8px">${esc(r.experiment.title)}</div>
        <table><thead><tr><th>Metrik</th><th class="num">A · ${esc(a.variant.coupon_code)}</th><th class="num">B · ${esc(b.variant.coupon_code)}</th></tr></thead><tbody>
          ${row('Teklif', (x) => esc(x.variant.description))}
          ${row('Sepet açan müşteri', (x) => x.exposed)}
          ${row('Satın alan müşteri', (x) => `${x.converted} (${pct(x.user_conversion_rate)})`)}
          ${row('Açılan sepet', (x) => x.carts)}
          ${row('Ödenen sepet', (x) => x.paid_carts)}
          ${row('<strong>Sepet dönüşümü</strong>', (x) => `<strong>${pct(x.conversion_rate)}</strong>`)}
          ${row('Sipariş', (x) => x.orders)}
          ${row('Kuponu kullanan sipariş', (x) => x.coupon_uses)}
          ${row('Ortalama sepet', (x) => tl(x.avg_order_value))}
          ${row('Sipariş başına indirim maliyeti', (x) => tl(x.discount_per_order))}
          ${row('Menü ekleme oranı', (x) => pct(x.combo_attach_rate))}
          ${row('Müşteri başına gelir', (x) => tl(x.revenue_per_exposed_user))}
          ${row('Toplam tahsilat', (x) => tl(x.revenue))}
        </tbody></table>
        <div style="margin-top:12px">${verdict}</div>
        <p class="small muted">Sepet dönüşüm farkı (B − A): ${(c.absolute_lift * 100).toFixed(1)} puan. Deneme birimi sepettir: her açılan sepet bir deneme, ödenen sepet bir başarıdır; gişe satışları teste dahil değildir.</p>`;
    } catch (e) { ab.innerHTML = failure(e); }
  };
  $app.querySelector('#go').addEventListener('click', run);
  run();
  void today;
}

// --- gate scanner: verifies Ed25519 signatures locally, works offline


async function viewGate() {
  const gs = store.get('gate') || { gateId: 'KAPI-1', used: [], revoked: [], pending: [], publicKey: '', lastSync: null, offline: false };
  const save = () => store.set('gate', gs);
  let key = null;
  const loadKey = async () => {
    if (!gs.publicKey || !crypto.subtle) return null;
    try { return await crypto.subtle.importKey('raw', Uint8Array.from(atob(gs.publicKey), (c) => c.charCodeAt(0)), { name: 'Ed25519' }, false, ['verify']); }
    catch (e) { console.warn('Ed25519 not supported', e); return null; }
  };
  $app.innerHTML = `
    <h1>Giriş tarayıcı</h1>
    <p class="lead">QR imzası cihazda doğrulanır; ağ kesilse de çalışır. Çevrimdışı girişler sırada bekler ve senkronizasyonda sunucuya gönderilir.</p>
    <div class="grid grid-7-5">
      <div class="card stack">
        <div class="row">
          <label class="field" style="width:160px">Kapı<input id="gid" value="${esc(gs.gateId)}"></label>
          <label class="check" style="margin-top:18px"><input id="off" type="checkbox" ${gs.offline ? 'checked' : ''}>Çevrimdışı mod (ağ kesintisi)</label>
        </div>
        <label class="field">QR içeriği (tarayıcıdan gelen metin)<input id="code" placeholder="SB1.…" autocomplete="off"></label>
        <button id="scan" class="btn">Doğrula</button>
        <div id="res"></div>
      </div>
      <div class="card stack">
        <div class="spread"><h2>Cihaz durumu</h2><button id="sync" class="btn btn-ghost btn-sm">Senkronize et</button></div>
        <div id="dev"></div>
        <div id="syncmsg"></div>
      </div>
    </div>`;
  const dev = () => {
    $app.querySelector('#dev').innerHTML = `
      <div class="price-line"><span>Bekleyen çevrimdışı giriş</span><strong>${gs.pending.length}</strong></div>
      <div class="price-line"><span>Bilinen kullanılmış bilet</span><span>${gs.used.length}</span></div>
      <div class="price-line"><span>Bilinen iade</span><span>${gs.revoked.length}</span></div>
      <div class="price-line"><span>Son senkron</span><span>${gs.lastSync ? fmtDT(gs.lastSync) : '—'}</span></div>
      <div class="small muted">${gs.publicKey ? 'Doğrulama anahtarı yüklü' : 'Anahtar yok: önce bir kez senkronize edin'}</div>`;
  };
  const show = (result, extra = '') => {
    const ok = result === 'ACCEPTED' || result === 'ACCEPTED_OFFLINE';
    const text = {
      ACCEPTED: 'GİRİŞ KABUL', ACCEPTED_OFFLINE: 'GİRİŞ KABUL (çevrimdışı)', REJECTED_ALREADY_USED: 'BİLET KULLANILMIŞ', REJECTED_REFUNDED: 'BİLET İADE EDİLMİŞ',
      REJECTED_SIGNATURE: 'SAHTE BİLET', REJECTED_MALFORMED: 'OKUNAMADI', REJECTED_NOT_YET: 'HENÜZ GEÇERLİ DEĞİL', REJECTED_EXPIRED: 'SÜRESİ GEÇMİŞ',
    }[result] || result;
    $app.querySelector('#res').innerHTML = `<div class="scan-result notice ${ok ? 'ok' : 'bad'}">${esc(text)}</div><div class="small muted">${extra}</div>`;
  };

  async function offlineScan(code) {
    key = key || await loadKey();
    if (!key) return show('REJECTED_MALFORMED', 'Bu tarayıcı Ed25519 doğrulamasını desteklemiyor ya da anahtar yok.');
    const { result, claims } = await checkTicketOffline(code, key, { used: gs.used, revoked: gs.revoked, nowSec: Date.now() / 1000 });
    if (result !== 'ACCEPTED') return show(result);
    gs.used.push(claims.t);
    gs.pending.push({ ticket_id: claims.t, gate_id: gs.gateId, scanned_at: new Date().toISOString() });
    save(); dev();
    show('ACCEPTED_OFFLINE', `Koltuk ${esc(claims.l)} · senkron bekliyor`);
  }

  async function doSync() {
    const msg = $app.querySelector('#syncmsg');
    msg.innerHTML = loading('Senkronize ediliyor');
    try {
      const st = await api('POST', '/api/gate/sync', { gate_id: gs.gateId, scans: gs.pending });
      gs.pending = []; gs.used = st.used; gs.revoked = st.revoked; gs.publicKey = st.public_key; gs.lastSync = new Date().toISOString();
      key = null; save(); dev();
      const conf = st.conflicts || [];
      msg.innerHTML = conf.length ? `<div class="notice warn">${conf.length} çakışma salon amirine bildirildi: ${conf.map((c) => esc(c.kind)).join(', ')}</div>` : '<div class="notice ok">Senkron tamam, çakışma yok.</div>';
    } catch (e) { msg.innerHTML = failure(e); }
  }

  $app.querySelector('#gid').addEventListener('change', (e) => { gs.gateId = e.target.value.trim() || 'KAPI-1'; save(); });
  $app.querySelector('#off').addEventListener('change', (e) => { gs.offline = e.target.checked; save(); });
  $app.querySelector('#sync').addEventListener('click', doSync);
  const scan = async () => {
    const code = $app.querySelector('#code').value.trim();
    if (!code) return;
    $app.querySelector('#code').value = '';
    if (gs.offline) return offlineScan(code);
    try {
      const r = await api('POST', '/api/gate/scan', { gate_id: gs.gateId, code });
      show(r.result, r.seat ? `Koltuk ${esc(r.seat)}${r.first_gate ? ' · ilk giriş ' + esc(r.first_gate) + ' ' + fmtDT(r.first_at) : ''}` : '');
    } catch (e) {
      if (e.code === 'NETWORK') { await offlineScan(code); return; }
      $app.querySelector('#res').innerHTML = failure(e);
    }
  };
  $app.querySelector('#scan').addEventListener('click', scan);
  $app.querySelector('#code').addEventListener('keydown', (e) => { if (e.key === 'Enter') scan(); });
  dev();
  if (!gs.publicKey) {
    try { const st = await api('GET', '/api/gate/state'); gs.publicKey = st.public_key; gs.used = st.used; gs.revoked = st.revoked; gs.lastSync = new Date().toISOString(); save(); dev(); }
    catch (e) { $app.querySelector('#syncmsg').innerHTML = failure(e); }
  }
}


// --- wallet: full-screen mobile ticket, stored on the device so it opens
// without network at the door

const walletStore = {
  all: () => store.get('wallet') || {},
  put(p) { const w = walletStore.all(); w[p.ticket_id] = p; store.set('wallet', w); },
  drop(id) { const w = walletStore.all(); delete w[id]; store.set('wallet', w); },
};


async function viewPass(tid) {
  let p = null;
  let offline = false;
  try {
    p = await api('GET', `/api/tickets/${tid}/qr`);
    walletStore.put(p);
  } catch (e) {
    if (e.code === 'TICKET_REFUNDED' || e.status === 404) { walletStore.drop(tid); throw e; }
    p = walletStore.all()[tid];
    if (!p) throw e;
    offline = true;
  }
  const past = new Date(p.ends_at) < new Date();
  $app.innerHTML = `
    <a href="#/wallet" class="small muted">← Cüzdanım</a>
    <div class="pass">
      <div class="pass-head">
        <div><div class="label" style="color:#CBD5E1">SahneBilet</div><div class="pass-title">${esc(p.movie_title)}</div></div>
        <span class="tag">${p.category === 'student' ? 'Öğrenci' : 'Tam'}</span>
      </div>
      <div class="pass-grid">
        <div><div class="label">Tarih</div><strong>${esc(fmtDay(p.starts_at))}</strong></div>
        <div><div class="label">Saat</div><strong>${fmtTime(p.starts_at)}</strong></div>
        <div><div class="label">Salon</div><strong>${esc(p.hall_name)}</strong></div>
        <div><div class="label">Koltuk</div><strong class="pass-seat">${esc(p.seat)}</strong></div>
      </div>
      <div class="pass-qr"><div id="qr" class="qr"></div></div>
      <div class="small muted" style="text-align:center;padding:0 16px">${past ? 'Seans sona erdi.' : `Girişte okutun · ${fmtDT(p.valid_from)} itibarıyla geçerli · tek kullanımlık`}</div>
      <div class="pass-id mono">${esc(p.ticket_id)}</div>
    </div>
    ${offline ? '<div class="notice warn" style="margin-top:12px">Çevrimdışı: cihazda kayıtlı bilet gösteriliyor.</div>' : ''}
    <div class="row noprint" style="margin-top:14px">
      <button id="ics" class="btn btn-ghost">Takvime ekle</button>
      <button id="print" class="btn btn-ghost">Yazdır / PDF</button>
    </div>
    <p class="small muted noprint">Bu bilet cihazınıza kaydedildi; kapıda internet olmasa da Cüzdanım'dan açılır. Ekran parlaklığını artırmanız okutmayı hızlandırır.</p>`;
  drawQR($app.querySelector('#qr'), p.qr, 232);
  $app.querySelector('#ics').addEventListener('click', () => {
    const url = URL.createObjectURL(new Blob([icsFor(p)], { type: 'text/calendar' }));
    const a = document.createElement('a');
    a.href = url; a.download = `sahnebilet-${p.seat}.ics`; a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  });
  $app.querySelector('#print').addEventListener('click', () => window.print());
}

function viewWallet() {
  const list = Object.values(walletStore.all()).sort((a, b) => new Date(a.starts_at) - new Date(b.starts_at));
  const now = new Date();
  $app.innerHTML = `
    <h1>Cüzdanım</h1>
    <p class="lead">Açtığınız biletler bu cihazda saklanır ve internet olmadan da gösterilebilir. Yeni bilet eklemek için Biletlerim'den "Cüzdan bileti"ne dokunun.</p>
    <div class="card">${list.length === 0 ? empty('Cüzdanınızda bilet yok.') : list.map((p) => `
      <a class="show-row" href="#/pass/${esc(p.ticket_id)}">
        <span class="show-time">${fmtTime(p.starts_at)}</span>
        <span><strong>${esc(p.movie_title)}</strong><br><span class="muted small">${esc(fmtDay(p.starts_at))} · ${esc(p.hall_name)} · ${esc(p.seat)}</span></span>
        <span>${new Date(p.ends_at) < now ? '<span class="tag">geçmiş</span>' : '<span class="tag ok">geçerli</span>'}</span>
      </a>`).join('')}</div>`;
}

// ---------- router

const routes = [
  [/^#\/login$/, () => viewLogin(), false],
  [/^#\/shows$/, () => render(viewShows)],
  [/^#\/show\/(\d+)$/, (m) => render(() => viewShow(m[1]))],
  [/^#\/orders$/, () => render(() => viewOrders())],
  [/^#\/order\/([0-9a-f-]+)$/, (m) => render(() => viewOrder(m[1]))],
  [/^#\/giftcards$/, () => render(viewGiftCards)],
  [/^#\/wallet$/, () => viewWallet()],
  [/^#\/pass\/([0-9a-f-]+)$/, (m) => render(() => viewPass(m[1]))],
  [/^#\/halls(?:\/(\d+))?$/, (m) => render(() => viewHalls(m[1]))],
  [/^#\/conflicts$/, () => render(viewConflicts)],
  [/^#\/audit$/, () => render(() => viewAudit())],
  [/^#\/schedule$/, () => render(viewSchedule)],
  [/^#\/movies$/, () => render(viewMovies)],
  [/^#\/coupons$/, () => render(viewCoupons)],
  [/^#\/reports$/, () => render(viewReports)],
  [/^#\/gate$/, () => render(viewGate)],
];

function route() {
  const hash = location.hash || '#/';
  const a = auth.get();
  setNav(hash);
  if (!a && hash !== '#/login') { location.hash = '#/login'; return; }
  if (a && (hash === '#/' || hash === '#/login')) { location.hash = HOME[a.user.role] || '#/shows'; return; }
  for (const [re, fn] of routes) {
    const m = hash.match(re);
    if (m) { fn(m); return; }
  }
  $app.innerHTML = empty('Sayfa bulunamadı.');
}

window.addEventListener('hashchange', route);
document.getElementById('logout').addEventListener('click', logout);
route();
pollLive();
