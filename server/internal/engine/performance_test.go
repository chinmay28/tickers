package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// fakeHistorian is a fakeProvider that also remembers a daily series, and
// counts who asked for what — the cache and the leg deduplication are both
// claims about request counts.
type fakeHistorian struct {
	*fakeProvider
	hmu   sync.Mutex
	bars  map[string][]quotes.Bar
	calls map[string]int
	err   error
}

func newFakeHistorian(prices map[string]float64) *fakeHistorian {
	return &fakeHistorian{
		fakeProvider: &fakeProvider{prices: prices},
		bars:         map[string][]quotes.Bar{},
		calls:        map[string]int{},
	}
}

func (f *fakeHistorian) History(_ context.Context, symbol string, _ time.Time) ([]quotes.Bar, error) {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	f.calls[symbol]++
	if f.err != nil {
		return nil, f.err
	}
	return f.bars[symbol], nil
}

func (f *fakeHistorian) callsFor(symbol string) int {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	return f.calls[symbol]
}

// series builds bars from a date → close list, in the order given.
func series(pairs ...any) []quotes.Bar {
	bars := make([]quotes.Bar, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		bars = append(bars, quotes.Bar{Date: pairs[i].(string), Close: pairs[i+1].(float64)})
	}
	return bars
}

func points(pairs ...any) []Point {
	out := make([]Point, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, Point{Date: pairs[i].(string), Value: pairs[i+1].(float64)})
	}
	return out
}

func returnFor(returns []Return, key string) Return {
	for _, r := range returns {
		if r.Key == key {
			return r
		}
	}
	return Return{}
}

func TestComputeReturnsMeasuresFromTheLastCloseBeforeEachWindow(t *testing.T) {
	// 15 March 2024 was a Friday. A week earlier is Friday the 8th, a month
	// earlier is 15 February, and the year's baseline is the last close of
	// 2023 — 29 December, because the 31st was a Sunday.
	now := time.Date(2024, 3, 15, 17, 0, 0, 0, time.UTC)
	closes := points(
		"2023-12-29", 100.0,
		"2024-01-02", 101.0,
		"2024-02-15", 110.0,
		"2024-03-07", 118.0,
		"2024-03-08", 120.0,
		"2024-03-15", 132.0,
	)

	returns := computeReturns(closes, now)

	week := returnFor(returns, "1w")
	if !week.Available || week.From != "2024-03-08" {
		t.Fatalf("1w measured from %q, want the close on 8 March", week.From)
	}
	if week.ChangePercent == nil || math.Abs(*week.ChangePercent-10) > 1e-9 {
		t.Errorf("1w = %v%%, want 10 (120 → 132)", deref(week.ChangePercent))
	}

	if ytd := returnFor(returns, "ytd"); ytd.From != "2023-12-29" {
		t.Errorf("YTD measured from %q; the baseline is last year's final close, not 1 January", ytd.From)
	}
	if month := returnFor(returns, "1m"); month.From != "2024-02-15" {
		t.Errorf("1m measured from %q, want 15 February", month.From)
	}
}

func TestComputeReturnsMarksWindowsTheSeriesCannotReach(t *testing.T) {
	now := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	// A listing three weeks old: it has a week, and nothing longer.
	returns := computeReturns(points("2024-02-26", 10.0, "2024-03-08", 11.0, "2024-03-15", 12.0), now)

	if week := returnFor(returns, "1w"); !week.Available {
		t.Error("a three-week series must still have a one-week return")
	}
	for _, key := range []string{"1y", "3y", "5y"} {
		r := returnFor(returns, key)
		if r.Available {
			t.Errorf("%s reported %v; a return measured from a series that does not reach back is a fabrication", key, r)
		}
		// The row still has to come back, or the sheet silently loses periods
		// and a reader can't tell that from a half-failed fetch.
		if r.Label == "" {
			t.Errorf("%s was dropped from the table entirely", key)
		}
	}
}

func TestComputeReturnsAnnualisesOnlyTheLongWindows(t *testing.T) {
	now := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	// A clean quadrupling over five years: 4^(1/5) − 1 ≈ 31.95% a year.
	returns := computeReturns(points("2019-03-15", 100.0, "2023-03-15", 300.0, "2024-03-15", 400.0), now)

	five := returnFor(returns, "5y")
	if five.Annualized == nil || math.Abs(*five.Annualized-31.9507) > 0.001 {
		t.Errorf("5y annualised = %v, want ≈31.95%% a year", deref(five.Annualized))
	}
	if year := returnFor(returns, "1y"); year.Annualized != nil {
		t.Errorf("1y carries an annualised rate of %v; over a year it would just restate the total", deref(year.Annualized))
	}
}

func TestComputeReturnsSkipsPercentagesOffANonPositiveBaseline(t *testing.T) {
	now := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	// A difference composite that was negative a month ago: the move is real,
	// the percentage of it would be noise.
	returns := computeReturns(points("2024-02-15", -4.0, "2024-03-15", 6.0), now)

	month := returnFor(returns, "1m")
	if !month.Available || month.Change != 10 {
		t.Fatalf("1m = %+v, want an available row with a change of 10", month)
	}
	if month.ChangePercent != nil {
		t.Errorf("1m percent = %v; a percentage of a negative baseline is not a return", deref(month.ChangePercent))
	}
}

func TestPerformanceNeedsAProviderWithHistory(t *testing.T) {
	eng, st := newTestEngine(t, &fakeProvider{prices: map[string]float64{"VTI": 300}})
	clearWatchlist(t, st)
	ticker, err := st.CreateTicker(store.NewTicker{Symbol: "VTI"})
	if err != nil {
		t.Fatalf("create ticker: %v", err)
	}

	if _, err := eng.Performance(context.Background(), ticker.ID); !errors.Is(err, quotes.ErrNoHistory) {
		t.Errorf("Performance error = %v, want ErrNoHistory for a provider that only prices today", err)
	}
}

func TestPerformanceIntersectsACompositesLegs(t *testing.T) {
	provider := newFakeHistorian(map[string]float64{"VTI": 300, "GLD": 200})
	// GLD did not trade on the 3rd — a holiday on one exchange and not the
	// other. A ratio for that day would be a number that never existed.
	provider.bars["VTI"] = series("2024-01-02", 200.0, "2024-01-03", 210.0, "2024-01-04", 220.0)
	provider.bars["GLD"] = series("2024-01-02", 100.0, "2024-01-04", 110.0)

	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)
	ticker, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}

	perf, err := eng.Performance(context.Background(), ticker.ID)
	if err != nil {
		t.Fatalf("performance: %v", err)
	}
	if !perf.Composite {
		t.Error("the sheet did not report the row as a composite")
	}
	if len(perf.Points) != 2 {
		t.Fatalf("got %d points, want 2 — the day one leg was shut must be dropped: %+v", len(perf.Points), perf.Points)
	}
	if perf.Points[0].Date != "2024-01-02" || perf.Points[0].Value != 2 {
		t.Errorf("first point = %+v, want 2024-01-02 at 2 (200/100)", perf.Points[0])
	}
	if perf.Points[1].Date != "2024-01-04" || perf.Points[1].Value != 2 {
		t.Errorf("second point = %+v, want 2024-01-04 at 2 (220/110)", perf.Points[1])
	}
}

func TestPerformanceFetchesEachSymbolOnceWithinTheCacheWindow(t *testing.T) {
	provider := newFakeHistorian(map[string]float64{"VTI": 300, "GLD": 200})
	provider.bars["VTI"] = series("2024-01-02", 200.0, "2024-01-03", 210.0)
	provider.bars["GLD"] = series("2024-01-02", 100.0, "2024-01-03", 105.0)

	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)
	plain, err := st.CreateTicker(store.NewTicker{Symbol: "VTI"})
	if err != nil {
		t.Fatalf("create ticker: %v", err)
	}
	composite, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}

	for range 2 {
		if _, err := eng.Performance(context.Background(), plain.ID); err != nil {
			t.Fatalf("performance: %v", err)
		}
	}
	if _, err := eng.Performance(context.Background(), composite.ID); err != nil {
		t.Fatalf("composite performance: %v", err)
	}

	// The sheet is opened by a double-tap, the one gesture people repeat when
	// they aren't sure it registered — and a composite over a symbol already on
	// the watchlist must cost no extra request, the same as the fetch plan.
	if got := provider.callsFor("VTI"); got != 1 {
		t.Errorf("VTI's history was fetched %d times, want 1 within the cache window", got)
	}
	if got := provider.callsFor("GLD"); got != 1 {
		t.Errorf("GLD's history was fetched %d times, want 1", got)
	}
}

func TestPerformanceReportsWhichLegHadNoHistory(t *testing.T) {
	provider := newFakeHistorian(map[string]float64{"VTI": 300})
	provider.err = errors.New("upstream said no")

	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)
	ticker, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}

	_, err = eng.Performance(context.Background(), ticker.ID)
	if err == nil {
		t.Fatal("a composite whose legs have no history reported success")
	}
	if !strings.Contains(err.Error(), "VTI") || !strings.Contains(err.Error(), "upstream said no") {
		t.Errorf("error = %q, want the leg and the provider's own reason", err)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
