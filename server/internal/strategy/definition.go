// Package strategy backtests rule-based trading strategies on one symbol:
// "buy when the 50-day average crosses above the 200-day, sell when RSI goes
// over 70".
//
// It is pure, like indicators: a Definition is compiled into a Plan, and a
// Plan is simulated over bars handed to it. Reading the bars, warming the
// indicators up and adjusting for dividends is the engine's job; this package
// knows nothing about the archive, the API or the clock, which is what lets
// every rule about fills, stops and metrics be tested on a dozen bars worked
// by hand.
package strategy

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Definition is a strategy as a person writes it and as it is saved: plain
// strings and numbers, so the JSON is readable and the same shape travels
// from the editor to the store and back.
type Definition struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	// From and To bound the test, as YYYY-MM-DD. To is inclusive; empty is
	// today.
	From string `json:"from"`
	To   string `json:"to"`
	// Entry opens a position when it holds and none is open; Exit closes
	// one when it holds. An empty Exit holds until a stop, a target or the
	// end of the test.
	Entry Rule `json:"entry"`
	Exit  Rule `json:"exit"`
	// StopLoss and TakeProfit are percentages from the entry price; zero is
	// none.
	StopLoss   float64 `json:"stopLoss"`
	TakeProfit float64 `json:"takeProfit"`
	// FeePercent is charged on the value traded, both ways.
	FeePercent float64 `json:"feePercent"`
	// Initial is the starting cash.
	Initial float64 `json:"initial"`
	// Dividends trades on dividend-adjusted daily prices, so a strategy
	// holding a payer is credited its payouts as buy-and-hold would be.
	Dividends bool `json:"dividends"`
}

// Rule is a list of conditions and how they combine.
type Rule struct {
	// Match is "all" (every condition) or "any" (at least one).
	Match      string      `json:"match"`
	Conditions []Condition `json:"conditions"`
}

// Condition compares two operands: "sma:50 crosses_above sma:200",
// "rsi:14 > 70", "close < bb:20:2.lower".
type Condition struct {
	Left  string `json:"left"`
	Op    string `json:"op"`
	Right string `json:"right"`
}

// Comparison operators. A cross compares two bars: the left was at or below
// the right on the previous bar and is above it on this one (or the mirror).
const (
	OpAbove        = ">"
	OpBelow        = "<"
	OpAtLeast      = ">="
	OpAtMost       = "<="
	OpCrossesAbove = "crosses_above"
	OpCrossesBelow = "crosses_below"
)

// Bounds on a definition.
const (
	MaxConditions = 8
	MaxPercent    = 1000
	MaxFeePercent = 10
	MaxInitial    = 1e12
)

// Plan is a compiled definition: its window, the indicators it needs, and
// its rules as functions of a bar index.
type Plan struct {
	Def      Definition
	Interval quotes.Interval
	From, To time.Time
	Specs    []indicators.Spec
	entry    compiled
	exit     compiled
}

type compiled struct {
	any   bool
	conds []compiledCondition
}

type compiledCondition struct {
	left, right operand
	op          string
}

// operand is one side of a condition: a price field, a constant, or a line
// of an indicator.
type operand struct {
	text  string
	field string  // open, high, low, close, volume
	value float64 // a constant
	isNum bool
	spec  indicators.Spec
	line  string // the indicator line's canonical name
}

// InvalidError is a definition that can't run. Its message is a sentence
// naming the field, for the editor to show as it is.
type InvalidError struct{ msg string }

func (e *InvalidError) Error() string {
	if e.msg == "" {
		return "those rules are not a strategy this version understands"
	}
	return e.msg
}

// IsInvalid reports whether err is a definition's fault rather than the
// server's.
func IsInvalid(err error) bool {
	var ie *InvalidError
	return errors.As(err, &ie)
}

// Compile validates a definition and turns it into a Plan. Every refusal is an
// InvalidError.
func Compile(d Definition, now time.Time) (p Plan, err error) {
	defer func() {
		if err != nil {
			err = &InvalidError{msg: err.Error()}
		}
	}()
	p = Plan{Def: d}
	d.Symbol = strings.ToUpper(strings.TrimSpace(d.Symbol))
	p.Def.Symbol = d.Symbol
	if d.Symbol == "" {
		return p, errors.New("a symbol is required")
	}
	if d.Interval == "" {
		d.Interval = "1d"
		p.Def.Interval = "1d"
	}
	interval, perr := quotes.ParseInterval(d.Interval)
	if perr != nil {
		return p, perr
	}
	p.Interval = interval
	if p.From, err = time.Parse(time.DateOnly, d.From); err != nil {
		return p, errors.New("the start date must be a date like 2015-01-31")
	}
	if d.To == "" {
		p.To = now
	} else if p.To, err = time.Parse(time.DateOnly, d.To); err != nil {
		return p, errors.New("the end date must be a date like 2024-12-31")
	} else {
		p.To = p.To.AddDate(0, 0, 1)
	}
	if !p.From.Before(p.To) {
		return p, errors.New("the start date must be before the end date")
	}
	for name, v := range map[string]float64{"stop-loss": d.StopLoss, "take-profit": d.TakeProfit} {
		if v < 0 || v > MaxPercent || math.IsNaN(v) {
			return p, fmt.Errorf("the %s must be between 0 (none) and %d%%", name, MaxPercent)
		}
	}
	if d.StopLoss >= 100 {
		return p, errors.New("a stop-loss of 100% or more can never trigger on a long position")
	}
	if d.FeePercent < 0 || d.FeePercent > MaxFeePercent || math.IsNaN(d.FeePercent) {
		return p, fmt.Errorf("the fee must be between 0 and %d%% a trade", MaxFeePercent)
	}
	if d.Initial == 0 {
		p.Def.Initial = 10000
	} else if d.Initial < 0 || d.Initial > MaxInitial || math.IsNaN(d.Initial) {
		return p, errors.New("the starting capital must be a positive amount")
	}

	seen := map[string]bool{}
	addSpec := func(o operand) {
		if o.spec.Kind != "" && !seen[o.spec.Key()] {
			seen[o.spec.Key()] = true
			p.Specs = append(p.Specs, o.spec)
		}
	}
	for _, side := range []struct {
		name string
		rule Rule
		out  *compiled
	}{{"entry", d.Entry, &p.entry}, {"exit", d.Exit, &p.exit}} {
		switch side.rule.Match {
		case "", "all":
		case "any":
			side.out.any = true
		default:
			return p, fmt.Errorf("the %s rule must match \"all\" or \"any\" of its conditions", side.name)
		}
		if len(side.rule.Conditions) > MaxConditions {
			return p, fmt.Errorf("the %s rule can have at most %d conditions", side.name, MaxConditions)
		}
		for i, c := range side.rule.Conditions {
			cc, err := compileCondition(c)
			if err != nil {
				return p, fmt.Errorf("%s condition %d: %w", side.name, i+1, err)
			}
			addSpec(cc.left)
			addSpec(cc.right)
			side.out.conds = append(side.out.conds, cc)
		}
	}
	if len(p.entry.conds) == 0 {
		return p, errors.New("a strategy needs at least one entry condition")
	}
	if len(p.Specs) > indicators.MaxSpecs {
		return p, fmt.Errorf("a strategy can use at most %d different indicators", indicators.MaxSpecs)
	}
	return p, nil
}

func compileCondition(c Condition) (compiledCondition, error) {
	var cc compiledCondition
	switch c.Op {
	case OpAbove, OpBelow, OpAtLeast, OpAtMost, OpCrossesAbove, OpCrossesBelow:
		cc.op = c.Op
	default:
		return cc, fmt.Errorf("unknown comparison %q (want >, <, >=, <=, crosses_above or crosses_below)", c.Op)
	}
	var err error
	if cc.left, err = parseOperand(c.Left); err != nil {
		return cc, err
	}
	if cc.right, err = parseOperand(c.Right); err != nil {
		return cc, err
	}
	if cc.left.isNum && cc.right.isNum {
		return cc, errors.New("comparing two numbers is always true or always false")
	}
	return cc, nil
}

// lineNames is each indicator's lines by the short name a rule uses, and the
// line a bare spec means: "bb:20:2" is the middle band, "macd" the MACD line.
var lineNames = map[string]struct {
	lines map[string]string // rule name → indicators line name
	main  string
}{
	indicators.KindSMA:        {map[string]string{"sma": "SMA"}, "SMA"},
	indicators.KindEMA:        {map[string]string{"ema": "EMA"}, "EMA"},
	indicators.KindBollinger:  {map[string]string{"upper": "upper", "middle": "middle", "lower": "lower"}, "middle"},
	indicators.KindVWAP:       {map[string]string{"vwap": "VWAP"}, "VWAP"},
	indicators.KindRSI:        {map[string]string{"rsi": "RSI"}, "RSI"},
	indicators.KindMACD:       {map[string]string{"macd": "MACD", "signal": "signal", "histogram": "histogram", "hist": "histogram"}, "MACD"},
	indicators.KindStochastic: {map[string]string{"k": "%K", "d": "%D"}, "%K"},
	indicators.KindATR:        {map[string]string{"atr": "ATR"}, "ATR"},
	indicators.KindOBV:        {map[string]string{"obv": "OBV"}, "OBV"},
}

func parseOperand(raw string) (operand, error) {
	text := strings.ToLower(strings.TrimSpace(raw))
	o := operand{text: text}
	if text == "" {
		return o, errors.New("both sides of a condition need a value")
	}
	switch text {
	case "open", "high", "low", "close", "volume":
		o.field = text
		return o, nil
	case "price":
		o.field = "close"
		return o, nil
	}
	if v, err := strconv.ParseFloat(text, 64); err == nil {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return o, fmt.Errorf("%q is not a usable number", raw)
		}
		o.value, o.isNum = v, true
		return o, nil
	}
	specText, lineText, _ := strings.Cut(text, ".")
	spec, err := indicators.Parse(specText)
	if err != nil {
		return o, fmt.Errorf("%q is not a price field, a number or an indicator: %w", raw, err)
	}
	names := lineNames[spec.Kind]
	o.spec, o.line = spec, names.main
	if lineText != "" {
		line, ok := names.lines[strings.TrimPrefix(lineText, "%")]
		if !ok {
			return o, fmt.Errorf("%s has no line %q", spec.Label(), lineText)
		}
		o.line = line
	}
	return o, nil
}
