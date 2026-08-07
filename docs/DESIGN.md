# Tickers — design

This document explains what the pieces are and why they are shaped the way
they are. [README.md](../README.md) is what the app does;
[DEPLOYMENT.md](../DEPLOYMENT.md) is how to run it;
[server/README.md](../server/README.md) is the Go module and its CLI.

## Where it came from

The whole application is a generalisation of one script:

```python
tickers = ["VTI", "GLD", "P", "ORCL", "STRC", "IBIT", "BTC-USD"]
quotes  = {t: f"{yf.Ticker(t).history('1d','1m')['Close'].iloc[-1]:.2f}" for t in tickers}
quotes["timestamp"] = datetime.now().strftime("%m/%d %H:%M:%S")
try:    requests.put(f"{base}/{key}", json={"value": quotes, "category": "minion"})
except: requests.post(base, json={"key": key, "value": quotes})
```

Four things in there are contracts, not implementation details, and the Go
version reproduces all four exactly:

1. **The payload shape.** A flat map of symbol → 2-decimal string, `"N/A"` when
   there is no price, and a `"timestamp"` key in `MM/DD HH:MM:SS`.
2. **The PUT-then-POST fallback.** PUT the entry at `{base}/{key}`; on any
   failure, POST `{"key": …, "value": …}` to `{base}`.
3. **The price itself.** The last non-null close of the 1-minute intraday
   series — *not* the quote endpoint's `regularMarketPrice`, which can differ
   by a minute. `yfinance` reads Yahoo's `/v8/finance/chart` endpoint, and so
   does `internal/quotes`.
4. **Never failing loudly.** A symbol that can't be priced becomes `"N/A"`; the
   cycle still publishes.

Everything else — the watchlist, the schedule, the destinations — was hardcoded
and is now data.

## Shape

```
                    ┌──────────────────────────────────────┐
   browser ───────► │  api      REST + the embedded client │
                    ├──────────────────────────────────────┤
                    │  engine   the refresh/publish cycle   │
                    ├──────────────┬───────────────────────┤
                    │  quotes      │  publish              │
                    │  (Yahoo)     │  (your endpoints)     │
                    ├──────────────┴───────────────────────┤
                    │  store    SQLite: schema + queries   │
                    └──────────────────────────────────────┘
```

Dependencies point one way: `api` → `engine` → {`quotes`, `publish`} → `store`.
Nothing below reaches back up. `store` is the only package that opens the
database; `quotes` and `publish` are the only packages that make outbound HTTP
calls; `api` contains no domain logic at all — it decodes, chooses a status
code, and encodes.

### Why Go, and why one binary

The original ran under Python with `yfinance`, `pandas` and `requests` — about
80 MB of dependencies to fetch seven numbers, and an interpreter to keep
working across OS upgrades on a Raspberry Pi. The rewrite target was a single
static file:

- **`modernc.org/sqlite`** — a pure-Go SQLite, so `CGO_ENABLED=0` produces a
  fully static binary that cross-compiles to `linux/arm64` from any machine.
- **`go:embed`** for the web client — so there is no asset directory to deploy,
  no path to misconfigure, and no version skew between the API and the client
  that talks to it.
- **A hand-written client** (HTML + CSS + one ES module) rather than a bundled
  framework — so `go build` is the entire build and the Pi install never needs
  Node. This is the one place the project accepts a bit more code in exchange
  for a much simpler deployment.

Those are the only two third-party concerns in the tree; everything else is the
standard library.

## Data model

SQLite, WAL mode, one writer. Timestamps are RFC3339 in UTC — sortable as text
and readable when you open the file by hand.

```sql
tickers(id, symbol UNIQUE, label, position, enabled, origin, created_at, updated_at)
quotes(ticker_id PK → tickers, symbol, price, previous_close, currency,
       short_name, market_state, status, error, fetched_at)
quote_history(id, symbol, price, at)
sinks(id, name, base_url, key, category, format, enabled, timeout_ms, …)
runs(id, started_at, finished_at, trigger, ok_count, error_count, publishes, error)
settings(key PK, value)
schema_migrations(id PK, applied_at)
```

A few decisions worth naming:

**`origin` is what makes placeholders work.** A fresh install seeds the seven
symbols from the script with `origin = 'seed'`. That is purely cosmetic to the
refresh loop, but it is what lets the UI put a `placeholder` chip and a
**Replace** button on a row nobody chose. Changing a row's symbol promotes it to
`origin = 'user'`, and the chip goes away for good.

**Seeding is keyed off a settings flag, not "is the table empty".** Otherwise
deleting every ticker would resurrect the author's watchlist on the next
restart — the opposite of what someone who just cleared the list wants.

**A failed read is still a quote.** `status = 'error'` with the reason, rather
than no row. "We tried and it didn't work" is information the UI must show, and
dropping the row would make a broken symbol look like one that was never asked
about. Failed reads are excluded from `quote_history`, though: a chart that
plots gaps as data is a chart that lies.

**History is keyed by symbol, not ticker ID.** Deleting a ticker takes its
current quote with it (`ON DELETE CASCADE`) but leaves the series behind, so
re-adding a symbol you removed by accident brings its chart back.

**Settings are a key/value table.** Adding one never needs a migration — which
matters because a rolled-back binary has to keep running against a database a
newer version has already touched. An unset key reads back as its default, not
as zero. That property is what let the quote-source settings ship without a
schema change at all.

**`runs` is capped at 500 rows.** An unbounded audit log on a Pi's SD card is a
slow-motion disk-full bug.

### Migrations

`internal/store/migrations.go` is an append-only list of `{ID, SQL}` steps, each
applied in its own transaction and recorded in `schema_migrations`. Two rules
govern it, and both exist because of the upgrade story:

1. **Never edit a shipped migration.** A deployed database has already recorded
   it and will skip it forever, so an edit only changes what *fresh* installs
   get — which is how two installs of the same version end up with different
   schemas.
2. **Keep every step additive.** `scripts/quickstart.sh` rolls back to the
   previous binary when an upgrade fails its health check. That older binary
   sees the newer schema, and it only keeps working if the new schema is a
   superset of the one it knows.

`/api/health` reports the applied list, so an operator can see from outside
which schema a running instance is on.

## The quote source

Prices come from **Yahoo Finance's public HTTP endpoints, called directly**.
There is no `yfinance` here — that is a Python library, it went with the rest
of the interpreter, and dropping it is most of why the deployable artifact is
one file. Two plain GETs are the whole integration:

| | |
|---|---|
| Quotes | `GET {base}/v8/finance/chart/{SYMBOL}?range=1d&interval=1m` |
| Symbol search | `GET {base}/v1/finance/search?q=…` |

The *behaviour* is still yfinance's, deliberately.
`yf.Ticker(t).history(period="1d", interval="1m")['Close'].iloc[-1]` reads that
same chart endpoint and takes the last close of the 1-minute series, so the
parser scans the `close` array **backwards for the last non-null value** rather
than reading `meta.regularMarketPrice`. Yahoo returns nulls for the current,
not-yet-closed minute, and the two values can disagree by a minute — which
would show up as this app and the old script quoting different numbers for the
same instant. `meta.regularMarketPrice` is the fallback for when the series
comes back empty, which is normal outside trading hours.
`TestFetchPrefersLastNonNullClose` pins it against a fixture with a
trailing-null series.

Three things follow from this being an *unofficial* API — no key, no
documentation, no stability contract:

- **It can start refusing you.** The endpoints answer browsers and stonewall
  obvious scripts, and the User-Agent that works drifts. That is why the UA is
  a GUI field rather than a constant: the fix should be a text box, not a
  redeploy.
- **It can go away, or be blocked by the network the Pi is on.** Hence the
  configurable base URL — point it at a mirror or a caching proxy — and the
  `POST /api/provider/test` probe, which reports the raw outcome so a wrong
  URL, a blocked network and a bad symbol don't all look identical.
- **A failure is per-symbol, never fatal.** `Fetch` returns two maps, quotes
  and errors, and one bad symbol cannot take the others down with it.

Swapping in a different source (a paid feed, Stooq, Alpha Vantage) is a new
file in `internal/quotes/` implementing `Provider` and nothing else changing —
that is the entire reason the interface exists. It is not a planned feature;
see the non-goals.

## Configuration precedence

Two kinds of configuration, split by a single question: *can this change while
the process is running?*

**Start-up flags** — the listen address, the database path, `--web-dist`. A
running process cannot move the socket it is already accepting on or swap the
file it has open, so these are flags only. They are reported read-only by
`/api/state` and shown on the Settings page, because "which database is this
instance actually using?" should not require reading a unit file.

**Stored settings** — the interval, retention, publish-on-refresh, and the
quote source's URL, timeout and user agent. These live in the database, are
edited in the GUI, and are re-read every cycle.

The quote-source fields exist in *both* places, and the ordering is
**stored > flag > env > built-in default**, implemented by
`quotes.Settings.Merge`: a zero field means "defer to the layer beneath", so
clearing a box in the GUI reveals the flag, and clearing the flag reveals the
provider's own default. The flags are there so a systemd unit can be templated;
the GUI is there so a blocked user agent can be fixed from a phone without a
redeploy.

Pushing a stored change into a live provider is `quotes.Configurable`:

```go
type Configurable interface {
    Apply(Settings)      // safe to call while Fetch is in flight
    Effective() Settings // what is in force, with defaults resolved
}
```

`Engine.ApplyConfig` calls it before every upstream operation — the refresh
cycle, symbol search, and the connection test — because "I changed the setting
and nothing happened" is the one failure a settings page must never have. On
the `Yahoo` side a `sync.RWMutex` guards the swap, and the HTTP client is
rebuilt only when the timeout actually moves: mutating a live client's
`Timeout` would race with requests already using it, and building one per
request would throw away connection pooling.

`Effective()` is what the UI renders, which is why an empty override field can
show the value it is falling back to as its placeholder.

## The refresh cycle

`engine.RunCycle` is the whole of the original `main()`:

1. Read the config and the enabled watchlist.
2. Fetch every symbol concurrently (capped at 4 in flight — a domestic
   connection firing twenty at once collects timeouts, not quotes).
3. Store one quote per ticker, success or failure, and append a history point
   for each success.
4. Prune history past the retention window.
5. If `publishOnRefresh`, build the snapshot and send it to every enabled sink.
6. Append a `run` record.

It never fails as a whole because a symbol failed — only something structural,
like an unreadable database, comes back as an error. Cycles are serialised by a
mutex, so a manual refresh landing on top of a scheduled one waits rather than
double-publishing.

`Start` re-reads the interval from the database **every pass** rather than
capturing it once, which is why changing it in Settings takes effect after the
current wait rather than at the next restart. A `Kick` channel (buffered by
one, so a double-tapped button collapses to one run) lets the API reset that
wait.

The snapshot includes enabled tickers that have no stored quote yet, rendered
as `"N/A"`. A consumer watching for a key is better served by a key that says
"unknown" than by a key that intermittently isn't there.

## Publishing

`publish.Payload` is the compatibility surface, and
`internal/publish/publish_test.go` is its specification: if a change would alter
what an existing consumer receives, those tests fail.

`Publish` does the PUT-then-POST dance per sink and records which verb landed,
so Activity can distinguish "updated" from "created" without guessing. When both
fail it reports **both** errors — being told only about the POST sends people
looking at the wrong endpoint, and the PUT's status is usually the one that
explains what the store actually wants.

Sink URLs are validated to `http`/`https` on the way in. The server POSTs
whatever is configured there, so restricting the scheme keeps a typo — or a
hostile settings payload — from turning the publisher into a `file://` client.

## The API

Go 1.22+ pattern routing (`"PATCH /api/tickers/{id}"`), so there is no
hand-rolled dispatch on method or path.

`GET /api/state` returns everything the client needs to render — tickers joined
to their quotes, sinks, settings, engine status, and a payload preview — in one
round trip. A page that had to fan out to five endpoints to redraw would spend
its life half-updated.

| Method | Path | |
|---|---|---|
| GET | `/api/health` | status, version, uptime, applied migrations |
| GET | `/api/state` | everything the client renders |
| GET/POST | `/api/tickers` | list / add |
| PATCH/DELETE | `/api/tickers/{id}` | edit (incl. replace) / remove |
| POST | `/api/tickers/reorder` | `{ids: [...]}` |
| GET | `/api/tickers/{id}/history` | sparkline points |
| GET/POST | `/api/sinks` | list / add |
| PATCH/DELETE | `/api/sinks/{id}` | edit / remove |
| POST | `/api/sinks/{id}/test` | send the real payload to one destination |
| GET/PATCH | `/api/settings` | read / update |
| POST | `/api/provider/test` | fetch one symbol through the current settings |
| GET | `/api/runs` | recent cycles |
| POST | `/api/refresh` | run a cycle now |
| POST | `/api/publish` | publish the current snapshot now |
| GET | `/api/preview` | render the payload without sending it |
| GET | `/api/search` | resolve free text to symbols |

Conventions:

- Request bodies reject unknown fields. A client sending `{"symbl": "VTI"}`
  should be told, not silently given a ticker with an empty symbol.
- Errors are `{"error": "…"}`. `404` for a missing row, `409` for a duplicate
  symbol, `400` for anything the caller can fix, `500` otherwise.
- A path or method under `/api/` that matches nothing gets a JSON 404 — never
  the HTML shell, which a client would then try to JSON-parse.
- `/api/search` returns `200` with a warning even when the upstream search
  fails. Search is a convenience on top of a third-party endpoint that can be
  blocked or rate-limited; failing it hard would block adding a ticker, and
  typing the exact symbol always works.

## The web client

One `state` object from `/api/state`, a hash router picking a render function,
and full re-renders of the routed view. The data is a handful of rows; diffing
it would cost more code than redrawing it. Every interpolated value goes
through an escape helper — symbols and sink names are user-supplied.

The client polls `/api/state` every 10s, deliberately shorter than the server's
30s minimum refresh interval, so a cycle's results never sit invisible for a
whole poll.

Caching is split on purpose: the shell, `app.js`, `styles.css` and the manifest
are `no-cache`, while icons and the badge get a day. An upgrade that left a
cached `app.js` talking to a new API is exactly the failure the non-disruptive
upgrade story is meant to prevent.

The developer mark in the header is a muted disk that comes to full strength on
hover and throws the badge up full screen for three seconds when tapped —
Escape or a tap ends it early. It matches CountRoster's, deliberately.

## Threat model

There is no authentication, by design, and the README says so plainly: run it
on a trusted network. Within that model the server still does the things that
are cheap and still buy something — `X-Content-Type-Options`, `X-Frame-Options`,
a 1 MB cap on request bodies, an 8 MB cap on provider responses, scheme
validation on outbound URLs, and a systemd unit with `ProtectSystem=strict`
and exactly one writable path.

## Deliberate non-goals

- **Portfolios, holdings, P&L.** This publishes prices. Whatever consumes them
  can do the arithmetic.
- **Alerting.** The downstream key-value store already has consumers; alerting
  belongs there.
- **Multiple users.** No accounts means no per-user watchlists, and that is the
  right trade for a household service on a LAN.
- **A licensed data feed.** Yahoo's public endpoints are free, unauthenticated
  and unsupported. The provider is an interface so a paid feed could be added,
  but that is not what this is for.
