package xsection

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

var t0 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// grow is n daily bars compounding at rate a day from 100, each opening at
// the close before it.
func grow(n int, rate float64) []quotes.Candle {
	out := make([]quotes.Candle, n)
	prev := 100.0
	for i := range out {
		c := prev * (1 + rate)
		out[i] = quotes.Candle{Time: t0.AddDate(0, 0, i), Open: prev, High: math.Max(prev, c), Low: math.Min(prev, c), Close: c, Volume: 1000}
		prev = c
	}
	return out
}

func mustFactor(t *testing.T, s string) Factor {
	t.Helper()
	f, err := ParseFactor(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return f
}

func TestPanelAlignsOnTheUnionOfDates(t *testing.T) {
	p := NewPanel(map[string][]quotes.Candle{"B": grow(3, 0), "A": grow(5, 0)[2:]})
	if len(p.Dates) != 5 || p.Symbols[0] != "A" {
		t.Fatalf("panel has %d dates and symbols %v, want the union of 5 and names sorted", len(p.Dates), p.Symbols)
	}
	if !math.IsNaN(p.Close[0][0]) || math.IsNaN(p.Close[0][2]) || !math.IsNaN(p.Close[1][4]) {
		t.Errorf("closes = %v / %v, want NaN before A lists and after B stops", p.Close[0], p.Close[1])
	}
	if p.First(t0.AddDate(0, 0, 3)) != 3 {
		t.Error("First didn't find the date")
	}
}

func TestFactorsFromHandWorkedSeries(t *testing.T) {
	bars := []quotes.Candle{}
	for i, c := range []float64{100, 110, 99, 121, 110} {
		bars = append(bars, quotes.Candle{Time: t0.AddDate(0, 0, i), Open: c, High: c, Low: c, Close: c, Volume: 10})
	}
	p := NewPanel(map[string][]quotes.Candle{"X": bars})
	at := func(f string, i int) float64 { return mustFactor(t, f).Values(p)[0][i] }
	near(t, "return:2 on day 4", at("return:2", 4), (110.0/99-1)*100)
	near(t, "return:2:1 on day 4 (skipping the last day)", at("return:2:1", 4), (121.0/110-1)*100)
	if !math.IsNaN(at("return:2:1", 2)) {
		t.Error("return:2:1 was defined before it had 3 days behind it")
	}
	near(t, "drawdown:3 on day 4", at("drawdown:3", 4), (110.0/121-1)*100)
	near(t, "dollarvolume:2 on day 1", at("dollarvolume:2", 1), (100*10+110*10)/2.0)
	near(t, "distance:sma:2 on day 1", at("distance:sma:2", 1), (110/105.0-1)*100)
	near(t, "close", at("close", 3), 121)
	// Returns +10%, -10%: sample stdev √0.02 = 0.1414, annualised.
	near(t, "volatility:2 on day 2", at("volatility:2", 2), math.Sqrt(0.02)*math.Sqrt(252)*100)

	for _, bad := range []string{"return", "return:0", "volatility:1", "drawdown:5:1", "distance:70", "momentum:5", "70"} {
		if _, err := ParseFactor(bad); !strategy.IsInvalid(err) {
			t.Errorf("%q gave %v, want an InvalidError", bad, err)
		}
	}
}

func TestAFactorComputesOverASymbolsOwnDays(t *testing.T) {
	// B misses day 2. Its return:1 on day 3 is from its day-1 close, the
	// last one it has, not undefined for want of a day-2 bar.
	b := grow(4, 0.1)
	p := NewPanel(map[string][]quotes.Candle{"A": grow(4, 0), "B": append(b[:2:2], b[3])})
	v := mustFactor(t, "return:1").Values(p)[1]
	near(t, "B's return over its missing day", v[3], (b[3].Close/b[1].Close-1)*100)
	if !math.IsNaN(v[2]) {
		t.Error("B has a value on a day it didn't trade")
	}
}

// fan is n symbols each compounding at its own steady rate — the kth at k
// tenths of a percent a day — so past return ranks them exactly as future
// return does.
func fan(n, days int) *Panel {
	series := map[string][]quotes.Candle{}
	for k := 0; k < n; k++ {
		series[string(rune('A'+k))] = grow(days, float64(k)/1000)
	}
	return NewPanel(series)
}

func TestAStudyOfAFactorThatPredictsPerfectly(t *testing.T) {
	p := fan(10, 60)
	st, err := RunStudy(p, StudySpec{Factor: mustFactor(t, "return:5"), From: 10, Every: 5, Horizon: 5, Quantiles: 5})
	if err != nil {
		t.Fatal(err)
	}
	if st.Rankings == 0 || st.AvgSymbols != 10 {
		t.Fatalf("study = %+v, want rankings over all ten symbols", st)
	}
	near(t, "mean IC", st.IC.Mean, 1)
	near(t, "IC positive share", st.IC.Positive, 100)
	if st.Spread.Mean <= 0 || st.Quantiles[4].Mean <= st.Quantiles[0].Mean {
		t.Errorf("spread %v, quantiles %+v: want the top fifth ahead of the bottom", st.Spread, st.Quantiles)
	}
	// The same factor read the other way round predicts perfectly badly.
	inv, _ := RunStudy(p, StudySpec{Factor: mustFactor(t, "drawdown:5"), From: 10, Every: 5, Horizon: 5, Quantiles: 5})
	if inv.IC.Mean > 0.99 {
		t.Errorf("drawdown on steadily rising series has IC %v; every symbol is at its high, so it ranks nothing", inv.IC.Mean)
	}
	few, _ := RunStudy(fan(3, 60), StudySpec{Factor: mustFactor(t, "return:5"), From: 10, Every: 5, Horizon: 5, Quantiles: 2})
	if few.Rankings != 0 || len(few.Quantiles) != 0 {
		t.Errorf("three symbols in two quantiles = %+v, want no rankings and no quantiles reported as zeros", few)
	}
	if _, err := RunStudy(p, StudySpec{Factor: mustFactor(t, "return:5"), Every: 5, Horizon: 5, Quantiles: 1}); !strategy.IsInvalid(err) {
		t.Errorf("one quantile gave %v, want refused", err)
	}
}

func TestOverlappingHorizonsDoNotInflateTheTStat(t *testing.T) {
	p := fan(10, 120)
	// Noise, so the IC varies and has a standard deviation to divide by.
	for s := range p.Symbols {
		for i := range p.Dates {
			p.Close[s][i] *= 1 + 0.02*math.Sin(float64(i*(s+3)))
		}
	}
	spec := StudySpec{Factor: mustFactor(t, "return:5"), From: 10, Every: 1, Horizon: 20, Quantiles: 2}
	overlap, _ := RunStudy(p, spec)
	spec.Horizon = 1
	apart, _ := RunStudy(p, spec)
	if overlap.IC.Stdev == 0 || apart.IC.Stdev == 0 {
		t.Fatal("the noise didn't make the IC vary")
	}
	// Daily rankings of a 20-day return share 19 days with the next: the
	// count is cut twentyfold, which divides the t-statistic by √20.
	naive := overlap.IC.Mean / (overlap.IC.Stdev / math.Sqrt(float64(overlap.Rankings)))
	near(t, "t-stat scaled for the overlap", overlap.IC.TStat*math.Sqrt(20), naive)
}

func TestRotationHoldsTheLeaderAndChargesItsTrades(t *testing.T) {
	p := fan(3, 30) // A flat, B +0.1%/day, C +0.2%/day
	rot, err := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:3"), From: 5, Every: 5, Hold: 1, FeePercent: 1, Initial: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, pk := range rot.Picks {
		if len(pk.Symbols) != 1 || pk.Symbols[0] != "C" {
			t.Fatalf("pick %+v, want C every time — it always leads", pk)
		}
	}
	// Bought at day 6's open after a 1% fee, then held to the end at
	// C's close: one fee, no churn.
	c := p.Symbols[2]
	if c != "C" {
		t.Fatal("symbols out of order")
	}
	want := 1000 * 0.99 * p.Close[2][29] / p.Open[2][6]
	near(t, "final equity", rot.Equity[len(rot.Equity)-1], want)
	if rot.Rebalances != len(rot.Picks) || rot.Turnover <= 0 || rot.Turnover > 100.0/float64(rot.Rebalances)+1e-6 {
		t.Errorf("turnover %v over %d rebalances, want one full buy and nothing after", rot.Turnover, rot.Rebalances)
	}
	if rot.Equity[0] != 1000 || len(rot.Equity) != len(rot.Dates) {
		t.Errorf("the curve starts at %v with %d points for %d dates, want the initial amount, aligned", rot.Equity[0], len(rot.Equity), len(rot.Dates))
	}
	// The benchmark is all three, equally weighted, fee-free: behind C.
	if rot.Benchmark.TotalReturn >= rot.Strategy.TotalReturn {
		t.Errorf("benchmark %v%% vs rotation %v%%, want the leader ahead of the average", rot.Benchmark.TotalReturn, rot.Strategy.TotalReturn)
	}

	low, _ := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:3"), From: 5, Every: 5, Hold: 1, Ascending: true, Benchmark: "C"})
	if low.Picks[0].Symbols[0] != "A" || low.Benchmark.TotalReturn <= low.Strategy.TotalReturn {
		t.Errorf("ascending picked %v and trailed C by %v, want the flat A behind the benchmark", low.Picks[0].Symbols, low.Benchmark.TotalReturn-low.Strategy.TotalReturn)
	}
	if _, err := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:3"), Every: 5, Hold: 1, Benchmark: "SPY"}); err == nil || !strings.Contains(err.Error(), "SPY") {
		t.Errorf("a benchmark not in the panel gave %v", err)
	}
}

func TestADelistedHoldingIsValuedAtItsLastClose(t *testing.T) {
	c := grow(20, 0.01)
	p := NewPanel(map[string][]quotes.Candle{"GONE": c[:10], "STAY": grow(20, 0)})
	rot, err := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:2"), From: 3, Every: 30, Hold: 1, Initial: 100})
	if err != nil {
		t.Fatal(err)
	}
	if rot.Picks[0].Symbols[0] != "GONE" {
		t.Fatalf("picked %v, want the rising GONE", rot.Picks[0].Symbols)
	}
	last := rot.Equity[len(rot.Equity)-1]
	near(t, "equity after GONE stops", last, 100*c[9].Close/c[4].Open)
}

func TestFiltersJudgeEachDayOnItsOwnData(t *testing.T) {
	cheap := grow(30, 0.01)
	for i := range cheap {
		cheap[i].Open, cheap[i].Close, cheap[i].High, cheap[i].Low = cheap[i].Open/50, cheap[i].Close/50, cheap[i].High/50, cheap[i].Low/50
	}
	p := NewPanel(map[string][]quotes.Candle{"PENNY": cheap, "FIRM": grow(30, 0.001)})
	rot, _ := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:2"), From: 3, Every: 5, Hold: 1, Filter: Filter{MinPrice: 5}})
	for _, pk := range rot.Picks {
		if len(pk.Symbols) > 0 && pk.Symbols[0] == "PENNY" {
			t.Fatalf("held a $2 stock with a $5 minimum: %+v", pk)
		}
	}
	thin, _ := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:2"), From: 25, Every: 5, Hold: 1, Filter: Filter{MinDollarVolume: 1e9}})
	if thin.AvgHeld != 0 || len(thin.Warnings) == 0 {
		t.Errorf("with nothing liquid enough the book held %v, warnings %v; want cash and a warning", thin.AvgHeld, thin.Warnings)
	}
	if thin.Equity[len(thin.Equity)-1] != 10000 {
		t.Errorf("an empty book ended at %v, want the cash it started with", thin.Equity[len(thin.Equity)-1])
	}
}

func TestAWhereRuleNarrowsWhoIsRanked(t *testing.T) {
	p := fan(3, 30)
	// Only symbols above 101 — A is flat at 100 and B takes ten days to get
	// there, C five — so the first ranking holds C alone and A is never held.
	sig, err := strategy.CompileSignal(strategy.Rule{Conditions: []strategy.Condition{{Left: "close", Op: ">", Right: "101"}}})
	if err != nil {
		t.Fatal(err)
	}
	rot, _ := Rotate(p, RotationSpec{Factor: mustFactor(t, "return:1"), From: 5, Every: 5, Hold: 3, Filter: Filter{Where: &sig}})
	if fmt.Sprint(rot.Picks[0].Symbols) != "[C]" {
		t.Errorf("the first ranking held %v, want C alone above 101", rot.Picks[0].Symbols)
	}
	last := rot.Picks[len(rot.Picks)-1].Symbols
	if fmt.Sprint(last) != "[C B]" {
		t.Errorf("the last ranking held %v, want B joined once above 101 and A never", last)
	}
}

func TestScreenRanksTheEligibleOnADay(t *testing.T) {
	p := fan(4, 10)
	got := Screen(p, mustFactor(t, "return:3"), Filter{}, 9, false)
	if len(got) != 4 || got[0].Symbol != "D" || got[3].Symbol != "A" {
		t.Fatalf("screen = %+v, want D (fastest) first and A last", got)
	}
	if got[0].Close != p.Close[3][9] {
		t.Errorf("D's close = %v, want the day's", got[0].Close)
	}
	low := Screen(p, mustFactor(t, "return:3"), Filter{MinPrice: 101.5}, 9, true)
	if len(low) != 2 || low[0].Symbol != "C" {
		t.Errorf("ascending, above $101.50 = %+v, want C then D — A and B are below it", low)
	}
	if len(Screen(p, mustFactor(t, "return:3"), Filter{}, 1, false)) != 0 {
		t.Error("a day before the factor is defined ranked something")
	}
}

func TestCompareMeasuresAndCorrelates(t *testing.T) {
	// B moves exactly twice A's daily move; C moves against A.
	var a, b, c []quotes.Candle
	pa, pb, pc := 100.0, 100.0, 100.0
	for i := 0; i < 60; i++ {
		move := 0.01 * math.Sin(float64(i))
		pa, pb, pc = pa*(1+move), pb*(1+2*move), pc*(1-move)
		d := t0.AddDate(0, 0, i)
		a = append(a, quotes.Candle{Time: d, Open: pa, High: pa, Low: pa, Close: pa})
		b = append(b, quotes.Candle{Time: d, Open: pb, High: pb, Low: pb, Close: pb})
		c = append(c, quotes.Candle{Time: d, Open: pc, High: pc, Low: pc, Close: pc})
	}
	p := NewPanel(map[string][]quotes.Candle{"A": a, "B": b, "C": c, "SHORT": a[:10]})
	cmp := Compare(p, 0, []string{"A", "B", "C", "SHORT", "MISSING"})
	if len(cmp.Stats) != 4 || cmp.Stats[0].Symbol != "A" {
		t.Fatalf("stats = %+v, want A, B, C and SHORT in the order asked", cmp.Stats)
	}
	near(t, "B's beta to A", cmp.Stats[1].Beta, 2)
	near(t, "B's correlation to A", cmp.Stats[1].Correlation, 1)
	near(t, "C against A", cmp.Correlation[2][0], -1)
	if !math.IsNaN(cmp.Stats[0].Beta) || !math.IsNaN(cmp.Stats[3].Correlation) {
		t.Error("A has a beta to itself, or SHORT a correlation from nine returns")
	}
	near(t, "B's volatility over A's", cmp.Stats[1].Volatility/cmp.Stats[0].Volatility, 2)
	if cmp.Stats[0].Days != 60 || cmp.Stats[0].Metrics.TotalReturn == 0 {
		t.Errorf("A = %+v, want 60 days measured", cmp.Stats[0])
	}
}
