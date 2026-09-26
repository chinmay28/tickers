package mcptools

// instructions is what a client's model is told about the server as a whole
// during the handshake: the order the tools are meant to be used in, and the
// few facts about the data that change how every answer should be read.
const instructions = `Tickers is a self-hosted market-data archive with a backtester. It holds OHLCV bars for US-listed stocks and ETFs (plus whatever else its owner added) at four widths — 1d, 1h, 5m, 1m — collected locally from Yahoo and optionally Polygon.

A productive order of work:
1. archive_status: what is collected and how far back. search_symbols and symbol_info for a particular symbol's coverage.
2. get_bars and get_indicators to look at the data.
3. find_signals to learn whether a condition carries information — the return after it versus the baseline from every bar — before trading on it. Pool across several symbols for more occurrences.
4. run_backtest to trade a rule; sweep_strategy to tune its parameters, with holdoutFrom so the ranking is checked on data it wasn't chosen on.
5. save_strategy to hand a finding to the person, who sees it on the app's Strategies page.

Facts that matter:
- Prices are split-adjusted, not dividend-adjusted, unless a tool's dividends flag is set (daily bars only).
- Daily history usually reaches back to the listing. Yahoo serves only two years of hourly bars, 60 days of 5-minute and 30 of 1-minute; the archive keeps them from then on, so intraday history is as deep as the archive is old (deeper with Polygon configured). symbol_info says exactly what is held.
- Intraday bars are the regular session unless asked otherwise. Times are UTC.
- Backtests fill at the next bar's open after a signal on a close, go all in, and are long only. Metrics are percentages (12.5 is 12.5%).
- Asking for a symbol the archive doesn't collect adds it to the front of the queue; the call says so, and a retry some minutes later finds it.
- The server may be a Raspberry Pi: prefer narrow windows and small grids first, then widen.

The rule language is described in the run_backtest tool and in full in the resource ` + languageURI + `.`

// languageURI names the rule language's reference document.
const languageURI = "tickers://docs/rule-language"

// languageDoc is the rule language in full: what the tool descriptions have
// no room for.
const languageDoc = `# The rule language

Strategies (run_backtest, sweep_strategy, save_strategy) and signals (find_signals) are written as rules.

## Rules

A rule is a list of up to 8 conditions and how they combine:

    {"match": "all", "conditions": [{"left": "sma:50", "op": "crosses_above", "right": "sma:200"}]}

"match" is "all" (every condition holds; the default) or "any" (at least one does). A strategy needs an entry rule; its exit rule may be empty, and then a position is held until a stop, a target or the end of the test.

## Conditions

Each condition compares two operands with one of:

| op | holds on a bar when |
|----|---------------------|
| > < >= <= | the comparison is true on that bar |
| crosses_above | left was at or below right on the bar before, and is above it on this one |
| crosses_below | left was at or above right on the bar before, and is below it on this one |

A cross holds on the one bar it happens. A level comparison holds on every bar it's true.

## Operands

- Price fields: open, high, low, close (or price), volume.
- Numbers, written as strings: "70", "0.5".
- Indicators, as spec[.line]:

| spec | indicator | lines (first is the default) |
|------|-----------|------------------------------|
| sma:N | simple moving average of the close | sma |
| ema:N | exponential moving average | ema |
| bb:N:K | Bollinger bands, N bars, K deviations | middle, upper, lower |
| vwap | volume-weighted average price, reset each day | vwap |
| rsi:N | relative strength index, 0–100 (Wilder) | rsi |
| macd:F:S:G | MACD | macd, signal, histogram (or hist) |
| stoch:K:D | stochastic oscillator, 0–100 | k, d |
| atr:N | average true range (Wilder) | atr |
| obv | on-balance volume | obv |

Parameters default when left off: sma and ema 20, bb 20:2, rsi 14, macd 12:26:9, stoch 14:3, atr 14. Periods are whole bars, 1–1000. A strategy can use at most 12 distinct indicators.

Every indicator is computed from bars before the test window as well, so it is settled on the first bar tested. An indicator not yet defined (not enough history) makes any condition reading it false.

Examples:

- Golden cross: sma:50 crosses_above sma:200
- Oversold in an uptrend: rsi:14 < "30" and close > sma:200
- Band touch: close < bb:20:2.lower
- Momentum turn: macd:12:26:9.histogram crosses_above "0"
- Quiet market: atr:14 < "2"

## How a backtest trades

- A rule is judged on a bar's close; the order fills at the next bar's open.
- All equity goes in on entry, in fractional shares; it all comes out on exit. Long only, one position at a time.
- stopLoss and takeProfit are percentages from the entry price, checked inside every bar held. A bar that opens beyond the level fills at its open; a bar that reaches both is assumed to hit the stop first.
- feePercent is charged on the value traded, both ways.
- A position still open at the end is marked at the last close, with reason "open".
- Buy-and-hold is the same money bought at the first bar's open and held.

## Metrics

Percentages are percentages. totalReturn and cagr are over the window; maxDrawdown is the deepest fall from a peak (negative); sharpe is annualised from per-bar returns at a zero risk-free rate, so compare it with buy-and-hold's rather than with published figures. profitFactor is gross gains over gross losses (null with no losing trade); exposure is the share of bars a position was held.

## Studies (find_signals)

An occurrence is a bar the signal held on — by default only the first of each consecutive stretch. Its return at horizon N is from the next bar's open to the close N bars after the signal. The baseline is the same measurement from every bar in the window; edge is the mean return after signals minus the baseline mean. A handful of occurrences is anecdote, not evidence: look at count, pool across related symbols, and check the edge holds in more than one period.
`
