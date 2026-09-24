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

	"github.com/chinmay28/tickers/server/internal/archiver"
	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

type emptyArchivist struct{}

func (emptyArchivist) Candles(context.Context, string, quotes.Interval, time.Time, time.Time) (quotes.CandleSeries, error) {
	return quotes.CandleSeries{}, nil
}

func newArchiveHarness(t *testing.T) (*harness, *archiver.Manager) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/api.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	paused, listed := true, false
	st.UpdateArchiveConfig(store.ArchivePatch{Paused: &paused, Listed: &listed})
	eng := engine.New(st, stubProvider{}, publish.New(), nil)
	m := archiver.New(archiver.Options{Store: st, Yahoo: emptyArchivist{}, Symbols: eng.Symbols})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	srv := New(Options{Store: st, Engine: eng, Archive: m})
	return &harness{handler: srv.Handler(), store: st, engine: eng}, m
}

func (h *harness) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestArchiveEndpointsNeedAnArchive(t *testing.T) {
	h := newHarness(t, stubProvider{})
	for _, path := range []string{"/api/archive", "/api/archive/symbols"} {
		rec, _ := h.do(t, http.MethodGet, path, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("GET %s without an archive = %d, want 501", path, rec.Code)
		}
	}
}

func TestTheDataPageLifecycle(t *testing.T) {
	h, m := newArchiveHarness(t)

	code, view := h.call(t, http.MethodGet, "/api/archive", nil)
	if code != http.StatusOK || view["state"] != archiver.StateOff {
		t.Fatalf("GET /api/archive = %d %v, want 200 and off", code, view["state"])
	}

	dir := t.TempDir()
	code, folder := h.call(t, http.MethodGet, "/api/archive/folder?path="+dir, nil)
	if code != http.StatusOK || folder["writable"] != true || folder["isArchive"] != false {
		t.Errorf("inspect = %d %v", code, folder)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/archive/folder", map[string]string{"path": "relative", "mode": "use"}); code != http.StatusBadRequest {
		t.Errorf("a relative folder = %d, want 400", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/archive/folder", map[string]string{"path": dir, "mode": "use"}); code != http.StatusAccepted {
		t.Fatalf("use folder = %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.Status(false).State != archiver.StateOpen {
		if time.Now().After(deadline) {
			t.Fatalf("archive never opened: %+v", m.Status(false))
		}
		time.Sleep(20 * time.Millisecond)
	}

	if code, _ := h.call(t, http.MethodPost, "/api/archive/symbols", map[string]string{"symbol": "not a symbol!"}); code != http.StatusBadRequest {
		t.Errorf("adding a malformed symbol = %d, want 400", code)
	}
	code, added := h.call(t, http.MethodPost, "/api/archive/symbols", map[string]string{"symbol": "tsla", "name": "Tesla"})
	if code != http.StatusCreated || added["symbol"] != "TSLA" || added["priority"] != true {
		t.Fatalf("add = %d %v, want TSLA, first in line", code, added)
	}
	code, page := h.call(t, http.MethodGet, "/api/archive/symbols?q=TS", nil)
	if code != http.StatusOK || page["total"].(float64) != 1 {
		t.Errorf("search = %d %v", code, page)
	}
	if code, _ := h.call(t, http.MethodGet, "/api/archive/symbols?filter=bogus", nil); code != http.StatusBadRequest {
		t.Errorf("an unknown filter = %d, want 400", code)
	}
	code, patched := h.call(t, http.MethodPatch, "/api/archive/symbols/TSLA", map[string]bool{"excluded": true})
	if code != http.StatusOK || patched["excluded"] != true || patched["active"] != false {
		t.Errorf("exclude = %d %v", code, patched)
	}
	if code, _ := h.call(t, http.MethodGet, "/api/archive/symbols/NOPE", nil); code != http.StatusNotFound {
		t.Errorf("an unknown symbol = %d, want 404", code)
	}
	code, detail := h.call(t, http.MethodGet, "/api/archive/symbols/TSLA", nil)
	if code != http.StatusOK || detail["symbol"].(map[string]any)["name"] != "Tesla" {
		t.Errorf("detail = %d %v", code, detail)
	}
	if code, _ := h.call(t, http.MethodGet, "/api/archive/symbols/TSLA/bars?interval=1d&from=2024-01-01&to=2024-12-31", nil); code != http.StatusOK {
		t.Errorf("bars = %d", code)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/archive/symbols/TSLA/fetch",
		map[string]any{"interval": "3m", "source": "yahoo", "from": "2024-01-01", "to": "2024-01-31"}); code != http.StatusBadRequest {
		t.Errorf("a fetch at an unknown interval = %d, want 400", code)
	}
	code, job := h.call(t, http.MethodPost, "/api/archive/symbols/TSLA/fetch",
		map[string]any{"interval": "1d", "source": "yahoo", "from": "2024-01-01", "to": "2024-01-31"})
	if code != http.StatusAccepted || job["state"] != "queued" {
		t.Errorf("fetch = %d %v", code, job)
	}
	if code, _ := h.call(t, http.MethodPost, "/api/archive/symbols/TSLA/reset", map[string]string{}); code != http.StatusOK {
		t.Errorf("reset = %d", code)
	}

	fast := 10
	if code, _ := h.call(t, http.MethodPatch, "/api/archive/settings", store.ArchivePatch{SpacingMS: &fast}); code != http.StatusBadRequest {
		t.Errorf("spacing below the floor = %d, want 400", code)
	}
	rec, _ := h.do(t, http.MethodPatch, "/api/archive/settings", map[string]string{"polygonKey": "pk_secret"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "pk_secret") {
		t.Errorf("setting the key = %d %s — the key must never be served back", rec.Code, rec.Body.String())
	}
	code, view = h.call(t, http.MethodGet, "/api/archive", nil)
	if sources := view["sources"].([]any); len(sources) != 2 || sources[1] != "polygon" {
		t.Errorf("sources = %v, want polygon offered once its key is set", sources)
	}
}
