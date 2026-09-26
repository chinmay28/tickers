package xsection

import (
	"math"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// SymbolStats is one symbol held over a window.
type SymbolStats struct {
	Symbol   string
	From, To time.Time
	Days     int
	// Metrics are buying at the first close in the window and holding.
	Metrics strategy.Metrics
	// Volatility is the annualised standard deviation of daily returns, in
	// percent.
	Volatility        float64
	BestDay, WorstDay float64
	// Beta and Correlation are against the comparison's first symbol, over
	// the days both traded; NaN for the first itself or without enough
	// overlap.
	Beta, Correlation float64
}

// Comparison is several symbols side by side.
type Comparison struct {
	Stats []SymbolStats
	// Correlation is of daily returns, pairwise over the days both traded,
	// in the order of Stats; NaN where they overlap too little to say.
	Correlation [][]float64
}

// minOverlap is how many shared daily returns a correlation needs: fewer
// and it is mostly noise.
const minOverlap = 20

// Compare measures each symbol in order (any not in the panel is skipped)
// from the panel's date from on.
func Compare(p *Panel, from int, order []string) Comparison {
	var cols []int
	for _, sym := range order {
		if s := sort.SearchStrings(p.Symbols, sym); s < len(p.Symbols) && p.Symbols[s] == sym {
			cols = append(cols, s)
		}
	}
	returns := make([][]float64, len(cols))
	var cmp Comparison
	for k, s := range cols {
		st := SymbolStats{Symbol: p.Symbols[s], Beta: math.NaN(), Correlation: math.NaN(), BestDay: math.Inf(-1), WorstDay: math.Inf(1)}
		r := make([]float64, len(p.Dates))
		var curve []float64
		var sum, sumSq float64
		n := 0
		for i := range p.Dates {
			r[i] = math.NaN()
			c := p.Close[s][i]
			if i < from || !defined(c) {
				continue
			}
			if len(curve) == 0 {
				st.From = p.Dates[i]
			}
			st.To = p.Dates[i]
			curve = append(curve, c)
			if len(curve) > 1 {
				x := (c/curve[len(curve)-2] - 1) * 100
				r[i] = x
				sum += x
				sumSq += x * x
				n++
				st.BestDay, st.WorstDay = math.Max(st.BestDay, x), math.Min(st.WorstDay, x)
			}
		}
		st.Days = len(curve)
		if len(curve) > 0 {
			st.Metrics = strategy.Measure(curve[0], curve, st.From, st.To, quotes.Daily)
		}
		if n > 1 {
			m := sum / float64(n)
			st.Volatility = math.Sqrt(math.Max(0, (sumSq-float64(n)*m*m)/float64(n-1))) * math.Sqrt(252)
		} else {
			st.BestDay, st.WorstDay = 0, 0
		}
		returns[k] = r
		cmp.Stats = append(cmp.Stats, st)
	}
	cmp.Correlation = make([][]float64, len(cols))
	for a := range cols {
		cmp.Correlation[a] = make([]float64, len(cols))
		for b := range cols {
			corr, _ := pairwise(returns[a], returns[b])
			cmp.Correlation[a][b] = corr
		}
	}
	for k := 1; k < len(cols); k++ {
		corr, beta := pairwise(returns[k], returns[0])
		cmp.Stats[k].Correlation, cmp.Stats[k].Beta = corr, beta
	}
	return cmp
}

// pairwise is the correlation of a with b, and a's beta to b, over the
// days both have a return.
func pairwise(a, b []float64) (corr, beta float64) {
	var xs, ys []float64
	for i := range a {
		if defined(a[i]) && defined(b[i]) {
			xs, ys = append(xs, a[i]), append(ys, b[i])
		}
	}
	if len(xs) < minOverlap {
		return math.NaN(), math.NaN()
	}
	n := float64(len(xs))
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/n, my/n
	var cov, vx, vy float64
	for i := range xs {
		cov += (xs[i] - mx) * (ys[i] - my)
		vx += (xs[i] - mx) * (xs[i] - mx)
		vy += (ys[i] - my) * (ys[i] - my)
	}
	if vx == 0 || vy == 0 {
		return math.NaN(), math.NaN()
	}
	return cov / math.Sqrt(vx*vy), cov / vy
}
