# Changelog

Versions are `vMAJOR.MINOR.PATCH` where the patch number is the repository's
commit count (see [README](./README.md#versioning)). The release workflow reads
the section matching a tag out of this file and uses it as the release body, so
each heading must be `## <tag> — <title>`.

## Unreleased

### Composite tickers

A watchlist row can now be a **formula over other symbols** instead of a
symbol. Type `VTI/GLD` into the same box you would type `AAPL` into and you get
a ratio — stocks against gold — recomputed every cycle from both legs.

- `+ - * /`, brackets and plain numbers: `P/VTI`, `(VTI+GLD)/2`,
  `BTC-USD/GLD`, `VTI*2 - GLD`. Write a subtraction **with spaces**; an
  unspaced hyphen belongs to the symbol, because `BTC-USD` is one.
- Composite rows are outlined in violet and carry a `composite` chip.
  Everything else is identical to an ordinary row: sparkline from stored
  history, change and change percentage against the previous close, pin, pause,
  drag to reorder, publish. That sameness is the design — a composite is a
  ticker with an expression, and nothing else.
- A leg does not have to be on the watchlist. It is fetched for the formula and
  never becomes a row; a leg that *is* on the watchlist is not fetched twice.
- If a leg can't be priced the row says which one, and why, in the provider's
  own words.
- Composites publish under the formula as the key (`"VTI/GLD": "0.9478"`), with
  more decimal places than a fetched price — a `P/VTI` of 0.0335 rendered
  `"0.03"` would be useless. **Nothing an existing consumer already reads
  changes**: fetched symbols keep their two decimals exactly as before.
- Editing a composite's formula re-derives its symbol and drops the stale
  quote, the same way retyping a symbol does; clearing it (with a symbol) turns
  the row back into an ordinary ticker.

Schema migration `003_composite_expression` adds one defaulted column, so an
older binary rolled back onto the new database keeps running.

### Adding a ticker moved to a floating button

The add-a-ticker card that sat above the watchlist is now a floating **+** in
the bottom-right corner, the way CountRoster's create action works — so the
list starts at the top of the page instead of a form you had already used. It
opens a sheet (a bottom sheet on a phone, a centred dialog on desktop) with the
same fields and the same **Search by name**.

It is a real `<dialog>`, which brings the focus trap, the Escape key and an
inert background with it, and it lives outside the ten-second redraw — so what
you type stays typed with no draft machinery involved. Closing it resets the
form; a rejected add (a duplicate, a formula that won't parse) deliberately
keeps it open on what you typed so the fix is an edit rather than a retype.
Toasts now stack above the button rather than landing on it.

### The script becomes an application

Tickers is `update_minion_quotes.py` — a cron script that fetched seven
hardcoded symbols and PUT them into a key-value store — rebuilt as a
self-hosted web app in the shape of
[CountRoster](https://github.com/chinmay28/countroster).

**What you can now do without editing source:**

- Add, replace, relabel, pause, pin, reorder and remove symbols from a browser.
- Pin the symbols you actually watch: pinned symbols always sort to the top of
  the watchlist and of the published payload. The list is configured in
  Settings as comma-separated symbols, with **Pin**/**Unpin** on each row as a
  shortcut into the same list. The seven symbols the script hardcoded ship
  pinned, as the starting example. It is a set rather than an order, so
  drag-to-reorder still works on a pinned row.
- Search Yahoo by company name when you don't know the ticker.
- Publish to several destinations, each with its own base URL, key, category,
  format and timeout — and **Test** one on demand with the real payload.
- Change the poll interval (a seconds field plus 30s/1m/5m/15m/1h presets),
  history retention, and whether every refresh publishes.
- Change the **quote source** — the upstream server URL, the request timeout
  and the User-Agent — and press **Test connection** to fetch one symbol and
  see the price or the exact error. The user-agent field matters in practice:
  Yahoo stonewalls obvious scripts and the string that works drifts, so this
  turns a redeploy into a text box. A read-only *Server* card shows the
  start-up configuration (listen address, database path, client source) that a
  browser genuinely can't change.
- Everything in Settings takes effect on the next cycle; no restart. The
  quote-source fields also exist as `--quote-*` flags for templating a systemd
  unit, and the ordering is stored setting > flag > env > built-in default.
- See recent refresh cycles on an Activity page, with per-symbol counts, which
  verb each destination accepted, and the failures in full. The server retains
  the last 500.

**Typing is never interrupted.** The client re-reads the server every 10
seconds, and a page that redraws itself on a timer will happily do it while you
are halfway through a form. It no longer does: a background redraw waits for
the field you are in, and anything you have typed is put back afterwards, so
adding a destination or editing Settings survives both the poll and a trip to
another tab. Search results stay put until you pick one.

**What deliberately did not change.** The published payload is byte-for-byte
what the script wrote — flat map of symbol → 2-decimal string, `"N/A"` for a
failure, a `"timestamp"` in `MM/DD HH:MM:SS` — sent with the same
PUT-then-POST fallback, and the price is still the last non-null close of the
1-minute intraday series. Existing consumers need no changes. A new `detailed`
format is available per destination for anyone who'd rather not parse strings.

**How it runs.** One static Go binary with the web client embedded via
`go:embed` and a pure-Go SQLite driver — no Python, no Node, no runtime
dependencies, and it cross-compiles to a Raspberry Pi.

- `scripts/quickstart.sh` installs it as a hardened systemd service from source
  or from a prebuilt release, and re-running it upgrades in place: it stages
  the new binary while the old one serves, snapshots the database once the
  service is quiesced, health-checks the result, and rolls back — binary and
  data — if the new version is unhealthy.
- Schema changes go through an append-only, idempotent, additive-only migration
  runner, which is what makes that rollback safe.
- Listens on **8797** so it can share a Pi with CountRoster.

**Also:** `tickers publish` runs one cycle and exits, for anyone who'd rather
keep using cron than run a daemon.
