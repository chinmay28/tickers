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
	Holdings      []store.Holding
	InitialAmount float64
	StartYear     int
	EndYear       int
	Rebalance     string
	Benchmark     string
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

	balances, rebalances := simulate(holdings, series, months, initial, spec.Rebalance)
	annual := annualReturns(months, balances)

	result := Backtest{
		Start:      months[0],
		End:        months[len(months)-1],
		Months:     len(months) - 1,
		Rebalances: rebalances,
		Initial:    initial,
		Points:     make([]Balance, len(months)),
		Annual:     annual,
		Holdings:   make([]HoldingResult, len(holdings)),
		Portfolio:  measure("Portfolio", months, balances, annual),
		Notes:      startNotes(months[0], holdings, benchmark, firstMonth),
	}

	// The benchmark is one holding at 100%, run over the same months by the
	// same code — which is what makes it a comparison rather than a second
	// implementation that might disagree.
	var benchBalances []float64
	if benchmark != "" {
		benchBalances, _ = simulate(
			[]HoldingResult{{Symbol: benchmark, Weight: 100}}, series, months, initial, store.RebalanceNone)
		benchAnnual := annualReturns(months, benchBalances)
		benchMetrics := measure(benchmark, months, benchBalances, benchAnnual)
		result.Benchmark = &benchMetrics

		byYear := make(map[int]float64, len(benchAnnual))
		for _, r := range benchAnnual {
			byYear[r.Year] = r.Percent
		}
		for i := range result.Annual {
			if pct, ok := byYear[result.Annual[i].Year]; ok {
				value := pct
				result.Annual[i].Benchmark = &value
			}
		}
	}

	for i, m := range months {
		result.Points[i] = Balance{Month: m, Value: balances[i]}
		if benchBalances != nil {
			value := benchBalances[i]
			result.Points[i].Benchmark = &value
		}
	}
	for i, h := range holdings {
		h.FirstMonth = firstMonth[h.Symbol]
		result.Holdings[i] = h
	}
	return result, nil
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

// simulate walks the months, compounding each holding by its own return and
// rebalancing on the cadence's boundaries. It returns the portfolio's value at
// each month and how many rebalances it performed.
//
// Between rebalances the weights drift, which is the point: a 60/40 that has
// not been touched for a decade is a 78/22, and a backtest that silently held
// it at 60/40 would be reporting a strategy nobody ran.
func simulate(holdings []HoldingResult, series map[string]map[string]float64,
	months []string, initial float64, cadence string) ([]float64, int) {
	values := make([]float64, len(holdings))
	for i, h := range holdings {
		values[i] = initial * h.Weight / 100
	}

	balances := make([]float64, len(months))
	balances[0] = initial
	rebalances := 0

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
		balances[k] = total

		// The last month never rebalances: it would move money at the closing
		// bell of the run and change nothing about the answer, while counting
		// as a trade that happened.
		if k < len(months)-1 && rebalanceAfter(months[k], cadence) {
			for i, h := range holdings {
				values[i] = total * h.Weight / 100
			}
			rebalances++
		}
	}
	return balances, rebalances
}

// rebalanceAfter reports whether the cadence rebalances at the end of this
// month. Boundaries are calendar ones — December for a yearly rebalance, not
// "twelve months after the run happened to start" — because that is when
// somebody doing this by hand would actually do it.
func rebalanceAfter(month, cadence string) bool {
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

// measure computes the summary table for one balance series.
func measure(label string, months []string, balances []float64, annual []AnnualReturn) Metrics {
	m := Metrics{
		Label: label,
		Start: balances[0],
		End:   balances[len(balances)-1],
	}
	if m.Start > 0 {
		m.TotalPercent = (m.End/m.Start - 1) * 100
	}

	years := float64(len(balances)-1) / 12
	if years >= cagrMinYears && m.Start > 0 && m.End > 0 {
		cagr := (math.Pow(m.End/m.Start, 1/years) - 1) * 100
		m.CAGR = &cagr
	}

	if stdev, ok := annualisedStdev(balances); ok {
		m.Stdev = &stdev
	}

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

	m.MaxDrawdown, m.DrawdownPeak, m.DrawdownTrough, m.DrawdownRecover = maxDrawdown(months, balances)
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
		Holdings:      p.Holdings,
		InitialAmount: p.InitialAmount,
		StartYear:     p.StartYear,
		EndYear:       p.EndYear,
		Rebalance:     p.Rebalance,
		Benchmark:     p.Benchmark,
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
