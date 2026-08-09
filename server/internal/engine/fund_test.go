package engine

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// fakeCompositor is a historian that can also say what a fund holds.
type fakeCompositor struct {
	*fakeHistorian
	holdings map[string]quotes.Composition
	err      error
	calls    int
}

func (f *fakeCompositor) Constituents(_ context.Context, symbol string) (quotes.Composition, error) {
	f.calls++
	if f.err != nil {
		return quotes.Composition{}, f.err
	}
	composition, ok := f.holdings[symbol]
	if !ok {
		return quotes.Composition{}, quotes.ErrNotFund
	}
	return composition, nil
}

// fundEngine wires an engine to a source with both series and compositions.
func fundEngine(t *testing.T, bars map[string][]quotes.Bar,
	holdings map[string]quotes.Composition) (*Engine, *fakeCompositor) {
	t.Helper()
	historian := newFakeHistorian(map[string]float64{})
	for symbol, series := range bars {
		historian.bars[symbol] = series
	}
	provider := &fakeCompositor{fakeHistorian: historian, holdings: holdings}
	eng, _ := newTestEngine(t, provider)
	return eng, provider
}

// twoYears is a fund that doubles over 2023 and 2024, one bar a month.
func twoYears(from float64, monthly float64, months int) []quotes.Bar {
	closes := make([]float64, months)
	value := from
	for i := range closes {
		closes[i] = value
		value *= monthly
	}
	return monthlyBars("2023-01", closes...)
}

func TestFundReportsItsOwnReturnAndNotItsHoldings(t *testing.T) {
	// The fund climbs; everything it holds falls. If the summary were built from
	// the holdings — the mistake this page exists to avoid — the fund would come
	// back negative.
	eng, _ := fundEngine(t,
		map[string][]quotes.Bar{
			"QQQ": twoYears(100, 1.02, 24),
			"AAA": twoYears(100, 0.95, 24),
			"BBB": twoYears(100, 0.90, 24),
		},
		map[string]quotes.Composition{
			"QQQ": {Name: "A Fund", Holdings: []quotes.Constituent{
				{Symbol: "AAA", Name: "Alpha", Weight: 40},
				{Symbol: "BBB", Name: "Beta", Weight: 25},
			}},
		})

	fund, err := eng.Fund(context.Background(), "qqq", "")
	if err != nil {
		t.Fatalf("looking a fund up: %v", err)
	}

	if fund.Portfolio.TotalPercent <= 0 {
		t.Errorf("the fund's total return came back %.2f%%, so it was measured from its holdings rather than from its own series",
			fund.Portfolio.TotalPercent)
	}
	if fund.Symbol != "QQQ" {
		t.Errorf("the fund's symbol came back as %q rather than normalised to QQQ", fund.Symbol)
	}
	if fund.Name != "A Fund" {
		t.Errorf("the fund's name came back as %q rather than the source's", fund.Name)
	}
	if math.Abs(fund.Covered-65) > 0.001 {
		t.Errorf("the listed holdings cover %.2f%% of the fund; the two weights given add up to 65%%", fund.Covered)
	}
	if len(fund.Notes) == 0 {
		t.Error("a fund page carries no note saying the holdings are today's, which is the one thing a reader has to be told")
	}
}

func TestFundIsNotTruncatedToItsYoungestHolding(t *testing.T) {
	// The whole reason the holdings are not intersected into the run: a fund
	// that has held something since last year has not existed only since then.
	eng, _ := fundEngine(t,
		map[string][]quotes.Bar{
			"QQQ": twoYears(100, 1.02, 24),
			"OLD": twoYears(100, 1.01, 24),
			"NEW": monthlyBars("2023-06", 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28),
		},
		map[string]quotes.Composition{
			"QQQ": {Holdings: []quotes.Constituent{
				{Symbol: "OLD", Weight: 30},
				{Symbol: "NEW", Weight: 20},
			}},
		})

	fund, err := eng.Fund(context.Background(), "QQQ", "")
	if err != nil {
		t.Fatalf("looking a fund up: %v", err)
	}

	if fund.Start != "2023-01" {
		t.Errorf("the run starts at %s; a holding that listed later must not shorten the fund's own history", fund.Start)
	}

	whole := periodByKey(t, fund, "run")
	if !contains(whole.Missing, "NEW") {
		t.Errorf("over the whole run, NEW is reported as measurable — it has no close in %s to measure from", whole.From)
	}
	if !contains(symbolsOf(whole.Returns), "OLD") {
		t.Error("over the whole run, OLD is missing from the returns even though it covers every month of it")
	}

	// And the same holding, over a window it does cover — the half of the claim
	// that makes the other half mean something.
	recent := periodByKey(t, fund, "ytd")
	if !recent.Available {
		t.Fatal("the year to date is unavailable on a two-year run, so this test proves nothing about a holding being dropped")
	}
	if !contains(symbolsOf(recent.Returns), "NEW") {
		t.Errorf("over the year to date (from %s), NEW is dropped even though its series reaches back that far", recent.From)
	}
}

func TestFundListsHoldingsItCannotPrice(t *testing.T) {
	// A fund's cash line, a foreign listing, something delisted last week: in
	// the fund, and not chartable. Dropping it silently would understate what
	// the page admits it is missing.
	eng, _ := fundEngine(t,
		map[string][]quotes.Bar{
			"QQQ": twoYears(100, 1.02, 24),
			"AAA": twoYears(100, 1.01, 24),
		},
		map[string]quotes.Composition{
			"QQQ": {Holdings: []quotes.Constituent{
				{Symbol: "AAA", Weight: 30},
				{Symbol: "CASH", Weight: 5},
			}},
		})

	fund, err := eng.Fund(context.Background(), "QQQ", "")
	if err != nil {
		t.Fatalf("looking a fund up: %v", err)
	}

	if len(fund.Constituents) != 2 {
		t.Fatalf("the page lists %d holdings; both were reported by the source and both belong on it", len(fund.Constituents))
	}
	for _, c := range fund.Constituents {
		if c.Symbol == "CASH" && c.Priced {
			t.Error("CASH is marked as priced, but the quote source has no series for it")
		}
		if c.Symbol == "AAA" && !c.Priced {
			t.Error("AAA is marked as unpriced, but its series was fetched")
		}
	}
	if math.Abs(fund.Covered-35) > 0.001 {
		t.Errorf("coverage came back %.2f%%; a holding that cannot be priced is still part of the fund", fund.Covered)
	}

	whole := periodByKey(t, fund, "run")
	if !contains(whole.Missing, "CASH") {
		t.Error("a holding with no series at all is absent from the period's Missing list, so the card cannot say why it isn't in the table")
	}
}

func TestFundRefusesSomethingThatIsNotAFund(t *testing.T) {
	eng, _ := fundEngine(t,
		map[string][]quotes.Bar{"AAPL": twoYears(100, 1.02, 24)},
		map[string]quotes.Composition{})

	_, err := eng.Fund(context.Background(), "AAPL", "")
	if !errors.Is(err, quotes.ErrNotFund) {
		t.Fatalf("looking up a company gave %v; it has to be ErrNotFund so the API can answer 400 rather than 500", err)
	}
}

func TestFundAsksTheSourceOncePerFund(t *testing.T) {
	// The crumbed endpoint is the fragile one. Opening the same fund twice must
	// not double the number of times it is asked.
	eng, provider := fundEngine(t,
		map[string][]quotes.Bar{
			"QQQ": twoYears(100, 1.02, 24),
			"AAA": twoYears(100, 1.01, 24),
		},
		map[string]quotes.Composition{
			"QQQ": {Holdings: []quotes.Constituent{{Symbol: "AAA", Weight: 30}}},
		})

	for i := 0; i < 3; i++ {
		if _, err := eng.Fund(context.Background(), "QQQ", ""); err != nil {
			t.Fatalf("looking a fund up: %v", err)
		}
	}
	if provider.calls != 1 {
		t.Errorf("the composition was fetched %d times for three page loads; it is cached for %v", provider.calls, constituentsTTL)
	}
}

func TestFundIgnoresABenchmarkThatIsItself(t *testing.T) {
	eng, _ := fundEngine(t,
		map[string][]quotes.Bar{
			"QQQ": twoYears(100, 1.02, 24),
			"AAA": twoYears(100, 1.01, 24),
		},
		map[string]quotes.Composition{
			"QQQ": {Holdings: []quotes.Constituent{{Symbol: "AAA", Weight: 30}}},
		})

	fund, err := eng.Fund(context.Background(), "QQQ", "qqq")
	if err != nil {
		t.Fatalf("looking a fund up: %v", err)
	}
	if fund.Benchmark != nil {
		t.Error("a fund benchmarked against itself came back with a benchmark; it would draw the same line twice and report a gap of zero on every row")
	}
}

// periodByKey pulls one window off a fund, failing rather than panicking when
// the key isn't there.
func periodByKey(t *testing.T, fund Fund, key string) PeriodPerformance {
	t.Helper()
	for _, p := range fund.Performance {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("the fund has no %q period at all", key)
	return PeriodPerformance{}
}

func symbolsOf(returns []HoldingReturn) []string {
	out := make([]string, 0, len(returns))
	for _, r := range returns {
		out = append(out, r.Symbol)
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
