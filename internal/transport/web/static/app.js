/* Quicc POS — agent web UI (buildless; Alpine.js v3 is loaded after this file).
 *
 * One Alpine component (`agentApp`, on <body>) holds the whole UI: the
 * provisioning screens driven by GET /api/session's `state`, then the app
 * (Overview, Live, Drivers, History, Printers, Settings). Every API call goes
 * through api()/call(): any 401 re-reads /api/session so the screen lands on
 * login (session gone) or setup (token revoked).
 *
 * Icons are Lucide (https://lucide.dev), ISC License — see LICENSES.md.
 */
'use strict';

const HIST_NUM = 20;
const DEFAULT_PREPS = [10, 15, 20, 30, 45, 60];
const DEFAULT_GREEN = 20;
const THEME_KEY = 'quicc.agent.theme';
const PAGES = ['overview', 'live', 'drivers', 'history', 'printers', 'settings'];
const SETTINGS_TABS = [
  { id: 'orders', label: 'Orders' },
  { id: 'kitchen', label: 'Kitchen tickets' },
  { id: 'receipts', label: 'Receipts' },
  { id: 'clockout', label: 'Daily clock-out' },
  { id: 'tablets', label: 'Tablets' },
  { id: 'store', label: 'Store info' },
  { id: 'account', label: 'Account' },
];
const NAV = [
  { id: 'overview', label: 'Overview', icon: 'overview' },
  { id: 'live', label: 'Live', icon: 'live' },
  { id: 'drivers', label: 'Drivers', icon: 'truck' },
  { id: 'history', label: 'History', icon: 'clock' },
  { id: 'printers', label: 'Printers', icon: 'printer' },
  { id: 'settings', label: 'Settings', icon: 'sliders' },
];
// /api/diag problems[] codes → what staff should read.
const PROBLEM_TEXT = {
  main_down: "Not connected to main — new orders can't arrive.",
  printer_down: 'A printer is down — see Printers.',
  sd_errors: 'The SD card reported errors — replace it soon.',
  rootfs_ro: "The Pi's disk went read-only — restart the Pi.",
  under_voltage: "The Pi's power supply is too weak (under-voltage).",
  ntp: "The Pi's clock isn't synced.",
  health_stale: "The Pi's health report is out of date.",
};
const PI_DOWN = "Can't reach the Pi — this screen is out of date.";
const STILL_WAITING_MS = 2 * 60000; // the full card turns amber ("STILL WAITING")
const REBEEP_MS = 10000;            // repeat the beep while any order waits
// main's order statuses (main/backend/internal/domain/order/entity.go) → what staff read.
const HIST_STATUSES = [
  ['received', 'New'], ['validated', 'New · checked'], ['accepted', 'Accepted'], ['preparing', 'Cooking'],
  ['ready', 'Ready'], ['on_delivery', 'Out for delivery'], ['completed', 'Done'], ['rejected', 'Rejected'], ['cancelled', 'Cancelled'],
];
// Receipt settings: allowed values (internal/domain/order/receipt_prefs.go).
const NUM_SIZES = [['large', 'Large'], ['xlarge', 'X-Large'], ['huge', 'Huge']];
const ITEM_SIZES = [['normal', 'Normal'], ['large', 'Large'], ['xlarge', 'X-Large']];
const FOOT_SIZES = [['normal', 'Normal'], ['large', 'Large'], ['xlarge', 'X-Large']];
const FOOT_MODES = [['default', 'Store default'], ['custom', 'Custom'], ['none', 'None']];
const MAX_FOOTER = 32;
const SIZE_PX = { normal: '12px', large: '16px', xlarge: '22px', huge: '34px' };
const PREF_KEYS = ['kitchen_item_size', 'kitchen_number_size', 'footer_text', 'footer_size', 'copies', 'show_store_address', 'show_phone', 'extra_paper'];

// --- icons (Lucide, ISC) ---------------------------------------------------
const ICON_PATHS = {
  overview: '<rect x="3" y="3" width="7" height="9" rx="1"/><rect x="14" y="3" width="7" height="5" rx="1"/><rect x="14" y="12" width="7" height="9" rx="1"/><rect x="3" y="16" width="7" height="5" rx="1"/>',
  live: '<path d="M3 2v7c0 1.1.9 2 2 2h4a2 2 0 0 0 2-2V2"/><path d="M7 2v20"/><path d="M21 15V2a5 5 0 0 0-5 5v6c0 1.1.9 2 2 2h3zm0 0v7"/>',
  truck: '<path d="M1 3h15v13H1z"/><path d="M16 8h4l3 3v5h-7z"/><circle cx="5.5" cy="18.5" r="2.5"/><circle cx="18.5" cy="18.5" r="2.5"/>',
  clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
  printer: '<path d="M6 9V2h12v7"/><path d="M6 18H4a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2h-2"/><rect x="6" y="14" width="12" height="8"/>',
  sliders: '<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"/>',
  moon: '<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9z"/>',
  sun: '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41"/>',
  logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="m16 17 5-5-5-5"/><path d="M21 12H9"/>',
  menu: '<path d="M4 6h16M4 12h16M4 18h16"/>',
  x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  refresh: '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
  up: '<path d="m18 15-6-6-6 6"/>',
  down: '<path d="m6 9 6 6 6-6"/>',
  right: '<path d="m9 18 6-6-6-6"/>',
  trash: '<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/>',
  alert: '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
  mute: '<path d="M11 5 6 9H2v6h4l5 4z"/><path d="m22 9-6 6"/><path d="m16 9 6 6"/>',
};
function icon(name, size = 18, extra = '') {
  return '<svg width="' + size + '" height="' + size + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"' + extra + '>' + (ICON_PATHS[name] || '') + '</svg>';
}

// --- pure helpers -----------------------------------------------------------
class ApiError extends Error {
  constructor(message, status, code, body) { super(message); this.status = status; this.code = code || ''; this.body = body; }
}
function ts(v) { if (!v) return 0; const n = new Date(v).getTime(); return Number.isFinite(n) ? n : 0; }
function money(cents) { const c = Number(cents) || 0; return (c < 0 ? '-$' : '$') + (Math.abs(c) / 100).toFixed(2); }
function clock(v) { const t = ts(v); return t ? new Date(t).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' }) : '—'; }
function dateTime(v) { const t = ts(v); return t ? new Date(t).toLocaleString([], { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }) : '—'; }
// m:ss, or h:mm:ss past an hour (Quibase's duration()).
function dur(ms) {
  const neg = ms < 0, a = Math.floor(Math.abs(ms) / 1000);
  const h = Math.floor(a / 3600), m = Math.floor(a / 60) % 60, s = String(a % 60).padStart(2, '0');
  const body = h > 0 ? h + ':' + String(m).padStart(2, '0') + ':' + s : m + ':' + s;
  return neg ? '-' + body : body;
}
// "4 min" / "1 h 5 min" (Quibase's minutes()).
function mins(m) { m = Math.max(0, Math.floor(m)); return m < 60 ? m + ' min' : Math.floor(m / 60) + ' h ' + (m % 60) + ' min'; }
function agoText(v, now) {
  const t = ts(v); if (!t) return 'never';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return s + ' s ago';
  if (s < 3600) return Math.floor(s / 60) + ' min ago';
  if (s < 86400) return Math.floor(s / 3600) + ' h ago';
  return Math.floor(s / 86400) + ' d ago';
}
function bytes(n) {
  n = Number(n) || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(1) + ' GB';
  if (n >= 1e6) return Math.round(n / 1e6) + ' MB';
  return Math.round(n / 1e3) + ' KB';
}
function isDelivery(so) { return ((so.order.fulfillment || {}).type || '') === 'delivery'; }
function arrivedAt(so) { return ts(so.status.arrival_date) || ts(so.order.received_at) || ts(so.status.accepted_at); }
function byTime(get) { return (a, b) => (get(a) || Infinity) - (get(b) || Infinity); }
// Kitchen display size: the vendor placeholder "Serving" is hidden and
// "Serving (2 Piece)" shows "2 Piece" (agent displaySize / Quibase).
function displaySize(s) {
  s = String(s || '').trim();
  if (s.toLowerCase() === 'serving') return '';
  const m = /^serving\s*\((.+)\)$/i.exec(s);
  return m ? m[1].trim() : s;
}
// "2× 12 Medium Cheese"; a single item has no "1× " (Quibase).
function itemTitle(i) {
  const q = i.quantity > 1 ? i.quantity + '× ' : '';
  const size = displaySize(i.size);
  return q + (size ? size + ' ' : '') + (i.name || '');
}
// Item lines for display: items, then "+ modifier" children, then item notes.
function itemLines(snap) {
  const out = [];
  (snap.items || []).forEach((it, n) => {
    out.push({ k: 'i' + n, cls: 'item', text: itemTitle(it) });
    (it.children || []).forEach((c, m) => out.push({ k: 'i' + n + 'c' + m, cls: 'mod', text: '+ ' + itemTitle(c) }));
    if ((it.notes || '').trim()) out.push({ k: 'i' + n + 'n', cls: 'note', text: '“' + it.notes.trim() + '”' });
  });
  return out;
}
function itemCount(snap) { return (snap.items || []).reduce((n, i) => n + (i.quantity > 0 ? i.quantity : 1), 0); }
// The one cash rule (order.Snapshot.CashDue / Quibase cashDueCents): the cash
// payments; no payments at all = the whole total; card / other = paid.
function cashDue(snap) {
  const pays = snap.payments || [], total = (snap.money || {}).total_cents || 0;
  if (!pays.length) return total;
  return pays.filter(p => String(p.method || '').trim().toLowerCase() === 'cash').reduce((n, p) => n + (p.amount_cents || 0), 0);
}
function moneyLabel(snap) { const c = cashDue(snap); return c > 0 ? 'CASH ' + money(c) : 'PAID'; }
function custName(snap) { const n = ((snap.customer || {}).display_name || '').trim(); return n || 'Guest'; }
function ftype(snap) { return ((snap.fulfillment || {}).type || snap.source || '').replace(/_/g, ' '); }
// Delivery address on one line: "line1, line2, City, ST 12345" (Quibase Address.oneLine).
function addrLine(snap) {
  const a = (snap.fulfillment || {}).address || {}, j = (xs, sep) => xs.map(x => String(x || '').trim()).filter(Boolean).join(sep);
  return j([j([a.line1, a.line2], ', '), j([j([a.city, a.state], ', '), a.postal_code], ' ')], ', ');
}
// The number staff call: a POS order's check number (2.1.0), else the
// kitchen number, else the vendor's order number.
function displayNumber(so) {
  if (so.status.display_number != null) return '#' + so.status.display_number;
  const k = so.status.kitchen_number != null ? so.status.kitchen_number : so.order.kitchen_order_number;
  return k != null ? '#' + k : '#' + (so.order.source_event_id || String(so.order.id || '').slice(0, 8));
}
function isPOS(so) { return !!(so && so.status && so.status.ext_source); }
// 2.1.0 switches (Settings › Orders / Kitchen tickets), in display order.
const FEATURES = [
  ['pos_pull', 'POS orders pull-in', 'Phone and counter orders from the old POS show on the tablet. Off: the store hub stops pulling them.'],
  ['order_insert', 'Online order insert into the POS', 'The store hub keys accepted online cash pickups into the old POS. Off: staff key them by hand.'],
  ['driver_writeback', 'Driver write-back', 'Drivers sent out on the tablet are written onto their POS checks.'],
  ['address_heal', 'Address heal', 'An address staff confirm on the tablet is corrected in the POS customer record.'],
  ['future_orders', 'Future orders', 'Orders for later are confirmed once, wait in a Later strip and ring again when it is time. Off: they ring like any order.'],
  ['paid_wave', 'Paid-not-ready wave', 'Paid pickups that are not ready yet get a slow wave on the tablet’s Kitchen screen.'],
];
const TICKETS_POS = [
  ['pos_delivery', 'POS deliveries', 'Phone deliveries from the POS. The POS normally prints these itself.'],
  ['pos_takeout', 'POS takeout', 'Takeout orders from the POS.'],
  ['pos_counter', 'POS counter', 'Counter / dine-in orders from the POS.'],
];
const TICKETS_ONLINE = [
  ['online_cash_pickup', 'Online cash pickups', 'Only while the store hub keys them into the POS live — otherwise only the receipt prints, and staff key them by hand.'],
  ['online_cash_delivery', 'Online cash deliveries', 'Off: only the customer receipt prints.'],
  ['online_card', 'Online card orders', 'Paid online. Off: only the customer receipt prints.'],
];
function prefsKey(p) { return JSON.stringify(PREF_KEYS.map(k => (p ? p[k] : undefined) ?? null)); }
function readTheme() { try { const t = localStorage.getItem(THEME_KEY); return t === 'light' || t === 'dark' ? t : 'auto'; } catch { return 'auto'; } }
function saveTheme(t) { try { if (t === 'auto') localStorage.removeItem(THEME_KEY); else localStorage.setItem(THEME_KEY, t); } catch { /* storage blocked: the toggle still works for this visit */ } }
// Sound: browsers only start an AudioContext after a click/key on the page,
// so it is created (or resumed) on the first pointerdown/keydown; until then
// the UI shows an "Enable sound" chip. Kept out of Alpine's reactive data.
let audioCtx = null, lastBeep = 0;
function unlockAudio() {
  try {
    audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
    if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => { /* still blocked */ });
  } catch { /* no Web Audio */ }
  return audioCtx;
}
function soundReady() { return !!audioCtx && audioCtx.state === 'running'; }
function beep() {
  lastBeep = Date.now();
  if (!soundReady()) return;
  try {
    const o = audioCtx.createOscillator(), g = audioCtx.createGain();
    o.frequency.value = 880; o.connect(g); g.connect(audioCtx.destination); g.gain.value = 0.2;
    o.start(); o.stop(audioCtx.currentTime + 0.4);
  } catch { /* audio failed: the screen still shows the order */ }
}
async function rawPost(path, body) {
  const res = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) });
  let b = {}; try { b = await res.json(); } catch { /* empty body */ }
  return { res, body: b };
}

document.addEventListener('alpine:init', () => {
  window.Alpine.data('agentApp', agentApp);
});

function agentApp() {
  // Non-reactive handles (an EventSource or timer inside Alpine's reactive
  // proxy breaks: native methods throw "Illegal invocation").
  const priv = { es: null, esRetry: 0, reloadTimer: 0, flashTimer: 0, floorBusy: false, floorAgain: false, lastFocus: null, toastSeq: 0, baseTitle: document.title };

  return {
    // --- shell ---
    state: 'loading', session: {}, page: 'overview', settingsTab: 'orders', navOpen: false,
    theme: readTheme(), systemDark: false, now: Date.now(),
    toasts: [], dialog: null, flash: null,
    piReachable: true, soundOff: true, histStatuses: HIST_STATUSES,
    nav: NAV, settingsTabs: SETTINGS_TABS,
    numSizes: NUM_SIZES, itemSizes: ITEM_SIZES, footSizes: FOOT_SIZES, footModes: FOOT_MODES, maxFooter: MAX_FOOTER,
    featureList: FEATURES, ticketsPOS: TICKETS_POS, ticketsOnline: TICKETS_ONLINE,

    // --- provisioning / login ---
    link: { user: '', pass: '', busy: false, err: '' }, showToken: false,
    tok: { token: '', busy: false, err: '' },
    np: { pass: '', confirm: '', hint: '', busy: false, err: '' },
    login: { user: '', pass: '', busy: false, err: '', hint: '', hintShown: false },

    // --- floor (Live, Overview, Drivers) ---
    floor: null, orders: [], drivers: [], floorErr: '', floorLoaded: false,
    prep: {}, busy: {}, errs: {}, showDone: false, focusWait: '',

    // --- overview ---
    diag: null, diagErr: false, devices: [], devicesLoaded: false, appLatest: null, batches: [], reportBatch: 'current', reports: [], reportsErr: '',

    // --- drivers ---
    newDriver: '', newDriverNum: '', names: {}, origNames: {}, nums: {}, origNums: {}, reset: { time: '', last_run: null, form: '' },

    // --- history ---
    hist: { q: '', status: '', sort: 'desc', from: '', to: '', offset: 0, rows: [], more: false, loading: false, err: '', errRaw: '', loaded: false },

    // --- printers ---
    printers: [], printersErr: '', printerResults: {}, printerBusy: '',
    found: null, foundBusy: false,

    // --- Pi updates (Settings › Account, Overview Pi card) ---
    upd: null, updErr: '', updBusy: '',

    // --- settings ---
    autoAccept: false, autoAcceptLoaded: false, autoUpdate: true, autoUpdateLoaded: false, floorForm: { green: '', tiptax: '' }, deliveryForm: { soon: '', target: '' }, timesForm: { pickup: '', delivery: '' },
    rcpt: null, rcptForm: null, footMode: 'default', customFooter: '', rcptPreview: 'r', rcptErr: '', rcptBusy: '',
    pair: null, storeCfg: null, storeCfgErr: '', storeCfgErrRaw: '',
    cp: { current: '', next: '', confirm: '', hint: '', err: '', busy: false },

    // ======================================================================
    init() {
      try {
        const mq = window.matchMedia('(prefers-color-scheme: dark)');
        this.systemDark = mq.matches;
        mq.addEventListener('change', e => { this.systemDark = e.matches; });
      } catch { /* old browser: light */ }
      this.readHash(false);
      window.addEventListener('hashchange', () => this.readHash(true));
      setInterval(() => this.tick(), 1000);
      const unlock = () => { unlockAudio(); this.syncSound(); };
      document.addEventListener('pointerdown', unlock, true);
      document.addEventListener('keydown', unlock, true);
      setInterval(() => this.pollSession(), 5000);
      setInterval(() => this.refreshStatus(), 15000);
      document.addEventListener('keydown', e => { if (e.key === 'Escape') this.onEscape(); });
      this.refreshSession();
    },

    // --- theme ---------------------------------------------------------------
    get isDark() { return this.theme === 'dark' || (this.theme !== 'light' && this.systemDark); },
    get themeClass() { return this.theme === 'dark' ? 'dark' : this.theme === 'light' ? 'light' : ''; },
    toggleTheme() { this.theme = this.isDark ? 'light' : 'dark'; saveTheme(this.theme); },
    icon,

    // --- clock tick: repeat beep + tab title while orders wait ---------------
    tick() {
      this.now = Date.now();
      this.syncSound();
      const n = this.state === 'ready' ? this.waiting.length : 0;
      if (n && this.now - lastBeep >= REBEEP_MS) beep();
      const title = n ? '(' + n + ') NEW ORDER — ' + this.storeName : priv.baseTitle;
      if (document.title !== title) document.title = title;
    },
    syncSound() { const off = !soundReady(); if (this.soundOff !== off) this.soundOff = off; },
    enableSound() { unlockAudio(); setTimeout(() => { this.syncSound(); beep(); }, 50); },

    // --- toasts + dialogs ----------------------------------------------------
    toast(msg, isErr) {
      const id = ++priv.toastSeq;
      this.toasts = [...this.toasts.slice(-2), { id, msg: String(msg), err: !!isErr }];
      setTimeout(() => { this.toasts = this.toasts.filter(t => t.id !== id); }, isErr ? 6000 : 3500);
    },
    dropToast(id) { this.toasts = this.toasts.filter(t => t.id !== id); },
    // ask() shows a styled confirm (or, with cancelLabel null, a notice) and
    // resolves true on confirm.
    ask(opts) {
      if (this.dialog) this.closeDialog(false);
      priv.lastFocus = document.activeElement;
      return new Promise(resolve => {
        this.dialog = Object.assign({ title: '', body: '', list: [], confirmLabel: 'OK', cancelLabel: 'Cancel', danger: false, focusCancel: false, input: null, value: '' }, opts, { resolve });
        // Focus the safe choice: Cancel on a destructive (danger / focusCancel)
        // dialog, else OK.
        // (Retried briefly: the dialog renders on Alpine's next flush.)
        const focusSafe = (tries) => {
          const wrap = document.getElementById('dialog');
          if (!this.dialog || !wrap || wrap.contains(document.activeElement)) return;
          const pick = (this.dialog.input && wrap.querySelector('#dlg-input')) || ((this.dialog.danger || this.dialog.focusCancel) && wrap.querySelector('[data-role=cancel]')) || wrap.querySelector('[data-role=ok]');
          if (pick) pick.focus();
          if (tries > 0 && !wrap.contains(document.activeElement)) setTimeout(() => focusSafe(tries - 1), 30);
        };
        this.$nextTick(() => setTimeout(() => focusSafe(10), 0));
      });
    },
    closeDialog(ok) {
      const d = this.dialog; if (!d) return;
      this.dialog = null;
      // A dialog with an input resolves the typed text (null when cancelled).
      d.resolve(d.input ? (ok ? String(d.value || '') : null) : !!ok);
      const f = priv.lastFocus; priv.lastFocus = null;
      if (f && typeof f.focus === 'function' && document.contains(f)) setTimeout(() => f.focus(), 0);
    },
    // Keep Tab inside the open dialog.
    trapTab(e) {
      const els = [...document.querySelectorAll('#dialog input, #dialog button:not([disabled])')].filter(el => el.offsetParent !== null);
      if (!els.length) return;
      const first = els[0], last = els[els.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    },
    onEscape() {
      if (this.dialog) return this.closeDialog(false);
      if (this.flash) { this.flash = null; return; }
      if (this.navOpen) this.navOpen = false;
    },

    // --- API -----------------------------------------------------------------
    async api(path, opts = {}) {
      const init = Object.assign({}, opts, { headers: Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {}) });
      if (opts.body !== undefined && typeof opts.body !== 'string') init.body = JSON.stringify(opts.body);
      let res;
      try { res = await fetch(path, init); } catch {
        // fetch only throws when the Pi can't be reached (TypeError "Failed to fetch").
        this.pollSession();
        throw new ApiError(PI_DOWN, 0, 'network');
      }
      // 401 = session expired OR key revoked: re-check /api/session so the
      // screen lands on login (session gone) or setup (key revoked).
      if (res.status === 401) this.refreshSession();
      return res;
    },
    // call() returns the parsed body, or throws ApiError with the server's message.
    async call(path, opts) {
      const res = await this.api(path, opts);
      let body = null;
      if (res.status !== 204) {
        const text = await res.text();
        try { body = text ? JSON.parse(text) : null; } catch { body = null; }
      }
      if (!res.ok) {
        // A 401 that names its own reason (e.g. a wrong current password) keeps
        // it; a bare session/device rejection reads as signed out.
        const own = body && body.code && body.code !== 'device_required';
        const msg = res.status === 401 && !own ? 'Signed out — log in again.' : (body && body.error) || ('Request failed (' + res.status + ')');
        throw new ApiError(msg, res.status, body && body.code, body);
      }
      return body;
    },

    // --- session / state machine ----------------------------------------------
    async refreshSession() {
      try {
        const s = await (await fetch('/api/session', { cache: 'no-store' })).json();
        this.session = s;
        this.setReachable(true);
        this.setState(s.state);
      } catch {
        this.setReachable(false);
        if (this.state === 'loading') this.state = 'locked';
      }
    },
    // pollSession keeps the sidebar light fresh and, if a background revoke
    // reset us (ready → unconfigured/locked), bounces out of the dead app.
    async pollSession() {
      try {
        const s = await (await fetch('/api/session', { cache: 'no-store' })).json();
        this.session = s;
        this.setReachable(true);
        if (s.state !== 'ready' && this.state === 'ready') this.setState(s.state);
      } catch { this.setReachable(false); /* keep the last state */ }
    },
    // The browser lost / regained the Pi. Coming back reloads the floor (it
    // may have changed meanwhile) and restarts the event stream.
    setReachable(ok) {
      if (ok === this.piReachable) return;
      this.piReachable = ok;
      if (ok && this.state === 'ready') { this.startEvents(); this.scheduleReload(); }
    },
    setState(st) {
      const was = this.state;
      this.state = st;
      if (st === 'unconfigured' && this.session.link_available === false) this.showToken = true;
      if (st === 'ready' && was !== 'ready') this.enterApp();
      if (st !== 'ready' && was === 'ready') this.leaveApp();
    },
    enterApp() {
      this.startEvents();
      this.loadFloor();
      this.loadPage();
    },
    leaveApp() {
      this.stopEvents();
      if (this.dialog) this.closeDialog(false);
      this.flash = null; this.navOpen = false;
      this.orders = []; this.drivers = []; this.floor = null; this.floorLoaded = false;
      this.diag = null; this.devices = []; this.reports = []; this.printers = []; this.storeCfg = null;
      this.rcpt = null; this.rcptForm = null; this.pair = null; this.hist.rows = []; this.hist.loaded = false;
      this.login.pass = ''; this.login.hintShown = false;
    },
    get connTone() {
      if (!this.piReachable) return 'bad';
      const s = this.session.sse_state;
      return s === 'connected' ? 'ok' : s === 'connecting' ? 'warn' : s === 'rejected' ? 'bad' : 'mute';
    },
    get connLabel() {
      if (!this.piReachable) return 'NO PI';
      return { connected: 'LIVE', connecting: 'RECONNECTING' }[this.session.sse_state] || 'OFFLINE';
    },
    get mainDown() { return this.piReachable && this.session.sse_state !== 'connected'; },
    get connTitle() {
      if (!this.piReachable) return PI_DOWN;
      const s = this.session;
      const since = s.sse_connected_since ? clock(s.sse_connected_since * 1000) : '';
      switch (s.sse_state) {
        case 'connected': return 'Connected to main' + (since ? ' since ' + since : '');
        case 'connecting': return 'Connecting to main' + (s.sse_reconnects ? ' (' + s.sse_reconnects + ' attempts)' : '') + (s.sse_last_error ? ' — ' + s.sse_last_error : '');
        case 'rejected': return s.sse_last_error || 'Token revoked by main — set the Pi up again';
        default: return 'Not linked to a store yet';
      }
    },
    get storeName() { return this.session.store_name || (this.storeCfg && this.storeCfg.location && this.storeCfg.location.name) || 'Quicc POS'; },

    // --- provisioning + login ---------------------------------------------------
    async submitLink() {
      this.link.err = '';
      const username = this.link.user.trim(), password = this.link.pass;
      if (!username || !password) { this.link.err = 'Type the username and password.'; return; }
      this.link.busy = true;
      try {
        const { res, body } = await rawPost('/api/setup/link', { username, password });
        if (!res.ok) throw new Error(body.error || ('Link failed (' + res.status + ')'));
        this.link.pass = '';
        await this.refreshSession();
        const r = body.restored;
        const where = [body.tenant, body.location].filter(Boolean).join(' — ') || 'your store';
        this.ask({
          title: 'Linked to ' + where,
          body: r ? 'This Pi brought back the open batch from the server:' : 'Nothing to restore — a fresh batch is open.',
          list: r ? [
            r.orders + ' order' + (r.orders === 1 ? '' : 's') + ' restored (' + r.waiting + ' waiting to be accepted)',
            r.drivers + ' driver' + (r.drivers === 1 ? '' : 's'),
            'Next kitchen # ' + r.kitchen_next,
          ] : [],
          confirmLabel: 'Open the dashboard', cancelLabel: null,
        });
      } catch (e) {
        this.link.err = e.message === 'Failed to fetch' ? "Can't reach this Pi — check the connection." : e.message;
      } finally { this.link.busy = false; }
    },
    async submitToken() {
      this.tok.err = '';
      const token = this.tok.token.trim();
      if (!token) { this.tok.err = 'Paste the token first.'; return; }
      this.tok.busy = true;
      try {
        const { res, body } = await rawPost('/api/setup/token', { token });
        if (!res.ok) throw new Error(body.error || ('Link failed (' + res.status + ')'));
        this.tok.token = '';
        await this.refreshSession(); // → create a password
      } catch (e) { this.tok.err = e.message; } finally { this.tok.busy = false; }
    },
    async submitNewPassword() {
      this.np.err = '';
      const password = this.np.pass, confirm = this.np.confirm, hint = this.np.hint.trim();
      if (!password) { this.np.err = 'Choose a password.'; return; }
      if (password !== confirm) { this.np.err = "The passwords don't match."; return; }
      this.np.busy = true;
      try {
        const { res, body } = await rawPost('/api/setup/password', { password, confirm, hint });
        if (!res.ok) throw new Error(body.error || ('Failed (' + res.status + ')'));
        this.np.pass = this.np.confirm = this.np.hint = '';
        await this.refreshSession(); // → the app
      } catch (e) { this.np.err = e.message; } finally { this.np.busy = false; }
    },
    async submitLogin() {
      this.login.err = '';
      this.login.busy = true;
      try {
        const { res, body } = await rawPost('/api/login', { username: this.login.user.trim(), password: this.login.pass });
        if (!res.ok) throw new Error(body.error || ('Login failed (' + res.status + ')'));
        this.login.pass = '';
        await this.refreshSession(); // → the app
      } catch (e) { this.login.err = e.message; } finally { this.login.busy = false; }
    },
    async showHint() {
      try {
        const b = await (await fetch('/api/hint')).json();
        this.login.hint = b.hint ? 'Hint: ' + b.hint : 'No hint was set.';
      } catch { this.login.hint = 'Could not load the hint.'; }
      this.login.hintShown = true;
    },
    async logout() {
      try { await fetch('/api/logout', { method: 'POST' }); } catch { /* the session check below decides */ }
      await this.refreshSession();
    },

    // --- routing -----------------------------------------------------------------
    readHash(fromEvent) {
      const parts = (location.hash || '').replace(/^#\/?/, '').split('/');
      const page = PAGES.includes(parts[0]) ? parts[0] : 'overview';
      const sub = SETTINGS_TABS.some(t => t.id === parts[1]) ? parts[1] : this.settingsTab;
      if (fromEvent && page === this.page && sub === this.settingsTab) return;
      this.page = page; this.settingsTab = sub;
      this.navOpen = false;
      if (this.state === 'ready') this.loadPage();
    },
    go(page, sub) {
      const hash = '#/' + page + (page === 'settings' ? '/' + (sub || this.settingsTab) : '');
      this.navOpen = false;
      if (location.hash === hash) { this.loadPage(); return; }
      location.hash = hash; // → hashchange → readHash → loadPage
      window.scrollTo(0, 0);
    },
    loadPage() {
      switch (this.page) {
        case 'overview': this.loadOverview(); break;
        case 'live': this.loadFloor(); break;
        case 'drivers': this.loadFloor(); this.loadReset(); break;
        case 'history': this.hist.offset = 0; this.loadHistory(); break;
        case 'printers': this.loadPrinters(); break;
        case 'settings': this.loadSettingsTab(); break;
      }
    },
    refreshStatus() {
      if (this.state !== 'ready' || document.hidden) return;
      if (this.page === 'overview') { this.loadDiag(); this.loadDevices(); }
      if (this.page === 'printers') this.loadPrinters();
      if (this.page === 'settings' && this.settingsTab === 'account') this.loadUpdate();
    },
    get pageTitle() { return (NAV.find(n => n.id === this.page) || NAV[0]).label; },

    // --- live updates (agent SSE, session cookie) ----------------------------------
    startEvents() {
      if (priv.es || this.state !== 'ready') return;
      const es = new EventSource('/api/events');
      priv.es = es;
      const reload = () => this.scheduleReload();
      // (Re)connected: reload — events sent while the stream was down are lost.
      es.onopen = reload;
      ['order.arrived', 'order.escalated'].forEach(t => es.addEventListener(t, () => { beep(); reload(); }));
      // POS orders (order.added) never ring.
      ['order.updated', 'order.added', 'drivers.updated', 'batch.closed', 'floor.updated', 'settings.updated', 'integration.status'].forEach(t => es.addEventListener(t, reload));
      es.addEventListener('integration.toast', e => {
        try { const d = JSON.parse(e.data); this.toast(d.text, d.severity === 'serious'); } catch { /* malformed frame */ }
      });
      es.addEventListener('drivers.dispatched', e => {
        try {
          const d = JSON.parse(e.data);
          this.showFlash(d.driver && d.driver.name, (d.orders || []).map(displayNumber));
        } catch { /* malformed frame */ }
        reload();
      });
      es.onerror = () => {
        es.close();
        if (priv.es === es) priv.es = null;
        clearTimeout(priv.esRetry);
        priv.esRetry = setTimeout(() => this.startEvents(), 5000);
      };
    },
    stopEvents() {
      clearTimeout(priv.esRetry);
      if (priv.es) { priv.es.close(); priv.es = null; }
    },
    scheduleReload() {
      clearTimeout(priv.reloadTimer);
      priv.reloadTimer = setTimeout(() => {
        this.loadFloor();
        if (this.page === 'overview') { this.loadReports(); this.loadBatches(); }
      }, 150);
    },
    showFlash(name, nums) {
      this.flash = { name: name || 'Driver', nums: nums || [] };
      clearTimeout(priv.flashTimer);
      priv.flashTimer = setTimeout(() => { this.flash = null; }, 8000);
    },

    // --- floor data -------------------------------------------------------------------
    async loadFloor() {
      if (this.state !== 'ready') return;
      if (priv.floorBusy) { priv.floorAgain = true; return; }
      priv.floorBusy = true;
      try {
        const [f, d, o] = await Promise.all([
          this.call('/api/floor'), this.call('/api/drivers'), this.call('/api/orders?batch=current&limit=200'),
        ]);
        this.floor = f; this.drivers = d || []; this.orders = o || [];
        this.floorErr = ''; this.floorLoaded = true;
        this.syncFloorUI();
      } catch (e) {
        this.floorErr = e.message;
      } finally {
        priv.floorBusy = false;
        if (priv.floorAgain) { priv.floorAgain = false; this.loadFloor(); }
      }
    },
    syncFloorUI() {
      for (const so of this.waiting) if (!this.prep[so.order.id]) this.prep[so.order.id] = this.defaultPrep;
      for (const d of this.drivers) {
        if (this.names[d.id] === undefined || this.names[d.id] === this.origNames[d.id]) this.names[d.id] = d.name;
        this.origNames[d.id] = d.name;
        const n = d.pos_number || '';
        if (this.nums[d.id] === undefined || this.nums[d.id] === this.origNums[d.id]) this.nums[d.id] = n;
        this.origNums[d.id] = n;
      }
    },
    get prepChoices() {
      const c = this.floor && Array.isArray(this.floor.prep_choices) ? this.floor.prep_choices.filter(n => n > 0) : [];
      return c.length ? c : DEFAULT_PREPS;
    },
    // Last used prep if it's a choice, else the closest one (Quibase defaultPrep).
    get defaultPrep() {
      const c = this.prepChoices, last = this.floor ? Number(this.floor.last_prep_minutes) || 0 : 0;
      if (c.includes(last)) return last;
      if (last <= 0) return c[0];
      return c.reduce((b, x) => (Math.abs(x - last) < Math.abs(b - last) ? x : b), c[0]);
    },
    get greenTill() { const g = this.floor && Number(this.floor.green_till_minutes); return g > 0 ? g : DEFAULT_GREEN; },
    // Oldest first (Quibase FloorState.arrived: arrival_date ?? received_at).
    get waiting() { return this.orders.filter(so => so.status.state === 'arrived').sort(byTime(arrivedAt)); },
    // The full alert card: the order picked with "Show", else the oldest.
    get alertOrder() { const w = this.waiting; return w.find(so => so.order.id === this.focusWait) || w[0] || null; },
    get alsoWaiting() { const a = this.alertOrder; return this.waiting.filter(so => so !== a); },
    isStill(so) { const t = arrivedAt(so); return !!t && this.now - t >= STILL_WAITING_MS; },
    get waitingText() { const n = this.waiting.length; return n + (n === 1 ? ' ORDER' : ' ORDERS') + ' WAITING'; },
    // Most urgent first: earliest promised ready time (no estimate last), then
    // kitchen number, then accepted time (Quibase kitchenByUrgency).
    get kitchen() {
      return this.orders.filter(so => so.status.state === 'accepted' && !so.status.board_hidden).sort((a, b) => {
        const ea = ts(a.status.estimated_ready_at), eb = ts(b.status.estimated_ready_at);
        if (ea && eb && ea !== eb) return ea - eb;
        if (!ea !== !eb) return ea ? -1 : 1;
        const ka = a.status.kitchen_number, kb = b.status.kitchen_number;
        if (ka != null && kb != null && ka !== kb) return ka - kb;
        return (ts(a.status.accepted_at) || Infinity) - (ts(b.status.accepted_at) || Infinity);
      });
    },
    // Deliveries no driver's pool holds yet (cooking + ready), oldest placed first.
    get unassignedDeliveries() {
      return this.orders.filter(so => isDelivery(so) && (so.status.state === 'accepted' || so.status.state === 'ready') && !so.status.pool_driver_id)
        .sort(byTime(so => ts(so.order.placed_at) || arrivedAt(so)));
    },
    // Read-only pools: one per available driver in queue order (next on top).
    get driverPools() {
      return this.availableDrivers.map((d, i) => ({
        d, next: i === 0,
        orders: this.orders.filter(so => so.status.pool_driver_id === d.id && (so.status.state === 'accepted' || so.status.state === 'ready'))
          // Stop order (pool_seq: the plan's or the fastest driving order); unnumbered last.
          .sort((a, b) => (a.status.pool_seq || 1e9) - (b.status.pool_seq || 1e9) || (ts(a.status.pooled_at) - ts(b.status.pooled_at))),
      }));
    },
    street(snap) { const a = snap && snap.fulfillment && snap.fulfillment.address; return (a && a.line1 ? a.line1 : '').trim(); },
    get onDelivery() { return this.orders.filter(so => so.status.state === 'on_delivery'); },
    get doneOrders() {
      return this.orders.filter(so => {
        const st = so.status.state;
        return st === 'delivered' || st === 'cancelled' || (st === 'ready' && !isDelivery(so));
      });
    },
    get availableDrivers() { return this.drivers.filter(d => d.status === 'available'); },
    get outDrivers() {
      return this.drivers.filter(d => d.status === 'out').map(d => {
        const os = this.onDelivery.filter(so => so.status.driver_id === d.id).sort(byTime(so => ts(so.status.out_at)));
        return { d, orders: os, outAt: os.length ? ts(os[0].status.out_at) : 0 };
      });
    },
    get batchOpenHours() { return this.floor && this.floor.batch ? (this.now - ts(this.floor.batch.opened_at)) / 3600000 : 0; },

    // Display helpers used by the markup.
    money, clock, dateTime, moneyLabel, custName, displayNumber, itemLines, itemCount, mins, dur, addrLine, isPOS,
    stateLabel(st) { return { delivered: 'Delivered', cancelled: 'Cancelled', ready: 'Ready for pickup', on_delivery: 'Out', accepted: 'Cooking', arrived: 'Waiting', scheduled: 'Later' }[st] || st; },
    // Future orders confirmed once (they ring again at ring_at), soonest first.
    get laterOrders() { return this.orders.filter(so => so.status.state === 'scheduled').sort(byTime(so => ts(so.status.ring_at))); },
    ftype(snap) { return ftype(snap); },
    ago(v) { return agoText(v, this.now); },
    sinceText(so) { const t = arrivedAt(so); return t ? dur(this.now - t) : '—'; },
    // Ticket tone from age vs the green threshold g: green ≤ g, green→amber
    // to 1.5g, amber→red to 2g, red after (Quibase lateColor).
    toneStyle(so) {
      const t0 = arrivedAt(so), g = this.greenTill;
      const m = t0 ? (this.now - t0) / 60000 : 0;
      if (m <= g) return '';
      const t = Math.min(1, (m - g) / g);
      const mix = (a, b, p) => 'color-mix(in srgb, var(' + b + ') ' + Math.round(p * 100) + '%, var(' + a + '))';
      if (t < 0.5) return '--tone:' + mix('--t-green', '--t-amber', t * 2) + ';--ink:' + mix('--i-green', '--i-amber', t * 2);
      return '--tone:' + mix('--t-amber', '--t-red', (t - 0.5) * 2) + ';--ink:' + mix('--i-amber', '--i-red', (t - 0.5) * 2);
    },
    dueText(so) {
      const eta = ts(so.status.estimated_ready_at); if (!eta) return '';
      const left = eta - this.now;
      return left < 0 ? 'LATE ' + mins(Math.ceil(-left / 60000)) : mins(left / 60000) + ' left · ' + clock(eta);
    },
    isLate(so) { const eta = ts(so.status.estimated_ready_at); return !!eta && eta < this.now; },

    // --- floor actions -------------------------------------------------------------
    async accept(so, prepMinutes) {
      const id = so.order.id, key = 'acc' + id;
      if (this.busy[key]) return;
      const minutes = prepMinutes || this.prep[id] || this.defaultPrep;
      this.busy[key] = true; this.errs[id] = '';
      try {
        await this.call('/api/orders/' + id + '/accept', { method: 'POST', body: { prep_minutes: minutes } });
        this.toast('Accepted ' + custName(so.order) + ' · ' + minutes + ' min');
      } catch (e) {
        this.errs[id] = this.acceptError(e);
      } finally {
        this.busy[key] = false;
        this.loadFloor();
      }
    },
    acceptError(e) {
      if (!(e instanceof ApiError) || e.code === 'network') return "Couldn't reach the Pi — check the kitchen list before trying again.";
      if (e.status === 502) return 'Printer problem — order NOT accepted. Check the printers and try again.';
      if (e.code === 'already_accepted') return 'Already accepted on another screen.';
      if (e.code === 'invalid_state') return "This order was cancelled or closed with its batch — it can't be accepted.";
      if (e.status === 404) return 'This order is no longer on the agent.';
      return 'Not accepted. ' + e.message;
    },
    async resendOrder(so) {
      const key = 'rs' + so.order.id;
      if (this.busy[key]) return;
      this.busy[key] = true;
      try {
        await this.call('/api/orders/' + so.order.id + '/resend', { method: 'POST' });
        this.toast('Rang the tablet again');
      } catch (e) { this.toast("Couldn't ring the tablet. " + e.message, true); } finally { this.busy[key] = false; }
    },
    async markReady(so) {
      const key = 'rd' + so.order.id;
      if (this.busy[key]) return;
      this.busy[key] = true;
      try {
        await this.call('/api/orders/' + so.order.id + '/ready', { method: 'POST' });
      } catch (e) {
        this.toast(e.status === 409 ? 'Already moved on — the screen will refresh.' : 'Not marked ready. ' + e.message, true);
      } finally { this.busy[key] = false; this.loadFloor(); }
    },
    async reprint(so) {
      const key = 'rp' + so.order.id;
      if (this.busy[key]) return;
      this.busy[key] = true;
      try {
        await this.call('/api/orders/' + so.order.id + '/reprint', { method: 'POST' });
        this.toast('Receipt reprinted · ' + displayNumber(so));
      } catch (e) {
        this.toast(e.status === 502 ? 'Printer problem — nothing printed. ' + e.message : 'Not reprinted. ' + e.message, true);
      } finally { this.busy[key] = false; }
    },
    async driverReturned(d) {
      const key = 'ret' + d.id;
      if (this.busy[key]) return;
      this.busy[key] = true;
      try {
        await this.call('/api/drivers/' + d.id + '/returned', { method: 'POST' });
        this.toast(d.name + ' is back — end of the queue');
      } catch (e) { this.toast(e.status === 409 ? 'Already back — the screen will refresh.' : e.message, true); }
      finally { this.busy[key] = false; this.loadFloor(); }
    },

    // --- overview -------------------------------------------------------------------
    loadOverview() {
      this.loadFloor(); this.loadDiag(); this.loadDevices(); this.loadAppLatest(); this.loadBatches(); this.loadReports(); this.loadUpdate();
    },
    async loadDiag() {
      try { this.diag = await this.call('/api/diag'); this.diagErr = false; } catch { this.diagErr = true; /* cards: "Couldn't check" */ }
    },
    async loadDevices() {
      try { this.devices = (await this.call('/api/devices')) || []; this.devicesLoaded = true; } catch { /* keep */ }
    },
    async loadAppLatest() {
      try {
        const res = await this.api('/api/app/latest');
        this.appLatest = res.status === 200 ? await res.json() : null;
      } catch { this.appLatest = null; }
    },
    async loadBatches() {
      try {
        this.batches = (await this.call('/api/batches')) || [];
        if (this.reportBatch !== 'current' && !this.batches.some(b => String(b.id) === String(this.reportBatch))) this.reportBatch = 'current';
      } catch { /* keep the old list */ }
    },
    async loadReports() {
      try { this.reports = (await this.call('/api/batches/' + this.reportBatch + '/driver-reports')) || []; this.reportsErr = ''; }
      catch (e) { this.reportsErr = e.message; }
    },
    get counts() {
      const c = { waiting: 0, cooking: 0, ready: 0, out: 0, done: 0 };
      for (const so of this.orders) {
        const st = so.status.state;
        if (st === 'arrived') c.waiting++;
        else if (st === 'accepted') c.cooking++;
        else if (st === 'ready') { if (isDelivery(so)) c.ready++; else c.done++; }
        else if (st === 'on_delivery') c.out++;
        else if (st === 'delivered') c.done++;
      }
      return c;
    },
    // Money this batch over accepted orders (not waiting, not cancelled):
    // cash due by the one cash rule; card & prepaid = the rest of the total.
    get batchMoney() {
      const m = { cash: 0, card: 0, tips: 0, orders: 0 };
      for (const so of this.orders) {
        const st = so.status.state;
        if (st === 'arrived' || st === 'cancelled') continue;
        const total = (so.order.money || {}).total_cents || 0, cash = Math.min(cashDue(so.order), total);
        m.cash += cash; m.card += total - cash; m.tips += (so.order.money || {}).tip_cents || 0; m.orders++;
      }
      return m;
    },
    get healthCards() {
      const cards = [
        Object.assign({ k: 'Main' }, this.mainCard), Object.assign({ k: 'Printers' }, this.printersCard),
        Object.assign({ k: 'Tablet' }, this.tabletCard), Object.assign({ k: 'Pi' }, this.piCard),
      ];
      if (this.hasPOS) cards.push(Object.assign({ k: 'POS link' }, this.posCard));
      return cards;
    },
    get integ() { return (this.floor && this.floor.integration) || null; },
    get settingsAll() { return (this.floor && this.floor.settings) || null; },
    // The store has a POS link: location.config.pos_integration, a paired
    // integration device, or a hub that ever reported.
    get hasPOS() {
      const cfg = this.storeCfg && this.storeCfg.location && this.storeCfg.location.config;
      return !!((cfg && cfg.pos_integration) || this.devices.some(d => d.kind === 'integration' && !d.revoked_at) || (this.integ && this.integ.last_seen_at));
    },
    get posCard() {
      const v = this.integ;
      if (!v || !v.last_seen_at) return { tone: 'mute', t: 'Not reporting yet', s: 'The store hub has not checked in' };
      const probs = v.problems || [], serious = probs.some(p => p.severity === 'serious');
      const mode = v.insert_live ? 'online insert LIVE' : 'online insert ' + (v.insert_mode || 'off');
      if (!v.connected) return { tone: 'bad', t: 'POS link down', s: 'last seen ' + agoText(ts(v.last_seen_at), this.now) };
      return { tone: serious ? 'bad' : (probs.length || !v.ok) ? 'warn' : 'ok', t: probs.length ? probs[0].text : 'Connected', s: mode + ' · seen ' + agoText(ts(v.last_seen_at), this.now) };
    },
    get systemProblems() { return this.attention.filter(a => a.k[0] !== 'w').length; },
    get mainCard() {
      const s = this.session, drops = s.sse_reconnects || 0;
      switch (s.sse_state) {
        case 'connected': return { tone: 'ok', t: 'Connected', s: 'since ' + clock((s.sse_connected_since || 0) * 1000) + ' · ' + drops + ' drop' + (drops === 1 ? '' : 's') };
        case 'connecting': return { tone: 'warn', t: 'Connecting…', s: (s.sse_last_error || 'trying again') + (drops ? ' · ' + drops + ' attempts' : '') };
        case 'rejected': return { tone: 'bad', t: 'Token revoked', s: 'Set this Pi up again' };
        default: return { tone: 'mute', t: 'Not linked', s: '' };
      }
    },
    get printersCard() {
      const ps = (this.diag && this.diag.printers) || [];
      if (this.diagErr) return { tone: 'warn', t: "Couldn't check", s: 'retrying…' };
      if (!this.diag) return { tone: 'mute', t: '—', s: 'checking…' };
      if (!ps.length) return { tone: 'warn', t: 'No printers', s: 'Add them in main → Store' };
      const up = ps.filter(p => p.up).length, checked = Math.max(0, ...ps.map(p => ts(p.last_checked)));
      return { tone: up === ps.length ? 'ok' : 'bad', t: up + ' / ' + ps.length + ' ready', s: checked ? 'checked ' + agoText(checked, this.now) : 'not checked yet' };
    },
    get tabletCard() {
      const live = this.devices.filter(d => !d.revoked_at).sort((a, b) => ts(b.last_seen_at) - ts(a.last_seen_at));
      const app = this.appLatest ? 'Quibase ' + this.appLatest.version_name + ' cached' : '';
      if (!live.length) return { tone: 'mute', t: this.devicesLoaded ? 'No tablet paired' : '—', s: app || 'Pair one in Settings → Tablets' };
      const seen = ts(live[0].last_seen_at), fresh = seen && this.now - seen < 5 * 60000;
      const t = live.length === 1 ? live[0].name : live.length + ' tablets';
      return { tone: fresh ? 'ok' : 'warn', t, s: (seen ? 'seen ' + agoText(seen, this.now) : 'never seen') + (app ? ' · ' + app : '') };
    },
    get piCard() {
      if (this.diagErr) return { tone: 'warn', t: "Couldn't check", s: 'retrying…' };
      if (!this.diag) return { tone: 'mute', t: '—', s: 'checking…' };
      const h = this.diag.health || {}, bits = [];
      if (h.temp_c) bits.push(Math.round(h.temp_c) + '°C');
      if (h.disk_free_bytes) bits.push(bytes(h.disk_free_bytes) + ' free');
      if (h.ntp_synced === 'yes') bits.push('clock synced');
      else if (h.ntp_synced === 'no') bits.push('clock NOT synced');
      if (this.upd && this.upd.update_available) bits.unshift('update available');
      const probs = (this.diag.problems || []).filter(p => ['sd_errors', 'rootfs_ro', 'under_voltage', 'ntp', 'health_stale'].includes(p));
      const bad = probs.some(p => p === 'sd_errors' || p === 'rootfs_ro');
      return { tone: bad ? 'bad' : probs.length ? 'warn' : 'ok', t: probs.length ? (probs.length === 1 ? 'Needs a look' : probs.length + ' problems') : 'Healthy', s: bits.join(' · ') || 'no health report on this machine' };
    },
    // Needs attention: waiting orders, diag problems, the sync backlog.
    get attention() {
      const out = [];
      // One row for every waiting order (keys starting 'w' aren't "system" problems).
      const w = this.waiting;
      if (w.length === 1) out.push({ k: 'w', tone: 'warn', text: custName(w[0].order) + ' is waiting ' + this.sinceText(w[0]) + ' — not accepted yet', go: 'live' });
      else if (w.length) out.push({ k: 'w', tone: 'warn', text: w.length + ' orders waiting — oldest ' + this.sinceText(w[0]), go: 'live' });
      if (this.diagErr) out.push({ k: 'diag', tone: 'warn', text: "Couldn't check the printers and the Pi — retrying.", go: '' });
      for (const p of (this.diag && this.diag.problems) || []) out.push({ k: 'p' + p, tone: p === 'main_down' || p === 'printer_down' || p === 'sd_errors' || p === 'rootfs_ro' ? 'bad' : 'warn', text: PROBLEM_TEXT[p] || p, go: p === 'printer_down' ? 'printers' : '' });
      if (this.integ && this.integ.last_seen_at) {
        for (const p of this.integ.problems || []) out.push({ k: 'pos' + (p.code || p.text), tone: p.severity === 'serious' ? 'bad' : 'warn', text: 'POS link: ' + p.text, go: '' });
      }
      const backlog = (this.diag && this.diag.floor_backlog) || (this.floor && this.floor.unsynced_events) || 0;
      if (backlog > 0) out.push({ k: 'sync', tone: 'warn', text: backlog + ' floor change' + (backlog === 1 ? '' : 's') + ' waiting to sync to main', go: '' });
      const acks = (this.diag && this.diag.acks_pending) || 0;
      if (acks > 0) out.push({ k: 'acks', tone: 'warn', text: acks + ' order receipt' + (acks === 1 ? '' : 's') + ' waiting to be confirmed to main', go: '' });
      if (this.batchOpenHours > 24) out.push({ k: 'old', tone: 'warn', text: 'This batch has been open over a day — close it?', go: '' });
      return out;
    },
    // Drivers table: one row per driver report, with the live queue status.
    get driverRows() {
      const firstAvail = this.availableDrivers[0];
      return this.reports.map(r => {
        const d = this.drivers.find(x => x.id === r.driver_id);
        let badge = { cls: 'b-mute', text: d ? 'Clocked out' : 'Removed' };
        if (d && d.status === 'out') { const n = this.onDelivery.filter(so => so.status.driver_id === d.id).length; badge = { cls: 'b-info', text: 'Out · ' + n + ' order' + (n === 1 ? '' : 's') }; }
        else if (d && d.status === 'available') badge = firstAvail && firstAvail.id === d.id ? { cls: 'b-ok', text: 'Next up' } : { cls: 'b-ok', text: 'In queue' };
        const owed = r.owed_cents || 0;
        return {
          id: r.driver_id, name: r.driver_name, badge, deliveries: r.deliveries, cash: money(r.cash_cents), tips: money(r.tip_after_tax_cents),
          settle: owed > 0 ? 'owes ' + money(owed) : owed < 0 ? 'is owed ' + money(-owed) : 'settled', owedTone: owed < 0 ? 'accent' : '',
        };
      });
    },
    get batchSubtitle() {
      if (!this.floor || !this.floor.batch) return this.floorErr || 'Loading…';
      const b = this.floor.batch;
      return 'Batch ' + b.id + ' open since ' + new Date(ts(b.opened_at)).toLocaleString([], { weekday: 'short', hour: 'numeric', minute: '2-digit' }) + ' · next kitchen # ' + this.floor.kitchen_next;
    },
    async printReports() {
      if (this.busy.reports) return;
      this.busy.reports = true;
      const id = this.reportBatch || 'current';
      try {
        const res = await this.api('/api/batches/' + id + '/driver-reports/print', { method: 'POST' });
        const b = await res.json().catch(() => ({}));
        if (!res.ok) { this.toast(b.error || 'Printer problem — nothing printed', true); return; }
        if (!b.printed.length && !b.failed.length) { this.toast('No drivers in this batch'); return; }
        this.toast('Printed ' + b.printed.length + ' report' + (b.printed.length === 1 ? '' : 's') + (b.failed.length ? ' — ' + b.failed.length + ' FAILED' : ''), b.failed.length > 0);
      } catch (e) { this.toast(e.message, true); } finally { this.busy.reports = false; }
    },
    async closeBatch() {
      if (this.busy.close) return;
      const ok = await this.ask({
        title: 'Close the batch?',
        body: 'Kitchen numbers restart at 1100 and a report prints for each driver.',
        confirmLabel: 'Close batch', cancelLabel: 'Keep it open', focusCancel: true,
      });
      if (ok) await this.doCloseBatch(false);
    },
    async doCloseBatch(force) {
      this.busy.close = true;
      try {
        const res = await this.api('/api/batch/close', { method: 'POST', body: { force } });
        const b = await res.json().catch(() => ({}));
        if (res.status === 409 && b.code === 'batch_has_leftovers') {
          const list = (b.leftovers || []).map(so => displayNumber(so) + ' ' + custName(so.order) + ' — ' + ({ arrived: 'waiting to be accepted', accepted: 'still cooking', ready: 'ready, not sent out', on_delivery: 'still out with a driver' }[so.status.state] || so.status.state));
          this.busy.close = false;
          const again = await this.ask({
            title: (b.leftovers || []).length + ' order' + ((b.leftovers || []).length === 1 ? " isn't" : "s aren't") + ' finished',
            body: 'Closing anyway marks orders in progress as done; orders nobody accepted are CANCELLED.',
            list, confirmLabel: 'Close anyway', cancelLabel: 'Keep it open', danger: true,
          });
          if (again) return this.doCloseBatch(true);
          return;
        }
        if (!res.ok) { this.toast(b.error || 'Close failed', true); return; }
        this.toast('Batch ' + b.closed.id + ' closed — printing driver reports');
        this.reportBatch = 'current';
        this.loadFloor(); this.loadBatches(); this.loadReports();
      } catch (e) { this.toast(e.message, true); } finally { this.busy.close = false; }
    },

    // --- drivers page ---------------------------------------------------------------
    async loadReset() {
      try { const r = await this.call('/api/settings/driver-reset'); this.reset = { time: r.time, last_run: r.last_run || null, form: r.time }; } catch { /* keep */ }
    },
    async addDriver() {
      const name = this.newDriver.trim(), num = this.newDriverNum.trim(); if (!name) return;
      try {
        await this.call('/api/drivers', { method: 'POST', body: { name, pos_number: num } });
        this.newDriver = ''; this.newDriverNum = ''; this.toast('Added ' + name);
      } catch (e) { this.toast(e.code === 'pos_number_taken' ? 'Another driver already has Driver # ' + num + '.' : e.message, true); } finally { this.loadFloor(); }
    },
    // Driver # = the driver's login number on the store's POS (the hub matches by it).
    async saveDriverNum(d) {
      const num = (this.nums[d.id] || '').trim(); if (num === (d.pos_number || '')) return;
      try {
        await this.call('/api/drivers/' + d.id, { method: 'PATCH', body: { pos_number: num } });
        this.origNums[d.id] = num; this.toast(num ? d.name + ' is Driver # ' + num : 'Driver # cleared');
      } catch (e) { this.toast(e.code === 'pos_number_taken' ? 'Another driver already has Driver # ' + num + '.' : e.message, true); } finally { this.loadFloor(); }
    },
    async renameDriver(d) {
      const name = (this.names[d.id] || '').trim(); if (!name || name === d.name) return;
      try {
        await this.call('/api/drivers/' + d.id, { method: 'PATCH', body: { name } });
        this.origNames[d.id] = name; this.toast('Saved');
      } catch (e) { this.toast(e.message, true); } finally { this.loadFloor(); }
    },
    async clockDriver(d, what) {
      const key = 'clk' + d.id; if (this.busy[key]) return;
      this.busy[key] = true;
      try {
        await this.call('/api/drivers/' + d.id + '/' + what, { method: 'POST' });
        this.toast(d.name + (what === 'clock-in' ? ' clocked in' : ' clocked out'));
      } catch (e) { this.toast(e.message, true); } finally { this.busy[key] = false; this.loadFloor(); }
    },
    async moveDriver(i, dir) {
      const ids = this.drivers.map(d => d.id), j = i + dir;
      if (j < 0 || j >= ids.length) return;
      [ids[i], ids[j]] = [ids[j], ids[i]];
      try { await this.call('/api/drivers/reorder', { method: 'POST', body: { ids } }); } catch (e) { this.toast(e.message, true); }
      this.loadFloor();
    },
    async removeDriver(d) {
      const ok = await this.ask({ title: 'Remove ' + d.name + '?', body: 'They leave the driver list. Their past deliveries stay on the reports.', confirmLabel: 'Remove driver', danger: true });
      if (!ok) return;
      try { await this.call('/api/drivers/' + d.id, { method: 'DELETE' }); this.toast('Removed ' + d.name); } catch (e) { this.toast(e.message, true); }
      this.loadFloor();
    },
    driverStatus(d) {
      return { off: { cls: 'b-mute', text: 'Clocked out' }, available: { cls: 'b-ok', text: 'In queue' }, out: { cls: 'b-info', text: 'On a run' } }[d.status] || { cls: 'b-mute', text: d.status };
    },
    wallTime(hhmm) {
      const m = /^(\d{1,2}):(\d{2})$/.exec(String(hhmm || '').trim()); if (!m) return hhmm || '—';
      const h = Number(m[1]); return (h % 12 === 0 ? 12 : h % 12) + ':' + m[2] + (h < 12 ? ' AM' : ' PM');
    },

    // --- history ----------------------------------------------------------------------
    histApply() { this.hist.offset = 0; this.loadHistory(); },
    histPage(dir) { this.hist.offset = Math.max(0, this.hist.offset + dir * HIST_NUM); this.loadHistory(); },
    // One row more than a page tells whether there is a next page.
    async loadHistory() {
      const h = this.hist, p = new URLSearchParams();
      p.set('offset', h.offset); p.set('num', HIST_NUM + 1);
      if (h.q.trim()) p.set('q', h.q.trim());
      if (h.status.trim()) p.set('status', h.status.trim());
      if (h.sort) p.set('sort', h.sort);
      if (h.from) p.set('from', new Date(h.from).toISOString());
      if (h.to) p.set('to', new Date(h.to).toISOString());
      h.loading = true; h.err = ''; h.errRaw = '';
      try {
        const rows = await this.call('/api/history?' + p.toString());
        const all = Array.isArray(rows) ? rows : [];
        h.more = all.length > HIST_NUM; h.rows = all.slice(0, HIST_NUM); h.loaded = true;
      } catch (e) {
        const m = this.mainError(e, 'history');
        h.err = m.msg; h.errRaw = m.raw; h.rows = []; h.more = false;
      } finally { h.loading = false; }
    },
    // A failed proxy call to main: 502/503/504 = main unreachable (friendly text,
    // the raw error goes in a details toggle); a Pi network error says so.
    mainError(e, what) {
      if (e && e.code === 'network') return { msg: PI_DOWN, raw: '' };
      if (e && [502, 503, 504].includes(e.status)) return { msg: "Can't reach the main server — " + what + ' lives there. Live and printing still work.', raw: e.message };
      return { msg: e ? e.message : 'Failed', raw: '' };
    },
    get histPageNo() { return this.hist.offset / HIST_NUM + 1; },
    get histHasNext() { return this.hist.more; },
    histStatusLabel(st) { const f = HIST_STATUSES.find(x => x[0] === st); return f ? f[1] : String(st || '').replace(/_/g, ' '); },
    histKey(snap, i) { return (snap.id || 'r') + ':' + i; },
    async histAct(snap, what, i) {
      const key = 'h' + what + i; if (this.busy[key]) return;
      if (what === 'resend' && !(await this.ask({ title: 'Bring back ' + custName(snap) + "'s order as a new order?", body: 'It comes back as a new order to accept on the tablet. If this Pi already accepted it, its receipt prints again instead.', confirmLabel: 'Bring back' }))) return;
      this.busy[key] = true;
      try {
        await this.call(what === 'print' ? '/api/print' : '/api/resend', { method: 'POST', body: snap });
        this.toast(what === 'print' ? 'Receipt reprinted' : 'Brought back as a new order');
      } catch (e) { this.toast(e.status === 502 ? 'Printer problem — nothing printed: ' + e.message : e.message, true); }
      finally { this.busy[key] = false; }
    },

    // --- printers -----------------------------------------------------------------------
    async loadPrinters() {
      // store-config is proxied from main: fetch it once, not on every refresh.
      const [st, dg, sc] = await Promise.allSettled([this.call('/api/printers'), this.call('/api/diag'), this.storeCfg ? Promise.resolve(this.storeCfg) : this.call('/api/store-config')]);
      if (dg.status === 'fulfilled') { this.diag = dg.value; this.diagErr = false; } else this.diagErr = true;
      if (sc.status === 'fulfilled') this.storeCfg = sc.value;
      const status = st.status === 'fulfilled' ? st.value || [] : [];
      const roster = (dg.status === 'fulfilled' && dg.value.printers) || [];
      const cfg = (sc.status === 'fulfilled' && sc.value && sc.value.printers) || [];
      const byName = new Map();
      const add = (name) => { if (!byName.has(name)) byName.set(name, { name, id: '', ip: '', is_receipt: false, up: null }); return byName.get(name); };
      for (const p of roster) Object.assign(add(p.name), { id: p.id, ip: p.ip || '', is_receipt: !!p.is_receipt });
      for (const p of cfg) { const e = add(p.name); if (!e.id) e.id = p.id; if (!e.ip) e.ip = p.ip; e.is_receipt = e.is_receipt || !!p.is_receipt; }
      for (const s of status) Object.assign(add(s.name), { ip: s.ip || byName.get(s.name).ip, up: s.up, mac: s.mac || '', last_checked: s.last_checked, last_error: s.last_error, up_since: s.up_since, down_since: s.down_since });
      this.printers = [...byName.values()].sort((a, b) => (b.is_receipt - a.is_receipt) || a.name.localeCompare(b.name));
      this.printersErr = st.status === 'rejected' ? st.reason.message : '';
      if (!this.foundBusy) this.loadFound(false);
    },
    // Printers answering on the Pi's network (TCP 9100). refresh scans now
    // (the Pi waits up to 15 s for the sweep; still scanning → ask again).
    async loadFound(refresh) {
      if (refresh) this.foundBusy = true;
      try {
        let f = await this.call('/api/printers/found' + (refresh ? '?refresh=1' : ''));
        for (let i = 0; refresh && f && f.scanning && i < 10; i++) {
          await new Promise(r => setTimeout(r, 3000));
          f = await this.call('/api/printers/found');
        }
        this.found = f;
      } catch (e) { if (refresh) this.toast("Couldn't scan. " + e.message, true); }
      finally { if (refresh) this.foundBusy = false; }
    },
    get foundNew() { return ((this.found && this.found.printers) || []).filter(f => !f.known); },
    get foundSub() {
      const f = this.found;
      if (this.foundBusy || (f && f.scanning)) return 'Looking for printers on this network…';
      if (!f || !f.scanned_at) return 'Not checked yet — press Scan again.';
      return "Printers answering on this Pi's network that aren't set up yet · checked " + this.ago(f.scanned_at);
    },
    async addFound(f) {
      const name = await this.ask({
        title: 'Add the printer at ' + f.ip + '?',
        body: 'Name it after its station (e.g. Pizza, Grill). It is added in main as a kitchen printer — the first printer becomes the receipt printer.',
        confirmLabel: 'Add printer', input: { label: 'Printer name', maxlength: 40 },
      });
      if (name === null) return;
      const n = name.trim();
      if (!n) { this.toast('Type a name for the printer.', true); return; }
      this.foundBusy = true;
      try {
        await this.call('/api/printers/add', { method: 'POST', body: { name: n, ip: f.ip, mac: f.mac || '' } });
        this.toast('Added ' + n + ' — it appears here in a moment');
        setTimeout(() => this.loadPrinters(), 2500);
      } catch (e) {
        this.toast(e.code === 'name_taken' ? 'There is already a printer called ' + n + '.' : 'Not added. ' + e.message, true);
      } finally { this.foundBusy = false; this.loadPrinters(); }
    },
    printerTone(p) { return p.up === true ? 'b-ok' : p.up === false && p.last_checked ? 'b-bad' : 'b-mute'; },
    printerState(p) { return p.up === true ? 'Ready' : p.up === false && p.last_checked ? 'Down' : 'Not checked yet'; },
    async printerAction(kind, p) {
      if (this.printerBusy) return;
      this.printerBusy = kind + (p ? p.id || p.name : '');
      const path = kind === 'sample' ? '/api/printers/sample' : '/api/printers/test';
      const body = {};
      if (p && p.id) body.printer_id = p.id;
      if (kind === 'probe') body.probe_only = true;
      try {
        const out = await this.call(path, { method: 'POST', body });
        const verb = kind === 'probe' ? 'Reachable' : kind === 'sample' ? 'Sample printed' : 'Test printed';
        const results = (out && out.results) || [];
        for (const r of results) this.printerResults[r.id || r.name] = { ok: r.ok, text: r.ok ? verb : (r.error || r.error_code || 'Failed') };
        const bad = results.filter(r => !r.ok).length;
        if (!results.length) this.toast('No printers to try', true);
        else this.toast(bad ? bad + ' of ' + results.length + ' failed' : verb + (results.length > 1 ? ' on all ' + results.length : ' on ' + results[0].name), bad > 0);
      } catch (e) {
        this.toast(e.code === 'test_in_progress' ? 'A printer test is already running — wait for it.' : e.message, true);
      } finally { this.printerBusy = ''; this.loadPrinters(); }
    },
    resultFor(p) { return this.printerResults[p.id] || this.printerResults[p.name] || null; },
    get receiptPrinter() { return this.printers.find(p => p.is_receipt) || null; },

    // --- settings -------------------------------------------------------------------------
    goSettings(id) { this.go('settings', id); },
    loadSettingsTab() {
      switch (this.settingsTab) {
        case 'orders': this.loadAutoAccept(); this.loadDeliveryPrefs(); this.loadTimes(); break;
        case 'kitchen': this.loadFloorPrefs(); this.loadDevices(); this.loadStoreCfg(); break;
        case 'receipts': this.loadReceipt(); this.loadStoreCfg(); this.loadPrinters(); break;
        case 'clockout': this.loadReset(); break;
        case 'tablets': this.loadDevices(); this.loadAppLatest(); break;
        case 'store': this.loadStoreCfg(); break;
        case 'account': this.cp.err = ''; this.loadUpdate(); break;
      }
    },
    async loadAutoAccept() {
      try { const a = await this.call('/api/settings/auto-accept'); this.autoAccept = !!a.auto_accept; this.autoAcceptLoaded = true; } catch (e) { this.toast(e.message, true); }
    },
    async toggleAutoAccept() {
      const v = !this.autoAccept;
      if (v && !(await this.ask({
        title: 'Turn on auto-accept?',
        body: 'Every order will print and be accepted with the last prep time, with nobody checking it.',
        confirmLabel: 'Turn on auto-accept', focusCancel: true,
      }))) return;
      this.autoAccept = v;
      try {
        await this.call('/api/settings/auto-accept', { method: 'PUT', body: { auto_accept: v } });
        this.toast(v ? 'Auto-accept is on' : 'Auto-accept is off');
      } catch (e) { this.autoAccept = !v; this.toast('Not saved. ' + e.message, true); }
    },
    async loadFloorPrefs() {
      await this.loadFloor();
      if (this.floor) this.floorForm = { green: this.floor.green_till_minutes, tiptax: this.floor.tip_tax_percent };
    },
    get floorDirty() {
      const f = this.floor, x = this.floorForm;
      return !!f && (Number(x.green) !== Number(f.green_till_minutes) || Number(x.tiptax) !== Number(f.tip_tax_percent));
    },
    async loadDeliveryPrefs() {
      await this.loadFloor();
      if (this.floor) this.deliveryForm = { soon: this.floor.soon_ready_minutes, target: this.floor.delivery_target_minutes };
    },
    get deliveryDirty() {
      const f = this.floor, x = this.deliveryForm;
      return !!f && (Number(x.soon) !== Number(f.soon_ready_minutes) || Number(x.target) !== Number(f.delivery_target_minutes));
    },
    async saveDeliveryPrefs() {
      const soon = parseInt(this.deliveryForm.soon, 10), target = parseInt(this.deliveryForm.target, 10);
      if (!(soon >= 0 && soon <= 30)) { this.toast('Soon-ready window: 0 to 30 minutes.', true); return; }
      if (!(target >= 15 && target <= 120)) { this.toast('Delivery target: 15 to 120 minutes.', true); return; }
      try {
        await this.call('/api/settings/deliveries', { method: 'PUT', body: { soon_ready_minutes: soon, delivery_target_minutes: target } });
        this.toast('Saved');
        await this.loadDeliveryPrefs();
      } catch (e) { this.toast('Not saved. ' + e.message, true); }
    },
    // 2.1.0 feature toggles + kitchen-ticket switches: saved on tap.
    async toggleSetting(part, key) {
      const set = this.settingsAll; if (!set || this.busy['set' + key]) return;
      const v = !set[part][key];
      this.busy['set' + key] = true;
      try {
        const out = await this.call('/api/settings/' + part, { method: 'PUT', body: { [key]: v } });
        this.floor.settings[part] = out;
        this.toast('Saved');
      } catch (e) { this.toast('Not saved. ' + e.message, true); } finally { this.busy['set' + key] = false; this.loadFloor(); }
    },
    settingOn(part, key) { const set = this.settingsAll; return !!(set && set[part] && set[part][key]); },
    async loadTimes() {
      await this.loadFloor();
      const t = this.settingsAll && this.settingsAll.times;
      if (t) this.timesForm = { pickup: t.default_pickup_minutes, delivery: t.default_delivery_minutes };
    },
    get timesDirty() {
      const t = this.settingsAll && this.settingsAll.times, x = this.timesForm;
      return !!t && (Number(x.pickup) !== Number(t.default_pickup_minutes) || Number(x.delivery) !== Number(t.default_delivery_minutes));
    },
    async saveTimes() {
      const pickup = parseInt(this.timesForm.pickup, 10), delivery = parseInt(this.timesForm.delivery, 10);
      if (!(pickup >= 1 && pickup <= 240) || !(delivery >= 1 && delivery <= 240)) { this.toast('Default times: 1 to 240 minutes.', true); return; }
      try {
        await this.call('/api/settings/times', { method: 'PUT', body: { default_pickup_minutes: pickup, default_delivery_minutes: delivery } });
        this.toast('Saved');
        await this.loadTimes();
      } catch (e) { this.toast('Not saved. ' + e.message, true); }
    },
    get resetDirty() { return !!this.reset.form && this.reset.form !== this.reset.time; },
    async saveFloorPrefs() {
      const green = parseInt(this.floorForm.green, 10), tip = parseFloat(this.floorForm.tiptax);
      if (!(green >= 1)) { this.toast('Type how many minutes tickets stay green.', true); return; }
      if (!(tip >= 0)) { this.toast('Type the tip tax as a percent (0 for none).', true); return; }
      try {
        await this.call('/api/settings/floor', { method: 'PUT', body: { green_till_minutes: green, tip_tax_percent: tip } });
        this.toast('Saved');
        await this.loadFloor();
        if (this.floor) this.floorForm = { green: this.floor.green_till_minutes, tiptax: this.floor.tip_tax_percent };
      } catch (e) { this.toast('Not saved. ' + e.message, true); }
    },
    async saveReset() {
      try {
        const r = await this.call('/api/settings/driver-reset', { method: 'PUT', body: { time: this.reset.form } });
        this.reset = { time: r.time, last_run: r.last_run || null, form: r.time };
        this.toast('Saved');
      } catch (e) { this.toast('Not saved. ' + e.message, true); }
    },
    // Inline error only (no toast): store info is proxied from main.
    async loadStoreCfg() {
      try { this.storeCfg = await this.call('/api/store-config'); this.storeCfgErr = ''; this.storeCfgErrRaw = ''; }
      catch (e) { const m = this.mainError(e, 'store info'); this.storeCfgErr = m.msg; this.storeCfgErrRaw = m.raw; }
    },
    get piAddress() { return location.host; },
    get storeAddress() {
      const l = (this.storeCfg && this.storeCfg.location) || {};
      return [l.address_line1, l.address_line2].filter(Boolean).join(', ');
    },

    // Receipts
    async loadReceipt() {
      try {
        const r = await this.call('/api/settings/receipt');
        this.rcpt = r; this.setReceiptForm(r.prefs); this.rcptErr = '';
      } catch (e) { this.rcptErr = e.message; }
    },
    setReceiptForm(p) {
      this.rcptForm = Object.assign({}, p);
      const f = p.footer_text;
      this.footMode = f === null || f === undefined ? 'default' : f === '' ? 'none' : 'custom';
      this.customFooter = this.footMode === 'custom' ? f : '';
    },
    buildPrefs() {
      const f = this.rcptForm || {};
      const footer = this.footMode === 'default' ? null : this.footMode === 'none' ? '' : this.customFooter.toUpperCase().trim();
      return {
        kitchen_item_size: f.kitchen_item_size, kitchen_number_size: f.kitchen_number_size, footer_text: footer,
        footer_size: f.footer_size, copies: Number(f.copies) || 1,
        show_store_address: !!f.show_store_address, show_phone: !!f.show_phone, extra_paper: !!f.extra_paper,
        // Set on the tablet (driver-report size, line spacing): kept as they are.
        driver_report_size: f.driver_report_size, spacing: f.spacing,
      };
    },
    get rcptDirty() { return !!(this.rcpt && this.rcptForm) && prefsKey(this.buildPrefs()) !== prefsKey(this.rcpt.prefs); },
    get footerError() {
      if (this.footMode !== 'custom') return '';
      const t = this.customFooter.toUpperCase().trim();
      if (!t) return 'Type the footer text, or pick None.';
      if (t.length > MAX_FOOTER) return 'At most ' + MAX_FOOTER + ' characters.';
      if (!/^[\x20-\x7e]*$/.test(t)) return 'Plain letters, digits, spaces and punctuation only (no accents or emoji).';
      return '';
    },
    get footerShown() {
      if (this.footMode === 'none') return '';
      if (this.footMode === 'custom') return this.customFooter.toUpperCase();
      return (this.rcpt && this.rcpt.footer_store_default) || 'ONLINE ORDER';
    },
    sizePx(s) { return SIZE_PX[s] || SIZE_PX.normal; },
    async saveReceipt() {
      if (this.footerError) { this.toast(this.footerError, true); return false; }
      this.rcptBusy = 'save';
      try {
        const r = await this.call('/api/settings/receipt', { method: 'PUT', body: this.buildPrefs() });
        this.rcpt = Object.assign({}, this.rcpt, r); this.setReceiptForm(r.prefs);
        this.toast('Receipt settings saved');
        return true;
      } catch (e) { this.toast('Not saved. ' + e.message, true); return false; } finally { this.rcptBusy = ''; }
    },
    // The sample prints with the SAVED settings, so unsaved changes save first.
    async printSample() {
      if (this.rcptDirty && !(await this.saveReceipt())) return;
      this.rcptBusy = 'sample';
      try {
        const out = await this.call('/api/printers/sample', { method: 'POST', body: {} });
        const r = ((out && out.results) || [])[0];
        if (r && !r.ok) this.toast('Sample not printed on ' + r.name + ': ' + (r.error || r.error_code), true);
        else this.toast('Sample printed' + (r ? ' on ' + r.name : ''));
      } catch (e) {
        this.toast(e.code === 'test_in_progress' ? 'A printer test is already running — wait for it.' : e.message, true);
      } finally { this.rcptBusy = ''; }
    },
    resetReceipt() {
      if (!this.rcpt || !this.rcpt.defaults) return;
      this.setReceiptForm(this.rcpt.defaults);
      this.toast('Defaults restored — Save to keep them');
    },

    // Tablets
    async pairCode() {
      try { this.pair = await this.call('/api/pairing/code', { method: 'POST' }); }
      catch (e) { this.toast(e.message, true); }
    },
    get pairLeft() { return this.pair ? ts(this.pair.expires_at) - this.now : 0; },
    async revokeDevice(d) {
      const ok = await this.ask({ title: 'Revoke ' + d.name + '?', body: 'The tablet is signed out at once and has to be paired again.', confirmLabel: 'Revoke tablet', danger: true });
      if (!ok) return;
      try { await this.call('/api/devices/' + d.id + '/revoke', { method: 'POST' }); this.toast(d.name + ' revoked'); } catch (e) { this.toast(e.message, true); }
      this.loadDevices();
    },

    // Updates (Settings › Account)
    async loadUpdate() {
      try { this.upd = await this.call('/api/update/status'); this.updErr = ''; } catch (e) { this.updErr = e.message; }
      try { const a = await this.call('/api/settings/auto-update'); this.autoUpdate = !!a.auto_update; this.autoUpdateLoaded = true; } catch (e) { /* older agent: no toggle */ }
    },
    async toggleAutoUpdate() {
      const v = !this.autoUpdate;
      this.autoUpdate = v;
      try {
        await this.call('/api/settings/auto-update', { method: 'PUT', body: { auto_update: v } });
        this.toast(v ? 'The Pi updates itself after Close batch' : 'Automatic updates are off');
      } catch (e) { this.autoUpdate = !v; this.toast('Not saved. ' + e.message, true); }
    },
    get updStateText() {
      const u = this.upd; if (!u) return '—';
      const p = u.pending || {};
      switch (u.state) {
        case 'waiting_for_idle': return 'Waiting until no orders are open, then it installs' + (p.requested_by ? ' (asked by ' + p.requested_by + ')' : '');
        case 'requested': return (p.action === 'rollback' ? 'Roll back' : 'Update') + ' requested — starting…';
        case 'running': return 'Installing — this page may drop for a minute';
        case 'ok': return 'Just finished';
        case 'up_to_date': return 'Already up to date';
        case 'failed': return 'Failed';
        case 'rolled_back': return 'Failed — rolled back';
        default: return 'Nothing running';
      }
    },
    get updLastText() {
      const l = this.upd && this.upd.last; if (!l) return 'None yet';
      const what = { ok: l.action === 'rollback' ? 'Rolled back' : 'Updated', up_to_date: 'Already up to date', failed: 'Failed', rolled_back: 'Failed, rolled back' }[l.state] || l.state;
      const vers = l.state === 'ok' && l.from_version && l.to_version ? ' ' + l.from_version + ' → ' + l.to_version : l.state === 'rolled_back' && l.from_version ? ' to ' + l.from_version : '';
      return what + vers + ' · ' + dateTime(l.finished_at || l.started_at) + (l.error ? ' — ' + l.error : '');
    },
    get updBlockedText() {
      const u = this.upd; if (!u) return '';
      switch (u.blocked_reason) {
        case 'not_after_close': return 'Only right after Close batch — close the batch on Overview first, then update.';
        case 'update_in_progress': return 'An update is already in progress.';
        case 'not_supported': return "This machine can't update itself — only an installed Pi can.";
        default: return u.update_available ? "Installs the published version now. Orders can't arrive for about a minute." : 'Re-checks and installs the published version (nothing changes when it is already current).';
      }
    },
    get updRollbackBlocked() { const u = this.upd; return !u || u.blocked_reason === 'not_supported' || u.blocked_reason === 'update_in_progress'; },
    async startUpdate() {
      const u = this.upd || {};
      const ok = await this.ask({
        title: 'Update this Pi now?',
        body: 'It installs ' + (u.latest_version ? 'version ' + u.latest_version : 'the published version') + ' and restarts the Pi software — this page and the tablet drop for about a minute. If the new version does not start properly the Pi goes back to ' + (u.current_version || 'this one') + ' by itself.',
        confirmLabel: 'Update now', focusCancel: true,
      });
      if (!ok) return;
      await this.sendUpdate('/api/update', 'Update started — the Pi restarts in a minute');
    },
    async startRollback() {
      const ok = await this.ask({
        title: 'Roll back this Pi?',
        body: 'It goes back to the version it ran before and restarts the Pi software — this page and the tablet drop for about a minute. Press Roll back again later to undo it.',
        confirmLabel: 'Roll back', danger: true,
      });
      if (!ok) return;
      await this.sendUpdate('/api/update/rollback', 'Roll back started — the Pi restarts in a minute');
    },
    async sendUpdate(path, done) {
      this.updBusy = path;
      try {
        await this.call(path, { method: 'POST', body: {} });
        this.toast(done);
      } catch (e) { this.toast(e.message, true); }
      finally { this.updBusy = ''; this.loadUpdate(); }
    },

    // Account
    async changePassword() {
      const c = this.cp; c.err = '';
      if (!c.next) { c.err = 'Choose a new password.'; return; }
      if (c.next !== c.confirm) { c.err = "The passwords don't match."; return; }
      c.busy = true;
      try {
        await this.call('/api/settings/password', { method: 'POST', body: { current: c.current, new: c.next, confirm: c.confirm, hint: c.hint.trim() } });
        c.current = c.next = c.confirm = c.hint = '';
        this.toast('Password updated — other browsers are signed out');
      } catch (e) { c.err = e.message; } finally { c.busy = false; }
    },
    async unlink() {
      const ok = await this.ask({
        title: 'Unlink this Pi?',
        body: "Its token is revoked on the server, every paired tablet is signed out and this Pi's login is wiped. Orders stop arriving here.\n\nTo use it again, link it with the store manager login.",
        confirmLabel: 'Unlink this Pi', danger: true,
      });
      if (!ok) return;
      try {
        await this.call('/api/settings/unlink', { method: 'POST' });
        this.toast('This Pi is unlinked');
      } catch (e) { this.toast('Not unlinked. ' + e.message, true); }
      this.refreshSession(); // token + login gone → setup screen
    },
  };
}
