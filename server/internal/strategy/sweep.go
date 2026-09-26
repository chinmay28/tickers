package strategy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Bounds on a parameter grid. A grid is a product, so a few parameters with
// a handful of values each is already hundreds of backtests — on a Pi.
const (
	MaxGridParams   = 4
	MaxGridValues   = 50
	MaxGridVariants = 250
)

// Variant is one point of a grid: the values chosen, and the definition
// they make.
type Variant struct {
	Params     map[string]float64 `json:"params"`
	Definition Definition         `json:"definition"`
}

// placeholder is a grid parameter's spelling inside a template: {fast}.
var placeholder = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// numericFields are the definition's numbers. A template spells a swept
// number as a string — "stopLoss": "{stop}" — because a placeholder isn't
// JSON; after substitution it is read back as the number it now spells.
var numericFields = []string{"stopLoss", "takeProfit", "feePercent", "initial"}

// Grid expands a template definition over every combination of params.
//
// The template is a definition whose strings may hold placeholders —
// "sma:{fast}", "{level}" — each named in params with the values to try.
// Substitution is textual, so a placeholder can be a whole operand, part of
// one, or a whole numeric field. Variants come in a fixed order: parameters
// by name, the last varying fastest.
//
// A variant that substitutes into something that can't run — a MACD whose
// fast period isn't shorter than its slow one — is still returned; whether
// a definition runs is Compile's to say, one variant at a time, so one bad
// corner of a grid doesn't sink the rest.
func Grid(template json.RawMessage, params map[string][]float64) (vs []Variant, err error) {
	defer func() {
		if err != nil {
			err = &InvalidError{msg: err.Error()}
		}
	}()
	var tree map[string]any
	if err := json.Unmarshal(template, &tree); err != nil || tree == nil {
		return nil, errors.New("the template must be a strategy definition, as a JSON object")
	}
	if len(params) == 0 {
		return nil, errors.New("a grid needs at least one parameter to vary")
	}
	if len(params) > MaxGridParams {
		return nil, fmt.Errorf("a grid can vary at most %d parameters", MaxGridParams)
	}
	used := map[string]bool{}
	walkStrings(tree, func(s string) string {
		for _, m := range placeholder.FindAllStringSubmatch(s, -1) {
			used[m[1]] = true
		}
		return s
	})
	names := make([]string, 0, len(params))
	total := 1
	for name, values := range params {
		if !used[name] {
			return nil, fmt.Errorf("the parameter %q isn't used in the template; write it as {%s}", name, name)
		}
		if len(values) == 0 || len(values) > MaxGridValues {
			return nil, fmt.Errorf("the parameter %q needs from 1 to %d values", name, MaxGridValues)
		}
		for _, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("the parameter %q has a value that isn't a number", name)
			}
		}
		names = append(names, name)
		total *= len(values)
		if total > MaxGridVariants {
			return nil, fmt.Errorf("that grid is more than %d backtests; vary fewer parameters or try fewer values", MaxGridVariants)
		}
	}
	for name := range used {
		if _, ok := params[name]; !ok {
			return nil, fmt.Errorf("the template uses {%s} but no values are given for it", name)
		}
	}
	slices.Sort(names)

	idx := make([]int, len(names))
	for {
		chosen := make(map[string]float64, len(names))
		for i, name := range names {
			chosen[name] = params[name][idx[i]]
		}
		def, err := substitute(template, chosen)
		if err != nil {
			return nil, err
		}
		vs = append(vs, Variant{Params: chosen, Definition: def})
		// Advance the odometer, last parameter fastest.
		i := len(idx) - 1
		for ; i >= 0; i-- {
			if idx[i]++; idx[i] < len(params[names[i]]) {
				break
			}
			idx[i] = 0
		}
		if i < 0 {
			return vs, nil
		}
	}
}

// substitute fills a template's placeholders and reads it as a definition.
func substitute(template json.RawMessage, chosen map[string]float64) (Definition, error) {
	var tree map[string]any
	json.Unmarshal(template, &tree) // already known to parse
	walkStrings(tree, func(s string) string {
		return placeholder.ReplaceAllStringFunc(s, func(m string) string {
			return strconv.FormatFloat(chosen[m[1:len(m)-1]], 'f', -1, 64)
		})
	})
	for _, field := range numericFields {
		if s, ok := tree[field].(string); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return Definition{}, fmt.Errorf("%s must be a number or a placeholder standing for one", field)
			}
			tree[field] = v
		}
	}
	raw, _ := json.Marshal(tree)
	var def Definition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return Definition{}, fmt.Errorf("the template isn't a strategy definition: %v", err)
	}
	return def, nil
}

// walkStrings rewrites every string value in a decoded JSON tree in place.
func walkStrings(v any, fn func(string) string) any {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		for k, x := range t {
			t[k] = walkStrings(x, fn)
		}
	case []any:
		for i, x := range t {
			t[i] = walkStrings(x, fn)
		}
	}
	return v
}

// Rankings a set of results can be ordered by.
const (
	RankSharpe       = "sharpe"
	RankTotalReturn  = "totalReturn"
	RankCAGR         = "cagr"
	RankMaxDrawdown  = "maxDrawdown"
	RankProfitFactor = "profitFactor"
	RankWinRate      = "winRate"
	// RankExcess is the strategy's total return over buy-and-hold's in the
	// same window: whether trading beat not trading.
	RankExcess = "excessReturn"
)

// Rankings lists every ranking, for a caller to show.
var Rankings = []string{RankSharpe, RankTotalReturn, RankCAGR, RankMaxDrawdown, RankProfitFactor, RankWinRate, RankExcess}

// Score is a result's value under a ranking, higher always better — a
// drawdown is negative, so the shallowest is the highest. False means the
// result has no value under it: a profit factor with no losing trade.
func Score(r Result, ranking string) (float64, bool, error) {
	switch ranking {
	case RankSharpe, "":
		return r.Strategy.Sharpe, true, nil
	case RankTotalReturn:
		return r.Strategy.TotalReturn, true, nil
	case RankCAGR:
		return r.Strategy.CAGR, true, nil
	case RankMaxDrawdown:
		return r.Strategy.MaxDrawdown, true, nil
	case RankProfitFactor:
		if r.Stats.ProfitFactor == nil {
			return 0, false, nil
		}
		return *r.Stats.ProfitFactor, true, nil
	case RankWinRate:
		return r.Stats.WinRate, true, nil
	case RankExcess:
		return r.Strategy.TotalReturn - r.Hold.TotalReturn, true, nil
	}
	return 0, false, &InvalidError{msg: fmt.Sprintf("unknown ranking %q (want one of %s)", ranking, strings.Join(Rankings, ", "))}
}
