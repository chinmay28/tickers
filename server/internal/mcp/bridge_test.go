package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// lines decodes a bridge's output, one JSON object per line, ordered by id.
func lines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("output line %q is not one JSON object", l)
		}
		got = append(got, v)
	}
	sort.Slice(got, func(i, j int) bool { return got[i]["id"].(float64) < got[j]["id"].(float64) })
	return got
}

func TestBridgeRelaysStdioToHTTP(t *testing.T) {
	var mu sync.Mutex
	var versions []string
	srv := testServer()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		versions = append(versions, r.Header.Get("MCP-Protocol-Version"))
		mu.Unlock()
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		``,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	}, "\n")
	var out bytes.Buffer
	if err := Bridge(context.Background(), ts.URL, strings.NewReader(in), &out, nil); err != nil {
		t.Fatal(err)
	}
	got := lines(t, out.String())
	if len(got) != 3 {
		t.Fatalf("got %d answers, want 3 — the notification has none: %s", len(got), out.String())
	}
	if !strings.Contains(out.String(), `\"said\":\"hi\"`) {
		t.Errorf("the tool call's answer didn't come through: %s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if versions[0] != "" {
		t.Errorf("the handshake carried version %q, before one was agreed", versions[0])
	}
	for _, v := range versions[1:] {
		if v != "2025-03-26" {
			t.Errorf("a request after the handshake carried version %q, want the one agreed", v)
		}
	}
}

func TestBridgeAnswersForAnUnreachableServer(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	in := `{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	var out bytes.Buffer
	if err := Bridge(context.Background(), url, strings.NewReader(in), &out, nil); err != nil {
		t.Fatal(err)
	}
	got := lines(t, out.String())
	if len(got) != 1 || got[0]["id"] != float64(7) || !strings.Contains(out.String(), "can't be reached") {
		t.Errorf("output = %s, want the request answered with an error saying the server is unreachable, and nothing for the notification", out.String())
	}
}

func TestBridgeReportsAServerThatIsNotMCP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>the web client</html>"))
	}))
	defer ts.Close()
	var out bytes.Buffer
	Bridge(context.Background(), ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), &out, nil)
	if !strings.Contains(out.String(), "/mcp address") {
		t.Errorf("output = %s, want a hint that the URL isn't the MCP endpoint", out.String())
	}
}
