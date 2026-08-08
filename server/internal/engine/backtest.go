package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// ErrBadSpec is every way a portfolio can be un-simulatable *because of what
// was asked for* rather than because something broke: a symbol the source has
// never heard of, holdings whose histories don't overlap, a window too short to
// contain a single monthly return.
//
// It has a sentinel because the API has to answer 400 for all of them — telling
// somebody their portfolio is a server error sends them to the logs for a
// problem they can fix in the form.
var ErrBadSpec = errors.New("this portfolio cannot be backtested")

// BacktestSpec is one simulation request. It is store.Portfolio without the
// stored-row parts, so an unsaved allocation typed into the form and a saved
// one run the same code.
type BacktestSpec struct {
	Holdings              []store.Holding
	InitialAmount         float64
	StartYear             int
	EndYear               int
	Rebalance             string
	Contribution          float64
	ContributionFrequency string
	Benchmark             string
}

// Backtest is what a simulation produced.
//
// Everything in it is monthly. Daily bars are what the provider gives and what
// the series is built from, but a portfolio's answer does not get more true at
// daily resolution — it gets forty times bigger, and it starts implying that
// the rebalance happened on a particular Tuesday.
type Backtest struct {
	// Start and End are the months actually simulated, YYYY-MM. They can differ
	// from what was asked for, which is what Notes explains.
	Start string `json:"start"`
	End   string `json:"end"`
	// Months is how many monthly returns went into this, which is Start to End
	// exclusive of the base month.
	Months     int `json:"months"`
	Rebalances int `json:"rebalances"`

	Initial float64 `json:"initial"`
	// Contributed is the total paid in after the initial amount. The gap
	// between Initial+Contributed and the final balance is what was earned;
	// without it a reader has no way to tell growth from deposits.
	Contributed float64 `json:"contributed"`
	// Points is the growth of the initial amount, month by month, starting at
	// Start with exactly Initial.
	Points []Balance `json:"points"`
	// Portfolio and Benchmark are the same measurements over the same months.
	// Benchmark is nil when none was asked for.
	Portfolio Metrics  `json:"portfolio"`
	Benchmark *Metrics `json:"benchmark"`
	// Annual is one row per calendar year the run touched, oldest first.
	Annual []AnnualReturn `json:"annual"`
	// Holdings echoes the allocation with the weight actually used and the
	// first month each symbol has data for — the numbers behind Notes.
	Holdings []HoldingResult `json:"holdings"`
	// RiskFree names the series Sharpe and Sortino were measured against, or is
	// empty when it couldn't be fetched and both are therefore unset. The UI
	// says which rate the ratios used rather than leaving a reader to assume.
	RiskFree string `json:"riskFree"`
	// Notes are the things a reader would otherwise have to infer from a date
	// that isn't the one they typed.
	Notes []string `json:"notes"`
}

// Balance is one month's closing value.
type Balance struct {
	Month     string   `json:"month"`
	Value     float64  `json:"value"`
	Benchmark *float64 `json:"benchmark"`
}

// AnnualReturn is one calendar year's move.
type AnnualReturn struct {
	Year      int      `json:"year"`
	Percent   float64  `json:"percent"`
	Benchmark *float64 `json:"benchmark"`
	// Partial marks a year the run did not cover end to end — the first and
	// last years nearly always. Reporting "1996: +8.4%" for a run that started
	// in June would be a fabrication, and dropping the row loses real
	// information, so it is shown and labelled.
	Partial bool `json:"partial"`
}

// HoldingResult is one line of the allocation as it was actually simulated.
type HoldingResult struct {
	Symbol string `json:"symbol"`
	// Weight is the target as a percentage, renormalised to sum to exactly 100
	// — three holdings of 33.33 are simulated as three exact thirds rather
	// than as a portfolio that quietly starts 0.01% in nothing.
	Weight float64 `json:"weight"`
	// FirstMonth is the earliest month this symbol has a close for, which is
	// what sets the run's start when it is the latest of them.
	FirstMonth string `json:"firstMonth"`
}

// Metrics is the summary table: what a strategy returned and what it put you
// through to return it.
type Metrics struct {
	Label string  `json:"label"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// TotalPercent is the whole-period move. CAGR is nil for a run shorter than
	// a year, where annualising a few months into a yearly rate is arithmetic
	// pretending to be a forecast.
	TotalPercent float64  `json:"totalPercent"`
	CAGR         *float64 `json:"cagrPercent"`
	// Stdev is the annualised standard deviation of the monthly returns — the
	// conventional volatility number, monthly deviation times the root of
	// twelve. Nil when there are fewer than two returns to deviate.
	Stdev *float64 `json:"stdevPercent"`
	// Sharpe is return above the risk-free rate per unit of volatility, and
	// Sortino is the same over *downside* volatility only.
	//
	// The pair is worth having together: a strategy whose swings are mostly
	// upward is punished by Sharpe and left alone by Sortino, and the gap
	// between the two numbers is itself the finding. Both are nil when the
	// risk-free series could not be fetched — a Sharpe silently computed
	// against 0% is a different statistic wearing the same name.
	Sharpe  *float64 `json:"sharpe"`
	Sortino *float64 `json:"sortino"`
	// Best and Worst are full calendar years only, so a run that started in
	// October can't report its first three months as its worst year.
	Best  *AnnualReturn `json:"bestYear"`
	Worst *AnnualReturn `json:"worstYear"`
	// MaxDrawdown is the deepest peak-to-trough fall, as a positive percentage,
	// with the months it ran between. Measured on the monthly series: a daily
	// low between two month ends is not in this data and is not claimed.
	MaxDrawdown     float64 `json:"maxDrawdownPercent"`
	DrawdownPeak    string  `json:"drawdownPeak"`
	DrawdownTrough  string  `json:"drawdownTrough"`
	DrawdownRecover string  `json:"drawdownRecovered"`
}

// cagrMinYears is how long a run has to be before a compound annual rate is
// reported. A year exactly is allowed — over that period the rate *is* the
// total return, and printing it keeps the column honest rather than empty.
const cagrMinYears = 1.0

// riskFreeSymbol is what Sharpe and Sortino measure against: Yahoo's 13-week
// Treasury bill, which is the short government rate every textbook means by
// "risk-free" and the closest public series to the 1-month bill the commercial
// tools use.
//
// It is a constant rather than a setting because it is not a preference — it is
// the definition of the statistic, and a Sharpe ratio computed against
// something else is a different number with the same name. It is fetched
// best-effort: a source that has never heard of it leaves both ratios unset
// rather than failing the backtest.
const riskFreeSymbol = "^IRX"

// riskFreeRates turns the bill's quoted yields into the monthly returns a
// portfolio's own returns are measured against.
//
// Two conversions, both worth naming. The series is quoted as an *annualised
// percentage* rather than as a price, so a month's share of it is the rate over
// twelve — nothing here is compounding a price series. And the last known rate
// is carried forward across gaps, which price series are never allowed to do:
// a rate is a level that stays in force until it changes, where a missing close
// is a day that did not trade.
//
// Returns nil when the bill has nothing at or before the run's first month,
// because carrying a rate *backwards* would be inventing monetary policy.
func riskFreeRates(months []string, bill map[string]float64) []float64 {
	if len(bill) == 0 {
		return nil
	}
	// The rate in force at the run's start was very likely set before it.
	opening := ""
	for month := range bill {
		if month <= months[0] && month > opening {
			opening = month
		}
	}
	if opening == "" {
		return nil
	}
	inForce := bill[opening]

	rates := make([]float64, len(months))
	for k, month := range months {
		if rate, ok := bill[month]; ok {
			inForce = rate
		}
		rates[k] = inForce / 100 / 12
	}
	return rates
}

// riskAdjusted computes Sharpe and Sortino from a growth index and the monthly
// risk-free returns beside it.
//
// Sortino's denominator divides by *every* period rather than by the losing
// ones — the conventional target semideviation. Dividing by the count of
// downside months instead would flatter a strategy that rarely falls by
// measuring its rare falls against themselves.
func riskAdjusted(index, rates []float64) (sharpe, sortino *float64) {
	if len(rates) != len(index) || len(index) < 3 {
		return nil, nil
	}

	excess := make([]float64, 0, len(index)-1)
	for k := 1; k < len(index); k++ {
		if index[k-1] <= 0 {
			continue
		}
		excess = append(excess, index[k]/index[k-1]-1-rates[k])
	}
	if len(excess) < 2 {
		return nil, nil
	}

	mean := 0.0
	for _, r := range excess {
		mean += r
	}
	mean /= float64(len(excess))

	variance, downside := 0.0, 0.0
	for _, r := range excess {
		variance += (r - mean) * (r - mean)
		if r < 0 {
			downside += r * r
		}
	}
	variance /= float64(len(excess) - 1)
	downside /= float64(len(excess))

	root12 := math.Sqrt(12)
	if sd := math.Sqrt(variance); sd > 0 {
		value := mean / sd * root12
		sharpe = &value
	}
	if dd := math.Sqrt(downside); dd > 0 {
		value := mean / dd * root12
		sortino = &value
	}
	return sharpe, sortino
}

// Backtest simulates one allocation against the provider's daily history.
//
// It shares symbolHistory with the performance sheet, so a backtest run just
// after a chart of one of its holdings costs nothing for that leg — and two
// portfolios over the same funds cost one fetch between them.
func (e *Engine) Backtest(ctx context.Context, spec BacktestSpec) (Backtest, error) {
	historian, ok := e.provider.(quotes.Historian)
	if !ok {
		return Backtest{}, quotes.ErrNoHistory
	}
	// Same reason every other upstream path calls this: a base URL or timeout
	// edited in Settings has to be in force now, not after a restart.
	e.syncProvider()

	holdings, err := weights(spec.Holdings)
	if err != nil {
		return Backtest{}, err
	}
	benchmark := store.NormalizeSymbol(spec.Benchmark)

	// One fetch per distinct symbol. A benchmark that is also a holding — a
	// perfectly reasonable thing to compare against — costs one series, not two.
	series := map[string]map[string]float64{}
	firstMonth := map[string]string{}
	for _, h := range holdings {
		if err := e.loadMonthly(ctx, historian, h.Symbol, series, firstMonth); err != nil {
			return Backtest{}, err
		}
	}
	if benchmark != "" {
		if err := e.loadMonthly(ctx, historian, benchmark, series, firstMonth); err != nil {
			return Backtest{}, err
		}
	}

	// Every leg is intersected, the benchmark included. A comparison drawn over
	// months one side didn't trade is not a comparison, and truncating both to
	// the common period is the only way the two curves on the chart can be read
	// against each other at all.
	months := window(commonMonths(series), spec.StartYear, spec.EndYear)
	if len(months) < 2 {
		return Backtest{}, fmt.Errorf(
			"%w: its holdings share %d month(s) of history in that period, which is not enough for a single monthly return",
			ErrBadSpec, len(months))
	}

	initial := spec.InitialAmount
	if initial <= 0 {
		initial = 10000
	}

	cash := plan{
		initial:      initial,
		contribution: spec.Contribution,
		frequency:    spec.ContributionFrequency,
		rebalance:    spec.Rebalance,
	}
	if cash.frequency == "" {
		cash.frequency = store.RebalanceNone
	}

	// Best-effort, and deliberately after the months are settled: the bill is
	// not a leg, so it must never narrow the run or fail it. A source that has
	// never heard of it simply leaves both ratios unset.
	var rates []float64
	if bill, err := e.riskFree(ctx, historian); err == nil {
		rates = riskFreeRates(months, bill)
	}
	run := simulate(holdings, series, months, cash)
	annual := annualReturns(months, run.index)

	out := Backtest{
		Start:       months[0],
		End:         months[len(months)-1],
		Months:      len(months) - 1,
		Rebalances:  run.rebalances,
		Initial:     initial,
		Contributed: run.contributed,
		Points:      make([]Balance, len(months)),
		Annual:      annual,
		Holdings:    make([]HoldingResult, len(holdings)),
		Portfolio:   measure("Portfolio", months, run, annual, rates),
		Notes:       startNotes(months[0], holdings, benchmark, firstMonth),
	}
	if rates != nil {
		out.RiskFree = riskFreeSymbol
	}

	// The benchmark is one holding at 100%, run over the same months with the
	// same cash flows by the same code — which is what makes it a comparison
	// rather than a second implementation that might disagree. Only the
	// rebalancing is dropped: a single holding has nothing to rebalance against.
	var bench *result
	if benchmark != "" {
		benchCash := cash
		benchCash.rebalance = store.RebalanceNone
		benchRun := simulate(
			[]HoldingResult{{Symbol: benchmark, Weight: 100}}, series, months, benchCash)
		bench = &benchRun

		benchAnnual := annualReturns(months, benchRun.index)
		benchMetrics := measure(benchmark, months, benchRun, benchAnnual, rates)
		out.Benchmark = &benchMetrics

		byYear := make(map[int]float64, len(benchAnnual))
		for _, r := range benchAnnual {
			byYear[r.Year] = r.Percent
		}
		for i := range out.Annual {
			if pct, ok := byYear[out.Annual[i].Year]; ok {
				value := pct
				out.Annual[i].Benchmark = &value
			}
		}
	}

	for i, m := range months {
		out.Points[i] = Balance{Month: m, Value: run.balances[i]}
		if bench != nil {
			value := bench.balances[i]
			out.Points[i].Benchmark = &value
		}
	}
	for i, h := range holdings {
		h.FirstMonth = firstMonth[h.Symbol]
		out.Holdings[i] = h
	}
	return out, nil
}

// riskFree reads the Treasury bill's month-end yields, through the same cache
// every other series uses — so a page of portfolios costs one fetch of it
// between them all rather than one each.
func (e *Engine) riskFree(ctx context.Context, h quotes.Historian) (map[string]float64, error) {
	bars, err := e.symbolHistory(ctx, h, riskFreeSymbol, historyStart())
	if err != nil {
		return nil, err
	}
	rates, _ := monthEnds(bars)
	return rates, nil
}

// loadMonthly fetches one symbol's daily bars and reduces them to month-end
// closes, recording the first month it has.
func (e *Engine) loadMonthly(ctx context.Context, h quotes.Historian, symbol string,
	series map[string]map[string]float64, firstMonth map[string]string) error {
	if _, done := series[symbol]; done {
		return nil
	}
	bars, err := e.symbolHistory(ctx, h, symbol, historyStart())
	if err != nil {
		// A symbol the source has never heard of is a typo in the form, not an
		// outage: say so as a bad request rather than as a failure.
		if errors.Is(err, quotes.ErrNotFound) {
			return fmt.Errorf("%w: %s is not a symbol the quote source knows", ErrBadSpec, symbol)
		}
		return fmt.Errorf("%s: %w", symbol, err)
	}
	closes, first := monthEnds(bars)
	if len(closes) == 0 {
		return fmt.Errorf("%w: the quote source has no price history for %s", ErrBadSpec, symbol)
	}
	series[symbol] = closes
	firstMonth[symbol] = first
	return nil
}

// monthEnds reduces daily bars to one close per month — the last session of
// each, which is the month's close.
//
// Adjusted closes are what the provider gives (see quotes.historyCloses), so
// each month's number already carries dividends and splits. That is the whole
// reason a backtest over funds is possible at all here: the total-return series
// is the series, and nothing has to model a distribution.
func monthEnds(bars []quotes.Bar) (map[string]float64, string) {
	closes := make(map[string]float64, len(bars)/20)
	first := ""
	for _, b := range bars {
		if len(b.Date) < 7 {
			continue
		}
		month := b.Date[:7]
		// Bars arrive oldest first, so the last write for a month wins and is
		// that month's final session.
		closes[month] = b.Close
		if first == "" || month < first {
			first = month
		}
	}
	return closes, first
}

// commonMonths is every month all the series have, oldest first.
//
// Intersected rather than carried forward, for the reason compositeSeries
// intersects its legs: a month one holding didn't trade and another did would
// otherwise contribute a return for one side of the portfolio and a flat line
// for the other, which is a rebalance nobody performed.
func commonMonths(series map[string]map[string]float64) []string {
	if len(series) == 0 {
		return nil
	}
	var shortest map[string]float64
	for _, closes := range series {
		if shortest == nil || len(closes) < len(shortest) {
			shortest = closes
		}
	}

	months := make([]string, 0, len(shortest))
	for month := range shortest {
		complete := true
		for _, closes := range series {
			if _, ok := closes[month]; !ok {
				complete = false
				break
			}
		}
		if complete {
			months = append(months, month)
		}
	}
	sort.Strings(months) // YYYY-MM sorts lexically into chronological order.
	return months
}

// window narrows the months to the requested years.
//
// The start is December of the year *before* the one asked for, when that month
// exists: a run "from 1996" should have 1996's own return measured from the end
// of 1995, not from the end of January. Where that month isn't there, the run
// starts at the first month it has and the first calendar year is marked
// partial rather than quietly measured from the wrong place.
func window(months []string, startYear, endYear int) []string {
	if startYear > 0 {
		months = months[sort.SearchStrings(months, fmt.Sprintf("%04d-12", startYear-1)):]
	}
	if endYear > 0 {
		to := fmt.Sprintf("%04d-12", endYear)
		months = months[:sort.Search(len(months), func(i int) bool { return months[i] > to })]
	}
	return months
}

// startNotes explains a start date that isn't the one the reader expected, by
// naming the holding responsible. "Starts 1996-06" is a fact; "starts 1996-06
// because VGSIX has nothing earlier" is the fact plus what to change.
func startNotes(start string, holdings []HoldingResult, benchmark string, firstMonth map[string]string) []string {
	late := []string{}
	for _, h := range holdings {
		if firstMonth[h.Symbol] == start {
			late = append(late, h.Symbol)
		}
	}
	if benchmark != "" && firstMonth[benchmark] == start {
		late = append(late, benchmark)
	}
	if len(late) == 0 {
		return []string{}
	}
	return []string{fmt.Sprintf("%s has no history before %s, which is where the run begins.",
		strings.Join(late, " and "), start)}
}

// weights renormalises the allocation to sum to exactly 1 and rejects what
// can't be simulated.
//
// The store validates a portfolio to the same rules before saving one, but a
// spec can also arrive straight from the form without ever being saved — so
// this checks rather than assumes.
func weights(holdings []store.Holding) ([]HoldingResult, error) {
	if len(holdings) == 0 {
		return nil, fmt.Errorf("%w: it has no holdings", ErrBadSpec)
	}
	total := 0.0
	for _, h := range holdings {
		if store.NormalizeSymbol(h.Symbol) == "" {
			return nil, fmt.Errorf("%w: every holding needs a symbol", ErrBadSpec)
		}
		if h.Weight <= 0 {
			return nil, fmt.Errorf("%w: %s needs a weight above 0%%", ErrBadSpec, store.NormalizeSymbol(h.Symbol))
		}
		total += h.Weight
	}
	if total <= 0 {
		return nil, fmt.Errorf("%w: the weights add up to nothing", ErrBadSpec)
	}

	out := make([]HoldingResult, 0, len(holdings))
	for _, h := range holdings {
		out = append(out, HoldingResult{
			Symbol: store.NormalizeSymbol(h.Symbol),
			Weight: h.Weight / total * 100,
		})
	}
	return out, nil
}

// plan is the money side of a simulation: what goes in, and when.
type plan struct {
	initial      float64
	contribution float64
	frequency    string
	rebalance    string
}

// result is one pass of the simulation.
//
// The two series exist because a contribution is not a return. Money paid in
// raises the balance without anything having been earned, so every *return*
// number — total, CAGR, volatility, drawdown, the yearly table — is measured on
// `index`, the growth of a single unit with the cash flows taken out, while
// every *money* number is read off `balances`. Compute a CAGR from a balance
// that has been topped up monthly for thirty years and you get a spectacular
// figure describing nothing.
//
// With no contributions the two are proportional and the distinction costs
// nothing.
type result struct {
	balances    []float64
	index       []float64
	rebalances  int
	contributed float64
}

// simulate walks the months, compounding each holding by its own return,
// paying in on the contribution cadence and rebalancing on the rebalancing one.
//
// Between rebalances the weights drift, which is the point: a 60/40 that has
// not been touched for a decade is a 78/22, and a backtest that silently held
// it at 60/40 would be reporting a strategy nobody ran.
func simulate(holdings []HoldingResult, series map[string]map[string]float64,
	months []string, p plan) result {
	values := make([]float64, len(holdings))
	for i, h := range holdings {
		values[i] = p.initial * h.Weight / 100
	}

	out := result{
		balances: make([]float64, len(months)),
		index:    make([]float64, len(months)),
	}
	out.balances[0] = p.initial
	out.index[0] = 1

	for k := 1; k < len(months); k++ {
		total := 0.0
		for i, h := range holdings {
			previous := series[h.Symbol][months[k-1]]
			current := series[h.Symbol][months[k]]
			if previous > 0 {
				values[i] *= current / previous
			}
			total += values[i]
		}

		// The month's return is measured before anything is paid in, against a
		// balance that already includes last month's contribution. That is the
		// time-weighted return: what the holdings did, uncontaminated by when
		// money happened to arrive.
		if out.balances[k-1] > 0 {
			out.index[k] = out.index[k-1] * (total / out.balances[k-1])
		} else {
			out.index[k] = out.index[k-1]
		}

		if p.contribution > 0 && onBoundary(months[k], p.frequency) {
			// A contribution in the final month is kept, unlike a rebalance:
			// it never earns anything, but it is money that genuinely went in,
			// and a balance that omitted it would not be the account's.
			for i, h := range holdings {
				values[i] += p.contribution * h.Weight / 100
			}
			total += p.contribution
			out.contributed += p.contribution
		}
		out.balances[k] = total

		// The last month never rebalances: it would move money at the closing
		// bell of the run and change nothing about the answer, while counting
		// as a trade that happened.
		if k < len(months)-1 && onBoundary(months[k], p.rebalance) {
			for i, h := range holdings {
				values[i] = total * h.Weight / 100
			}
			out.rebalances++
		}
	}
	return out
}

// onBoundary reports whether this month closes one of the cadence's periods.
// Boundaries are calendar ones — December for a yearly cadence, not "twelve
// months after the run happened to start" — because that is when somebody doing
// this by hand would actually do it. Rebalancing and contributing share it, for
// the same reason they share a vocabulary.
func onBoundary(month, cadence string) bool {
	switch cadence {
	case store.RebalanceMonthly:
		return true
	case store.RebalanceQuarterly:
		switch month[5:] {
		case "03", "06", "09", "12":
			return true
		}
		return false
	case store.RebalanceAnnually:
		return month[5:] == "12"
	default: // store.RebalanceNone, and anything unrecognised.
		return false
	}
}

// annualReturns is one row per calendar year the run touched.
//
// A year's baseline is the balance at the end of the previous December where
// the run has one, and the run's own first month where it doesn't — which is
// what makes the first year partial. The last year is partial whenever the run
// ends before December.
func annualReturns(months []string, balances []float64) []AnnualReturn {
	if len(months) < 2 {
		return []AnnualReturn{}
	}
	lastOf := map[int]int{}
	order := []int{}
	for i, m := range months {
		year, err := strconv.Atoi(m[:4])
		if err != nil {
			continue
		}
		if _, seen := lastOf[year]; !seen {
			order = append(order, year)
		}
		lastOf[year] = i
	}
	// A run beginning at a December has that December as its baseline and
	// nothing else in that year, so the year gets no row. A run beginning in
	// May does have returns in its first year — seven months of them — and
	// they are reported as a part year, exactly as the final year is. Showing
	// one and dropping the other would be the inconsistency.
	if len(order) > 0 && lastOf[order[0]] == 0 {
		order = order[1:]
	}

	out := make([]AnnualReturn, 0, len(order))
	for _, year := range order {
		end := lastOf[year]
		// The baseline is the last month before this year; index 0 when the
		// year is the run's first, which is exactly the base month.
		start := 0
		for i := end; i >= 0; i-- {
			if months[i][:4] != months[end][:4] {
				start = i
				break
			}
		}
		if balances[start] <= 0 {
			continue
		}
		out = append(out, AnnualReturn{
			Year:    year,
			Percent: (balances[end]/balances[start] - 1) * 100,
			// Full means measured from the previous December to this one.
			Partial: months[start][5:] != "12" || months[end][5:] != "12",
		})
	}
	return out
}

// measure computes the summary table for one run.
//
// Money comes off the balances and returns come off the index — see result.
// Without contributions the two agree; with them, "final balance" and "total
// return" answer different questions and both are wanted.
func measure(label string, months []string, run result, annual []AnnualReturn, rates []float64) Metrics {
	balances, index := run.balances, run.index
	m := Metrics{
		Label: label,
		Start: balances[0],
		End:   balances[len(balances)-1],
	}
	growth := index[len(index)-1] / index[0]
	m.TotalPercent = (growth - 1) * 100

	years := float64(len(index)-1) / 12
	if years >= cagrMinYears && growth > 0 {
		cagr := (math.Pow(growth, 1/years) - 1) * 100
		m.CAGR = &cagr
	}

	if stdev, ok := annualisedStdev(index); ok {
		m.Stdev = &stdev
	}
	m.Sharpe, m.Sortino = riskAdjusted(index, rates)

	for i := range annual {
		if annual[i].Partial {
			continue
		}
		year := annual[i]
		if m.Best == nil || year.Percent > m.Best.Percent {
			best := year
			m.Best = &best
		}
		if m.Worst == nil || year.Percent < m.Worst.Percent {
			worst := year
			m.Worst = &worst
		}
	}

	// On the index, not the balances: a portfolio paid into every month can be
	// falling hard while its balance still climbs, and a drawdown row that
	// never fires because the contributions covered the fall is worse than
	// none at all.
	m.MaxDrawdown, m.DrawdownPeak, m.DrawdownTrough, m.DrawdownRecover = maxDrawdown(months, index)
	return m
}

// annualisedStdev is the sample standard deviation of the monthly returns,
// scaled by the root of twelve — the conventional way a monthly series is
// quoted as a yearly volatility.
func annualisedStdev(balances []float64) (float64, bool) {
	returns := make([]float64, 0, len(balances)-1)
	for i := 1; i < len(balances); i++ {
		if balances[i-1] <= 0 {
			continue
		}
		returns = append(returns, balances[i]/balances[i-1]-1)
	}
	if len(returns) < 2 {
		return 0, false
	}

	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	sum := 0.0
	for _, r := range returns {
		sum += (r - mean) * (r - mean)
	}
	// Sample rather than population: these months are a sample of the
	// strategy's behaviour, not the whole of it.
	return math.Sqrt(sum/float64(len(returns)-1)) * math.Sqrt(12) * 100, true
}

// maxDrawdown is the deepest peak-to-trough fall as a positive percentage, the
// months it ran between, and the month it was recovered — empty if it never
// was, which is the case a reader most wants to see.
func maxDrawdown(months []string, balances []float64) (depth float64, peak, trough, recovered string) {
	peakIndex, peakValue := 0, balances[0]
	worst, worstPeak, worstTrough := 0.0, 0, 0

	for i, v := range balances {
		if v > peakValue {
			peakValue, peakIndex = v, i
		}
		if peakValue > 0 {
			if fall := (peakValue - v) / peakValue; fall > worst {
				worst, worstPeak, worstTrough = fall, peakIndex, i
			}
		}
	}
	if worst == 0 {
		return 0, "", "", ""
	}

	for i := worstTrough; i < len(balances); i++ {
		if balances[i] >= balances[worstPeak] {
			recovered = months[i]
			break
		}
	}
	return worst * 100, months[worstPeak], months[worstTrough], recovered
}

// backtestSpec builds a spec from a stored portfolio, so a saved allocation and
// a typed one reach Backtest as the same thing.
func backtestSpec(p store.Portfolio) BacktestSpec {
	return BacktestSpec{
		Holdings:              p.Holdings,
		InitialAmount:         p.InitialAmount,
		StartYear:             p.StartYear,
		EndYear:               p.EndYear,
		Rebalance:             p.Rebalance,
		Contribution:          p.Contribution,
		ContributionFrequency: p.ContributionFrequency,
		Benchmark:             p.Benchmark,
	}
}

// BacktestPortfolio runs a saved portfolio.
func (e *Engine) BacktestPortfolio(ctx context.Context, id string) (Backtest, error) {
	p, err := e.store.Portfolio(id)
	if err != nil {
		return Backtest{}, err
	}
	return e.Backtest(ctx, backtestSpec(p))
}
