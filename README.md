<div align="center">
  <img src="server/internal/web/assets/icon.svg" alt="" width="96" height="96" />
  <h1>Tickers</h1>
  <p><em>A self-hosted watchlist that fetches quotes on a schedule and publishes them to your own endpoints.</em></p>
</div>

Tickers grew out of [a 50-line Python cron script](./legacy/update_minion_quotes.py)
that fetched seven hardcoded symbols with `yfinance` and PUT them into a
home-automation key-value store. It did one useful thing, and changing the
watchlist meant editing the source and redeploying.

This is that script as a proper application — built the same way as
[CountRoster](https://github.com/chinmay28/countroster):

- **A GUI for the watchlist.** Add, replace, relabel, pause, pin, reorder and
  remove symbols from a browser. The seven symbols the script hardcoded ship as
  the starting **pinned** list, configured in Settings — pinned symbols always
  sort to the top of the watchlist and of the payload.
- **Backwards-compatible publishing.** The payload is byte-for-byte the one the
  script wrote — flat map of symbol → 2-decimal string, `"N/A"` for a failure,
  a `"timestamp"` in `MM/DD HH:MM:SS` — with the same PUT-then-POST fallback.
  Anything already reading that entry keeps working, unchanged.
- **Many destinations, not one.** Publish the same snapshot to several
  key-value endpoints, each with its own key, category, format and timeout.
- **One static binary.** No Python, no Node, no runtime dependencies. The web
  client is embedded with `go:embed`, so `go build` is the whole build.
- **Non-disruptive upgrades.** Re-run one command: it snapshots the database,
  swaps code in, health-checks the result, and rolls back — data and all — if
  the new version is unhealthy.
- **No accounts, no auth.** Meant to run on a trusted network (your LAN, a
  Tailscale tailnet, a VPN). Anyone who can reach the server can use it.

## Layout

```
tickers/
├── docs/DESIGN.md              # architecture & design document
├── DEPLOYMENT.md               # deploying, upgrading, backing up
├── legacy/                     # the original cron script, kept for reference
├── scripts/                    #   quickstart.sh · build.sh · version.sh
├── deploy/                     #   reference systemd unit
└── server/                     # the Go application — ONE static binary
    ├── cmd/tickers/            #   entrypoint & CLI
    └── internal/
        ├── store/              #   SQLite: schema, migrations, every query
        ├── expr/               #   the formula language behind composite rows
        ├── quotes/             #   quote providers (Yahoo Finance)
        ├── publish/            #   downstream publishing + the legacy payload
        ├── engine/             #   the scheduled refresh + publish cycle
        ├── archive/            #   the market-data archive: a folder of SQLite files
        ├── collector/          #   fills it from each source, paced to its limits
        ├── archiver/           #   keeps it open wherever the settings say it is
        ├── universe/           #   every symbol listed on a US exchange
        ├── api/                #   the REST layer
        └── web/assets/         #   the web client, embedded at build time
```

The deployable artifact is a **single static Go binary**
(`server/bin/tickers`) that serves the REST API and the web client from one
origin, with zero runtime dependencies.

## Getting started

```bash
scripts/build.sh                              # needs Go >= 1.21 (newer toolchains fetch automatically)
./server/bin/tickers serve --db ./data/tickers.sqlite
```

Open `http://localhost:8797`. The watchlist starts with the seven shipped
symbols, all pinned; press **Edit** on each, or **Add** your own and pin what
you actually watch. To use it from your phone, reach the server over your
LAN/Tailscale and "Add to Home Screen".

While working on the web client, skip the rebuild — serve the assets from disk:

```bash
go run ./server/cmd/tickers serve --web-dist server/internal/web/assets
```

### Quick start on a Raspberry Pi (or any Linux box)

Install Tickers as a hardened **systemd service** with one command:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh | sudo bash
```

(or, from a checkout: `sudo ./scripts/quickstart.sh`)

It installs Go if needed (build-time only), creates a dedicated `tickers`
system user, compiles the static binary, and runs it under systemd serving the
API + web client on `http://<host>:8797`.

**Or skip the build entirely** and install the prebuilt binary from the latest
[release](https://github.com/chinmay28/tickers/releases) — no Go, no source
tree, seconds instead of minutes on a Pi:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh \
  | sudo TICKERS_INSTALL=release bash
```

The download's checksum is verified before anything is swapped in, and
`TICKERS_RELEASE=v2026.8.42` pins a specific release instead of the latest.
Releases publish **`linux/arm64`** and **`linux/amd64`**; other architectures
build from source (the default), which works everywhere. Both modes install the
same thing — one static binary with the web client embedded, under the same
unit and the same data directory — so you can switch between them by re-running
with a different `TICKERS_INSTALL`.

> Tickers listens on **8797**, not 8787, so it and CountRoster can share a Pi.

**Re-run it any time to upgrade — installs and upgrades are non-disruptive and
never lose data:**

- The live SQLite database lives at a stable path **outside** the source tree
  (`/var/lib/tickers/`), so rebuilding or pulling can't clobber it.
- Each upgrade quiesces the service, **snapshots the database** (`+ WAL/SHM`) to
  a timestamped backup, then swaps code in. The new binary is built (or
  downloaded) to a staging path while the old one keeps serving, so a failed
  build leaves the running app untouched.
- After restart it polls `/api/health`; if the new version is unhealthy it
  **rolls back** to the previous binary and **restores the pre-upgrade
  snapshot**.
- Schema changes run through an append-only, idempotent migration runner, and
  every migration is additive — so the binary it rolls back *to* can still read
  the database the new one touched.
- Re-running never re-seeds: symbols you replaced stay replaced, symbols you
  deleted stay deleted, and your pinned list stays as you set it.

Override defaults with env vars (`PORT`, `HOST`, `TICKERS_INSTALL`,
`TICKERS_REF`, `TICKERS_RELEASE`, `TICKERS_DATA_DIR`, `TICKERS_ARCHIVE_DIR`, `TICKERS_PREFIX`,
`TICKERS_USER`, …). The generated unit is documented at
[`deploy/tickers.service`](./deploy/tickers.service). Manage it with
`systemctl status tickers` and `journalctl -u tickers -f`.

## Using it

**Watchlist** — the symbols being tracked. The floating **+** in the
bottom-right corner is how you add one; it opens a sheet that takes a symbol or
a formula, with **Search by name** for when you don't know the ticker. Each row
shows the last price, the move from the previous close, a sparkline from stored
history, and when it was last read. Each carries a small **mark**: by default the
symbol's initials over a colour derived from the symbol itself, drawn locally so
rows are told apart at a glance without a logo service knowing what you track.
**Edit** a row to upload your own image for it — that works for a symbol, a
composite and a portfolio alike, needs nothing configured, and is left alone by
everything else. Turn **Symbol logos** on in Settings and the server also
fetches a real logo for each symbol it can — see below. Pinned symbols carry a `pinned` chip and
always sort above the rest;
**Pin**/**Unpin** on a row edits the same list the Settings page shows as text.
Drag to reorder on a desktop, or use ↑↓ on a phone; **the watchlist order is
the payload order**. Pinning never takes reordering away from a row — it is a
set, not an order, so the watchlist's own sequence still applies within the
pinned group.

Three kinds of row share it. A **symbol** is fetched. A **composite** is
computed from a formula. And a **portfolio** — every saved allocation gets one,
automatically — is the value of that allocation's holdings, marked to today's
prices. Portfolio rows are keyed by the portfolio's name (`Four fund` publishes
as `FOUR-FUND`), carry a `portfolio` chip, and are managed from the Portfolios
page: **Open** takes you there, and there is no **Remove**, because removing one
means deleting the portfolio.

A portfolio row is a **holding, not a target**. It starts at exactly the
portfolio's initial amount and moves with the holdings from there; its units are
fixed, so the weights drift exactly as a real account's would and it never
quietly rebalances itself between refreshes — rebalancing belongs to the
backtest, where there is a period to rebalance over. Every holding has to price
for the row to have a value: three quarters of a portfolio is not what it is
worth.

The baseline is only reset when the **allocation or the initial amount** changes,
because different holdings are genuinely different units. Renaming a portfolio,
changing its benchmark, its start or end year, its rebalancing cadence, its
contributions or a replacement all leave the row alone — none of them change
what is held, and none of them should reset a number you have been watching
climb.

**History** — **double-tap (or double-click) a row** to open its performance
sheet: a chart of the daily closes behind it, and a table underneath. Pick a
span with the chips above the chart — 1M through 10Y and All — and drag across
the line to read any single day off it.

For an ordinary symbol the table is **returns**: a day, a week, a month, three
months, the year to date, and one, three, five and ten years, plus all time.
Anything measured over more than about a year also shows a compound annual rate,
worked out from the dates the baseline actually has rather than the window's
name.

**A day means the previous close**, counted rather than dated. Every other window
starts on a date — a month is the same day last month — but a series of daily
closes has no 24 hours in it to measure: dated, "1 day" would be empty every
Sunday and every Monday until that day's close arrived, and a row saying "not
enough history" about a twenty-year-old symbol teaches a reader to distrust the
rest of the sheet.

For a **composite** it is **highs and lows** instead — a ratio has no capital in
it to have returned anything, but "it is 7% below its all-time high, at 22% of
that range" says something true about the same number. Each row gives **how far
the latest value is from each end as a percentage**, with the low and the high
themselves and the days they happened underneath, then where it sits between the
two: one day, one week, one month, three months, year to date, one year, five
years, ten years, and all time. The short windows are there because the table
reports distances — a pair of bare extremes needed a month to be worth printing,
where "0.1% off yesterday's close" is a reading in its own right.

The percentages are distances, not returns — measured against the ends of the
band rather than against a baseline. A band with an end at or below zero, which
a formula that subtracts can produce, shows the levels alone. The move above the
chart is a percentage for a ratio as well: `−0.353017` is the number a reader has
the least chance of sizing on sight, having no currency and no habitual magnitude
to weigh it against.

Either way, a period the series doesn't cover says **"not enough history"**
rather than quietly measuring a young listing from its first day. A range is
only reported once the symbol existed for the whole window — every close a
symbol listed last month has falls inside the last ten years, and calling their
high a ten-year high would be a fabrication.

The series comes from the quote source, not from the sparkline history stored
locally — that is pruned to a window measured in hours, so it can say what a
symbol did today and nothing longer. Closes are adjusted for splits and
dividends where the source reports them, and returns are measured from the last
close *on or before* each period's start, because markets are shut at weekends.
"All time" is as far back as the source goes; to keep a forty-year chart drawable
on a phone, closes older than two years are thinned to one a week and then one a
month, with the high and the low always kept so the chart never contradicts the
table beside it.

**Composites** — a row can be a *formula over other symbols* instead of a
symbol. Type `VTI/GLD` into the same box you would type `AAPL` into and you get
a ratio: stocks against gold, priced every cycle from both legs. Composite rows
are outlined in violet and carry a `composite` chip; everything else about them
is identical to an ordinary row — sparkline, change against the previous close,
pin, pause, reorder, publish.

- `+ - * /`, brackets and plain numbers all work: `P/VTI`, `(VTI+GLD)/2`,
  `BTC-USD/GLD`, `VTI*2 - GLD`.
- Write a subtraction **with spaces** (`VTI - GLD`). An unspaced hyphen belongs
  to the symbol, because `BTC-USD` is one.
- A leg does not have to be on the watchlist. It is fetched for the formula and
  never becomes a row of its own; a leg that *is* on the watchlist is not
  fetched twice.
- Composites are published like anything else, under the formula as the key
  (`"VTI/GLD": "0.9478"`). They get more decimal places than a fetched price —
  a `P/VTI` of 0.0335 rendered `"0.03"` would be useless. Nothing about the
  keys existing consumers already read changes.
- If a leg can't be priced, the row says which one and why, the same way a
  broken symbol does.

**Portfolios** — what an allocation would have done, and what it is worth now.
Every saved portfolio also appears on the watchlist as a live row (above), so it
is priced every cycle and published with everything else. Give it a set of symbols
and weights adding up to 100%, an initial amount, and a rebalancing cadence, and
it compounds the mix month by month from the quote source's own closes. Those
closes are adjusted for splits and dividends, so distributions are already
reinvested — this is a total return, not a price chart.

- **Growth** from the initial amount, with an optional **benchmark** symbol run
  at 100% alongside it. Both sides are cut to the period they share, so the two
  curves can be read against each other.
- **CAGR, volatility, best and worst calendar year, and the deepest
  peak-to-trough fall** — with the months it ran between and whether it was ever
  recovered.
- **Sharpe and Sortino**, measured against `^IRX` (the 13-week Treasury bill).
  Both are omitted rather than computed against a silent 0% when that series
  can't be fetched.
- **Periodic contributions** — an amount and a cadence. Returns stay
  time-weighted, so money paid in never counts as growth; what went in is shown
  as its own row instead.
- **Dividend yield per year**, the cash actually distributed over what the
  portfolio was worth when the year opened. It needs a quote source with a
  payout feed; without one the column is absent rather than zero.
- **Every calendar year**, with the first and last labelled *part year* where
  the run doesn't cover them end to end.
- **Holding performance** under those years: every holding, what it returned on
  its own, and how far that was ahead of or behind the portfolio — over the year
  to date, one, three, five or ten years, or the whole run. Sortable by symbol,
  weight or return, because which rows matter is your question: a 2% position
  that halved is a footnote where a 40% one that halved is the whole story.
  Each period is measured back from the run's *last* month — on a run told to
  end in 2019, "year to date" means 2019's, from the December before — and one
  the run doesn't cover end to end is offered but not measured, for the reason
  the performance sheet withholds a window. **The benchmark is one of the
  rows**, marked and weightless: sorted by return it lands in the ranking, so
  "which of these beat the index" is a line across the table rather than
  arithmetic over every row.
- The run starts at the latest month all of its holdings have data for, whatever
  start year you ask for, and **names the holding that reaches back the least**
  — after any replacement, since a stand-in changes the answer. A four-fund
  portfolio asked for 1985 will start in 1996 and say which fund launched then.
  The shortest holding is named even when it isn't what set the start, because
  it is how far back the portfolio *could* go and so what decides whether
  another replacement is worth adding.
- **Replacements for historical data** — give a holding a stand-in symbol and
  the stand-in's returns cover every month before the holding's own history
  begins. `QQQ` behind `HOOD` turns a five-year run into a twenty-five-year one.
  The stand-in is scaled to meet the real series exactly where it starts, so
  nothing invents a jump on the splice date, and the result always names the
  substitution — a proxy nobody was told about is a fabrication. The benchmark
  is never substituted.
- **Sector allocation** at the bottom: a pie of where the money actually is,
  and one beside it for every fund you name in **Compare against** — which
  starts at the portfolio's benchmark, since that is the comparison you have
  already chosen for every other number on the page. Clear the box to see the
  allocation on its own; up to four comparisons are drawn, and any past that are
  named as dropped rather than silently missing.
  - It is a **look-through**, holding by holding: each fund's own breakdown
    scaled by what it is held at. Two 60/40s built from different index funds
    can be ten points apart in technology, and nothing else on the page would
    say so.
  - **The slices do not add up to 100, and are not made to.** Each pie says what
    share of it got a sector at all, and draws the rest in grey. A holding
    nothing can be said about — gold, cash, a bond fund, a currency pair — is
    named under the pies rather than folded into the gap, because "40% of this
    is gold" is the allocation, not a hole in the data.
  - The breakdown is what the source says **today**, so nothing here feeds a
    return: it describes the allocation as it stands and never a period.
  - A colour means one sector everywhere, whichever pies are on screen and
    however big the slice is in each — that is what makes two of them
    comparable. The exact figures sit under the pies in a table that is also
    the legend — three of the eleven fills are too pale to carry a number on
    their own against a light background, so the figures are text.
- Holdings need not be on the watchlist, and a holding you also chart costs no
  extra request — the daily series is shared with the performance sheet.
- **Rebalancing** happens on calendar boundaries (December for annual), not
  twelve months after the run began. *Never* lets the weights drift, which over
  decades is a materially different portfolio from the one it started as.

Nothing here models fees, taxes or spreads, and there is no inflation
adjustment: it is what the allocation did, not what an account holding it would
have.

**Funds** — look through an ETF. Type a symbol (or open `#/funds/QQQ` directly)
and the fund is run as a one-holding portfolio: the same growth chart, summary
table and calendar years as above, all of them the fund's own adjusted series
and none of them derived from what it holds. Under them, its top holdings over
the same windows, in the same sortable table, each with its weight in the fund
and its gap to the fund's own return.

Nothing here is saved. A fund is opened, not added — the symbol is in the URL,
so a page is something you can send somebody, and it costs no row, no setting
and no schedule.

- **The holdings are today's**, stamped with when they were read, and the card
  says what share of the fund they add up to. That matters most over long
  windows: ten years of a fund's *current* holdings is a story about the
  companies that are in it now, not about the fund, and the page says so instead
  of leaving you to infer it. The fund's own half of the page is unaffected —
  that is why the two halves come from different places.
- **Each holding carries its company name** under its symbol. A fund holds what
  it holds, so half a top ten is tickers nobody could be expected to read —
  `CCO.TO`, `028260.KS` — and the name is what turns the table into something
  about companies. The source gives it with the weight, so it costs nothing.
- **A holding too young for a window is named**, not quietly dropped. Ask for
  ten years of a fund holding something that listed in 2020 and the table says
  which names are missing and why, rather than being silently shorter.
- **The run is never shortened to the youngest holding.** A fund that started
  holding something last year has not existed only since last year — unlike a
  portfolio, whose legs are all held at once and are intersected.
- **Holdings the source can't price** — cash lines, foreign listings — are
  listed on their own rather than dropped, because they are part of the fund
  and part of no number above.
- **The same sector card as a portfolio's**, with the fund as its subject —
  a fund is one holding at 100%, so it is the same look-through. It defaults to
  comparing against whatever the page is benchmarked to.
- Series are shared with the performance sheet and with backtests, so a fund
  holding something you already chart costs no extra request.

The holdings and the sector breakdowns both come from Yahoo's `quoteSummary`,
which — unlike the endpoint everything else uses — needs a session cookie and a
crumb. All of that is confined to these two features: it is done lazily,
reused, and retried once when the crumb expires, and a source that stops
answering costs you those cards and nothing else. Yahoo reports a fund's **top ten** holdings, so that is what the
page shows; the rest of the fund is stated as a percentage rather than implied.
A quote source that can't answer at all leaves the page unavailable with a
reason, in the way the performance sheet does.

**Settings** — everything that configures a running instance, and the evidence
that it worked. The refresh interval, history retention, the quote source,
pinned symbols and logos are at the top; **Publishing** and **Recent cycles**
are the last two sections of the same page. `#/publishing` still works and
lands on the Publishing section.

The sections, in the order the page shows them:

- *Refresh loop* — how often symbols are fetched (a seconds field plus
  30s/1m/5m/15m/1h presets; 30s is the floor), how long price history is kept,
  and whether every refresh also publishes.
- *Pinned tickers* — the comma-separated symbols that sort to the top of the
  watchlist, up to 50. A symbol that isn't on the watchlist is ignored, so
  removing a ticker never means editing this too.
- *Symbol logos* — off by default, and only about *fetching*: uploading your own
  image on a row never needs it. On, the **server** fetches a logo per symbol
  and caches the image in the database, so it is fetched once and your browser
  never talks to anyone else. Symbols with no logo — funds, crypto pairs,
  composites, portfolios — keep the drawn mark. A cached answer is re-checked
  **once a day**, and the check asks the source *whether the image has changed*
  rather than downloading it again — so a day's re-checking of a whole watchlist
  normally transfers nothing at all. Turning the setting off
  empties the fetched cache; **uploads are never touched**, by that or by the
  daily refresh.

  **Logo URL** is where the pictures come from, with `{symbol}` standing in for
  the ticker (`{symbol_lower}` for the lower-case form). Left blank it uses
  whatever the quote source itself offers, which for Yahoo is a logo on *some*
  search results and nothing at all for most symbols — so if you want logos on
  everything, point this at a source that answers by ticker. Changing it clears
  the cache. Under the checkbox, Settings reports how many symbols have a logo
  and why the rest don't, which is how you tell "this fund hasn't got one" from
  "that URL is wrong".
- *Quote source* — the **server URL** prices come from, the request timeout,
  and the User-Agent sent upstream. Leave a field blank to fall back to the
  default, which the field shows as its placeholder. **Test connection** fetches
  one symbol right then and reports the price or the exact error — the fastest
  way to tell a wrong URL from a blocked network from a bad symbol.

Below those, a read-only **Server** card shows what the process was started
with: listen address, database path, whether the client is embedded or served
from disk, and the quote settings actually in force.

**Publishing** — where snapshots go. A destination is a base URL, a key, an
optional category, and a format. After every refresh the snapshot is
`PUT {base}/{key}`; if that fails (typically a 404 because the entry doesn't
exist yet) it is `POST {base}`. The section shows a live preview of exactly what
a destination receives, and **Test** sends the real payload to one destination
on demand.

Two formats:

| Format | Payload |
|---|---|
| `minion` (default) | `{"VTI": "295.50", "VTI/GLD": "0.9478", "BTC-USD": "N/A", "timestamp": "08/07 14:03:22"}` — the original script's shape, for existing consumers |
| `detailed` | per-symbol objects with `price`, `previousClose`, `change`, `changePercent`, `currency`, `status`, plus an ISO timestamp |

Below those, **Recent cycles** — the newest refresh cycles, with per-symbol
counts, which verb each destination accepted, and the failures in full. It sits
here rather than on a page of its own because almost every question it answers
("did it go?", "why didn't it?") is about a destination listed above it.

It opens on the newest 25 and **Show 25 older** extends it, up to the 500 the
server keeps. The window is always "the newest N" rather than a page number:
the log is appended to while you read it, and a page number would quietly mean
something different each time it refreshed. Opening it deeper sticks until you
leave the page.

Everything in Settings takes effect on the next cycle. Nothing needs a restart.

## Configuration

Almost everything is configured in the GUI and stored in the database: the
watchlist, the publish destinations, the poll interval, and the quote source.
The flags below are what has to be decided before the process starts.

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `--port` | `PORT` | `8797` | listen port |
| `--host` | `HOST` | `0.0.0.0` | bind address |
| `--db` | `TICKERS_DB` | `./data/tickers.sqlite` | SQLite file path |
| `--web-dist` | `WEB_DIST` | — | serve the client from this directory instead of the embedded copy |
| `--verbose` | `TICKERS_VERBOSE` | off | log every API request |
| `--quote-base-url` | `TICKERS_QUOTE_BASE_URL` | Yahoo's | quote API root — *overridable in the GUI* |
| `--quote-timeout` | `TICKERS_QUOTE_TIMEOUT` | `20` | seconds per quote request — *overridable in the GUI* |
| `--quote-user-agent` | `TICKERS_QUOTE_USER_AGENT` | a browser string | *overridable in the GUI* |
| `--archive` | `TICKERS_ARCHIVE` | — (the quick start sets it) | market-data archive folder — *overridden by the one chosen in Settings* |
| `--universe-url` | `TICKERS_UNIVERSE_URL` | Nasdaq Trader | where the exchange symbol lists are read |

For the first five: **flag > env > default**.

The three quote-source flags are a *fallback*, not a fixed value — they exist so
a systemd unit can be templated, but the Settings page wins:
**stored setting > flag > env > built-in default**. Clearing the field in the
GUI reveals the flag again; clearing both reveals the built-in default.

The listen address and database path deliberately stay out of the GUI: a web
app that can change the port it is served on is a web app that can lock you out
of itself. They're shown read-only on the Settings page so you don't have to go
read the unit file to find them.

There is also a one-shot mode, which is the original script's job exactly:

```bash
tickers publish --db /var/lib/tickers/tickers.sqlite   # fetch, publish, exit
```

Useful from cron on a host that would rather not run a daemon. It exits
non-zero if every symbol failed.

## The market-data archive

Tickers keeps a long-term archive of OHLCV bars for the whole listed US
market, plus anything else you add, for analysis: candles, moving averages,
backtests. The archive is a folder, separate from the watchlist's database.
It is configured in **Settings → Market-data archive** and followed on the
**Data** page.

**It is on by default.** The quick start creates `/var/lib/tickers/archive`
and collection starts with the service. `TICKERS_ARCHIVE_DIR=/some/folder`
starts it somewhere else, and `TICKERS_ARCHIVE_DIR=none` installs without one.
A plain `tickers serve` has no archive until a folder is chosen in Settings (or
passed with `--archive`).

**On a Pi, move it off the SD card.** Minute bars for the whole market come to
about 60 GB a year. Mount an external drive under `/mnt` or `/media` and create
a folder on it (DEPLOYMENT.md has the steps). Then, in Settings:

1. enter the folder's path and press **Check folder**;
2. press **Move the archive here**.

The move copies everything and then switches. The quick start's service is
allowed to write under `/mnt` and `/media`, so nothing else needs changing.

**Switching it off.** Untick **Collect market data** in Settings and save. The
archive closes and every request stops. Charts and backtests read from Yahoo
as they did before the archive existed. Nothing stored is deleted, and ticking
it again carries on where it stopped. **Paused** is the lighter version: the
archive stays open and readable, but nothing new is fetched.

- **The folder must already exist.** The app never creates it. An unplugged
  drive leaves its mount point behind as an empty folder on the SD card, and
  the app must never write there.
- **If the drive goes missing, collection pauses.** Settings and the Data page
  both say so, and collection resumes by itself within half a minute of the
  drive coming back.
- **Everything else keeps working meanwhile.** The watchlist, the performance
  sheet and backtests fall back to Yahoo for as long as the archive is gone.

**What it collects.** It collects every stock and ETF listed on Nasdaq, NYSE,
NYSE American, NYSE Arca, Cboe BZX and IEX: roughly ten thousand symbols, read
daily from Nasdaq Trader's symbol directory. It also collects bitcoin, ether
and the major indices, and everything the app itself uses. The app's own
symbols (watchlist rows, composite legs, portfolio holdings) always go first in
line. Symbols you add on the Data page do too.

| Width | From Yahoo | Kept current |
|---|---|---|
| `1d` | back to the listing date | daily |
| `1m` | the 30 days Yahoo keeps | daily (every 15 minutes for the app's symbols) |
| `5m` | the 60 days Yahoo keeps, once | built from `1m` from then on |
| `1h` | the two years Yahoo keeps, once | built from `1m` from then on |

Coarser intraday bars are built from finer ones when they are read, so minute
bars are the only intraday series kept current. Intraday history older than
Yahoo keeps can only come from a paid source.

**Adding a paid source.** Paste a Polygon.io API key in Settings and give
your plan's history (in years) and its per-minute request limit. Polygon now
also goes by Massive.

- **It only fills gaps.** It walks each series backward and skips the days the
  archive already holds, at no request cost. It then fetches only the years
  Yahoo never had.
- **Every bar records its source.** A source can revise its own bars, but never
  overwrites another source's unless you ask.
- **To trust it over Yahoo for a range:** open the symbol's page and use
  **Fetch a range now** with *Replace what's there* ticked.
- **Its bars match Yahoo's.** They are split-adjusted and cover the regular
  session only (9:30–16:00 New York).
- **The free tier is enough to try it.** Two years of history at five
  requests a minute.

**How fast.** Yahoo is asked once every two seconds, about 1,800 times an hour
(adjustable). A 429 pauses that source, starting at a minute and doubling to an
hour. Each source is paced separately. Keeping ten thousand symbols current
takes about half of each day's requests, and the backfill uses the other half.
The order is:

1. anything close to losing bars off the edge of Yahoo's history;
2. the app's own symbols;
3. keeping every series current;
4. first fetches;
5. digging history out breadth-first, so every symbol gets its second decade
   before any gets its third.

**Settings → Market-data archive** holds everything that configures it:

- **The switch.** On or off, and paused or collecting.
- **What to collect.** Which widths, the whole listed market or not, and
  extras.
- **Limits.** Yahoo's request spacing, and the free-space floor.
- **Polygon.**
- **The folder.** Check one, use one, or move the archive into one.

**The Data page** shows what came of it:

- **Overview:** how much is held, per width and in total, disk use and
  collection rate.
- **Symbols:** a searchable browser over every symbol. Each symbol has a page
  with a candle chart, a month-by-month coverage heatmap per width, each
  source's progress, and actions: put it first, stop collecting it, walk it
  again, or fetch a range from a chosen source.

**What is stored.** Every bar has an open, high, low, close and volume. Prices
are as the source prints them: split-adjusted, not dividend-adjusted.

- **VWAP and trade count.** Each bar also carries the source's own
  volume-weighted average price and trade count where it gives them. Polygon
  does and Yahoo doesn't, so they are there for the stretches a Polygon key
  filled in.
- **Extended hours are opt-in.** *Pre-market and after-hours bars too*, in
  Settings, collects 4:00–9:30 and 16:00–20:00 New York time as well. Each bar
  is tagged with its session. Charts, returns and sparklines stay regular hours
  unless asked; the symbol page's chart has an *Extended hours* toggle.
- **Splits and dividends** are stored alongside the bars, and a new split
  rescales the bars already stored (prices, VWAP and volume). The Treasury
  yield curve (`^IRX`, `^FVX`, `^TNX`, `^TYX`) is collected with the default
  extras, for risk-free rates.
- **Delisted symbols** stop being fetched, but their history is kept.
- **Renames are joined, with a Polygon key.** Each day's newly listed symbols
  are checked for a former name. When FB becomes META, reading META includes
  the bars collected as FB before the rename.

**Indicators.** A symbol's page on the Data page charts its candles, with
volume under them, and indicators you switch on with a tap:

- **Over the price:** SMA (20, 50, 200), EMA, Bollinger Bands, and VWAP on
  intraday charts.
- **In panels below:** RSI, MACD, Stochastic, ATR and On-Balance Volume.
- **Custom periods:** *Add* takes any parameters, such as `SMA 100`, `MACD 8,
  21, 5` or `Bollinger 20, 2.5`. Your choice is remembered in this browser.

The server computes them from the archive's bars, and reads extra history
before the chart's first bar, so a 200-day average is already settled at the
left edge rather than starting 200 bars in. Where the archive doesn't reach
that far back, the line starts later rather than being averaged over fewer
bars. The conventions are the usual ones, so the numbers match other charting
tools: EMAs are seeded with an SMA, RSI and ATR use Wilder's smoothing, and
VWAP resets each session.

The same numbers are available to scripts from
`/api/archive/symbols/{symbol}/bars?interval=1d&from=…&to=…&ind=sma:200,rsi,macd:12:26:9`.

**How big.** About 60 bytes a bar. One-minute bars for the whole market are
around 60 GB a year; daily history for the whole market is a few GB once.
Collection pauses before the drive's free space falls below a floor (10 GB by
default). Nothing is ever deleted to make room.

**What reads it.** The performance sheet and backtests read a symbol's daily
series from the archive, plus one small request for today, once some source has
walked it back to its listing. Before that they read from Yahoo, as they always
have. Sparklines use the archive's minute bars, followed by the refresh loop's
own newest readings.

From the command line:

```bash
tickers serve   --archive /mnt/usb/tickers-archive   # a folder to use until one is chosen in Settings
tickers collect --db ./data/tickers.sqlite           # the collector alone, with the Settings page's settings
tickers coverage --db ./data/tickers.sqlite          # how far it has got
```

The archive is plain SQLite:

- `catalog.sqlite` holds the symbols (`symbols`), splits and dividends.
- Each bar file has a `bars` table. Daily bars are in `bars/1d/all.sqlite`,
  and intraday bars in one file per width per year, such as
  `bars/1m/2026.sqlite`.
- Daily bars are keyed by date (midnight UTC); intraday bars by the instant
  they opened.

A finished year's file never changes again, so it only needs backing up once.

```sql
ATTACH 'catalog.sqlite' AS c;
SELECT datetime(b.ts, 'unixepoch') AS day, open, high, low, close, volume
  FROM bars b JOIN c.symbols s ON s.id = b.symbol_id
 WHERE s.symbol = 'AAPL' ORDER BY b.ts;          -- run against bars/1d/all.sqlite
```

## Versioning

**`vYEAR.MONTH.PATCH`** — a calendar version, where **the patch number is the
repository's commit count** — every commit is a patch release, so `v2026.8.42`
is the 42nd commit on the 2026.8 line. It's shown in the app header, printed by
`tickers version`, and returned by `/api/health`.

There is no semantic major/minor: the leading numbers say *when* a release line
opened, not what it promises about compatibility. The one compatibility promise
this project makes — the published payload's format — is pinned by
`internal/publish`'s tests, not by a version number, and anything that would
break it is called out in [`CHANGELOG.md`](./CHANGELOG.md).

- `YEAR`/`MONTH` are constants in
  [`server/internal/version/version.go`](./server/internal/version/version.go).
  Bump them by hand when a release line opens — they are deliberately not read
  from the build clock, so rebuilding an old tree still reports what it
  originally shipped.
- The month is not zero-padded (`v2026.8.42`, not `v2026.08.42`): semver forbids
  a leading zero, and an unpadded month keeps every tag something a semver
  parser will accept.
- `scripts/version.sh` is the one place that assembles the whole string, reading
  those constants so nothing can disagree about them.

```bash
scripts/version.sh            # v2026.8.42
scripts/version.sh --patch    # 42
```

A build with no git — a tarball, or a **shallow clone** — reports patch `0`.
That's deliberate: `git clone --depth 1` answers `rev-list --count HEAD` with
`1`, which isn't an error and isn't obviously wrong, it just quietly ships a
build calling itself `v2026.8.1`. Patch `0` is the agreed "unstamped development
build" marker, it matches the Go default, and the release workflow refuses to
publish one. Anything building a release needs the full commit graph:
`fetch-depth: 0` in Actions, and `--filter=blob:none` rather than `--depth 1`
for a cheap clone that still carries all of it (which is what
`scripts/quickstart.sh` does, deepening an old shallow checkout if it finds
one).

Because the tag is determined by the commit rather than chosen, releasing is:

```bash
git tag "$(scripts/version.sh)" && git push origin "$(scripts/version.sh)"
```

The workflow refuses to publish if the tag doesn't match the version the commit
builds.

## Testing & checks

```bash
cd server
go vet ./...
go test -race ./...     # store, quote parsing, publishing, the engine, the API
```

The suites under `server/internal/` are the authority on behaviour. In
particular `internal/publish` pins the legacy payload: if a change would alter
what an existing consumer receives, those tests fail.

## Documentation

- [docs/DESIGN.md](./docs/DESIGN.md) — architecture, schema, the REST contract
- [DEPLOYMENT.md](./DEPLOYMENT.md) — deploying, upgrading, backup and restore
- [server/README.md](./server/README.md) — the Go server and its CLI
- [CHANGELOG.md](./CHANGELOG.md)

## Credits

Built by **CM Hegday** ([github.com/chinmay28](https://github.com/chinmay28)) —
tap the developer mark in the app header to see the badge.

## License

Tickers is free software licensed under the **GNU Affero General Public License
v3.0** (`AGPL-3.0-only`). See [LICENSE](./LICENSE) for the full text.

The AGPL is a strong copyleft license: anyone who distributes Tickers — or
**runs a modified version as a network service** — must make the complete
corresponding source available under the same license.

> **Note for operators (AGPL §13):** if you run a modified Tickers server that
> other people interact with over a network, you must offer those users the
> corresponding source of your modified version.

Market data comes from Yahoo Finance's public endpoints. It is provided for
personal, informational use; it is not a licensed market-data feed, and it
carries no warranty of accuracy or timeliness. Don't trade on it.
