package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/web"
)

// stubProvider prices everything it knows and refuses everything else.
type stubProvider struct {
	prices     map[string]float64
	searchErr  error
	searchHits []quotes.Match
}

func (s stubProvider) Name() string { return "stub" }

func (s stubProvider) Fetch(_ context.Context, symbols []string) (map[string]quotes.Quote, map[string]error) {
	out := map[string]quotes.Quote{}
	failures := map[string]error{}
	for _, sym := range symbols {
		if price, ok := s.prices[sym]; ok {
			p, prev := price, price-2
			out[sym] = quotes.Quote{Symbol: sym, Price: &p, PreviousClose: &prev,
				Currency: "USD", FetchedAt: time.Now().UTC()}
		} else {
			failures[sym] = quotes.ErrNotFound
		}
	}
	return out, failures
}

func (s stubProvider) Search(context.Context, string) ([]quotes.Match, error) {
	return s.searchHits, s.searchErr
}

type harness struct {
	handler http.Handler
	store   *store.Store
	engine  *engine.Engine
}

func newHarness(t *testing.T, provider quotes.Provider) *harness {
	t.Helper()
	return newHarnessWith(t, provider, Runtime{})
}

func newHarnessWith(t *testing.T, provider quotes.Provider, runtime Runtime) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/api.sqlite")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	eng := engine.New(st, provider, publish.New(), nil)
	webHandler, err := web.Handler("")
	if err != nil {
		t.Fatalf("web handler: %v", err)
	}
	srv := New(Options{Store: st, Engine: eng, Web: webHandler, Runtime: runtime})
	return &harness{handler: srv.Handler(), store: st, engine: eng}
}

// do performs a request and decodes the JSON body (if any).
func (h *harness) do(t *testing.T, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		blob, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(blob)
	} else {
		reader = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s: decode body %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec, decoded
}

// raw sends a body that is not necessarily valid JSON.
func (h *harness) raw(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func TestHealth(t *testing.T) {
	h := newHarness(t, stubProvider{})
	rec, body := h.do(t, http.MethodGet, "/api/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v", body["status"])
	}
	if version, _ := body["version"].(string); !strings.HasPrefix(version, "v") {
		t.Errorf("version = %v, want a v-prefixed string", body["version"])
	}
	// The applied migration list is what an operator reads to see which schema
	// a running instance is on.
	if migrations, ok := body["migrations"].([]any); !ok || len(migrations) == 0 {
		t.Errorf("migrations = %v, want the applied list", body["migrations"])
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("API responses must not be cached; got %q", rec.Header().Get("Cache-Control"))
	}
}

func TestStateBundlesEverythingTheClientNeeds(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})
	if _, err := h.engine.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	rec, body := h.do(t, http.MethodGet, "/api/state", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	tickers, ok := body["tickers"].([]any)
	if !ok || len(tickers) != len(store.SeedSymbols) {
		t.Fatalf("tickers = %v", body["tickers"])
	}

	first := tickers[0].(map[string]any)
	if first["symbol"] != "VTI" {
		t.Fatalf("first ticker is %v, want VTI", first["symbol"])
	}
	if first["pinned"] != true {
		t.Error("seeded tickers must be flagged as pinned so the UI can chip them")
	}
	// The change is computed server-side so the client doesn't re-derive it.
	if change, ok := first["change"].(float64); !ok || change != 2 {
		t.Errorf("change = %v, want 2", first["change"])
	}

	// The preview is the exact payload a destination would receive.
	preview, ok := body["preview"].(map[string]any)
	if !ok || preview["VTI"] != "300.00" || preview["timestamp"] == nil {
		t.Errorf("preview = %v", body["preview"])
	}

	for _, key := range []string{"settings", "engine", "meta", "sinks", "version"} {
		if _, ok := body[key]; !ok {
			t.Errorf("state is missing %q", key)
		}
	}
}

func TestCreateTickerPricesItImmediately(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"AAPL": 150}})

	rec, body := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"symbol": " aapl ", "label": "Fruit"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %v", rec.Code, body)
	}
	ticker := body["ticker"].(map[string]any)
	if ticker["symbol"] != "AAPL" {
		t.Errorf("symbol = %v, want the normalised AAPL", ticker["symbol"])
	}

	// Adding a symbol runs a cycle, so it must already have a price rather
	// than sitting blank until the next scheduled poll.
	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	for _, raw := range state["tickers"].([]any) {
		view := raw.(map[string]any)
		if view["symbol"] != "AAPL" {
			continue
		}
		quote, ok := view["quote"].(map[string]any)
		if !ok || quote["price"] != 150.0 {
			t.Fatalf("AAPL was not priced on creation: %v", view["quote"])
		}
		return
	}
	t.Fatal("AAPL is not on the watchlist")
}

func TestCreateTickerRejectsDuplicatesAndBlanks(t *testing.T) {
	h := newHarness(t, stubProvider{})

	if rec, _ := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"symbol": "vti"}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate returned %d, want 409", rec.Code)
	}
	if rec, _ := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"symbol": "  "}); rec.Code != http.StatusBadRequest {
		t.Errorf("blank symbol returned %d, want 400", rec.Code)
	}
	if rec, _ := h.do(t, http.MethodPost, "/api/tickers", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("missing symbol returned %d, want 400", rec.Code)
	}
	// A typo'd field name must be an error, not a silently empty ticker.
	if rec := h.raw(t, http.MethodPost, "/api/tickers", `{"symbl":"VTI"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field returned %d, want 400", rec.Code)
	}
	if rec := h.raw(t, http.MethodPost, "/api/tickers", `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON returned %d, want 400", rec.Code)
	}
}

func TestReplacingASymbolUnpinsItAndRepricesIt(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"P": 30, "MSFT": 420}})
	if _, err := h.engine.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	var id string
	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	for _, raw := range state["tickers"].([]any) {
		view := raw.(map[string]any)
		if view["symbol"] == "P" {
			id = view["id"].(string)
		}
	}
	if id == "" {
		t.Fatal("seeded P is missing")
	}

	rec, body := h.do(t, http.MethodPatch, "/api/tickers/"+id, map[string]any{"symbol": "msft"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %v", rec.Code, body)
	}
	ticker := body["ticker"].(map[string]any)
	if ticker["symbol"] != "MSFT" || ticker["origin"] != store.OriginUser {
		t.Fatalf("replaced ticker is %v", ticker)
	}

	_, state = h.do(t, http.MethodGet, "/api/state", nil)
	for _, raw := range state["tickers"].([]any) {
		view := raw.(map[string]any)
		if view["id"] != id {
			continue
		}
		// Pins are keyed by symbol, so the replacement is not on the list.
		if view["pinned"] != false {
			t.Error("a replaced symbol is still flagged as pinned")
		}
		// The old price must be gone and the new one already in place.
		quote, ok := view["quote"].(map[string]any)
		if !ok || quote["price"] != 420.0 {
			t.Fatalf("replacement was not repriced: %v", view["quote"])
		}
		return
	}
	t.Fatal("the replaced ticker vanished")
}

func TestToggleAndDeleteTicker(t *testing.T) {
	h := newHarness(t, stubProvider{})
	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	id := state["tickers"].([]any)[0].(map[string]any)["id"].(string)

	rec, body := h.do(t, http.MethodPatch, "/api/tickers/"+id, map[string]any{"enabled": false})
	if rec.Code != http.StatusOK || body["ticker"].(map[string]any)["enabled"] != false {
		t.Fatalf("pause returned %d / %v", rec.Code, body)
	}

	if rec, _ := h.do(t, http.MethodDelete, "/api/tickers/"+id, nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete returned %d, want 204", rec.Code)
	}
	if rec, _ := h.do(t, http.MethodDelete, "/api/tickers/"+id, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice returned %d, want 404", rec.Code)
	}
	if rec, _ := h.do(t, http.MethodPatch, "/api/tickers/nope", map[string]any{"enabled": true}); rec.Code != http.StatusNotFound {
		t.Errorf("patching an unknown id returned %d, want 404", rec.Code)
	}
}

func TestReorderTickers(t *testing.T) {
	h := newHarness(t, stubProvider{})
	_, state := h.do(t, http.MethodGet, "/api/state", nil)

	views := state["tickers"].([]any)
	ids := make([]string, 0, len(views))
	for i := len(views) - 1; i >= 0; i-- {
		ids = append(ids, views[i].(map[string]any)["id"].(string))
	}

	rec, body := h.do(t, http.MethodPost, "/api/tickers/reorder", map[string]any{"ids": ids})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %v", rec.Code, body)
	}
	got := body["tickers"].([]any)
	if got[0].(map[string]any)["symbol"] != store.SeedSymbols[len(store.SeedSymbols)-1] {
		t.Errorf("order was not applied: first is %v", got[0].(map[string]any)["symbol"])
	}
}

func TestSinkCRUDAndTest(t *testing.T) {
	var hits int
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()

	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	rec, body := h.do(t, http.MethodPost, "/api/sinks", map[string]any{
		"name": "Home", "baseUrl": downstream.URL, "key": "minion-quotes", "category": "minion",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %v", rec.Code, body)
	}
	id := body["sink"].(map[string]any)["id"].(string)

	// An unusable URL is the caller's mistake, not a 500.
	if rec, _ := h.do(t, http.MethodPost, "/api/sinks", map[string]any{
		"baseUrl": "file:///etc/passwd", "key": "k",
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("a file:// sink returned %d, want 400", rec.Code)
	}

	rec, body = h.do(t, http.MethodPost, "/api/sinks/"+id+"/test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("test returned %d: %v", rec.Code, body)
	}
	result := body["result"].(map[string]any)
	if result["ok"] != true || result["method"] != http.MethodPut {
		t.Errorf("test result = %v", result)
	}
	if hits != 1 {
		t.Errorf("downstream saw %d requests, want 1", hits)
	}

	rec, body = h.do(t, http.MethodPatch, "/api/sinks/"+id, map[string]any{"enabled": false, "format": "detailed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch returned %d: %v", rec.Code, body)
	}
	sink := body["sink"].(map[string]any)
	if sink["enabled"] != false || sink["format"] != "detailed" {
		t.Errorf("patched sink = %v", sink)
	}

	// A disabled sink is skipped by publish but still testable by hand.
	rec, body = h.do(t, http.MethodPost, "/api/publish", nil)
	if rec.Code != http.StatusOK || len(body["results"].([]any)) != 0 {
		t.Errorf("publish hit a disabled sink: %v", body)
	}

	if rec, _ := h.do(t, http.MethodDelete, "/api/sinks/"+id, nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete returned %d", rec.Code)
	}
	if rec, _ := h.do(t, http.MethodPost, "/api/sinks/nope/test", nil); rec.Code != http.StatusNotFound {
		t.Errorf("testing an unknown sink returned %d, want 404", rec.Code)
	}
}

func TestSettingsRoundTripAndFloor(t *testing.T) {
	h := newHarness(t, stubProvider{})

	if rec, _ := h.do(t, http.MethodPatch, "/api/settings", map[string]any{"refreshSeconds": 1}); rec.Code != http.StatusBadRequest {
		t.Errorf("an interval below the floor returned %d, want 400", rec.Code)
	}

	rec, body := h.do(t, http.MethodPatch, "/api/settings", map[string]any{
		"refreshSeconds": 120, "publishOnRefresh": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch returned %d: %v", rec.Code, body)
	}
	settings := body["settings"].(map[string]any)
	if settings["refreshSeconds"] != 120.0 || settings["publishOnRefresh"] != false {
		t.Fatalf("settings = %v", settings)
	}

	_, body = h.do(t, http.MethodGet, "/api/settings", nil)
	if body["settings"].(map[string]any)["refreshSeconds"] != 120.0 {
		t.Errorf("settings did not persist: %v", body["settings"])
	}
	if body["minRefreshSeconds"] != float64(store.MinRefreshSeconds) {
		t.Errorf("minRefreshSeconds = %v", body["minRefreshSeconds"])
	}
}

func TestPinnedSymbolsAreConfiguredInSettingsAndSortTheWatchlist(t *testing.T) {
	h := newHarness(t, stubProvider{})

	// Out of the box the seeded symbols are the pinned list.
	_, body := h.do(t, http.MethodGet, "/api/settings", nil)
	pinned := body["settings"].(map[string]any)["pinnedSymbols"].([]any)
	if len(pinned) != len(store.SeedSymbols) || pinned[0] != store.SeedSymbols[0] {
		t.Fatalf("pinnedSymbols = %v, want the seeded symbols", pinned)
	}

	// Pin one symbol from the back of the watchlist; it must come out first.
	last := store.SeedSymbols[len(store.SeedSymbols)-1]
	rec, body := h.do(t, http.MethodPatch, "/api/settings", map[string]any{
		"pinnedSymbols": []string{strings.ToLower(last)},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch returned %d: %v", rec.Code, body)
	}
	if got := body["settings"].(map[string]any)["pinnedSymbols"].([]any); len(got) != 1 || got[0] != last {
		t.Fatalf("pinnedSymbols = %v, want the normalised [%s]", got, last)
	}

	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	views := state["tickers"].([]any)
	first := views[0].(map[string]any)
	if first["symbol"] != last || first["pinned"] != true {
		t.Fatalf("watchlist starts with %v (pinned %v), want the pinned %s", first["symbol"], first["pinned"], last)
	}
	// Everything else keeps the watchlist's own order behind it.
	for i, want := range store.SeedSymbols[:len(store.SeedSymbols)-1] {
		view := views[i+1].(map[string]any)
		if view["symbol"] != want {
			t.Fatalf("unpinned row %d is %v, want %s", i, view["symbol"], want)
		}
		if view["pinned"] != false {
			t.Errorf("%v is flagged pinned but is not on the list", view["symbol"])
		}
	}

	// The published payload is built from the same order.
	if rec, _ := h.do(t, http.MethodPost, "/api/refresh", nil); rec.Code != http.StatusOK {
		t.Fatalf("refresh returned %d", rec.Code)
	}
	snap, err := h.engine.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Quotes[0].Symbol != last {
		t.Errorf("payload starts with %s, want the pinned %s", snap.Quotes[0].Symbol, last)
	}

	// Over the cap is the caller's mistake, not a 500.
	tooMany := make([]string, store.MaxPinnedSymbols+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("SYM%d", i)
	}
	if rec, _ := h.do(t, http.MethodPatch, "/api/settings", map[string]any{"pinnedSymbols": tooMany}); rec.Code != http.StatusBadRequest {
		t.Errorf("an over-long pinned list returned %d, want 400", rec.Code)
	}
}

func TestPreviewFormats(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})
	if _, err := h.engine.RunCycle(context.Background(), store.TriggerManual); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	_, body := h.do(t, http.MethodGet, "/api/preview", nil)
	if body["format"] != store.FormatMinion {
		t.Errorf("default format = %v, want minion", body["format"])
	}
	if body["payload"].(map[string]any)["VTI"] != "300.00" {
		t.Errorf("minion payload = %v", body["payload"])
	}

	_, body = h.do(t, http.MethodGet, "/api/preview?format=detailed", nil)
	vti := body["payload"].(map[string]any)["VTI"].(map[string]any)
	if vti["price"] != 300.0 || vti["change"] != 2.0 {
		t.Errorf("detailed payload = %v", vti)
	}

	if rec, _ := h.do(t, http.MethodGet, "/api/preview?format=xml", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown format returned %d, want 400", rec.Code)
	}
}

func TestSearchDegradesGracefully(t *testing.T) {
	hits := []quotes.Match{{Symbol: "ORCL", Name: "Oracle Corporation", Exchange: "NYSE", Type: "Equity"}}
	h := newHarness(t, stubProvider{searchHits: hits})

	_, body := h.do(t, http.MethodGet, "/api/search?q=oracle", nil)
	if len(body["matches"].([]any)) != 1 {
		t.Fatalf("matches = %v", body["matches"])
	}

	// A blocked or rate-limited provider must not block adding a ticker: the
	// endpoint stays 200 and says so, and the user types the symbol instead.
	broken := newHarness(t, stubProvider{searchErr: quotes.ErrNoSearch})
	rec, body := broken.do(t, http.MethodGet, "/api/search?q=oracle", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a failing search returned %d, want 200", rec.Code)
	}
	if len(body["matches"].([]any)) != 0 || body["warning"] == nil {
		t.Errorf("expected an empty result with a warning, got %v", body)
	}

	// An empty query short-circuits.
	_, body = h.do(t, http.MethodGet, "/api/search?q=", nil)
	if len(body["matches"].([]any)) != 0 {
		t.Errorf("empty query returned %v", body["matches"])
	}
}

func TestRefreshAndRuns(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}})

	rec, body := h.do(t, http.MethodPost, "/api/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh returned %d: %v", rec.Code, body)
	}
	run := body["run"].(map[string]any)
	if run["okCount"] != 2.0 {
		t.Errorf("okCount = %v, want 2", run["okCount"])
	}
	if run["trigger"] != store.TriggerManual {
		t.Errorf("trigger = %v", run["trigger"])
	}

	_, body = h.do(t, http.MethodGet, "/api/runs?limit=5", nil)
	if len(body["runs"].([]any)) != 1 {
		t.Errorf("runs = %v", body["runs"])
	}
}

func TestHistoryEndpoint(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})
	for range 3 {
		if _, err := h.engine.RunCycle(context.Background(), store.TriggerManual); err != nil {
			t.Fatalf("cycle: %v", err)
		}
	}

	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	id := state["tickers"].([]any)[0].(map[string]any)["id"].(string)

	rec, body := h.do(t, http.MethodGet, "/api/tickers/"+id+"/history?limit=10", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("history returned %d", rec.Code)
	}
	if body["symbol"] != "VTI" || len(body["points"].([]any)) != 3 {
		t.Errorf("history = %v", body)
	}

	if rec, _ := h.do(t, http.MethodGet, "/api/tickers/nope/history", nil); rec.Code != http.StatusNotFound {
		t.Errorf("history for an unknown ticker returned %d, want 404", rec.Code)
	}
}

func TestWebClientIsServedAndDeepLinksFallBackToTheShell(t *testing.T) {
	h := newHarness(t, stubProvider{})

	// "/index.html" is deliberately absent: net/http's file server canonicalises
	// it to "/" with a 301, which is correct behaviour, not a served page.
	for _, path := range []string{"/", "/app.js", "/styles.css", "/icon.svg", "/manifest.webmanifest", "/dev-badge.png"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s returned %d, want 200", path, rec.Code)
		}
	}

	// A deep link renders the app; a genuinely missing asset still 404s, so a
	// broken script tag doesn't silently serve HTML to a <script>.
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Tickers</title>") {
		t.Errorf("deep link returned %d, body starts %q", rec.Code, truncate(rec.Body.String()))
	}

	req = httptest.NewRequest(http.MethodGet, "/missing.js", nil)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing asset returned %d, want 404", rec.Code)
	}

	// The shell and the client code must never be cached across an upgrade.
	req = httptest.NewRequest(http.MethodGet, "/app.js", nil)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("app.js Cache-Control = %q, want no-cache", rec.Header().Get("Cache-Control"))
	}
}

func TestUnknownAPIRoutesAnswerJSONNotTheWebShell(t *testing.T) {
	h := newHarness(t, stubProvider{})

	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/tickers"},  // right path, wrong method
		{http.MethodGet, "/api/nonsense"}, // no such endpoint
	} {
		rec := h.raw(t, tc.method, tc.path, `{}`)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s returned %d, want 404 or 405", tc.method, tc.path, rec.Code)
		}
		// A client that JSON-parses this must not be handed the HTML shell.
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s Content-Type = %q, want JSON", tc.method, tc.path, ct)
		}
	}
}

func truncate(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func TestSettingsExposeAndUpdateTheQuoteSource(t *testing.T) {
	h := newHarness(t, stubProvider{})

	// A stub provider isn't Configurable, so the UI is told to hide the fields.
	_, body := h.do(t, http.MethodGet, "/api/settings", nil)
	if body["provider"] != nil {
		t.Errorf("provider = %v, want null for a non-configurable provider", body["provider"])
	}

	// A real one is, and reports the defaults the form uses as placeholders.
	real := newHarnessWith(t, quotes.NewYahoo(quotes.Settings{}), Runtime{})
	_, body = real.do(t, http.MethodGet, "/api/settings", nil)
	provider, ok := body["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider = %v, want the effective settings", body["provider"])
	}
	if provider["baseUrl"] != quotes.DefaultBaseURL {
		t.Errorf("baseUrl = %v, want the package default", provider["baseUrl"])
	}
	if provider["defaultTimeoutSeconds"] != float64(quotes.DefaultTimeout/time.Second) {
		t.Errorf("defaultTimeoutSeconds = %v", provider["defaultTimeoutSeconds"])
	}

	// Saving an override must be visible in the provider straight away — the
	// response itself has to reflect what the next request will use, or the
	// page shows a value the server isn't honouring.
	rec, body := real.do(t, http.MethodPatch, "/api/settings", map[string]any{
		"quoteBaseUrl": "https://mirror.example.com/", "quoteTimeoutSeconds": 45,
		"quoteUserAgent": "Tickers/test",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch returned %d: %v", rec.Code, body)
	}
	provider = body["provider"].(map[string]any)
	if provider["baseUrl"] != "https://mirror.example.com" {
		t.Errorf("provider baseUrl = %v, want the saved override", provider["baseUrl"])
	}
	if provider["timeoutSeconds"] != 45.0 || provider["userAgent"] != "Tickers/test" {
		t.Errorf("provider = %v", provider)
	}

	// And it survives into /api/state, which is what the page actually renders.
	_, state := real.do(t, http.MethodGet, "/api/state", nil)
	if state["provider"].(map[string]any)["baseUrl"] != "https://mirror.example.com" {
		t.Errorf("state provider = %v", state["provider"])
	}
	if state["settings"].(map[string]any)["quoteBaseUrl"] != "https://mirror.example.com" {
		t.Errorf("state settings = %v", state["settings"])
	}

	// Clearing goes back to the default rather than to an empty base URL.
	_, body = real.do(t, http.MethodPatch, "/api/settings", map[string]any{"quoteBaseUrl": ""})
	if body["provider"].(map[string]any)["baseUrl"] != quotes.DefaultBaseURL {
		t.Errorf("clearing did not restore the default: %v", body["provider"])
	}
}

func TestSettingsRejectAnUnusableQuoteSource(t *testing.T) {
	h := newHarnessWith(t, quotes.NewYahoo(quotes.Settings{}), Runtime{})

	for name, patch := range map[string]map[string]any{
		"file scheme":      {"quoteBaseUrl": "file:///etc/passwd"},
		"no host":          {"quoteBaseUrl": "http://"},
		"timeout low":      {"quoteTimeoutSeconds": 1},
		"timeout high":     {"quoteTimeoutSeconds": store.MaxQuoteTimeout + 1},
		"header injection": {"quoteUserAgent": "curl\r\nX-Evil: 1"},
	} {
		if rec, _ := h.do(t, http.MethodPatch, "/api/settings", patch); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: returned %d, want 400", name, rec.Code)
		}
	}
}

func TestProviderTestReportsSuccessAndFailureAsPayload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"chart":{"result":[{"meta":{"currency":"USD","symbol":"VTI",
		  "shortName":"Vanguard Total Stock Market ETF","regularMarketPrice":301.5,
		  "chartPreviousClose":300},"indicators":{"quote":[{"close":[301.5]}]}}],"error":null}}`))
	}))
	defer upstream.Close()

	h := newHarnessWith(t, quotes.NewYahoo(quotes.Settings{}), Runtime{})
	if rec, body := h.do(t, http.MethodPatch, "/api/settings", map[string]any{
		"quoteBaseUrl": upstream.URL, "quoteTimeoutSeconds": 10,
	}); rec.Code != http.StatusOK {
		t.Fatalf("patch returned %d: %v", rec.Code, body)
	}

	rec, body := h.do(t, http.MethodPost, "/api/provider/test", map[string]any{"symbol": "vti"})
	if rec.Code != http.StatusOK {
		t.Fatalf("test returned %d: %v", rec.Code, body)
	}
	result := body["result"].(map[string]any)
	if result["ok"] != true || result["symbol"] != "VTI" || result["price"] != 301.5 {
		t.Fatalf("result = %v", result)
	}

	// An unreachable source is reported in the payload, not as an HTTP error —
	// a 502 here would just be a second error for the user to interpret.
	if rec, _ := h.do(t, http.MethodPatch, "/api/settings", map[string]any{
		"quoteBaseUrl": "http://127.0.0.1:1", "quoteTimeoutSeconds": 5,
	}); rec.Code != http.StatusOK {
		t.Fatal("could not point the provider at a dead port")
	}
	rec, body = h.do(t, http.MethodPost, "/api/provider/test", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("a failing test returned %d, want 200", rec.Code)
	}
	result = body["result"].(map[string]any)
	if result["ok"] != false || result["error"] == nil {
		t.Errorf("result = %v, want a reported failure", result)
	}
}

func TestStateReportsRuntimeConfiguration(t *testing.T) {
	runtime := Runtime{ListenAddr: "0.0.0.0:8797", DBPath: "/var/lib/tickers/tickers.sqlite", WebSource: "embedded"}
	h := newHarnessWith(t, stubProvider{}, runtime)

	_, body := h.do(t, http.MethodGet, "/api/state", nil)
	got, ok := body["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime = %v", body["runtime"])
	}
	if got["listenAddr"] != runtime.ListenAddr || got["dbPath"] != runtime.DBPath || got["webSource"] != "embedded" {
		t.Errorf("runtime = %v, want %+v", got, runtime)
	}
}

// ---------------------------------------------------------------------------
// Composites
// ---------------------------------------------------------------------------

func TestCreateCompositeTickerPricesItImmediately(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}})

	rec, body := h.do(t, http.MethodPost, "/api/tickers", map[string]any{
		"expression": "vti / gld",
		"label":      "Stocks vs gold",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}
	ticker, _ := body["ticker"].(map[string]any)
	if ticker["symbol"] != "VTI/GLD" || ticker["expression"] != "VTI/GLD" {
		t.Fatalf("created %v", ticker)
	}

	// Adding a ticker runs a cycle, so the row must already carry its value
	// rather than showing "—" until the next poll.
	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	view := findTicker(t, state, "VTI/GLD")
	quote, _ := view["quote"].(map[string]any)
	if quote == nil || quote["status"] != "ok" {
		t.Fatalf("composite quote = %v", quote)
	}
	if price, _ := quote["price"].(float64); price != 1.5 {
		t.Errorf("price = %v, want 1.5", quote["price"])
	}
	// The stub's previous close is two dollars under, so the ratio moved.
	if _, ok := view["changePercent"].(float64); !ok {
		t.Errorf("composite has no change percentage: %v", view)
	}
}

// The symbol field takes a formula too, so a client that only knows about
// symbols can still add a ratio.
func TestCreateTickerAcceptsAFormulaInTheSymbolField(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300, "P": 30}})

	rec, body := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"symbol": "P/VTI"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}
	ticker, _ := body["ticker"].(map[string]any)
	if ticker["expression"] != "P/VTI" {
		t.Errorf("created %v, want a composite", ticker)
	}
}

func TestCreateCompositeRejectsABadFormula(t *testing.T) {
	h := newHarness(t, stubProvider{})

	rec, body := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"expression": "VTI/"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %v)", rec.Code, body)
	}
	if msg, _ := body["error"].(string); msg == "" {
		t.Error("a rejected formula came back without an explanation")
	}
}

func TestPatchCompositeFormulaRepricesTheRow(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300, "GLD": 200, "P": 30}})

	_, created := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"expression": "VTI/GLD"})
	ticker, _ := created["ticker"].(map[string]any)
	id, _ := ticker["id"].(string)

	rec, body := h.do(t, http.MethodPatch, "/api/tickers/"+id, map[string]any{"expression": "P/VTI"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}

	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	view := findTicker(t, state, "P/VTI")
	quote, _ := view["quote"].(map[string]any)
	if quote == nil || quote["status"] != "ok" {
		t.Fatalf("quote after the edit = %v", quote)
	}
	if price, _ := quote["price"].(float64); price != 0.1 {
		t.Errorf("price = %v, want 0.1", quote["price"])
	}
}

func TestCompositeHistoryIsServedLikeAnyOther(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}})

	_, created := h.do(t, http.MethodPost, "/api/tickers", map[string]any{"expression": "VTI/GLD"})
	ticker, _ := created["ticker"].(map[string]any)
	id, _ := ticker["id"].(string)

	rec, body := h.do(t, http.MethodGet, "/api/tickers/"+id+"/history", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}
	if body["symbol"] != "VTI/GLD" {
		t.Errorf("history is for %v, want VTI/GLD", body["symbol"])
	}
	points, _ := body["points"].([]any)
	if len(points) == 0 {
		t.Error("the composite recorded no history point, so it would draw no chart")
	}
}

// historianProvider is a stubProvider that also has a past, for the
// performance sheet.
type historianProvider struct {
	stubProvider
	bars map[string][]quotes.Bar
}

func (h historianProvider) History(_ context.Context, symbol string, _ time.Time) ([]quotes.Bar, error) {
	return h.bars[symbol], nil
}

func TestPerformanceEndpointServesTheChartAndTheReturns(t *testing.T) {
	oldest := time.Now().UTC().AddDate(-2, 0, 0).Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	provider := historianProvider{
		stubProvider: stubProvider{prices: map[string]float64{"VTI": 300}},
		bars: map[string][]quotes.Bar{"VTI": {
			{Date: oldest, Close: 100},
			{Date: time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02"), Close: 180},
			{Date: yesterday, Close: 200},
		}},
	}
	h := newHarness(t, provider)

	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	id, _ := findTicker(t, state, "VTI")["id"].(string)

	rec, body := h.do(t, http.MethodGet, "/api/tickers/"+id+"/performance", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %v", rec.Code, body)
	}
	perf, _ := body["performance"].(map[string]any)
	if perf["symbol"] != "VTI" {
		t.Fatalf("performance = %v", perf)
	}
	if points, _ := perf["points"].([]any); len(points) != 3 {
		t.Errorf("got %d points, want the 3 daily closes the provider has", len(points))
	}

	// Every window comes back whether or not the series reaches it, so the
	// table can say "not enough history" rather than quietly losing a row.
	returns := byKey(t, perf["returns"])
	if len(returns) != 9 {
		t.Fatalf("got %d return windows, want all 9: %v", len(returns), returns)
	}
	if year := returns["1y"]; year["available"] != true || year["changePercent"].(float64) != 100 {
		t.Errorf("1y = %v, want an available +100%% (100 → 200)", year)
	}
	for _, key := range []string{"5y", "10y"} {
		if r := returns[key]; r["available"] != false {
			t.Errorf("%s = %v; a two-year series has no return that long", key, r)
		}
	}
	// All time is the series' own first close — two years back here — so it is
	// available whenever anything is.
	if all := returns["all"]; all["available"] != true ||
		all["from"] != oldest || all["fromValue"].(float64) != 100 {
		t.Errorf("all-time = %v, want it measured from %s at 100", all, oldest)
	}

	// Ranges ride along for every ticker; the client shows them in place of
	// returns for composites, where a return would be a category error.
	ranges := byKey(t, perf["ranges"])
	if len(ranges) != 7 {
		t.Fatalf("got %d range windows, want all 7: %v", len(ranges), ranges)
	}
	all := ranges["all"]
	if all["available"] != true || all["low"].(float64) != 100 || all["high"].(float64) != 200 {
		t.Errorf("all-time range = %v, want 100–200", all)
	}
	if all["highDate"] != yesterday || all["position"].(float64) != 100 {
		t.Errorf("all-time range = %v; the latest close is the high, so it sits at 100%% of it", all)
	}

	if rec, _ := h.do(t, http.MethodGet, "/api/tickers/nope/performance", nil); rec.Code != http.StatusNotFound {
		t.Errorf("performance for an unknown ticker returned %d, want 404", rec.Code)
	}
}

func TestPerformanceSaysSoWhenTheProviderHasNoHistory(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	id, _ := findTicker(t, state, "VTI")["id"].(string)

	// A provider that can only price today is a choice, not a fault: the sheet
	// gets a sentence it can show rather than a 500 and a log line.
	rec, body := h.do(t, http.MethodGet, "/api/tickers/"+id+"/performance", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501", rec.Code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "history") {
		t.Errorf("error = %q, want it to name what is missing", msg)
	}
}

// byKey indexes a decoded list of return or range windows by its "key" field.
func byKey(t *testing.T, list any) map[string]map[string]any {
	t.Helper()
	entries, _ := list.([]any)
	out := make(map[string]map[string]any, len(entries))
	for _, entry := range entries {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("window is not an object: %v", entry)
		}
		key, _ := row["key"].(string)
		out[key] = row
	}
	return out
}

// findTicker pulls one row out of a /api/state body by symbol.
func findTicker(t *testing.T, state map[string]any, symbol string) map[string]any {
	t.Helper()
	tickers, _ := state["tickers"].([]any)
	for _, entry := range tickers {
		view, _ := entry.(map[string]any)
		if view["symbol"] == symbol {
			return view
		}
	}
	t.Fatalf("%s is not on the watchlist", symbol)
	return nil
}

func TestLogoEndpointServesCachedImage(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	png := []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10}
	if err := h.store.SaveLogo(store.Logo{
		Symbol: "VTI", Status: store.LogoOK, ContentType: "image/png", Bytes: png,
	}); err != nil {
		t.Fatalf("save logo: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/logos/VTI", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("logo returned %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("content type = %q, want image/png", got)
	}
	if rec.Body.String() != string(png) {
		t.Errorf("served %q, want the cached bytes", rec.Body.String())
	}
	// The bytes came from a third party and are served from this origin, so
	// the browser must not be left to decide what they are.
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("no Cache-Control on a logo; a watchlist redraw would re-fetch every image")
	}

	// A symbol with no cached image is a 404, not an empty 200 — the client
	// only asks for the ones state said it had.
	req = httptest.NewRequest(http.MethodGet, "/api/logos/GLD", nil)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("uncached logo returned %d, want 404", rec.Code)
	}
}

func TestStateListsCachedLogos(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	if err := h.store.SaveLogo(store.Logo{
		Symbol: "VTI", Status: store.LogoOK, ContentType: "image/png", Bytes: []byte{1, 2},
	}); err != nil {
		t.Fatalf("save logo: %v", err)
	}
	if err := h.store.SaveLogo(store.Logo{Symbol: "GLD", Status: store.LogoNone}); err != nil {
		t.Fatalf("save tombstone: %v", err)
	}

	_, body := h.do(t, http.MethodGet, "/api/state", nil)
	logos, ok := body["logos"].(map[string]any)
	if !ok {
		t.Fatalf("state has no logos map: %v", body["logos"])
	}
	if len(logos) != 1 {
		t.Errorf("state listed %v, want just VTI — a symbol with no logo must not be listed, "+
			"or the client draws a broken image for it", logos)
	}
	// The entry carries a version, so a replaced image beats the day of browser
	// caching the bytes are served with.
	mark, ok := logos["VTI"].(map[string]any)
	if !ok {
		t.Fatalf("VTI's entry is %v, want an object", logos["VTI"])
	}
	if v, ok := mark["v"].(float64); !ok || v <= 0 {
		t.Errorf("VTI's version is %v, want the second its image was stored", mark["v"])
	}
}

func TestStateNeverCarriesTheLogoKey(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	if rec, body := h.do(t, http.MethodPatch, "/api/settings", map[string]any{
		"logoKey":         "sk_live_do_not_leak",
		"logoUrlTemplate": "https://logos.test/{symbol}.png",
	}); rec.Code != http.StatusOK {
		t.Fatalf("patch returned %d: %v", rec.Code, body)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "sk_live_do_not_leak") {
		t.Error("the logo key is in /api/state, which every browser on the network gets")
	}

	// But the page still has to know one is stored, or the field cannot say so.
	_, body := h.do(t, http.MethodGet, "/api/state", nil)
	settings := body["settings"].(map[string]any)
	if settings["logoKeySet"] != true {
		t.Errorf("logoKeySet = %v, want true", settings["logoKeySet"])
	}
	if _, leaked := settings["logoKey"]; leaked {
		t.Error("the settings object has a logoKey field at all; it should never be serialised")
	}
}

func TestUploadAndRemoveALogo(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	upload := func(symbol string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/logos/"+symbol, bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}

	// A real PNG: the bytes are sniffed, not trusted, because they are served
	// back from this app's own origin.
	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89,
	}
	if rec := upload("VTI", png); rec.Code != http.StatusOK {
		t.Fatalf("upload returned %d: %s", rec.Code, rec.Body)
	}

	stored, err := h.store.Logo("VTI")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Origin != store.LogoCustom {
		t.Errorf("origin = %q, want custom — a refresh cycle would fetch over it", stored.Origin)
	}
	if stored.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png sniffed from the bytes", stored.ContentType)
	}

	// State reports it as an upload, so the UI can offer to remove it.
	_, body := h.do(t, http.MethodGet, "/api/state", nil)
	mark := body["logos"].(map[string]any)["VTI"].(map[string]any)
	if mark["custom"] != true {
		t.Errorf("state says custom=%v for an uploaded logo", mark["custom"])
	}

	// Anything that isn't an image is refused, whoever uploaded it: it would be
	// served back from this origin.
	if rec := upload("GLD", []byte("<html>not a logo</html>")); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("uploading markup returned %d, want 415", rec.Code)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/logos/VTI", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete returned %d", rec.Code)
	}
	if _, err := h.store.Logo("VTI"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the logo survived being removed")
	}
}

func TestUploadALogoForASymbolWithASlashInIt(t *testing.T) {
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300, "GLD": 200}})

	png := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	}
	// A composite's symbol is its formula, so it carries a slash. Escaped, it
	// has to arrive as one path segment rather than two.
	req := httptest.NewRequest(http.MethodPut, "/api/logos/VTI%2FGLD", bytes.NewReader(png))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload returned %d: %s — a composite cannot be given a logo", rec.Code, rec.Body)
	}
	if _, err := h.store.Logo("VTI/GLD"); err != nil {
		t.Fatalf("the composite's logo was stored under some other key: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/logos/VTI%2FGLD", nil)
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("serving it back returned %d", rec.Code)
	}
}
