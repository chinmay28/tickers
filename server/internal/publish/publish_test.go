package publish

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/store"
)

func price(v float64) *float64 { return &v }

func testSnapshot() Snapshot {
	return Snapshot{
		At: time.Date(2025, 8, 7, 14, 3, 22, 0, time.UTC),
		Quotes: []store.Quote{
			{Symbol: "VTI", Price: price(295.501), PreviousClose: price(290), Currency: "USD",
				ShortName: "Vanguard Total Stock Market ETF", Status: store.StatusOK},
			{Symbol: "BTC-USD", Price: price(68120.115), PreviousClose: price(67000), Currency: "USD",
				Status: store.StatusOK},
			{Symbol: "NOPE", Status: store.StatusError, Error: "no such symbol"},
		},
	}
}

func TestMinionPayloadMatchesTheOriginalScript(t *testing.T) {
	got := Payload(testSnapshot(), store.FormatMinion)

	// This is the compatibility test that matters: anything already consuming
	// the feed the Python script wrote must see the same shape — flat map,
	// 2-decimal strings, "N/A" for a failure, and a "MM/DD HH:MM:SS" timestamp.
	want := map[string]any{
		"VTI":       "295.50",
		"BTC-USD":   "68120.12",
		"NOPE":      "N/A",
		"timestamp": "08/07 14:03:22",
	}
	if len(got) != len(want) {
		t.Fatalf("payload has %d keys, want %d: %v", len(got), len(want), got)
	}
	for key, expected := range want {
		if got[key] != expected {
			t.Errorf("payload[%q] = %v (%T), want %q", key, got[key], got[key], expected)
		}
	}
}

func TestDetailedPayload(t *testing.T) {
	got := Payload(testSnapshot(), store.FormatDetailed)

	vti, ok := got["VTI"].(map[string]any)
	if !ok {
		t.Fatalf("VTI entry is %T, want an object", got["VTI"])
	}
	if vti["price"] != 295.50 {
		t.Errorf("price = %v, want 295.50", vti["price"])
	}
	if vti["change"] != 5.50 {
		t.Errorf("change = %v, want 5.50", vti["change"])
	}
	if vti["status"] != store.StatusOK || vti["currency"] != "USD" {
		t.Errorf("metadata missing: %v", vti)
	}

	failed := got["NOPE"].(map[string]any)
	if failed["status"] != store.StatusError || failed["error"] != "no such symbol" {
		t.Errorf("failed entry is %v, want the recorded error", failed)
	}
	if _, ok := failed["price"]; ok {
		t.Error("a failed reading must not carry a price")
	}

	// The legacy timestamp stays, so a consumer can move to the richer format
	// without changing how it reads the clock.
	if got["timestamp"] != "08/07 14:03:22" {
		t.Errorf("timestamp = %v, want the legacy rendering", got["timestamp"])
	}
	if got["timestampISO"] != "2025-08-07T14:03:22Z" {
		t.Errorf("timestampISO = %v", got["timestampISO"])
	}
}

func TestPublishFallsBackFromPutToPost(t *testing.T) {
	var (
		methods  []string
		putBody  map[string]any
		postBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		blob, _ := io.ReadAll(r.Body)

		switch r.Method {
		case http.MethodPut:
			// The store doesn't have the entry yet — the exact case the
			// original script's try/except handled.
			json.Unmarshal(blob, &putBody)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"no such entry"}`))
		case http.MethodPost:
			json.Unmarshal(blob, &postBody)
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()

	sink := store.Sink{
		ID: "s1", Name: "Home", BaseURL: srv.URL + "/api/entries",
		Key: "minion-quotes", Category: "minion", Format: store.FormatMinion, TimeoutMS: 5000,
	}

	result := New().Publish(context.Background(), sink, testSnapshot())
	if !result.OK {
		t.Fatalf("publish failed: %s", result.Error)
	}
	if result.Method != http.MethodPost || result.StatusCode != http.StatusCreated {
		t.Errorf("result = %s %d, want POST 201", result.Method, result.StatusCode)
	}
	if len(methods) != 2 || methods[0] != "PUT /api/entries/minion-quotes" || methods[1] != "POST /api/entries" {
		t.Fatalf("request sequence was %v, want PUT the key then POST the base", methods)
	}

	// The two bodies differ, and both must match what the script sent.
	if putBody["category"] != "minion" {
		t.Errorf("PUT body missing category: %v", putBody)
	}
	if _, ok := putBody["key"]; ok {
		t.Error("the PUT body must not carry a key — the key is in the URL")
	}
	if postBody["key"] != "minion-quotes" {
		t.Errorf("POST body key = %v, want minion-quotes", postBody["key"])
	}
	if value, ok := postBody["value"].(map[string]any); !ok || value["VTI"] != "295.50" {
		t.Errorf("POST body value is wrong: %v", postBody["value"])
	}
}

func TestPublishPrefersPutWhenItWorks(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := New().Publish(context.Background(), store.Sink{
		BaseURL: srv.URL, Key: "k", Format: store.FormatMinion, TimeoutMS: 5000,
	}, testSnapshot())

	if !result.OK || result.Method != http.MethodPut {
		t.Fatalf("result = %+v, want a successful PUT", result)
	}
	if len(methods) != 1 {
		t.Errorf("made %d requests (%v); a successful PUT must not be followed by a POST", len(methods), methods)
	}
}

func TestPublishReportsBothFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("the store is on fire"))
	}))
	defer srv.Close()

	result := New().Publish(context.Background(), store.Sink{
		BaseURL: srv.URL, Key: "k", Format: store.FormatMinion, TimeoutMS: 5000,
	}, testSnapshot())

	if result.OK {
		t.Fatal("publish reported success against a failing store")
	}
	// Being told only about the POST sends people to the wrong endpoint.
	for _, want := range []string{"PUT", "POST", "500", "on fire"} {
		if !contains(result.Error, want) {
			t.Errorf("error %q does not mention %q", result.Error, want)
		}
	}
}

func TestPublishHonoursSinkTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := New().Publish(context.Background(), store.Sink{
		BaseURL: srv.URL, Key: "k", Format: store.FormatMinion, TimeoutMS: 50,
	}, testSnapshot())

	if result.OK {
		t.Fatal("a request past the sink's timeout was reported as a success")
	}
}

func TestPublishAllVisitsEverySink(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sinks := []store.Sink{
		{Name: "a", BaseURL: srv.URL, Key: "one", Format: store.FormatMinion, TimeoutMS: 5000},
		{Name: "b", BaseURL: srv.URL, Key: "two", Format: store.FormatDetailed, TimeoutMS: 5000},
	}
	results := New().PublishAll(context.Background(), sinks, testSnapshot())

	if len(results) != 2 || !results[0].OK || !results[1].OK {
		t.Fatalf("results = %+v", results)
	}
	if len(seen) != 2 || seen[0] != "/one" || seen[1] != "/two" {
		t.Errorf("visited %v, want /one then /two", seen)
	}
}

func TestRound2(t *testing.T) {
	// Half-way cases are decided by float64's representation, not by a rule —
	// 68120.115 is really 68120.11500000000...728, so it rounds up. The cases
	// below are the unambiguous ones plus that documented reality.
	for in, want := range map[float64]float64{
		295.501: 295.50, 295.506: 295.51, -1.006: -1.01, 0: 0, 68120.115: 68120.12,
	} {
		if got := round2(in); got != want {
			t.Errorf("round2(%v) = %v, want %v", in, got, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	}()
}
