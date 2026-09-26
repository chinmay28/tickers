package engine

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

func TestRunStrategyOverTheArchive(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	def := strategy.Definition{Symbol: "VTI", Entry: strategy.Rule{Conditions: []strategy.Condition{{Left: "sma:5", Op: "crosses_above", Right: "sma:20"}}}}
	if _, err := eng.RunStrategy(strategy.Definition{Symbol: "VTI"}); err == nil {
		t.Error("a strategy with no start date ran")
	}
	def.From = time.Now().AddDate(0, -3, 0).Format(time.DateOnly)
	if _, err := eng.RunStrategy(def); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("with no archive: %v, want ErrNoArchive", err)
	}

	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	_, err := eng.RunStrategy(strategy.Definition{Symbol: "NEWCO", From: def.From, Entry: def.Entry})
	if !errors.Is(err, ErrNoBars) || !strings.Contains(err.Error(), "has been added") {
		t.Fatalf("an unknown symbol gave %v, want it added and said so", err)
	}
	if s, err := a.Lookup("NEWCO"); err != nil || !s.Priority {
		t.Errorf("NEWCO = %+v (%v), want it tracked and first in line", s, err)
	}

	// Named by the exchange lists but not collected — the lists were
	// switched off — is as good as unknown, and gets the same treatment.
	if err := a.Add(archive.Listed, archive.Entry{Symbol: "OLDCO"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := a.Remove(archive.Listed, "OLDCO"); err != nil {
		t.Fatal(err)
	}
	_, err = eng.RunStrategy(strategy.Definition{Symbol: "OLDCO", From: def.From, Entry: def.Entry})
	if !errors.Is(err, ErrNoBars) || !strings.Contains(err.Error(), "has been added") {
		t.Fatalf("a known but uncollected symbol gave %v, want it added and said so", err)
	}
	if s, _ := a.Lookup("OLDCO"); !s.Active {
		t.Errorf("OLDCO = %+v, want it collected after being asked for", s)
	}

	// A dip then a rally, so the fast average crosses the slow one.
	var closes []float64
	for i := 0; i < 200; i++ {
		p := 100.0
		if i > 120 {
			p = 100 + float64(i-120)
		} else if i > 80 {
			p = 100 - float64(i-80)*0.5
		}
		closes = append(closes, p)
	}
	archiveDaily(t, a, "VTI", closes, nil)
	res, err := eng.RunStrategy(def)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Trades == 0 || res.Strategy.TotalReturn <= 0 {
		t.Errorf("result = %+v, want the golden cross caught and in profit", res.Stats)
	}
	if first := res.From; first.Before(time.Now().AddDate(0, -3, -1)) {
		t.Errorf("the test started %s, before the window asked for — warm-up bars must not be traded", first)
	}
}

func TestAdjustCandlesCreditsDividends(t *testing.T) {
	day := func(n int) time.Time { return time.Date(2024, 1, 1+n, 0, 0, 0, 0, time.UTC) }
	bars := []quotes.Candle{
		{Time: day(0), Open: 100, High: 100, Low: 100, Close: 100},
		{Time: day(1), Open: 98, High: 98, Low: 98, Close: 98},
	}
	got := adjustCandles(bars, []quotes.Dividend{{Time: day(1), Amount: 2}})
	if got[0].Close != 98 || got[0].Open != 98 || got[1].Close != 98 {
		t.Errorf("adjusted = %+v, want the day before the $2 ex-date scaled to 98 so the drop isn't a loss", got)
	}
	if bars[0].Close != 100 {
		t.Error("adjusting rewrote the caller's bars")
	}
}

func TestRunStrategiesMatchesRunningEachAlone(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	var closes []float64
	for i := 0; i < 400; i++ {
		closes = append(closes, 100+10*math.Sin(float64(i)/15)+float64(i)/20)
	}
	// One payout inside every window, and one after the shorter run ends —
	// which that run must not be adjusted for.
	ago := func(n int) time.Time { return time.Now().AddDate(0, 0, -n).UTC().Truncate(24 * time.Hour) }
	days := archiveDaily(t, a, "VTI", closes, []quotes.Dividend{{Time: ago(100), Amount: 1}, {Time: ago(30), Amount: 2}})
	archiveDaily(t, a, "GLD", closes[:300], nil)

	from := days[250].Format(time.DateOnly)
	mid := days[330].Format(time.DateOnly)
	cross := func(symbol string, fast int, to string) strategy.Definition {
		return strategy.Definition{Symbol: symbol, From: from, To: to, Dividends: true,
			Entry: strategy.Rule{Conditions: []strategy.Condition{{Left: fmt.Sprintf("ema:%d", fast), Op: "crosses_above", Right: "sma:50"}}},
			Exit:  strategy.Rule{Conditions: []strategy.Condition{{Left: fmt.Sprintf("ema:%d", fast), Op: "crosses_below", Right: "sma:50"}}}}
	}
	defs := []strategy.Definition{
		cross("VTI", 5, ""), cross("VTI", 20, mid), cross("GLD", 10, ""),
		{Symbol: "VTI"}, // no start: refused on its own, without sinking the rest
		cross("NOPE", 5, ""),
	}
	runs := eng.RunStrategies(defs)
	if len(runs) != len(defs) {
		t.Fatalf("got %d runs for %d definitions", len(runs), len(defs))
	}
	for i, def := range defs[:3] {
		alone, err := eng.RunStrategy(def)
		if err != nil || runs[i].Err != nil {
			t.Fatalf("run %d: alone %v, batched %v", i, err, runs[i].Err)
		}
		if !reflect.DeepEqual(alone, runs[i].Result) {
			t.Errorf("run %d: batched %+v differs from alone %+v — a shared read must cut each plan its own window and adjustment",
				i, runs[i].Result.Strategy, alone.Strategy)
		}
	}
	if runs[1].Result.To.After(days[330]) {
		t.Errorf("the run ending %s went on to %s", mid, runs[1].Result.To)
	}
	if !strategy.IsInvalid(runs[3].Err) {
		t.Errorf("an invalid definition gave %v, want an InvalidError", runs[3].Err)
	}
	if !errors.Is(runs[4].Err, ErrNoBars) {
		t.Errorf("an unknown symbol gave %v, want ErrNoBars", runs[4].Err)
	}
}

func TestRunStudyOverTheArchive(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	def := strategy.StudyDefinition{Symbol: "VTI", From: time.Now().AddDate(0, -6, 0).Format(time.DateOnly),
		Signal: strategy.Rule{Conditions: []strategy.Condition{{Left: "close", Op: "<", Right: "sma:10"}}}, Horizons: []int{3}}
	if _, err := eng.RunStudy(def); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("with no archive: %v, want ErrNoArchive", err)
	}
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	// A saw-tooth: ten days up a point, then a five-point drop. Every drop
	// takes the close under its average, and the next three days rise.
	var closes []float64
	for i := 0; i < 300; i++ {
		p := 100 + float64(i%11)
		if i%11 == 10 {
			p = 95
		}
		closes = append(closes, p)
	}
	archiveDaily(t, a, "VTI", closes, nil)
	st, err := eng.RunStudy(def)
	if err != nil {
		t.Fatal(err)
	}
	if st.Signals == 0 || st.Horizons[0].Count == 0 {
		t.Fatalf("study = %+v, want the drops found", st)
	}
	if st.Horizons[0].Mean <= st.Horizons[0].Baseline.Mean {
		t.Errorf("after a drop the mean was %.2f%%, baseline %.2f%%: want the bounce to show as an edge", st.Horizons[0].Mean, st.Horizons[0].Baseline.Mean)
	}
	if st.From.Before(time.Now().AddDate(0, -6, -1)) {
		t.Errorf("the study started %s, before the window — warm-up bars must not be counted", st.From)
	}
	def.Signal.Conditions[0].Op = "sideways"
	if _, err := eng.RunStudy(def); !strategy.IsInvalid(err) {
		t.Errorf("a bad operator gave %v, want an InvalidError", err)
	}
}

func TestAReadOnlyArchiveIsNotQueuedInto(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	a := newTestArchive(t)
	archiveDaily(t, a, "VTI", make([]float64, 50), nil)
	ro, err := archive.OpenReadOnly(a.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	eng.UseArchive(openArchive{ro})
	def := strategy.Definition{Symbol: "NEWCO", From: time.Now().AddDate(0, -1, 0).Format(time.DateOnly),
		Entry: strategy.Rule{Conditions: []strategy.Condition{{Left: "close", Op: ">", Right: "1"}}}}
	_, err = eng.RunStrategy(def)
	if !errors.Is(err, ErrNoBars) || !strings.Contains(err.Error(), "copy of the archive") {
		t.Fatalf("an unknown symbol in a read-only archive gave %v, want ErrNoBars saying so", err)
	}
	if _, err := a.Lookup("NEWCO"); !errors.Is(err, archive.ErrUnknownSymbol) {
		t.Errorf("NEWCO was added to an archive opened read-only (%v)", err)
	}
	def.Symbol = "VTI"
	if _, err := eng.RunStrategy(def); err != nil {
		t.Errorf("a held symbol in a read-only archive: %v", err)
	}
}
