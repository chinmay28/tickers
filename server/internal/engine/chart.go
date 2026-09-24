package engine

import (
	"errors"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
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
		bars, err = a.Best(lead)
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
