package xsection

import (
	"math"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// RotationSpec is a rank-and-hold strategy: every Every days from From, rank
// the eligible symbols by the factor and hold the Hold best, equally
// weighted, until the next ranking.
type RotationSpec struct {
	Factor      Factor
	From, Every int
	Hold        int
	// Ascending holds the lowest-ranked instead: low volatility, the worst
	// recent losers for a reversal.
	Ascending  bool
	Filter     Filter
	FeePercent float64
	Initial    float64
	// Benchmark is a symbol in the panel to buy and hold beside it; empty
	// is every eligible symbol, equally weighted, rebalanced on the same
	// days without fees — what ranking added over owning the lot.
	Benchmark string
}

// Pick is one ranking's holdings, best first.
type Pick struct {
	Date    time.Time `json:"date"`
	Symbols []string  `json:"symbols"`
	Values  []float64 `json:"values"`
}

// Rotation is what a rotation did.
type Rotation struct {
	From, To   time.Time
	Days       int
	Strategy   strategy.Metrics
	Benchmark  strategy.Metrics
	Rebalances int
	// Turnover is the average share of equity traded at a rebalance, in
	// percent: 200 is selling everything and buying all new.
	Turnover float64
	// AvgHeld is how many symbols were held on average — fewer than Hold
	// when not enough were eligible.
	AvgHeld float64
	Dates   []time.Time
	Equity  []float64
	Bench   []float64
	// Picks are every ranking's holdings, oldest first.
	Picks    []Pick
	Warnings []string
}

// Rotate simulates the spec over the panel. Rankings are made on a close
// and traded at the next open, like every other backtest here. A holding
// that stops trading — delisted — is valued at its last close until the
// next rebalance sells it there: without delisting returns, the kindest
// assumption available, and a reason to read results on small caps warily.
func Rotate(p *Panel, spec RotationSpec) (Rotation, error) {
	if spec.Every < 1 || spec.Hold < 1 {
		return Rotation{}, invalid("the days between rebalances and the number held must be at least 1")
	}
	if spec.FeePercent < 0 || spec.FeePercent > strategy.MaxFeePercent {
		return Rotation{}, invalid("the fee must be between 0 and %d%% a trade", strategy.MaxFeePercent)
	}
	if spec.Initial <= 0 {
		spec.Initial = 10000
	}
	bench := -1
	if spec.Benchmark != "" {
		bench = sort.SearchStrings(p.Symbols, spec.Benchmark)
		if bench == len(p.Symbols) || p.Symbols[bench] != spec.Benchmark {
			return Rotation{}, invalid("the benchmark %s has no bars in this window", spec.Benchmark)
		}
	}
	start := max(spec.From, 0)
	if start >= len(p.Dates)-1 {
		return Rotation{}, invalid("there are no bars to trade in that window")
	}
	values := spec.Factor.Values(p)
	ok := p.eligible(spec.Filter)
	rot := Rotation{From: p.Dates[start], To: p.Dates[len(p.Dates)-1], Days: len(p.Dates) - start, Warnings: []string{}}

	pick := func(i int) ([]int, []float64) {
		var cand []int
		for s := range p.Symbols {
			if ok(s, i) && defined(values[s][i]) {
				cand = append(cand, s)
			}
		}
		sort.SliceStable(cand, func(a, b int) bool {
			va, vb := values[cand[a]][i], values[cand[b]][i]
			if va == vb {
				return p.Symbols[cand[a]] < p.Symbols[cand[b]]
			}
			return (va > vb) != spec.Ascending
		})
		cand = cand[:min(len(cand), spec.Hold)]
		vals := make([]float64, len(cand))
		for k, s := range cand {
			vals[k] = values[s][i]
		}
		return cand, vals
	}
	everyone := func(i int) ([]int, []float64) {
		var all []int
		for s := range p.Symbols {
			if ok(s, i) {
				all = append(all, s)
			}
		}
		return all, nil
	}

	var held int
	var turnover float64
	record := func(i int, syms []int, vals []float64, traded float64) {
		names := make([]string, len(syms))
		for k, s := range syms {
			names[k] = p.Symbols[s]
		}
		rot.Picks = append(rot.Picks, Pick{Date: p.Dates[i], Symbols: names, Values: vals})
		held += len(syms)
		turnover += traded
	}
	rot.Dates = p.Dates[start:]
	rot.Equity = simulateBook(p, start, spec.Every, spec.Initial, spec.FeePercent/100, pick, record)
	if bench >= 0 {
		rot.Bench = simulateBook(p, start, len(p.Dates), spec.Initial, 0, func(int) ([]int, []float64) { return []int{bench}, nil }, nil)
	} else {
		rot.Bench = simulateBook(p, start, spec.Every, spec.Initial, 0, everyone, nil)
	}
	rot.Rebalances = len(rot.Picks)
	if rot.Rebalances > 0 {
		rot.AvgHeld = float64(held) / float64(rot.Rebalances)
		rot.Turnover = turnover / float64(rot.Rebalances) * 100
	}
	if rot.AvgHeld < float64(spec.Hold) {
		rot.Warnings = append(rot.Warnings, "fewer symbols were eligible than the number to hold on some rankings; the book held what there was")
	}
	rot.Strategy = strategy.Measure(spec.Initial, rot.Equity, rot.From, rot.To, quotes.Daily)
	rot.Benchmark = strategy.Measure(spec.Initial, rot.Bench, rot.From, rot.To, quotes.Daily)
	return rot, nil
}

// simulateBook runs an equal-weighted book: ranked on day d = start,
// start+every, …, traded at d+1's open, marked at every close from start
// on. record, when set, hears each rebalance and the share of equity it
// traded.
func simulateBook(p *Panel, start, every int, initial, fee float64, choose func(i int) ([]int, []float64),
	record func(i int, syms []int, vals []float64, traded float64)) []float64 {
	n := len(p.Dates)
	shares := map[int]float64{}
	last := make([]float64, len(p.Symbols)) // each symbol's last close, for valuing one that stopped trading
	for s := range last {
		last[s] = math.NaN()
	}
	price := func(col [][]float64, s, i int) float64 {
		if v := col[s][i]; defined(v) {
			return v
		}
		return last[s]
	}
	cash := initial
	curve := make([]float64, 0, n-start)
	var pending []int
	var pendingVals []float64
	pendingAt := -1
	for i := start; i < n; i++ {
		if pendingAt >= 0 {
			// Value the book at this open, then trade to equal weights.
			equity := cash
			current := map[int]float64{}
			for s, sh := range shares {
				v := sh * price(p.Open, s, i)
				if defined(v) {
					current[s] = v
					equity += v
				}
			}
			var tradable []int
			for _, s := range pending {
				if o := p.Open[s][i]; defined(o) && o > 0 {
					tradable = append(tradable, s)
				}
			}
			target := map[int]float64{}
			for _, s := range tradable {
				target[s] = equity / float64(len(tradable))
			}
			traded := 0.0
			for s, v := range current {
				traded += math.Abs(target[s] - v)
			}
			for s, v := range target {
				if _, had := current[s]; !had {
					traded += v
				}
			}
			equity -= traded * fee
			shares = map[int]float64{}
			cash = equity
			for _, s := range tradable {
				w := equity / float64(len(tradable))
				shares[s] = w / p.Open[s][i]
				cash -= w
			}
			if record != nil && equity > 0 {
				record(pendingAt, pending, pendingVals, traded/(equity+traded*fee))
			}
			pendingAt = -1
		}
		for s := range p.Symbols {
			if c := p.Close[s][i]; defined(c) {
				last[s] = c
			}
		}
		value := cash
		for s, sh := range shares {
			if v := sh * last[s]; defined(v) {
				value += v
			}
		}
		curve = append(curve, value)
		if (i-start)%every == 0 && i+1 < n {
			pending, pendingVals = choose(i)
			pendingAt = i
		}
	}
	return curve
}
