package engine

import (
	"errors"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/expr"
	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// ErrNoArchive is what a chart request gets on a server with no archive open.
var ErrNoArchive = errors.New("the market-data archive is not open")

// Chart is a window of an archived series with indicators computed over it.
type Chart struct {
	Bars       []quotes.Candle
	Indicators []indicators.Result
}

// Chart reads a window of bars from the archive and computes the indicators
// over it.
//
// The bars are read from further back than the window, by as much as the
// slowest indicator needs to settle, and that lead-in is cut off afterwards:
// a 200-day average drawn from the first bar shown, rather than starting 200
// bars into the chart, is the difference between a chart and a puzzle. Where
// the archive simply doesn't reach that far back, the indicator starts later,
// undefined until it has its n bars — never computed over fewer.
func (e *Engine) Chart(q archive.Query, specs []indicators.Spec) (Chart, error) {
	r := e.archiveReader()
	if r == nil {
		return Chart{}, ErrNoArchive
	}
	warm := 0
	for _, s := range specs {
		warm = max(warm, s.Warmup())
	}
	lead := q
	lead.From = q.From.Add(-lookback(q.Interval, warm, q.Extended))

	var bars []quotes.Candle
	err := r.Read(func(a *archive.Archive) error {
		var err error
		bars, err = best(a, lead, false)
		return err
	})
	if err != nil {
		return Chart{}, err
	}
	cut := 0
	for cut < len(bars) && bars[cut].Time.Before(q.From) {
		cut++
	}
	// Keep only as much lead-in as the indicators asked for; a generous
	// lookback over a long weekend fetches more, and computing over the
	// surplus changes nothing an SMA or EMA reports once settled.
	start := max(0, cut-warm)
	bars = bars[start:]
	cut -= start
	results := indicators.Trim(indicators.Compute(specs, bars), cut)
	return Chart{Bars: bars[cut:], Indicators: results}, nil
}

// lookback is how far back n bars reach, generously: calendar days hold
// weekends and holidays, and a session holds only so many intraday bars.
// Too far costs a few extra rows read; too short leaves the first bars of the
// chart with unsettled indicators.
func lookback(interval quotes.Interval, n int, extended bool) time.Duration {
	if n <= 0 {
		return 0
	}
	const day = 24 * time.Hour
	if !interval.Intraday() {
		return time.Duration(n*7/5+n/15+7) * day
	}
	session := 390 * time.Minute // 9:30–16:00
	if extended {
		session = 16 * time.Hour // 4:00–20:00
	}
	perDay := max(1, int(session/interval.Step()))
	days := (n + perDay - 1) / perDay
	return time.Duration(days*7/5+4) * day
}

// Bars reads a window of a symbol's regular-session bars, adjusted for
// dividends when asked (daily bars only: a payout is a daily event).
func (e *Engine) Bars(symbol string, interval quotes.Interval, from, to time.Time, dividends bool) ([]quotes.Candle, error) {
	r := e.archiveReader()
	if r == nil {
		return nil, ErrNoArchive
	}
	var bars []quotes.Candle
	err := r.Read(func(a *archive.Archive) error {
		var err error
		q := archive.Query{Symbol: symbol, Interval: interval, From: from, To: to}
		if expr.Looks(symbol) || !dividends || interval.Intraday() {
			bars, err = best(a, q, dividends && !interval.Intraday())
			return err
		}
		if bars, err = a.Best(q); err != nil {
			return err
		}
		divs, err := a.Dividends(symbol, from, to)
		if err == nil && len(divs) > 0 {
			bars = adjustCandles(bars, divs)
		}
		return err
	})
	return bars, err
}

// best is Archive.Best that also reads a formula — "SPY/TLT" — by combining
// its legs, each adjusted for its own dividends when asked. It never adds a
// symbol to the archive: a chart is looking, not asking for collection.
func best(a *archive.Archive, q archive.Query, dividends bool) ([]quotes.Candle, error) {
	if !expr.Looks(q.Symbol) {
		return a.Best(q)
	}
	f, err := expr.Parse(q.Symbol)
	if err != nil {
		return nil, err
	}
	legs := map[string][]quotes.Candle{}
	for _, sym := range f.Symbols() {
		lq := q
		lq.Symbol = sym
		bars, err := a.Best(lq)
		if err != nil {
			return nil, err
		}
		if dividends {
			divs, err := a.Dividends(sym, q.From, q.To)
			if err != nil {
				return nil, err
			}
			if len(divs) > 0 {
				bars = adjustCandles(bars, divs)
			}
		}
		legs[sym] = bars
	}
	return combine(f, legs), nil
}
