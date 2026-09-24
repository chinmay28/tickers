package quotes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPolygonSymbol(t *testing.T) {
	cases := map[string]string{
		"AAPL":    "AAPL",
		"brk-b":   "BRK.B",
		"BTC-USD": "X:BTCUSD",
		"ETH-USD": "X:ETHUSD",
		"^GSPC":   "I:SPX",
		"^VIX":    "I:VIX",
	}
	for in, want := range cases {
		if got, err := PolygonSymbol(in); err != nil || got != want {
			t.Errorf("PolygonSymbol(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"GC=F", "^FOO", ""} {
		if _, err := PolygonSymbol(in); !errors.Is(err, ErrUnsupportedSymbol) {
			t.Errorf("PolygonSymbol(%q) = %v, want ErrUnsupportedSymbol", in, err)
		}
	}
}

// ms is a New York wall-clock time as Polygon's millisecond timestamp.
func ms(t *testing.T, s string) int64 {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04", s, newYork)
	if err != nil {
		t.Fatal(err)
	}
	return at.UnixMilli()
}

func TestPolygonCandlesKeepTheRegularSessionAndReportSplits(t *testing.T) {
	var splitCalls int32
	var auth string
	var page2 string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/aggs/ticker/BRK.B/range/1/minute/"):
			if r.URL.Query().Get("cursor") == "" {
				// Polygon's next_url carries the query in its cursor, so only
				// the first page is ours to check.
				if r.URL.Query().Get("adjusted") != "true" {
					t.Error("bars were not asked for split-adjusted")
				}
				fmt.Fprintf(w, `{"status":"OK","results":[
				  {"o":1,"h":1,"l":1,"c":1,"v":10,"t":%d},
				  {"o":2,"h":2,"l":2,"c":2,"v":20,"t":%d}
				],"next_url":"%s"}`, ms(t, "2024-06-03 09:29"), ms(t, "2024-06-03 09:30"), page2)
				return
			}
			fmt.Fprintf(w, `{"status":"OK","results":[
			  {"o":3,"h":3,"l":3,"c":3,"v":30,"t":%d},
			  {"o":4,"h":4,"l":4,"c":4,"v":40,"t":%d}
			]}`, ms(t, "2024-06-03 15:59"), ms(t, "2024-06-03 16:00"))
		case r.URL.Path == "/v3/reference/splits":
			atomic.AddInt32(&splitCalls, 1)
			fmt.Fprint(w, `{"results":[
			  {"execution_date":"2024-06-03","split_from":1,"split_to":4},
			  {"execution_date":"2010-01-04","split_from":1,"split_to":2}
			]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	page2 = srv.URL + "/v2/aggs/ticker/BRK.B/range/1/minute/x/y?cursor=abc"

	p := NewPolygon(srv.URL, "secret", 0)
	from := time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC)
	got, err := p.Candles(context.Background(), "BRK-B", OneMinute, from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("candles: %v", err)
	}
	if auth != "Bearer secret" {
		t.Errorf("Authorization = %q, want the key as a bearer token, not in the URL", auth)
	}
	if len(got.Candles) != 2 || got.Candles[0].Close != 2 || got.Candles[1].Close != 3 {
		t.Fatalf("candles = %+v, want 9:30 and 15:59 across both pages — pre- and post-market dropped", got.Candles)
	}
	if len(got.Splits) != 1 || got.Splits[0].Ratio() != 0.25 {
		t.Errorf("splits = %+v, want only the one inside the window, 4-for-1", got.Splits)
	}

	// A second window for the same symbol reuses the split history.
	if _, err := p.Candles(context.Background(), "BRK-B", OneMinute, from, from.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&splitCalls); n != 1 {
		t.Errorf("split history fetched %d times, want once per symbol", n)
	}
}

func TestPolygonDailyBarsAreKeyedByDateAndCryptoIsNotFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/X:BTCUSD/range/1/day/"):
			fmt.Fprintf(w, `{"results":[{"o":1,"h":2,"l":0.5,"c":1.5,"v":7,"t":%d}]}`,
				time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC).UnixMilli())
		case strings.Contains(r.URL.Path, "/X:BTCUSD/range/1/minute/"):
			fmt.Fprintf(w, `{"results":[{"o":1,"h":1,"l":1,"c":1,"v":1,"t":%d}]}`,
				time.Date(2024, 6, 3, 3, 0, 0, 0, time.UTC).UnixMilli())
		case strings.Contains(r.URL.Path, "/v3/reference/splits"):
			t.Error("asked for a crypto pair's splits")
		}
	}))
	defer srv.Close()
	p := NewPolygon(srv.URL, "k", 0)
	from := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	d, err := p.Candles(context.Background(), "BTC-USD", Daily, from, from.AddDate(0, 0, 5))
	if err != nil || len(d.Candles) != 1 || !d.Candles[0].Time.Equal(time.Date(2024, 6, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("daily = %+v, %v", d.Candles, err)
	}
	m, _ := p.Candles(context.Background(), "BTC-USD", OneMinute, from, from.AddDate(0, 0, 5))
	if len(m.Candles) != 1 {
		t.Errorf("a 3am crypto bar was filtered as out of session: %+v", m.Candles)
	}
}

func TestPolygonFailures(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"status":"ERROR","message":"Your plan doesn't include this data timeframe."}`)
	}))
	defer srv.Close()
	from := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	p := NewPolygon(srv.URL, "k", 0)

	if _, err := p.Candles(context.Background(), "AAPL", OneMinute, from, from.Add(time.Hour)); !errors.Is(err, ErrRateLimited) {
		t.Errorf("a 429 gave %v, want ErrRateLimited", err)
	}
	status = http.StatusForbidden
	_, err := p.Candles(context.Background(), "AAPL", OneMinute, from, from.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "plan doesn't include") {
		t.Errorf("a 403 gave %v, want Polygon's own message", err)
	}
	if _, err := NewPolygon(srv.URL, "", 0).Candles(context.Background(), "AAPL", OneMinute, from, from.Add(time.Hour)); err == nil {
		t.Error("a request went out with no key")
	}
}
