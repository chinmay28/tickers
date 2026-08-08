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
	"github.com/chinmay28/tickers/server/internal/store"
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
	// Points is the series to chart, oldest first, thinned by chartPoints. The
	// client slices it for whichever range the reader picked; sending it once
	// is cheaper than a round trip per chip.
	Points []Point `json:"points"`
	// Returns and Ranges are both computed from the *full* series, before any
	// thinning, so the numbers are exact whatever the chart is drawn from.
	//
	// Both are always sent. A return is the natural reading of a price and a
	// dubious one of a ratio — a composite has no capital in it to return —
	// while a high, a low and where today sits between them mean the same
	// thing for either. The client picks; the payload does not have to guess.
	Returns []Return `json:"returns"`
	Ranges  []Range  `json:"ranges"`
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
	// Annualized is the compound annual rate, set only where the baseline is
	// far enough back for one to mean something — over a year or less it would
	// merely restate ChangePercent.
	Annualized *float64 `json:"annualizedPercent"`
}

// Range is the high and low over one window, and where the latest value sits
// between them.
//
// This is what a composite has instead of a return. "VTI/GLD is at 1.62,
// against a five-year low of 1.02 and a high of 2.14" says something a reader
// can act on; "VTI/GLD returned 8%" invites them to read a ratio as a holding.
type Range struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Available is false unless the series covers the whole window — see
	// windowStart, where the difference between covering one and merely
	// falling inside it is the whole point.
	Available bool    `json:"available"`
	Low       float64 `json:"low"`
	LowDate   string  `json:"lowDate"`
	High      float64 `json:"high"`
	HighDate  string  `json:"highDate"`
	Latest    float64 `json:"latest"`
	// Position is where Latest sits between Low and High, 0 on the low and 100
	// on the high. Nil when the window never moved, because everywhere in a
	// band of zero width is equally the top and the bottom of it.
	Position *float64 `json:"position"`
}

// historyTTL is how long a fetched daily series is reused. The series gains a
// point once a day, but its last point moves while the market is open, which is
// why this is minutes rather than hours. It exists because the sheet is opened
// by a double-tap — the one gesture people repeat when they aren't sure it
// registered.
const historyTTL = 10 * time.Minute

// historyStart is how far back a series is fetched: everything the source will
// give up. "All time" has to mean it, and asking for a bounded window instead
// would make the longest row a restatement of wherever that boundary fell.
//
// It is a fixed value rather than a per-call argument, and that is what makes
// caching a series by symbol alone correct.
func historyStart() time.Time { return time.Unix(0, 0).UTC() }

// Performance assembles the history sheet for one ticker.
//
// Composites are priced here the same way the refresh cycle prices them —
// evaluate the formula against a map of symbol to value — only once per day
// instead of once per cycle. That is why a ratio gets a decade-long chart
// without anything having stored one.
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

	var full []Point
	switch {
	case t.IsPortfolio():
		var p store.Portfolio
		if p, err = e.store.Portfolio(t.PortfolioID); err == nil {
			full, err = e.portfolioSeries(ctx, historian, p, historyStart())
		}
	case t.IsComposite():
		full, err = e.compositeSeries(ctx, historian, t.Expression, historyStart())
	default:
		full, err = e.symbolSeries(ctx, historian, t.Symbol, historyStart())
	}
	if err != nil {
		return Performance{}, err
	}
	// Measure first, thin second: an all-time high dropped by the thinning
	// would still have to be reported, and reporting one the chart cannot show
	// is worse than either.
	perf.Returns = computeReturns(full, now)
	perf.Ranges = computeRanges(full, now)
	perf.Points = chartPoints(full, now)
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

// portfolioSeries values a saved allocation's units against past closes, one
// day at a time.
//
// It is the same shape as compositeSeries and intersects its legs for the same
// reason — a day one holding was shut and another moved would produce a total
// nobody's account ever showed. The difference is what it means: this is the
// *holding* the watchlist row stands for, carried backwards, so it shows what
// today's units would have been worth rather than what the strategy returned.
// The rebalanced answer is the backtest's, on the Portfolios page.
func (e *Engine) portfolioSeries(ctx context.Context, h quotes.Historian, p store.Portfolio, since time.Time) ([]Point, error) {
	if len(p.Holdings) == 0 {
		return nil, fmt.Errorf("%s has no holdings", p.Name)
	}

	legs := make(map[string]map[string]float64, len(p.Holdings))
	var dates []string
	for i, holding := range p.Holdings {
		if holding.Units == 0 {
			return nil, fmt.Errorf("%s has not been priced yet", p.Name)
		}
		bars, err := e.symbolHistory(ctx, h, holding.Symbol, since)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", holding.Symbol, err)
		}
		byDate := make(map[string]float64, len(bars))
		for _, b := range bars {
			byDate[b.Date] = b.Close
		}
		legs[holding.Symbol] = byDate
		if i == 0 {
			for _, b := range bars {
				dates = append(dates, b.Date)
			}
		}
	}

	points := make([]Point, 0, len(dates))
	for _, date := range dates {
		value, complete := 0.0, true
		for _, holding := range p.Holdings {
			close, ok := legs[holding.Symbol][date]
			if !ok {
				complete = false
				break
			}
			value += holding.Units * close
		}
		if complete {
			points = append(points, Point{Date: date, Value: value})
		}
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
	// start is the day the window begins. Nil means the series' own beginning:
	// "all time" is as far back as the source goes, which is the only honest
	// definition available to something reading a third-party feed.
	start func(now time.Time) time.Time
	// ranged marks the windows that also get a high/low row. The very short
	// ones don't: five sessions have a highest and a lowest close, but nobody
	// calls that a range.
	ranged bool
}{
	{"1w", "1 week", func(t time.Time) time.Time { return t.AddDate(0, 0, -7) }, false},
	{"1m", "1 month", func(t time.Time) time.Time { return t.AddDate(0, -1, 0) }, true},
	{"3m", "3 months", func(t time.Time) time.Time { return t.AddDate(0, -3, 0) }, true},
	{"ytd", "Year to date", func(t time.Time) time.Time {
		return time.Date(t.Year()-1, time.December, 31, 0, 0, 0, 0, time.UTC)
	}, true},
	{"1y", "1 year", func(t time.Time) time.Time { return t.AddDate(-1, 0, 0) }, true},
	{"3y", "3 years", func(t time.Time) time.Time { return t.AddDate(-3, 0, 0) }, false},
	{"5y", "5 years", func(t time.Time) time.Time { return t.AddDate(-5, 0, 0) }, true},
	{"10y", "10 years", func(t time.Time) time.Time { return t.AddDate(-10, 0, 0) }, true},
	{"all", "All time", nil, true},
}

// annualiseAbove is how long a window has to be before a compound annual rate
// is worth showing. Just over a year, so the "1 year" row doesn't print its own
// total twice while an all-time row for a two-year-old listing still gets one.
const annualiseAbove = 1.5

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
			base, ok := points[0], true
			if w.start != nil {
				base, ok = baseline(points, w.start(now))
			}
			if ok && base.Date != latest.Date {
				r.Available = true
				r.From = base.Date
				r.FromValue = base.Value
				r.ToValue = latest.Value
				r.Change = latest.Value - base.Value
				if base.Value > 0 {
					pct := r.Change / base.Value * 100
					r.ChangePercent = &pct
					// Annualised from the dates the baseline actually has, not
					// from the window's nominal length: the "5 years" row is
					// measured from a close five years and a few days back, and
					// "all time" has no nominal length at all.
					if years := yearsBetween(base.Date, latest.Date); years > annualiseAbove && latest.Value > 0 {
						annual := (math.Pow(latest.Value/base.Value, 1/years) - 1) * 100
						r.Annualized = &annual
					}
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// computeRanges finds the high and low inside each window, and where the latest
// value sits between them.
//
// A range starts at the first close *inside* the window, where a return starts
// at the last close *before* it. That is not an inconsistency: a return needs
// something to measure from, and a high is only a high if it happened during
// the period being claimed for it.
func computeRanges(points []Point, now time.Time) []Range {
	out := make([]Range, 0, len(windows))
	for _, w := range windows {
		if !w.ranged {
			continue
		}
		r := Range{Key: w.key, Label: w.label}
		if from, ok := windowStart(points, w.start, now); ok {
			low, high := points[from], points[from]
			for _, p := range points[from:] {
				if p.Value < low.Value {
					low = p
				}
				if p.Value > high.Value {
					high = p
				}
			}
			latest := points[len(points)-1]

			r.Available = true
			r.Low, r.LowDate = low.Value, low.Date
			r.High, r.HighDate = high.Value, high.Date
			r.Latest = latest.Value
			if span := high.Value - low.Value; span > 0 {
				position := (latest.Value - low.Value) / span * 100
				r.Position = &position
			}
		}
		out = append(out, r)
	}
	return out
}

// windowStart is the index a window's data begins at, and whether the window is
// reportable at all.
//
// The test is coverage, not overlap. Every close a symbol listed three weeks ago
// has falls inside the last ten years, but calling their high a ten-year high is
// the same fabrication as quoting that symbol a ten-year return — so a window is
// only available when the series was already running when it opened.
func windowStart(points []Point, start func(time.Time) time.Time, now time.Time) (int, bool) {
	if len(points) == 0 {
		return 0, false
	}
	if start == nil {
		return 0, true
	}
	opened := start(now)
	if points[0].Date > opened.UTC().Format("2006-01-02") {
		return 0, false
	}
	from := firstOnOrAfter(points, opened)
	// A series that stops before the window opens — a delisted symbol still on
	// the watchlist — has no range inside it either.
	return from, from < len(points)
}

// baseline is the last point on or before target — the close a return is
// measured from. Weekends, holidays and halts all mean the window's nominal
// start is usually not a trading day, so "on or before" is the whole job.
func baseline(points []Point, target time.Time) (Point, bool) {
	i := firstOnOrAfter(points, target)
	// BinarySearchFunc lands on an exact match when there is one; otherwise on
	// the insertion point, whose predecessor is the last earlier day.
	if i < len(points) && points[i].Date == target.UTC().Format("2006-01-02") {
		return points[i], true
	}
	if i == 0 {
		return Point{}, false
	}
	return points[i-1], true
}

// firstOnOrAfter is the index of the earliest point dated on or after target,
// or len(points) when the series ends before it.
func firstOnOrAfter(points []Point, target time.Time) int {
	date := target.UTC().Format("2006-01-02")
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
	return i
}

// yearsBetween is the span between two YYYY-MM-DD dates in years, for
// annualising. Zero if either won't parse, which reads as "don't annualise".
func yearsBetween(from, to string) float64 {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return 0
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return 0
	}
	return end.Sub(start).Hours() / 24 / 365.25
}

// ---------------------------------------------------------------------------
// Thinning the chart
// ---------------------------------------------------------------------------

// How much of the series keeps its daily resolution, and how much is thinned to
// one point a week before the rest drops to one a month.
const (
	chartDailyYears  = 2
	chartWeeklyYears = 10
)

// chartPoints thins a long series down to something worth sending and drawing.
//
// A symbol listed in the eighties has ten thousand daily closes, which is a
// quarter of a megabyte on the wire and ten thousand segments in an SVG for a
// phone to rasterise — to draw a line a few hundred pixels wide. The recent
// years keep every session, because that is what the short ranges show; older
// stretches keep one close a week and then one a month, which is what a decade
// chart is anyway.
//
// The high and the low always survive. They are named in the ranges table, and
// a peak the chart doesn't reach beside a number that says it happened is the
// kind of disagreement that makes a reader distrust both.
func chartPoints(points []Point, now time.Time) []Point {
	if len(points) == 0 {
		return points
	}
	daily := now.AddDate(-chartDailyYears, 0, 0).Format("2006-01-02")
	weekly := now.AddDate(-chartWeeklyYears, 0, 0).Format("2006-01-02")

	// Points inside a bucket collapse to their last one — the bucket's close.
	bucket := func(p Point) string {
		switch {
		case p.Date >= daily:
			return p.Date
		case p.Date >= weekly:
			at, err := time.Parse("2006-01-02", p.Date)
			if err != nil {
				return p.Date
			}
			year, week := at.ISOWeek()
			return fmt.Sprintf("%04d-W%02d", year, week)
		default:
			return p.Date[:len("2006-01")]
		}
	}

	low, high := 0, 0
	for i, p := range points {
		if p.Value < points[low].Value {
			low = i
		}
		if p.Value > points[high].Value {
			high = i
		}
	}

	out := make([]Point, 0, len(points))
	for i, p := range points {
		closes := i == len(points)-1 || bucket(points[i+1]) != bucket(p)
		if closes || i == 0 || i == low || i == high {
			out = append(out, p)
		}
	}
	return out
}
