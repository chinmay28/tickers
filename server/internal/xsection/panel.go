// Package xsection is cross-sectional research: many symbols' daily bars on
// one calendar, a factor computed for each, and the two questions asked of
// it — does ranking symbols by it today say anything about their returns
// tomorrow (Study), and what would holding the best-ranked have made
// (Rotate).
//
// It is pure, like strategy: the engine loads the bars, and everything here
// is arithmetic over them, testable on a handful of symbols worked by hand.
// The fill model is the backtester's — decided on a close, traded at the next
// open — so a rotation and a single-symbol backtest can be compared.
package xsection

import (
	"math"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Panel is many symbols' daily bars aligned on the union of their dates.
// Every column is [symbol][date], NaN where the symbol has no bar that day —
// before it listed, after it delisted, or on a day it didn't trade.
type Panel struct {
	Dates   []time.Time
	Symbols []string
	Open    [][]float64
	High    [][]float64
	Low     [][]float64
	Close   [][]float64
	Volume  [][]float64
}

// NewPanel aligns series on one calendar, symbols in name order.
func NewPanel(series map[string][]quotes.Candle) *Panel {
	p := &Panel{}
	seen := map[int64]bool{}
	for sym, bars := range series {
		p.Symbols = append(p.Symbols, sym)
		for _, b := range bars {
			if k := b.Time.Unix(); !seen[k] {
				seen[k] = true
				p.Dates = append(p.Dates, b.Time.UTC())
			}
		}
	}
	sort.Strings(p.Symbols)
	sort.Slice(p.Dates, func(i, j int) bool { return p.Dates[i].Before(p.Dates[j]) })
	index := make(map[int64]int, len(p.Dates))
	for i, d := range p.Dates {
		index[d.Unix()] = i
	}
	blank := func() []float64 {
		v := make([]float64, len(p.Dates))
		for i := range v {
			v[i] = math.NaN()
		}
		return v
	}
	for _, sym := range p.Symbols {
		o, h, l, c, v := blank(), blank(), blank(), blank(), blank()
		for _, b := range series[sym] {
			i := index[b.Time.Unix()]
			o[i], h[i], l[i], c[i], v[i] = b.Open, b.High, b.Low, b.Close, float64(b.Volume)
		}
		p.Open, p.High, p.Low, p.Close, p.Volume = append(p.Open, o), append(p.High, h), append(p.Low, l), append(p.Close, c), append(p.Volume, v)
	}
	return p
}

// First is the index of the first date on or after t.
func (p *Panel) First(t time.Time) int {
	return sort.Search(len(p.Dates), func(i int) bool { return !p.Dates[i].Before(t) })
}

// bars is one symbol's own bars — the days it traded — and where each sits
// on the panel's calendar. Indicators and factors run over these, so a
// 20-day average is twenty of the symbol's days, not twenty calendar slots
// some of which it didn't trade on.
func (p *Panel) bars(s int) ([]quotes.Candle, []int) {
	var out []quotes.Candle
	var at []int
	for i, c := range p.Close[s] {
		if math.IsNaN(c) {
			continue
		}
		out = append(out, quotes.Candle{Time: p.Dates[i], Open: p.Open[s][i], High: p.High[s][i], Low: p.Low[s][i], Close: c, Volume: int64(p.Volume[s][i])})
		at = append(at, i)
	}
	return out, at
}

func defined(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
