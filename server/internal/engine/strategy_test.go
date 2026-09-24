package engine

import (
	"errors"
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
