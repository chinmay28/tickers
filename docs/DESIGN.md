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
tickers(id, symbol UNIQUE, expression, portfolio_id, label, position, enabled, origin, …)
quotes(ticker_id PK → tickers, symbol, price, previous_close, currency,
       short_name, market_state, status, error, fetched_at)
quote_history(id, symbol, price, at)
logos(symbol PK, status, origin, content_type, bytes, source, reason, fetched_at)
sinks(id, name, base_url, key, category, format, enabled, timeout_ms, …)
portfolios(id, name, allocations, initial_amount, start_year, end_year,
           rebalance, benchmark, position, created_at, updated_at)
runs(id, started_at, finished_at, trigger, ok_count, error_count, publishes, error)
settings(key PK, value)
schema_migrations(id PK, applied_at)
```

A few decisions worth naming:

**Pinned symbols live in `settings`, not on the ticker row.** The pinned list
is one comma-separated `settings` value holding *symbols*, and every query that
returns a ticker stamps a derived `Pinned` flag onto it. Keying on symbols
rather than ticker IDs is what makes the setting survive removing and re-adding
a row, and what makes it something a person can read and edit in a text field.

It is a **set, not an ordering**: the sort that lifts pinned rows is stable and
keys on nothing but pinned-ness, so `position` still decides the sequence
*within* each group. Pinning a row therefore never takes drag-to-reorder away
from it, and unpinning drops it straight back into the slot it would otherwise
have had. Ordering the pinned group by the settings list instead would have
made dragging a pinned row do nothing — with the shipped watchlist pinned by
default, that is every row on a fresh install.

**A watchlist row is one of three things, and the columns say which.** An empty
`expression` and an empty `portfolio_id` is a fetched symbol. A non-empty
`expression` is a composite. A non-empty `portfolio_id` is a saved allocation's
live value.

The third one is a column rather than another expression, and that is the whole
reason it exists as a separate concept: a composite's symbol *is* its formula,
and a portfolio's symbol has to be its **name**, because that is the key a
downstream dashboard reads the value under. `0.0353*VTSMX+0.0164*VGTSX+…` is a
correct formula and a useless payload key — and at twenty holdings it would
also blow through `expr.MaxLen`.

The row is a **holding, not a target**. Its units — the weight's share of the
initial amount over the price when the allocation was last set — are stored on
the holdings, so the row starts at exactly the initial amount and the weights
drift from there as a real account's would. Recomputing them every cycle would
make the row silently rebalance itself every thirty seconds, which is a
different portfolio and not one anybody asked for; rebalancing belongs to the
backtest, where there is a period to rebalance over.

Recomputing them re-bases the row and throws away whatever it had grown by, so
it happens only when the **allocation or the initial amount** moved. That cannot
be decided from which fields the patch carried: the editor posts the whole
allocation on every save and posts it *without* units, so "holdings were in the
patch" is true even for a rename. `carryUnits` compares what the units actually
depend on instead, and it is all-or-nothing — a row holding some old units and
one freshly priced would be worth neither the initial amount nor what it had
grown to. `LinkPortfolio` then prices only the holdings whose units are missing,
which also means a rename makes no upstream request at all.

Two consequences worth naming. A portfolio's row **fails whole**: three quarters
of an allocation is not three quarters of its value, it is an unknown amount,
so one unpriceable holding makes the row an error that names it. And deleting a
portfolio **deletes its row in the same transaction** — left behind it would be
an unpriceable symbol nothing knows how to value, which an older binary rolled
back onto the database would try to fetch from the provider every cycle forever.

**A portfolio's allocation is one JSON column, not a child table.** Nothing
joins a holding to anything — a portfolio leg is fetched the way a composite's
leg is, and need not be on the watchlist — nothing queries across portfolios by
symbol, and the whole list is written and read as a unit. That is the same shape
`runs.publishes` already has, and a `portfolio_holdings` table would have bought
a join and a second set of ordering rules for none of it.

**A composite is a ticker with an `expression`, and nothing else.** A row whose
`expression` is non-empty is priced by evaluating that formula over other
symbols (`VTI/GLD`) rather than by fetching its own symbol; an empty expression
— every row that existed before the feature — means an ordinary ticker. That is
the entire distinction, and it is deliberately the *only* one: composites reuse
the same table, the same quote row, the same history series, the same publish
path. Everything downstream of "a row has a number" therefore worked on day one
without knowing composites exist.

A composite's `symbol` is **derived** from its expression — the canonical
formula with the spaces taken out, so `vti / gld` and `VTI/GLD` are the same
row and collide on the existing unique index. Deriving it rather than asking
for one is what gives a composite the same stable, unique, publishable key
every other row has: history keys on it, the payload keys on it, the pinned
list keys on it. The expression is stored separately in its canonical *spaced*
form, because that is the only form guaranteed to re-parse (see below).

**`origin` is provenance now, nothing more.** A fresh install seeds the seven
symbols from the script with `origin = 'seed'`, and changing a row's symbol
promotes it to `origin = 'user'`. Nothing reads it at runtime — it is there for
whoever opens the database, and it is what migration `002` used to work out
which symbols to pin on an install that predates pinning. It used to drive a
`placeholder` chip and a **Replace** button; that chip is now the `pinned` one,
and what it reflects is a setting rather than where the row came from.

**Seeding is keyed off a settings flag, not "is the table empty".** Otherwise
deleting every ticker would resurrect the author's watchlist on the next
restart — the opposite of what someone who just cleared the list wants. The
same reasoning sets the *default* pinned list from `seed()` rather than from
`DefaultConfig()`: an unset key and an empty one have to mean different things,
or unpinning everything would read back as the shipped defaults on the next
load.

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

## Composite formulas

`internal/expr` is a ~350-line recursive-descent parser over a deliberately
tiny grammar:

```
expr   := term (('+' | '-') term)*
term   := factor (('*' | '/') factor)*
factor := '-' factor | '(' expr ')' | number | symbol
```

Length and nesting are both capped, because the input is a text field and
unbounded recursion on parentheses is a stack overflow waiting for a fuzzer.

**The hyphen is the only genuinely ambiguous character**, and it is worth
naming because it is where a naive tokeniser breaks: `-` is subtraction, and it
is also a character in `BTC-USD` and `BRK-B`. The lexer resolves it
positionally — a hyphen wedged between two symbol characters with no space
belongs to the symbol; a hyphen with a space on either side is the operator. So
`BTC-USD/GLD` reads as one ratio and `VTI - GLD` reads as a difference, which
is what someone typing either of them meant. The cost is that `VTI-GLD` lexes
as a single unpriceable symbol, which is why the error names the symbol it
could not price and why the UI asks for spaces around a minus.

**Parse returns two renderings.** `String()` is canonical with spaces around
`+`/`−` so it re-parses to the same tree — that is what gets stored and shown
back in the edit box, and `expr_test.go` asserts the round trip, because a
canonical form that drifts would rewrite a row every time you saved it
untouched. `Key()` is the same thing with the spaces removed, and becomes the
row's symbol.

**Typing a formula into the symbol field is the same as giving an expression.**
`expr.Looks` decides, and the store promotes one to the other. No provider has
a symbol containing `/`, `*`, `+` or brackets, so there is nothing else the
input could mean, and a mode switch in front of it would be ceremony. A bare
hyphen deliberately does not count.

**Evaluation happens in the refresh cycle, twice per composite** — once against
the fetched prices and once against the fetched previous closes. The second is
what gives a ratio a change and a change percentage, and it is best-effort: a
composite with no previous close still shows a price. A missing leg comes back
as a typed `*expr.MissingError`, which the engine swaps for that leg's own
provider error, because "no price for GLD" is a worse answer than the 404 that
explains why.

## The quote source

Prices come from **Yahoo Finance's public HTTP endpoints, called directly**.
There is no `yfinance` here — that is a Python library, it went with the rest
of the interpreter, and dropping it is most of why the deployable artifact is
one file. Two plain GETs are the whole integration:

| | |
|---|---|
| Quotes | `GET {base}/v8/finance/chart/{SYMBOL}?range=1d&interval=1m` |
| History | `GET {base}/v8/finance/chart/{SYMBOL}?period1=…&period2=…&interval=1d` |
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

### Past prices are a separate, optional capability

`Historian` is a second interface next to `Provider`, implemented by Yahoo and
type-asserted at the call site, exactly like `Configurable`. The refresh loop
never needs a past price, so requiring one of every provider would be taxing
the common case for the rare one; a source that can only price today simply
doesn't implement it, and the performance sheet gets `ErrNoHistory` and a
sentence to show (`501`, because it is a capability that is absent rather than
a request that is wrong).

Four decisions inside it, each with a failure it prevents:

- **The series is fetched, not read from `quote_history`.** That table holds one
  point per refresh cycle and is pruned to a retention window measured in
  hours: it is the sparkline's, and it can never answer "what has this done in
  five years". They are different questions with different sources, and
  conflating them would have meant retaining a year of one-minute samples on an
  SD card to answer a question the provider answers in one GET.
- **`period1`/`period2`, not a named range.** A ten-year return is measured from
  the close *before* ten years ago, and a named range starts on that boundary at
  best. The engine asks from the epoch — everything the source has, because
  "all time" has to mean it and a bounded window would make the longest row a
  restatement of wherever that bound fell.
- **Adjusted closes where they exist.** An unadjusted five-year chart of a stock
  that split 4:1 shows a crash nobody experienced. Raw closes are the fallback
  for instruments with no adjustments to make — currencies, crypto. A bar
  carries *both* series, because the one thing that needs the unadjusted price
  is a yield: a payout is per share in the money of its own day, and dividing a
  1998 dividend by a 1998 close since marked down by thirty years of
  distributions gives a yield several times the real one.
- **A bar is dated by the exchange's calendar, not by UTC.** `meta.gmtoffset`
  shifts each timestamp before the date is taken. A close in Auckland stamped
  in UTC lands on the previous day, and that date is the key a composite aligns
  its legs on.

`Distributor` is a third such interface, for dividends, and it is separate from
`Historian` rather than a method on it for the same reason `Historian` is
separate from `Provider`: a source can have prices and no payout feed — a
currency pair and a crypto ticker have nothing to distribute — and folding the
two together would leave such a source unable to offer history at all. Yahoo
implements it off the same chart endpoint with `events=div`, at a monthly
interval, because the events block does not depend on the bar interval and
daily would drag decades of prices along for a call that reads neither.

Composite history is the refresh cycle's composite pricing, run once per day
instead of once per cycle: fetch each leg, evaluate the formula against a map of
symbol to value. Days are **intersected across the legs** rather than carried
forward — a day one leg was shut and another moved would produce a ratio that
never existed, on exactly the days a reader is most likely to be squinting at.
Series are cached by *symbol* for ten minutes, which is both what stops a
repeated double-tap repeating the fetch and what makes a composite over symbols
already on the watchlist cost no extra requests.

### Returns, ranges, and what each is for

One window list, two projections of it.

A **return** is the natural reading of a price and a category error on a ratio:
there is no capital in `VTI/GLD` to have returned 8%, and printing that invites
someone to read a ratio as a holding. A **range** — the high, the low, and where
the latest value sits between them — says something true about either. So both
are computed for every ticker and the client picks: returns for a symbol, ranges
for a composite. Making that a payload decision instead would have meant the
API's shape depending on the row, for no gain.

The two disagree about where a window starts, on purpose:

- A return is measured from the **last close on or before** the start. It needs
  something to measure *from*, and the nominal start is usually a weekend.
- A range covers only closes **inside** the window. A high is only a high if it
  happened during the period being claimed for it.
- A range is reported only when the series was already running when the window
  opened. Every close a symbol listed three weeks ago has falls inside the last
  ten years; calling their high a ten-year high is the same fabrication as
  quoting that symbol a ten-year return.

Annualised rates come from the dates the baseline actually has, not from the
window's name — the "5 years" row is measured from a close five years and a few
days back, and "all time" has no nominal length to use.

### Thinning the chart

A symbol listed in the eighties has ten thousand daily closes: a quarter of a
megabyte on the wire, and ten thousand segments for a phone to rasterise into a
line a few hundred pixels wide. The last two years keep every session, because
that is what the short spans show; older stretches keep one close a week and
then one a month.

Two things follow, and both are load-bearing:

- **The high and the low always survive the thinning.** They are named in the
  ranges table, and a peak the chart cannot reach beside a number saying it
  happened is the kind of disagreement that makes a reader distrust both.
- **The chart's x axis is time, not index.** Mixed resolution makes those two
  wildly different — spacing by index would give the most recent two years a
  third of the width and squeeze thirty into the rest. Returns and ranges are
  computed from the *full* series before any of this, so thinning changes what
  is drawn and never what is claimed.

### Backtesting an allocation

A portfolio is simulated from the same daily series the performance sheet reads,
through the same per-symbol cache — which is the whole reason this cost so
little to add. A portfolio over funds somebody also charts fetches nothing extra,
and two portfolios sharing a fund fetch it once between them.

Everything about the simulation is **monthly**. Daily bars are what the provider
gives, but a portfolio's answer does not get more true at daily resolution: it
gets forty times bigger on the wire, and it starts implying the rebalance
happened on a particular Tuesday. Each month is reduced to its last session's
close, and because that close is the provider's *adjusted* one, dividends are
already reinvested — nothing here has to model a distribution.

The decisions worth naming:

- **Every leg is intersected, the benchmark included.** Same reasoning as
  composite legs: a month one holding didn't trade would otherwise contribute a
  return on one side of the portfolio and a flat line on the other, which is a
  rebalance nobody performed. For the benchmark it is stronger still — two
  curves drawn over different months cannot be read against each other, which is
  the only thing a benchmark is for.
- **The run starts where its latest holding does**, and says which one that was.
  The date alone leaves a reader to work out which holding to change.
- **Weights are renormalised to sum to exactly 1.** Three holdings of 33.33 are
  simulated as three exact thirds rather than as a portfolio quietly starting
  0.01% in nothing. The store still rejects weights that miss 100 by more than a
  rounding tolerance, because weights summing to 90 are a typo, not a position
  in cash — nothing here models cash.
- **Between rebalances the weights drift**, which is the point: a 60/40 left
  alone for a decade is a 78/22, and holding it at 60/40 would report a strategy
  nobody ran. Boundaries are calendar ones — December for an annual rebalance,
  not twelve months after the run happened to start — because that is when
  somebody doing this by hand would do it.
- **A part calendar year is shown and labelled**, at both ends. Dropping it
  loses real information; printing it unmarked claims a year the run never
  covered. Best and worst year use full years only, so a run starting in October
  cannot report its first three months as its worst year.
- **The drawdown row says whether it was recovered.** A depth and two dates
  without that leaves out the thing a reader actually wants.

`ErrBadSpec` covers every way a portfolio is un-runnable because of what was
asked for — an unknown symbol, histories that don't overlap, a window too short
for one monthly return — so the API can answer 400 for all of them. Telling
somebody their portfolio is a server error sends them to the logs for a problem
they can fix in the form.

#### Replacements for historical data

A holding can name a stand-in whose returns cover the months before its own
history begins — a broad fund behind a recent listing, so one four-year-old
holding doesn't truncate a thirty-year portfolio to four.

The graft is a **level shift, not an average**. The stand-in is scaled so it
meets the real series exactly at the month the real one starts, which leaves its
month-to-month returns untouched and the joined series continuous. Splicing the
stand-in's *prices* instead would invent a jump on the splice date and report it
as a return — the test pins a case where doing so would turn a ×4.4 run into an
×8.8 one.

Three rules around it, each preventing a specific dishonesty:

- **Every substitution is disclosed**, as a row in a Replacements table naming
  the holding, the stand-in and the month the real data takes over. It started
  as a sentence each, which reads fine for one and becomes five near-identical
  paragraphs above the chart for a portfolio that leans on them — at which point
  nobody reads any of them, including the one line beside them that is not
  boilerplate. The engine emits the fields, not the prose; the client builds the
  table. The holding rows carry the configured replacement whether or not it was
  used, so "it wasn't needed" is distinguishable from "none was set".
- **The shortest history is reported after the substitution, not before.** A
  stand-in changes which series is actually holding the run back, and the answer
  people want is about the one that was used: "QQQ (standing in for HOOD) has no
  history before 1999-03" rather than a note about HOOD, which is not what has
  no history before 1999. It is named even when something else set the start —
  a start year, or the benchmark — because it is how far back the portfolio
  could go, and that is what decides whether another replacement is worth
  adding. The sentence carries no "because" in that case: a later start can come
  from the start year, the end year or the benchmark, and guessing which would
  be wrong about a third of the time.
- **The benchmark is never substituted.** It answers "what would the plain index
  have done"; a proxy stitched into it is not a benchmark. That is why the
  effective series live in their own map rather than replacing the raw ones.
- **An unanchorable stand-in is ignored, not guessed at.** If it has no value at
  the splice month there is nothing to scale against, and picking a factor would
  silently shift every earlier month by an unknown amount. The run falls back to
  the holding's own history.

Payouts are spliced in the stand-in's units — scaled by the *unadjusted* price
factor, so `value ÷ price × dividend` comes out as the stand-in's own yield on
the money held. Without that a proxied year would read as zero income rather
than as the income the stand-in actually produced.

#### Money and returns are two different series

A portfolio can be paid into on a cadence, and that breaks every return figure
unless they are computed somewhere else. Money paid in raises the balance
without anything having been earned; a CAGR taken off a balance topped up
monthly for thirty years is a spectacular number describing nothing.

So `simulate` keeps two series. **`balances`** is the money in the account.
**`index`** is the growth of a single unit with the cash flows removed — each
month's return measured *before* that month's contribution, against a balance
that already includes the last one, which is the time-weighted return. Every
percentage comes off the index; every amount comes off the balances. With no
contributions the two are proportional and the distinction costs nothing, which
is what keeps the lump-sum case exactly as it was.

Drawdown is on the index for the sharper version of the same problem: a
portfolio paid into every month can be halving while its balance still climbs,
and a drawdown row papered over by deposits is worse than no row at all. The
summary shows what went in as its own line, because a balance four times the
initial amount beside a total return of 40% is otherwise a puzzle with no
visible answer.

Contributions land on the same calendar boundaries rebalancing uses, and the
two share a vocabulary because they share a meaning. One asymmetry: a
contribution in the final month is kept where a rebalance is dropped. Moving
money between holdings at the closing bell changes nothing and would still count
as a trade; paying money in changes the balance, and it genuinely went in.

#### Risk-adjusted, and income

Sharpe and Sortino are measured against `^IRX`, the 13-week Treasury bill. It is
a constant rather than a setting because it is not a preference — a Sharpe
computed against something else is a different number wearing the same name.
Two conversions inside it are worth knowing: the bill is quoted as an
*annualised percentage* rather than as a price, so a month's share is the rate
over twelve; and the last known rate is carried **forward** across gaps, which a
price series may never do — a rate is a level that stays in force until it
changes, where a missing close is a day that did not trade. Carrying one
*backwards* is refused outright, so a bill starting after the run leaves the
ratios unset rather than inventing monetary policy.

Yield divides what was actually distributed by what the portfolio was worth when
the year opened. The shares held come from each holding's *money* and the
*unadjusted* price of the baseline month — weights have drifted by then, and
that is the point: income depends on what a portfolio actually held, not on what
it was aiming at. Payouts before the baseline month are excluded, which is what
keeps a part first year from claiming a full year of income, and the summary
averages full years only for the same reason best and worst do.

Both are best-effort and neither is a leg. The bill and the payout feed are
fetched after the months are settled, so a source that lacks them costs the
rows that depend on them and nothing else. The distinction the yield has to
keep is between **nil and zero**: nil is "this source cannot say", zero is "this
paid nothing", and collapsing them would report every income fund on earth as
yielding nothing.

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
2. Work out the symbols to fetch: the plain rows, plus every leg of every
   composite, deduplicated. A ratio over a symbol already on the watchlist
   costs no extra request, and a leg that isn't on it is fetched without
   becoming a row.
3. Fetch them concurrently (capped at 4 in flight — a domestic connection
   firing twenty at once collects timeouts, not quotes).
4. Store one quote per ticker, success or failure — fetched for a plain row,
   evaluated for a composite — and append a history point for each success.
5. Prune history past the retention window.
6. If `publishOnRefresh`, build the snapshot and send it to every enabled sink.
7. Append a `run` record.

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
so the cycle log can distinguish "updated" from "created" without guessing. When both
fail it reports **both** errors — being told only about the POST sends people
looking at the wrong endpoint, and the PUT's status is usually the one that
explains what the store actually wants.

The one place the payload does move is **composites**, and only because they
are new. A ratio published to the legacy two decimals is not a number anyone
can use — a `P/VTI` of 0.0335 rendered `"0.03"` has thrown away most of what it
said — so a composite gets 2, 4 or 6 places by magnitude. No pre-existing
consumer has ever had a `VTI/GLD` key, so nothing that was already being read
changes; `TestFetchedQuotesStayAtTwoDecimals` is the guard on that boundary.
Composite-ness is not stored on the quote row: `Engine.Snapshot` already joins
tickers to quotes, and stamps it there.

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
| GET | `/api/tickers/{id}/performance` | five years of daily closes + returns |
| GET/POST | `/api/portfolios` | list / add |
| PATCH/DELETE | `/api/portfolios/{id}` | edit / remove |
| POST | `/api/portfolios/{id}/backtest` | simulate a saved allocation |
| POST | `/api/backtest` | simulate one that hasn't been saved |
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
- `POST/PATCH /api/tickers` take `symbol` **or** `expression`; a `symbol` that
  reads as a formula is treated as one, so a client that only knows about
  symbols can still add `VTI/GLD`.
- Errors are `{"error": "…"}`. `404` for a missing row, `409` for a duplicate
  symbol, `400` for anything the caller can fix (including a formula that
  won't parse and an allocation that can't be simulated, each carrying its own
  message), `500` otherwise.
- The backtest endpoints are `POST` despite changing nothing. The first run of
  a portfolio fans out to one full-history request per holding, and a URL a
  browser is free to prefetch, retry or cache is the wrong shape for that. Both
  answer `501` when the quote source has no history at all — a configuration
  somebody chose, not a fault, the same way `/performance` does.
- `/api/state` carries saved portfolios but never their results. A backtest is
  slow and large; riding along with a ten-second poll would turn every redraw
  into a fan of upstream requests.
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

Redrawing the whole view every 10s is fine for read-only rows and hostile to
anyone typing into a form, so a redraw nobody asked for is not allowed to
happen under a cursor. Two rules cover it, and both are needed:

- **A background redraw waits while a field has focus.** Nothing is rebuilt
  under the cursor, so the caret, the selection and — on iOS, where a
  re-created input closes it — the keyboard survive. The redraw is owed, not
  dropped: it lands on `focusout`. The header, the footer and the offline
  banner sit outside the routed view and keep updating regardless, so a
  deferred redraw never means a stale status line.
- **Typed values are stashed by (form, field) and put back after any redraw
  that does happen.** Redraws the user *did* ask for — saving a ticker, opening
  an edit form, changing tabs — still land immediately, and they no longer
  empty the other forms on the page. A draft exists only once that field has
  been typed into, so restoring one can never shadow a fresh server value; it
  is dropped when the form is saved or cancelled.

The alternative — diffing, or keeping every form out of the re-rendered subtree
— is more machinery than a page of drafts, and it would have to be got right in
every view rather than once in `render`.

### Symbol marks

Wherever a symbol is listed — a watchlist row, a holding's chip, a search
result — it is preceded by a small rounded square. By default it is *drawn*
rather than fetched: two initials over a hue hashed out of the symbol, which is
free, offline, identical on every device, and available for the rows no logo
could ever cover. Real logos are an opt-in layer on top of it (below), not a
replacement for it.

The hue is drawn from a curated list rather than the whole wheel. Four hues are
already spoken for within a row's width: `--up` and `--down` on the same card,
`--composite` and `--portfolio` on its outline. A green mark beside a red
change, or a violet one on a row that isn't computed, is a colour contradicting
the one next to it. The hash is FNV-1a, not `hash * 31 + c`, whose low bits
barely move across four-letter symbols and would hand most of a watchlist the
same two hues.

Composites and portfolios get a glyph in their own hue instead of initials. A
composite's symbol is its formula and a portfolio's is its name; neither was
issued by anyone, and initials would say otherwise. The obelus and the pie
repeat what the row's outline already claims rather than making a new claim.

#### Real logos, when they are asked for

`Symbol logos` in Settings turns on fetching an actual logo per symbol. It is
off until somebody turns it on, because it is the only feature here that makes
an install talk to a host it otherwise wouldn't, about the symbols on its
watchlist by name. That is a decision for whoever runs it, not a default.

**The server fetches, never the browser.** A client-side `<img>` pointing at a
logo host is twenty lines and the wrong twenty: it tells that host what you
track from every browser that ever opens the page, it costs a request per row
per load, and it leaves a dashboard with no internet full of broken images. So
`quotes.Iconographer` returns *bytes*, the engine writes them into `logos`, and
`/api/logos/{symbol}` serves them from this app's own origin. The image is
fetched once per symbol, ever.

**Where the picture comes from is a setting, not a constant.** There is no
standard way to get a logo from a ticker: Yahoo has no logo endpoint, what it
has is a `logoUrl` on *some* search results and none at all for most symbols,
and the free services that do answer by ticker come and go. So `Logo URL` takes
a template with `{symbol}` in it and the provider fetches that directly; blank
falls back to the search-result path. This is the same argument as the
configurable user agent — the string that works upstream is a moving target,
and an install that can reach a working source should not have to wait for a
release. Changing the template **clears the cache**, because the previous
source's answers, including everything it said had no logo, describe a source
that is no longer being used.

**A re-check asks whether anything changed.** The ETag and Last-Modified the
last fetch returned are stored beside the image and sent back as
`If-None-Match` and `If-Modified-Since`, so the usual outcome of a daily
re-check is `304` and no body at all. A source offering neither is still
handled: the bytes that come back are compared with the bytes already stored.
Both paths end in the same place — the row's *checked* time moves and nothing
else does.

That distinction is why there are two timestamps. `fetched_at` answers "when did
we last look?", which decides whether a symbol is due a check; `updated_at`
answers "when did the picture last change?", which is what the client puts in
the image URL to get past a day of browser caching. With one column the two
uses fight: a re-check that found nothing new moved the version, and every
browser re-downloaded an image it already had — turning a feature meant to save
requests into one that generated them.

**A fetched answer stands for a day.** Logos change when a company rebrands, so
asking more often is wasted requests; but never re-asking — where this started —
was worse than it looked, because it made a wrong URL, an expired key and a
source that was down for an hour into permanent answers that only a manual
cache clear could undo. A day is long enough to cost nothing and short enough
that a mistake fixes itself overnight. The window covers the noes as well:
those are most of the cache, and exactly the rows a corrected setting has to be
able to overturn.

Most symbols have no logo, which is why **"there isn't one" is stored** as a
row with no image in it. Without that tombstone every fund, index and crypto
pair on the watchlist would be re-asked on every cycle for the life of the
install. What makes that safe is the line between a durable answer and a
transient one: no `logoUrl`, a 404, a body that isn't an image — all recorded,
because none of them will be different next time. A timeout, a refused
connection, a 502 — not recorded, so they are retried.

**And every "no" carries its reason**, which the Settings page reports back as
"3 of 7 symbols asked about have a logo. 4 came back without one: <why>".
Without it the feature is untestable from the outside: logos on and no logos
showing looks identical whether the symbols genuinely have none, the URL is
mistyped, or the cycle simply hasn't reached them yet. The reason is what
separates those, and "answered with text/html, not an image" is the whole
diagnosis for the commonest mistake.

Three smaller things the code would not explain on its own. A cycle fetches at
most `maxLogosPerCycle` logos, so a first run on a long watchlist doesn't open
forty connections to a host that owes it nothing — the pictures arrive over the
next few cycles and then the work stops entirely. The bytes are **sniffed, not
trusted**: content type comes from `http.DetectContentType`, because these
bytes get served back from our own origin and a "logo" that is really HTML
would be a stored cross-site script. And turning the setting off **empties the
cache**, since a drawer of third-party images should not outlive the feature
that filled it.

The drawn mark stays underneath the image rather than being replaced by it. An
`<img>` with an empty `alt` that fails to load collapses to nothing, so a logo
that 404s or a cache that was cleared mid-render leaves the initials showing —
no error handler, no second render pass, no empty squares.

### The add dialog

The one form that *is* kept out of the re-rendered subtree is the add form,
which lives in the shell as a `<dialog>` opened by a floating **+** button in
the bottom-right corner — CountRoster's create affordance, adapted. It sits
outside `#view` for a reason worth naming: nothing in it has to survive a
redraw, because nothing redraws it. No draft, no focus capture, no deferral —
what you typed stays typed because the elements are never replaced. The
symbol-search results are the same story, painted into their own region rather
than carried through `render`.

`showModal()` rather than a hand-rolled overlay: it brings the focus trap, the
Escape key and an inert background, none of which this client would otherwise
have. Closing — by the button, by the backdrop, or by Escape — resets the form;
a half-typed symbol from ten minutes ago is a puzzle, not a convenience. A
*rejected* add is the exception and keeps the dialog open on what you typed, so
a duplicate or a formula that won't parse is fixed with an edit rather than a
retype.

CountRoster shows its FAB on phones only, because its desktop layout keeps a
create button on the page. This one shows at every width: removing the
add-a-ticker card from the top of the watchlist left the button as the only way
in. Its distance from the bottom is a token (`--fab-inset`) because the toasts
read it too and stack above the button rather than landing on it, and because
the phone layout has to lift both clear of the tab bar.

### The allocation editor

Same shell, for a stronger reason. It is a *repeating* form: rows are added and
removed as you type, and a background redraw of `#view` landing mid-row would
take the half-typed row with it. Out in the shell nothing replaces it, so the
DOM is the state — `allocationRows()` reads the fields back rather than keeping
a model in step with them, and only a change to the *set* of rows repaints
anything.

The running weight total under the rows exists because the server rejects an
allocation that doesn't add up to 100, and finding that out on submit, after
typing eight symbols, is the wrong moment. Saving runs the portfolio, because
the result is what the portfolio is for; **Run without saving** answers "would
this even work?" without committing anything to the database.

One thing this cost an hour to learn: a `type="number"` field with `min="1"` and
`step="100"` makes 10,000 an *invalid* value, and the browser then refuses to
submit the form with nothing on screen to explain it. Stepped number inputs
silently invalidate everything off their own grid; `step="any"` is the default
worth reaching for unless the grid is the point.

On a phone the row carries four controls, and four will not share 390px once
each is 44px tall — squeezing them leaves a symbol field five characters wide.
The replacement drops to its own line underneath instead, which is also the
right reading order: it qualifies the holding above it rather than being a
fourth thing to fill in. One grid, two `grid-template-areas`, no JavaScript.

### The performance sheet

A second `<dialog>` in the shell, for the same reason: a chart being scrubbed
must not be replaced by the ten-second poll mid-drag.

It is opened by **double-tapping a row**, counted as two clicks on the same
`.quote` inside 450ms rather than by a `dblclick` listener plus a touch path — a
tap raises a click everywhere this app runs, so one timing check covers a thumb
and a mouse. Controls are excluded from the count (double-clicking **Pause**
means two pauses), and the row carries `touch-action: manipulation`, which gives
up the browser's own double-tap gesture — page zoom — so the second tap reaches
the handler at all. The gesture is undiscoverable on its own, so the watchlist's
blurb names it; it is deliberately *not* a sixth button, because five already
have to fit one line at 390px.

The server sends the whole series and the chips re-slice it client-side, so
changing span is instant and costs no request. Two things the chart does that a
naive one doesn't, both found by pointing it at a ratio of two legs that move
together:

- **A series flat to within floating-point residue is drawn flat.** Scaling by
  its span magnifies the last bits of a double into a dramatic zigzag.
- **Axis labels get enough decimals to differ from each other.** The row's own
  formatter gives three identical labels to a series that moved a thousandth of
  a percent, which makes a chart of a rounding error look exactly like a chart
  of a rally. The gutter is then sized from the widest label, which is also
  what stops `68,000.00` being clipped.

The scrub handler maps a pointer's x straight back to a point index, which only
works because the SVG's aspect ratio is pinned in CSS to its `viewBox` — with
letterboxing the crosshair would sit under the wrong day at every width but one.
`touch-action: pan-y` on the chart lets a sideways drag read the line while an
up-and-down drag still scrolls the sheet.

### The on-screen keyboard

A bottom sheet and a soft keyboard fight over the same edge of the screen, and
the keyboard wins by default. **A modal dialog is positioned against the
*layout* viewport, which the keyboard does not shrink** — so a sheet pinned to
the bottom puts its Add button behind the keyboard the instant you tap the
field above it. The field stays visible (the browser scrolls it into view); the
button you need next does not.

The *visual* viewport does shrink, so the difference between the two is how
much the keyboard is covering. `trackKeyboard` publishes it as
`--keyboard-inset`, and the phone layout sits the sheet on top of that and
takes the same height out of its `max-height`. It degrades to `0px` everywhere
it doesn't apply — desktop, keyboard down, or a browser without
`visualViewport` — so nothing reading it needs a fallback branch.

Two details that are easy to get wrong:

- **The actions live outside the scrolling body**, in a `.sheet__foot` pinned
  to the bottom of the sheet, attached to the form by the `form` attribute.
  When the keyboard takes half the screen the sheet shrinks to fit above it,
  and a submit button that scrolled with the fields would be exactly the thing
  that disappeared.
- **A collapsing URL bar is not a keyboard.** It moves the visual viewport by
  60–100px, so the class that hides the tab bar and the FAB is gated at 150px;
  without the threshold the bottom chrome flickers away as you scroll the
  watchlist.

The same media query also puts every input on a phone at `font-size: 16px`.
Below 16px, iOS Safari zooms the page in the moment a field is tapped, and
leaves you zoomed and scrolled sideways with no obvious way back. The
`min-height` alongside it takes those fields, the sheet's buttons, its close
button and the formula disclosure past the 44px touch target.

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

- **Holdings, cost basis, P&L.** Portfolios here are hypothetical allocations to
  backtest, not an account: there are no lots, no contributions, no fees, no tax
  and no inflation adjustment. The published payload is still prices and nothing
  else — whatever consumes it does its own arithmetic.
- **Alerting.** The downstream key-value store already has consumers; alerting
  belongs there.
- **Multiple users.** No accounts means no per-user watchlists, and that is the
  right trade for a household service on a LAN.
- **A licensed data feed.** Yahoo's public endpoints are free, unauthenticated
  and unsupported. The provider is an interface so a paid feed could be added,
  but that is not what this is for.
