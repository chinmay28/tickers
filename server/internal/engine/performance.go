package engine

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/expr"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Performance is a ticker's own past, assembled on demand for the watchlist's
// history sheet.
//
// It deliberately does *not* come from the stored quote_history table. That
// table is the sparkline's: it holds one point per refresh cycle, pruned to a
// retention window measured in hours, so it can answer "what has this done
// today" and nothing longer. A five-year return has to come from the provider,
// which already has the daily series.
type Performance struct {
	Symbol string `json:"symbol"`
	Label  string `json:"label"`
	// Composite says the series was computed from a formula, leg by leg and
	// day by day, rather than fetched.
	Composite bool `json:"composite"`
	// Points is the daily series, oldest first. The client slices it for
	// whichever range the reader picked; sending it once is cheaper than a
	// round trip per chip.
	Points  []Point  `json:"points"`
	Returns []Return `json:"returns"`
}

// Point is one day's value. Date rather than a timestamp because the series is
// daily and because a date is what composites align their legs on.
type Point struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
}

// Return is the move from one past close to the latest one.
type Return struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Available is false when the series does not reach back far enough — a
	// symbol listed last year has no five-year return, and reporting one
	// measured from its first day would be a fabrication.
	Available bool `json:"available"`
	// From is the day the baseline actually came from, which is usually a day
	// or two before the window's nominal start: markets are shut at weekends.
	From      string  `json:"from"`
	FromValue float64 `json:"fromValue"`
	ToValue   float64 `json:"toValue"`
	Change    float64 `json:"change"`
	// ChangePercent is nil when the baseline is not positive. A composite whose
	// formula is a difference can sit at or below zero, and a percentage of
	// that is noise wearing a percent sign.
	ChangePercent *float64 `json:"changePercent"`
	// Annualized is the compound annual rate, set only for windows longer than
	// a year — over a year or less it would merely restate ChangePercent.
	Annualized *float64 `json:"annualizedPercent"`
}

// historyTTL is how long a fetched daily series is reused. The series gains a
// point once a day, but its last point moves while the market is open, which is
// why this is minutes rather than hours. It exists because the sheet is opened
// by a double-tap — the one gesture people repeat when they aren't sure it
// registered.
const historyTTL = 10 * time.Minute

// historySince is how far back a series is fetched. Two months past the longest
// window, so the five-year return has a close from *before* five years ago to
// measure from even across a long holiday or a trading halt.
func historySince(now time.Time) time.Time { return now.AddDate(-5, -2, 0) }

// Performance assembles the history sheet for one ticker.
//
// Composites are priced here the same way the refresh cycle prices them —
// evaluate the formula against a map of symbol to value — only once per day
// instead of once per cycle. That is why a ratio gets a five-year chart without
// anything having stored one.
func (e *Engine) Performance(ctx context.Context, tickerID string) (Performance, error) {
	historian, ok := e.provider.(quotes.Historian)
	if !ok {
		return Performance{}, quotes.ErrNoHistory
	}
	t, err := e.store.Ticker(tickerID)
	if err != nil {
		return Performance{}, err
	}
	// The same reason every other upstream path calls this: a base URL or
	// timeout edited in Settings has to be in force now, not after a restart.
	e.syncProvider()

	now := time.Now().UTC()
	perf := Performance{Symbol: t.Symbol, Label: t.Label, Composite: t.IsComposite()}

	if t.IsComposite() {
		perf.Points, err = e.compositeSeries(ctx, historian, t.Expression, historySince(now))
	} else {
		perf.Points, err = e.symbolSeries(ctx, historian, t.Symbol, historySince(now))
	}
	if err != nil {
		return Performance{}, err
	}
	perf.Returns = computeReturns(perf.Points, now)
	return perf, nil
}

func (e *Engine) symbolSeries(ctx context.Context, h quotes.Historian, symbol string, since time.Time) ([]Point, error) {
	bars, err := e.symbolHistory(ctx, h, symbol, since)
	if err != nil {
		return nil, err
	}
	points := make([]Point, 0, len(bars))
	for _, b := range bars {
		points = append(points, Point{Date: b.Date, Value: b.Close})
	}
	sortPoints(points)
	return points, nil
}

// compositeSeries evaluates the formula once per day, on the days every leg
// traded.
//
// Legs are intersected rather than carried forward: a day where one leg is shut
// and another moved would otherwise produce a ratio that never existed, and it
// would do so on exactly the days — holidays, half-sessions — a reader is most
// likely to be squinting at.
func (e *Engine) compositeSeries(ctx context.Context, h quotes.Historian, formula string, since time.Time) ([]Point, error) {
	parsed, err := expr.Parse(formula)
	if err != nil {
		return nil, err
	}

	legs := make(map[string]map[string]float64, len(parsed.Symbols()))
	var dates []string
	for i, symbol := range parsed.Symbols() {
		bars, err := e.symbolHistory(ctx, h, symbol, since)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", symbol, err)
		}
		byDate := make(map[string]float64, len(bars))
		for _, b := range bars {
			byDate[b.Date] = b.Close
		}
		legs[symbol] = byDate
		if i == 0 {
			for _, b := range bars {
				dates = append(dates, b.Date)
			}
		}
	}

	points := make([]Point, 0, len(dates))
	values := make(map[string]float64, len(legs))
	for _, date := range dates {
		clear(values)
		complete := true
		for symbol, byDate := range legs {
			v, ok := byDate[date]
			if !ok {
				complete = false
				break
			}
			values[symbol] = v
		}
		if !complete {
			continue
		}
		// A day whose legs divide to zero is dropped rather than failing the
		// whole series — the other 1,200 days are still worth charting.
		value, err := parsed.Eval(values)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		points = append(points, Point{Date: date, Value: value})
	}
	sortPoints(points)
	return points, nil
}

func sortPoints(points []Point) {
	// Dates are YYYY-MM-DD, so lexical order is chronological order. The sort
	// is defensive: computeReturns binary-searches this slice, and a provider
	// that returned bars out of order would silently give wrong baselines.
	sort.SliceStable(points, func(i, j int) bool { return points[i].Date < points[j].Date })
}

// ---------------------------------------------------------------------------
// The history cache
// ---------------------------------------------------------------------------

type historyEntry struct {
	bars    []quotes.Bar
	fetched time.Time
}

// symbolHistory fetches one symbol's daily bars, or returns the cached ones.
// Caching by *symbol* rather than by ticker is what makes a composite over
// symbols already on the watchlist cost no extra requests, exactly as the
// refresh cycle's fetch plan does.
func (e *Engine) symbolHistory(ctx context.Context, h quotes.Historian, symbol string, since time.Time) ([]quotes.Bar, error) {
	if bars, ok := e.cachedHistory(symbol); ok {
		return bars, nil
	}
	bars, err := h.History(ctx, symbol, since)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	if e.historyCache == nil {
		e.historyCache = map[string]historyEntry{}
	}
	e.historyCache[symbol] = historyEntry{bars: bars, fetched: time.Now()}
	e.mu.Unlock()
	return bars, nil
}

func (e *Engine) cachedHistory(symbol string) ([]quotes.Bar, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.historyCache[symbol]
	if !ok {
		return nil, false
	}
	if time.Since(entry.fetched) > historyTTL {
		delete(e.historyCache, symbol)
		return nil, false
	}
	return entry.bars, true
}

// ---------------------------------------------------------------------------
// Returns
// ---------------------------------------------------------------------------

// windows are the periods the sheet reports, in the order it shows them.
//
// Each names a start *date* rather than a number of days, because that is how
// people read them: "a month" is the same day last month, not 30 days, and
// "year to date" is measured from the last close of last year rather than from
// the first of January, which is a holiday in every market there is.
var windows = []struct {
	key, label string
	start      func(now time.Time) time.Time
	// years is the window's length for annualising, and 0 for the windows
	// short enough that a compound annual rate would be theatre.
	years float64
}{
	{"1w", "1 week", func(t time.Time) time.Time { return t.AddDate(0, 0, -7) }, 0},
	{"1m", "1 month", func(t time.Time) time.Time { return t.AddDate(0, -1, 0) }, 0},
	{"3m", "3 months", func(t time.Time) time.Time { return t.AddDate(0, -3, 0) }, 0},
	{"ytd", "Year to date", func(t time.Time) time.Time {
		return time.Date(t.Year()-1, time.December, 31, 0, 0, 0, 0, time.UTC)
	}, 0},
	{"1y", "1 year", func(t time.Time) time.Time { return t.AddDate(-1, 0, 0) }, 0},
	{"3y", "3 years", func(t time.Time) time.Time { return t.AddDate(-3, 0, 0) }, 3},
	{"5y", "5 years", func(t time.Time) time.Time { return t.AddDate(-5, 0, 0) }, 5},
}

// computeReturns measures every window against the latest point.
//
// Windows the series cannot reach are still returned, marked unavailable: a row
// that says "not enough history" is information, and a row that silently
// vanishes leaves a reader wondering whether the fetch half-failed.
func computeReturns(points []Point, now time.Time) []Return {
	out := make([]Return, 0, len(windows))
	for _, w := range windows {
		r := Return{Key: w.key, Label: w.label}
		if len(points) > 0 {
			latest := points[len(points)-1]
			if base, ok := baseline(points, w.start(now)); ok && base.Date != latest.Date {
				r.Available = true
				r.From = base.Date
				r.FromValue = base.Value
				r.ToValue = latest.Value
				r.Change = latest.Value - base.Value
				if base.Value > 0 {
					pct := r.Change / base.Value * 100
					r.ChangePercent = &pct
					if w.years > 0 && latest.Value > 0 {
						annual := (math.Pow(latest.Value/base.Value, 1/w.years) - 1) * 100
						r.Annualized = &annual
					}
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// baseline is the last point on or before target — the close a return is
// measured from. Weekends, holidays and halts all mean the window's nominal
// start is usually not a trading day, so "on or before" is the whole job.
func baseline(points []Point, target time.Time) (Point, bool) {
	date := target.UTC().Format("2006-01-02")
	// The first point *after* the target, so the one before it is the answer.
	i, _ := slices.BinarySearchFunc(points, date, func(p Point, date string) int {
		switch {
		case p.Date < date:
			return -1
		case p.Date > date:
			return 1
		default:
			return 0
		}
	})
	// BinarySearchFunc lands on an exact match when there is one; otherwise on
	// the insertion point, whose predecessor is the last earlier day.
	if i < len(points) && points[i].Date == date {
		return points[i], true
	}
	if i == 0 {
		return Point{}, false
	}
	return points[i-1], true
}
