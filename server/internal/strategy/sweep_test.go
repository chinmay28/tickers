package strategy

import (
	"encoding/json"
	"strings"
	"testing"
)

const crossTemplate = `{
	"symbol": "SPY", "from": "2020-01-01", "stopLoss": "{stop}",
	"entry": {"conditions": [{"left": "sma:{fast}", "op": "crosses_above", "right": "sma:{slow}"}]},
	"exit": {"conditions": [{"left": "rsi", "op": ">", "right": "{level}"}]}
}`

func TestGridExpandsEveryCombinationInOrder(t *testing.T) {
	vs, err := Grid(json.RawMessage(crossTemplate), map[string][]float64{
		"fast": {10, 20}, "slow": {100, 200}, "stop": {5}, "level": {70.5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 4 {
		t.Fatalf("got %d variants, want 2×2×1×1", len(vs))
	}
	// Parameters by name — fast, level, slow, stop — the last varying fastest.
	want := [][2]float64{{10, 100}, {10, 200}, {20, 100}, {20, 200}}
	for i, v := range vs {
		if v.Params["fast"] != want[i][0] || v.Params["slow"] != want[i][1] {
			t.Errorf("variant %d = %v, want fast %v slow %v", i, v.Params, want[i][0], want[i][1])
		}
	}
	d := vs[1].Definition
	if d.Entry.Conditions[0].Left != "sma:10" || d.Entry.Conditions[0].Right != "sma:200" {
		t.Errorf("entry = %+v, want the placeholders inside the operands filled", d.Entry)
	}
	if d.Exit.Conditions[0].Right != "70.5" || d.StopLoss != 5 {
		t.Errorf("exit %+v, stop %v: want a whole-string placeholder filled and a numeric field read back as a number", d.Exit, d.StopLoss)
	}
	if _, err := Compile(d, t0); err != nil {
		t.Errorf("a variant didn't compile: %v", err)
	}
}

func TestGridRefusesWhatItCannotExpand(t *testing.T) {
	all := map[string][]float64{"fast": {10}, "slow": {100}, "stop": {5}, "level": {70}}
	with := func(edit func(map[string][]float64)) map[string][]float64 {
		p := map[string][]float64{}
		for k, v := range all {
			p[k] = v
		}
		edit(p)
		return p
	}
	many := make([]float64, MaxGridValues)
	for i := range many {
		many[i] = float64(i + 1)
	}
	for name, tc := range map[string]struct {
		template string
		params   map[string][]float64
		want     string
	}{
		"not an object":     {`[1]`, all, "JSON object"},
		"no parameters":     {crossTemplate, nil, "at least one"},
		"an unused one":     {`{"stopLoss": "{x}"}`, map[string][]float64{"x": {1}, "other": {1}}, `"other" isn't used`},
		"a missing one":     {crossTemplate, with(func(p map[string][]float64) { delete(p, "level") }), "{level}"},
		"no values":         {crossTemplate, with(func(p map[string][]float64) { p["fast"] = nil }), "needs from 1"},
		"too many variants": {crossTemplate, with(func(p map[string][]float64) { p["fast"] = many; p["slow"] = many }), "more than"},
		"an unknown field":  {`{"symbl": "{x}"}`, map[string][]float64{"x": {1}}, "isn't a strategy definition"},
		"a bad number":      {`{"stopLoss": "five {x}"}`, map[string][]float64{"x": {1}}, "stopLoss must be a number"},
	} {
		_, err := Grid(json.RawMessage(tc.template), tc.params)
		if err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want an InvalidError mentioning %q", name, err, tc.want)
		}
	}
}

func TestScoreRanksHigherAsBetter(t *testing.T) {
	pf := 2.5
	r := Result{Strategy: Metrics{Sharpe: 1.2, TotalReturn: 30, MaxDrawdown: -12}, Hold: Metrics{TotalReturn: 50}, Stats: Stats{ProfitFactor: &pf}}
	for ranking, want := range map[string]float64{"": 1.2, RankTotalReturn: 30, RankMaxDrawdown: -12, RankProfitFactor: 2.5, RankExcess: -20} {
		if got, ok, err := Score(r, ranking); err != nil || !ok || got != want {
			t.Errorf("Score(%q) = %v %v %v, want %v", ranking, got, ok, err, want)
		}
	}
	if _, ok, _ := Score(Result{}, RankProfitFactor); ok {
		t.Error("a result with no losing trade has a profit factor")
	}
	if _, _, err := Score(r, "luck"); !IsInvalid(err) {
		t.Errorf("an unknown ranking gave %v, want an InvalidError", err)
	}
}
