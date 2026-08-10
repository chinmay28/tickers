/* Tickers — the web client.
 *
 * Plain ES modules, no framework, no build step. That is a deployment
 * decision, not a stylistic one: the server embeds this directory with
 * go:embed, so `go build` is the entire front-end toolchain and a Raspberry Pi
 * install never installs Node.
 *
 * Shape: one `state` object fetched from /api/state, a hash router picking a
 * render function, and full re-renders of the routed view. The data is a
 * handful of rows; diffing it would cost more code than redrawing it.
 */

/* ------------------------------------------------------------------ *
 * Small helpers
 * ------------------------------------------------------------------ */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/** Escape text for interpolation into an HTML template string. Every value
 *  that reaches innerHTML goes through this — symbols and sink names are
 *  user-supplied, and one of them will eventually contain an angle bracket. */
function esc(value) {
  return String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}

/** Format a price with the sensible number of decimals for its magnitude:
 *  two for anything normal, more for the sub-dollar instruments (a penny
 *  stock at "0.00" is not a price, it's a bug report waiting to happen). */
function money(value, currency) {
  if (value === null || value === undefined) return '—';
  const abs = Math.abs(value);
  const digits = abs >= 1 ? 2 : abs >= 0.01 ? 4 : 6;
  const text = value.toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
  return currency && currency !== 'USD' ? `${text} ${currency}` : text;
}

/** How many decimals a composite's value is worth showing. It has no currency
 *  and no natural magnitude, so it gets more than a price would: a VTI/GLD
 *  rendered "0.97" hides the move that "0.9683" shows. */
function ratioDigits(value) {
  const abs = Math.abs(value ?? 0);
  return abs >= 100 ? 2 : abs >= 1 ? 4 : 6;
}

/** Format a composite's value. */
function ratio(value) {
  if (value === null || value === undefined) return '—';
  const digits = ratioDigits(value);
  return value.toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
}

/** Mirror of the server's expr.Looks: does this text read as a composite
 *  formula rather than a plain symbol? A bare hyphen doesn't count — BTC-USD is
 *  a symbol, and a subtraction has to be spaced. Used only to pick wording and
 *  to keep "Search by name" from looking up a ratio; the server decides. */
function looksComposite(text) {
  const s = String(text ?? '');
  return /[/*+()]/.test(s) || s.includes(' - ');
}

function signed(value, digits = 2) {
  if (value === null || value === undefined) return '';
  const text = Math.abs(value).toLocaleString(undefined, {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
  return `${value >= 0 ? '+' : '−'}${text}`;
}

/** Relative time, for "updated 40s ago". Absolute timestamps on a dashboard
 *  make you do the subtraction yourself. */
function ago(iso) {
  if (!iso) return 'never';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return 'never';
  const secs = Math.max(0, Math.round((Date.now() - then) / 1000));
  if (secs < 10) return 'just now';
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.round(hours / 24)}d ago`;
}

/** Split a hand-typed symbol list ("vti, gld btc-usd") into normalised
 *  symbols. The server normalises again on the way in; doing it here too is
 *  what makes the settings field show back what you meant to type. */
function symbolList(raw) {
  return String(raw ?? '')
    .split(/[,\s]+/)
    .map((s) => s.trim().toUpperCase())
    .filter(Boolean);
}

/* ------------------------------------------------------------------ *
 * Symbol marks
 * ------------------------------------------------------------------ */

/** The hues a fetched symbol's mark can take.
 *
 *  A curated list rather than the whole wheel, because four hues here already
 *  mean something: --up and --down are on the same card as the mark, and
 *  --composite and --portfolio say what kind of row this is. A green AAPL
 *  beside a red change, or a violet GLD that isn't a composite, would each be
 *  a colour arguing with the one next to it. What is left is three bands —
 *  warm, blue, magenta — and they are stepped through finely rather than
 *  coarsely: with a handful of hues a four-holding card regularly drew three
 *  of the same one, and two neighbours a step apart still look less alike
 *  than two that are identical. */
const MARK_HUES = [20, 34, 48, 62, 78, 90, 210, 222, 234, 292, 312, 332];

/** Pick a symbol's hue. The hash is only here so the mark comes out the same
 *  on every device and after every reload without a byte being stored or
 *  fetched — but it is FNV-1a rather than the obvious `hash * 31 + c`, whose
 *  low bits barely move across four-letter symbols and hand most of a
 *  watchlist the same two hues. */
function markHue(symbol) {
  let hash = 2166136261;
  for (const ch of String(symbol)) {
    hash ^= ch.codePointAt(0);
    hash = Math.imul(hash, 16777619);
  }
  return MARK_HUES[(hash >>> 0) % MARK_HUES.length];
}

/** The two letters a mark carries. Punctuation is dropped so `BTC-USD` reads
 *  as BT rather than B-. */
function markInitials(symbol) {
  const text = String(symbol ?? '');
  return (text.replace(/[^A-Za-z0-9]/g, '').slice(0, 2) || text.slice(0, 2)).toUpperCase();
}

/** The version of this symbol's logo, or undefined if it hasn't got one.
 *
 *  The map rides along with the state payload rather than being guessed at.
 *  Pointing an `<img>` at every symbol and letting the misses 404 would cost a
 *  failed request per fund, per crypto pair and per composite on every load.
 *
 *  The value is *when* the image was stored, and it goes in the URL: the bytes
 *  are served with a day of browser caching, so without it a logo you just
 *  replaced would keep showing the old one until tomorrow. */
function logoVersion(symbol) {
  return state.data?.logos?.[symbol]?.v;
}

/** Was this one uploaded? It decides what "remove" promises: deleting your own
 *  picture, or throwing away a cached one that comes back tomorrow. */
function isCustomLogo(symbol) {
  return Boolean(state.data?.logos?.[symbol]?.custom);
}

/** The endpoint for one symbol's image. Symbols reach it as a single path
 *  segment — a composite's `VTI/GLD` has to arrive as one, not as two. */
function logoURL(symbol, version) {
  const path = `/api/logos/${encodeURIComponent(symbol)}`;
  return version ? `${path}?v=${encodeURIComponent(version)}` : path;
}

/** The square that sits in front of a symbol wherever one is listed.
 *
 *  Most of them are drawn, not fetched: initials over a hue hashed out of the
 *  symbol, which costs nothing, works offline and is the same on every device.
 *  When logos are turned on the server caches a real one per symbol and this
 *  puts it in the same box — the drawn mark stays underneath, so a picture
 *  that fails to load leaves the initials showing rather than a hole.
 *
 *  The two computed kinds are *never fetched* a logo — neither is a symbol
 *  anyone issued, so there is nothing upstream to ask for — and fall back to a
 *  glyph in their own hue: the obelus and the pie say "priced from a formula"
 *  and "priced from a basket", which is what the row's outline says too. An
 *  uploaded picture still wins for them, because somebody choosing one for
 *  their own portfolio knows better than this rule does. */
function symbolMark(symbol, kind = '') {
  const version = logoVersion(symbol);
  if (!version && kind === 'composite') {
    return `<span class="mark mark--composite" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round">
        <path d="M5 12h14"/><circle cx="12" cy="7" r="1.1" fill="currentColor"/><circle cx="12" cy="17" r="1.1" fill="currentColor"/>
      </svg></span>`;
  }
  if (!version && kind === 'portfolio') {
    return `<span class="mark mark--portfolio" aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
        <path d="M12 3a9 9 0 1 0 9 9h-9z"/><path d="M14 3.2A9 9 0 0 1 20.8 10H14z"/>
      </svg></span>`;
  }
  // The image sits on top of the initials rather than instead of them: with an
  // empty alt a picture that never arrives collapses to nothing and the drawn
  // mark is simply still there. No error handler, no second render pass.
  const logo = version
    ? `<img class="mark__logo" src="${esc(logoURL(symbol, version))}" alt="" loading="lazy" />`
    : '';
  return `<span class="mark${logo ? ' mark--logo' : ''}" style="--mark-h:${markHue(
    symbol,
  )}" aria-hidden="true">${esc(markInitials(symbol))}${logo}</span>`;
}

function clock(iso) {
  if (!iso) return '—';
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleTimeString();
}

function duration(seconds) {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`;
  const hours = seconds / 3600;
  return `${Number.isInteger(hours) ? hours : hours.toFixed(1)} h`;
}

/* ------------------------------------------------------------------ *
 * Toasts
 * ------------------------------------------------------------------ */

const TOAST_MS = 4500;

function toast(message, kind = '') {
  const el = document.createElement('div');
  el.className = `toast${kind ? ` toast--${kind}` : ''}`;
  el.textContent = message;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), TOAST_MS);
}

/* ------------------------------------------------------------------ *
 * API client
 * ------------------------------------------------------------------ */

async function api(path, options = {}) {
  const response = await fetch(`/api${path}`, {
    headers: options.body ? { 'Content-Type': 'application/json' } : undefined,
    ...options,
  });
  if (response.status === 204) return null;

  const text = await response.text();
  let body = null;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = { error: text };
    }
  }
  if (!response.ok) {
    const error = new Error(body?.error || `HTTP ${response.status}`);
    // The status rides along for the one caller that treats a code as an
    // answer rather than a failure: a 501 means the quote source doesn't do
    // that, which is a card that shouldn't be drawn, not an error to shout
    // about. Everything else still only ever reads the message.
    error.status = response.status;
    throw error;
  }
  return body;
}

const send = (method) => (path, payload) =>
  api(path, { method, body: payload === undefined ? undefined : JSON.stringify(payload) });

const post = send('POST');
const patch = send('PATCH');
const del = send('DELETE');

/* ------------------------------------------------------------------ *
 * App state
 * ------------------------------------------------------------------ */

/** POLL_MS is how often the client re-reads /api/state. It is deliberately
 *  shorter than the server's shortest allowed refresh interval (30s) so a
 *  cycle's results never sit invisible for a whole poll. */
const POLL_MS = 10_000;
const DEV_FLASH_MS = 3000;

/** How many cycles the log shows at a time.
 *
 *  Small on purpose. The log answers "did the last one go, and if not why" far
 *  more often than it answers anything about last Tuesday, and a page that
 *  opens with two hundred rows buries the row that matters under the ones that
 *  don't. Deeper is one button away and stays open once asked for. */
const RUNS_PAGE = 25;

/** The most the server will return, matching store.RunKeep. Asking past it is
 *  clamped there rather than refused, so this only decides when the button
 *  stops being offered. */
const RUNS_MAX = 500;

const state = {
  data: null,
  connected: true,
  /** Row IDs currently showing their inline edit form. */
  editing: new Set(),
  /** Sink IDs currently showing their edit form, plus 'new' for the add form. */
  editingSinks: new Set(),
  /** Sparkline points by ticker ID, fetched lazily per row. */
  history: new Map(),
  /** Symbol-search results. They live in the add dialog, which is outside the
   *  redrawn view, so this is the render state for one small painted region
   *  rather than something the routed render has to carry. */
  matches: null,
  /** The performance sheet: which row it is showing, the series the server
   *  sent, and which range chip is selected. Outside #view for the same
   *  reason `matches` is. */
  perf: null,
  /** The last backtest asked for: which portfolio (or 'draft' for an unsaved
   *  allocation), whether it is in flight, and what came back. One at a time —
   *  the page shows the result under the list, and running a second replaces
   *  the first rather than growing a stack of them. */
  backtest: null,
  /** The sector card under each run, by the slot the run lives in. Per slot
   *  rather than one between them, because Portfolios and Funds each keep their
   *  own run: opening a fund and coming back would otherwise leave the
   *  portfolio's chart with the card underneath it gone. Each entry carries the
   *  subject it belongs to, the comparison symbols as they were typed, and what
   *  came back — see sectorSubject for what `key` is. */
  sectors: {},
  /** The fund being looked through: its symbol, whether it is in flight, and
   *  what came back. Shaped like `backtest` on purpose — the two pages show the
   *  same card, so the period and the sort live in the same fields and one set
   *  of handlers drives both. */
  fund: null,
  runs: [],
  /** How many cycles the log is showing, and whether the server has older ones
   *  behind them. The count grows a page at a time and never shrinks while the
   *  page is open — a reader who opened the log deeper should not have it fold
   *  back up under them on the next poll. */
  runsShown: RUNS_PAGE,
  runsMore: false,
  busy: false,
  /** An inline, non-`act` request is in flight (a button showing "Testing…"),
   *  so a background redraw would undo its state mid-flight. */
  inlineBusy: false,
  /** A background redraw was skipped and is owed once the field is released. */
  renderPending: false,
  /** A section the next redraw should scroll to, once — see MOVED. */
  pendingScroll: '',
};

async function loadState(opts) {
  try {
    state.data = await api('/state');
    if (!state.connected) {
      state.connected = true;
      toast('Reconnected to the Tickers server', 'ok');
    }
    state.connected = true;
  } catch (err) {
    state.connected = false;
    console.warn('state fetch failed', err);
  }
  $('#offline-banner').hidden = state.connected;
  render(opts);
}

/** Reload everything the current view needs, then redraw. */
async function refreshView(opts) {
  // A fund page's subject is in the URL, so landing on one — by navigation, by
  // reload, or by somebody's pasted link — is what asks for the lookup. Only
  // when it changed: the poll runs through here every ten seconds, and
  // re-fetching a fund on each one would hammer the source's crumbed endpoint
  // for a page that has not moved.
  if (route() === 'funds' && routeArg() && state.fund?.symbol !== routeArg().toUpperCase()) {
    loadFund(routeArg());
  }
  if (route() === 'settings') {
    try {
      // The window is "the newest N", not "page number N". It grows when the
      // reader asks for more and is re-fetched whole on every poll, which is
      // what keeps a log being appended to at one end and pruned at the other
      // from needing cursors that go stale between redraws — the newest cycle
      // is always the first row, however deep the page has been opened.
      const body = await api(`/runs?limit=${state.runsShown}`);
      state.runs = body?.runs ?? [];
      state.runsMore = Boolean(body?.more);
    } catch {
      state.runs = [];
      state.runsMore = false;
    }
  }
  await loadState(opts);
}

/** Wrap an action so the UI can't fire two overlapping mutations, and so
 *  every failure surfaces as a toast instead of a silent console entry. */
async function act(fn, { success } = {}) {
  if (state.busy) return;
  state.busy = true;
  try {
    await fn();
    if (success) toast(success, 'ok');
    // The user asked for this one, so it redraws even if a field still has
    // focus — the draft and the caret are put back afterwards.
    await refreshView({ force: true });
  } catch (err) {
    toast(err.message || String(err), 'error');
  } finally {
    state.busy = false;
  }
}

/* ------------------------------------------------------------------ *
 * Form drafts
 *
 * The routed view is redrawn wholesale, and the 10s poll means that happens
 * under a user who is halfway through typing an endpoint into a form. Two
 * guards, because either alone leaves a hole:
 *
 *   1. A redraw nobody asked for waits while a field has focus. Nothing is
 *      rebuilt under the cursor, so the caret, the selection and — on iOS,
 *      where a re-created input closes it — the keyboard all survive.
 *   2. Every value the user types is stashed by (form, field) name, and put
 *      back after any redraw that does happen. Redraws the user *did* ask for
 *      (saving a ticker, adding a destination) still land immediately, and
 *      they no longer empty the other forms on the page.
 *
 * A draft only exists once the user has typed into that field, so restoring
 * one can never shadow a fresh server value with a stale rendered one.
 * ------------------------------------------------------------------ */

/** Draft values by form key: { [fieldName]: string | boolean }. */
const drafts = new Map();

/** A form's identity across redraws. The element is thrown away every time,
 *  but the key is stable — that is what lets a draft find its way home. */
function formKey(form) {
  return form?.dataset?.formKey || form?.id || '';
}

function isField(el) {
  return (
    el instanceof HTMLInputElement ||
    el instanceof HTMLTextAreaElement ||
    el instanceof HTMLSelectElement
  );
}

/** True while the user is in a field in the routed view. */
function isTyping() {
  const el = document.activeElement;
  return isField(el) && $('#view').contains(el);
}

function readForm(form) {
  const values = {};
  for (const el of form.elements) {
    // A file input is skipped rather than stashed: its value is not assignable,
    // so putting one back after a redraw throws, and there is nothing to put
    // back anyway — choosing a file uploads it there and then.
    if (!el.name || !isField(el) || el.type === 'file') continue;
    values[el.name] = el.type === 'checkbox' ? el.checked : el.value;
  }
  return values;
}

function saveDraft(form) {
  const key = formKey(form);
  if (key) drafts.set(key, readForm(form));
}

function clearDraft(key) {
  drafts.delete(key);
}

/** The draft value for one field, for render code that has to agree with what
 *  the user typed (the interval presets highlight from this, not from the
 *  saved setting). */
function draftValue(key, name) {
  const values = drafts.get(key);
  return values && name in values ? values[name] : undefined;
}

function findForm(key) {
  return $$('form', $('#view')).find((form) => formKey(form) === key) ?? null;
}

/** Put every stashed value back into the freshly rendered forms. */
function restoreDrafts() {
  for (const form of $$('form', $('#view'))) {
    const values = drafts.get(formKey(form));
    if (!values) continue;
    for (const el of form.elements) {
      if (!el.name || !isField(el) || !(el.name in values)) continue;
      if (el.type === 'checkbox') el.checked = values[el.name];
      else el.value = values[el.name];
    }
  }
}

/** Note where the cursor is, in terms that survive the elements being
 *  re-created: (form key, field name) rather than the node itself. */
function captureFocus() {
  const el = document.activeElement;
  if (!isField(el) || !$('#view').contains(el)) return null;
  const snap = { key: formKey(el.form), name: el.name, id: el.id, start: null, end: null };
  try {
    snap.start = el.selectionStart;
    snap.end = el.selectionEnd;
  } catch {
    // number and url inputs don't expose a selection in every browser.
  }
  return snap;
}

function restoreFocus(snap) {
  if (!snap) return;
  const view = $('#view');
  let el = snap.id ? view.querySelector(`#${CSS.escape(snap.id)}`) : null;
  if (!el && snap.name) el = findForm(snap.key)?.elements?.[snap.name] ?? null;
  if (!isField(el)) return;
  el.focus({ preventScroll: true });
  if (snap.start === null) return;
  try {
    el.setSelectionRange(snap.start, snap.end);
  } catch {
    // Same inputs as above; the focus is the part that mattered.
  }
}

/* ------------------------------------------------------------------ *
 * Router
 * ------------------------------------------------------------------ */

const ROUTES = ['watchlist', 'portfolios', 'funds', 'settings'];

/** Routes that used to exist and now land somewhere else.
 *
 *  Publishing is part of Settings: destinations, the payload they receive and
 *  the cycles that sent it are all answers to "is this configured the way I
 *  meant", and they were a whole tab away from the interval that decides how
 *  often any of it happens. The old hash still works — it is in bookmarks and
 *  in the phone's tab bar history — and it lands on the section rather than at
 *  the top of a long page. */
const MOVED = { publishing: { route: 'settings', section: 'publishing' } };

/** The route is the first segment, so a page can carry a subject in the second.
 *  Funds is the only one that does — `#/funds/QQQ` — and it is a path rather
 *  than a query because a fund page is a thing you send somebody, not a filter
 *  you set. Every existing hash has one segment and is unaffected. */
function route() {
  const first = routePath()[0];
  if (MOVED[first]) return MOVED[first].route;
  return ROUTES.includes(first) ? first : 'watchlist';
}

/** The section a moved route should be scrolled to, once. */
function movedSection() {
  return MOVED[routePath()[0]]?.section ?? '';
}

/** What the route is about: `QQQ` in `#/funds/QQQ`, empty where there is none. */
function routeArg() {
  return decodeURIComponent(routePath()[1] ?? '');
}

function routePath() {
  return location.hash.replace(/^#\/?/, '').split('?')[0].split('/');
}

function syncNav() {
  const current = route();
  for (const link of $$('[data-route]')) {
    link.classList.toggle('is-active', link.dataset.route === current);
  }
  // The floating button is the create action for the two pages that have one —
  // a ticker on the Watchlist, an allocation on Portfolios. On Publishing and
  // Settings there is nothing for it to add.
  const fab = $('#add-fab');
  const creates = current === 'watchlist' || current === 'portfolios';
  fab.hidden = !creates;
  if (creates) {
    const what = current === 'watchlist' ? 'a ticker or a ratio' : 'a portfolio';
    fab.title = `Add ${what}`;
    fab.setAttribute('aria-label', `Add ${what}`);
  }
}

/* ------------------------------------------------------------------ *
 * Render
 * ------------------------------------------------------------------ */

/** Redraw the routed view.
 *
 *  `force` marks a redraw the user asked for — a save, a route change, a
 *  search result. Without it the redraw is a background one (the poll), and it
 *  is deferred rather than performed while a field has focus; `renderPending`
 *  remembers that it is owed, and the focusout handler pays it back. The
 *  header, footer and offline banner live outside #view and update either way,
 *  so a deferred redraw never means stale status. */
function render({ force = false } = {}) {
  syncNav();
  const view = $('#view');
  const data = state.data;

  if (!data) {
    view.innerHTML = `<div class="empty"><strong>${
      state.connected ? 'Loading…' : 'Disconnected'
    }</strong>${
      state.connected
        ? 'Fetching the watchlist.'
        : 'Waiting for the Tickers server to come back.'
    }</div>`;
    return;
  }

  $('#app-version').textContent = data.version;
  renderFooter(data);

  if (!force && (isTyping() || state.inlineBusy)) {
    state.renderPending = true;
    return;
  }
  state.renderPending = false;

  const focus = captureFocus();

  switch (route()) {
    case 'portfolios':
      view.innerHTML = renderPortfolios(data);
      break;
    case 'funds':
      view.innerHTML = renderFunds(data);
      break;
    case 'settings':
      view.innerHTML = renderSettings(data);
      break;
    default:
      view.innerHTML = renderWatchlist(data);
      drawSparklines();
  }

  restoreDrafts();
  restoreFocus(focus);
  scrollToSection();
}

/** Scroll a moved route to the section it used to be, exactly once.
 *
 *  Once, because #view is redrawn every ten seconds: a scroll that ran on every
 *  redraw would drag the page back the moment anyone scrolled away from it. The
 *  flag is set by the hash change and cleared here. */
function scrollToSection() {
  if (!state.pendingScroll) return;
  const target = $(`#section-${state.pendingScroll}`);
  state.pendingScroll = '';
  target?.scrollIntoView({ block: 'start', behavior: 'auto' });
}

function renderFooter(data) {
  const next = data.engine.nextRun ? `next ${clock(data.engine.nextRun)}` : 'idle';
  const last = data.engine.lastRun
    ? `last run ${ago(data.engine.lastRun.finishedAt)} · ${data.engine.lastRun.okCount} ok, ${data.engine.lastRun.errorCount} failed`
    : 'no runs yet';
  const sinks = data.sinks.filter((s) => s.enabled).length;
  $('#footer-status').textContent =
    `${data.engine.running ? 'Refreshing…' : next} · ${last} · ` +
    `${sinks} publish ${sinks === 1 ? 'destination' : 'destinations'} · quotes via ${data.engine.provider}`;
}

/* ---------------------------- Watchlist ---------------------------- */

function renderWatchlist(data) {
  return `
    <div class="page-head">
      <div>
        <h1>Watchlist</h1>
        <p>
          Use <strong>+</strong> to add a symbol, or a ratio like
          <code>VTI/GLD</code>. Double-tap a row for its chart and returns.
        </p>
      </div>
    </div>

    <div class="card">
      <div class="card__head">
        <h2 class="card__title">${data.tickers.length} ${
          data.tickers.length === 1 ? 'symbol' : 'symbols'
        }</h2>
      </div>
      <div class="card__body">
        ${
          data.tickers.length === 0
            ? `<div class="empty"><strong>Nothing on the watchlist</strong>Press the <strong>+</strong> button to add a symbol; it will be priced immediately.</div>`
            : `<div class="quotes" id="quotes">${data.tickers.map(renderQuote).join('')}</div>`
        }
      </div>
    </div>
  `;
}

/** Paint the "Search by name" results into the add dialog. The dialog is
 *  outside the routed view and never re-rendered, so this one region updates
 *  itself rather than riding along with a full redraw. */
function paintMatches() {
  $('#matches').innerHTML = renderMatches();
}

/** The "Search by name" results as HTML. */
function renderMatches() {
  const m = state.matches;
  if (!m) return '';
  if (m.status === 'loading') return '<span class="field__hint">Searching…</span>';
  if (m.status === 'error') {
    return `<span class="field__hint">Search failed: ${esc(m.message)}</span>`;
  }
  if (!m.items.length) {
    return '<span class="field__hint">No matches. Type the exact symbol instead.</span>';
  }
  return m.items
    .map(
      (m2) => `<button class="match" type="button" data-symbol="${esc(m2.symbol)}">
          <span class="match__symbol">${symbolMark(m2.symbol)}${esc(m2.symbol)}</span>
          <span class="match__meta">${esc(m2.name)} · ${esc(m2.exchange)} · ${esc(m2.type)}</span>
        </button>`,
    )
    .join('');
}

/** The logo control for one row: what it has, a way to replace it, and — for
 *  an uploaded one — a way to take it back off.
 *
 *  Removing a *fetched* logo is deliberately not offered. It would come back
 *  on the next daily pass, so the button would be a promise the app cannot
 *  keep; uploading over it is the way to change what a row shows. */
function logoField(symbol) {
  const custom = isCustomLogo(symbol);
  return `
    <div class="field field--logo">
      <label class="field__label">Logo</label>
      <div class="logo-edit">
        ${symbolMark(symbol)}
        <label class="btn btn--sm btn--outline">
          ${custom ? 'Replace image' : 'Upload an image'}
          <input type="file" accept="image/png,image/jpeg,image/gif,image/webp"
                 data-logo-upload="${esc(symbol)}" hidden />
        </label>
        ${
          custom
            ? `<button class="btn btn--sm btn--ghost" type="button"
                       data-action="remove-logo" data-symbol="${esc(symbol)}">Remove image</button>`
            : ''
        }
      </div>
      <span class="field__hint">
        PNG, JPEG, GIF or WebP, up to 256 KB. It is stored here and served from
        this server, and the daily logo refresh leaves an uploaded image alone.
      </span>
    </div>`;
}

function renderQuote(t) {
  const q = t.quote;
  const editing = state.editing.has(t.id);
  const failed = q && q.status === 'error';
  // A composite is a row whose price came from a formula over other symbols.
  // It renders like every other row apart from the outline, the chip and the
  // extra decimals — that sameness is the point of the feature.
  const composite = Boolean(t.expression);
  // A portfolio's row is the third kind. It is a currency amount, so it takes
  // a price's two decimals rather than a ratio's six, and its symbol and value
  // both belong to the Portfolios page — which is why it has no Edit or Remove
  // here. Removing it from the watchlist would mean deleting the portfolio.
  const portfolio = Boolean(t.portfolioId);
  const money2 = !composite || portfolio;

  let priceHTML;
  if (q && q.status === 'ok' && q.price !== null) {
    const dir = t.change === null || t.change === undefined ? 'flat' : t.change > 0 ? 'up' : t.change < 0 ? 'down' : 'flat';
    const change =
      t.change === null || t.change === undefined
        ? ''
        : // The change is shown to the same precision as the value it moved,
          // so a row never disagrees with itself about how exact it is.
          `${signed(t.change, money2 ? 2 : ratioDigits(q.price))} (${signed(t.changePercent, 2)}%)`;
    priceHTML = `
      <span class="quote__value">${esc(money2 ? money(q.price, q.currency) : ratio(q.price))}</span>
      ${change ? `<span class="quote__change quote__change--${dir}">${esc(change)}</span>` : ''}
      <span class="quote__change quote__change--flat">${esc(ago(q.fetchedAt))}</span>`;
  } else {
    priceHTML = `<span class="quote__value quote__value--na">N/A</span>
      ${q ? `<span class="quote__change quote__change--flat">${esc(ago(q.fetchedAt))}</span>` : ''}`;
  }

  return `
    <article class="quote${t.enabled ? '' : ' quote--disabled'}${
      portfolio ? ' quote--portfolio' : composite ? ' quote--composite' : ''
    }" data-id="${esc(t.id)}" draggable="true">
      <button class="quote__handle" type="button" aria-label="Reorder ${esc(t.symbol)}" tabindex="-1">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
          <path d="M9 6h.01M9 12h.01M9 18h.01M15 6h.01M15 12h.01M15 18h.01"/>
        </svg>
      </button>

      <div class="quote__head">
        ${symbolMark(t.symbol, portfolio ? 'portfolio' : composite ? 'composite' : '')}
        <span class="quote__symbol">${esc(t.symbol)}</span>
        ${
          portfolio
            ? `<span class="chip chip--portfolio">portfolio</span>`
            : composite
              ? `<span class="chip chip--composite">composite</span>`
              : ''
        }
        ${
          // The symbol is the formula with its spaces removed, so it is only
          // worth printing the formula as well when the two actually differ.
          composite && t.expression !== t.symbol
            ? `<span class="quote__name quote__formula">${esc(t.expression)}</span>`
            : ''
        }
        ${t.label ? `<span class="quote__name">${esc(t.label)}</span>` : ''}
        ${!t.label && !composite && q?.shortName ? `<span class="quote__name">${esc(q.shortName)}</span>` : ''}
        ${t.pinned ? `<span class="chip chip--pinned">pinned</span>` : ''}
        ${t.enabled ? '' : `<span class="chip chip--off">paused</span>`}
      </div>

      <svg class="quote__spark" data-spark="${esc(t.id)}" viewBox="0 0 180 26" preserveAspectRatio="none" aria-hidden="true"></svg>

      <div class="quote__price">${priceHTML}</div>

      ${failed ? `<div class="quote__error">${esc(q.error)}</div>` : ''}

      <div class="quote__actions">
        ${
          portfolio
            ? `<a class="btn btn--sm btn--outline" href="#/portfolios">Open</a>
               <button class="btn btn--sm btn--ghost" data-action="edit" data-id="${esc(t.id)}">Edit</button>`
            : `<button class="btn btn--sm btn--outline" data-action="edit" data-id="${esc(t.id)}">Edit</button>`
        }
        <button class="btn btn--sm btn--ghost" data-action="pin" data-id="${esc(t.id)}">
          ${t.pinned ? 'Unpin' : 'Pin'}
        </button>
        <button class="btn btn--sm btn--ghost" data-action="toggle" data-id="${esc(t.id)}">
          ${t.enabled ? 'Pause' : 'Resume'}
        </button>
        <button class="btn btn--sm btn--ghost" data-action="up" data-id="${esc(t.id)}" aria-label="Move up">↑</button>
        <button class="btn btn--sm btn--ghost" data-action="down" data-id="${esc(t.id)}" aria-label="Move down">↓</button>
        ${
          portfolio
            ? ''
            : `<button class="btn btn--sm btn--danger" data-action="delete" data-id="${esc(t.id)}">Remove</button>`
        }
      </div>

      ${
        editing
          ? `<form class="quote__edit" data-edit="${esc(t.id)}" data-form-key="ticker:${esc(t.id)}" autocomplete="off">
              ${
                // A portfolio's symbol is its name and belongs to the
                // Portfolios page, so its form offers the two things that are
                // this row's own: what it is called here, and what it looks
                // like. The absent field is also what the submit handler keys
                // off to leave the symbol alone.
                portfolio
                  ? ''
                  : `<div class="field">
                      <label class="field__label">${composite ? 'Formula' : 'Symbol'}</label>
                      ${
                        // The field's *name* is what tells the submit handler
                        // which of the two this row is; the server re-derives
                        // the symbol from the formula either way.
                        composite
                          ? `<input class="input input--mono" name="expression" value="${esc(t.expression)}" required />`
                          : `<input class="input input--mono" name="symbol" value="${esc(t.symbol)}" required />`
                      }
                    </div>`
              }
              <div class="field">
                <label class="field__label">Label</label>
                <input class="input" name="label" value="${esc(t.label)}" placeholder="optional" />
              </div>
              ${logoField(t.symbol)}
              <button class="btn btn--sm btn--primary" type="submit">Save</button>
              <button class="btn btn--sm btn--ghost" type="button" data-action="cancel-edit" data-id="${esc(t.id)}">Cancel</button>
            </form>`
          : ''
      }
    </article>
  `;
}

/** Draw each row's sparkline from history fetched on demand. Rows without
 *  enough points simply stay blank — an axis-less two-point line says nothing
 *  a reader can use. */
async function drawSparklines() {
  for (const svg of $$('[data-spark]')) {
    const id = svg.dataset.spark;
    let points = state.history.get(id);
    if (!points) {
      try {
        points = (await api(`/tickers/${id}/history?limit=90`))?.points ?? [];
      } catch {
        points = [];
      }
      state.history.set(id, points);
      // The view may have been replaced while that request was in flight.
      if (!svg.isConnected) continue;
    }
    if (points.length < 3) continue;

    const values = points.map((p) => p.price);
    const min = Math.min(...values);
    const max = Math.max(...values);
    const span = max - min || 1;
    const stepX = 180 / (values.length - 1);
    const path = values
      .map((v, i) => `${i ? 'L' : 'M'}${(i * stepX).toFixed(1)} ${(24 - ((v - min) / span) * 22).toFixed(1)}`)
      .join(' ');

    const rising = values[values.length - 1] >= values[0];
    svg.innerHTML =
      `<path d="${path}" fill="none" stroke="var(--${rising ? 'up' : 'down'})" ` +
      `stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round" opacity="0.9" />`;
  }
}

/* --------------------------- Portfolios ----------------------------
 *
 * A saved allocation and what it would have done. The result comes from
 * /api/backtest, which is monthly: the provider's daily closes reduced to one
 * per month, because a portfolio's answer does not get more true at daily
 * resolution — it gets forty times bigger and starts implying the rebalance
 * happened on a particular Tuesday.
 * ------------------------------------------------------------------ */

/** A balance, rounded to whole units. Cents on a forty-year growth curve are
 *  six digits of noise in front of the two that matter. */
function amount(value) {
  if (value === null || value === undefined) return '—';
  return Math.round(value).toLocaleString();
}

/** A percentage that keeps its sign, for the return columns. */
function percent(value, digits = 2) {
  if (value === null || value === undefined) return '—';
  return `${signed(value, digits)}%`;
}

function direction(value) {
  if (value === null || value === undefined) return 'flat';
  return value > 0 ? 'up' : value < 0 ? 'down' : 'flat';
}

/** A month key as something to read. "2004-11" is a key; "Nov 2004" is a date. */
function monthName(key) {
  if (!key) return '—';
  const [year, month] = String(key).split('-');
  const names = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  return names[Number(month) - 1] ? `${names[Number(month) - 1]} ${year}` : key;
}

const REBALANCE_LABELS = {
  none: 'never rebalanced',
  annually: 'rebalanced annually',
  quarterly: 'rebalanced quarterly',
  monthly: 'rebalanced monthly',
};

function renderPortfolios(data) {
  const portfolios = data.portfolios ?? [];

  return `
    <div class="page-head">
      <div>
        <h1>Portfolios</h1>
        <p>
          Backtest an allocation. Each one also appears on the watchlist, priced
          live and published with everything else.
        </p>
      </div>
    </div>

    <div class="card">
      <div class="card__head">
        <h2 class="card__title">${portfolios.length} ${
          portfolios.length === 1 ? 'portfolio' : 'portfolios'
        }</h2>
        <button class="btn btn--sm btn--primary" data-action="new-portfolio" type="button">New portfolio</button>
      </div>
      <div class="card__body">
        ${
          portfolios.length === 0
            ? `<div class="empty"><strong>No portfolios yet</strong>Add one to see what a mix of funds would have returned. Holdings don't have to be on the watchlist — and the portfolio itself joins it automatically, priced live and published with everything else.</div>`
            : portfolios.map(portfolioRow).join('')
        }
      </div>
    </div>

    ${renderBacktest()}
  `;
}

/** The watchlist symbol a portfolio is published under, read off its own row
 *  rather than re-derived here — the server decides, and a second
 *  implementation of the rule would eventually disagree with it. */
function portfolioSymbol(p) {
  return state.data?.tickers.find((t) => t.portfolioId === p.id)?.symbol ?? '';
}

function portfolioRow(p) {
  const period = [
    p.startYear ? `from ${p.startYear}` : 'from as early as the data goes',
    p.endYear ? `to ${p.endYear}` : '',
  ]
    .filter(Boolean)
    .join(' ');
  const held = (p.holdings ?? []).length;

  return `
    <div class="card" style="margin-top:0.7rem">
      <div class="card__head">
        <div class="card__heading">
          <h3 class="card__title">
            ${esc(p.name)}
            ${p.benchmark ? `<span class="chip">vs ${esc(p.benchmark)}</span>` : ''}
          </h3>
          <span class="card__meta">${held} ${held === 1 ? 'holding' : 'holdings'}</span>
        </div>
        <div style="display:flex;gap:0.3rem;flex-wrap:wrap">
          <button class="btn btn--sm btn--primary" data-action="run-portfolio" data-id="${esc(p.id)}">Run</button>
          <button class="btn btn--sm btn--ghost" data-action="edit-portfolio" data-id="${esc(p.id)}">Edit</button>
          <button class="btn btn--sm btn--danger" data-action="delete-portfolio" data-id="${esc(p.id)}">Remove</button>
        </div>
      </div>
      <div class="card__body">
        <div class="allocation-chips">
          ${(p.holdings ?? [])
            .map(
              (h) =>
                `<span class="chip chip--weight">${symbolMark(h.symbol)}<strong>${esc(h.symbol)}</strong> ${esc(
                  Number(h.weight).toLocaleString(undefined, { maximumFractionDigits: 2 }),
                )}%${
                  h.replacement
                    ? ` <span class="chip__stand" title="Stands in before ${esc(
                        h.symbol,
                      )}'s own history begins">← ${esc(h.replacement)}</span>`
                    : ''
                }</span>`,
            )
            .join('')}
        </div>
        <p class="field__hint" style="margin:0.55rem 0 0">
          ${esc(amount(p.initialAmount))}${
            p.contribution > 0
              ? ` + ${esc(amount(p.contribution))} ${esc(p.contributionFrequency)}`
              : ''
          } · ${esc(period)} ·
          ${esc(REBALANCE_LABELS[p.rebalance] ?? p.rebalance)}
        </p>
        ${
          // Said in words rather than as a bare chip. The chip showed the same
          // symbol and left "how do I put this on the watchlist?" a fair
          // question to still be asking — the answer being that it already is,
          // which a label has to actually say.
          portfolioSymbol(p)
            ? `<p class="field__hint" style="margin:0.3rem 0 0">
                 On the watchlist as
                 <a class="quote__formula" href="#/">${esc(portfolioSymbol(p))}</a>,
                 priced every refresh and published with everything else.
               </p>`
            : ''
        }
      </div>
    </div>
  `;
}

/** What a run calls itself. Read off the saved list rather than off the result,
 *  which knows what it simulated and not whose allocation it was — and shared
 *  with the sector card underneath so the two headings can never disagree. */
function backtestName(id) {
  if (id === 'draft') return 'Unsaved allocation';
  return state.data?.portfolios?.find((p) => p.id === id)?.name ?? 'Portfolio';
}

/** The result panel: whichever run was asked for last, in whatever state it is
 *  in. Nothing at all before the first run — an empty chart frame implies the
 *  answer is "flat" rather than "not asked yet". */
function renderBacktest() {
  const run = state.backtest;
  if (!run) return '';

  const name = backtestName(run.id);

  if (run.status === 'loading') {
    return `
      <div class="page-head page-head--sub"><div><h2>${esc(name)}</h2></div></div>
      <div class="card"><div class="card__body">
        <div class="empty"><strong>Working through the history</strong>Every holding's closes since it listed, compounded a month at a time.</div>
      </div></div>`;
  }
  if (run.status === 'error') {
    return `
      <div class="page-head page-head--sub"><div><h2>${esc(name)}</h2></div></div>
      <div class="card"><div class="card__body">
        <div class="empty"><strong>Couldn't run it</strong>${esc(run.error)}</div>
      </div></div>`;
  }

  const b = run.data;

  return `
    <div class="page-head page-head--sub">
      <div>
        <h2>${esc(name)}</h2>
        <p>
          ${esc(amount(b.initial))}${
            b.contributed ? ` plus ${esc(amount(b.contributed))} paid in` : ''
          } from ${esc(monthName(b.start))} to
          ${esc(monthName(b.end))} — ${b.months} ${b.months === 1 ? 'month' : 'months'}${
            b.rebalances ? `, rebalanced ${b.rebalances} ${b.rebalances === 1 ? 'time' : 'times'}` : ''
          }.
        </p>
      </div>
      <div class="perf-head__delta">
        <div class="perf-head__value">${esc(amount(b.portfolio.end))}</div>
        <div class="perf-change perf-change--${direction(b.portfolio.totalPercent)}">${esc(
          percent(b.portfolio.totalPercent),
        )}</div>
      </div>
    </div>

    ${(b.notes ?? []).map((note) => `<p class="field__hint backtest-note">${esc(note)}</p>`).join('')}
    ${replacementsTable(b)}

    <div class="card">
      <div class="card__body">
        ${growthChart(b)}
        ${growthLegend(b)}
      </div>
    </div>

    <div class="card">
      <div class="card__head"><h3 class="card__title">Summary</h3></div>
      <div class="card__body">${metricsTable(b)}</div>
    </div>

    <div class="card">
      <div class="card__head"><h3 class="card__title">Calendar years</h3></div>
      <div class="card__body">${annualTable(b)}</div>
    </div>

    ${holdingsCard(b)}
    ${sectorsCard(sectorSubject(b, { label: name }))}
  `;
}

/** Which holdings were stood in for, and until when.
 *
 *  A table rather than a paragraph each. One substitution reads fine as a
 *  sentence; five read as five near-identical sentences filling the screen
 *  above the chart, at which point nobody reads any of them — including the one
 *  line beside them that is not boilerplate. The same facts, scannable, in a
 *  fifth of the height. */
function replacementsTable(b) {
  const swapped = (b.holdings ?? []).filter((h) => h.replacedUntil);
  if (!swapped.length) return '';

  return `
    <div class="card backtest-swaps">
      <div class="card__head">
        <h3 class="card__title">Replacements</h3>
        <span class="field__hint">a stand-in's returns cover the months before the holding listed</span>
      </div>
      <div class="card__body" style="padding:0">
        <div class="table-scroll"><table class="table">
          <thead><tr><th>Holding</th><th>Stood in by</th><th>Own data from</th></tr></thead>
          <tbody>${swapped
            .map(
              (h) => `<tr>
                <th>${esc(h.symbol)}</th>
                <td><span class="quote__formula">${esc(h.replacement)}</span></td>
                <td class="field__hint">${esc(monthName(h.replacedUntil))}</td>
              </tr>`,
            )
            .join('')}</tbody>
        </table></div>
      </div>
    </div>`;
}

/** The growth curve, and the benchmark's alongside it when there is one.
 *
 *  Points are placed by index rather than by date, which the performance sheet
 *  deliberately does not do — there the series changes resolution partway
 *  through, and here every point is exactly one month after the last. */
function growthChart(b) {
  // The performance sheet's geometry exactly, so the two charts in this app
  // reuse one rule and one aspect ratio rather than drifting apart.
  const W = 640;
  const H = 240;
  const points = b.points ?? [];
  if (points.length < 2) return '<div class="empty">Nothing to chart.</div>';

  const values = points.flatMap((p) =>
    p.benchmark === null || p.benchmark === undefined ? [p.value] : [p.value, p.benchmark],
  );
  const lo = Math.min(...values, 0);
  const hi = Math.max(...values);
  const max = hi + (hi - lo) * 0.06 || 1;
  const min = lo;

  const ticks = [max, (max + min) / 2, min].map(amount);
  const widest = Math.max(...ticks.map((t) => t.length));
  const pad = { l: Math.min(130, 16 + widest * 6.7), r: 12, t: 12, b: 24 };

  const x = (i) => pad.l + (i / (points.length - 1)) * (W - pad.l - pad.r);
  const y = (v) => pad.t + (1 - (v - min) / (max - min || 1)) * (H - pad.t - pad.b);

  const path = (pick) =>
    points
      .map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(pick(p)).toFixed(1)}`)
      .join(' ');

  const line = path((p) => p.value);
  const floor = (H - pad.b).toFixed(1);
  const area = `${line} L${x(points.length - 1).toFixed(1)} ${floor} L${x(0).toFixed(1)} ${floor} Z`;
  const stroke = `var(--${b.portfolio.end >= b.initial ? 'up' : 'down'})`;

  const grid = [max, (max + min) / 2, min]
    .map(
      (v, i) => `<line x1="${pad.l}" x2="${W - pad.r}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"
                    stroke="var(--border)" stroke-width="1" />
               <text x="${pad.l - 7}" y="${(y(v) + 3.5).toFixed(1)}" text-anchor="end"
                     class="perf-chart__tick">${esc(ticks[i])}</text>`,
    )
    .join('');

  const benchmark =
    b.benchmark && points[0].benchmark !== null && points[0].benchmark !== undefined
      ? `<path d="${path((p) => p.benchmark)}" fill="none" stroke="var(--muted)" stroke-width="1.5"
               stroke-dasharray="5 4" stroke-linejoin="round" />`
      : '';

  return `
    <svg class="perf-chart" viewBox="0 0 ${W} ${H}" role="img"
         aria-label="Growth of ${esc(amount(b.initial))} from ${esc(b.start)} to ${esc(b.end)}">
      ${grid}
      <path d="${area}" fill="${stroke}" opacity="0.12" />
      ${benchmark}
      <path d="${line}" fill="none" stroke="${stroke}" stroke-width="1.8"
            stroke-linejoin="round" stroke-linecap="round" />
      <text x="${pad.l}" y="${H - 6}" class="perf-chart__tick">${esc(monthName(b.start))}</text>
      <text x="${W - pad.r}" y="${H - 6}" text-anchor="end" class="perf-chart__tick">${esc(monthName(b.end))}</text>
    </svg>`;
}

function growthLegend(b, label = 'Portfolio') {
  if (!b.benchmark) return '';
  return `
    <div class="chart-legend">
      <span class="chart-legend__item"><span class="chart-legend__swatch chart-legend__swatch--line"></span>${esc(
        label,
      )}</span>
      <span class="chart-legend__item"><span class="chart-legend__swatch chart-legend__swatch--dash"></span>${esc(
        b.benchmark.label,
      )}</span>
    </div>`;
}

/** The summary table. One column per strategy, so a portfolio and its benchmark
 *  are read across a row rather than by holding two tables in your head. */
function metricsTable(b) {
  const columns = [b.portfolio, b.benchmark].filter(Boolean);
  const cells = (pick) => columns.map((m) => `<td>${pick(m)}</td>`).join('');

  const year = (y) =>
    y ? `<span class="perf-change perf-change--${direction(y.percent)}">${esc(percent(y.percent))}</span>
         <span class="perf-when">${y.year}</span>`
      : '<span class="field__hint">no full year</span>';

  return `<div class="table-scroll"><table class="table table--summary">
      <thead><tr><th></th>${columns.map((m) => `<th>${esc(m.label)}</th>`).join('')}</tr></thead>
      <tbody>
        <tr><th>Final balance</th>${cells((m) => esc(amount(m.end)))}</tr>
        ${
          // Only when there were any. Without this row a reader looking at a
          // balance four times the initial amount and a total return of 40%
          // has no way to see where the rest came from.
          b.contributed
            ? `<tr><th>Of which paid in</th>${cells(
                () => `${esc(amount(b.initial))} + ${esc(amount(b.contributed))}`,
              )}</tr>`
            : ''
        }
        <tr><th>Total return</th>${cells(
          (m) =>
            `<span class="perf-change perf-change--${direction(m.totalPercent)}">${esc(
              percent(m.totalPercent),
            )}</span>`,
        )}</tr>
        <tr><th>Annualised</th>${cells((m) =>
          m.cagrPercent === null || m.cagrPercent === undefined
            ? '<span class="field__hint">under a year</span>'
            : `<span class="perf-change perf-change--${direction(m.cagrPercent)}">${esc(
                percent(m.cagrPercent),
              )}</span>`,
        )}</tr>
        <tr><th title="Annualised standard deviation of the monthly returns">Volatility</th>${cells((m) =>
          m.stdevPercent === null || m.stdevPercent === undefined
            ? '—'
            : `${esc(m.stdevPercent.toFixed(2))}%`,
        )}</tr>
        ${
          // Only with a risk-free series behind them. A Sharpe quietly computed
          // against 0% is a different statistic under the same name, so the
          // rows are absent rather than wrong.
          b.riskFree
            ? `<tr><th title="Return above the risk-free rate per unit of volatility">Sharpe</th>${cells(
                (m) => ratioCell(m.sharpe),
              )}</tr>
               <tr><th title="The same, counting only downside volatility">Sortino</th>${cells(
                 (m) => ratioCell(m.sortino),
               )}</tr>`
            : ''
        }
        ${
          columns.some((m) => m.yieldPercent !== null && m.yieldPercent !== undefined)
            ? `<tr><th title="Mean income yield across the full calendar years">Income yield</th>${cells(
                (m) => esc(yieldCell(m.yieldPercent)),
              )}</tr>`
            : ''
        }
        <tr><th>Best year</th>${cells((m) => year(m.bestYear))}</tr>
        <tr><th>Worst year</th>${cells((m) => year(m.worstYear))}</tr>
        <tr><th>Deepest fall</th>${cells((m) => drawdownCell(m))}</tr>
      </tbody>
    </table></div>`;
}

/** Sharpe and Sortino. Two decimals, unsigned — they are ratios, not moves, and
 *  a "+1.24" would read as a gain of something. A missing one is a series with
 *  nothing to divide by (a run that never fell has no downside deviation), not
 *  a zero. */
function ratioCell(value) {
  if (value === null || value === undefined) {
    return '<span class="field__hint">n/a</span>';
  }
  return `<span class="perf-change perf-change--${direction(value)}">${esc(value.toFixed(2))}</span>`;
}

/** The drawdown cell says how far down, from when to when, and — the part that
 *  matters most — whether it ever came back. */
function drawdownCell(m) {
  if (!m.maxDrawdownPercent) return '<span class="field__hint">never fell</span>';
  const span = `${monthName(m.drawdownPeak)} → ${monthName(m.drawdownTrough)}`;
  const recovery = m.drawdownRecovered
    ? `back by ${monthName(m.drawdownRecovered)}`
    : 'not yet recovered';
  return `<span class="perf-change perf-change--down">−${esc(m.maxDrawdownPercent.toFixed(2))}%</span>
          <span class="perf-when">${esc(span)} · ${esc(recovery)}</span>`;
}

function annualTable(b, label = 'Portfolio') {
  const rows = b.annual ?? [];
  if (!rows.length) return '<div class="empty">The run is too short to cover a calendar year.</div>';
  const hasBenchmark = Boolean(b.benchmark);
  // The column appears only when the quote source can actually answer. A
  // yield of "—" on every row would read as "this paid nothing".
  const hasYield = rows.some((r) => r.yieldPercent !== null && r.yieldPercent !== undefined);

  return `<div class="table-scroll"><table class="table">
      <thead><tr>
        <th>Year</th><th>${esc(label)}</th>${hasBenchmark ? `<th>${esc(b.benchmark.label)}</th>` : ''}
        ${hasYield ? '<th title="Income as a percentage of the value the year opened at">Yield</th>' : ''}
      </tr></thead>
      <tbody>${rows
        .map(
          (r) => `<tr>
            <th>${r.year}${
              // A part-year is shown and labelled rather than dropped: hiding it
              // loses real information, and printing it unmarked claims a full
              // year the run never covered.
              r.partial ? ' <span class="field__hint">part year</span>' : ''
            }</th>
            <td><span class="perf-change perf-change--${direction(r.percent)}">${esc(
              percent(r.percent),
            )}</span></td>
            ${
              hasBenchmark
                ? `<td><span class="perf-change perf-change--${direction(
                    r.benchmark,
                  )}">${esc(percent(r.benchmark))}</span></td>`
                : ''
            }
            ${hasYield ? `<td class="field__hint">${esc(yieldCell(r.yieldPercent))}</td>` : ''}
          </tr>`,
        )
        .join('')}</tbody>
    </table></div>`;
}

/* ----------------------- Holding performance ------------------------
 *
 * Under the calendar years because it answers the question they raise. "2022:
 * −14%" is the portfolio; what a reader wants next is which holdings did that
 * to it, and which ones kept it from being worse.
 *
 * Every holding is listed rather than a leading and trailing few. Which rows
 * matter is the reader's question and not one this can answer for them — a 2%
 * position that halved is a footnote where a 40% one that halved is the whole
 * story — so the table sorts instead of choosing. */

/** Short forms for the period chips. The server's own labels are sentences —
 *  right for the caption underneath, too wide for six chips on a phone. */
const PERIOD_CHIPS = { ytd: 'YTD', '1y': '1Y', '3y': '3Y', '5y': '5Y', '10y': '10Y', run: 'All' };

/** The sortable columns: how each one compares, and which way round it starts.
 *
 *  Returns and weights open at their largest, because "what did best" and
 *  "what am I most exposed to" are the questions being asked of them; a symbol
 *  opens at A, where nobody is looking for a ranking at all.
 *
 *  The gap to the portfolio rides along inside the return rather than as a
 *  fourth column: it is the return minus a constant within a period, so a
 *  column of it would sort into the order the return already gives and, at
 *  390px, would sit behind a horizontal scroll — which is where the number a
 *  reader came for must never be. */
const HOLDING_SORTS = {
  symbol: { dir: 'asc', compare: (a, b) => a.symbol.localeCompare(b.symbol) },
  weight: { dir: 'desc', compare: (a, b) => a.weight - b.weight },
  percent: { dir: 'desc', compare: (a, b) => a.percent - b.percent },
};

const HOLDING_SORT_DEFAULT = { key: 'percent', dir: 'desc' };

/** Which period is showing. The stored choice wins where the run can answer it —
 *  someone comparing two portfolios over three years should not have to pick 3Y
 *  twice — falling back to the year to date, and then to the whole run for a
 *  portfolio too young to have one. */
/** Which run the holding card on screen belongs to.
 *
 *  Portfolios and Funds show the same card over the same payload shape, and
 *  only one of them is ever rendered — so the period and the sort are read and
 *  written through here rather than being duplicated per page. Two copies of
 *  this logic would drift the moment one of them gained a column. */
function runSlot() {
  return route() === 'funds' ? 'fund' : 'backtest';
}

function currentRun() {
  return state[runSlot()];
}

function setRun(patch) {
  const slot = runSlot();
  state[slot] = { ...state[slot], ...patch };
}

function periodKey(periods) {
  const chosen = periods.find((p) => p.key === currentRun()?.period);
  if (chosen?.available) return chosen.key;
  const ytd = periods.find((p) => p.key === 'ytd' && p.available);
  return (ytd ?? periods.findLast((p) => p.available))?.key ?? '';
}

/** How the card describes itself and what it is measuring against.
 *
 *  A portfolio's holdings are the allocation somebody chose; a fund's are what
 *  it happens to hold today, and the difference is the whole reason the second
 *  page exists. The numbers and the sorting are identical, so only the words
 *  are parameterised. */
const HOLDINGS_CAPTION = {
  title: 'Holding performance',
  meta: 'what each holding did on its own',
  subject: 'portfolio',
};

function holdingsCard(b, caption = HOLDINGS_CAPTION) {
  const periods = b.performance ?? [];
  if (!periods.some((p) => p.available)) return '';

  const period = periods.find((p) => p.key === periodKey(periods));
  return `
    <div class="card">
      <div class="card__head">
        <div class="card__heading">
          <h3 class="card__title">${esc(caption.title)}</h3>
          <span class="card__meta">${esc(caption.meta)}</span>
        </div>
      </div>
      <div class="card__body">
        <div class="presets">
          ${periods
            .map(
              (p) => `<button class="btn btn--sm ${
                p.key === period.key ? 'btn--outline btn--active' : 'btn--ghost'
              }" type="button" data-action="period" data-key="${esc(p.key)}"${
                p.available ? '' : ` disabled title="${esc(p.label)} — the run doesn’t reach back that far"`
              }>${esc(PERIOD_CHIPS[p.key] ?? p.label)}</button>`,
            )
            .join('')}
        </div>
        ${holdingsTable(b, period, caption)}
      </div>
    </div>`;
}

function holdingsTable(b, period, caption) {
  const rows = sortedHoldings(holdingRows(b, period));
  if (!rows.length) return '<div class="empty">Nothing to measure over this period.</div>';

  return `
    <p class="field__hint holdings__when">
      ${esc(monthName(period.from))} → ${esc(monthName(period.to))} · ${esc(caption.subject)}
      <span class="perf-change perf-change--${direction(period.portfolio)}">${esc(
        percent(period.portfolio),
      )}</span>
    </p>
    ${missingNote(period)}
    <div class="table-scroll"><table class="table">
      <thead><tr>
        ${sortHeader('symbol', 'Holding')}
        ${sortHeader('weight', 'Weight')}
        ${sortHeader('percent', 'Return')}
      </tr></thead>
      <tbody>${rows.map((r) => holdingRow(b, r, period, caption)).join('')}</tbody>
    </table></div>`;
}

/** Which holdings this period could not measure, and why.
 *
 *  Always empty for a portfolio, whose legs are intersected before anything is
 *  measured. On a fund it is the difference between a table a reader can see is
 *  short and one they assume is complete — a ten-year window over a company
 *  that listed two years ago has nothing to say, and saying nothing about that
 *  is how a look-through quietly becomes a survivorship story. */
function missingNote(period) {
  const missing = period.missing ?? [];
  if (!missing.length) return '';
  return `
    <p class="field__hint holdings__missing">
      ${missing.map((s) => `<span class="chip">${esc(s)}</span>`).join(' ')}
      ${missing.length === 1 ? 'has' : 'have'} no price in ${esc(monthName(period.from))},
      so ${missing.length === 1 ? 'it is' : 'they are'} not in this period.
    </p>`;
}

/** The rows: every holding, and the benchmark as one of them where there is one.
 *
 *  In the table rather than only in the caption above it, because sorted by
 *  return it lands *in* the ranking — and "which of these beat the index" stops
 *  being arithmetic over eight rows and becomes a question about which side of
 *  one line a row is on. It costs nothing to put there: the number is already
 *  in the payload, measured over the same months by the same code.
 *
 *  It is not held, so it has no weight. That is a null rather than a zero: zero
 *  would rank it below every holding as though it were a position somebody had
 *  closed. */
function holdingRows(b, period) {
  const rows = (period.returns ?? []).map((r) => ({ ...r, isBenchmark: false }));
  if (b.benchmark && period.benchmark !== null && period.benchmark !== undefined) {
    rows.push({
      symbol: b.benchmark.label,
      weight: null,
      percent: period.benchmark,
      proxied: false,
      isBenchmark: true,
    });
  }
  return rows;
}

/** The rows in the order the reader asked for, leaving the server's own — best
 *  first — as the default. Sorting a copy: the payload is rendered again on
 *  every redraw, and a sort in place would make the arrow the only thing that
 *  could still say which order it is in. */
function sortedHoldings(rows) {
  const { key, dir } = holdingSort();
  const compare = HOLDING_SORTS[key].compare;
  return [...rows].sort((a, b) => {
    // The benchmark has no weight to be ranked by, so it sits under the
    // holdings whichever way that column points rather than leaping to the top
    // the moment the sort is reversed. Every other column ranks it like any
    // other row, which is the point of it being one.
    if (key === 'weight' && a.isBenchmark !== b.isBenchmark) return a.isBenchmark ? 1 : -1;
    return dir === 'asc' ? compare(a, b) : compare(b, a);
  });
}

function holdingSort() {
  const sort = currentRun()?.sort;
  return sort && HOLDING_SORTS[sort.key] ? sort : HOLDING_SORT_DEFAULT;
}

/** A sortable column head. The button is what makes the column reachable from a
 *  keyboard; aria-sort on the cell is what says, out loud, which way the table
 *  is currently ordered. The arrow is drawn only on the column doing the
 *  sorting — one on every head is three claims and one truth. */
function sortHeader(key, label) {
  const sort = holdingSort();
  const active = sort.key === key;
  return `
    <th${active ? ` aria-sort="${sort.dir === 'asc' ? 'ascending' : 'descending'}"` : ''}>
      <button class="th-sort${active ? ' th-sort--active' : ''}" type="button"
              data-action="holdings-sort" data-key="${esc(key)}">${esc(label)}<span
              class="th-sort__arrow" aria-hidden="true">${active ? (sort.dir === 'asc' ? '▲' : '▼') : ''}</span></button>
    </th>`;
}

function holdingRow(b, r, period, caption = HOLDINGS_CAPTION) {
  // The stand-in is named from the allocation rather than carried on the row:
  // the payload says *that* part of the move is a proxy's, and the holding
  // already says whose.
  const stand = (b.holdings ?? []).find((h) => h.symbol === r.symbol)?.replacement;
  // What the symbol actually is. A fund's top holdings are whatever it happens
  // to own, so half of them are tickers nobody recognises — `CCO.TO`,
  // `028260.KS` — and a table of those is a ranking of strangers. The name is
  // read off the fund's own constituent list rather than carried on the return,
  // which is keyed by symbol and knows nothing else; a portfolio, whose
  // holdings are symbols somebody chose and typed, has none to show and shows
  // none.
  const company = (b.constituents ?? []).find((c) => c.symbol === r.symbol)?.name;
  // The gap to the portfolio, which is what turns a return into a verdict. It
  // rides under the return rather than in a column of its own: within a period
  // it is the return minus a constant, so a column would sort into the order
  // the return already gives and, at 390px, would sit behind a horizontal
  // scroll. The unit is points, not percent — it is a difference of two
  // percentages — and what it is a gap *to* is the caption directly above.
  const gap = r.percent - period.portfolio;
  const against = `${Math.abs(gap).toFixed(2)} percentage points ${
    gap < 0 ? 'behind' : 'ahead of'
  } the ${caption.subject} over this period`;
  return `
    <tr${r.isBenchmark ? ' class="holding--benchmark"' : ''}>
      <th>
        <span class="holding__name">
          ${symbolMark(r.symbol)}
          <span>${esc(r.symbol)}</span>
          ${r.isBenchmark ? '<span class="chip">benchmark</span>' : ''}
          ${
            r.proxied && stand
              ? `<span class="chip__stand" title="Part of this move is ${esc(
                  stand,
                )}’s — it stood in before ${esc(r.symbol)}’s own history begins">← ${esc(stand)}</span>`
              : ''
          }
        </span>
        ${company ? `<span class="holding__company">${esc(company)}</span>` : ''}
      </th>
      <td class="field__hint">${
        r.isBenchmark
          ? '<span title="Nothing is held in it — it is here to be measured against">not held</span>'
          : `${esc(Number(r.weight).toLocaleString(undefined, { maximumFractionDigits: 2 }))}%`
      }</td>
      <td class="holding__return">
        <span class="perf-change perf-change--${direction(r.percent)}">${esc(percent(r.percent))}</span>
        <span class="perf-when" title="${esc(against)}">${esc(signed(gap))} pts</span>
      </td>
    </tr>`;
}

/** A yield is never signed — income is not a move — and a missing one is a year
 *  the source couldn't price, not a year that paid nothing. */
function yieldCell(value) {
  if (value === null || value === undefined) return '—';
  return `${value.toFixed(2)}%`;
}

/* ------------------------- Sector allocation -------------------------
 *
 * Where the money actually is, and how that compares with the funds a reader
 * names. It sits under the holding table because it answers the question that
 * one leaves open: "what did each holding do" is about the names, and this is
 * about what the names amount to.
 *
 * A look-through, not a lookup — the server adds up each holding's own
 * breakdown scaled by what it is held at, so two 60/40s built from different
 * index funds can and do come out ten points apart in technology.
 *
 * Portfolios and Funds share it, as they share the holding card, because a fund
 * is one holding at 100% and the maths does not care which page asked. */

/** The sectors, in the order the pies are drawn round and the colour tokens are
 *  numbered.
 *
 *  This list is `quotes.SectorNames` in the server, and the two have to agree —
 *  a sector's colour is looked up by its position here, and the server sorts
 *  slices into the same order so every pie on the card runs the same way round.
 *  That is the whole comparison mechanism: "Energy" is one colour in every pie,
 *  whichever pies are on screen, however big its slice is in each.
 *
 *  A sector the server sends that is not on this list is drawn in the neutral
 *  and still named in the table — see styles.css. */
const SECTORS = [
  'Basic Materials',
  'Communication Services',
  'Consumer Cyclical',
  'Consumer Defensive',
  'Energy',
  'Financial Services',
  'Healthcare',
  'Industrials',
  'Real Estate',
  'Technology',
  'Utilities',
];

/** A sector's fill. The neutral doubles as the colour of the part of a basket
 *  that has no sector at all, which is right: both are "nothing to say here",
 *  and the table beside the pie is what tells them apart by name. */
function sectorFill(name) {
  const slot = SECTORS.indexOf(name);
  return slot < 0 ? 'var(--sector-none)' : `var(--sector-${slot + 1})`;
}

/** A share of a basket. Never signed — an allocation is not a move — and one
 *  decimal, because the sources quote these to about that. */
function share(value) {
  if (value === null || value === undefined) return '—';
  return `${value.toFixed(1)}%`;
}

/** What the card is looking through, and what it compares against until
 *  somebody says otherwise.
 *
 *  `symbol` is given on the fund page, where the subject is the fund itself
 *  rather than an allocation; everywhere else the run's own holdings are it,
 *  with the weights the simulation actually used rather than the ones typed
 *  into the form.
 *
 *  The default comparison is the run's benchmark. It is the fund a reader has
 *  already chosen to measure this against on every other card on the page, so
 *  making them type it again here would be asking a question they have
 *  answered. */
function sectorSubject(b, { label, symbol } = {}) {
  const holdings = symbol
    ? [{ symbol, weight: 100 }]
    : (b.holdings ?? []).map((h) => ({ symbol: h.symbol, weight: h.weight }));
  return {
    // Which run this belongs to. A second portfolio, or another fund, is a
    // different subject and drops the card rather than showing the last one's
    // pies under the new one's name.
    key: `${runSlot()}:${symbol || currentRun()?.id || label}`,
    label,
    holdings,
    benchmark: b.benchmark?.label ?? '',
  };
}

/** The card. Its comparison box carries the subject in its draft key, so what
 *  is typed into one run's box is never put back into the next one's — a box
 *  reading "SPY, QQQ" over pies drawn for the benchmark alone is worse than an
 *  empty one. */
function sectorsCard(subject) {
  if (!subject.holdings.length) return '';
  const run = state.sectors[runSlot()];
  // Nothing until the lookup for *this* subject has been asked for, and nothing
  // ever on a quote source that cannot classify anything — a page missing a
  // card, not a broken one.
  if (!run || run.key !== subject.key || run.status === 'unavailable') return '';

  return `
    <div class="card">
      <div class="card__head">
        <div class="card__heading">
          <h3 class="card__title">Sector allocation</h3>
          <span class="card__meta">what the money is actually in, holding by holding</span>
        </div>
      </div>
      <div class="card__body">
        <form class="sectors__form" data-sector-form
              data-form-key="sectors:${esc(subject.key)}" autocomplete="off">
          <div class="field">
            <label class="field__label" for="sector-peers">Compare against</label>
            <input class="input" id="sector-peers" name="peers" type="text" placeholder="SPY, QQQ"
                   value="${esc(run.peers ?? '')}" />
            <span class="field__hint">
              ${
                subject.benchmark
                  ? `Defaults to the benchmark, ${esc(subject.benchmark)}. Clear it to see the allocation on its own.`
                  : 'Any funds you like, separated by commas. Clear it to see the allocation on its own.'
              }
            </span>
          </div>
          <div class="field field--actions">
            <button class="btn btn--outline" type="submit">Compare</button>
          </div>
        </form>
        ${sectorsBody(run)}
      </div>
    </div>`;
}

function sectorsBody(run) {
  if (run.status === 'loading') {
    return '<div class="empty"><strong>Looking through the holdings</strong>What each one is invested in, scaled by what it is held at.</div>';
  }
  if (run.status === 'error') {
    return `<div class="empty"><strong>Couldn't work out the sectors</strong>${esc(run.error)}</div>`;
  }

  const report = run.data;
  const baskets = [report.subject, ...(report.peers ?? [])];
  return `
    ${(report.notes ?? []).map((note) => `<p class="field__hint">${esc(note)}</p>`).join('')}
    <div class="sectors__pies">${baskets.map(sectorPie).join('')}</div>
    ${sectorTable(baskets)}
    ${baskets.map(unclassifiedNote).join('')}`;
}

/* The pie itself.
 *
 * A donut rather than a disc: the hole is where the coverage goes, and it is
 * the number that stops the picture reading as the whole basket. Slices are
 * separated by a ring of the card's own colour rather than by a stroke of
 * their own, so two neighbouring sectors are told apart by a gap and not only
 * by hue — which is what makes the card survive being looked at by somebody who
 * cannot see the difference between the two.
 *
 * The part of a basket nothing could be said about is drawn, in the neutral,
 * rather than the slices being scaled up to fill the circle. A bond fund is
 * mostly not in any equity sector, and a pie that hid that would say it was
 * 60% financials. */
const PIE = { size: 132, outer: 60, inner: 36, gap: 2 };

/** How much of a basket got a sector, as a figure to print.
 *
 *  Clamped at 100 because sources round: eleven weightings quoted to two
 *  decimals routinely add up to 100.4, and "100.4% in a sector" reads as a bug
 *  in this app rather than as rounding in somebody else's feed. The pie itself
 *  divides by the sum it actually has, so a basket over 100 is drawn in correct
 *  proportion either way. */
function coverage(basket) {
  return Math.min(100, basket.covered ?? 0);
}

function sectorPie(basket) {
  const slices = (basket.slices ?? []).filter((s) => s.weight > 0.005);
  const rest = 100 - (basket.covered ?? 0);
  // Last, and always last: it is the remainder, so it closes the circle.
  if (rest > 0.05) slices.push({ sector: '', weight: rest, rest: true });

  const label = `${basket.label}: ${
    slices.length
      ? slices.map((s) => `${s.sector || 'unclassified'} ${share(s.weight)}`).join(', ')
      : 'nothing the quote source will put in a sector'
  }`;

  return `
    <figure class="pie">
      <svg class="pie__chart" viewBox="0 0 ${PIE.size} ${PIE.size}" role="img" aria-label="${esc(label)}">
        ${
          slices.length
            ? slicePaths(slices)
            : `<circle cx="${PIE.size / 2}" cy="${PIE.size / 2}" r="${
                (PIE.outer + PIE.inner) / 2
              }" fill="none" stroke="var(--border)" stroke-width="${PIE.outer - PIE.inner}" />`
        }
      </svg>
      <figcaption class="pie__caption">
        <span class="pie__label">${esc(basket.label)}</span>
        <span class="pie__meta">${
          basket.covered > 0 ? `${esc(share(coverage(basket)))} in a sector` : 'no sector breakdown'
        }</span>
      </figcaption>
    </figure>`;
}

function slicePaths(slices) {
  const c = PIE.size / 2;
  // One slice at 100% has no arc — its start and end are the same point, and
  // every arc flag would be a guess. A ring is the same picture and cannot be
  // drawn wrong.
  if (slices.length === 1) {
    return `<circle cx="${c}" cy="${c}" r="${(PIE.outer + PIE.inner) / 2}" fill="none"
                    stroke="${sectorFill(slices[0].sector)}" stroke-width="${PIE.outer - PIE.inner}"
            ><title>${esc(sliceTitle(slices[0]))}</title></circle>`;
  }

  const total = slices.reduce((sum, s) => sum + s.weight, 0) || 100;
  // From twelve o'clock, clockwise, which is the only way anybody reads a pie.
  const point = (radius, turn) => {
    const angle = turn * 2 * Math.PI - Math.PI / 2;
    return `${(c + radius * Math.cos(angle)).toFixed(2)} ${(c + radius * Math.sin(angle)).toFixed(2)}`;
  };

  let at = 0;
  return slices
    .map((slice) => {
      const from = at;
      at += slice.weight / total;
      const big = at - from > 0.5 ? 1 : 0;
      const d = [
        `M${point(PIE.outer, from)}`,
        `A${PIE.outer} ${PIE.outer} 0 ${big} 1 ${point(PIE.outer, at)}`,
        `L${point(PIE.inner, at)}`,
        `A${PIE.inner} ${PIE.inner} 0 ${big} 0 ${point(PIE.inner, from)}`,
        'Z',
      ].join(' ');
      return `<path d="${d}" fill="${
        slice.rest ? 'var(--sector-none)' : sectorFill(slice.sector)
      }" stroke="var(--surface)" stroke-width="${PIE.gap}" stroke-linejoin="round"
      ><title>${esc(sliceTitle(slice))}</title></path>`;
    })
    .join('');
}

function sliceTitle(slice) {
  return `${slice.sector || 'Not in any sector the quote source names'} — ${share(slice.weight)}`;
}

/** The legend and the numbers, in one table.
 *
 *  One thing rather than two because they are one thing: a swatch beside a name
 *  is what makes the pies readable, and the exact figures beside it are what
 *  makes them comparable. Three of the eleven fills sit under 3:1 against the
 *  light card, so the numbers being here in text is not a nicety — it is what
 *  keeps the card readable when the colours are not. */
function sectorTable(baskets) {
  const rows = SECTORS.filter((name) =>
    baskets.some((b) => (b.slices ?? []).some((s) => s.sector === name)),
  );
  // Anything the server named that this build has never heard of, after the
  // ones it has. Never in practice; visible rather than swallowed if ever.
  for (const basket of baskets) {
    for (const slice of basket.slices ?? []) {
      if (!rows.includes(slice.sector)) rows.push(slice.sector);
    }
  }
  if (!rows.length) return '';

  const weightIn = (basket, sector) =>
    (basket.slices ?? []).find((s) => s.sector === sector)?.weight;

  return `
    <div class="table-scroll"><table class="table sectors__table">
      <thead><tr>
        <th>Sector</th>
        ${baskets.map((b) => `<th class="sectors__col">${esc(b.label)}</th>`).join('')}
      </tr></thead>
      <tbody>
        ${rows
          .map(
            (sector) => `<tr>
              <th><span class="sector__name"><span class="sector__swatch" style="background:${sectorFill(
                sector,
              )}"></span>${esc(sector)}</span></th>
              ${baskets
                .map((b) => `<td class="sectors__col">${esc(share(weightIn(b, sector)))}</td>`)
                .join('')}
            </tr>`,
          )
          .join('')}
        <tr class="sectors__rest">
          <th><span class="sector__name"><span class="sector__swatch"
              style="background:var(--sector-none)"></span>Unclassified</span></th>
          ${baskets
            .map(
              (b) => `<td class="sectors__col">${esc(share(100 - coverage(b)))}</td>`,
            )
            .join('')}
        </tr>
      </tbody>
    </table></div>`;
}

/** Which of a basket's holdings the source would not place.
 *
 *  Named rather than folded silently into the remainder, because for a
 *  portfolio it is usually the most interesting line on the card: "40% of this
 *  is gold" is not a gap in the data, it is the allocation. */
function unclassifiedNote(basket) {
  const missing = basket.unclassified ?? [];
  // A basket that *is* one symbol has already said this twice — "no sector
  // breakdown" under its pie and 100% on the Unclassified row — and naming the
  // fund inside itself reads as a stutter.
  if (!missing.length || basket.symbol) return '';
  return `
    <p class="field__hint">
      In ${esc(basket.label)}, ${missing.map((s) => `<span class="chip">${esc(s)}</span>`).join(' ')}
      ${missing.length === 1 ? 'is' : 'are'} not in any sector the quote source names.
    </p>`;
}

/** Look an allocation's sectors up. `typed` is given only when somebody edited
 *  the comparison box; otherwise the choice already on screen is carried, and a
 *  fresh subject falls back to its own benchmark. */
async function loadSectors(subject, typed) {
  // The slot is read once: the route can change while the request is in flight,
  // and the answer belongs to the page that asked for it either way.
  const slot = runSlot();
  // Unset means "nobody has said", which takes the benchmark; an empty string
  // means somebody cleared the box, which means none. The same distinction the
  // fund page draws, for the same reason.
  const carried = state.sectors[slot]?.key === subject.key ? state.sectors[slot].peers : undefined;
  const peers = typed ?? carried ?? subject.benchmark;
  state.sectors[slot] = { key: subject.key, peers, status: 'loading' };
  render({ force: true });

  try {
    const body = await post('/sectors', {
      holdings: subject.holdings,
      label: subject.label,
      peers: symbolList(peers),
    });
    // The page may have moved on — another run, or off it entirely — while the
    // request was in flight.
    if (state.sectors[slot]?.key !== subject.key) return;
    state.sectors[slot] = { ...state.sectors[slot], status: 'ready', data: body.sectors };
  } catch (err) {
    if (state.sectors[slot]?.key !== subject.key) return;
    state.sectors[slot] = {
      ...state.sectors[slot],
      // A quote source that cannot classify anything answers 501, and that is a
      // configuration somebody chose rather than a fault: the card goes away
      // instead of accusing the page of being broken.
      status: err.status === 501 ? 'unavailable' : 'error',
      error: err.message || String(err),
    };
  }
  render({ force: true });
}

/* ------------------------------ Funds -------------------------------
 *
 * A fund page is the backtest page with one holding in it, and the holding
 * card pointed at what the fund holds rather than at an allocation. Everything
 * here is layout and words; the components are the ones Portfolios uses.
 *
 * Nothing on this page is saved. A fund is opened, and the symbol lives in the
 * URL — so a page is something you can send somebody, and the back button does
 * what it looks like it does. */

/** What a fund's holding card calls itself. The count and the coverage are in
 *  the meta line because they are the two facts that stop the table reading as
 *  the fund: twenty names and 65% is a very different claim from ten and 24%. */
/** What a fund is compared against until somebody says otherwise.
 *
 *  A default rather than a placeholder, and it is written into the field: a
 *  fund on its own is a number without a verdict, and a comparison that applied
 *  invisibly would be worse than none. Clearing the box means none, which is
 *  why an empty benchmark and an unset one are different things below. A fund
 *  benchmarked against itself is dropped by the server. */
const DEFAULT_BENCHMARK = 'SPY';

function fundCaption(f) {
  return {
    title: `Top ${f.constituents?.length ?? 0} holdings`,
    meta: `${(f.covered ?? 0).toFixed(1)}% of the fund · what each of them did on its own`,
    subject: 'fund',
  };
}

function renderFunds() {
  const run = state.fund;

  return `
    <div class="page-head">
      <div>
        <h1>Funds</h1>
        <p>
          Look through an ETF: what it returned, year by year, and what the
          things it holds today have done. Any symbol the quote source can price
          opens here.
        </p>
      </div>
    </div>

    <div class="card">
      <div class="card__body">
        <form class="form-grid" data-fund-form data-form-key="fund-open" autocomplete="off">
          <div class="field">
            <label class="field__label" for="fund-symbol">Fund</label>
            <input class="input" id="fund-symbol" name="symbol" type="text" placeholder="QQQ"
                   value="${esc(run?.symbol ?? '')}" />
          </div>
          <div class="field">
            <label class="field__label" for="fund-benchmark">Compare against</label>
            <input class="input" id="fund-benchmark" name="benchmark" type="text"
                   value="${esc(run?.benchmark ?? DEFAULT_BENCHMARK)}" />
            <span class="field__hint">Clear it to see the fund on its own.</span>
          </div>
          <div class="field field--actions">
            <button class="btn btn--primary" type="submit">Look through</button>
          </div>
        </form>
      </div>
    </div>

    ${renderFund(run)}`;
}

function renderFund(run) {
  if (!run) return '';

  if (run.status === 'loading') {
    return `
      <div class="card"><div class="card__body">
        <div class="empty"><strong>Looking through ${esc(run.symbol)}</strong>Its own history since it listed, and a series for each thing it holds.</div>
      </div></div>`;
  }
  if (run.status === 'error') {
    return `
      <div class="card"><div class="card__body">
        <div class="empty"><strong>Couldn't open ${esc(run.symbol)}</strong>${esc(run.error)}</div>
      </div></div>`;
  }

  const f = run.data;
  const caption = fundCaption(f);

  return `
    <div class="page-head page-head--sub">
      <div>
        <h2>${esc(f.symbol)} <span class="chip">fund</span></h2>
        <p>
          ${f.name ? `${esc(f.name)} — ` : ''}${esc(amount(f.initial))} from
          ${esc(monthName(f.start))} to ${esc(monthName(f.end))}, ${f.months}
          ${f.months === 1 ? 'month' : 'months'}.
        </p>
      </div>
      <div class="perf-head__delta">
        <div class="perf-head__value">${esc(amount(f.portfolio.end))}</div>
        <div class="perf-change perf-change--${direction(f.portfolio.totalPercent)}">${esc(
          percent(f.portfolio.totalPercent),
        )}</div>
      </div>
    </div>

    ${(f.notes ?? []).map((note) => `<p class="field__hint backtest-note">${esc(note)}</p>`).join('')}
    <p class="field__hint backtest-note">Holdings read ${esc(ago(f.asOf))}.</p>

    <div class="card">
      <div class="card__body">
        ${growthChart(f)}
        ${growthLegend(f, f.symbol)}
      </div>
    </div>

    <div class="card">
      <div class="card__head"><h3 class="card__title">Summary</h3></div>
      <div class="card__body">${metricsTable(f)}</div>
    </div>

    <div class="card">
      <div class="card__head"><h3 class="card__title">Calendar years</h3></div>
      <div class="card__body">${annualTable(f, f.symbol)}</div>
    </div>

    ${holdingsCard(f, caption)}
    ${sectorsCard(sectorSubject(f, { label: f.symbol, symbol: f.symbol }))}
    ${unpricedCard(f)}`;
}

/** The holdings the quote source cannot price at all.
 *
 *  Their own card rather than a row with a dash in it: they are part of the
 *  fund and they are not part of any of the numbers above, which is a fact
 *  about the page's coverage rather than a result. A fund with none of them —
 *  most of them — shows nothing here. */
function unpricedCard(f) {
  const unpriced = (f.constituents ?? []).filter((c) => !c.priced);
  if (!unpriced.length) return '';

  return `
    <div class="card">
      <div class="card__head">
        <div class="card__heading">
          <h3 class="card__title">Held, and not priced</h3>
          <span class="card__meta">in the fund; no series to measure</span>
        </div>
      </div>
      <div class="card__body">
        <div class="table-scroll"><table class="table">
          <thead><tr><th>Holding</th><th>Weight</th></tr></thead>
          <tbody>${unpriced
            .map(
              (c) => `<tr>
                <th><span class="holding__name">${symbolMark(c.symbol)}<span>${esc(
                  c.symbol,
                )}</span></span>${
                  c.name ? `<span class="holding__company">${esc(c.name)}</span>` : ''
                }</th>
                <td class="field__hint">${esc(
                  Number(c.weight).toLocaleString(undefined, { maximumFractionDigits: 2 }),
                )}%</td>
              </tr>`,
            )
            .join('')}</tbody>
        </table></div>
      </div>
    </div>`;
}

/** Open a fund. The symbol in the URL is what asked for this — see refreshView
 *  — so this only ever fetches and redraws. */
async function loadFund(symbol) {
  symbol = symbol.trim().toUpperCase();
  if (!symbol) return;

  // The benchmark and the card's period survive the lookup, for the reason a
  // backtest's do: comparing two funds against the same index should not mean
  // typing it twice.
  // Unset means "nobody has said", which takes the default; empty means
  // somebody cleared the box, which means none.
  const { period, sort } = state.fund ?? {};
  const benchmark = state.fund?.benchmark ?? DEFAULT_BENCHMARK;
  state.fund = { symbol, benchmark, period, sort, status: 'loading' };
  render({ force: true });

  try {
    const query = benchmark ? `?benchmark=${encodeURIComponent(benchmark)}` : '';
    const body = await api(`/funds/${encodeURIComponent(symbol)}${query}`);
    // The page may have moved on — another fund, or off Funds entirely — while
    // the request was in flight.
    if (state.fund?.symbol !== symbol) return;
    state.fund = { ...state.fund, status: 'ready', data: body.fund };
    // After the run, not alongside it: the sector card is a second fan-out
    // upstream, and it must not hold up the page it sits at the bottom of.
    loadSectors(sectorSubject(body.fund, { label: symbol, symbol }));
  } catch (err) {
    if (state.fund?.symbol !== symbol) return;
    state.fund = { ...state.fund, status: 'error', error: err.message || String(err) };
  }
  render({ force: true });
}

/* --------------------------- Publishing ----------------------------
 *
 * Destinations, the payload they receive, and the cycles that sent it. The
 * last of those was its own Activity page until the run log turned out to be
 * read almost exclusively to answer a publishing question — "did it go?", "why
 * didn't it?" — which is one page away from the destination it is asking
 * about. It reads better underneath them than beside them. */

function renderPublishing(data) {
  const preview = JSON.stringify(data.preview, null, 2);

  return `
    <div class="page-head page-head--sub" id="section-publishing">
      <div>
        <h2>Publishing</h2>
        <p>
          After every refresh the snapshot is PUT to
          <code>{base URL}/{key}</code>; if that fails it is POSTed to the base
          URL instead — the same two-step the original script used, so an
          existing consumer needs no changes.
        </p>
      </div>
      <button class="btn btn--outline" id="publish-now" type="button">Publish now</button>
    </div>

    <div class="card">
      <div class="card__head">
        <h3 class="card__title">Destinations</h3>
        <button class="btn btn--sm btn--primary" data-action="new-sink" type="button">Add destination</button>
      </div>
      <div class="card__body">
        ${
          state.editingSinks.has('new')
            ? sinkForm(null, data)
            : ''
        }
        ${
          data.sinks.length === 0 && !state.editingSinks.has('new')
            ? `<div class="empty"><strong>No destinations yet</strong>Quotes are still fetched and stored — they just aren't sent anywhere.</div>`
            : data.sinks
                .map((s) =>
                  state.editingSinks.has(s.id)
                    ? sinkForm(s, data)
                    : sinkRow(s),
                )
                .join('')
        }
      </div>
    </div>

    <div class="card">
      <div class="card__head">
        <h3 class="card__title">Payload preview</h3>
        <span class="field__hint">format: minion (legacy) · exactly what a destination receives right now</span>
      </div>
      <div class="card__body">
        <pre class="code code--payload">${esc(preview)}</pre>
      </div>
    </div>

    ${renderCycles(data)}
  `;
}

/** The refresh loop's state and its audit log — every cycle, whether it
 *  succeeded or not, and what each destination did with it. */
function renderCycles(data) {
  const runs = state.runs;
  const engine = data.engine;

  return `
    <div class="page-head page-head--sub" id="section-cycles">
      <div>
        <h2>Recent cycles</h2>
        <p>
          ${
            // "The last 25 of them" rather than "25", because with a button
            // offering more the count is a position in a log, not its size.
            state.runsMore
              ? `The newest ${runs.length} refresh cycles, and there are older ones.`
              : `All ${runs.length} recorded refresh ${runs.length === 1 ? 'cycle' : 'cycles'}, newest first.`
          }
          Every cycle is recorded whether it succeeded or not.
        </p>
      </div>
      <button class="btn btn--outline" id="refresh-view" type="button">Reload</button>
    </div>

    <div class="stat-row">
      <div class="stat"><div class="stat__label">Status</div><div class="stat__value">${engine.running ? 'refreshing' : 'idle'}</div></div>
      <div class="stat"><div class="stat__label">Next run</div><div class="stat__value">${esc(clock(engine.nextRun))}</div></div>
      <div class="stat"><div class="stat__label">Interval</div><div class="stat__value">${esc(duration(data.settings.refreshSeconds))}</div></div>
      <div class="stat"><div class="stat__label">Provider</div><div class="stat__value">${esc(engine.provider)}</div></div>
    </div>

    <div class="card">
      <div class="card__body" style="padding:0">
        ${
          runs.length === 0
            ? `<div class="empty"><strong>No runs recorded yet</strong>The first cycle runs at startup.</div>`
            : `<div class="table-scroll"><table class="table table--runs">
                <thead><tr>
                  <th>When</th><th>Quotes</th><th>Published</th><th>Detail</th>
                </tr></thead>
                <tbody>${runs.map(runRow).join('')}</tbody>
              </table></div>`
        }
        ${
          state.runsMore && state.runsShown < RUNS_MAX
            ? `<div class="runs__more">
                 <button class="btn btn--sm btn--outline" data-action="more-runs" type="button">
                   Show ${RUNS_PAGE} older
                 </button>
               </div>`
            : ''
        }
      </div>
    </div>
  `;
}

function sinkRow(s) {
  return `
    <div class="card" style="margin-top:0.7rem">
      <div class="card__head">
        <h3 class="card__title">
          ${esc(s.name)}
          ${s.enabled ? '<span class="chip chip--ok">enabled</span>' : '<span class="chip chip--off">disabled</span>'}
          <span class="chip">${esc(s.format)}</span>
        </h3>
        <div style="display:flex;gap:0.3rem;flex-wrap:wrap">
          <button class="btn btn--sm btn--outline" data-action="test-sink" data-id="${esc(s.id)}">Test</button>
          <button class="btn btn--sm btn--ghost" data-action="edit-sink" data-id="${esc(s.id)}">Edit</button>
          <button class="btn btn--sm btn--ghost" data-action="toggle-sink" data-id="${esc(s.id)}">${s.enabled ? 'Disable' : 'Enable'}</button>
          <button class="btn btn--sm btn--danger" data-action="delete-sink" data-id="${esc(s.id)}">Remove</button>
        </div>
      </div>
      <div class="card__body">
        <div class="table-scroll">
          <table class="table">
            <tbody>
              <tr><th>Endpoint</th><td class="wrap"><code>${esc(s.baseUrl)}/${esc(s.key)}</code></td></tr>
              <tr><th>Key</th><td><code>${esc(s.key)}</code></td></tr>
              <tr><th>Category</th><td>${s.category ? `<code>${esc(s.category)}</code>` : '<span class="field__hint">none</span>'}</td></tr>
              <tr><th>Timeout</th><td>${esc(s.timeoutMs)} ms</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  `;
}

function sinkForm(s, data) {
  const formats = data.meta.formats ?? ['minion', 'detailed'];
  const id = s?.id ?? 'new';
  return `
    <form class="card" style="margin-top:0.7rem" data-sink-form="${esc(id)}"
          data-form-key="sink:${esc(id)}" autocomplete="off">
      <div class="card__head">
        <h3 class="card__title">${s ? `Edit ${esc(s.name)}` : 'New destination'}</h3>
      </div>
      <div class="card__body">
        <div class="form-grid">
          <div class="field">
            <label class="field__label">Name</label>
            <input class="input" name="name" value="${esc(s?.name ?? '')}" placeholder="Home dashboard" />
          </div>
          <div class="field">
            <label class="field__label">Base URL</label>
            <input class="input" name="baseUrl" value="${esc(s?.baseUrl ?? '')}"
                   placeholder="http://100.84.70.60:9999/api/entries" required />
          </div>
          <div class="field">
            <label class="field__label">Key</label>
            <input class="input" name="key" value="${esc(s?.key ?? '')}" placeholder="minion-quotes" required />
          </div>
          <div class="field">
            <label class="field__label">Category</label>
            <input class="input" name="category" value="${esc(s?.category ?? '')}" placeholder="minion" />
          </div>
          <div class="field">
            <label class="field__label">Format</label>
            <select class="select" name="format">
              ${formats
                .map(
                  (f) =>
                    `<option value="${esc(f)}"${(s?.format ?? 'minion') === f ? ' selected' : ''}>${esc(f)}${
                      f === 'minion' ? ' — legacy flat strings' : ' — objects with change'
                    }</option>`,
                )
                .join('')}
            </select>
          </div>
          <div class="field">
            <label class="field__label">Timeout (ms)</label>
            <input class="input" name="timeoutMs" type="number" min="500" step="500"
                   value="${esc(s?.timeoutMs ?? 10000)}" />
          </div>
        </div>
        <div class="form-actions">
          <button class="btn btn--primary" type="submit">${s ? 'Save' : 'Add destination'}</button>
          <button class="btn btn--ghost" type="button" data-action="cancel-sink" data-id="${esc(id)}">Cancel</button>
        </div>
      </div>
    </form>
  `;
}

function runRow(run) {
  const took = Math.max(0, new Date(run.finishedAt) - new Date(run.startedAt));
  const publishes = run.publishes ?? [];
  const failed = publishes.filter((p) => !p.ok);

  let detail;
  if (run.error) {
    detail = `<span class="chip chip--error">error</span> ${esc(run.error)}`;
  } else if (failed.length) {
    detail = failed
      .map((p) => `<span class="chip chip--error">${esc(p.sinkName)}</span> ${esc(p.error ?? '')}`)
      .join('<br />');
  } else if (publishes.length) {
    detail = publishes
      .map((p) => `<span class="chip chip--ok">${esc(p.sinkName)}</span> ${esc(p.method)} ${esc(p.statusCode)}`)
      .join(' ');
  } else {
    detail = '<span class="field__hint">no destinations</span>';
  }

  // What started the cycle and how long it took ride under when it ran: three
  // facts about the run itself, against three columns of what it produced. Six
  // columns pushed Detail — the only one that ever says anything unexpected —
  // off the side of a phone entirely.
  return `
    <tr>
      <td title="${esc(run.startedAt)}">${esc(ago(run.finishedAt))}
        <span class="perf-when">${esc(run.trigger)} · ${took} ms</span></td>
      <td>${run.okCount} ok${
        run.errorCount
          ? `<span class="perf-when" style="color:var(--down)">${run.errorCount} failed</span>`
          : ''
      }</td>
      <td>${publishes.length - failed.length}/${publishes.length}<span
        class="runs__label"> published</span></td>
      <td class="wrap">${detail}</td>
    </tr>
  `;
}

/* ---------------------------- Settings ----------------------------- */

/** Interval presets, so the common cadences are one tap rather than arithmetic
 *  in an number field. The floor (30s) is enforced server-side too. */
const INTERVAL_PRESETS = [
  { label: '30s', seconds: 30 },
  { label: '1 min', seconds: 60 },
  { label: '5 min', seconds: 300 },
  { label: '15 min', seconds: 900 },
  { label: '1 hour', seconds: 3600 },
];

/** What the logo cache has actually managed to fetch.
 *
 *  Without this the feature is untestable from the UI: logos on and no logos
 *  showing is the same picture whether the symbols genuinely haven't got any,
 *  the configured URL is wrong, or the cycle hasn't got round to them yet. The
 *  server records why each symbol came back empty; this reports the commonest
 *  reason, which is the one that says which of those three it is. */
function logoReport(data) {
  if (!data.settings.logos) return '';
  const stats = data.logoStats ?? { ok: 0, none: 0 };
  if (!stats.ok && !stats.none) {
    return `<span class="field__hint">Nothing fetched yet — the next refresh starts on it.</span>`;
  }
  const answered = stats.ok + stats.none;
  return `<span class="field__hint">
    <strong>${stats.ok} of ${answered}</strong> symbols asked about have a logo.
    ${
      stats.none
        ? `${stats.none} came back without one${stats.reason ? `: ${esc(stats.reason)}` : ''}.`
        : ''
    }
  </span>`;
}

function renderSettings(data) {
  const s = data.settings;
  const min = data.meta.minRefreshSeconds ?? 30;
  const minTimeout = data.meta.minQuoteTimeout ?? 5;
  const maxTimeout = data.meta.maxQuoteTimeout ?? 120;
  const maxPinned = data.meta.maxPinnedSymbols ?? 50;
  const provider = data.provider;
  const runtime = data.runtime ?? {};
  // The chips highlight whatever is in the field, which after an unsaved edit
  // is the draft rather than the saved interval.
  const shownRefresh = Number(draftValue('settings-form', 'refreshSeconds') ?? s.refreshSeconds);

  return `
    <div class="page-head">
      <div>
        <h1>Settings</h1>
        <p>Changes take effect immediately — the refresh loop and the quote source re-read these every cycle, so nothing here needs a restart.</p>
      </div>
    </div>

    <form class="card" id="settings-form">
      <div class="card__head"><h2 class="card__title">Refresh loop</h2></div>
      <div class="card__body">
        <div class="form-grid">
          <div class="field">
            <label class="field__label" for="refreshSeconds">Fetch every (seconds)</label>
            <input class="input" id="refreshSeconds" name="refreshSeconds" type="number"
                   min="${min}" step="10" value="${esc(s.refreshSeconds)}" />
            <div class="presets">
              ${INTERVAL_PRESETS.filter((p) => p.seconds >= min)
                .map(
                  (p) =>
                    `<button class="btn btn--sm ${
                      p.seconds === shownRefresh ? 'btn--outline btn--active' : 'btn--ghost'
                    }" type="button" data-preset="${p.seconds}">${esc(p.label)}</button>`,
                )
                .join('')}
            </div>
            <span class="field__hint">Minimum ${min}s — the provider is free and unauthenticated, so polling harder is how you get rate-limited.</span>
          </div>
          <div class="field">
            <label class="field__label" for="historyHours">History retention (hours)</label>
            <input class="input" id="historyHours" name="historyHours" type="number"
                   min="0" step="1" value="${esc(s.historyHours)}" />
            <span class="field__hint">Backs the sparklines. 0 keeps everything.</span>
          </div>
          <div class="field">
            <label class="field__label">Publishing</label>
            <label class="checkbox">
              <input type="checkbox" name="publishOnRefresh" ${s.publishOnRefresh ? 'checked' : ''} />
              Publish after every refresh
            </label>
            <span class="field__hint">Off means quotes are still stored, but only sent when you press Publish now.</span>
          </div>
          <div class="field">
            <label class="field__label">Symbol logos</label>
            <label class="checkbox">
              <input type="checkbox" name="logos" ${s.logos ? 'checked' : ''} />
              Fetch a real logo for each symbol
            </label>
            <span class="field__hint">
              Off by default, because this is the one setting that has the
              server ask about your symbols by name. Each logo is fetched once
              and cached here, so your browser never talks to anyone else and
              the watchlist still works offline. Symbols with no logo — funds,
              crypto, ratios, portfolios — keep the mark drawn from their name.
              Turning it off again empties the cache.
            </span>
            ${logoReport(data)}
          </div>
          <div class="field">
            <label class="field__label" for="logoUrlTemplate">Logo URL</label>
            <input class="input input--mono" id="logoUrlTemplate" name="logoUrlTemplate"
                   value="${esc(s.logoUrlTemplate)}" placeholder="the quote source's own" />
            <span class="field__hint">
              Where a logo comes from, with <code>{symbol}</code> standing in for
              the ticker (<code>{symbol_lower}</code> for the lower-case form,
              <code>{key}</code> for the key below). Leave it blank to use
              whatever the quote source itself offers — which for Yahoo is a
              logo on some search results and nothing at all for most symbols.
              Changing this clears the cache, so the next few refreshes ask the
              new source.
            </span>
          </div>
          <div class="field">
            <label class="field__label" for="logoKey">Logo key</label>
            <input class="input input--mono" id="logoKey" name="logoKey" type="password"
                   autocomplete="off" spellcheck="false"
                   placeholder="${s.logoKeySet ? 'stored — type to replace' : 'none'}" />
            <span class="field__hint">
              For a source that wants credentials. Put <code>{key}</code> in the
              URL above and it goes there; leave it out and it is sent as a
              bearer token instead — which is where a server-side secret
              belongs. It is never sent back to this page, so an empty box means
              "leave it as it is".
            </span>
            ${
              s.logoKeySet
                ? `<label class="checkbox">
                     <input type="checkbox" name="forgetLogoKey" />
                     Forget the stored key
                   </label>`
                : ''
            }
          </div>
        </div>

        <h3 class="card__subtitle">Pinned tickers</h3>
        <p class="field__hint" style="margin:0 0 0.7rem">
          Symbols listed here sort above everything else on the watchlist. It is
          a set, not an order — the watchlist's own order still decides the
          sequence within the pinned group, so drag-to-reorder keeps working.
          A symbol that isn't on the watchlist is simply ignored.
        </p>
        <div class="form-grid">
          <div class="field">
            <label class="field__label" for="pinnedSymbols">Pinned symbols</label>
            <input class="input input--mono" id="pinnedSymbols" name="pinnedSymbols"
                   value="${esc((s.pinnedSymbols ?? []).join(', '))}"
                   placeholder="VTI, BTC-USD" />
            <span class="field__hint">
              Comma-separated, up to ${maxPinned}. Leave empty to pin nothing —
              the <strong>Pin</strong> button on each row edits this same list.
            </span>
          </div>
        </div>

        ${
          provider
            ? `
        <h3 class="card__subtitle">Quote source</h3>
        <p class="field__hint" style="margin:0 0 0.7rem">
          Where prices come from. Leave a field blank to use the default shown in it —
          the values in force right now are listed under <em>Server</em> below.
        </p>
        <div class="form-grid">
          <div class="field">
            <label class="field__label" for="quoteBaseUrl">Server URL</label>
            <input class="input" id="quoteBaseUrl" name="quoteBaseUrl" type="url"
                   value="${esc(s.quoteBaseUrl)}" placeholder="${esc(provider.defaultBaseUrl)}" />
            <span class="field__hint">Point at a mirror or a caching proxy. Must be http:// or https://.</span>
          </div>
          <div class="field">
            <label class="field__label" for="quoteTimeoutSeconds">Request timeout (seconds)</label>
            <input class="input" id="quoteTimeoutSeconds" name="quoteTimeoutSeconds" type="number"
                   min="0" max="${maxTimeout}" step="1" value="${esc(s.quoteTimeoutSeconds || '')}"
                   placeholder="${esc(provider.defaultTimeoutSeconds)}" />
            <span class="field__hint">${minTimeout}–${maxTimeout}s, or blank for the default.</span>
          </div>
          <div class="field">
            <label class="field__label" for="quoteUserAgent">User agent</label>
            <input class="input" id="quoteUserAgent" name="quoteUserAgent"
                   value="${esc(s.quoteUserAgent)}" placeholder="browser default" />
            <span class="field__hint">Yahoo stonewalls obvious scripts. If every symbol suddenly reads N/A, a fresher browser string is the usual fix.</span>
          </div>
        </div>`
            : ''
        }

        <div class="form-actions">
          <button class="btn btn--primary" type="submit">Save settings</button>
          ${provider ? `<button class="btn btn--outline" type="button" id="test-provider">Test connection</button>` : ''}
        </div>
      </div>
    </form>

    <div class="card">
      <div class="card__head">
        <h2 class="card__title">Server</h2>
        <span class="field__hint">Start-up flags — change these in the systemd unit, then restart.</span>
      </div>
      <div class="card__body">
        <div class="table-scroll">
          <table class="table">
            <tbody>
              <tr><th>Version</th><td><code>${esc(data.version)}</code></td></tr>
              <tr><th>Listening on</th><td><code>${esc(runtime.listenAddr ?? '—')}</code></td></tr>
              <tr><th>Database</th><td class="wrap"><code>${esc(runtime.dbPath ?? '—')}</code></td></tr>
              <tr><th>Web client</th><td class="wrap"><code>${esc(runtime.webSource ?? '—')}</code></td></tr>
              ${
                provider
                  ? `<tr><th>Quote URL in use</th><td class="wrap"><code>${esc(provider.baseUrl)}</code></td></tr>
                     <tr><th>Timeout in use</th><td>${esc(provider.timeoutSeconds)}s</td></tr>
                     <tr><th>User agent in use</th><td class="wrap"><code>${esc(provider.userAgent)}</code></td></tr>`
                  : `<tr><th>Quote provider</th><td>${esc(data.engine.provider)}</td></tr>`
              }
              <tr><th>Seeded symbols</th><td class="wrap"><code>${esc((data.meta.seedSymbols ?? []).join(', '))}</code></td></tr>
              <tr><th>Upgrades</th><td class="wrap">Re-run <code>scripts/quickstart.sh</code>. It snapshots the database, swaps code in, and rolls back if the new version fails its health check.</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>

    ${
      // Publishing last, and in this order: where snapshots go, what they look
      // like, and what happened to them. It ends the page because the cycle log
      // is the one section that grows — everything above it stays a fixed
      // height however long the app has been running.
      renderPublishing(data)
    }
  `;
}

/* ------------------------------------------------------------------ *
 * Events — one delegated listener per kind, on containers that outlive
 * every re-render.
 * ------------------------------------------------------------------ */

function orderedIDs() {
  return state.data.tickers.map((t) => t.id);
}

/** Move a ticker one slot and persist the new order. */
function nudge(id, delta) {
  const ids = orderedIDs();
  const from = ids.indexOf(id);
  const to = from + delta;
  if (from < 0 || to < 0 || to >= ids.length) return;
  ids.splice(to, 0, ids.splice(from, 1)[0]);
  act(() => post('/tickers/reorder', { ids }));
}

/** Send a chosen file as one symbol's logo.
 *
 *  The file is the whole request body rather than a multipart form: the input
 *  hands over a File, `fetch` sends it as-is, and the server has the bytes
 *  without a form parser in between. The response is small; what matters is
 *  reloading state afterwards, since the mark's URL carries a version that has
 *  just changed. */
async function uploadLogo(symbol, file) {
  const res = await fetch(logoURL(symbol), { method: 'PUT', body: file });
  if (!res.ok) {
    const detail = await res.json().catch(() => ({}));
    throw new Error(detail.error || `upload failed (${res.status})`);
  }
}

$('#view').addEventListener('change', (event) => {
  const input = event.target.closest('[data-logo-upload]');
  if (!input || !input.files?.length) return;
  const symbol = input.dataset.logoUpload;
  const file = input.files[0];
  // Cleared straight away: leaving the file selected would re-upload it if the
  // form were submitted, and the input is thrown away by the next redraw.
  input.value = '';
  act(() => uploadLogo(symbol, file), { success: `${symbol}'s logo updated` });
});

$('#view').addEventListener('click', (event) => {
  const button = event.target.closest('[data-action]');
  if (!button) return;
  const { action, id } = button.dataset;

  switch (action) {
    case 'edit':
      state.editing.add(id);
      render({ force: true });
      break;
    case 'remove-logo':
      act(() => del(`/logos/${encodeURIComponent(button.dataset.symbol)}`), {
        success: 'Logo removed',
      });
      break;
    case 'cancel-edit':
      state.editing.delete(id);
      clearDraft(`ticker:${id}`);
      render({ force: true });
      break;
    // The holding table's period and its sort. Both are purely a redraw: every
    // period came back with the run, so neither costs a request or can fail.
    case 'period':
      setRun({ period: button.dataset.key });
      render({ force: true });
      break;
    // Opening the log deeper. It re-fetches rather than appending, so the rows
    // already on screen are refreshed by the same request that adds the older
    // ones and the newest cycle stays the first row.
    case 'more-runs':
      state.runsShown = Math.min(state.runsShown + RUNS_PAGE, RUNS_MAX);
      refreshView({ force: true });
      break;
    case 'holdings-sort': {
      const key = button.dataset.key;
      const sort = holdingSort();
      // The same column again reverses it; a new one opens the way that column
      // is worth reading — biggest return, biggest weight, but symbols from A.
      setRun({
        sort:
          sort.key === key
            ? { key, dir: sort.dir === 'asc' ? 'desc' : 'asc' }
            : { key, dir: HOLDING_SORTS[key].dir },
      });
      render({ force: true });
      break;
    }
    case 'toggle': {
      const t = state.data.tickers.find((x) => x.id === id);
      act(() => patch(`/tickers/${id}`, { enabled: !t.enabled }));
      break;
    }
    // Pinning is a settings edit, not a ticker edit — this button is a
    // shortcut into the same list the Settings page shows as text.
    case 'pin': {
      const t = state.data.tickers.find((x) => x.id === id);
      const current = state.data.settings.pinnedSymbols ?? [];
      const next = t.pinned
        ? current.filter((sym) => sym !== t.symbol)
        : [...current, t.symbol];
      act(() => patch('/settings', { pinnedSymbols: next }), {
        success: `${t.pinned ? 'Unpinned' : 'Pinned'} ${t.symbol}`,
      });
      break;
    }
    case 'up':
      nudge(id, -1);
      break;
    case 'down':
      nudge(id, 1);
      break;
    case 'delete': {
      const t = state.data.tickers.find((x) => x.id === id);
      if (!confirm(`Remove ${t?.symbol ?? 'this ticker'} from the watchlist?`)) return;
      clearDraft(`ticker:${id}`);
      act(() => del(`/tickers/${id}`), { success: `Removed ${t?.symbol ?? 'ticker'}` });
      break;
    }

    case 'new-portfolio':
      openPortfolio(null);
      break;
    case 'edit-portfolio':
      openPortfolio(state.data.portfolios.find((p) => p.id === id));
      break;
    case 'run-portfolio':
      runBacktest(id);
      break;
    case 'delete-portfolio': {
      const p = state.data.portfolios.find((x) => x.id === id);
      if (!confirm(`Remove the portfolio "${p?.name ?? ''}"?`)) return;
      // Its result is on screen under the list; leaving it there after the
      // portfolio it belongs to is gone reads as a bug.
      if (state.backtest?.id === id) state.backtest = null;
      act(() => del(`/portfolios/${id}`), { success: 'Portfolio removed' });
      break;
    }

    case 'new-sink':
      state.editingSinks.add('new');
      render({ force: true });
      break;
    case 'edit-sink':
      state.editingSinks.add(id);
      render({ force: true });
      break;
    case 'cancel-sink':
      state.editingSinks.delete(id);
      clearDraft(`sink:${id}`);
      render({ force: true });
      break;
    case 'toggle-sink': {
      const s = state.data.sinks.find((x) => x.id === id);
      act(() => patch(`/sinks/${id}`, { enabled: !s.enabled }));
      break;
    }
    case 'delete-sink': {
      const s = state.data.sinks.find((x) => x.id === id);
      if (!confirm(`Remove the destination "${s?.name ?? ''}"? Quotes will stop being sent there.`)) return;
      clearDraft(`sink:${id}`);
      act(() => del(`/sinks/${id}`), { success: 'Destination removed' });
      break;
    }
    case 'test-sink':
      act(async () => {
        const { result } = await post(`/sinks/${id}/test`);
        if (result.ok) {
          toast(`${result.method} succeeded (HTTP ${result.statusCode}, ${result.durationMs} ms)`, 'ok');
        } else {
          toast(result.error || 'Publish failed', 'error');
        }
      });
      break;

    default:
      break;
  }
});

/* Double-tap — or double-click — a row to open its performance sheet.
 *
 * Two clicks counted on the same row rather than a `dblclick` listener plus a
 * touch path: a tap raises a click everywhere this app runs, so one timing
 * check covers a thumb and a mouse with the same four lines. Controls are
 * excluded — double-clicking Pause means two pauses, not "show me the chart" —
 * and `touch-action: manipulation` on the row (styles.css) is what stops the
 * second tap zooming the page instead. */
const DOUBLE_TAP_MS = 450;
let lastTap = { id: null, at: 0 };

$('#view').addEventListener('click', (event) => {
  const row = event.target.closest('.quote');
  if (!row || event.target.closest('button, a, input, select, label')) return;

  const now = Date.now();
  if (lastTap.id === row.dataset.id && now - lastTap.at < DOUBLE_TAP_MS) {
    lastTap = { id: null, at: 0 };
    // A double-click has already selected the symbol underneath; leaving it
    // highlighted behind the sheet looks like something went wrong.
    window.getSelection?.()?.removeAllRanges();
    openPerformance(row.dataset.id);
    return;
  }
  lastTap = { id: row.dataset.id, at: now };
});

$('#view').addEventListener('submit', (event) => {
  const form = event.target;
  event.preventDefault();
  const values = Object.fromEntries(new FormData(form).entries());

  if ('sectorForm' in form.dataset) {
    // The subject is whatever run is on screen; only the comparison changed, so
    // nothing above the card is re-fetched.
    const run = currentRun();
    if (run?.status !== 'ready') return;
    const subject =
      runSlot() === 'fund'
        ? sectorSubject(run.data, { label: run.data.symbol, symbol: run.data.symbol })
        : sectorSubject(run.data, { label: backtestName(run.id) });
    loadSectors(subject, values.peers ?? '');
    return;
  }

  if ('fundForm' in form.dataset) {
    const symbol = (values.symbol || '').trim().toUpperCase();
    if (!symbol) return;
    // The benchmark is stashed before the hash moves, because the hashchange is
    // what triggers the lookup and it reads this back out.
    state.fund = { ...state.fund, benchmark: (values.benchmark || '').trim().toUpperCase() };
    // Asking for the fund already showing has to work — the benchmark may be
    // what changed — and a hash that doesn't move fires no hashchange.
    if (routeArg().toUpperCase() === symbol) {
      loadFund(symbol);
    } else {
      location.hash = `#/funds/${encodeURIComponent(symbol)}`;
    }
    return;
  }

  if (form.dataset.edit) {
    const id = form.dataset.edit;
    // A composite's form carries `expression`, a plain row's carries `symbol`.
    // Sending only the one that exists is what keeps a composite from being
    // reinterpreted as a symbol, and vice versa.
    const payload = { label: (values.label || '').trim() };
    if ('expression' in values) payload.expression = (values.expression || '').trim();
    else if ('symbol' in values) payload.symbol = (values.symbol || '').trim();
    act(
      async () => {
        await patch(`/tickers/${id}`, payload);
        state.editing.delete(id);
        clearDraft(`ticker:${id}`);
        state.history.delete(id);
      },
      { success: 'Ticker updated' },
    );
    return;
  }

  if (form.dataset.sinkForm) {
    const id = form.dataset.sinkForm;
    const payload = {
      name: values.name || '',
      baseUrl: values.baseUrl || '',
      key: values.key || '',
      category: values.category || '',
      format: values.format || 'minion',
      timeoutMs: Number(values.timeoutMs) || 10000,
    };
    act(
      async () => {
        if (id === 'new') await post('/sinks', payload);
        else await patch(`/sinks/${id}`, payload);
        state.editingSinks.delete(id);
        clearDraft(`sink:${id}`);
      },
      { success: id === 'new' ? 'Destination added' : 'Destination saved' },
    );
    return;
  }

  if (form.id === 'settings-form') {
    const payload = {
      refreshSeconds: Number(values.refreshSeconds),
      historyHours: Number(values.historyHours),
      publishOnRefresh: form.elements.publishOnRefresh.checked,
      logos: form.elements.logos.checked,
      // A cleared box means "go back to the quote source's own", so it has to
      // reach the server as an empty string rather than being dropped.
      logoUrlTemplate: values.logoUrlTemplate ?? '',
      // An emptied field means "pin nothing", so it has to reach the server as
      // an empty list rather than being dropped from the payload.
      pinnedSymbols: symbolList(values.pinnedSymbols),
    };
    // The key is the one field this page cannot show, so an empty box has to
    // mean "unchanged" — sending "" on every save would wipe a stored key the
    // moment anybody edited the refresh interval. Deleting it is therefore its
    // own explicit tick rather than an empty box.
    if (form.elements.forgetLogoKey?.checked) {
      payload.logoKey = '';
    } else if (values.logoKey) {
      payload.logoKey = values.logoKey;
    }
    // The quote-source fields only exist for a configurable provider. Send
    // them as strings so a cleared box reaches the server as "" — which is
    // how you ask for the default back — rather than being dropped.
    if (form.elements.quoteBaseUrl) {
      payload.quoteBaseUrl = values.quoteBaseUrl ?? '';
      payload.quoteUserAgent = values.quoteUserAgent ?? '';
      payload.quoteTimeoutSeconds = Number(values.quoteTimeoutSeconds) || 0;
    }
    act(
      async () => {
        await patch('/settings', payload);
        clearDraft('settings-form');
      },
      { success: 'Settings saved' },
    );
  }
});

/* Every keystroke in the routed view is stashed against its form, so any
 * redraw — background or not — can put it back. This is the listener that
 * makes the whole draft mechanism work; the render side only reads. */
$('#view').addEventListener('input', (event) => {
  if (isField(event.target) && event.target.form) saveDraft(event.target.form);
});

/* A redraw the poll wanted while a field was focused is owed, not dropped.
 * The timeout is because focusout fires before focus lands on whatever is
 * next — which is often another field in the same form. */
$('#view').addEventListener('focusout', () => {
  setTimeout(() => {
    if (state.renderPending && !isTyping()) render();
  }, 0);
});

// "Publish now" and "Reload" live in the routed view, so they are wired by
// delegation on the same container as everything else.
$('#view').addEventListener('click', async (event) => {
  if (event.target.id === 'publish-now') {
    act(
      async () => {
        const { results } = await post('/publish');
        const failed = results.filter((r) => !r.ok);
        if (!results.length) toast('No enabled destinations to publish to');
        else if (failed.length) toast(`${failed.length} of ${results.length} destinations failed`, 'error');
        else toast(`Published to ${results.length} ${results.length === 1 ? 'destination' : 'destinations'}`, 'ok');
      },
    );
    return;
  }

  if (event.target.id === 'refresh-view') {
    refreshView({ force: true });
    return;
  }

  // Interval preset chips write into the number field rather than saving, so
  // one tap is still followed by a deliberate Save.
  const preset = event.target.closest('[data-preset]');
  if (preset) {
    const input = $('#refreshSeconds');
    if (input) {
      input.value = preset.dataset.preset;
      saveDraft(input.form);
      for (const chip of $$('[data-preset]')) {
        chip.classList.toggle('btn--active', chip === preset);
        chip.classList.toggle('btn--outline', chip === preset);
        chip.classList.toggle('btn--ghost', chip !== preset);
      }
    }
    return;
  }

  if (event.target.id === 'test-provider') {
    const button = event.target;
    button.disabled = true;
    const previous = button.textContent;
    button.textContent = 'Testing…';
    // Hold off the poll's redraw, which would otherwise replace this button
    // mid-request and leave it looking idle while the test is still running.
    state.inlineBusy = true;
    try {
      // No symbol: the server falls back to a known-good one. This used to
      // read the add box, which only ever rendered on the Watchlist while this
      // button only renders on Settings — so it always read nothing. Now that
      // the add box lives in a dialog in the shell it would read a stale value
      // instead, which is worse than the default it never actually replaced.
      const { result } = await post('/provider/test', {});
      if (result.ok) {
        toast(
          `${result.provider}: ${result.symbol} = ${money(result.price, result.currency)} (${result.durationMs} ms)`,
          'ok',
        );
      } else {
        toast(`${result.provider} unreachable: ${result.error}`, 'error');
      }
    } catch (err) {
      toast(err.message || String(err), 'error');
    } finally {
      state.inlineBusy = false;
      button.disabled = false;
      button.textContent = previous;
      if (state.renderPending && !isTyping()) render();
    }
  }
});

/* ------------------------------------------------------------------ *
 * The add dialog
 *
 * It lives in the shell rather than in #view, so nothing here has to survive a
 * redraw: no drafts, no focus capture, no deferral. What you typed stays typed
 * because the elements are never replaced. `showModal` supplies the focus
 * trap, Escape and the inert background.
 * ------------------------------------------------------------------ */

const addDialog = $('#add-dialog');

/* The on-screen keyboard.
 *
 * A modal dialog is positioned against the *layout* viewport, and the keyboard
 * does not shrink that — so a sheet pinned to the bottom of the screen puts its
 * Add button behind the keyboard the instant you tap the field above it. The
 * *visual* viewport does shrink, so the difference between the two is how much
 * the keyboard is covering. Publishing it as a length lets the stylesheet sit
 * the sheet on top of the keyboard and take the same height out of its ceiling.
 *
 * Everything reading it degrades to 0: desktop, keyboard down, or a browser
 * without visualViewport. */
function trackKeyboard() {
  const viewport = window.visualViewport;
  if (!viewport) return;

  const sync = () => {
    const covered = Math.max(
      0,
      document.documentElement.clientHeight - viewport.height - viewport.offsetTop,
    );
    document.documentElement.style.setProperty('--keyboard-inset', `${Math.round(covered)}px`);
    // A collapsing URL bar moves the visual viewport by ~60–100px, which is not
    // a keyboard; the threshold keeps the tab bar from flickering away as you
    // scroll the watchlist.
    $('.app').classList.toggle('app--keyboard-open', covered > 150);
  };

  viewport.addEventListener('resize', sync);
  viewport.addEventListener('scroll', sync);
  sync();
}

trackKeyboard();

function openAdd() {
  if (addDialog.open) return;
  addDialog.showModal();
  $('#add-symbol').focus();
}

$('#add-fab').addEventListener('click', () => {
  if (route() === 'portfolios') openPortfolio(null);
  else openAdd();
});
$('#add-cancel').addEventListener('click', () => addDialog.close());

/* Clicking the backdrop closes it. The backdrop is not a child, so it surfaces
 * as a click on the dialog itself whose coordinates fall outside the panel. */
addDialog.addEventListener('click', (event) => {
  if (event.target !== addDialog) return;
  const box = addDialog.getBoundingClientRect();
  const outside =
    event.clientX < box.left ||
    event.clientX > box.right ||
    event.clientY < box.top ||
    event.clientY > box.bottom;
  if (outside) addDialog.close();
});

/* Closing — by button, by backdrop or by Escape — resets the form. Reopening
 * to a half-typed symbol from ten minutes ago is a puzzle, not a convenience. */
addDialog.addEventListener('close', () => {
  $('#add-form').reset();
  state.matches = null;
  paintMatches();
});

addDialog.addEventListener('submit', (event) => {
  event.preventDefault();
  const form = $('#add-form');
  const values = Object.fromEntries(new FormData(form).entries());
  const symbol = (values.symbol || '').trim();
  if (!symbol) return;

  act(
    async () => {
      await post('/tickers', { symbol, label: (values.label || '').trim() });
      // Only on success: a rejected symbol — a duplicate, a formula that
      // won't parse — leaves the dialog open on what you typed, so the fix is
      // an edit rather than a retype.
      addDialog.close();
      state.history.clear();
    },
    {
      success: `Added ${symbol.toUpperCase().replace(/\s+/g, '')}${
        looksComposite(symbol) ? ' (composite)' : ''
      }`,
    },
  );
});

addDialog.addEventListener('click', async (event) => {
  if (event.target.id === 'search-btn') {
    const query = $('#add-symbol').value.trim();
    if (!query) {
      toast('Type a company or symbol first');
      return;
    }
    if (looksComposite(query)) {
      // The provider has no idea what a ratio is; searching for one returns
      // nothing and reads as a broken search rather than a wrong button.
      toast('That is a formula — press Add. Search looks up one symbol at a time.');
      return;
    }
    state.matches = { status: 'loading', items: [] };
    paintMatches();
    try {
      const { matches, warning } = await api(`/search?q=${encodeURIComponent(query)}`);
      if (warning) toast(warning, 'error');
      state.matches = { status: 'done', items: matches ?? [] };
    } catch (err) {
      state.matches = { status: 'error', items: [], message: err.message || String(err) };
    }
    paintMatches();
    return;
  }

  const match = event.target.closest('[data-symbol]');
  if (match) {
    $('#add-symbol').value = match.dataset.symbol;
    state.matches = null;
    paintMatches();
    $('#add-symbol').focus();
  }
});

/* ------------------------------------------------------------------ *
 * The allocation editor
 *
 * In the shell, like the add dialog, and for a stronger reason: its rows are
 * added and removed as you type, and a background redraw of #view landing in
 * the middle of that would take the half-typed row with it. Out here nothing
 * ever replaces it, so there is no draft to stash and no focus to restore.
 * ------------------------------------------------------------------ */

const portfolioDialog = $('#portfolio-dialog');

/** Which portfolio the dialog is editing, or null for a new one. Kept out of
 *  the form so a save knows whether to POST or PATCH. */
let editingPortfolio = null;

function openPortfolio(portfolio) {
  editingPortfolio = portfolio ?? null;
  const form = $('#portfolio-form');
  form.reset();

  $('#portfolio-dialog-title').textContent = portfolio ? `Edit ${portfolio.name}` : 'New portfolio';
  form.elements.name.value = portfolio?.name ?? '';
  form.elements.initialAmount.value = portfolio?.initialAmount ?? 10000;
  form.elements.rebalance.value = portfolio?.rebalance ?? 'annually';
  form.elements.contribution.value = portfolio?.contribution || '';
  form.elements.contributionFrequency.value = portfolio?.contributionFrequency ?? 'none';
  form.elements.startYear.value = portfolio?.startYear || '';
  form.elements.endYear.value = portfolio?.endYear || '';
  form.elements.benchmark.value = portfolio?.benchmark ?? '';

  // One blank row to start typing into, so a new portfolio is never an empty
  // panel with a button you have to find first.
  paintAllocation(portfolio?.holdings?.length ? portfolio.holdings : [blankHolding()]);

  if (!portfolioDialog.open) portfolioDialog.showModal();
  $('#allocation-rows input')?.focus();
}

const blankHolding = () => ({ symbol: '', weight: '', replacement: '' });

/** Repaint the allocation rows from a list of holdings. Only called when the
 *  *set* of rows changes — typing in one never repaints, so nothing you are in
 *  the middle of is ever replaced. */
function paintAllocation(holdings) {
  $('#allocation-rows').innerHTML = holdings
    .map(
      (h, i) => `
      <div class="allocation-row" data-row="${i}">
        <input class="input input--mono allocation-row__symbol" name="symbol"
               value="${esc(h.symbol ?? '')}" placeholder="VTSMX" aria-label="Symbol" />
        <div class="allocation-row__weight">
          <input class="input" name="weight" type="number" min="0" max="100" step="any"
                 value="${esc(h.weight ?? '')}" placeholder="0" aria-label="Weight in percent" />
          <span aria-hidden="true">%</span>
        </div>
        <button class="btn btn--ghost allocation-row__drop" type="button" data-drop-row="${i}"
                aria-label="Remove this holding">×</button>
        <input class="input input--mono allocation-row__stand" name="replacement"
               value="${esc(h.replacement ?? '')}" placeholder="replacement (optional)"
               aria-label="Replacement for historical data" />
      </div>`,
    )
    .join('');
  paintAllocationTotal();
}

/** Read the rows back out. The DOM is the state here — there is no model to
 *  keep in step with it, because nothing else ever rewrites these fields. */
function allocationRows() {
  return $$('.allocation-row', portfolioDialog).map((row) => ({
    symbol: $('input[name="symbol"]', row).value.trim().toUpperCase(),
    weight: Number($('input[name="weight"]', row).value),
    replacement: $('input[name="replacement"]', row).value.trim().toUpperCase(),
  }));
}

/** The running total, live under the rows.
 *
 *  The server rejects an allocation that doesn't add up to 100, and finding
 *  that out on submit — after typing eight symbols — is the wrong moment. */
function paintAllocationTotal() {
  const rows = allocationRows().filter((h) => h.symbol || h.weight);
  const total = rows.reduce((sum, h) => sum + (h.weight || 0), 0);
  const rounded = Math.round(total * 100) / 100;
  const ok = Math.abs(total - 100) <= 0.05;

  const el = $('#allocation-total');
  el.textContent = rows.length === 0 ? '' : `${rounded}%${ok ? '' : ' — needs to be 100%'}`;
  el.classList.toggle('allocation-total--ok', ok && rows.length > 0);
  el.classList.toggle('allocation-total--off', !ok && rows.length > 0);
}

/** Everything the form says, in the shape both /portfolios and /backtest take. */
function portfolioPayload() {
  const form = $('#portfolio-form');
  const values = Object.fromEntries(new FormData(form).entries());
  return {
    name: (values.name || '').trim(),
    holdings: allocationRows().filter((h) => h.symbol || h.weight),
    initialAmount: Number(values.initialAmount) || 10000,
    // An empty year field means "as far as the data goes", which reaches the
    // server as 0 rather than being dropped — otherwise clearing it on an
    // existing portfolio would leave the old year in place.
    startYear: Number(values.startYear) || 0,
    endYear: Number(values.endYear) || 0,
    rebalance: values.rebalance || 'annually',
    contribution: Number(values.contribution) || 0,
    contributionFrequency: values.contributionFrequency || 'none',
    benchmark: (values.benchmark || '').trim().toUpperCase(),
  };
}

$('#allocation-add').addEventListener('click', () => {
  paintAllocation([...allocationRows(), blankHolding()]);
  // Focus the row that was just added — the reason the button was pressed.
  const rows = $$('.allocation-row input[name="symbol"]', portfolioDialog);
  rows[rows.length - 1]?.focus();
});

portfolioDialog.addEventListener('click', (event) => {
  const drop = event.target.closest('[data-drop-row]');
  if (drop) {
    const at = Number(drop.dataset.dropRow);
    const rest = allocationRows().filter((_, i) => i !== at);
    paintAllocation(rest.length ? rest : [blankHolding()]);
    return;
  }

  // Same backdrop-click close as the other dialogs: the backdrop is not a
  // child, so it arrives as a click on the dialog with coordinates outside it.
  if (event.target !== portfolioDialog) return;
  const box = portfolioDialog.getBoundingClientRect();
  if (
    event.clientX < box.left ||
    event.clientX > box.right ||
    event.clientY < box.top ||
    event.clientY > box.bottom
  ) {
    portfolioDialog.close();
  }
});

portfolioDialog.addEventListener('input', (event) => {
  if (event.target.name === 'weight' || event.target.name === 'symbol') paintAllocationTotal();
});

portfolioDialog.addEventListener('submit', (event) => {
  event.preventDefault();
  const payload = portfolioPayload();
  const id = editingPortfolio?.id;

  act(
    async () => {
      const body = id ? await patch(`/portfolios/${id}`, payload) : await post('/portfolios', payload);
      // Only on success: a rejected allocation leaves the dialog open on what
      // was typed, so the fix is an edit rather than a retype.
      portfolioDialog.close();
      // Saving is nearly always followed by wanting to see it, and the result
      // is the reason the portfolio exists.
      runBacktest(body.portfolio.id);
    },
    { success: id ? 'Portfolio saved' : 'Portfolio added' },
  );
});

/* "Run without saving" — the answer to "would this even run?", which is worth
 * having before committing a portfolio to the database. */
$('#portfolio-run').addEventListener('click', () => {
  portfolioDialog.close();
  runBacktest('draft', portfolioPayload());
});

$('#portfolio-cancel').addEventListener('click', () => portfolioDialog.close());

/** Run a backtest and paint it under the list. `spec` is given only for the
 *  unsaved case; a saved portfolio is run by ID so the server reads the stored
 *  allocation rather than trusting a copy of it. */
async function runBacktest(id, spec) {
  if (route() !== 'portfolios') location.hash = '#/portfolios';
  // The period and the sort survive a run, for the same reason the performance
  // sheet's range survives an opening: comparing two portfolios over three
  // years should not mean picking 3Y twice.
  const { period, sort } = state.backtest ?? {};
  state.backtest = { id, period, sort, status: 'loading' };
  render({ force: true });

  try {
    const body = spec
      ? await post('/backtest', spec)
      : await post(`/portfolios/${id}/backtest`, undefined);
    // The page may have moved on — another portfolio run, or this one deleted —
    // while the request was in flight.
    if (state.backtest?.id !== id) return;
    state.backtest = { ...state.backtest, status: 'ready', data: body.backtest };
    // After the run rather than with it: the sector card is a second fan-out
    // upstream, against the source's crumbed endpoint, and the chart above it
    // must not wait on that.
    loadSectors(sectorSubject(body.backtest, { label: backtestName(id) }));
  } catch (err) {
    if (state.backtest?.id !== id) return;
    state.backtest = { ...state.backtest, status: 'error', error: err.message || String(err) };
  }
  render({ force: true });
}

/* ------------------------------------------------------------------ *
 * The performance sheet
 *
 * The chart and the returns table for one row, from /api/tickers/{id}/
 * performance. The series comes from the quote provider rather than from the
 * stored history the sparklines use: that table is pruned to a window measured
 * in hours, so it can say what a symbol did today and nothing longer.
 *
 * Like the add dialog it lives in the shell, so the ten-second redraw cannot
 * replace the chart under a finger that is scrubbing it.
 * ------------------------------------------------------------------ */

const perfDialog = $('#perf-dialog');

/** Spans the chart can be narrowed to. The server sends the whole series; these
 *  only decide how much of it is drawn, so switching is instant. `days` is null
 *  where the start is computed rather than counted — YTD from the turn of the
 *  year, All from whatever the source's first close is. */
const PERF_SPANS = [
  { key: '1m', label: '1M', days: 31 },
  { key: '3m', label: '3M', days: 92 },
  { key: '6m', label: '6M', days: 183 },
  { key: 'ytd', label: 'YTD', days: null },
  { key: '1y', label: '1Y', days: 366 },
  { key: '5y', label: '5Y', days: 1827 },
  { key: '10y', label: '10Y', days: 3653 },
  { key: 'all', label: 'All', days: null },
];

/** Chart geometry from the last paint, so the scrub handler can turn a pointer
 *  position back into a point without re-deriving the scales. */
let perfGeom = null;

/** Are these two values the same number, allowing for the residue of the
 *  arithmetic that produced them? A composite's legs moving together give a
 *  ratio that differs in its last bits and nowhere else, and calling that a
 *  fall — a red −0.00%, a line that lurches — is worse than calling it flat. */
function isFlat(a, b) {
  return Math.abs(a - b) <= Math.max(Math.abs(a), Math.abs(b)) * 1e-9;
}

/** A value formatter with enough decimals to tell this range's ends apart.
 *
 *  The row's own formatter is right for a price and wrong for an axis: a ratio
 *  that spent three months between 1.4999995 and 1.5000005 gets three identical
 *  labels from it, which makes a chart of a rounding error look exactly like a
 *  chart of a rally. Where the usual formatting already distinguishes them —
 *  which is nearly always — it is used unchanged. */
function axisFormat(min, max, format) {
  if (format(min) !== format(max)) return format;
  const span = Math.abs(max - min);
  const digits = Math.min(12, Math.max(2, Math.ceil(-Math.log10(span || 1)) + 1));
  return (v) =>
    v.toLocaleString(undefined, { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

async function openPerformance(id) {
  const ticker = state.data?.tickers.find((t) => t.id === id);
  if (!ticker) return;

  // The range is deliberately kept between openings: someone comparing two
  // rows over three months should not have to pick 3M twice.
  state.perf = { id, ticker, status: 'loading', span: state.perf?.span ?? '1y' };
  $('#perf-title').textContent = `${ticker.symbol}${ticker.label ? ` · ${ticker.label}` : ''}`;
  if (!perfDialog.open) perfDialog.showModal();
  paintPerf();

  try {
    const { performance } = await api(`/tickers/${id}/performance`);
    // The sheet may have been closed, or pointed at another row, while the
    // request was in flight.
    if (state.perf?.id !== id) return;
    state.perf = { ...state.perf, status: 'ready', data: performance };
  } catch (err) {
    if (state.perf?.id !== id) return;
    state.perf = { ...state.perf, status: 'error', error: err.message || String(err) };
  }
  paintPerf();
}

/** The first date the selected range includes. Empty means "everything". */
function perfCutoff(key) {
  if (key === 'ytd') return `${new Date().getUTCFullYear() - 1}-12-31`;
  const days = PERF_SPANS.find((r) => r.key === key)?.days;
  if (!days) return '';
  return new Date(Date.now() - days * 86_400_000).toISOString().slice(0, 10);
}

/** The points inside the selected range, starting one point *before* it. That
 *  first point is the close the range's own change is measured from — the same
 *  baseline the returns table uses, so the two never disagree. */
function perfVisible(points, key) {
  const cutoff = perfCutoff(key);
  if (!cutoff) return points;
  const first = points.findIndex((p) => p.date > cutoff);
  if (first < 0) return points.slice(-2);
  return points.slice(Math.max(0, first - 1));
}

function paintPerf() {
  const perf = state.perf;
  const body = $('#perf-body');
  perfGeom = null;
  // No row selected: the sheet is closed and only its remembered range is left.
  if (!perf?.id) {
    body.innerHTML = '';
    return;
  }

  if (perf.status === 'loading') {
    body.innerHTML = `<div class="empty"><strong>Reading ${esc(perf.ticker.symbol)}’s history</strong>Every daily close the quote source has.</div>`;
    return;
  }
  if (perf.status === 'error') {
    body.innerHTML = `<div class="empty"><strong>No history for ${esc(perf.ticker.symbol)}</strong>${esc(perf.error)}</div>`;
    return;
  }

  const points = perf.data.points ?? [];
  const composite = Boolean(perf.ticker.expression);
  const format = composite ? ratio : (v) => money(v);

  if (points.length < 2) {
    body.innerHTML = `<div class="empty"><strong>Nothing to chart</strong>The quote source returned no daily history for ${esc(perf.ticker.symbol)}.</div>`;
    return;
  }

  const visible = perfVisible(points, perf.span);
  body.innerHTML = `
    ${perfHeader(perf, visible, format, composite)}
    <div class="presets perf-spans">
      ${PERF_SPANS.map(
        (r) =>
          `<button class="btn btn--sm ${
            r.key === perf.span ? 'btn--outline btn--active' : 'btn--ghost'
          }" type="button" data-perf-span="${esc(r.key)}">${esc(r.label)}</button>`,
      ).join('')}
    </div>
    ${perfChart(visible, format)}
    ${
      // A composite gets highs and lows where a symbol gets returns. There is
      // no capital in a ratio to have returned anything — "VTI/GLD made 8%"
      // invites a reader to treat a ratio as a holding — but "it is near the
      // top of its five-year range" says something true about the same number.
      composite
        ? `<h3 class="card__subtitle">Range</h3>${perfRanges(perf.data.ranges ?? [], format)}`
        : `<h3 class="card__subtitle">Returns</h3>${perfReturns(perf.data.returns ?? [], format)}`
    }
    <p class="field__hint perf-note">
      ${
        composite
          ? 'Recomputed from the formula on every day all of its legs traded. A window is only reported once the series covers all of it.'
          : 'Daily closes, adjusted for splits and dividends where the source reports them, measured from the last close on or before each period’s start.'
      }
      All time is as far back as the quote source goes; longer spans are thinned
      to weekly and then monthly closes to draw.
    </p>
  `;
  wirePerfChart();
}

/** Current value, and what the visible range did — the two numbers the sheet
 *  exists to answer, above the chart that explains them. */
function perfHeader(perf, visible, format, composite) {
  const first = visible[0];
  const last = visible[visible.length - 1];
  const change = isFlat(first.value, last.value) ? 0 : last.value - first.value;
  // A composite's move is shown as a move, not as a percentage. The percentage
  // of a ratio is a real quantity, but it reads as a return, and a ratio has
  // nothing invested in it to return anything.
  const pct = composite || !(first.value > 0) ? null : (change / first.value) * 100;
  const dir = change > 0 ? 'up' : change < 0 ? 'down' : 'flat';
  const currency = composite ? '' : perf.ticker.quote?.currency ?? '';

  return `
    <div class="perf-head">
      <div>
        <div class="perf-head__value">${esc(format(last.value))}${
          currency && currency !== 'USD' ? ` <span class="perf-head__ccy">${esc(currency)}</span>` : ''
        }</div>
        <div class="field__hint">${esc(last.date)}${
          composite ? ` · <span class="quote__formula">${esc(perf.ticker.expression)}</span>` : ''
        }</div>
      </div>
      <div class="perf-head__delta">
        <div class="perf-change perf-change--${dir}">${esc(
          // The move is shown to the same precision as the value it moved, so
          // the two halves of the header never disagree about how exact they
          // are — the watchlist row does the same.
          pct === null
            ? signed(change, composite ? ratioDigits(last.value) : 2)
            : `${signed(pct, 2)}%`,
        )}</div>
        <div class="field__hint">since ${esc(first.date)}</div>
      </div>
    </div>
    <div class="perf-readout" id="perf-readout" aria-live="off"></div>
  `;
}

/** The line chart, as a self-contained SVG string.
 *
 *  The aspect ratio is pinned in CSS to the viewBox's, which is what lets the
 *  scrub handler map a pointer's x straight back to a point index: with
 *  letterboxing the two would drift apart at every width but one. */
function perfChart(points, format) {
  const W = 640;
  const H = 240;

  const values = points.map((p) => p.value);
  const lo = Math.min(...values);
  const hi = Math.max(...values);
  // A series that is flat — or flat to within floating-point residue, which is
  // what a ratio of two legs that move together comes out as — has no span to
  // scale by. Scaling by it anyway magnifies the last bits of a double into a
  // dramatic zigzag; giving it a band instead draws the straight line it is.
  const spread = isFlat(lo, hi) ? 0 : hi - lo;
  const margin = spread * 0.08 || Math.abs(hi) * 0.02 || 1;
  const min = lo - margin;
  const max = hi + margin;

  // The axis decides its own precision and, from that, how much room it needs:
  // "68,000.00" and "1.5000005" are both real labels here, and one hard-coded
  // gutter would clip the first and mislabel the second.
  const tick = axisFormat(min, max, format);
  const ticks = [max, (max + min) / 2, min].map(tick);
  const widest = Math.max(...ticks.map((t) => t.length));
  const pad = { l: Math.min(130, 14 + widest * 6.7), r: 12, t: 12, b: 24 };

  // Points are placed by *date*, not by index. The series has mixed resolution
  // — every session for the recent years, then one a week and one a month —
  // so index spacing would hand the last two years a third of the width and
  // squeeze thirty into the rest. It also closes the smaller version of the
  // same lie: weekends are gaps in a daily series.
  const days = points.map((p) => Date.parse(`${p.date}T00:00:00Z`) / 86_400_000);
  const from = days[0];
  const total = days[days.length - 1] - from || 1;

  const x = (i) => pad.l + ((days[i] - from) / total) * (W - pad.l - pad.r);
  const y = (v) => pad.t + (1 - (v - min) / (max - min)) * (H - pad.t - pad.b);
  perfGeom = { points, days, from, total, x, y, pad, width: W, format: tick };

  const line = points
    .map((p, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)} ${y(p.value).toFixed(1)}`)
    .join(' ');
  const floor = (H - pad.b).toFixed(1);
  const area = `${line} L${x(points.length - 1).toFixed(1)} ${floor} L${x(0).toFixed(1)} ${floor} Z`;
  const stroke = `var(--${values[values.length - 1] >= values[0] ? 'up' : 'down'})`;

  const grid = [max, (max + min) / 2, min]
    .map(
      (v, i) => `<line x1="${pad.l}" x2="${W - pad.r}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}"
                    stroke="var(--border)" stroke-width="1" />
               <text x="${pad.l - 7}" y="${(y(v) + 3.5).toFixed(1)}" text-anchor="end"
                     class="perf-chart__tick">${esc(ticks[i])}</text>`,
    )
    .join('');

  return `
    <svg class="perf-chart" id="perf-chart" viewBox="0 0 ${W} ${H}" role="img"
         aria-label="Daily closes from ${esc(points[0].date)} to ${esc(points[points.length - 1].date)}">
      ${grid}
      <path d="${area}" fill="${stroke}" opacity="0.12" />
      <path d="${line}" fill="none" stroke="${stroke}" stroke-width="1.8"
            stroke-linejoin="round" stroke-linecap="round" />
      <g id="perf-cursor" opacity="0">
        <line y1="${pad.t}" y2="${floor}" stroke="var(--muted)" stroke-width="1" stroke-dasharray="3 3" />
        <circle r="3.6" fill="${stroke}" stroke="var(--surface)" stroke-width="1.5" />
      </g>
      <text x="${pad.l}" y="${H - 6}" class="perf-chart__tick">${esc(points[0].date)}</text>
      <text x="${W - pad.r}" y="${H - 6}" text-anchor="end" class="perf-chart__tick">${esc(
        points[points.length - 1].date,
      )}</text>
    </svg>`;
}

/** Scrubbing. Pointer events cover mouse and touch in one handler; the chart's
 *  `touch-action: pan-y` is what lets a sideways drag read the line while an
 *  up-and-down drag still scrolls the sheet. */
function wirePerfChart() {
  const svg = $('#perf-chart');
  const cursor = $('#perf-cursor');
  const readout = $('#perf-readout');
  if (!svg || !cursor || !perfGeom) return;

  /** The point nearest the pointer — by date, since that is what the x axis
   *  measures. Binary search rather than a scan: this runs on every move, and
   *  an all-time series is over a thousand points. */
  const at = (clientX) => {
    const box = svg.getBoundingClientRect();
    const { days, from, total, pad, width } = perfGeom;
    const sx = ((clientX - box.left) / box.width) * width;
    const target = from + ((sx - pad.l) / (width - pad.l - pad.r)) * total;

    let lo = 0;
    let hi = days.length - 1;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (days[mid] < target) lo = mid + 1;
      else hi = mid;
    }
    // lo is the first point at or after the target; its predecessor may be closer.
    if (lo > 0 && target - days[lo - 1] < days[lo] - target) return lo - 1;
    return lo;
  };

  const rule = $('line', cursor);
  const dot = $('circle', cursor);

  const show = (event) => {
    const { points, x, y, format } = perfGeom;
    const i = at(event.clientX);
    const point = points[i];
    const px = x(i).toFixed(1);
    rule.setAttribute('x1', px);
    rule.setAttribute('x2', px);
    dot.setAttribute('cx', px);
    dot.setAttribute('cy', y(point.value).toFixed(1));
    cursor.setAttribute('opacity', '1');

    const base = points[0].value;
    const change = isFlat(base, point.value) ? 0 : point.value - base;
    const move = base > 0 ? `${signed((change / base) * 100, 2)}%` : signed(change, 2);
    readout.textContent = `${point.date} · ${format(point.value)} · ${move} over the range`;
  };

  const hide = () => {
    cursor.setAttribute('opacity', '0');
    readout.textContent = '';
  };

  svg.addEventListener('pointermove', show);
  svg.addEventListener('pointerdown', show);
  svg.addEventListener('pointerleave', hide);
  svg.addEventListener('pointercancel', hide);
}

/** The high/low table — what a composite gets instead of returns.
 *
 *  "Now" is where the latest value sits between the two, which is the column
 *  that turns two numbers into a reading: a ratio at 4% of its five-year range
 *  is saying something a low and a high on their own leave to the reader. */
function perfRanges(ranges, format) {
  if (!ranges.length) return '<div class="empty">No ranges could be computed.</div>';
  return `<div class="table-scroll"><table class="table">
      <thead><tr><th>Period</th><th>Low</th><th>High</th><th>Now</th></tr></thead>
      <tbody>${ranges.map((r) => perfRangeRow(r, format)).join('')}</tbody>
    </table></div>`;
}

function perfRangeRow(r, format) {
  if (!r.available) {
    return `<tr><th>${esc(r.label)}</th>
      <td class="field__hint" colspan="3">not enough history</td></tr>`;
  }
  const position =
    r.position === null || r.position === undefined
      ? '<span class="field__hint">flat</span>'
      : `${Math.round(r.position)}%`;

  return `<tr>
    <th>${esc(r.label)}</th>
    <td><span class="perf-change perf-change--down">${esc(format(r.low))}</span>
        <span class="perf-when">${esc(r.lowDate)}</span></td>
    <td><span class="perf-change perf-change--up">${esc(format(r.high))}</span>
        <span class="perf-when">${esc(r.highDate)}</span></td>
    <td class="field__hint">${esc(position)}</td>
  </tr>`;
}

function perfReturns(returns, format) {
  if (!returns.length) return '<div class="empty">No returns could be computed.</div>';
  return `<div class="table-scroll"><table class="table">
      <thead><tr><th>Period</th><th>Return</th><th>From</th></tr></thead>
      <tbody>${returns.map((r) => perfReturnRow(r, format)).join('')}</tbody>
    </table></div>`;
}

function perfReturnRow(r, format) {
  if (!r.available) {
    return `<tr><th>${esc(r.label)}</th>
      <td class="field__hint" colspan="2">not enough history</td></tr>`;
  }
  const flat = isFlat(r.fromValue, r.toValue);
  const dir = flat || r.change === 0 ? 'flat' : r.change > 0 ? 'up' : 'down';
  // A composite whose formula is a difference can have a baseline at or below
  // zero, and the server sends no percentage for one; the absolute move is
  // still worth showing.
  const headline =
    r.changePercent === null || r.changePercent === undefined
      ? signed(flat ? 0 : r.change, 2)
      : `${signed(flat ? 0 : r.changePercent, 2)}%`;
  const annual =
    r.annualizedPercent === null || r.annualizedPercent === undefined
      ? ''
      : `<span class="perf-annual">${esc(signed(r.annualizedPercent, 1))}%/yr</span>`;

  return `<tr>
    <th>${esc(r.label)}</th>
    <td><span class="perf-change perf-change--${dir}">${esc(headline)}</span>${annual}</td>
    <td class="field__hint wrap">${esc(r.from)} · ${esc(format(r.fromValue))}</td>
  </tr>`;
}

$('#perf-close').addEventListener('click', () => perfDialog.close());

/* Same backdrop-click close as the add dialog: the backdrop is not a child, so
 * it arrives as a click on the dialog whose coordinates fall outside it. */
perfDialog.addEventListener('click', (event) => {
  const span = event.target.closest('[data-perf-span]');
  if (span) {
    state.perf = { ...state.perf, span: span.dataset.perfSpan };
    paintPerf();
    return;
  }
  if (event.target !== perfDialog) return;
  const box = perfDialog.getBoundingClientRect();
  if (
    event.clientX < box.left ||
    event.clientX > box.right ||
    event.clientY < box.top ||
    event.clientY > box.bottom
  ) {
    perfDialog.close();
  }
});

/* Closing drops the row but keeps the range, so reopening on another symbol
 * lands on the same period. */
perfDialog.addEventListener('close', () => {
  state.perf = { span: state.perf?.span ?? '1y' };
  paintPerf();
});

$('#refresh-now').addEventListener('click', () => {
  act(
    async () => {
      state.history.clear();
      const { run } = await post('/refresh');
      toast(`Refreshed ${run.okCount} of ${run.okCount + run.errorCount} symbols`, run.errorCount ? '' : 'ok');
    },
  );
});

/* Drag-and-drop reordering (pointer devices; the ↑↓ buttons cover touch). */
let dragID = null;

$('#view').addEventListener('dragstart', (event) => {
  const row = event.target.closest('.quote');
  if (!row) return;
  dragID = row.dataset.id;
  row.classList.add('quote--dragging');
  event.dataTransfer.effectAllowed = 'move';
  // Firefox refuses to start a drag without payload.
  event.dataTransfer.setData('text/plain', dragID);
});

$('#view').addEventListener('dragover', (event) => {
  const row = event.target.closest('.quote');
  if (!row || !dragID || row.dataset.id === dragID) return;
  event.preventDefault();
  row.classList.add('quote--drop-target');
});

$('#view').addEventListener('dragleave', (event) => {
  event.target.closest('.quote')?.classList.remove('quote--drop-target');
});

$('#view').addEventListener('drop', (event) => {
  const row = event.target.closest('.quote');
  if (!row || !dragID) return;
  event.preventDefault();
  row.classList.remove('quote--drop-target');

  const ids = orderedIDs();
  const from = ids.indexOf(dragID);
  const to = ids.indexOf(row.dataset.id);
  if (from < 0 || to < 0 || from === to) return;
  ids.splice(to, 0, ids.splice(from, 1)[0]);
  act(() => post('/tickers/reorder', { ids }));
});

$('#view').addEventListener('dragend', () => {
  dragID = null;
  for (const el of $$('.quote--dragging, .quote--drop-target')) {
    el.classList.remove('quote--dragging', 'quote--drop-target');
  }
});

/* The developer badge, full screen for a beat. */
$('#dev-badge').addEventListener('click', () => {
  if ($('.dev-flash')) return;

  const overlay = document.createElement('div');
  overlay.className = 'dev-flash';
  overlay.setAttribute('role', 'presentation');
  overlay.innerHTML = `
    <div class="dev-flash__lockup">
      <img class="dev-flash__logo" src="/dev-badge-full.png" alt="Built by CM Hegday — 0x434d" />
      <span class="dev-flash__handle">github.com/chinmay28</span>
    </div>`;

  // Nobody should be stuck waiting out an animation: a tap anywhere, or
  // Escape, ends it early.
  const dismiss = () => {
    overlay.remove();
    window.removeEventListener('keydown', onKey);
    clearTimeout(timer);
  };
  const onKey = (e) => {
    if (e.key === 'Escape') dismiss();
  };
  const timer = setTimeout(dismiss, DEV_FLASH_MS);

  overlay.addEventListener('click', dismiss);
  window.addEventListener('keydown', onKey);
  document.body.append(overlay);
});

/* ------------------------------------------------------------------ *
 * Boot
 * ------------------------------------------------------------------ */

window.addEventListener('hashchange', () => {
  state.pendingScroll = movedSection();
  // A route change wants fresh data for the view it lands on; Activity in
  // particular has its own collection. It redraws unconditionally — a tab that
  // didn't move because a field still had focus would be a worse bug than the
  // one the deferral exists to fix. Drafts survive the trip, so a half-filled
  // form is still there when you come back to it.
  refreshView({ force: true });
});

// A moved hash typed straight into the address bar fires no hashchange, so the
// first paint has to pick the section up too.
state.pendingScroll = movedSection();
refreshView();
setInterval(loadState, POLL_MS);
