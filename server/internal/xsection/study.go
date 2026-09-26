package xsection

import (
	"math"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/strategy"
)

// Filter is what makes a symbol eligible to be ranked on a day, judged on
// that day's data alone — a universe chosen by what was liquid later would
// know the future.
type Filter struct {
	// MinPrice drops symbols closing below it: penny stocks' returns are
	// mostly spread and noise.
	MinPrice float64
	// MinDollarVolume drops symbols whose average close × volume over the
	// last 20 days is below it.
	MinDollarVolume float64
	// Where, when set, keeps only symbols its rule holds for that day:
	// "close > sma:200", ranked only among stocks in an uptrend.
	Where *strategy.Signal
	// Exclude is a symbol never ranked: a benchmark loaded to be compared
	// with, not held.
	Exclude string
}

// Warmup is how many days before the first ranking the filter needs.
func (f Filter) Warmup() int {
	w := 0
	if f.MinDollarVolume > 0 {
		w = 21
	}
	if f.Where != nil {
		w = max(w, f.Where.Warmup())
	}
	return w
}

// eligible returns the day-by-day eligibility test for the panel.
func (p *Panel) eligible(f Filter) func(s, i int) bool {
	var dv [][]float64
	if f.MinDollarVolume > 0 {
		dv = Factor{kind: kindDollar, n: 20}.Values(p)
	}
	var where [][]bool
	if f.Where != nil {
		where = make([][]bool, len(p.Symbols))
		for s := range p.Symbols {
			where[s] = make([]bool, len(p.Dates))
			bars, at := p.bars(s)
			for k, ok := range f.Where.Holds(bars) {
				where[s][at[k]] = ok
			}
		}
	}
	return func(s, i int) bool {
		if f.Exclude != "" && p.Symbols[s] == f.Exclude {
			return false
		}
		c := p.Close[s][i]
		if !defined(c) || c < f.MinPrice {
			return false
		}
		if where != nil && !where[s][i] {
			return false
		}
		return dv == nil || (defined(dv[s][i]) && dv[s][i] >= f.MinDollarVolume)
	}
}

// StudySpec is one factor study.
type StudySpec struct {
	Factor Factor
	// From is the panel index of the first ranking; Every is the days
	// between rankings.
	From, Every int
	// Horizon is how many days after a ranking the return is measured to:
	// from the next open, as a trade on the ranking would fill, to the
	// close Horizon days after the ranking.
	Horizon   int
	Quantiles int
	Filter    Filter
}

// Summary is a statistic's mean over the rankings and how sure it is.
type Summary struct {
	Mean  float64 `json:"mean"`
	Stdev float64 `json:"stdev"`
	// TStat is the mean over its standard error. When rankings are closer
	// together than the horizon, consecutive ones measure overlapping
	// returns and aren't independent; the count is reduced in proportion
	// so the t-statistic isn't inflated by the overlap.
	TStat float64 `json:"tStat"`
	// Positive is the share of rankings it was above zero, in percent.
	Positive float64 `json:"positive"`
}

// Quantile is one slice of the ranking, 1 the lowest factor values.
type Quantile struct {
	N    int     `json:"quantile"`
	Mean float64 `json:"meanReturn"`
}

// Point is one ranking's outcome.
type Point struct {
	Date    time.Time `json:"date"`
	IC      float64   `json:"ic"`
	Spread  float64   `json:"spread"`
	Symbols int       `json:"symbols"`
}

// Study is what a factor said about the returns that followed.
type Study struct {
	Rankings   int     `json:"rankings"`
	AvgSymbols float64 `json:"avgSymbols"`
	// IC is the information coefficient: the rank correlation between the
	// factor and the returns that followed, per ranking. Around 0.05 held
	// steadily is a useful factor; a mean of zero is none.
	IC Summary `json:"ic"`
	// Spread is the top quantile's mean return minus the bottom's, per
	// ranking, in percent.
	Spread    Summary    `json:"spread"`
	Quantiles []Quantile `json:"quantiles"`
	Points    []Point    `json:"points"`
}

// RunStudy ranks the panel by the factor on every Every-th day from From and
// measures what followed. A ranking with fewer than two symbols a quantile
// is skipped: a correlation over five points is noise.
func RunStudy(p *Panel, spec StudySpec) (Study, error) {
	if spec.Every < 1 || spec.Horizon < 1 {
		return Study{}, invalid("the days between rankings and the horizon must be at least 1")
	}
	if spec.Quantiles < 2 || spec.Quantiles > 10 {
		return Study{}, invalid("quantiles must be from 2 to 10")
	}
	values := spec.Factor.Values(p)
	ok := p.eligible(spec.Filter)
	st := Study{Quantiles: make([]Quantile, spec.Quantiles), Points: []Point{}}
	var ics, spreads []float64
	qsum := make([]float64, spec.Quantiles)
	symbols := 0
	for i := max(spec.From, 0); i+spec.Horizon < len(p.Dates); i += spec.Every {
		var fx, ret []float64
		for s := range p.Symbols {
			if !ok(s, i) || !defined(values[s][i]) {
				continue
			}
			entry, exit := p.Open[s][i+1], p.Close[s][i+spec.Horizon]
			if !defined(entry) || !defined(exit) || entry <= 0 {
				continue
			}
			fx = append(fx, values[s][i])
			ret = append(ret, (exit/entry-1)*100)
		}
		if len(fx) < 2*spec.Quantiles {
			continue
		}
		ic := pearson(ranks(fx), ranks(ret))
		means := quantileMeans(fx, ret, spec.Quantiles)
		spread := means[len(means)-1] - means[0]
		for q, m := range means {
			qsum[q] += m
		}
		ics, spreads = append(ics, ic), append(spreads, spread)
		symbols += len(fx)
		st.Points = append(st.Points, Point{Date: p.Dates[i], IC: ic, Spread: spread, Symbols: len(fx)})
	}
	st.Rankings = len(ics)
	if st.Rankings == 0 {
		return st, nil
	}
	st.AvgSymbols = float64(symbols) / float64(st.Rankings)
	independent := float64(st.Rankings) * math.Min(1, float64(spec.Every)/float64(spec.Horizon))
	st.IC, st.Spread = summarize(ics, independent), summarize(spreads, independent)
	for q := range st.Quantiles {
		st.Quantiles[q] = Quantile{N: q + 1, Mean: qsum[q] / float64(st.Rankings)}
	}
	return st, nil
}

func summarize(xs []float64, independent float64) Summary {
	var s Summary
	n := float64(len(xs))
	pos := 0
	for _, x := range xs {
		s.Mean += x
		if x > 0 {
			pos++
		}
	}
	s.Mean /= n
	s.Positive = float64(pos) / n * 100
	if len(xs) > 1 {
		var ss float64
		for _, x := range xs {
			ss += (x - s.Mean) * (x - s.Mean)
		}
		s.Stdev = math.Sqrt(ss / (n - 1))
		if s.Stdev > 0 && independent > 0 {
			s.TStat = s.Mean / (s.Stdev / math.Sqrt(independent))
		}
	}
	return s
}

// ranks are 1-based ranks, ties sharing their average rank.
func ranks(xs []float64) []float64 {
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return xs[idx[a]] < xs[idx[b]] })
	out := make([]float64, len(xs))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && xs[idx[j+1]] == xs[idx[i]] {
			j++
		}
		r := float64(i+j)/2 + 1
		for k := i; k <= j; k++ {
			out[idx[k]] = r
		}
		i = j + 1
	}
	return out
}

func pearson(a, b []float64) float64 {
	n := float64(len(a))
	var ma, mb float64
	for i := range a {
		ma += a[i]
		mb += b[i]
	}
	ma, mb = ma/n, mb/n
	var cov, va, vb float64
	for i := range a {
		cov += (a[i] - ma) * (b[i] - mb)
		va += (a[i] - ma) * (a[i] - ma)
		vb += (b[i] - mb) * (b[i] - mb)
	}
	if va == 0 || vb == 0 {
		return 0
	}
	return cov / math.Sqrt(va*vb)
}

// quantileMeans sorts by factor and returns each quantile's mean return,
// lowest factor first.
func quantileMeans(fx, ret []float64, q int) []float64 {
	idx := make([]int, len(fx))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return fx[idx[a]] < fx[idx[b]] })
	sums, counts := make([]float64, q), make([]int, q)
	for rank, i := range idx {
		b := rank * q / len(idx)
		sums[b] += ret[i]
		counts[b]++
	}
	for b := range sums {
		sums[b] /= float64(counts[b])
	}
	return sums
}
