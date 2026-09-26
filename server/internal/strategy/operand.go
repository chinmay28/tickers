package strategy

import (
	"math"
	"time"

	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Operand is one side of a condition on its own — "rsi:14", "bb:20:2.lower",
// "close" — for a caller that wants its values rather than a comparison: a
// cross-sectional factor ranks symbols by exactly what a rule would compare.
type Operand struct{ o operand }

// ParseOperand reads an operand as a condition would. A bare number is
// refused: it has the same value for every symbol, so ranks nothing.
func ParseOperand(raw string) (Operand, error) {
	o, err := parseOperand(raw)
	if err != nil {
		return Operand{}, &InvalidError{msg: err.Error()}
	}
	if o.isNum {
		return Operand{}, &InvalidError{msg: "a number is the same for every symbol, so it can't be a factor"}
	}
	return Operand{o}, nil
}

// Warmup is how many bars before the first value it needs to be settled.
func (op Operand) Warmup() int {
	if op.o.spec.Kind == "" {
		return 0
	}
	return op.o.spec.Warmup()
}

// Series is the operand's value on every bar, NaN where it isn't defined.
func (op Operand) Series(bars []quotes.Candle) []float64 {
	var specs []indicators.Spec
	if op.o.spec.Kind != "" {
		specs = []indicators.Spec{op.o.spec}
	}
	f := newFrame(specs, bars)
	out := make([]float64, len(bars))
	for i := range bars {
		v := f.value(op.o, i)
		if math.IsInf(v, 0) {
			v = math.NaN()
		}
		out[i] = v
	}
	return out
}

// Measure summarises an equity curve the way a backtest's is: total return,
// CAGR, drawdown and the per-bar statistics, from initial.
func Measure(initial float64, curve []float64, first, last time.Time, interval quotes.Interval) Metrics {
	return metrics(initial, curve, first, last, BarsPerYear(interval))
}

// Signal is a rule compiled on its own, for asking on which bars it held
// rather than trading on it: a filter on who a cross-sectional ranking
// considers.
type Signal struct {
	rule  compiled
	specs []indicators.Spec
}

// CompileSignal validates a rule. It needs at least one condition.
func CompileSignal(r Rule) (Signal, error) {
	c, err := compileRule("filter", r)
	if err != nil {
		return Signal{}, &InvalidError{msg: err.Error()}
	}
	if len(c.conds) == 0 {
		return Signal{}, &InvalidError{msg: "a filter rule needs at least one condition"}
	}
	s := Signal{rule: c, specs: specsOf(c)}
	if len(s.specs) > indicators.MaxSpecs {
		return Signal{}, &InvalidError{msg: "a filter rule uses too many indicators"}
	}
	return s, nil
}

// Warmup is how many bars before the first judged one the rule needs.
func (s Signal) Warmup() int { return warmup(s.specs) }

// Holds reports, for every bar, whether the rule held on it.
func (s Signal) Holds(bars []quotes.Candle) []bool {
	f := newFrame(s.specs, bars)
	out := make([]bool, len(bars))
	for i := range bars {
		out[i] = f.holds(s.rule, i)
	}
	return out
}
