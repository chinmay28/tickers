package xsection

import "sort"

// Ranked is one symbol's place in a screen.
type Ranked struct {
	Symbol string
	Value  float64
	Close  float64
}

// Screen ranks the symbols eligible on day i by the factor, highest first
// unless ascending, and reports how many were eligible at all.
func Screen(p *Panel, f Factor, filter Filter, i int, ascending bool) []Ranked {
	values := f.Values(p)
	ok := p.eligible(filter)
	out := []Ranked{}
	for s, sym := range p.Symbols {
		if ok(s, i) && defined(values[s][i]) {
			out = append(out, Ranked{Symbol: sym, Value: values[s][i], Close: p.Close[s][i]})
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Value == out[b].Value {
			return out[a].Symbol < out[b].Symbol
		}
		return (out[a].Value > out[b].Value) != ascending
	})
	return out
}
