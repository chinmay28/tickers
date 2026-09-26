package strategy

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// StudyDefinition asks what followed a pattern: on which bars a rule held,
// and how the price moved over the bars after. It is the question a backtest
// answers only indirectly — whether a condition carries information — asked
// without an exit rule to get in the way.
type StudyDefinition struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	From     string `json:"from"`
	To       string `json:"to"`
	// Dividends measures on dividend-adjusted daily prices, as a backtest
	// does, so a payout isn't counted as a fall.
	Dividends bool `json:"dividends"`
	// Signal is the pattern, in the same language as a strategy's rules.
	Signal Rule `json:"signal"`
	// Horizons are how many bars after the signal to measure at; empty is
	// 1, 5 and 20.
	Horizons []int `json:"horizons"`
	// EveryBar counts every bar the rule holds on. Off, only the first bar
	// of each stretch counts: ten oversold days in a row are one event, not
	// ten that all say the same thing and make a pattern look ten times as
	// well attested as it is.
	EveryBar bool `json:"everyBar"`
}

// Bounds on a study.
const (
	MaxHorizons = 8
	MaxHorizon  = 1000
)

// DefaultHorizons are measured when a study names none: the next bar, a
// week and a month of daily bars.
var DefaultHorizons = []int{1, 5, 20}

// StudyPlan is a compiled study.
type StudyPlan struct {
	Def      StudyDefinition
	Interval quotes.Interval
	From, To time.Time
	Specs    []indicators.Spec
	Horizons []int
	// Others are the bars of the series in References; see Plan.Others.
	Others map[string][]quotes.Candle
	signal compiled
}

// CompileStudy validates a study. Every refusal is an InvalidError.
func CompileStudy(d StudyDefinition, now time.Time) (p StudyPlan, err error) {
	defer func() {
		if err != nil {
			err = &InvalidError{msg: err.Error()}
		}
	}()
	p = StudyPlan{Def: d}
	if p.Def.Symbol, p.Def.Interval, p.Interval, err = compileSeries(d.Symbol, d.Interval); err != nil {
		return p, err
	}
	if p.From, p.To, err = compileWindow(d.From, d.To, now); err != nil {
		return p, err
	}
	if p.signal, err = compileRule("signal", d.Signal); err != nil {
		return p, err
	}
	if len(p.signal.conds) == 0 {
		return p, errors.New("a study needs at least one signal condition")
	}
	if p.Specs = specsOf(p.signal); len(p.Specs) > indicators.MaxSpecs {
		return p, fmt.Errorf("a signal can use at most %d different indicators", indicators.MaxSpecs)
	}
	horizons := d.Horizons
	if len(horizons) == 0 {
		horizons = DefaultHorizons
	}
	if len(horizons) > MaxHorizons {
		return p, fmt.Errorf("a study can measure at most %d horizons", MaxHorizons)
	}
	for _, h := range horizons {
		if h < 1 || h > MaxHorizon {
			return p, fmt.Errorf("a horizon must be from 1 to %d bars", MaxHorizon)
		}
	}
	p.Horizons = slices.Compact(slices.Sorted(slices.Values(horizons)))
	p.Def.Horizons = p.Horizons
	return p, nil
}

// Warmup is how many bars before the window the signal needs; see
// Plan.Warmup.
func (p StudyPlan) Warmup() int { return max(warmup(p.Specs), refWarmup(p.signal)) }

// References are the other series the signal reads; see Plan.References.
func (p StudyPlan) References() []string { return references(p.signal) }

// Occurrence is one bar the signal fired on, and what followed.
type Occurrence struct {
	Time  time.Time `json:"time"`
	Close float64   `json:"close"`
	// Entry is the next bar's open: the first price anyone acting on the
	// signal could have had, the same fill a backtest gets.
	Entry float64 `json:"entry"`
	// Returns are percentage changes from Entry to the close each horizon's
	// bars after the signal, aligned with the plan's horizons; nil where the
	// data ends first.
	Returns []*float64 `json:"returns"`
}

// HorizonStats summarises the forward returns at one horizon. Percentages
// are percentages.
type HorizonStats struct {
	Bars    int     `json:"bars"`
	Count   int     `json:"count"`
	Mean    float64 `json:"mean"`
	Median  float64 `json:"median"`
	WinRate float64 `json:"winRate"`
	Best    float64 `json:"best"`
	Worst   float64 `json:"worst"`
	// Baseline is the same measurement taken from every bar of the window,
	// signal or not. A pattern is worth something only as far as it beats
	// what simply holding over the same stretch did: in a market that rose
	// 10% a year, a signal followed by a rise has said nothing yet.
	Baseline Baseline `json:"baseline"`
}

// Baseline is the unconditional forward return at a horizon.
type Baseline struct {
	Count   int     `json:"count"`
	Mean    float64 `json:"mean"`
	WinRate float64 `json:"winRate"`
}

// Study is a study's outcome on one symbol.
type Study struct {
	Symbol   string    `json:"symbol"`
	Interval string    `json:"interval"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Bars     int       `json:"bars"`
	// Signals counts the occurrences, including any too recent to measure.
	Signals     int            `json:"signals"`
	Horizons    []HorizonStats `json:"horizons"`
	Occurrences []Occurrence   `json:"occurrences"`
	// HoldsNow says the rule holds on the last bar — the pattern is on
	// right now, whatever the history says it's worth.
	HoldsNow bool     `json:"holdsNow"`
	Warnings []string `json:"warnings"`
}

// Run finds the signal in bars from bars[start] on and measures what
// followed each occurrence. As in Simulate, the bars before start are
// warm-up, read by the indicators and by a cross looking back one bar but
// never counted.
func (p StudyPlan) Run(bars []quotes.Candle, start int) Study {
	st := Study{Symbol: p.Def.Symbol, Interval: string(p.Interval), Occurrences: []Occurrence{}, Warnings: []string{}}
	start = max(start, 0)
	n := len(bars)
	if start >= n {
		st.Warnings = append(st.Warnings, "there are no bars in that window")
		st.Horizons = summarize(nil, p.Horizons, make([]baseSum, len(p.Horizons)))
		return st
	}
	st.From, st.To, st.Bars = bars[start].Time, bars[n-1].Time, n-start

	f := newFrame(p.Specs, bars)
	f.attach(refOperands(p.signal), p.Others, p.Interval)
	fired := make([]bool, n)
	for i := range bars {
		fired[i] = f.holds(p.signal, i)
	}
	st.HoldsNow = fired[n-1]

	base := make([]baseSum, len(p.Horizons))
	for i := start; i < n; i++ {
		for k, h := range p.Horizons {
			if r, ok := forward(bars, i, h); ok {
				base[k].add(r)
			}
		}
		if !fired[i] || (!p.Def.EveryBar && i > 0 && fired[i-1]) {
			continue
		}
		o := Occurrence{Time: bars[i].Time, Close: bars[i].Close, Returns: make([]*float64, len(p.Horizons))}
		if i+1 < n {
			o.Entry = bars[i+1].Open
		}
		for k, h := range p.Horizons {
			if r, ok := forward(bars, i, h); ok {
				o.Returns[k] = &r
			}
		}
		st.Occurrences = append(st.Occurrences, o)
	}
	st.Signals = len(st.Occurrences)
	st.Horizons = summarize(st.Occurrences, p.Horizons, base)
	if st.Signals == 0 {
		st.Warnings = append(st.Warnings, "the signal never fired in that window")
	}
	return st
}

// forward is the percentage return from the open after bar i to the close h
// bars after it, and whether the bars reach that far.
func forward(bars []quotes.Candle, i, h int) (float64, bool) {
	if i+h >= len(bars) || bars[i+1].Open <= 0 {
		return 0, false
	}
	return (bars[i+h].Close/bars[i+1].Open - 1) * 100, true
}

// baseSum accumulates a baseline so baselines from several studies can be
// pooled exactly.
type baseSum struct {
	n, wins int
	sum     float64
}

func (b *baseSum) add(r float64) {
	b.n++
	b.sum += r
	if r > 0 {
		b.wins++
	}
}

func (b baseSum) baseline() Baseline {
	if b.n == 0 {
		return Baseline{}
	}
	return Baseline{Count: b.n, Mean: b.sum / float64(b.n), WinRate: float64(b.wins) / float64(b.n) * 100}
}

func summarize(occ []Occurrence, horizons []int, base []baseSum) []HorizonStats {
	out := make([]HorizonStats, len(horizons))
	for k, h := range horizons {
		var rs []float64
		for _, o := range occ {
			if k < len(o.Returns) && o.Returns[k] != nil {
				rs = append(rs, *o.Returns[k])
			}
		}
		hs := HorizonStats{Bars: h, Count: len(rs), Baseline: base[k].baseline()}
		if len(rs) > 0 {
			slices.Sort(rs)
			wins, sum := 0, 0.0
			for _, r := range rs {
				sum += r
				if r > 0 {
					wins++
				}
			}
			hs.Mean = sum / float64(len(rs))
			hs.WinRate = float64(wins) / float64(len(rs)) * 100
			hs.Worst, hs.Best = rs[0], rs[len(rs)-1]
			if m := len(rs) / 2; len(rs)%2 == 1 {
				hs.Median = rs[m]
			} else {
				hs.Median = (rs[m-1] + rs[m]) / 2
			}
		}
		out[k] = hs
	}
	return out
}

// Pool summarises several studies of the same signal and horizons as one: a
// pattern across a whole list of symbols, where each alone has too few
// occurrences to say much. Every occurrence counts once, so a symbol with
// more history weighs more.
func Pool(studies []Study) []HorizonStats {
	if len(studies) == 0 {
		return []HorizonStats{}
	}
	horizons := make([]int, len(studies[0].Horizons))
	for k, h := range studies[0].Horizons {
		horizons[k] = h.Bars
	}
	var occ []Occurrence
	base := make([]baseSum, len(horizons))
	for _, s := range studies {
		occ = append(occ, s.Occurrences...)
		for k := range base {
			if k >= len(s.Horizons) {
				continue
			}
			b := s.Horizons[k].Baseline
			base[k].n += b.Count
			base[k].sum += b.Mean * float64(b.Count)
			base[k].wins += int(math.Round(b.WinRate / 100 * float64(b.Count)))
		}
	}
	return summarize(occ, horizons, base)
}
