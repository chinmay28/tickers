package strategy

import (
	"strings"
	"testing"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func study(t *testing.T, d StudyDefinition) StudyPlan {
	t.Helper()
	if d.Symbol == "" {
		d.Symbol = "TEST"
	}
	if d.From == "" {
		d.From = "2024-01-01"
	}
	p, err := CompileStudy(d, t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

// chain builds daily bars from closes, each opening at the close before it,
// so a return from the next open is a return from the close.
func chain(closes ...float64) []quotes.Candle {
	rows := make([][4]float64, len(closes))
	for i, c := range closes {
		o := c
		if i > 0 {
			o = closes[i-1]
		}
		rows[i] = [4]float64{o, max(o, c), min(o, c), c}
	}
	return ohlc(rows...)
}

func TestAStudyMeasuresFromTheNextOpen(t *testing.T) {
	// Close goes over 10 on bar 1. Anyone acting on it buys bar 2's open
	// (11), so one bar on is bar 2's close (12) and two bars on is bar 3's
	// (8.8): +9.09% and -20%, not the +14%/-16% measured from the close.
	bars := ohlc(flat(9), flat(10.5), [4]float64{11, 12, 11, 12}, flat(8.8), flat(8.8))
	p := study(t, StudyDefinition{Signal: rule(Condition{"close", ">", "10"}), Horizons: []int{2, 1}})
	s := p.Run(bars, 0)
	if s.Signals != 1 {
		t.Fatalf("signals = %d, want one — the stretch over 10 is bars 1–2, and only its first bar counts", s.Signals)
	}
	o := s.Occurrences[0]
	if !o.Time.Equal(bars[1].Time) || o.Entry != 11 {
		t.Errorf("occurrence = %+v, want bar 1 entered at bar 2's open", o)
	}
	if s.Horizons[0].Bars != 1 || s.Horizons[1].Bars != 2 {
		t.Fatalf("horizons = %+v, want them sorted", s.Horizons)
	}
	near(t, "1-bar return", *o.Returns[0], (12.0/11-1)*100)
	near(t, "2-bar return", *o.Returns[1], (8.8/11-1)*100)
}

func TestEveryBarCountsEachBarOfAStretch(t *testing.T) {
	bars := ohlc(flat(9), flat(11), flat(12), flat(9), flat(13), flat(13))
	cond := rule(Condition{"close", ">", "10"})
	if s := study(t, StudyDefinition{Signal: cond}).Run(bars, 0); s.Signals != 2 {
		t.Errorf("first bars only: %d signals, want 2 (bars 1 and 4)", s.Signals)
	}
	s := study(t, StudyDefinition{Signal: cond, EveryBar: true}).Run(bars, 0)
	if s.Signals != 4 {
		t.Errorf("every bar: %d signals, want 4", s.Signals)
	}
	if !s.HoldsNow {
		t.Error("the rule holds on the last bar, and the study didn't say so")
	}
	// The last two occurrences are too recent to have a next open, let
	// alone five bars after.
	last := s.Occurrences[len(s.Occurrences)-1]
	if last.Entry != 0 || last.Returns[0] != nil {
		t.Errorf("the last bar's occurrence = %+v, want nothing measured past the data", last)
	}
	if s.Horizons[0].Count != 3 {
		t.Errorf("1-bar count = %d, want 3 — the last occurrence can't be measured", s.Horizons[0].Count)
	}
}

func TestAStretchBegunInTheWarmUpIsNotANewSignal(t *testing.T) {
	bars := ohlc(flat(11), flat(11), flat(11), flat(9), flat(11), flat(11))
	s := study(t, StudyDefinition{Signal: rule(Condition{"close", ">", "10"})}).Run(bars, 2)
	if s.Signals != 1 || !s.Occurrences[0].Time.Equal(bars[4].Time) {
		t.Errorf("occurrences = %+v, want only bar 4 — bar 2 continues a stretch the warm-up started", s.Occurrences)
	}
	if s.Bars != 4 {
		t.Errorf("bars = %d, want the 4 from start on", s.Bars)
	}
}

func TestStatsAndTheBaseline(t *testing.T) {
	// Signals on bars 0, 2 and 3. One bar on, bar 0 was followed by +10%
	// and bar 2 by -10%; bar 3 is the last and can't be measured. From
	// every bar: +10%, -10%, -10%.
	bars := chain(100, 110, 99, 89.1)
	s := study(t, StudyDefinition{Signal: rule(Condition{"close", "<", "105"}), Horizons: []int{1}, EveryBar: true}).Run(bars, 0)
	h := s.Horizons[0]
	if h.Count != 2 {
		t.Fatalf("count = %d, want 2 measurable signals (bars 0 and 2)", h.Count)
	}
	near(t, "mean", h.Mean, 0)
	near(t, "median", h.Median, 0)
	near(t, "win rate", h.WinRate, 50)
	near(t, "best", h.Best, 10)
	near(t, "worst", h.Worst, -10)
	if h.Baseline.Count != 3 {
		t.Errorf("baseline count = %d, want every bar with a bar after it", h.Baseline.Count)
	}
	near(t, "baseline win rate", h.Baseline.WinRate, 100.0/3)
	near(t, "baseline mean", h.Baseline.Mean, -10.0/3)
}

func TestPoolWeighsEveryOccurrenceOnce(t *testing.T) {
	p := study(t, StudyDefinition{Signal: rule(Condition{"close", ">", "10"}), Horizons: []int{1}, EveryBar: true})
	a := p.Run(chain(11, 12, 12), 0) // +9.09%, 0%
	b := p.Run(chain(11, 22), 0)     // +100%
	pooled := Pool([]Study{a, b})
	if len(pooled) != 1 || pooled[0].Count != 3 {
		t.Fatalf("pooled = %+v, want three occurrences at one horizon", pooled)
	}
	near(t, "pooled median", pooled[0].Median, (12.0/11-1)*100)
	if pooled[0].Baseline.Count != a.Horizons[0].Baseline.Count+b.Horizons[0].Baseline.Count {
		t.Errorf("pooled baseline = %+v, want the two baselines' counts added", pooled[0].Baseline)
	}
	near(t, "pooled win rate", pooled[0].WinRate, 200.0/3)
	if len(Pool(nil)) != 0 {
		t.Error("pooling nothing produced horizons")
	}
}

func TestCompileStudyRefusesWhatCannotRun(t *testing.T) {
	now := t0.AddDate(1, 0, 0)
	ok := StudyDefinition{Symbol: "x", From: "2024-01-01", Signal: rule(Condition{"rsi", "<", "30"})}
	p, err := CompileStudy(ok, now)
	if err != nil {
		t.Fatalf("a valid study was refused: %v", err)
	}
	if p.Def.Symbol != "X" || len(p.Horizons) != 3 || p.Warmup() < 14 {
		t.Errorf("plan = %+v, want the symbol upper-cased, default horizons and RSI's warm-up", p)
	}
	for name, mutate := range map[string]func(*StudyDefinition){
		"no signal":        func(d *StudyDefinition) { d.Signal = Rule{} },
		"a zero horizon":   func(d *StudyDefinition) { d.Horizons = []int{0} },
		"too far ahead":    func(d *StudyDefinition) { d.Horizons = []int{MaxHorizon + 1} },
		"too many":         func(d *StudyDefinition) { d.Horizons = []int{1, 2, 3, 4, 5, 6, 7, 8, 9} },
		"a bad operand":    func(d *StudyDefinition) { d.Signal = rule(Condition{"nope", ">", "1"}) },
		"no start":         func(d *StudyDefinition) { d.From = "" },
		"an unknown width": func(d *StudyDefinition) { d.Interval = "3d" },
	} {
		d := ok
		mutate(&d)
		if _, err := CompileStudy(d, now); err == nil || !IsInvalid(err) {
			t.Errorf("%s: err = %v, want an InvalidError", name, err)
		}
	}
	if _, err := CompileStudy(StudyDefinition{Symbol: "x", From: "2024-01-01", Signal: rule(Condition{"nope", ">", "1"})}, now); !strings.Contains(err.Error(), "signal condition 1") {
		t.Errorf("err = %v, want it to name the signal condition", err)
	}
}
