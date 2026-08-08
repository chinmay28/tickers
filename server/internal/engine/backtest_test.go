package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// monthlyBars turns a run of month-end closes into daily bars, one per month,
// starting at first ("2020-01"). A backtest reduces each month to its last bar,
// so one bar a month is a complete series as far as it is concerned.
func monthlyBars(first string, closes ...float64) []quotes.Bar {
	start, err := time.Parse("2006-01", first)
	if err != nil {
		panic(err)
	}
	bars := make([]quotes.Bar, 0, len(closes))
	for i, close := range closes {
		bars = append(bars, quotes.Bar{
			Date:  start.AddDate(0, i, 0).Format("2006-01") + "-28",
			Close: close,
		})
	}
	return bars
}

// payingHistorian is a fakeHistorian that also distributes, so the yield has
// something to divide.
type payingHistorian struct {
	*fakeHistorian
	payouts map[string][]quotes.Distribution
}

func (p payingHistorian) Dividends(_ context.Context, symbol string, _ time.Time) ([]quotes.Distribution, error) {
	return p.payouts[symbol], nil
}

// dividendEngine wires an engine to a source with both prices and payouts.
// Bars carry Raw explicitly: a yield divides a payout by the price of its own
// day, and the adjusted close is not that price.
func dividendEngine(t *testing.T, bars map[string][]quotes.Bar,
	payouts map[string][]quotes.Distribution) *Engine {
	t.Helper()
	historian := newFakeHistorian(map[string]float64{})
	for symbol, series := range bars {
		historian.bars[symbol] = series
	}
	eng, _ := newTestEngine(t, payingHistorian{fakeHistorian: historian, payouts: payouts})
	return eng
}

// backtestEngine wires an engine to a historian holding the given series.
func backtestEngine(t *testing.T, bars map[string][]quotes.Bar) (*Engine, *fakeHistorian) {
	t.Helper()
	provider := newFakeHistorian(map[string]float64{})
	for symbol, series := range bars {
		provider.bars[symbol] = series
	}
	eng, _ := newTestEngine(t, provider)
	return eng, provider
}

// rising and falling are the same three months from opposite directions:
// +10% a month against −10% a month. Every weighting claim below is checked
// against these, because the arithmetic stays doable in your head.
var (
	rising  = monthlyBars("2020-01", 100, 110, 121)
	falling = monthlyBars("2020-01", 100, 90, 81)
)

func spec(holdings ...store.Holding) BacktestSpec {
	return BacktestSpec{Holdings: holdings, InitialAmount: 1000, Rebalance: store.RebalanceNone}
}

func half() []store.Holding {
	return []store.Holding{{Symbol: "UP", Weight: 50}, {Symbol: "DOWN", Weight: 50}}
}

// lump is a run with no money paid in, where the index is just the balances
// rebased to 1 — which is what the simulation produces for a lump sum.
func lump(balances []float64) result {
	index := make([]float64, len(balances))
	for i, v := range balances {
		index[i] = v / balances[0]
	}
	return result{balances: balances, index: index}
}

func TestBacktestCompoundsEachHoldingByItsOwnReturn(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	result, err := eng.Backtest(context.Background(), spec(half()...))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	// 500 compounding at +10% for two months is 605; 500 at −10% is 405.
	if math.Abs(result.Portfolio.End-1010) > 1e-9 {
		t.Errorf("final balance = %.4f, want 1010 — each holding has to grow by its own return, not the portfolio's",
			result.Portfolio.End)
	}
	if result.Points[0].Value != 1000 {
		t.Errorf("the run starts at %.2f, want the initial amount 1000", result.Points[0].Value)
	}
	if result.Start != "2020-01" || result.End != "2020-03" {
		t.Errorf("ran %s to %s, want 2020-01 to 2020-03", result.Start, result.End)
	}
	if result.Months != 2 {
		t.Errorf("counted %d monthly returns over three months, want 2", result.Months)
	}
}

func TestBacktestRebalancingSellsWhatGrewAndBuysWhatDidNot(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	drifted, err := eng.Backtest(context.Background(), spec(half()...))
	if err != nil {
		t.Fatalf("drifting backtest: %v", err)
	}

	monthly := spec(half()...)
	monthly.Rebalance = store.RebalanceMonthly
	held, err := eng.Backtest(context.Background(), monthly)
	if err != nil {
		t.Fatalf("rebalanced backtest: %v", err)
	}

	// Rebalanced back to 500/500 after February, the third month's +10% and
	// −10% cancel exactly and the portfolio ends where it started.
	if math.Abs(held.Portfolio.End-1000) > 1e-9 {
		t.Errorf("monthly rebalancing ended at %.4f, want 1000", held.Portfolio.End)
	}
	if held.Portfolio.End >= drifted.Portfolio.End {
		t.Errorf("rebalancing (%.4f) did not differ from letting the weights drift (%.4f); "+
			"the whole point of the cadence is that it changes the answer",
			held.Portfolio.End, drifted.Portfolio.End)
	}
	// Three months hold two rebalance opportunities, and the last month is not
	// one of them — moving money at the closing bell changes nothing and would
	// still be counted as a trade.
	if held.Rebalances != 1 {
		t.Errorf("performed %d rebalances over three months, want 1", held.Rebalances)
	}
}

func TestBacktestRebalancesOnCalendarBoundariesNotElapsedMonths(t *testing.T) {
	// The same three months, straddling a year end rather than sitting inside
	// one: an annual rebalance has to fire at December.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"UP":   monthlyBars("2019-11", 100, 110, 121),
		"DOWN": monthlyBars("2019-11", 100, 90, 81),
	})

	annual := spec(half()...)
	annual.Rebalance = store.RebalanceAnnually
	result, err := eng.Backtest(context.Background(), annual)
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if result.Rebalances != 1 {
		t.Fatalf("an annual rebalance over November→January fired %d times, want once at December",
			result.Rebalances)
	}
	if math.Abs(result.Portfolio.End-1000) > 1e-9 {
		t.Errorf("final balance = %.4f, want 1000 — rebalanced at the December close, January's ±10%% cancels",
			result.Portfolio.End)
	}
}

func TestBacktestIntersectsMonthsSoNoLegContributesAPhantomReturn(t *testing.T) {
	// DOWN did not trade in February. Carrying its January close forward would
	// hand the portfolio a month where one side moved and the other didn't.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"UP": rising,
		"DOWN": {
			{Date: "2020-01-28", Close: 100},
			{Date: "2020-03-28", Close: 81},
		},
	})

	result, err := eng.Backtest(context.Background(), spec(half()...))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if result.Months != 1 {
		t.Errorf("used %d monthly returns, want 1 — February is not a month both holdings have",
			result.Months)
	}
	// January to March directly: 500×1.21 + 500×0.81.
	if math.Abs(result.Portfolio.End-1010) > 1e-9 {
		t.Errorf("final balance = %.4f, want 1010", result.Portfolio.End)
	}
}

func TestBacktestStartsWhereItsLatestHoldingDoesAndSaysSo(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"OLD": monthlyBars("2018-01", 100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111,
			112, 113, 114, 115, 116, 117, 118, 119, 120, 121, 122, 123, 124, 125, 126),
		"NEW": monthlyBars("2020-01", 100, 110, 121),
	})

	result, err := eng.Backtest(context.Background(), BacktestSpec{
		Holdings:      []store.Holding{{Symbol: "OLD", Weight: 50}, {Symbol: "NEW", Weight: 50}},
		InitialAmount: 1000,
		StartYear:     2018,
	})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if result.Start != "2020-01" {
		t.Errorf("ran from %s; a portfolio can only start where all of it has prices", result.Start)
	}
	// The date on its own leaves the reader to work out which holding is
	// responsible, which is the thing they'd have to change.
	if len(result.Notes) == 0 {
		t.Fatal("a start two years later than the one asked for went unexplained")
	}
	if !strings.Contains(result.Notes[0], "NEW") {
		t.Errorf("the note is %q; it has to name the holding that set the start", result.Notes[0])
	}
	for _, h := range result.Holdings {
		if h.Symbol == "NEW" && h.FirstMonth != "2020-01" {
			t.Errorf("NEW reports its first month as %q, want 2020-01", h.FirstMonth)
		}
	}
}

func TestBacktestMeasuresCalendarYearsFromTheDecemberBefore(t *testing.T) {
	// December 2019 through June 2021: 2020 is a whole year, 2021 is half of one.
	closes := []float64{100}
	for c := 101.0; c <= 111; c++ {
		closes = append(closes, c)
	}
	closes = append(closes, 120)
	closes = append(closes, 121, 122, 123, 124, 125, 132)

	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"ONE": monthlyBars("2019-12", closes...)})
	result, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "ONE", Weight: 100}))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if len(result.Annual) != 2 {
		t.Fatalf("got %d annual rows, want 2 — the base month is a baseline, not a year of its own: %+v",
			len(result.Annual), result.Annual)
	}
	first, second := result.Annual[0], result.Annual[1]
	if first.Year != 2020 || math.Abs(first.Percent-20) > 1e-9 || first.Partial {
		t.Errorf("2020 = %+v, want a full year of +20%% measured from the December 2019 close", first)
	}
	if second.Year != 2021 || math.Abs(second.Percent-10) > 1e-9 || !second.Partial {
		t.Errorf("2021 = %+v, want +10%% marked partial — the run stops in June", second)
	}
	// A half year is not a worst year, however bad it was.
	if result.Portfolio.Worst == nil || result.Portfolio.Worst.Year != 2020 {
		t.Errorf("worst year = %+v, want 2020 — the only year the run covers end to end",
			result.Portfolio.Worst)
	}
}

func TestBacktestReportsAPartFirstYearJustAsItReportsAPartLastOne(t *testing.T) {
	// Starting in October, 2020 is three months long. Dropping it while showing
	// an equally partial final year is the inconsistency; showing it unmarked
	// would claim a year the run never covered.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"ONE": monthlyBars("2020-10", 100, 110, 120, 130, 140),
	})

	result, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "ONE", Weight: 100}))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if len(result.Annual) != 2 || result.Annual[0].Year != 2020 {
		t.Fatalf("annual rows = %+v, want a part 2020 and a part 2021", result.Annual)
	}
	if !result.Annual[0].Partial || math.Abs(result.Annual[0].Percent-20) > 1e-9 {
		t.Errorf("2020 = %+v, want +20%% marked partial (October to December)", result.Annual[0])
	}
	// Neither year is whole, so there is no best or worst to report.
	if result.Portfolio.Best != nil {
		t.Errorf("best year = %+v; no calendar year here was covered end to end", result.Portfolio.Best)
	}
}

func TestBacktestRunsTheBenchmarkOverTheSameMonths(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	withBenchmark := spec(half()...)
	withBenchmark.Benchmark = "up"
	result, err := eng.Backtest(context.Background(), withBenchmark)
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if result.Benchmark == nil {
		t.Fatal("no benchmark came back for a spec that asked for one")
	}
	if result.Benchmark.Label != "UP" {
		t.Errorf("benchmark labelled %q, want the normalised symbol UP", result.Benchmark.Label)
	}
	// 1000 fully invested in a symbol that gained 21%.
	if math.Abs(result.Benchmark.End-1210) > 1e-9 {
		t.Errorf("benchmark ended at %.4f, want 1210", result.Benchmark.End)
	}
	if result.Points[len(result.Points)-1].Benchmark == nil {
		t.Error("the growth points carry no benchmark value; the chart has nothing to draw the second line from")
	}
}

func TestBacktestBenchmarkTruncatesBothSidesToTheSharedPeriod(t *testing.T) {
	// A benchmark that only exists for the last two months. Drawing the
	// portfolio over three and the benchmark over two would put two curves on
	// one chart that cannot be read against each other.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"UP":    rising,
		"DOWN":  falling,
		"LATER": monthlyBars("2020-02", 100, 110),
	})

	withBenchmark := spec(half()...)
	withBenchmark.Benchmark = "LATER"
	result, err := eng.Backtest(context.Background(), withBenchmark)
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if result.Start != "2020-02" {
		t.Errorf("ran from %s, want 2020-02 — the benchmark is intersected like any other leg", result.Start)
	}
	if result.Months != 1 {
		t.Errorf("counted %d monthly returns, want 1", result.Months)
	}
}

func TestBacktestRenormalisesWeightsThatDoNotQuiteReachAHundred(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"A": rising, "B": rising, "C": rising,
	})

	result, err := eng.Backtest(context.Background(), spec(
		store.Holding{Symbol: "A", Weight: 33.33},
		store.Holding{Symbol: "B", Weight: 33.33},
		store.Holding{Symbol: "C", Weight: 33.33},
	))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	// Three thirds of a portfolio all up 21% is 1210 — not 1209.9, which is
	// what simulating 99.99% invested and 0.01% in nothing would produce.
	if math.Abs(result.Portfolio.End-1210) > 1e-9 {
		t.Errorf("final balance = %.6f, want 1210; weights have to be renormalised, not taken at face value",
			result.Portfolio.End)
	}
	total := 0.0
	for _, h := range result.Holdings {
		total += h.Weight
	}
	if math.Abs(total-100) > 1e-9 {
		t.Errorf("reported weights sum to %v, want exactly 100", total)
	}
}

func TestBacktestNarrowsToTheRequestedYears(t *testing.T) {
	// Five Decembers, so a start year has a December before it to measure from.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"ONE": {
			{Date: "2017-12-28", Close: 100},
			{Date: "2018-12-28", Close: 110},
			{Date: "2019-12-28", Close: 120},
			{Date: "2020-12-28", Close: 130},
			{Date: "2021-12-28", Close: 140},
		},
	})

	result, err := eng.Backtest(context.Background(), BacktestSpec{
		Holdings:      []store.Holding{{Symbol: "ONE", Weight: 100}},
		InitialAmount: 1000,
		StartYear:     2019,
		EndYear:       2020,
	})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	// 2019's return is measured from the end of 2018, not from January.
	if result.Start != "2018-12" {
		t.Errorf("started at %s; a run from 2019 needs the December 2018 close as its baseline", result.Start)
	}
	if result.End != "2020-12" {
		t.Errorf("ended at %s, want 2020-12", result.End)
	}
}

func TestBacktestRefusesSymbolsTheSourceCannotPrice(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising})

	_, err := eng.Backtest(context.Background(), spec(
		store.Holding{Symbol: "UP", Weight: 50},
		store.Holding{Symbol: "NOPE", Weight: 50},
	))
	if err == nil {
		t.Fatal("a holding with no history anywhere produced a backtest")
	}
	// A typo in the form is the caller's to fix; reporting it as a server
	// failure sends somebody to the logs for a problem they can see.
	if !errors.Is(err, ErrBadSpec) {
		t.Errorf("error %v is not an ErrBadSpec, so the API will answer 500 for a typo", err)
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error %q does not name the holding that failed", err)
	}
}

func TestBacktestRefusesAPeriodTooShortForAReturn(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"ONE": monthlyBars("2020-01", 100),
	})

	_, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "ONE", Weight: 100}))
	if !errors.Is(err, ErrBadSpec) {
		t.Errorf("a single month of history gave %v, want ErrBadSpec", err)
	}
}

func TestBacktestNeedsAProviderThatKnowsThePast(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{prices: map[string]float64{"VTI": 300}})

	_, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "VTI", Weight: 100}))
	if !errors.Is(err, quotes.ErrNoHistory) {
		t.Errorf("error = %v, want ErrNoHistory — a source that can only price today makes the "+
			"feature unavailable, not broken", err)
	}
}

func TestBacktestSharesItsHistoryWithThePerformanceSheet(t *testing.T) {
	eng, provider := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	for i := 0; i < 3; i++ {
		if _, err := eng.Backtest(context.Background(), spec(half()...)); err != nil {
			t.Fatalf("backtest %d: %v", i, err)
		}
	}

	// Caching by symbol is what keeps a portfolio of funds somebody also
	// charts from costing a fetch per view.
	if calls := provider.callsFor("UP"); calls != 1 {
		t.Errorf("fetched UP %d times for three backtests, want 1", calls)
	}
}

func TestBacktestPricesABenchmarkThatIsAlsoAHoldingOnce(t *testing.T) {
	eng, provider := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	withBenchmark := spec(half()...)
	withBenchmark.Benchmark = "UP"
	if _, err := eng.Backtest(context.Background(), withBenchmark); err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if calls := provider.callsFor("UP"); calls != 1 {
		t.Errorf("fetched UP %d times when it was both a holding and the benchmark, want 1", calls)
	}
}

func TestBacktestPaysInOnTheContributionCadence(t *testing.T) {
	// A flat holding, so every penny of the final balance is a deposit and the
	// arithmetic has nowhere to hide.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"FLAT": monthlyBars("2020-01", 100, 100, 100, 100),
	})

	paid := spec(store.Holding{Symbol: "FLAT", Weight: 100})
	paid.Contribution = 100
	paid.ContributionFrequency = store.RebalanceMonthly

	result, err := eng.Backtest(context.Background(), paid)
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	// Three monthly returns, so three contributions — including the one in the
	// final month, which earns nothing but is money that genuinely went in.
	if math.Abs(result.Contributed-300) > 1e-9 {
		t.Errorf("contributed %.2f, want 300", result.Contributed)
	}
	if math.Abs(result.Portfolio.End-1300) > 1e-9 {
		t.Errorf("final balance = %.2f, want 1300 (1000 in, 300 paid in, nothing earned)",
			result.Portfolio.End)
	}
}

func TestBacktestDoesNotCountContributionsAsReturn(t *testing.T) {
	// The bug this whole index/balance split exists to prevent: a flat holding
	// paid into every month triples the balance while returning nothing.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"FLAT": monthlyBars("2020-01", 100, 100, 100, 100),
	})

	paid := spec(store.Holding{Symbol: "FLAT", Weight: 100})
	paid.Contribution = 1000
	paid.ContributionFrequency = store.RebalanceMonthly

	result, err := eng.Backtest(context.Background(), paid)
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if math.Abs(result.Portfolio.TotalPercent) > 1e-9 {
		t.Errorf("total return = %.4f%%, want 0 — nothing was earned; the balance grew because "+
			"money was paid in", result.Portfolio.TotalPercent)
	}
	for _, year := range result.Annual {
		if math.Abs(year.Percent) > 1e-9 {
			t.Errorf("%d returned %.4f%%, want 0 for the same reason", year.Year, year.Percent)
		}
	}
	// And the balance still shows the money, because that is the other question.
	if math.Abs(result.Portfolio.End-4000) > 1e-9 {
		t.Errorf("final balance = %.2f, want 4000", result.Portfolio.End)
	}
}

func TestBacktestDrawdownIgnoresMoneyPaidInDuringTheFall(t *testing.T) {
	// Halving, with contributions large enough to keep the balance climbing
	// throughout. A drawdown measured on the balance would report none at all.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"SINKING": monthlyBars("2020-01", 100, 50, 25),
	})

	paid := spec(store.Holding{Symbol: "SINKING", Weight: 100})
	paid.Contribution = 5000
	paid.ContributionFrequency = store.RebalanceMonthly

	result, err := eng.Backtest(context.Background(), paid)
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	if result.Portfolio.MaxDrawdown < 70 {
		t.Errorf("deepest fall = %.2f%%, want about 75 — the holding lost three quarters, and a "+
			"drawdown row papered over by deposits is worse than none",
			result.Portfolio.MaxDrawdown)
	}
	if result.Portfolio.End <= result.Initial {
		t.Errorf("balance = %.2f; the deposits did raise it, and that has to stay true",
			result.Portfolio.End)
	}
}

func TestBacktestLumpSumIsUnchangedByTheContributionMachinery(t *testing.T) {
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	result, err := eng.Backtest(context.Background(), spec(half()...))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if result.Contributed != 0 {
		t.Errorf("contributed %v with no contribution configured", result.Contributed)
	}
	// With no cash flows the index and the balances are proportional, so the
	// return is still the one the balance shows.
	if math.Abs(result.Portfolio.TotalPercent-1) > 1e-9 {
		t.Errorf("total return = %.4f%%, want 1 (1000 → 1010)", result.Portfolio.TotalPercent)
	}
}

func TestRiskFreeRatesCarryTheLastKnownRateForward(t *testing.T) {
	// A rate is a level that stays in force until it changes, which is exactly
	// what a price series may never do across a gap.
	months := []string{"2020-01", "2020-02", "2020-03", "2020-04"}
	bill := map[string]float64{"2019-11": 1.8, "2020-03": 0.12}

	rates := riskFreeRates(months, bill)
	if rates == nil {
		t.Fatal("no rates from a bill that predates the run")
	}
	// November's 1.8% annual is 0.15% a month, and it is still in force in
	// January and February because nothing replaced it.
	if math.Abs(rates[0]-0.0015) > 1e-12 || math.Abs(rates[1]-0.0015) > 1e-12 {
		t.Errorf("rates = %v; November's rate has to hold until March replaces it", rates[:2])
	}
	if math.Abs(rates[2]-0.12/100/12) > 1e-12 || math.Abs(rates[3]-0.12/100/12) > 1e-12 {
		t.Errorf("rates = %v; March's cut has to apply from March on", rates[2:])
	}
}

func TestRiskFreeRatesRefuseToRunBackwards(t *testing.T) {
	// The bill starts after the portfolio does. Carrying a rate backwards would
	// be inventing monetary policy, so there is no Sharpe rather than a wrong one.
	months := []string{"2020-01", "2020-02"}
	if rates := riskFreeRates(months, map[string]float64{"2021-06": 0.5}); rates != nil {
		t.Errorf("rates = %v, want none", rates)
	}
	if rates := riskFreeRates(months, nil); rates != nil {
		t.Errorf("rates = %v for an empty bill, want none", rates)
	}
}

func TestSharpeAndSortinoSeparateUpsideFromDownsideSwings(t *testing.T) {
	// A series that only ever rises, in uneven steps. It has real volatility
	// and no downside at all — the case the two ratios exist to tell apart.
	index := []float64{1}
	for _, step := range []float64{0.01, 0.05, 0.01, 0.06, 0.02, 0.04} {
		index = append(index, index[len(index)-1]*(1+step))
	}
	rates := make([]float64, len(index)) // 0% risk-free, so excess is the return

	sharpe, sortino := riskAdjusted(index, rates)
	if sharpe == nil {
		t.Fatal("no Sharpe for a series with plenty of variance")
	}
	if sortino != nil {
		t.Errorf("Sortino = %v; a series that never fell has no downside deviation to divide by, "+
			"and inventing one would flatter it", *sortino)
	}
	if *sharpe <= 0 {
		t.Errorf("Sharpe = %v for a series that only rose", *sharpe)
	}
}

func TestSharpeFallsWhenTheRiskFreeRateRises(t *testing.T) {
	index := []float64{1, 1.02, 1.01, 1.04, 1.03, 1.06}
	flat := make([]float64, len(index))
	generous := make([]float64, len(index))
	for i := range generous {
		generous[i] = 0.01 // 1% a month risk-free, which most of these months miss
	}

	cheap, _ := riskAdjusted(index, flat)
	dear, _ := riskAdjusted(index, generous)
	if cheap == nil || dear == nil {
		t.Fatal("a Sharpe went missing")
	}
	if *dear >= *cheap {
		t.Errorf("Sharpe was %v against 0%% and %v against 1%% a month; beating a higher bar by "+
			"less has to score worse", *cheap, *dear)
	}
}

func TestRiskAdjustedNeedsARateForEveryMonth(t *testing.T) {
	index := []float64{1, 1.02, 1.01, 1.04}
	// A short or missing rate series leaves both unset. Silently treating the
	// gap as 0% would publish a different statistic under the same name.
	if sharpe, sortino := riskAdjusted(index, nil); sharpe != nil || sortino != nil {
		t.Errorf("got %v/%v with no risk-free series, want neither", sharpe, sortino)
	}
	if sharpe, _ := riskAdjusted(index, []float64{0, 0}); sharpe != nil {
		t.Errorf("got %v from a rate series shorter than the run", *sharpe)
	}
}

func TestBacktestReportsWhichRateItMeasuredAgainst(t *testing.T) {
	long := make([]float64, 40)
	for i := range long {
		long[i] = 100 * math.Pow(1.005, float64(i))
	}
	bill := make([]float64, 40)
	for i := range bill {
		bill[i] = 2.0 // 2% annualised, quoted as a percentage rather than a price
	}

	eng, _ := backtestEngine(t, map[string][]quotes.Bar{
		"ONE":          monthlyBars("2019-01", long...),
		riskFreeSymbol: monthlyBars("2019-01", bill...),
	})

	result, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "ONE", Weight: 100}))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if result.RiskFree != riskFreeSymbol {
		t.Errorf("riskFree = %q, want %q — a ratio whose benchmark rate is unnamed is unreadable",
			result.RiskFree, riskFreeSymbol)
	}
	if result.Portfolio.Sharpe == nil {
		t.Error("no Sharpe despite a full risk-free series")
	}
}

func TestBacktestSurvivesAQuoteSourceWithNoTreasuryBill(t *testing.T) {
	// The bill is not a leg: a source that has never heard of it must cost the
	// two ratios and nothing else.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	result, err := eng.Backtest(context.Background(), spec(half()...))
	if err != nil {
		t.Fatalf("a missing risk-free series failed the whole backtest: %v", err)
	}
	if result.RiskFree != "" {
		t.Errorf("riskFree = %q, want empty", result.RiskFree)
	}
	if result.Portfolio.Sharpe != nil || result.Portfolio.Sortino != nil {
		t.Error("ratios were computed with no risk-free rate to compute them against")
	}
	if math.Abs(result.Portfolio.End-1010) > 1e-9 {
		t.Errorf("final balance = %.4f, want the usual 1010", result.Portfolio.End)
	}
}

// flatWithRaw is thirteen months at a steady 100, with the unadjusted price
// held at 50 — deliberately different, so a yield computed off the wrong
// series is off by exactly a factor of two and cannot pass by accident.
func flatWithRaw(first string, months int, adjusted, raw float64) []quotes.Bar {
	bars := monthlyBars(first, make([]float64, months)...)
	for i := range bars {
		bars[i].Close = adjusted
		bars[i].Raw = raw
	}
	return bars
}

func TestYieldDividesPayoutsByThePriceOfTheirOwnDay(t *testing.T) {
	// December 2019 through December 2020, so 2020 is a whole year. 1,000 buys
	// 20 shares at the unadjusted 50; four payouts of 0.25 is 1 per share, so
	// 20 of income on 1,000 — a yield of 2%.
	eng := dividendEngine(t,
		map[string][]quotes.Bar{"INCOME": flatWithRaw("2019-12", 13, 100, 50)},
		map[string][]quotes.Distribution{"INCOME": {
			{Date: "2020-03-15", Amount: 0.25},
			{Date: "2020-06-15", Amount: 0.25},
			{Date: "2020-09-15", Amount: 0.25},
			{Date: "2020-12-15", Amount: 0.25},
		}})

	result, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "INCOME", Weight: 100}))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if len(result.Annual) != 1 || result.Annual[0].Yield == nil {
		t.Fatalf("no yield for 2020: %+v", result.Annual)
	}
	// 4% would be the answer from the adjusted close, which is the mistake this
	// pins: a payout is per share in the money of its own day.
	if got := *result.Annual[0].Yield; math.Abs(got-2) > 1e-9 {
		t.Errorf("2020 yielded %.4f%%, want 2 — 20 shares at 50 collecting 1 each on 1,000", got)
	}
}

func TestYieldCountsOnlyWhatWasPaidWhileHeld(t *testing.T) {
	// The run starts in June, so the March payout went to whoever held it then.
	eng := dividendEngine(t,
		map[string][]quotes.Bar{"INCOME": flatWithRaw("2020-06", 7, 100, 50)},
		map[string][]quotes.Distribution{"INCOME": {
			{Date: "2020-03-15", Amount: 10},
			{Date: "2020-09-15", Amount: 1},
		}})

	result, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "INCOME", Weight: 100}))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if len(result.Annual) != 1 || result.Annual[0].Yield == nil {
		t.Fatalf("no yield row: %+v", result.Annual)
	}
	// 20 shares collecting 1 each on 1,000 is 2%. Counting March too would be
	// 22% — income the portfolio never received.
	if got := *result.Annual[0].Yield; math.Abs(got-2) > 1e-9 {
		t.Errorf("yield = %.4f%%, want 2; a payout made before the run began is not its income", got)
	}
	if !result.Annual[0].Partial {
		t.Error("a run starting in June has a part 2020, and the yield row has to say so")
	}
	// A part year is not the portfolio's yield, for the same reason it is not
	// its best year: it collected a fraction of the distributions.
	if result.Portfolio.Yield != nil {
		t.Errorf("summary yield = %v from part years only, want none", *result.Portfolio.Yield)
	}
}

func TestYieldIsUnknownRatherThanZeroWithoutADividendFeed(t *testing.T) {
	// A source with prices and no payouts. Reporting 0% would say every income
	// fund on earth yields nothing.
	eng, _ := backtestEngine(t, map[string][]quotes.Bar{"UP": rising, "DOWN": falling})

	result, err := eng.Backtest(context.Background(), spec(half()...))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	for _, year := range result.Annual {
		if year.Yield != nil {
			t.Errorf("%d reported a yield of %v from a source that has no dividend data",
				year.Year, *year.Yield)
		}
	}
	if result.Portfolio.Yield != nil {
		t.Errorf("summary yield = %v, want none", *result.Portfolio.Yield)
	}
}

func TestYieldOfSomethingThatPaysNothingIsZeroNotUnknown(t *testing.T) {
	// The other half of the distinction: the feed works, this fund just doesn't
	// distribute. That is a 0 worth printing.
	eng := dividendEngine(t,
		map[string][]quotes.Bar{"GROWTH": flatWithRaw("2019-12", 13, 100, 100)},
		map[string][]quotes.Distribution{"GROWTH": nil})

	result, err := eng.Backtest(context.Background(), spec(store.Holding{Symbol: "GROWTH", Weight: 100}))
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}
	if len(result.Annual) != 1 || result.Annual[0].Yield == nil {
		t.Fatalf("yield went missing for a working feed: %+v", result.Annual)
	}
	if *result.Annual[0].Yield != 0 {
		t.Errorf("yield = %v, want 0", *result.Annual[0].Yield)
	}
}

func TestYieldWeighsHoldingsByWhatIsActuallyHeld(t *testing.T) {
	// Two halves, one of which triples over the first year while the other is
	// flat. By 2021 the payer is three quarters of the portfolio, and its
	// income counts for what it grew into rather than for its target weight.
	payer := monthlyBars("2019-12", 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100,
		100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100)
	for i := range payer {
		payer[i].Raw = payer[i].Close
	}
	// Triple it across 2020 so the weights have genuinely drifted by 2021.
	for i := 1; i <= 12; i++ {
		payer[i].Close = 100 + float64(i)*200/12
		payer[i].Raw = payer[i].Close
	}
	for i := 13; i < len(payer); i++ {
		payer[i].Close, payer[i].Raw = 300, 300
	}

	eng := dividendEngine(t,
		map[string][]quotes.Bar{
			"PAYER": payer,
			"FLAT":  flatWithRaw("2019-12", 25, 100, 100),
		},
		map[string][]quotes.Distribution{"PAYER": {{Date: "2021-06-15", Amount: 30}}})

	result, err := eng.Backtest(context.Background(), BacktestSpec{
		Holdings:      []store.Holding{{Symbol: "PAYER", Weight: 50}, {Symbol: "FLAT", Weight: 50}},
		InitialAmount: 1000,
		Rebalance:     store.RebalanceNone,
	})
	if err != nil {
		t.Fatalf("backtest: %v", err)
	}

	var yield2021 *float64
	for _, year := range result.Annual {
		if year.Year == 2021 {
			yield2021 = year.Yield
		}
	}
	if yield2021 == nil {
		t.Fatalf("no 2021 yield: %+v", result.Annual)
	}
	// Entering 2021 the payer is worth 1,500 of a 2,000 portfolio: 5 shares at
	// 300, collecting 30 each is 150, which is 7.5% of 2,000. Using the 50%
	// target weight instead would give 5%.
	if math.Abs(*yield2021-7.5) > 1e-6 {
		t.Errorf("2021 yielded %.4f%%, want 7.5 — income follows what was held, not what was aimed at",
			*yield2021)
	}
}

func TestMaxDrawdownIsThePeakToTroughFallAndWhenItWasRecovered(t *testing.T) {
	months := []string{"2020-01", "2020-02", "2020-03", "2020-04", "2020-05", "2020-06"}
	balances := []float64{100, 120, 60, 90, 130, 110}

	depth, peak, trough, recovered := maxDrawdown(months, balances)

	if math.Abs(depth-50) > 1e-9 {
		t.Errorf("drawdown = %.4f%%, want 50 (120 down to 60)", depth)
	}
	if peak != "2020-02" || trough != "2020-03" {
		t.Errorf("drawdown ran %s → %s, want 2020-02 → 2020-03", peak, trough)
	}
	// The later, shallower fall from 130 must not be reported as the deepest
	// just because it is the most recent.
	if recovered != "2020-05" {
		t.Errorf("recovered at %q, want 2020-05 — the first month back above the old peak", recovered)
	}
}

func TestMaxDrawdownLeavesAnUnrecoveredFallOpen(t *testing.T) {
	months := []string{"2020-01", "2020-02", "2020-03"}
	_, _, _, recovered := maxDrawdown(months, []float64{100, 50, 70})

	if recovered != "" {
		t.Errorf("recovered = %q; a portfolio still below its peak has not recovered, and saying "+
			"it did is the one thing a drawdown row must not do", recovered)
	}
}

func TestMeasureWithholdsAnAnnualRateFromARunTooShortToHaveOne(t *testing.T) {
	months := []string{"2020-01", "2020-02", "2020-03"}
	balances := []float64{1000, 1100, 1200}

	run := lump(balances)
	m := measure("Portfolio", months, run, annualReturns(months, run.index), nil)

	if m.CAGR != nil {
		t.Errorf("CAGR = %v for a two-month run; annualising that is a forecast wearing a "+
			"measurement's clothes", *m.CAGR)
	}
	if math.Abs(m.TotalPercent-20) > 1e-9 {
		t.Errorf("total = %.4f%%, want 20 — the number a short run does have", m.TotalPercent)
	}
}

func TestMeasureCompoundsTheAnnualRateOverTheRunsRealLength(t *testing.T) {
	months := make([]string, 25)
	balances := make([]float64, 25)
	for i := range months {
		months[i] = time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, i, 0).Format("2006-01")
		// Doubling over exactly two years.
		balances[i] = 1000 * math.Pow(2, float64(i)/24)
	}

	run := lump(balances)
	m := measure("Portfolio", months, run, annualReturns(months, run.index), nil)

	if m.CAGR == nil {
		t.Fatal("no CAGR for a two-year run")
	}
	want := (math.Sqrt2 - 1) * 100
	if math.Abs(*m.CAGR-want) > 1e-6 {
		t.Errorf("CAGR = %.6f%%, want %.6f%% — doubling over two years", *m.CAGR, want)
	}
}
