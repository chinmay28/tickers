# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

All Go commands run from `server/`.

```bash
cd server
go vet ./...
go test -race ./...                          # what CI runs
go test -run TestParseCanonicalises ./internal/expr/
go test ./internal/store/ -v                 # one package, verbose
gofmt -l .                                   # must print nothing; CI fails otherwise
```

Build and run:

```bash
scripts/build.sh                             # → server/bin/tickers
GOOS=linux GOARCH=arm64 scripts/build.sh     # Raspberry Pi target (CI checks this)
scripts/version.sh                           # the version this commit builds

./server/bin/tickers serve --db ./data/tickers.sqlite    # http://localhost:8797
./server/bin/tickers publish --db ...                    # one fetch+publish cycle, then exit
```

**Working on the web client, serve the assets from disk** — otherwise every CSS
tweak needs a rebuild to clear `go:embed`:

```bash
go run ./server/cmd/tickers serve --web-dist server/internal/web/assets
```

**Exercising the app without hitting Yahoo:** `--quote-base-url` points the
provider at any host serving `/v8/finance/chart/{symbol}` in Yahoo's shape. A
throwaway stub server plus `--db /tmp/x.sqlite` gives a full working instance
with deterministic prices, which is the only way to see the UI's real states
(changes, sparklines, error rows) end to end.

CI (`.github/workflows/ci.yml`) is exactly: `gofmt -l server`, `go vet`,
`go test -race`, `scripts/build.sh`, `bash -n scripts/*.sh`, arm64 cross-compile.

## Constraints that shape every change

These are not style preferences. They come from the deployment story — people
run this on a Raspberry Pi and upgrade by re-running one command that rolls back
to the previous binary if the new one fails its health check.

1. **The published payload is a contract.** `internal/publish/publish_test.go`
   pins the legacy `minion` format byte for byte — flat map of symbol to a
   2-decimal string, `"N/A"` for failures, `MM/DD HH:MM:SS` timestamp. If a
   change makes those fail, the change is wrong; add a format instead. The one
   sanctioned exception is composites (below), which no pre-existing consumer
   can have been reading.
2. **Migrations are append-only and additive.** Never edit or reorder a shipped
   migration in `internal/store/migrations.go`; never drop or rename a column an
   older binary reads. A rolled-back binary sees the newer schema and only keeps
   working if the new schema is a superset.
3. **One static binary, no runtime dependencies.** `CGO_ENABLED=0` with a pure-Go
   SQLite driver is what makes cross-compiling to a Pi work.
4. **No front-end build step.** The client is hand-written HTML/CSS/ES modules
   embedded with `go:embed`. A bundler would put Node on every install.
5. **`/api/health` must fail when the database is unreachable.** The upgrade
   rollback keys off it.

## Architecture

`store` validates, `engine` behaves, `api` decodes/encodes. Putting validation in
a handler or business logic in the store is the main way to get this wrong.

**`internal/store`** owns SQLite — schema, migrations, every query. Nothing else
opens the file. Returns plain `errors.New` sentences for bad input (`api`
pattern-matches those into 400s) plus real sentinels: `ErrNotFound`,
`ErrDuplicateSymbol`, `ErrInvalidExpression`.

**`internal/engine`** runs the refresh loop: read config → build a fetch plan →
fetch → store one quote per ticker (success or failure) → prune history →
publish if configured → append a run record. A cycle never fails because a
symbol failed; only structural errors surface. Config is re-read **every pass**,
which is why Settings takes effect without a restart.

**`internal/api`** is Go 1.22+ pattern routing and nothing else. `GET /api/state`
returns everything the client renders in one round trip.

**`internal/expr`** is the formula language behind composites (see below).

### Things that require reading several files to discover

- **A composite is a ticker with a non-empty `expression`, and nothing else.** It
  reuses the same table, quote row, history series and publish path, which is
  why sparklines, change-vs-previous-close, pinning and publishing all worked on
  it without knowing it exists. Its `symbol` is *derived* from the canonical
  formula (spaces removed), so it has one stable unique key like every other row.
- **Composite-ness is not stored on the quote row.** `Engine.Snapshot` already
  joins tickers to quotes and stamps `Quote.Composite` there. It exists so the
  payload can give a ratio 2/4/6 decimals by magnitude — a `P/VTI` of 0.0335
  published as the legacy `"0.03"` is useless. Fetched symbols stay at 2.
- **The hyphen is the one ambiguous character in a formula:** subtraction, and
  also part of `BTC-USD`. The lexer resolves it positionally — glued to symbol
  characters it belongs to the symbol, spaced it is the operator.
- **Composite legs are fetched but never become rows,** and are deduplicated
  against the watchlist, so a ratio over a symbol already tracked costs no extra
  request.
- **Pinned symbols live in `settings` as a comma-separated list of symbols,** not
  on the ticker row. Every query that returns a ticker stamps a derived `Pinned`.
  It is a *set, not an order* — the sort is stable and keys only on pinned-ness,
  so `position` still decides sequence within each group and dragging keeps
  working.
- **History is keyed by symbol, not ticker ID,** so deleting a ticker leaves its
  series behind and re-adding brings the chart back.
- **There are two histories, from two places.** `quote_history` is the
  sparkline's — one point per cycle, pruned to a window measured in hours.
  The performance sheet's five-year series is fetched from the provider through
  `quotes.Historian`, an *optional* interface asserted at the call site like
  `Configurable`; a provider without it gets `ErrNoHistory` and a 501, not a
  failure. Composite series are the refresh cycle's pricing run once per day,
  with the legs' dates intersected.
- **One window list, two projections.** `windows` in `performance.go` drives
  both the returns table and the high/low ranges; the client shows returns for
  a symbol and ranges for a composite, because a ratio has no capital in it to
  have returned anything. A return starts at the last close *on or before* the
  window; a range covers only closes *inside* it, and needs the whole window
  covered to be reported at all. Both are computed from the full series — the
  chart's points are thinned, the numbers never are.
- **Settings are a key/value table** so adding one never needs a migration. An
  unset key reads back as its default, not as zero — that distinction is what
  lets "unpin everything" differ from "never configured".
- **Config precedence differs by field.** Listen address, DB path and friends are
  flag > env > default. The three quote-source fields are
  **stored setting > flag > env > built-in default**, so the GUI wins and
  clearing a field reveals the flag again.
- **`origin` is provenance only.** Nothing reads it at runtime.

### The web client

One `state` object from `/api/state`, a hash router, and full re-renders of
`#view` every 10s. Because that happens under people typing, two rules apply and
both are needed: a background redraw **waits while a field has focus** (and is
owed on `focusout`), and typed values are **stashed by (form, field) and put
back** after any redraw. Skipping either reintroduces the bug.

**The add dialog is the deliberate exception.** It lives in the shell in
`index.html`, not in `#view`, so nothing in it has to survive a redraw — no
draft, no focus capture. `showModal()` supplies the focus trap, Escape and an
inert background. Its actions sit in `.sheet__foot` outside the scrolling body,
attached by the `form` attribute.

**`--keyboard-inset`** is how the phone layout survives the soft keyboard. A
modal dialog is positioned against the *layout* viewport, which the keyboard
does not shrink; `trackKeyboard` measures the difference against the *visual*
viewport and publishes it, so the sheet can sit on top of the keyboard. It reads
`0px` on desktop and without `visualViewport`, so nothing needs a fallback.

On phones, inputs must stay at `font-size: 16px` or larger — below that iOS
Safari zooms the page the moment a field is tapped.

## Conventions

- Comments explain *why*, not *what*, and the existing code is the reference for
  density and tone. Match it — this codebase is unusually heavily commented on
  rationale and thin on restating the code.
- Tests assert behaviour with messages that name what broke, not `got != want`.
- Update the docs a change touches: `README.md` for user-facing behaviour,
  `docs/DESIGN.md` for structural decisions, `DEPLOYMENT.md` for operator
  concerns, and a line in `CHANGELOG.md` under **Unreleased**.
- One concern per PR. Sign off commits (`git commit -s`).
- The version's patch number is the repository's commit count, so it changes
  every commit — never hardcode a version.
