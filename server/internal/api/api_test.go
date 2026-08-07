package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	srv := New(Options{Store: st, Engine: eng, Web: webHandler})
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
	if first["placeholder"] != true {
		t.Error("seeded tickers must be flagged as placeholders so the UI can offer Replace")
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

func TestReplacePlaceholderClearsTheFlagAndRepricesIt(t *testing.T) {
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
		t.Fatal("placeholder P is missing")
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
		if view["placeholder"] != false {
			t.Error("a replaced placeholder is still flagged as one")
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
