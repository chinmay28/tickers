package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Fetch(ctx context.Context, symbols []string) (map[string]quotes.Quote, map[string]error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

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
