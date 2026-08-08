package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// iconProvider is a fakeProvider that also draws logos: an image for the
// symbols it has one for, ErrNoLogo for the ones it doesn't, and a plain
// failure for the ones set to break.
type iconProvider struct {
	fakeProvider
	mu     sync.Mutex
	images map[string][]byte
	broken map[string]bool
	asked  []string
}

func (i *iconProvider) Logo(_ context.Context, symbol string) (quotes.Logo, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.asked = append(i.asked, symbol)

	if i.broken[symbol] {
		return quotes.Logo{}, errors.New("the logo host is having a day")
	}
	img, ok := i.images[symbol]
	if !ok {
		return quotes.Logo{}, quotes.ErrNoLogo
	}
	return quotes.Logo{ContentType: "image/png", Bytes: img, Source: "https://example.test/" + symbol}, nil
}

func (i *iconProvider) logoCalls() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.asked...)
}

func enableLogos(t *testing.T, st *store.Store) {
	t.Helper()
	on := true
	if _, err := st.UpdateConfig(store.ConfigPatch{Logos: &on}); err != nil {
		t.Fatalf("enable logos: %v", err)
	}
}

func TestCycleCachesLogosAndTombstones(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}},
		images:       map[string][]byte{"VTI": {0x89, 'P', 'N', 'G'}},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	if _, err := st.Logo("VTI"); err != nil {
		t.Errorf("VTI's logo was not cached: %v", err)
	}
	if _, err := st.Logo("GLD"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GLD has no logo upstream but the cache serves one: %v", err)
	}

	asked, err := st.AskedAboutLogos()
	if err != nil {
		t.Fatalf("asked set: %v", err)
	}
	if !asked["GLD"] {
		t.Error("a symbol with no logo was not recorded as asked, so it will be re-asked every cycle forever")
	}

	// A second cycle picks up where the first left off — the seed watchlist is
	// longer than one cycle's cap — but it does not go back over the symbols
	// already answered for. That is the whole reason "none" is stored.
	before := len(provider.logoCalls())
	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	for _, symbol := range provider.logoCalls()[before:] {
		if symbol == "VTI" || symbol == "GLD" {
			t.Errorf("%s was asked about again after being answered for", symbol)
		}
	}
}

func TestLogoFailuresAreRetried(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		broken:       map[string]bool{"VTI": true},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	asked, err := st.AskedAboutLogos()
	if err != nil {
		t.Fatalf("asked set: %v", err)
	}
	if asked["VTI"] {
		t.Error("a timeout was recorded as 'this symbol has no logo'; a network blink must not be a permanent answer")
	}

	// And because nothing was recorded, the next cycle tries again.
	provider.mu.Lock()
	provider.broken = nil
	provider.images = map[string][]byte{"VTI": {0x89, 'P', 'N', 'G'}}
	provider.mu.Unlock()

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if _, err := st.Logo("VTI"); err != nil {
		t.Errorf("the retry after a transient failure never happened: %v", err)
	}
}

func TestLogosAreNotFetchedUntilAskedFor(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		images:       map[string][]byte{"VTI": {0x89, 'P', 'N', 'G'}},
	}
	eng, _ := newTestEngine(t, provider)

	// No enableLogos: the setting is off, which is how a fresh install runs.
	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if calls := provider.logoCalls(); len(calls) != 0 {
		t.Errorf("a cycle asked about %v with the setting off; nothing should reach a logo host uninvited", calls)
	}
}

func TestOneCycleOnlyFetchesSoManyLogos(t *testing.T) {
	prices := map[string]float64{}
	for _, s := range store.SeedSymbols {
		prices[s] = 100
	}
	provider := &iconProvider{fakeProvider: fakeProvider{prices: prices}}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	if len(store.SeedSymbols) <= maxLogosPerCycle {
		t.Skipf("the seed watchlist (%d) no longer exceeds the per-cycle cap (%d)",
			len(store.SeedSymbols), maxLogosPerCycle)
	}
	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if got := len(provider.logoCalls()); got != maxLogosPerCycle {
		t.Errorf("one cycle went and got %d logos, want the cap of %d — a first run "+
			"must not fire a request per symbol at a host that owes us nothing", got, maxLogosPerCycle)
	}
}

func TestProviderWithoutLogosIsFine(t *testing.T) {
	// The plain fakeProvider is not an Iconographer, which is the case for any
	// provider that can only price.
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300}}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	run, err := eng.RunCycle(context.Background(), store.TriggerManual)
	if err != nil {
		t.Fatalf("a provider that cannot draw logos broke the cycle: %v", err)
	}
	if run.Error != "" {
		t.Errorf("cycle reported %q; a missing optional capability is not a failure", run.Error)
	}
}
