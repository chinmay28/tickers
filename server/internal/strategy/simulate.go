package strategy

import (
	"math"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Exit reasons.
const (
	ReasonRule   = "rule"
	ReasonStop   = "stop"
	ReasonTarget = "target"
	ReasonOpen   = "open"
)

// Trade is one round trip, or the position still open at the end.
type Trade struct {
	EntryTime  time.Time `json:"entryTime"`
	EntryPrice float64   `json:"entryPrice"`
	ExitTime   time.Time `json:"exitTime"`
	ExitPrice  float64   `json:"exitPrice"`
	// Return is the trade's percentage gain after fees both ways.
	Return float64 `json:"return"`
	Bars   int     `json:"bars"`
	// Reason is why it closed: its exit rule, its stop, its target, or
	// "open" — it hadn't by the end, and is marked at the last close.
	Reason string `json:"reason"`
}

// Point is one step of the equity curves.
type Point struct {
	T      time.Time `json:"t"`
	Equity float64   `json:"equity"`
	Hold   float64   `json:"hold"`
}

// PricePoint is one close on the price chart, with the value of every price
// overlay the rules use.
type PricePoint struct {
	T     time.Time `json:"t"`
	Close float64   `json:"close"`
}

// Metrics summarise a curve. Percentages are percentages (12.5 is 12.5%).
type Metrics struct {
	Final       float64 `json:"final"`
	TotalReturn float64 `json:"totalReturn"`
	CAGR        float64 `json:"cagr"`
	MaxDrawdown float64 `json:"maxDrawdown"`
	// Sharpe is annualised from per-bar returns with a zero risk-free rate —
	// the number to compare against the same strategy's buy-and-hold, not
	// against a figure quoted with a rate taken out.
	Sharpe float64 `json:"sharpe"`
}

// Stats is the strategy's trading record.
type Stats struct {
	Trades  int     `json:"trades"`
	WinRate float64 `json:"winRate"`
	// ProfitFactor is gross gains over gross losses; nil when there were no
	// losses to divide by.
	ProfitFactor *float64 `json:"profitFactor"`
	AvgTrade     float64  `json:"avgTrade"`
	AvgBars      float64  `json:"avgBars"`
	BestTrade    float64  `json:"bestTrade"`
	WorstTrade   float64  `json:"worstTrade"`
	// Exposure is the share of bars a position was held, 0–100.
	Exposure float64 `json:"exposure"`
}

// Result is a backtest's outcome.
type Result struct {
	Symbol   string    `json:"symbol"`
	Interval string    `json:"interval"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Bars     int       `json:"bars"`
	Strategy Metrics   `json:"strategy"`
	Hold     Metrics   `json:"hold"`
	Stats    Stats     `json:"stats"`
	Trades   []Trade   `json:"trades"`
	// Equity and Prices are thinned for drawing; every number above was
	// computed from every bar.
	Equity []Point       `json:"equity"`
	Prices []PricePoint  `json:"prices"`
	Lines  []OverlayLine `json:"lines"`
	// Warnings are things the reader should know before trusting the
	// numbers: a window the data didn't cover, a strategy that never traded.
	Warnings []string `json:"warnings"`
}

// OverlayLine is a price-pane indicator a rule uses, thinned like Prices and
// aligned with it, for drawing on the price chart.
type OverlayLine struct {
	Label  string     `json:"label"`
	Values []*float64 `json:"values"`
}

// maxPoints bounds the drawn curves. A chart a few hundred pixels wide gains
// nothing from a year of minute bars.
const maxPoints = 1500

// maxTrades bounds the trades sent back; a strategy firing on every minute
// bar for a year has made its point long before its ten-thousandth trade.
const maxTrades = 5000

// barsPerYear annualises per-bar statistics: trading days, times the regular
// session's bars at each width.
func barsPerYear(i quotes.Interval) float64 {
	switch i {
	case quotes.Hourly:
		return 252 * 7
	case quotes.FiveMinute:
		return 252 * 78
	case quotes.OneMinute:
		return 252 * 390
	}
	return 252
}

// Simulate runs the plan over bars, trading from bars[start] on. The bars
// before start are warm-up: indicators are computed over them so the first
// bar traded has settled values, and a cross can look back one bar into them.
//
// The execution model is the one that can't cheat:
//
//   - A rule is judged on a bar's close, and the order fills at the next
//     bar's open. Filling at the close that triggered it would trade on a
//     price nobody could have acted on.
//   - The whole of equity goes in on entry, in fractional shares, and comes
//     out whole on exit. Fees are charged on the value traded, both ways.
//   - Stops and targets are checked inside every bar held, the entry bar
//     included. A bar that gaps through the level fills at its open, not at
//     the level — a stop is a market order, and the market was already past
//     it. A bar that reaches both is assumed to have hit the stop first; with
//     only OHLC nobody can know, and the pessimistic answer is the honest one.
func Simulate(p Plan, bars []quotes.Candle, start int) Result {
	res := Result{Symbol: p.Def.Symbol, Interval: string(p.Interval), Trades: []Trade{}, Equity: []Point{}, Prices: []PricePoint{}, Lines: []OverlayLine{}, Warnings: []string{}}
	if start < 0 {
		start = 0
	}
	if start >= len(bars) {
		res.Warnings = append(res.Warnings, "there are no bars in that window")
		return res
	}
	n := len(bars)
	series := evaluate(p.Specs, bars)
	val := func(o operand, i int) float64 {
		switch {
		case o.isNum:
			return o.value
		case o.field != "":
			b := bars[i]
			switch o.field {
			case "open":
				return b.Open
			case "high":
				return b.High
			case "low":
				return b.Low
			case "volume":
				return float64(b.Volume)
			}
			return b.Close
		}
		return series[o.spec.Key()][o.line][i]
	}
	holds := func(r compiled, i int) bool {
		if len(r.conds) == 0 {
			return false
		}
		for _, c := range r.conds {
			ok := c.holds(val, i)
			if r.any && ok {
				return true
			}
			if !r.any && !ok {
				return false
			}
		}
		return !r.any
	}

	fee := p.Def.FeePercent / 100
	cash := p.Def.Initial
	var shares, entryPrice, entryCash float64
	var entryAt int
	inPos, wantIn, wantOut := false, false, false
	held := 0
	holdShares := p.Def.Initial * (1 - fee) / bars[start].Open
	equity := make([]float64, 0, n-start)
	hold := make([]float64, 0, n-start)

	closeTrade := func(i int, price float64, reason string) {
		cash = shares * price * (1 - fee)
		res.Trades = append(res.Trades, Trade{
			EntryTime: bars[entryAt].Time, EntryPrice: entryPrice,
			ExitTime: bars[i].Time, ExitPrice: price,
			Return: (cash/entryCash - 1) * 100, Bars: i - entryAt + 1, Reason: reason,
		})
		shares, inPos = 0, false
	}

	for i := start; i < n; i++ {
		b := bars[i]
		if wantIn && !inPos {
			entryCash, entryPrice, entryAt = cash, b.Open, i
			shares, cash, inPos = cash*(1-fee)/b.Open, 0, true
		} else if wantOut && inPos {
			closeTrade(i, b.Open, ReasonRule)
		}
		wantIn, wantOut = false, false

		if inPos {
			held++
			stop := entryPrice * (1 - p.Def.StopLoss/100)
			target := entryPrice * (1 + p.Def.TakeProfit/100)
			switch {
			case p.Def.StopLoss > 0 && b.Low <= stop:
				closeTrade(i, math.Min(b.Open, stop), ReasonStop)
			case p.Def.TakeProfit > 0 && b.High >= target:
				closeTrade(i, math.Max(b.Open, target), ReasonTarget)
			}
		}

		if inPos {
			equity = append(equity, shares*b.Close)
		} else {
			equity = append(equity, cash)
		}
		hold = append(hold, holdShares*b.Close)

		if i == n-1 {
			break
		}
		switch {
		case !inPos && holds(p.entry, i):
			wantIn = true
		case inPos && holds(p.exit, i):
			wantOut = true
		}
	}
	if inPos {
		last := bars[n-1]
		res.Trades = append(res.Trades, Trade{
			EntryTime: bars[entryAt].Time, EntryPrice: entryPrice, ExitTime: last.Time, ExitPrice: last.Close,
			Return: (shares*last.Close/entryCash - 1) * 100, Bars: n - entryAt, Reason: ReasonOpen,
		})
	}

	first, last := bars[start].Time, bars[n-1].Time
	res.From, res.To, res.Bars = first, last, n-start
	perYear := barsPerYear(p.Interval)
	res.Strategy = metrics(p.Def.Initial, equity, first, last, perYear)
	res.Hold = metrics(p.Def.Initial, hold, first, last, perYear)
	res.Stats = stats(res.Trades, held, n-start)
	if len(res.Trades) == 0 {
		res.Warnings = append(res.Warnings, "the entry rule never held, so the strategy never traded")
	}
	if len(res.Trades) > maxTrades {
		res.Warnings = append(res.Warnings, "only the first trades are listed; every trade is in the numbers")
		res.Trades = res.Trades[:maxTrades]
	}
	res.Equity, res.Prices, res.Lines = thin(bars[start:], equity, hold, series, p.Specs, start)
	return res
}

func (c compiledCondition) holds(val func(operand, int) float64, i int) bool {
	l, r := val(c.left, i), val(c.right, i)
	if math.IsNaN(l) || math.IsNaN(r) {
		return false
	}
	switch c.op {
	case OpAbove:
		return l > r
	case OpBelow:
		return l < r
	case OpAtLeast:
		return l >= r
	case OpAtMost:
		return l <= r
	}
	if i == 0 {
		return false
	}
	pl, pr := val(c.left, i-1), val(c.right, i-1)
	if math.IsNaN(pl) || math.IsNaN(pr) {
		return false
	}
	if c.op == OpCrossesAbove {
		return pl <= pr && l > r
	}
	return pl >= pr && l < r
}

// evaluate computes every indicator the plan uses, keyed by spec and line,
// with NaN where one isn't defined.
func evaluate(specs []indicators.Spec, bars []quotes.Candle) map[string]map[string][]float64 {
	out := map[string]map[string][]float64{}
	for _, r := range indicators.Compute(specs, bars) {
		lines := map[string][]float64{}
		for _, l := range r.Lines {
			v := make([]float64, len(l.Values))
			for i, p := range l.Values {
				if p == nil {
					v[i] = math.NaN()
				} else {
					v[i] = *p
				}
			}
			lines[l.Name] = v
		}
		out[r.Key] = lines
	}
	return out
}

func metrics(initial float64, curve []float64, first, last time.Time, perYear float64) Metrics {
	m := Metrics{}
	if len(curve) == 0 {
		return m
	}
	m.Final = curve[len(curve)-1]
	m.TotalReturn = (m.Final/initial - 1) * 100
	if years := last.Sub(first).Hours() / 24 / 365.25; years > 0 && m.Final > 0 {
		m.CAGR = (math.Pow(m.Final/initial, 1/years) - 1) * 100
	}
	peak, prev := initial, initial
	var sum, sumSq float64
	for _, v := range curve {
		peak = math.Max(peak, v)
		if peak > 0 {
			m.MaxDrawdown = math.Min(m.MaxDrawdown, (v/peak-1)*100)
		}
		r := 0.0
		if prev > 0 {
			r = v/prev - 1
		}
		sum += r
		sumSq += r * r
		prev = v
	}
	n := float64(len(curve))
	mean := sum / n
	if variance := sumSq/n - mean*mean; variance > 1e-18 {
		m.Sharpe = mean / math.Sqrt(variance) * math.Sqrt(perYear)
	}
	return m
}

func stats(trades []Trade, held, total int) Stats {
	s := Stats{Trades: len(trades)}
	if total > 0 {
		s.Exposure = float64(held) / float64(total) * 100
	}
	if len(trades) == 0 {
		return s
	}
	var wins, gains, losses, sum, bars float64
	s.BestTrade, s.WorstTrade = math.Inf(-1), math.Inf(1)
	for _, t := range trades {
		sum += t.Return
		bars += float64(t.Bars)
		s.BestTrade = math.Max(s.BestTrade, t.Return)
		s.WorstTrade = math.Min(s.WorstTrade, t.Return)
		if t.Return > 0 {
			wins++
			gains += t.Return
		} else {
			losses -= t.Return
		}
	}
	n := float64(len(trades))
	s.WinRate, s.AvgTrade, s.AvgBars = wins/n*100, sum/n, bars/n
	if losses > 0 {
		pf := gains / losses
		s.ProfitFactor = &pf
	}
	return s
}

// thin picks at most maxPoints bars to draw, evenly, keeping the last, and
// the price-pane indicator lines the rules use at the same bars.
func thin(bars []quotes.Candle, equity, hold []float64, series map[string]map[string][]float64, specs []indicators.Spec, offset int) ([]Point, []PricePoint, []OverlayLine) {
	step := 1
	if len(bars) > maxPoints {
		step = (len(bars) + maxPoints - 1) / maxPoints
	}
	var idx []int
	for i := 0; i < len(bars); i += step {
		idx = append(idx, i)
	}
	if idx[len(idx)-1] != len(bars)-1 {
		idx = append(idx, len(bars)-1)
	}
	points := make([]Point, len(idx))
	prices := make([]PricePoint, len(idx))
	for k, i := range idx {
		points[k] = Point{T: bars[i].Time, Equity: equity[i], Hold: hold[i]}
		prices[k] = PricePoint{T: bars[i].Time, Close: bars[i].Close}
	}
	var lines []OverlayLine
	for _, s := range specs {
		if !priceOverlay(s.Kind) {
			continue
		}
		names := make([]string, 0, len(series[s.Key()]))
		for name := range series[s.Key()] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			v := series[s.Key()][name]
			label := s.Label()
			if len(series[s.Key()]) > 1 {
				label += " " + name
			}
			vals := make([]*float64, len(idx))
			for k, i := range idx {
				if x := v[i+offset]; !math.IsNaN(x) {
					vals[k] = &x
				}
			}
			lines = append(lines, OverlayLine{Label: label, Values: vals})
		}
	}
	if lines == nil {
		lines = []OverlayLine{}
	}
	return points, prices, lines
}

func priceOverlay(kind string) bool {
	switch kind {
	case indicators.KindSMA, indicators.KindEMA, indicators.KindBollinger, indicators.KindVWAP:
		return true
	}
	return false
}
