package indicators

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Kinds of indicator a Spec can name.
const (
	KindSMA        = "sma"
	KindEMA        = "ema"
	KindBollinger  = "bb"
	KindVWAP       = "vwap"
	KindRSI        = "rsi"
	KindMACD       = "macd"
	KindStochastic = "stoch"
	KindATR        = "atr"
	KindOBV        = "obv"
)

// Panes an indicator draws in.
const (
	// PanePrice draws over the candles, on the price axis.
	PanePrice = "price"
	// PaneLower draws in a panel of its own under the chart.
	PaneLower = "lower"
)

// kind describes one indicator: its parameters and their defaults, where it
// draws, and how to compute it.
type kind struct {
	label    string
	pane     string
	defaults []float64
	// integer marks the parameters that are counts of bars.
	integer []bool
	// bounds is the fixed range of an oscillator (RSI, Stochastic), so its
	// panel draws the same scale every time and 70 always sits in the same
	// place. Nil scales to the data.
	bounds *[2]float64
	// warmup is how many bars before the first one shown it needs to be
	// settled there.
	warmup  func(p []float64) int
	compute func(p []float64, b bars) []Line
}

var kinds = map[string]kind{
	KindSMA: {label: "SMA", pane: PanePrice, defaults: []float64{20}, integer: []bool{true},
		warmup: func(p []float64) int { return int(p[0]) },
		compute: func(p []float64, b bars) []Line {
			return []Line{line("SMA", SMA(b.close, int(p[0])))}
		}},
	KindEMA: {label: "EMA", pane: PanePrice, defaults: []float64{20}, integer: []bool{true},
		// An EMA never quite forgets its seed; three periods puts the seed's
		// weight under 1% for any n worth charting.
		warmup: func(p []float64) int { return 3 * int(p[0]) },
		compute: func(p []float64, b bars) []Line {
			return []Line{line("EMA", EMA(b.close, int(p[0])))}
		}},
	KindBollinger: {label: "Bollinger", pane: PanePrice, defaults: []float64{20, 2}, integer: []bool{true, false},
		warmup: func(p []float64) int { return int(p[0]) },
		compute: func(p []float64, b bars) []Line {
			mid, up, lo := Bollinger(b.close, int(p[0]), p[1])
			return []Line{line("upper", up), line("middle", mid), line("lower", lo)}
		}},
	KindVWAP: {label: "VWAP", pane: PanePrice,
		warmup: func([]float64) int { return 0 },
		compute: func(_ []float64, b bars) []Line {
			return []Line{line("VWAP", VWAP(b.high, b.low, b.close, b.volume, b.vwap, b.day))}
		}},
	KindRSI: {label: "RSI", pane: PaneLower, defaults: []float64{14}, integer: []bool{true}, bounds: &[2]float64{0, 100},
		warmup: func(p []float64) int { return 3*int(p[0]) + 1 },
		compute: func(p []float64, b bars) []Line {
			return []Line{line("RSI", RSI(b.close, int(p[0])))}
		}},
	KindMACD: {label: "MACD", pane: PaneLower, defaults: []float64{12, 26, 9}, integer: []bool{true, true, true},
		warmup: func(p []float64) int { return 3*int(p[1]) + int(p[2]) },
		compute: func(p []float64, b bars) []Line {
			m, s, h := MACD(b.close, int(p[0]), int(p[1]), int(p[2]))
			return []Line{line("MACD", m), line("signal", s), histogram("histogram", h)}
		}},
	KindStochastic: {label: "Stochastic", pane: PaneLower, defaults: []float64{14, 3}, integer: []bool{true, true}, bounds: &[2]float64{0, 100},
		warmup: func(p []float64) int { return int(p[0]) + int(p[1]) },
		compute: func(p []float64, b bars) []Line {
			k, d := Stochastic(b.high, b.low, b.close, int(p[0]), int(p[1]))
			return []Line{line("%K", k), line("%D", d)}
		}},
	KindATR: {label: "ATR", pane: PaneLower, defaults: []float64{14}, integer: []bool{true},
		warmup: func(p []float64) int { return 3 * int(p[0]) },
		compute: func(p []float64, b bars) []Line {
			return []Line{line("ATR", ATR(b.high, b.low, b.close, int(p[0])))}
		}},
	KindOBV: {label: "OBV", pane: PaneLower,
		warmup: func([]float64) int { return 0 },
		compute: func(_ []float64, b bars) []Line {
			return []Line{line("OBV", OBV(b.close, b.volume))}
		}},
}

// Bounds on what a request can ask for.
const (
	// MaxPeriod bounds any bar-count parameter. A 1,000-bar average is
	// already four years of daily bars.
	MaxPeriod = 1000
	// MaxSpecs bounds the indicators one request computes.
	MaxSpecs = 12
)

// Spec is one indicator with its parameters: "sma:50", "macd:12:26:9".
type Spec struct {
	Kind   string
	Params []float64
}

// Key is the spec's canonical spelling, which is also how a client matches a
// result to what it asked for.
func (s Spec) Key() string {
	parts := []string{s.Kind}
	for _, p := range s.Params {
		parts = append(parts, strconv.FormatFloat(p, 'f', -1, 64))
	}
	return strings.Join(parts, ":")
}

// Label is how the spec reads on a chart: "SMA 50", "MACD 12, 26, 9".
func (s Spec) Label() string {
	label := kinds[s.Kind].label
	if len(s.Params) == 0 {
		return label
	}
	params := make([]string, len(s.Params))
	for i, p := range s.Params {
		params[i] = strconv.FormatFloat(p, 'f', -1, 64)
	}
	return label + " " + strings.Join(params, ", ")
}

// Warmup is how many bars before the first one shown the spec needs.
func (s Spec) Warmup() int { return kinds[s.Kind].warmup(s.Params) }

// Parse reads one spec. Missing parameters take their defaults, so "rsi" is
// RSI 14; extra ones, zeros, fractions of a bar and out-of-range values are
// refused with a sentence saying which.
func Parse(raw string) (Spec, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), ":")
	k, ok := kinds[parts[0]]
	if !ok {
		return Spec{}, fmt.Errorf("unknown indicator %q (want one of sma, ema, bb, vwap, rsi, macd, stoch, atr, obv)", parts[0])
	}
	if len(parts)-1 > len(k.defaults) {
		return Spec{}, fmt.Errorf("%s takes at most %d parameters", k.label, len(k.defaults))
	}
	s := Spec{Kind: parts[0], Params: append([]float64(nil), k.defaults...)}
	for i, raw := range parts[1:] {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return Spec{}, fmt.Errorf("%s: %q is not a number", k.label, raw)
		}
		if k.integer[i] {
			if v != math.Trunc(v) || v < 1 || v > MaxPeriod {
				return Spec{}, fmt.Errorf("%s: a period must be a whole number of bars from 1 to %d", k.label, MaxPeriod)
			}
		} else if v <= 0 || v > 10 {
			return Spec{}, fmt.Errorf("%s: the band width must be more than 0 and at most 10 deviations", k.label)
		}
		s.Params[i] = v
	}
	if s.Kind == KindMACD && s.Params[0] >= s.Params[1] {
		return Spec{}, errors.New("MACD: the fast period must be shorter than the slow one")
	}
	return s, nil
}

// ParseList reads a comma-separated list of specs, deduplicated.
func ParseList(raw string) ([]Spec, error) {
	var out []Spec
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		s, err := Parse(part)
		if err != nil {
			return nil, err
		}
		if !seen[s.Key()] {
			seen[s.Key()] = true
			out = append(out, s)
		}
	}
	if len(out) > MaxSpecs {
		return nil, fmt.Errorf("at most %d indicators at once", MaxSpecs)
	}
	return out, nil
}

// Line is one drawn series. Values are aligned with the bars they were
// computed from; nil is "not defined here".
type Line struct {
	Name   string     `json:"name"`
	Style  string     `json:"style"`
	Values []*float64 `json:"values"`
}

func line(name string, v []float64) Line { return Line{Name: name, Style: "line", Values: pointers(v)} }
func histogram(name string, v []float64) Line {
	return Line{Name: name, Style: "histogram", Values: pointers(v)}
}

func pointers(v []float64) []*float64 {
	out := make([]*float64, len(v))
	for i := range v {
		if !math.IsNaN(v[i]) && !math.IsInf(v[i], 0) {
			x := v[i]
			out[i] = &x
		}
	}
	return out
}

// Result is one spec, computed.
type Result struct {
	Key    string      `json:"key"`
	Label  string      `json:"label"`
	Pane   string      `json:"pane"`
	Bounds *[2]float64 `json:"bounds,omitempty"`
	Lines  []Line      `json:"lines"`
}

// bars is a candle series split into the columns the formulas take.
type bars struct {
	high, low, close, volume, vwap []float64
	day                            []int64
}

func columns(candles []quotes.Candle) bars {
	b := bars{
		high: make([]float64, len(candles)), low: make([]float64, len(candles)),
		close: make([]float64, len(candles)), volume: make([]float64, len(candles)),
		vwap: make([]float64, len(candles)), day: make([]int64, len(candles)),
	}
	for i, c := range candles {
		b.high[i], b.low[i], b.close[i] = c.High, c.Low, c.Close
		b.volume[i], b.vwap[i] = float64(c.Volume), c.VWAP
		b.day[i] = c.Time.Unix() / 86400
	}
	return b
}

// Compute runs every spec over the same candles, oldest first. Each result's
// lines are as long as candles; trimming off the warm-up is the caller's.
func Compute(specs []Spec, candles []quotes.Candle) []Result {
	b := columns(candles)
	out := make([]Result, 0, len(specs))
	for _, s := range specs {
		k := kinds[s.Kind]
		out = append(out, Result{Key: s.Key(), Label: s.Label(), Pane: k.pane, Bounds: k.bounds, Lines: k.compute(s.Params, b)})
	}
	return out
}

// Trim drops the first n values of every line: the warm-up bars fetched so
// the first bar shown has a settled value, and which the caller doesn't show.
func Trim(results []Result, n int) []Result {
	for i := range results {
		for j := range results[i].Lines {
			v := results[i].Lines[j].Values
			results[i].Lines[j].Values = v[min(n, len(v)):]
		}
	}
	return results
}
