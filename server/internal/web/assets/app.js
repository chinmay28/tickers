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
    throw new Error(body?.error || `HTTP ${response.status}`);
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

const state = {
  data: null,
  connected: true,
  /** Row IDs currently showing their inline edit form. */
  editing: new Set(),
  /** Sink IDs currently showing their edit form, plus 'new' for the add form. */
  editingSinks: new Set(),
  /** Sparkline points by ticker ID, fetched lazily per row. */
  history: new Map(),
  runs: [],
  busy: false,
};

async function loadState() {
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
  render();
}

/** Reload everything the current view needs, then redraw. */
async function refreshView() {
  if (route() === 'activity') {
    try {
      state.runs = (await api('/runs?limit=40'))?.runs ?? [];
    } catch {
      state.runs = [];
    }
  }
  await loadState();
}

/** Wrap an action so the UI can't fire two overlapping mutations, and so
 *  every failure surfaces as a toast instead of a silent console entry. */
async function act(fn, { success } = {}) {
  if (state.busy) return;
  state.busy = true;
  try {
    await fn();
    if (success) toast(success, 'ok');
    await refreshView();
  } catch (err) {
    toast(err.message || String(err), 'error');
  } finally {
    state.busy = false;
  }
}

/* ------------------------------------------------------------------ *
 * Router
 * ------------------------------------------------------------------ */

const ROUTES = ['watchlist', 'publishing', 'activity', 'settings'];

function route() {
  const hash = location.hash.replace(/^#\/?/, '').split('?')[0];
  return ROUTES.includes(hash) ? hash : 'watchlist';
}

function syncNav() {
  const current = route();
  for (const link of $$('[data-route]')) {
    link.classList.toggle('is-active', link.dataset.route === current);
  }
}

/* ------------------------------------------------------------------ *
 * Render
 * ------------------------------------------------------------------ */

function render() {
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

  switch (route()) {
    case 'publishing':
      view.innerHTML = renderPublishing(data);
      break;
    case 'activity':
      view.innerHTML = renderActivity(data);
      break;
    case 'settings':
      view.innerHTML = renderSettings(data);
      break;
    default:
      view.innerHTML = renderWatchlist(data);
      drawSparklines();
  }

  renderFooter(data);
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
  const placeholders = data.tickers.filter((t) => t.placeholder).length;

  return `
    <div class="page-head">
      <div>
        <h1>Watchlist</h1>
        <p>
          Every enabled symbol is fetched on the schedule and published to your
          destinations. ${
            placeholders
              ? `<strong>${placeholders}</strong> ${
                  placeholders === 1 ? 'symbol is' : 'symbols are'
                } still the shipped placeholder — replace them with your own.`
              : ''
          }
        </p>
      </div>
    </div>

    <div class="card">
      <div class="card__head">
        <h2 class="card__title">Add a ticker</h2>
      </div>
      <div class="card__body">
        <form class="add-row" id="add-form" autocomplete="off">
          <div class="field">
            <label class="field__label" for="add-symbol">Symbol</label>
            <input class="input input--mono" id="add-symbol" name="symbol"
                   placeholder="AAPL, BTC-USD, VWRL.L" required />
          </div>
          <div class="field">
            <label class="field__label" for="add-label">Label <span style="text-transform:none">(optional)</span></label>
            <input class="input" id="add-label" name="label" placeholder="Rainy-day fund" />
          </div>
          <button class="btn btn--primary" type="submit">Add</button>
          <button class="btn btn--outline" type="button" id="search-btn">Search by name</button>
        </form>
        <div class="matches" id="matches"></div>
      </div>
    </div>

    <div class="card" style="margin-top:1rem">
      <div class="card__head">
        <h2 class="card__title">${data.tickers.length} ${
          data.tickers.length === 1 ? 'symbol' : 'symbols'
        }</h2>
        <span class="field__hint">Drag to reorder — the order is the payload's order.</span>
      </div>
      <div class="card__body">
        ${
          data.tickers.length === 0
            ? `<div class="empty"><strong>Nothing on the watchlist</strong>Add a symbol above and it will be priced immediately.</div>`
            : `<div class="quotes" id="quotes">${data.tickers.map(renderQuote).join('')}</div>`
        }
      </div>
    </div>
  `;
}

function renderQuote(t) {
  const q = t.quote;
  const editing = state.editing.has(t.id);
  const failed = q && q.status === 'error';

  let priceHTML;
  if (q && q.status === 'ok' && q.price !== null) {
    const dir = t.change === null || t.change === undefined ? 'flat' : t.change > 0 ? 'up' : t.change < 0 ? 'down' : 'flat';
    const change =
      t.change === null || t.change === undefined
        ? ''
        : `${signed(t.change)} (${signed(t.changePercent, 2)}%)`;
    priceHTML = `
      <span class="quote__value">${esc(money(q.price, q.currency))}</span>
      ${change ? `<span class="quote__change quote__change--${dir}">${esc(change)}</span>` : ''}
      <span class="quote__change quote__change--flat">${esc(ago(q.fetchedAt))}</span>`;
  } else {
    priceHTML = `<span class="quote__value quote__value--na">N/A</span>
      ${q ? `<span class="quote__change quote__change--flat">${esc(ago(q.fetchedAt))}</span>` : ''}`;
  }

  return `
    <article class="quote${t.enabled ? '' : ' quote--disabled'}" data-id="${esc(t.id)}" draggable="true">
      <button class="quote__handle" type="button" aria-label="Reorder ${esc(t.symbol)}" tabindex="-1">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
          <path d="M9 6h.01M9 12h.01M9 18h.01M15 6h.01M15 12h.01M15 18h.01"/>
        </svg>
      </button>

      <div class="quote__head">
        <span class="quote__symbol">${esc(t.symbol)}</span>
        ${t.label ? `<span class="quote__name">${esc(t.label)}</span>` : ''}
        ${!t.label && q?.shortName ? `<span class="quote__name">${esc(q.shortName)}</span>` : ''}
        ${t.placeholder ? `<span class="chip chip--placeholder">placeholder</span>` : ''}
        ${t.enabled ? '' : `<span class="chip chip--off">paused</span>`}
      </div>

      <svg class="quote__spark" data-spark="${esc(t.id)}" viewBox="0 0 180 26" preserveAspectRatio="none" aria-hidden="true"></svg>

      <div class="quote__price">${priceHTML}</div>

      ${failed ? `<div class="quote__error">${esc(q.error)}</div>` : ''}

      <div class="quote__actions">
        <button class="btn btn--sm btn--outline" data-action="edit" data-id="${esc(t.id)}">
          ${t.placeholder ? 'Replace' : 'Edit'}
        </button>
        <button class="btn btn--sm btn--ghost" data-action="toggle" data-id="${esc(t.id)}">
          ${t.enabled ? 'Pause' : 'Resume'}
        </button>
        <button class="btn btn--sm btn--ghost" data-action="up" data-id="${esc(t.id)}" aria-label="Move up">↑</button>
        <button class="btn btn--sm btn--ghost" data-action="down" data-id="${esc(t.id)}" aria-label="Move down">↓</button>
        <button class="btn btn--sm btn--danger" data-action="delete" data-id="${esc(t.id)}">Remove</button>
      </div>

      ${
        editing
          ? `<form class="quote__edit" data-edit="${esc(t.id)}" autocomplete="off">
              <div class="field">
                <label class="field__label">Symbol</label>
                <input class="input input--mono" name="symbol" value="${esc(t.symbol)}" required />
              </div>
              <div class="field">
                <label class="field__label">Label</label>
                <input class="input" name="label" value="${esc(t.label)}" placeholder="optional" />
              </div>
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

/* --------------------------- Publishing ---------------------------- */

function renderPublishing(data) {
  const preview = JSON.stringify(data.preview, null, 2);

  return `
    <div class="page-head">
      <div>
        <h1>Publishing</h1>
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
        <h2 class="card__title">Destinations</h2>
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
        <h2 class="card__title">Payload preview</h2>
        <span class="field__hint">format: minion (legacy) · exactly what a destination receives right now</span>
      </div>
      <div class="card__body">
        <pre class="code">${esc(preview)}</pre>
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
    <form class="card" style="margin-top:0.7rem" data-sink-form="${esc(id)}" autocomplete="off">
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

/* ---------------------------- Activity ----------------------------- */

function renderActivity(data) {
  const runs = state.runs;
  const engine = data.engine;

  return `
    <div class="page-head">
      <div>
        <h1>Activity</h1>
        <p>The last ${runs.length} refresh cycles, newest first. Every cycle is recorded whether it succeeded or not.</p>
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
            : `<div class="table-scroll"><table class="table">
                <thead><tr>
                  <th>When</th><th>Trigger</th><th>Quotes</th><th>Published</th><th>Took</th><th>Detail</th>
                </tr></thead>
                <tbody>${runs.map(runRow).join('')}</tbody>
              </table></div>`
        }
      </div>
    </div>
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

  return `
    <tr>
      <td title="${esc(run.startedAt)}">${esc(ago(run.finishedAt))}</td>
      <td>${esc(run.trigger)}</td>
      <td>${run.okCount} ok${run.errorCount ? ` · <span style="color:var(--down)">${run.errorCount} failed</span>` : ''}</td>
      <td>${publishes.length - failed.length}/${publishes.length}</td>
      <td>${took} ms</td>
      <td class="wrap">${detail}</td>
    </tr>
  `;
}

/* ---------------------------- Settings ----------------------------- */

function renderSettings(data) {
  const s = data.settings;
  const min = data.meta.minRefreshSeconds ?? 30;

  return `
    <div class="page-head">
      <div>
        <h1>Settings</h1>
        <p>Changes take effect immediately — the refresh loop re-reads these every cycle, so nothing here needs a restart.</p>
      </div>
    </div>

    <form class="card" id="settings-form">
      <div class="card__head"><h2 class="card__title">Refresh loop</h2></div>
      <div class="card__body">
        <div class="form-grid">
          <div class="field">
            <label class="field__label" for="refreshSeconds">Interval (seconds)</label>
            <input class="input" id="refreshSeconds" name="refreshSeconds" type="number"
                   min="${min}" step="10" value="${esc(s.refreshSeconds)}" />
            <span class="field__hint">Minimum ${min}s. Currently every ${esc(duration(s.refreshSeconds))}.</span>
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
        </div>
        <div class="form-actions">
          <button class="btn btn--primary" type="submit">Save settings</button>
        </div>
      </div>
    </form>

    <div class="card">
      <div class="card__head"><h2 class="card__title">About</h2></div>
      <div class="card__body">
        <div class="table-scroll">
          <table class="table">
            <tbody>
              <tr><th>Version</th><td><code>${esc(data.version)}</code></td></tr>
              <tr><th>Quote provider</th><td>${esc(data.engine.provider)}</td></tr>
              <tr><th>Seeded placeholders</th><td class="wrap"><code>${esc((data.meta.seedSymbols ?? []).join(', '))}</code></td></tr>
              <tr><th>Upgrades</th><td class="wrap">Re-run <code>scripts/quickstart.sh</code>. It snapshots the database, swaps code in, and rolls back if the new version fails its health check.</td></tr>
            </tbody>
          </table>
        </div>
      </div>
    </div>
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

$('#view').addEventListener('click', (event) => {
  const button = event.target.closest('[data-action]');
  if (!button) return;
  const { action, id } = button.dataset;

  switch (action) {
    case 'edit':
      state.editing.add(id);
      render();
      break;
    case 'cancel-edit':
      state.editing.delete(id);
      render();
      break;
    case 'toggle': {
      const t = state.data.tickers.find((x) => x.id === id);
      act(() => patch(`/tickers/${id}`, { enabled: !t.enabled }));
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
      act(() => del(`/tickers/${id}`), { success: `Removed ${t?.symbol ?? 'ticker'}` });
      break;
    }

    case 'new-sink':
      state.editingSinks.add('new');
      render();
      break;
    case 'edit-sink':
      state.editingSinks.add(id);
      render();
      break;
    case 'cancel-sink':
      state.editingSinks.delete(id);
      render();
      break;
    case 'toggle-sink': {
      const s = state.data.sinks.find((x) => x.id === id);
      act(() => patch(`/sinks/${id}`, { enabled: !s.enabled }));
      break;
    }
    case 'delete-sink': {
      const s = state.data.sinks.find((x) => x.id === id);
      if (!confirm(`Remove the destination "${s?.name ?? ''}"? Quotes will stop being sent there.`)) return;
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

$('#view').addEventListener('submit', (event) => {
  const form = event.target;
  event.preventDefault();
  const values = Object.fromEntries(new FormData(form).entries());

  if (form.id === 'add-form') {
    const symbol = (values.symbol || '').trim();
    if (!symbol) return;
    act(
      async () => {
        await post('/tickers', { symbol, label: (values.label || '').trim() });
        form.reset();
        $('#matches').innerHTML = '';
        state.history.clear();
      },
      { success: `Added ${symbol.toUpperCase()}` },
    );
    return;
  }

  if (form.dataset.edit) {
    const id = form.dataset.edit;
    act(
      async () => {
        await patch(`/tickers/${id}`, {
          symbol: (values.symbol || '').trim(),
          label: (values.label || '').trim(),
        });
        state.editing.delete(id);
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
      },
      { success: id === 'new' ? 'Destination added' : 'Destination saved' },
    );
    return;
  }

  if (form.id === 'settings-form') {
    act(
      () =>
        patch('/settings', {
          refreshSeconds: Number(values.refreshSeconds),
          historyHours: Number(values.historyHours),
          publishOnRefresh: form.elements.publishOnRefresh.checked,
        }),
      { success: 'Settings saved' },
    );
  }
});

// "Search by name" and "Publish now" / "Reload" live in the routed view, so
// they are wired by delegation on the same container as everything else.
$('#view').addEventListener('click', async (event) => {
  if (event.target.id === 'search-btn') {
    const query = $('#add-symbol').value.trim();
    if (!query) {
      toast('Type a company or symbol first');
      return;
    }
    const box = $('#matches');
    box.innerHTML = '<span class="field__hint">Searching…</span>';
    try {
      const { matches, warning } = await api(`/search?q=${encodeURIComponent(query)}`);
      if (warning) toast(warning, 'error');
      box.innerHTML = matches.length
        ? matches
            .map(
              (m) => `<button class="match" type="button" data-symbol="${esc(m.symbol)}">
                  <span class="match__symbol">${esc(m.symbol)}</span>
                  <span class="match__meta">${esc(m.name)} · ${esc(m.exchange)} · ${esc(m.type)}</span>
                </button>`,
            )
            .join('')
        : '<span class="field__hint">No matches. Type the exact symbol instead.</span>';
    } catch (err) {
      box.innerHTML = `<span class="field__hint">Search failed: ${esc(err.message)}</span>`;
    }
    return;
  }

  const match = event.target.closest('[data-symbol]');
  if (match) {
    $('#add-symbol').value = match.dataset.symbol;
    $('#matches').innerHTML = '';
    return;
  }

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

  if (event.target.id === 'refresh-view') refreshView();
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
  // A route change wants fresh data for the view it lands on; Activity in
  // particular has its own collection.
  refreshView();
});

refreshView();
setInterval(loadState, POLL_MS);
