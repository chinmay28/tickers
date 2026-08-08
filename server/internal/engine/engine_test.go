package engine

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// fakeProvider is a deterministic quote source: whatever prices it is given,
// and an error for everything else.
type fakeProvider struct {
	mu     sync.Mutex
	prices map[string]float64
	calls  int
	fail   map[string]bool
	// asked records the symbol list of the most recent Fetch, so a test can
	// assert what one cycle actually went and got.
	asked []string
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Fetch(ctx context.Context, symbols []string) (map[string]quotes.Quote, map[string]error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.asked = append([]string(nil), symbols...)

	out := map[string]quotes.Quote{}
	failures := map[string]error{}
	for _, s := range symbols {
		if f.fail[s] {
			failures[s] = errors.New("provider says no")
			continue
		}
		price, ok := f.prices[s]
		if !ok {
			failures[s] = quotes.ErrNotFound
			continue
		}
		prev := price - 1
		out[s] = quotes.Quote{
			Symbol: s, Price: &price, PreviousClose: &prev,
			Currency: "USD", FetchedAt: time.Now().UTC(),
		}
	}
	return out, failures
}

func (f *fakeProvider) Search(context.Context, string) ([]quotes.Match, error) {
	return nil, quotes.ErrNoSearch
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) lastAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func newTestEngine(t *testing.T, provider quotes.Provider) (*Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/engine.sqlite")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, provider, publish.New(), nil), st
}

func TestRunCycleStoresQuotesAndCounts(t *testing.T) {
	provider := &fakeProvider{
		prices: map[string]float64{"VTI": 300, "GLD": 200, "BTC-USD": 68000},
		fail:   map[string]bool{"STRC": true},
	}
	eng, st := newTestEngine(t, provider)

	run, err := eng.RunCycle(context.Background(), store.TriggerManual)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}

	// The seed watchlist is 7 symbols; 3 price, 4 don't. A failing symbol must
	// not fail the cycle.
	if run.OKCount != 3 {
		t.Errorf("okCount = %d, want 3", run.OKCount)
	}
	if run.ErrorCount != len(store.SeedSymbols)-3 {
		t.Errorf("errorCount = %d, want %d", run.ErrorCount, len(store.SeedSymbols)-3)
	}
	if run.Error != "" {
		t.Errorf("cycle reported an error for per-symbol failures: %q", run.Error)
	}

	stored, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	if len(stored) != len(store.SeedSymbols) {
		t.Fatalf("stored %d quotes, want one per symbol including failures", len(stored))
	}

	var okCount, errCount int
	for _, q := range stored {
		switch q.Status {
		case store.StatusOK:
			okCount++
		case store.StatusError:
			errCount++
			if q.Error == "" {
				t.Errorf("%s stored as an error with no reason", q.Symbol)
			}
		}
	}
	if okCount != 3 || errCount != 4 {
		t.Errorf("stored %d ok / %d error, want 3 / 4", okCount, errCount)
	}
}

func TestRunCycleIsRecordedAndSurfacedAsStatus(t *testing.T) {
	eng, st := newTestEngine(t, &fakeProvider{prices: map[string]float64{"VTI": 300}})

	if status := eng.Status(); status.LastRun != nil {
		t.Fatal("a fresh engine reports a last run")
	}
	if _, err := eng.RunCycle(context.Background(), store.TriggerStartup); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	status := eng.Status()
	if status.LastRun == nil || status.LastRun.Trigger != store.TriggerStartup {
		t.Fatalf("status.LastRun = %+v", status.LastRun)
	}
	if status.Running {
		t.Error("engine still reports running after the cycle finished")
	}
	if status.Provider != "fake" {
		t.Errorf("status.Provider = %q", status.Provider)
	}

	runs, err := st.Runs(10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %d (err %v), want 1", len(runs), err)
	}
}

func TestRunCyclePublishesWhenConfigured(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bodies++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	eng, st := newTestEngine(t, &fakeProvider{prices: map[string]float64{"VTI": 300}})
	if _, err := st.CreateSink(store.NewSink{
		Name: "Home", BaseURL: srv.URL, Key: "minion-quotes", TimeoutMS: 5000,
	}); err != nil {
		t.Fatalf("create sink: %v", err)
	}

	run, err := eng.RunCycle(context.Background(), store.TriggerSchedule)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if len(run.Publishes) != 1 || !run.Publishes[0].OK {
		t.Fatalf("publishes = %+v", run.Publishes)
	}

	// Turning publishing off must stop the traffic while still fetching.
	off := false
	if _, err := st.UpdateConfig(store.ConfigPatch{PublishOnRefresh: &off}); err != nil {
		t.Fatalf("update config: %v", err)
	}
	run, err = eng.RunCycle(context.Background(), store.TriggerSchedule)
	if err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if len(run.Publishes) != 0 {
		t.Errorf("published %d times with publishOnRefresh off", len(run.Publishes))
	}
	if run.OKCount == 0 {
		t.Error("disabling publishing also stopped fetching")
	}

	mu.Lock()
	defer mu.Unlock()
	if bodies != 1 {
		t.Errorf("sink received %d requests, want 1", bodies)
	}
}

func TestRunCycleSkipsDisabledTickers(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}}
	eng, st := newTestEngine(t, provider)

	tickers, _ := st.Tickers()
	disabled := false
	for _, ticker := range tickers {
		if ticker.Symbol != "VTI" {
			if _, err := st.UpdateTicker(ticker.ID, store.TickerPatch{Enabled: &disabled}); err != nil {
				t.Fatalf("disable %s: %v", ticker.Symbol, err)
			}
		}
	}

	run, err := eng.RunCycle(context.Background(), store.TriggerManual)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if run.OKCount != 1 || run.ErrorCount != 0 {
		t.Fatalf("run = %d ok / %d error, want only the enabled VTI", run.OKCount, run.ErrorCount)
	}

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Quotes) != 1 || snap.Quotes[0].Symbol != "VTI" {
		t.Errorf("snapshot includes disabled tickers: %+v", snap.Quotes)
	}
}

func TestSnapshotIncludesUnfetchedTickersAsErrors(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})

	// Nothing has run yet, so nothing has a stored quote. A consumer watching
	// for a key is better served by a key that says "unknown" than by a key
	// that intermittently isn't there.
	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Quotes) != len(store.SeedSymbols) {
		t.Fatalf("snapshot has %d entries, want %d", len(snap.Quotes), len(store.SeedSymbols))
	}
	for _, q := range snap.Quotes {
		if q.Status != store.StatusError {
			t.Errorf("%s: status %q, want error before the first fetch", q.Symbol, q.Status)
		}
	}

	// And the published payload renders them as the legacy "N/A".
	payload := publish.Payload(snap, store.FormatMinion)
	if payload["VTI"] != "N/A" {
		t.Errorf("payload[VTI] = %v, want N/A", payload["VTI"])
	}
}

func TestSnapshotFollowsDisplayOrder(t *testing.T) {
	eng, st := newTestEngine(t, &fakeProvider{})

	tickers, _ := st.Tickers()
	reversed := make([]string, 0, len(tickers))
	for i := len(tickers) - 1; i >= 0; i-- {
		reversed = append(reversed, tickers[i].ID)
	}
	if err := st.ReorderTickers(reversed); err != nil {
		t.Fatalf("reorder: %v", err)
	}

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for i, q := range snap.Quotes {
		want := store.SeedSymbols[len(store.SeedSymbols)-1-i]
		if q.Symbol != want {
			t.Fatalf("snapshot[%d] = %s, want %s — the watchlist order is the payload order", i, q.Symbol, want)
		}
	}
}

func TestStartRunsImmediatelyAndStopsOnCancel(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300}}
	eng, st := newTestEngine(t, provider)

	// A long interval, so the only cycle we can observe is the startup one.
	seconds := 3600
	if _, err := st.UpdateConfig(store.ConfigPatch{RefreshSeconds: &seconds}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		eng.Start(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for provider.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("Start did not run a cycle at startup")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if status := eng.Status(); status.NextRun == nil && !status.Running {
		// nextRun is set after the startup cycle; give the loop a moment.
		time.Sleep(50 * time.Millisecond)
		if eng.Status().NextRun == nil {
			t.Error("the loop never scheduled a next run")
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}

func TestKickNeverBlocks(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	// Nothing is reading the channel; several nudges must still return.
	for range 10 {
		eng.Kick()
	}
}

func TestConcurrentCyclesAreSerialised(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300}}
	eng, _ := newTestEngine(t, provider)

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
				t.Errorf("cycle: %v", err)
			}
		}()
	}
	wg.Wait()

	// Five cycles, five provider calls — none lost, none doubled, and no data
	// race (this test is worth running under -race).
	if provider.callCount() != 5 {
		t.Errorf("provider called %d times, want 5", provider.callCount())
	}
}

// configurableProvider records what settings it was handed, so a test can
// assert that the engine actually pushes stored configuration down.
type configurableProvider struct {
	fakeProvider
	mu      sync.Mutex
	applied []quotes.Settings
}

func (c *configurableProvider) Apply(s quotes.Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = append(c.applied, s)
}

func (c *configurableProvider) Effective() quotes.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) == 0 {
		return quotes.Settings{}
	}
	return c.applied[len(c.applied)-1]
}

func (c *configurableProvider) last() (quotes.Settings, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.applied) == 0 {
		return quotes.Settings{}, false
	}
	return c.applied[len(c.applied)-1], true
}

func TestRunCyclePushesQuoteSettingsToTheProvider(t *testing.T) {
	provider := &configurableProvider{fakeProvider: fakeProvider{prices: map[string]float64{"VTI": 300}}}
	eng, st := newTestEngine(t, provider)

	url := "https://mirror.example.com"
	ua := "Tickers/test"
	timeout := 42
	if _, err := st.UpdateConfig(store.ConfigPatch{
		QuoteBaseURL: &url, QuoteUserAgent: &ua, QuoteTimeoutSeconds: &timeout,
	}); err != nil {
		t.Fatalf("update config: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	// The point of the whole feature: settings edited in the GUI are in force
	// for the very next cycle, with no restart.
	got, ok := provider.last()
	if !ok {
		t.Fatal("the cycle never configured the provider")
	}
	if got.BaseURL != url || got.UserAgent != ua || got.Timeout != 42*time.Second {
		t.Errorf("applied %+v, want the stored settings", got)
	}
}

func TestProviderSettingsReportsUnconfigurableProviders(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	if _, ok := eng.ProviderSettings(); ok {
		t.Error("a plain provider reported itself configurable")
	}
	// ApplyConfig must be a no-op rather than a panic for those.
	eng.ApplyConfig(store.DefaultConfig())
}

func TestCheckProviderReportsBothOutcomes(t *testing.T) {
	provider := &configurableProvider{fakeProvider: fakeProvider{
		prices: map[string]float64{"VTI": 300},
		fail:   map[string]bool{"BROKEN": true},
	}}
	eng, _ := newTestEngine(t, provider)

	q, err := eng.CheckProvider(context.Background(), " vti ")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if q.Symbol != "VTI" || q.Price == nil || *q.Price != 300 {
		t.Errorf("quote = %+v", q)
	}

	if _, err := eng.CheckProvider(context.Background(), "BROKEN"); err == nil {
		t.Error("a failing symbol reported success")
	}
	// An empty symbol falls back to a known-good default rather than erroring.
	if _, err := eng.CheckProvider(context.Background(), ""); err != nil {
		t.Errorf("empty symbol: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Composites
// ---------------------------------------------------------------------------

// clearWatchlist empties the seeded watchlist, so a composite test asserts on
// exactly the rows it created.
func clearWatchlist(t *testing.T, st *store.Store) {
	t.Helper()
	existing, err := st.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	for _, ticker := range existing {
		if err := st.DeleteTicker(ticker.ID); err != nil {
			t.Fatalf("delete %s: %v", ticker.Symbol, err)
		}
	}
}

func TestRunCyclePricesACompositeFromItsLegs(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}}
	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)

	ratio, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}

	run, err := eng.RunCycle(context.Background(), store.TriggerManual)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if run.OKCount != 1 || run.ErrorCount != 0 {
		t.Fatalf("run counted %d ok / %d failed, want 1 / 0", run.OKCount, run.ErrorCount)
	}

	stored, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q, ok := stored[ratio.ID]
	if !ok {
		t.Fatal("the composite has no stored quote")
	}
	if q.Status != store.StatusOK || q.Price == nil {
		t.Fatalf("composite quote = %+v", q)
	}
	if *q.Price != 1.5 {
		t.Errorf("VTI/GLD priced at %v, want 1.5", *q.Price)
	}
	// The fake's previous close is a dollar under the price, so the ratio's
	// previous close is 299/199 — which is what gives a composite a change.
	if q.PreviousClose == nil {
		t.Fatal("composite has no previous close, so it can show no change")
	}
	if want := 299.0 / 199.0; math.Abs(*q.PreviousClose-want) > 1e-9 {
		t.Errorf("previous close %v, want %v", *q.PreviousClose, want)
	}
	if _, ok := q.ChangePercent(); !ok {
		t.Error("composite reports no change percentage")
	}
	// A ratio is dimensionless; stamping it USD would be a lie the payload
	// would then carry downstream.
	if q.Currency != "" {
		t.Errorf("composite carries currency %q, want none", q.Currency)
	}
}

// A composite's legs are fetched whether or not they are on the watchlist, and
// a leg that is on it is not fetched twice.
func TestCompositeLegsAreFetchedOnceAndNeedNoRow(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}}
	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)

	if _, err := st.CreateTicker(store.NewTicker{Symbol: "VTI"}); err != nil {
		t.Fatalf("create VTI: %v", err)
	}
	if _, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"}); err != nil {
		t.Fatalf("create composite: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	asked := provider.lastAsked()
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"GLD", "VTI"}) {
		t.Errorf("cycle fetched %v, want [GLD VTI] — GLD is a leg only, VTI is shared", asked)
	}

	tickers, err := st.Tickers()
	if err != nil {
		t.Fatalf("tickers: %v", err)
	}
	if len(tickers) != 2 {
		t.Errorf("watchlist has %d rows, want 2 — a leg must not become a row", len(tickers))
	}
}

func TestCompositeReportsTheLegThatFailed(t *testing.T) {
	provider := &fakeProvider{
		prices: map[string]float64{"VTI": 300},
		fail:   map[string]bool{"GLD": true},
	}
	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)

	ratio, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"})
	if err != nil {
		t.Fatalf("create composite: %v", err)
	}
	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	stored, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q := stored[ratio.ID]
	if q.Status != store.StatusError {
		t.Fatalf("composite with an unpriceable leg is %q", q.Status)
	}
	if !strings.Contains(q.Error, "GLD") {
		t.Errorf("error %q does not name the leg that failed", q.Error)
	}
	if !strings.Contains(q.Error, "provider says no") {
		t.Errorf("error %q drops the provider's own reason", q.Error)
	}
}

// A composite stores history under its own symbol like anything else, which is
// the whole of what the sparkline needs.
func TestCompositeAccumulatesHistory(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}}
	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)

	if _, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"}); err != nil {
		t.Fatalf("create composite: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	points, err := st.History("VTI/GLD", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("composite history has %d points, want 2", len(points))
	}
	for _, p := range points {
		if p.Price != 1.5 {
			t.Errorf("history point %v, want 1.5", p.Price)
		}
	}
}

// The composite reaches the published payload as an ordinary key, which is the
// point of computing it into a quote rather than into a special case.
func TestCompositeAppearsInTheSnapshot(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}}
	eng, st := newTestEngine(t, provider)
	clearWatchlist(t, st)

	if _, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"}); err != nil {
		t.Fatalf("create composite: %v", err)
	}
	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	snap, err := eng.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// Four decimals rather than the legacy two: the snapshot's join stamps the
	// quote as composite, and a ratio needs the places to say anything.
	payload := publish.Payload(snap, store.FormatMinion)
	if got := payload["VTI/GLD"]; got != "1.5000" {
		t.Errorf("payload[VTI/GLD] = %v, want \"1.5000\"", got)
	}
}

func TestPortfolioRowIsPricedFromItsUnits(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "BND": 70}}
	eng, st := newTestEngine(t, provider)

	p, err := st.CreatePortfolio(store.NewPortfolio{
		Name:          "Two fund",
		Holdings:      []store.Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "BND", Weight: 40}},
		InitialAmount: 10000,
	})
	if err != nil {
		t.Fatalf("create portfolio: %v", err)
	}
	row, err := eng.LinkPortfolio(context.Background(), p)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if row.Symbol != "TWO-FUND" {
		t.Fatalf("row symbol = %q, want TWO-FUND", row.Symbol)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	quotes, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q, ok := quotes[row.ID]
	if !ok || q.Status != store.StatusOK {
		t.Fatalf("portfolio row = %+v, want a priced reading", q)
	}
	// Units were fixed at the prices it was linked at, so marking them to the
	// same prices gives back exactly the initial amount.
	if math.Abs(*q.Price-10000) > 1e-6 {
		t.Errorf("value = %v, want 10000", *q.Price)
	}
	// The fake reports a previous close one below each price, so the row's own
	// previous close is the same units against those — which is what gives the
	// watchlist a real value-weighted change for the day.
	want := 10000.0/300*0.6*299 + 10000.0/70*0.4*69
	if q.PreviousClose == nil || math.Abs(*q.PreviousClose-want) > 1e-6 {
		t.Errorf("previous close = %v, want %v", deref(q.PreviousClose), want)
	}
}

func TestPortfolioRowFailsWholeRatherThanPartly(t *testing.T) {
	// Three quarters of a portfolio is not what it is worth; it is worth an
	// unknown amount, and publishing the three-quarter figure is worse than
	// publishing nothing.
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "BND": 70}}
	eng, st := newTestEngine(t, provider)

	p, err := st.CreatePortfolio(store.NewPortfolio{
		Name:          "Two fund",
		Holdings:      []store.Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "BND", Weight: 40}},
		InitialAmount: 10000,
	})
	if err != nil {
		t.Fatalf("create portfolio: %v", err)
	}
	row, err := eng.LinkPortfolio(context.Background(), p)
	if err != nil {
		t.Fatalf("link: %v", err)
	}

	provider.fail = map[string]bool{"BND": true}
	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	quotes, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q := quotes[row.ID]
	if q.Status != store.StatusError {
		t.Fatalf("row priced at %v with a holding missing", q.Price)
	}
	if !strings.Contains(q.Error, "BND") {
		t.Errorf("error %q does not name the holding that failed", q.Error)
	}
}

func TestPortfolioLegsAreFetchedOnceAlongsideTheWatchlist(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}}
	eng, st := newTestEngine(t, provider)

	// VTI is already on the seeded watchlist, so a portfolio holding it must
	// not cost a second request — the same deduplication composites get.
	p, err := st.CreatePortfolio(store.NewPortfolio{
		Name:          "Half half",
		Holdings:      []store.Holding{{Symbol: "VTI", Weight: 50}, {Symbol: "GLD", Weight: 50}},
		InitialAmount: 1000,
	})
	if err != nil {
		t.Fatalf("create portfolio: %v", err)
	}
	if _, err := eng.LinkPortfolio(context.Background(), p); err != nil {
		t.Fatalf("link: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	asked := eng.Provider().(*fakeProvider).lastAsked()
	seen := map[string]int{}
	for _, sym := range asked {
		seen[sym]++
	}
	if seen["VTI"] != 1 {
		t.Errorf("VTI was asked for %d times in one cycle, want 1: %v", seen["VTI"], asked)
	}
	if seen["GLD"] != 1 {
		t.Errorf("GLD (a holding not on the watchlist) was asked for %d times, want 1", seen["GLD"])
	}
}

func TestRelinkingKeepsTheBaselineAndAsksForNothing(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "BND": 70}}
	eng, st := newTestEngine(t, provider)

	p, err := st.CreatePortfolio(store.NewPortfolio{
		Name:          "Two fund",
		Holdings:      []store.Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "BND", Weight: 40}},
		InitialAmount: 10000,
	})
	if err != nil {
		t.Fatalf("create portfolio: %v", err)
	}
	if _, err := eng.LinkPortfolio(context.Background(), p); err != nil {
		t.Fatalf("link: %v", err)
	}
	priced, err := st.Portfolio(p.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	before := provider.callCount()

	// The store has already decided the units survive this edit. Linking again
	// must then leave them alone *and* not go upstream: there is nothing to
	// price, and a rename that quietly re-based the row would throw away
	// whatever it had grown by.
	renamed := "Renamed"
	updated, err := st.UpdatePortfolio(p.ID, store.PortfolioPatch{Name: &renamed})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := eng.LinkPortfolio(context.Background(), updated); err != nil {
		t.Fatalf("relink: %v", err)
	}

	if provider.callCount() != before {
		t.Errorf("relinking made %d extra fetches; nothing needed pricing",
			provider.callCount()-before)
	}
	after, err := st.Portfolio(p.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for i, h := range after.Holdings {
		if h.Units != priced.Holdings[i].Units {
			t.Errorf("%s units moved from %v to %v across a rename",
				h.Symbol, priced.Holdings[i].Units, h.Units)
		}
	}
}

func TestChangingTheAllocationRebasesTheRow(t *testing.T) {
	provider := &fakeProvider{prices: map[string]float64{"VTI": 300, "BND": 70}}
	eng, st := newTestEngine(t, provider)

	p, err := st.CreatePortfolio(store.NewPortfolio{
		Name:          "Two fund",
		Holdings:      []store.Holding{{Symbol: "VTI", Weight: 60}, {Symbol: "BND", Weight: 40}},
		InitialAmount: 10000,
	})
	if err != nil {
		t.Fatalf("create portfolio: %v", err)
	}
	if _, err := eng.LinkPortfolio(context.Background(), p); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Different holdings are genuinely different units, so this one *should*
	// re-base — the row starts again at the initial amount.
	shifted := []store.Holding{{Symbol: "VTI", Weight: 70}, {Symbol: "BND", Weight: 30}}
	updated, err := st.UpdatePortfolio(p.ID, store.PortfolioPatch{Holdings: &shifted})
	if err != nil {
		t.Fatalf("reweight: %v", err)
	}
	row, err := eng.LinkPortfolio(context.Background(), updated)
	if err != nil {
		t.Fatalf("relink: %v", err)
	}

	if _, err := eng.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	quotes, err := st.Quotes()
	if err != nil {
		t.Fatalf("quotes: %v", err)
	}
	q := quotes[row.ID]
	if q.Price == nil || math.Abs(*q.Price-10000) > 1e-6 {
		t.Errorf("value = %v after reweighting, want the initial amount 10000", deref(q.Price))
	}
}
