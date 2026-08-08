package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
	// conditional records the validators each call was made with, so a test can
	// assert that a re-check actually asked "has it changed?".
	conditional []quotes.LogoValidators
	// unchanged makes the source answer a conditional request with "no".
	unchanged map[string]bool
}

func (i *iconProvider) Logo(_ context.Context, symbol string, known quotes.LogoValidators) (quotes.Logo, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.asked = append(i.asked, symbol)
	i.conditional = append(i.conditional, known)

	if i.broken[symbol] {
		return quotes.Logo{}, errors.New("the logo host is having a day")
	}
	if i.unchanged[symbol] && (known.ETag != "" || known.LastModified != "") {
		return quotes.Logo{}, quotes.ErrLogoUnchanged
	}
	img, ok := i.images[symbol]
	if !ok {
		return quotes.Logo{}, quotes.ErrNoLogo
	}
	return quotes.Logo{
		ContentType: "image/png", Bytes: img, Source: "https://example.test/" + symbol,
		Validators: quotes.LogoValidators{ETag: `"v1-` + symbol + `"`},
	}, nil
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

	asked, err := st.SettledLogos(time.Now().Add(-time.Hour))
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
	asked, err := st.SettledLogos(time.Now().Add(-time.Hour))
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

func TestAnUploadedLogoIsNeverFetchedOver(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		images:       map[string][]byte{"VTI": []byte("from upstream")},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	mine := []byte("my own picture")
	if err := st.SaveLogo(store.Logo{
		Symbol: "VTI", Status: store.LogoOK, Origin: store.LogoCustom,
		ContentType: "image/png", Bytes: mine,
		// Old enough that a fetched one would be long overdue a refresh.
		FetchedAt: time.Now().Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("save upload: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	for _, symbol := range provider.logoCalls() {
		if symbol == "VTI" {
			t.Fatal("the cycle asked upstream about a symbol whose logo was uploaded")
		}
	}
	got, err := st.Logo("VTI")
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}
	if string(got.Bytes) != string(mine) {
		t.Error("an uploaded image was replaced by whatever the source had")
	}
}

func TestStaleAnswersAreAskedAgain(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		images:       map[string][]byte{"VTI": {0x89, 'P', 'N', 'G'}},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	// A "no" from before the source was configured properly, older than the TTL.
	if err := st.SaveLogo(store.Logo{
		Symbol: "VTI", Status: store.LogoNone, Reason: "rejected the request (HTTP 401)",
		FetchedAt: time.Now().Add(-2 * logoTTL),
	}); err != nil {
		t.Fatalf("save tombstone: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := st.Logo("VTI"); err != nil {
		t.Errorf("a day-old 'no' was never revisited, so a fixed setting could not take "+
			"effect without clearing the cache by hand: %v", err)
	}
}

func TestFreshAnswersAreLeftAlone(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		images:       map[string][]byte{"VTI": {0x89, 'P', 'N', 'G'}},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	if err := st.SaveLogo(store.Logo{
		Symbol: "VTI", Status: store.LogoNone, Reason: "no logo here",
		FetchedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("save tombstone: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	for _, symbol := range provider.logoCalls() {
		if symbol == "VTI" {
			t.Error("an hour-old answer was asked again; logos change about never and the " +
				"whole point of caching them is not to re-ask every cycle")
		}
	}
}

func TestADailyRecheckCostsNothingWhenNothingChanged(t *testing.T) {
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		images:       map[string][]byte{"VTI": []byte("the original image")},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	first, err := st.Logo("VTI")
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}
	if first.ETag == "" {
		t.Fatal("the validator was not stored, so tomorrow's check cannot be conditional")
	}

	// Age it past the TTL and let the source say it hasn't moved.
	if err := st.TouchLogo("VTI", time.Now().Add(-2*logoTTL)); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	provider.mu.Lock()
	provider.unchanged = map[string]bool{"VTI": true}
	provider.mu.Unlock()

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("second cycle: %v", err)
	}

	// It asked, and it asked conditionally.
	var asked bool
	provider.mu.Lock()
	for i, symbol := range provider.asked {
		if symbol == "VTI" && provider.conditional[i].ETag == first.ETag {
			asked = true
		}
	}
	provider.mu.Unlock()
	if !asked {
		t.Error("the re-check did not send the stored validator, so the source could not " +
			"answer 'unchanged' and the image was downloaded again")
	}

	after, err := st.Logo("VTI")
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}
	if string(after.Bytes) != string(first.Bytes) {
		t.Error("an unchanged logo was rewritten")
	}
	if after.FetchedAt.Before(time.Now().Add(-time.Minute)) {
		t.Errorf("fetched_at is %v; an unchanged answer still has to reset the clock, or the "+
			"symbol is re-asked on every cycle from now on", after.FetchedAt)
	}
	if !after.UpdatedAt.Equal(first.UpdatedAt) {
		t.Error("a 304 moved the image's version, so every browser re-downloads a picture " +
			"that never changed")
	}
}

func TestAnIdenticalImageIsNotRewritten(t *testing.T) {
	// A source with no ETag and no Last-Modified: the only way to know whether
	// anything changed is to look at what came back.
	provider := &iconProvider{
		fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}},
		images:       map[string][]byte{"VTI": []byte("byte for byte the same")},
	}
	eng, st := newTestEngine(t, provider)
	enableLogos(t, st)

	if err := st.SaveLogo(store.Logo{
		Symbol: "VTI", Status: store.LogoOK, ContentType: "image/png",
		Bytes:     []byte("byte for byte the same"),
		FetchedAt: time.Now().Add(-2 * logoTTL),
	}); err != nil {
		t.Fatalf("seed the cache: %v", err)
	}
	before, _ := st.Logo("VTI")

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	after, err := st.Logo("VTI")
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}
	if !after.FetchedAt.After(before.FetchedAt) {
		t.Error("the row was not touched, so it stays stale and is re-asked on every cycle")
	}
	// UpdatedAt is what the client puts in the image URL. Moving it for an
	// identical picture would make every browser re-download it once a day.
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("the version moved from %v to %v for a byte-identical image",
			before.UpdatedAt, after.UpdatedAt)
	}
}
