package quotes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// chartJSON is a trimmed but structurally faithful /v8/finance/chart response:
// the meta block, and a 1-minute close series whose tail is null — which is
// what Yahoo actually returns for the current, not-yet-closed minute, and the
// reason the parser scans backwards rather than taking the last element.
const chartJSON = `{
  "chart": {
    "result": [{
      "meta": {
        "currency": "USD",
        "symbol": "VTI",
        "exchangeName": "PCX",
        "shortName": "Vanguard Total Stock Market ETF",
        "marketState": "REGULAR",
        "regularMarketPrice": 299.99,
        "chartPreviousClose": 290.00,
        "previousClose": 289.00
      },
      "indicators": {
        "quote": [{ "close": [291.10, 294.25, 295.50, null, null] }]
      }
    }],
    "error": null
  }
}`

// metaOnlyJSON has an empty series — a symbol outside its trading session.
const metaOnlyJSON = `{
  "chart": {
    "result": [{
      "meta": { "currency": "EUR", "symbol": "VWRL.AS", "regularMarketPrice": 123.45, "previousClose": 120.00 },
      "indicators": { "quote": [{ "close": [null, null] }] }
    }],
    "error": null
  }
}`

const chartErrorJSON = `{
  "chart": { "result": null, "error": { "code": "Not Found", "description": "No data found, symbol may be delisted" } }
}`

const searchJSON = `{
  "quotes": [
    { "symbol": "ORCL", "shortname": "Oracle Corporation", "exchDisp": "NYSE", "typeDisp": "Equity" },
    { "symbol": "ORCL.MX", "longname": "Oracle Corp", "exchDisp": "Mexico", "quoteType": "EQUITY" },
    { "shortname": "no symbol, must be skipped" }
  ]
}`

func newTestYahoo(t *testing.T, handler http.HandlerFunc) *Yahoo {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewYahoo(Settings{BaseURL: srv.URL, Timeout: 5 * time.Second})
}

func TestFetchPrefersLastNonNullClose(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v8/finance/chart/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != DefaultUserAgent {
			t.Errorf("User-Agent %q; Yahoo stonewalls requests without a browser UA", got)
		}
		w.Write([]byte(chartJSON))
	})

	got, failures := y.Fetch(context.Background(), []string{"vti"})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	q, ok := got["VTI"]
	if !ok {
		t.Fatalf("no quote for VTI; got keys %v", keys(got))
	}
	// 295.50 is the last non-null close, not the 299.99 in meta — the original
	// script took `data['Close'].iloc[-1]` and this must match it.
	if q.Price == nil || *q.Price != 295.50 {
		t.Errorf("price = %v, want 295.50 (the last non-null 1m close)", deref(q.Price))
	}
	if q.PreviousClose == nil || *q.PreviousClose != 290.00 {
		t.Errorf("previousClose = %v, want 290.00 (chartPreviousClose wins)", deref(q.PreviousClose))
	}
	if q.Currency != "USD" || q.ShortName == "" || q.MarketState != "REGULAR" {
		t.Errorf("metadata not carried through: %+v", q)
	}
}

func TestFetchFallsBackToMetaPrice(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(metaOnlyJSON))
	})

	got, failures := y.Fetch(context.Background(), []string{"VWRL.AS"})
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if q := got["VWRL.AS"]; q.Price == nil || *q.Price != 123.45 {
		t.Errorf("price = %v, want the meta fallback 123.45", deref(q.Price))
	}
}

func TestFetchReportsPerSymbolFailures(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "GOOD"):
			w.Write([]byte(chartJSON))
		case strings.Contains(r.URL.Path, "DELISTED"):
			w.Write([]byte(chartErrorJSON))
		case strings.Contains(r.URL.Path, "MISSING"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("upstream is unwell"))
		}
	})

	got, failures := y.Fetch(context.Background(), []string{"GOOD", "DELISTED", "MISSING", "BROKEN"})

	// One bad symbol must not take the others down with it — that is the whole
	// contract the engine relies on.
	if _, ok := got["GOOD"]; !ok {
		t.Error("the healthy symbol was lost alongside the failing ones")
	}
	for _, symbol := range []string{"DELISTED", "MISSING", "BROKEN"} {
		if failures[symbol] == nil {
			t.Errorf("%s: expected a failure, got none", symbol)
		}
	}
	if !errors.Is(failures["MISSING"], ErrNotFound) {
		t.Errorf("a 404 should map to ErrNotFound, got %v", failures["MISSING"])
	}
	if !strings.Contains(failures["BROKEN"].Error(), "500") {
		t.Errorf("a 5xx should report its status, got %v", failures["BROKEN"])
	}
}

func TestFetchSkipsBlankSymbols(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("blank symbol reached the network as %s", r.URL.Path)
	})
	got, failures := y.Fetch(context.Background(), []string{"", "   "})
	if len(got) != 0 || len(failures) != 0 {
		t.Errorf("blank symbols produced results: %v / %v", got, failures)
	}
}

func TestSearchMapsResults(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "oracle corp" {
			t.Errorf("query not passed through: %q", r.URL.RawQuery)
		}
		w.Write([]byte(searchJSON))
	})

	matches, err := y.Search(context.Background(), " oracle corp ")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2 (the symbol-less entry must be dropped)", len(matches))
	}
	if matches[0].Symbol != "ORCL" || matches[0].Name != "Oracle Corporation" || matches[0].Exchange != "NYSE" {
		t.Errorf("first match is %+v", matches[0])
	}
	// longname/quoteType are the fallbacks when shortname/typeDisp are absent.
	if matches[1].Name != "Oracle Corp" || matches[1].Type != "EQUITY" {
		t.Errorf("second match did not use the fallback fields: %+v", matches[1])
	}
}

func TestSearchEmptyQueryMakesNoRequest(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("empty query reached the network")
	})
	matches, err := y.Search(context.Background(), "  ")
	if err != nil || len(matches) != 0 {
		t.Fatalf("Search(\"\") = %v, %v; want empty, nil", matches, err)
	}
}

func TestFetchHonoursCancelledContext(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chartJSON))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, failures := y.Fetch(ctx, []string{"VTI"})
	if len(got) != 0 {
		t.Errorf("a cancelled context still produced quotes: %v", got)
	}
	if failures["VTI"] == nil {
		t.Error("cancellation was not reported as a per-symbol failure")
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestSettingsMergeKeepsUnsetFields(t *testing.T) {
	base := Settings{BaseURL: "https://a", Timeout: time.Second, UserAgent: "ua"}

	if got := base.Merge(Settings{}); got != base {
		t.Errorf("an empty override changed things: %+v", got)
	}
	got := base.Merge(Settings{BaseURL: "https://b"})
	if got.BaseURL != "https://b" || got.Timeout != time.Second || got.UserAgent != "ua" {
		t.Errorf("merge = %+v; only BaseURL should have moved", got)
	}
}

func TestApplyLayersStoredOverStartupOverDefault(t *testing.T) {
	// The precedence the Settings page depends on: stored > flag > built-in,
	// and clearing a stored field reveals the layer beneath it again.
	y := NewYahoo(Settings{BaseURL: "https://from-flag", UserAgent: "flag-ua"})

	eff := y.Effective()
	if eff.BaseURL != "https://from-flag" || eff.UserAgent != "flag-ua" {
		t.Fatalf("start-up fallback not applied: %+v", eff)
	}
	if eff.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want the package default %v", eff.Timeout, DefaultTimeout)
	}

	y.Apply(Settings{BaseURL: "https://from-db/", Timeout: 7 * time.Second})
	eff = y.Effective()
	if eff.BaseURL != "https://from-db" {
		t.Errorf("stored base URL did not win (or the trailing slash survived): %q", eff.BaseURL)
	}
	if eff.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want the stored 7s", eff.Timeout)
	}
	if eff.UserAgent != "flag-ua" {
		t.Errorf("user agent = %q; an unset stored field must fall through to the flag", eff.UserAgent)
	}

	// Clearing everything stored falls back to the flag, then the default.
	y.Apply(Settings{})
	if eff := y.Effective(); eff.BaseURL != "https://from-flag" || eff.Timeout != DefaultTimeout {
		t.Errorf("clearing the override did not reveal the fallback: %+v", eff)
	}
}

func TestApplyTakesEffectOnTheNextRequest(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.Header.Get("User-Agent"); got != "changed-ua" {
			t.Errorf("User-Agent = %q, want the reconfigured value", got)
		}
		w.Write([]byte(chartJSON))
	}))
	defer srv.Close()

	// Start pointed nowhere useful, then reconfigure — this is what the
	// Settings page does, and the point is that no restart is involved.
	y := NewYahoo(Settings{BaseURL: "http://127.0.0.1:1", Timeout: time.Second})
	y.Apply(Settings{BaseURL: srv.URL, UserAgent: "changed-ua", Timeout: 5 * time.Second})

	got, failures := y.Fetch(context.Background(), []string{"VTI"})
	if len(failures) != 0 {
		t.Fatalf("fetch failed after reconfiguration: %v", failures)
	}
	if _, ok := got["VTI"]; !ok || hits != 1 {
		t.Fatalf("request did not reach the new base URL (hits=%d)", hits)
	}
}

func TestApplyIsSafeDuringFetch(t *testing.T) {
	// Worth running under -race: Apply swaps the client and the settings while
	// Fetch is reading them.
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(chartJSON))
	})
	base := y.Effective()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				y.Apply(Settings{BaseURL: base.BaseURL, Timeout: time.Duration(i+1) * time.Second})
				return
			}
			y.Fetch(context.Background(), []string{"VTI"})
		}(i)
	}
	wg.Wait()
}
