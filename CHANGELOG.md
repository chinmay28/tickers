# Changelog

Versions are `vMAJOR.MINOR.PATCH` where the patch number is the repository's
commit count (see [README](./README.md#versioning)). The release workflow reads
the section matching a tag out of this file and uses it as the release body, so
each heading must be `## <tag> — <title>`.

## Unreleased

### Portfolios, and what they would have done

A new **Portfolios** page backtests an allocation: symbols and weights, an
initial amount, a rebalancing cadence, and an optional benchmark.

- **Growth compounded monthly** from the quote source's own daily closes,
  reduced to one per month. Those closes are split- and dividend-adjusted, so
  distributions are already reinvested and the result is a total return rather
  than a price chart.
- **CAGR, annualised volatility, best and worst full calendar year, and the
  deepest peak-to-trough fall** — the last with the months it ran between and
  whether it was ever recovered, which is the part a depth on its own leaves out.
- **A benchmark symbol** run at 100% over the same months. Both sides are cut to
  the period they share; a comparison drawn over months one side didn't trade is
  not a comparison.
- **Every calendar year**, with a part year shown and labelled rather than
  dropped or quietly claimed as a whole one.
- The run starts where its **latest holding** does, whatever start year is
  asked for, and names the holding responsible. Rebalancing lands on calendar
  boundaries, and *never* lets the weights drift on purpose.
- Holdings don't have to be on the watchlist, and the daily series is shared
  with the performance sheet — a portfolio over funds you also chart costs no
  extra requests.

Nothing models fees, taxes, spreads, contributions or inflation. It is what the
allocation did, not what an account holding it would have.

### Activity folded into Publishing

The run log has moved under the destinations it describes, as a **Recent
cycles** section on the **Publishing** page, and the Activity tab is gone.
Nothing about the log itself changed — same cycles, same counts, same
per-destination detail. It was always read to answer a publishing question, and
that answer now sits on the same page as the destination it is about.

### Historical performance, on a double-tap

**Double-tap (or double-click) a watchlist row** and it opens a sheet with a
chart of that symbol's daily closes and a table of its returns.

- For a symbol, **returns** over 1 week, 1 month, 3 months, year to date, and
  1, 3, 5 and 10 years, plus **all time** — each measured from the last close
  *on or before* the period's start, because markets are shut at weekends.
  Anything longer than about a year also shows a compound annual rate, worked
  out from the dates the baseline actually has rather than the window's name.
- For a **composite**, **highs and lows** instead: the low, the high, the days
  they happened, and where the latest value sits between them, over the same
  windows down to **all-time high and all-time low**. A ratio has no capital in
  it to have returned anything, but "at 22% of its all-time range" says
  something true about the same number.
- A period the series doesn't cover says **"not enough history"** rather than
  quietly measuring a young listing from its first day. A range needs the whole
  window covered: every close a symbol listed last month has falls inside the
  last ten years, and calling their high a ten-year high would be a fabrication.
- Span chips (1M/3M/6M/YTD/1Y/5Y/10Y/All) re-slice the chart without another
  request, and dragging across the line reads off any single day.
- The series comes from the quote source, not from the stored sparkline
  history: that is pruned to a window measured in hours and can only say what a
  symbol did today. Closes are adjusted for splits and dividends where the
  source reports them, and each bar is dated by its own exchange's calendar.
- **Composites get the same sheet**, recomputed from the formula on every day
  all of its legs traded. Days where a leg was shut are dropped rather than
  carried forward — a ratio for a day one leg didn't trade never existed.
- Series are cached per symbol for ten minutes, so a repeated double-tap costs
  one fetch and a composite over symbols already on the watchlist costs none.
- Forty years of daily closes is a quarter of a megabyte and ten thousand SVG
  segments, so closes older than two years are thinned to weekly and then
  monthly for the chart — keeping the high and the low, so the chart never
  contradicts the table beside it. Returns and ranges are computed from the
  full series, and the chart's x axis is time rather than point index.

New endpoint `GET /api/tickers/{id}/performance`. Past prices are an optional
provider capability (`quotes.Historian`), so a quote source that can only price
today keeps working and the sheet says what is missing instead of failing. No
schema change: nothing about this is stored.

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

**It works with a soft keyboard up**, which took more than it looks like it
should. A modal dialog is positioned against the layout viewport, and the
keyboard does not shrink that — so a bottom sheet puts its Add button behind
the keyboard the moment you tap the field above it. The sheet now sits on top
of the keyboard instead, measured from the visual viewport, and the Add and
Search buttons are pinned to the bottom of the sheet rather than scrolling with
the fields. The tab bar and the floating button get out of the way while the
keyboard is up, and come back when it closes.

Also on phones: every input is 16px or larger, because below that iOS Safari
zooms the page in the moment you tap a field and leaves you zoomed and scrolled
sideways. Inputs, the sheet's buttons and its close control are all past the
44px touch target. The formula help folds into a **More about formulas**
disclosure rather than spending a quarter of the screen on prose you read once.

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
