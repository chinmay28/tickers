package quotes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// historyJSON is a daily /v8/finance/chart response.
//
// The three details that matter, all of them real: the timestamps are the
// session's own open in UTC (21:00 the previous day for an exchange 13 hours
// ahead), there is an adjusted series alongside the raw closes, and one of its
// entries is null — a session that produced no trade.
const historyJSON = `{
  "chart": {
    "result": [{
      "meta": { "currency": "NZD", "symbol": "AIR.NZ", "gmtoffset": 46800 },
      "timestamp": [1704229200, 1704315600, 1704402000],
      "indicators": {
        "quote": [{ "close": [10.0, 11.0, 12.0] }],
        "adjclose": [{ "adjclose": [9.5, null, 11.5] }]
      }
    }],
    "error": null
  }
}`

// rawOnlyHistoryJSON is what comes back for an instrument with no adjustments
// to make — a currency pair, a crypto ticker.
const rawOnlyHistoryJSON = `{
  "chart": {
    "result": [{
      "meta": { "currency": "USD", "symbol": "BTC-USD", "gmtoffset": 0 },
      "timestamp": [1704153600, 1704240000],
      "indicators": { "quote": [{ "close": [42000.0, 43000.0] }] }
    }],
    "error": null
  }
}`

// dividendJSON is what `events=div` adds: a map keyed by epoch second, in no
// particular order, with a zero-amount entry of the kind Yahoo occasionally
// emits for a declared-but-unpaid distribution.
const dividendJSON = `{
  "chart": {
    "result": [{
      "meta": { "currency": "USD", "symbol": "VTSMX", "gmtoffset": -18000 },
      "timestamp": [1679500800],
      "indicators": { "quote": [{ "close": [100.0] }] },
      "events": {
        "dividends": {
          "1687363200": { "amount": 0.27, "date": 1687363200 },
          "1679500800": { "amount": 0.31, "date": 1679500800 },
          "1690000000": { "amount": 0, "date": 1690000000 }
        }
      }
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

func TestHistoryPrefersAdjustedClosesAndDropsGaps(t *testing.T) {
	var query url.Values
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Write([]byte(historyJSON))
	})

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars, err := y.History(context.Background(), "air.nz", since)
	if err != nil {
		t.Fatalf("history: %v", err)
	}

	// Explicit epochs, not a named range: a five-year return needs a close from
	// before five years ago, which "range=5y" cannot be asked for.
	if got := query.Get("period1"); got != strconv.FormatInt(since.Unix(), 10) {
		t.Errorf("period1 = %q, want the requested start %d", got, since.Unix())
	}
	if query.Get("interval") != "1d" {
		t.Errorf("interval = %q, want 1d", query.Get("interval"))
	}

	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2 — the null adjusted close is a session with no trade and must be dropped", len(bars))
	}
	// 9.5, not the raw 10.0: an unadjusted five-year chart of a stock that has
	// split shows a crash nobody experienced.
	if bars[0].Close != 9.5 || bars[1].Close != 11.5 {
		t.Errorf("closes = %v, want the adjusted series", bars)
	}
	// 21:00 UTC on 2 January is the 3rd in Auckland, and the exchange's day is
	// what a composite aligns its legs on.
	if bars[0].Date != "2024-01-03" || bars[1].Date != "2024-01-05" {
		t.Errorf("dates = %q/%q, want the exchange's own calendar days", bars[0].Date, bars[1].Date)
	}
	// Both series come back. A yield divides a payout by the price of its own
	// day, and the adjusted close is not that price.
	if bars[0].Raw != 10.0 || bars[1].Raw != 12.0 {
		t.Errorf("raw closes = %v/%v, want the unadjusted 10.0/12.0", bars[0].Raw, bars[1].Raw)
	}
}

func TestDividendsReadTheEventsBlock(t *testing.T) {
	var query url.Values
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Write([]byte(dividendJSON))
	})

	since := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	dividends, err := y.Dividends(context.Background(), "vtsmx", since)
	if err != nil {
		t.Fatalf("dividends: %v", err)
	}

	if query.Get("events") != "div" {
		t.Errorf("events = %q; without it Yahoo returns prices and no payouts", query.Get("events"))
	}
	// Monthly bars, because this call reads the events block and nothing else —
	// asking for daily would drag thirty years of prices along with it.
	if query.Get("interval") != "1mo" {
		t.Errorf("interval = %q, want 1mo", query.Get("interval"))
	}

	if len(dividends) != 2 {
		t.Fatalf("got %d dividends, want 2 (the zero-amount entry is not a payout): %+v", len(dividends), dividends)
	}
	// Yahoo keys the block by epoch, so what comes out of the map is unordered
	// until this sorts it — and a yield summed per year needs them in order.
	if dividends[0].Date != "2023-03-22" || dividends[1].Date != "2023-06-21" {
		t.Errorf("dates = %q/%q, want them oldest first", dividends[0].Date, dividends[1].Date)
	}
	if dividends[0].Amount != 0.31 {
		t.Errorf("amount = %v, want 0.31", dividends[0].Amount)
	}
}

func TestDividendsAreEmptyForSomethingThatPaysNone(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rawOnlyHistoryJSON))
	})

	dividends, err := y.Dividends(context.Background(), "BTC-USD", time.Now().AddDate(-1, 0, 0))
	if err != nil {
		t.Fatalf("a symbol with no events block is not an error: %v", err)
	}
	if len(dividends) != 0 {
		t.Errorf("got %v, want none", dividends)
	}
}

func TestHistoryFallsBackToRawCloses(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(rawOnlyHistoryJSON))
	})

	bars, err := y.History(context.Background(), "BTC-USD", time.Now().AddDate(-1, 0, 0))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(bars) != 2 || bars[0].Close != 42000 || bars[1].Date != "2024-01-03" {
		t.Errorf("bars = %+v; an instrument with no adjusted series still has closes", bars)
	}
}

func TestHistoryReportsUpstreamFailures(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := y.History(context.Background(), "NOPE", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("History error = %v, want ErrNotFound for a 404", err)
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

// onePixelPNG is a real, minimal PNG. It has to be real: the fetcher sniffs
// the bytes rather than trusting the content type, so a fake would be rejected
// for exactly the right reason and the test would prove nothing.
var onePixelPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

// logoServer answers a symbol search with whatever logoUrl is asked for, and
// serves images from /img/.
func logoServer(t *testing.T, logoFor map[string]bool, body []byte, contentType string) *httptest.Server {
	t.Helper()
	// Declared before it is built so the search response can point its logoUrl
	// at this same server — httptest only knows its address once it listens,
	// and nothing serves a request until after that.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/img/") {
			w.Header().Set("Content-Type", contentType)
			w.Write(body)
			return
		}
		symbol := strings.ToUpper(r.URL.Query().Get("q"))
		logo := ""
		if logoFor[symbol] {
			logo = `"logoUrl": "` + srv.URL + "/img/" + symbol + `.png",`
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"quotes":[{"symbol":"` + symbol + `",` + logo +
			`"shortname":"Test","exchDisp":"NMS","typeDisp":"Equity"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLogoFetchesTheImageBehindTheSearchResult(t *testing.T) {
	srv := logoServer(t, map[string]bool{"AAPL": true}, onePixelPNG, "image/png")

	y := NewYahoo(Settings{BaseURL: srv.URL})
	logo, err := y.Logo(context.Background(), "aapl", LogoValidators{})
	if err != nil {
		t.Fatalf("logo: %v", err)
	}
	if string(logo.Bytes) != string(onePixelPNG) {
		t.Error("the bytes returned are not the image the search result pointed at")
	}
	if logo.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png sniffed from the bytes", logo.ContentType)
	}
	if !strings.Contains(logo.Source, "/img/AAPL.png") {
		t.Errorf("source = %q, want the URL it was fetched from recorded", logo.Source)
	}
}

func TestLogoSaysSoWhenThereIsntOne(t *testing.T) {
	srv := logoServer(t, nil, onePixelPNG, "image/png")

	y := NewYahoo(Settings{BaseURL: srv.URL})
	if _, err := y.Logo(context.Background(), "GLD", LogoValidators{}); !errors.Is(err, ErrNoLogo) {
		t.Errorf("a search result without a logoUrl gave %v, want ErrNoLogo — "+
			"the caller caches that answer and a plain error would be retried forever", err)
	}
}

func TestLogoRejectsSomethingThatIsNotAnImage(t *testing.T) {
	srv := logoServer(t, map[string]bool{"AAPL": true}, []byte("<html>gotcha</html>"), "image/png")

	y := NewYahoo(Settings{BaseURL: srv.URL})
	_, err := y.Logo(context.Background(), "AAPL", LogoValidators{})
	if err == nil {
		t.Fatal("markup labelled image/png was accepted; it would then be served from this app's own origin")
	}
	// Recorded rather than retried: a URL answering with HTML will answer with
	// HTML again next cycle, and the message is the diagnosis a misconfigured
	// template needs — which only reaches the Settings page via a tombstone.
	if !errors.Is(err, ErrNoLogo) {
		t.Error("a source answering with markup is retried forever instead of being reported")
	}
	if !strings.Contains(err.Error(), "not an image") {
		t.Errorf("error was %q, want it to say what came back instead", err)
	}
}

func TestLogoTemplateGoesStraightToTheURL(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		w.Header().Set("Content-Type", "image/png")
		w.Write(onePixelPNG)
	}))
	defer srv.Close()

	y := NewYahoo(Settings{BaseURL: srv.URL, LogoURL: srv.URL + "/logos/{symbol_lower}.png"})
	logo, err := y.Logo(context.Background(), "AAPL", LogoValidators{})
	if err != nil {
		t.Fatalf("logo: %v", err)
	}
	if string(logo.Bytes) != string(onePixelPNG) {
		t.Error("the configured URL was not what got fetched")
	}
	if len(asked) != 1 || asked[0] != "/logos/aapl.png" {
		t.Errorf("requested %v, want a single /logos/aapl.png — a template must not also "+
			"cost a symbol search, and {symbol_lower} has to be lower-cased", asked)
	}
}

func TestExpandLogoURL(t *testing.T) {
	cases := []struct{ template, symbol, key, want string }{
		{"https://x.test/{symbol}.png", "aapl", "", "https://x.test/AAPL.png"},
		{"https://x.test/{symbol_lower}.png", "AAPL", "", "https://x.test/aapl.png"},
		{"https://x.test/i?t={symbol}", "BRK-B", "", "https://x.test/i?t=BRK-B"},
		// A slash in a symbol would otherwise address a different path.
		{"https://x.test/{symbol}.png", "A/B", "", "https://x.test/A%2FB.png"},
		{"https://x.test/{symbol}?token={key}", "AAPL", "pk_1", "https://x.test/AAPL?token=pk_1"},
		// The key lands in a query string, so it is escaped as one.
		{"https://x.test/{symbol}?token={key}", "AAPL", "a b&c", "https://x.test/AAPL?token=a+b%26c"},
	}
	for _, c := range cases {
		if got := ExpandLogoURL(c.template, c.symbol, c.key); got != c.want {
			t.Errorf("ExpandLogoURL(%q, %q, %q) = %q, want %q", c.template, c.symbol, c.key, got, c.want)
		}
	}
}

func TestLogoKeyIsSentAndNeverWrittenDown(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "image/png")
		w.Write(onePixelPNG)
	}))
	defer srv.Close()

	// No {key} in the template: the credential is a server-side one and belongs
	// in a header.
	y := NewYahoo(Settings{BaseURL: srv.URL, LogoURL: srv.URL + "/{symbol}.png", LogoKey: "sk_secret"})
	logo, err := y.Logo(context.Background(), "AAPL", LogoValidators{})
	if err != nil {
		t.Fatalf("logo: %v", err)
	}
	if auth != "Bearer sk_secret" {
		t.Errorf("Authorization = %q, want a bearer token — a key with nowhere to go in the "+
			"URL is a server-side credential", auth)
	}
	if strings.Contains(logo.Source, "sk_secret") {
		t.Errorf("the stored source %q carries the key; the cache is readable by anyone "+
			"who can open the Settings page", logo.Source)
	}
}

func TestLogoKeyInTheURLStaysOutOfTheHeaderAndTheRecord(t *testing.T) {
	var auth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		// 404, so the failure path is what gets inspected — that is where a
		// URL turns into a message somebody reads.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	y := NewYahoo(Settings{
		BaseURL: srv.URL,
		LogoURL: srv.URL + "/{symbol}.png?token={key}",
		LogoKey: "pk_public",
	})
	_, err := y.Logo(context.Background(), "AAPL", LogoValidators{})
	if !errors.Is(err, ErrNoLogo) {
		t.Fatalf("logo: %v, want ErrNoLogo for a 404", err)
	}
	if !strings.Contains(gotQuery, "pk_public") {
		t.Errorf("query was %q; a template with {key} in it has to carry the key", gotQuery)
	}
	if auth != "" {
		t.Errorf("Authorization = %q; the key was already in the URL and must not be sent twice", auth)
	}
	if strings.Contains(err.Error(), "pk_public") {
		t.Errorf("the error %q carries the key, and it ends up on the Settings page", err)
	}
}

func TestRedactLogoURL(t *testing.T) {
	cases := []struct{ src, key, want string }{
		{"https://x.test/A.png?token=pk_1", "pk_1", "https://x.test/A.png?token=%E2%80%A6"},
		// Pasted straight into the template, so this build never saw the value
		// as a key — the parameter name is the only clue and it is enough.
		{"https://x.test/A.png?token=pasted", "", "https://x.test/A.png?token=%E2%80%A6"},
		{"https://x.test/A.png?apikey=x&format=png", "", "https://x.test/A.png?apikey=%E2%80%A6&format=png"},
		{"https://x.test/A.png", "", "https://x.test/A.png"},
	}
	for _, c := range cases {
		if got := RedactLogoURL(c.src, c.key); got != c.want {
			t.Errorf("RedactLogoURL(%q, %q) = %q, want %q", c.src, c.key, got, c.want)
		}
	}
}

func TestLogoAsksWhetherItChanged(t *testing.T) {
	var seen struct {
		etag     string
		modified string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.etag = r.Header.Get("If-None-Match")
		seen.modified = r.Header.Get("If-Modified-Since")
		if seen.etag == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
		w.Header().Set("Content-Type", "image/png")
		w.Write(onePixelPNG)
	}))
	defer srv.Close()

	y := NewYahoo(Settings{BaseURL: srv.URL, LogoURL: srv.URL + "/{symbol}.png"})

	// First time: nothing to be conditional about, and the validators come back
	// for next time.
	logo, err := y.Logo(context.Background(), "AAPL", LogoValidators{})
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if logo.Validators.ETag != `"abc"` {
		t.Errorf("ETag came back as %q; without it every re-check re-downloads the image",
			logo.Validators.ETag)
	}
	if logo.Validators.LastModified == "" {
		t.Error("Last-Modified was dropped; it is the fallback for a source with no ETag")
	}

	// Second time: asked conditionally, and answered without a body.
	_, err = y.Logo(context.Background(), "AAPL", logo.Validators)
	if !errors.Is(err, ErrLogoUnchanged) {
		t.Fatalf("re-check returned %v, want ErrLogoUnchanged", err)
	}
	if seen.etag != `"abc"` {
		t.Errorf("If-None-Match was %q, want the stored ETag", seen.etag)
	}
	if seen.modified == "" {
		t.Error("If-Modified-Since was not sent, so a source with only a date cannot answer 304")
	}
}

func TestLogoStillFetchesWhenItHasChanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A source that has moved on: it ignores the old validator and sends
		// the new image with a new one.
		w.Header().Set("ETag", `"v2"`)
		w.Header().Set("Content-Type", "image/png")
		w.Write(onePixelPNG)
	}))
	defer srv.Close()

	y := NewYahoo(Settings{BaseURL: srv.URL, LogoURL: srv.URL + "/{symbol}.png"})
	logo, err := y.Logo(context.Background(), "AAPL", LogoValidators{ETag: `"v1"`})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(logo.Bytes) == 0 {
		t.Error("a changed logo came back empty")
	}
	if logo.Validators.ETag != `"v2"` {
		t.Errorf("ETag = %q, want the new one — keeping the old would re-ask with a stale "+
			"validator forever", logo.Validators.ETag)
	}
}

// ---------------------------------------------------------------------------
// Fund composition
// ---------------------------------------------------------------------------

// topHoldingsJSON is a trimmed /v10/finance/quoteSummary response with the two
// modules the fund page asks for. The weights are fractions, as Yahoo sends
// them, and one holding carries no symbol — a fund's cash line looks exactly
// like that and cannot be priced.
const topHoldingsJSON = `{
  "quoteSummary": {
    "result": [{
      "topHoldings": {
        "holdings": [
          { "symbol": "NVDA", "holdingName": "NVIDIA Corp", "holdingPercent": 0.0921 },
          { "symbol": "AAPL", "holdingName": "Apple Inc", "holdingPercent": 0.0814 },
          { "holdingName": "Cash and other", "holdingPercent": 0.0012 }
        ]
      },
      "quoteType": { "longName": "Invesco QQQ Trust, Series 1", "shortName": "Invesco QQQ Trust", "quoteType": "ETF" }
    }],
    "error": null
  }
}`

// wrappedHoldingsJSON is the same payload in the shape Yahoo uses when it
// ignores `formatted=false` — every number an object with a raw inside it.
const wrappedHoldingsJSON = `{
  "quoteSummary": {
    "result": [{
      "topHoldings": {
        "holdings": [
          { "symbol": "NVDA", "holdingName": "NVIDIA Corp", "holdingPercent": { "raw": 0.0921, "fmt": "9.21%" } },
          { "symbol": "AAPL", "holdingName": "Apple Inc", "holdingPercent": { "raw": 0.0814, "fmt": "8.14%" } }
        ]
      },
      "quoteType": { "longName": "Invesco QQQ Trust, Series 1", "quoteType": "ETF" }
    }],
    "error": null
  }
}`

// equityJSON is what a company comes back as: the module is there and empty.
const equityJSON = `{
  "quoteSummary": {
    "result": [{ "topHoldings": {}, "quoteType": { "longName": "Apple Inc", "quoteType": "EQUITY" } }],
    "error": null
  }
}`

// crumbServer is a stand-in for the crumbed half of Yahoo's API: it hands out a
// cookie, mints a crumb against it, and refuses anything presenting the wrong
// one. `issued` is what it will accept now, so a test can expire a crumb by
// changing it.
type crumbServer struct {
	mu         sync.Mutex
	issued     string
	body       string
	handshakes int
	rejected   int
}

func (c *crumbServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.URL.Path == "/":
			http.SetCookie(w, &http.Cookie{Name: "A1", Value: "session", Path: "/"})
			// Yahoo answers the cookie host with a 404 and the header that
			// matters. A provider that checked this status would never get past
			// it, which is exactly what this reproduces.
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/v1/test/getcrumb":
			if _, err := r.Cookie("A1"); err != nil {
				t.Errorf("the crumb was requested without the session cookie, so it would authenticate nothing")
			}
			c.handshakes++
			w.Write([]byte(c.issued))
		case strings.HasPrefix(r.URL.Path, "/v10/finance/quoteSummary/"):
			if r.URL.Query().Get("crumb") != c.issued {
				c.rejected++
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"finance":{"error":{"code":"Unauthorized","description":"Invalid Crumb"}}}`))
				return
			}
			w.Write([]byte(c.body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}
}

func TestConstituentsReadsTheHoldingsBlock(t *testing.T) {
	server := &crumbServer{issued: "abc123", body: topHoldingsJSON}
	y := newTestYahoo(t, server.handler(t))

	fund, err := y.Constituents(context.Background(), "qqq")
	if err != nil {
		t.Fatalf("reading a fund's holdings: %v", err)
	}

	if fund.Name != "Invesco QQQ Trust, Series 1" {
		t.Errorf("the fund's name came back as %q rather than its long name", fund.Name)
	}
	if len(fund.Holdings) != 2 {
		t.Fatalf("got %d holdings; the third has no symbol and cannot be priced, so it belongs nowhere but out", len(fund.Holdings))
	}
	if fund.Holdings[0].Symbol != "NVDA" || fund.Holdings[0].Name != "NVIDIA Corp" {
		t.Errorf("the first holding came back as %+v", fund.Holdings[0])
	}
	// The interface is in percent; Yahoo quotes a fraction. Getting this wrong
	// makes every fund look like it holds a hundredth of what it holds.
	if got := fund.Holdings[0].Weight; got < 9.20 || got > 9.22 {
		t.Errorf("NVDA's weight came back as %.4f; 0.0921 of the fund is 9.21%%", got)
	}
}

func TestConstituentsAcceptsWrappedNumbers(t *testing.T) {
	// Yahoo sends the same field bare or wrapped depending on a query parameter
	// it does not always honour. Both have to mean the same weight.
	server := &crumbServer{issued: "abc123", body: wrappedHoldingsJSON}
	y := newTestYahoo(t, server.handler(t))

	fund, err := y.Constituents(context.Background(), "QQQ")
	if err != nil {
		t.Fatalf("reading a fund's holdings: %v", err)
	}
	if got := fund.Holdings[0].Weight; got < 9.20 || got > 9.22 {
		t.Errorf("a wrapped {\"raw\":0.0921} came back as %.4f rather than 9.21%%", got)
	}
}

func TestConstituentsReHandshakesWhenTheCrumbExpires(t *testing.T) {
	// The failure this whole arrangement exists for. A crumb expires on the
	// source's schedule, and a page that failed until somebody reloaded it would
	// be worse than no page.
	server := &crumbServer{issued: "first", body: topHoldingsJSON}
	y := newTestYahoo(t, server.handler(t))

	if _, err := y.Constituents(context.Background(), "QQQ"); err != nil {
		t.Fatalf("the first lookup failed: %v", err)
	}

	server.mu.Lock()
	server.issued = "second"
	server.mu.Unlock()

	if _, err := y.Constituents(context.Background(), "QQQ"); err != nil {
		t.Fatalf("a lookup with a stale crumb failed instead of handshaking again: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if server.rejected != 1 {
		t.Errorf("the source rejected %d requests; exactly one should have been refused before the retry", server.rejected)
	}
	if server.handshakes != 2 {
		t.Errorf("the provider handshook %d times; one at the start and one after the rejection", server.handshakes)
	}
}

func TestConstituentsReusesOneCrumbAcrossLookups(t *testing.T) {
	server := &crumbServer{issued: "abc123", body: topHoldingsJSON}
	y := newTestYahoo(t, server.handler(t))

	for i := 0; i < 3; i++ {
		if _, err := y.Constituents(context.Background(), "QQQ"); err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if server.handshakes != 1 {
		t.Errorf("three lookups cost %d handshakes; a crumb is held for %v", server.handshakes, crumbTTL)
	}
}

func TestConstituentsSaysWhenSomethingIsNotAFund(t *testing.T) {
	server := &crumbServer{issued: "abc123", body: equityJSON}
	y := newTestYahoo(t, server.handler(t))

	_, err := y.Constituents(context.Background(), "AAPL")
	if !errors.Is(err, ErrNotFund) {
		t.Fatalf("a company came back as %v; it has to be ErrNotFund, which is a durable answer rather than a failure to retry", err)
	}
	if !strings.Contains(err.Error(), "equity") {
		t.Errorf("the message is %q; it should name what the source said the symbol was", err.Error())
	}
}

func TestConstituentsRejectsAnEmptySessionToken(t *testing.T) {
	// An empty 200 is one of the ways Yahoo refuses. Reported as itself, because
	// "HTTP 200" beside a broken feature helps nobody.
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/test/getcrumb" {
			w.Write([]byte("  \n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := y.Constituents(context.Background(), "QQQ")
	if err == nil {
		t.Fatal("an empty session token was accepted")
	}
	if !strings.Contains(err.Error(), "refusing API access") {
		t.Errorf("the message is %q, which does not say that the source refused", err.Error())
	}
}

func TestCrumbRequestAcceptsSomethingOtherThanJSON(t *testing.T) {
	// The crumb is served as text/plain. Asking for JSON only is answered by
	// Yahoo's edge with an HTTP 406 and no token — content negotiation working
	// exactly as specified, and invisible until the one endpoint on this API
	// that isn't JSON is the one you need.
	var accept string
	server := &crumbServer{issued: "abc123", body: topHoldingsJSON}
	inner := server.handler(t)
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/test/getcrumb" {
			accept = r.Header.Get("Accept")
			if !strings.Contains(accept, "*/*") {
				// Answer as Yahoo does, so the test fails the way production did.
				w.WriteHeader(http.StatusNotAcceptable)
				w.Write([]byte(`{"finance":{"error":{"code":"Not Acceptable","description":"HTTP 406 Not Acceptable"}}}`))
				return
			}
		}
		inner(w, r)
	})

	if _, err := y.Constituents(context.Background(), "QQQ"); err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	if !strings.Contains(accept, "*/*") {
		t.Errorf("the crumb was requested with Accept: %q, which excludes the text/plain it is served as", accept)
	}
}

func TestCrumbFailureNamesWhatToDoAboutIt(t *testing.T) {
	// A 406 beside a broken card tells an operator nothing. Which of the three
	// refusals it was decides what they change, so each one says.
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/test/getcrumb" {
			w.WriteHeader(http.StatusNotAcceptable)
			w.Write([]byte(`{"finance":{"error":{"description":"HTTP 406 Not Acceptable"}}}`))
			return
		}
		// No cookie, and no holdings without a crumb either.
		w.WriteHeader(http.StatusUnauthorized)
	})

	_, err := y.Constituents(context.Background(), "QQQ")
	if err == nil {
		t.Fatal("a refused handshake was not reported")
	}
	if !strings.Contains(err.Error(), "User-Agent") {
		t.Errorf("the message is %q; a 406 is about the headers, and saying so is the whole fix", err.Error())
	}
}

func TestConstituentsFallsBackToAnUncrumbedRequest(t *testing.T) {
	// The handshake is a means, not the goal. A deployment that refuses to mint
	// a token but answers the question anyway should not cost the feature.
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/test/getcrumb":
			w.WriteHeader(http.StatusNotAcceptable)
		case strings.HasPrefix(r.URL.Path, "/v10/finance/quoteSummary/"):
			if r.URL.Query().Has("crumb") {
				t.Errorf("the fallback request still carried a crumb, which is the thing that could not be got")
			}
			w.Write([]byte(topHoldingsJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	fund, err := y.Constituents(context.Background(), "QQQ")
	if err != nil {
		t.Fatalf("a source that needs no crumb was treated as unavailable: %v", err)
	}
	if len(fund.Holdings) != 2 {
		t.Errorf("got %d holdings from the uncrumbed request", len(fund.Holdings))
	}
}

func TestCookieIsCollectedBeforeTheCrumb(t *testing.T) {
	// A cookie the API host would never be sent is not a handshake — it is a
	// successful-looking prelude to a 401.
	server := &crumbServer{issued: "abc123", body: topHoldingsJSON}
	y := newTestYahoo(t, server.handler(t))

	if _, err := y.Constituents(context.Background(), "QQQ"); err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	if !y.hasSessionCookie(y.Effective().BaseURL) {
		t.Error("no session cookie is held for the API host after a successful handshake")
	}
}
