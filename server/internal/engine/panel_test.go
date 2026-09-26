package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func TestPanelLoadsAUniverseWithItsWarmUp(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	ctx := context.Background()
	from := time.Now().AddDate(0, 0, -30)
	if _, _, err := eng.Panel(ctx, Universe{Symbols: []string{"VTI"}}, from, time.Now(), 10, false); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("with no archive: %v", err)
	}
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	flat := make([]float64, 100)
	for i := range flat {
		flat[i] = 100
	}
	days := archiveDaily(t, a, "VTI", flat, []quotes.Dividend{{Time: time.Now().AddDate(0, 0, -10).UTC().Truncate(24 * time.Hour), Amount: 1}})
	archiveDaily(t, a, "GLD", flat[:50], nil)
	if err := a.SetExcluded("GLD", true); err != nil {
		t.Fatal(err)
	}

	p, info, err := eng.Panel(ctx, Universe{Symbols: []string{"vti", "gld", "NOPE"}, Also: []string{"VTI", "SPY"}}, from, time.Now(), 10, false)
	warnings := info.Warnings
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.Symbols, ",") != "GLD,VTI" {
		t.Errorf("symbols = %v, want GLD (excluded from collection, but its bars are history) and VTI", p.Symbols)
	}
	if first := p.Dates[0]; !first.Before(from.AddDate(0, 0, -10)) {
		t.Errorf("the panel starts %s, want at least ten days of warm-up before %s", first, from)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "NOPE") || !strings.Contains(warnings[0], "SPY") {
		t.Errorf("warnings = %v, want the symbols with no bars named", warnings)
	}
	if strings.Join(info.Extra, ",") != "SPY" {
		t.Errorf("extras = %v, want SPY alone — VTI is in the universe already", info.Extra)
	}

	adj, _, _ := eng.Panel(ctx, Universe{Symbols: []string{"VTI"}}, from, time.Now(), 0, true)
	i := adj.First(days[80])
	if adj.Close[0][i] >= 100 || adj.Close[0][len(adj.Dates)-1] != 100 {
		t.Errorf("adjusted closes %v … %v, want those before the payout scaled down", adj.Close[0][i], adj.Close[0][len(adj.Dates)-1])
	}

	top, info, err := eng.Panel(ctx, Universe{Size: 1}, from, time.Now(), 0, false)
	if err != nil || len(top.Symbols) != 1 || !strings.Contains(strings.Join(info.Warnings, " "), "most-traded") {
		t.Errorf("a universe by size = %v %v %v, want one symbol and a note on how it was chosen", top, info, err)
	}
	if _, _, err := eng.Panel(ctx, Universe{Symbols: []string{"NOPE"}}, from, time.Now(), 0, false); !errors.Is(err, ErrNoBars) {
		t.Errorf("a universe with no bars gave %v, want ErrNoBars", err)
	}
}
