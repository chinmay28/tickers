package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/mcptools"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/web"
)

func TestTheMCPEndpointIsMountedBesideTheClient(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/api.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, stubProvider{}, publish.New(), nil)
	webHandler, _ := web.Handler("")
	h := New(Options{Store: st, Engine: eng, Web: webHandler, MCP: mcptools.New(mcptools.Options{Store: st, Engine: eng})}).Handler()

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "run_backtest") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("POST /mcp = %d %.80s (Cache-Control %q), want the tool list, uncached", rec.Code, rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want the transport's 405 rather than the web client's shell", rec.Code)
	}
}
