package quotes

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// candlesJSON is a daily response with everything the archive reads: the full
// OHLCV block, a bar with a null (a halt), a bar with no volume, a split and a
// dividend in the events block, and a first trade date.
//
// The timestamps are 13:30 UTC — a US session's open — and the offset is New
// York's in summer, so each lands on its own calendar day.
const candlesJSON = `{
  "chart": {
    "result": [{
      "meta": { "symbol": "AAPL", "gmtoffset": -14400, "firstTradeDate": 345479400 },
      "timestamp": [1598621400, 1598880600, 1598967000, 1599053400],
      "indicators": {
        "quote": [{
          "open":   [504.05, 127.58, 132.76, 137.59],
          "high":   [505.77, 131.00, 134.80, 137.98],
          "low":    [498.31, 126.00, 130.53, 127.00],
          "close":  [499.23, 129.04, 134.18, null],
          "volume": [46907479, 225702700, null, 200119000]
        }]
      },
      "events": {
        "splits": { "1598880600": { "date": 1598880600, "numerator": 4, "denominator": 1, "splitRatio": "4:1" } },
        "dividends": {
          "1596807000": { "amount": 0.82, "date": 1596807000 },
          "1597000000": { "amount": 0, "date": 1597000000 }
        }
      }
    }],
    "error": null
  }
}`

func TestCandlesReadTheWholeBarAndTheEvents(t *testing.T) {
	var query url.Values
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		w.Write([]byte(candlesJSON))
	})

	from := time.Date(2020, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2020, 9, 3, 0, 0, 0, 0, time.UTC)
	got, err := y.Candles(context.Background(), " aapl ", Daily, from, to)
	if err != nil {
		t.Fatalf("candles: %v", err)
	}

	if query.Get("period1") != strconv.FormatInt(from.Unix(), 10) || query.Get("period2") != strconv.FormatInt(to.Unix(), 10) {
		t.Errorf("window = %s–%s, want the explicit epochs asked for", query.Get("period1"), query.Get("period2"))
	}
	if query.Get("interval") != "1d" {
		t.Errorf("interval = %q, want 1d", query.Get("interval"))
	}
	// Without the split in the same response, the archive cannot tell which of
	// its stored rows are on the old basis.
	if query.Get("events") != "div,splits" {
		t.Errorf("events = %q, want div,splits on every request", query.Get("events"))
	}

	if len(got.Candles) != 3 {
		t.Fatalf("got %d candles, want 3 — the bar with a null close must be dropped, not filled in", len(got.Candles))
	}
	first := got.Candles[0]
	if first.Open != 504.05 || first.High != 505.77 || first.Low != 498.31 || first.Close != 499.23 || first.Volume != 46907479 {
		t.Errorf("first candle = %+v, want the OHLCV exactly as printed", first)
	}
	if want := time.Date(2020, 8, 28, 0, 0, 0, 0, time.UTC); !first.Time.Equal(want) {
		t.Errorf("daily candle keyed at %s, want the exchange's date %s at midnight UTC", first.Time, want)
	}
	if got.Candles[2].Volume != 0 {
		t.Errorf("a null volume read back as %d, want 0", got.Candles[2].Volume)
	}

	if len(got.Splits) != 1 {
		t.Fatalf("got %d splits, want 1", len(got.Splits))
	}
	split := got.Splits[0]
	if want := time.Date(2020, 8, 31, 0, 0, 0, 0, time.UTC); !split.Time.Equal(want) {
		t.Errorf("split dated %s, want the ex-date %s — keyed like a daily bar so the ex-date's own bar is not rescaled", split.Time, want)
	}
	if split.Ratio() != 0.25 {
		t.Errorf("a 4:1 split has ratio %v, want 0.25", split.Ratio())
	}

	if len(got.Dividends) != 1 || got.Dividends[0].Amount != 0.82 {
		t.Errorf("dividends = %+v, want the one non-zero payout", got.Dividends)
	}
	if want := time.Date(1980, 12, 12, 0, 0, 0, 0, time.UTC); !got.FirstTrade.Equal(want) {
		t.Errorf("first trade = %s, want %s", got.FirstTrade, want)
	}
}

func TestIntradayCandlesKeepTheirInstant(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"chart":{"result":[{"meta":{"gmtoffset":-14400},
		  "timestamp":[1599053700, 1599053400],
		  "indicators":{"quote":[{"open":[2,1],"high":[2,1],"low":[2,1],"close":[2,1],"volume":[20,10]}]}}],"error":null}}`))
	})
	now := time.Now()
	got, err := y.Candles(context.Background(), "AAPL", FiveMinute, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("candles: %v", err)
	}
	if len(got.Candles) != 2 {
		t.Fatalf("got %d candles, want 2", len(got.Candles))
	}
	if !got.Candles[0].Time.Equal(time.Unix(1599053400, 0)) || got.Candles[0].Close != 1 {
		t.Errorf("first candle = %+v, want the earlier bar at its own instant — out-of-order input must come back sorted", got.Candles[0])
	}
	if !got.FirstTrade.IsZero() {
		t.Errorf("first trade = %s, want zero when the provider didn't say", got.FirstTrade)
	}
}

func TestCandlesRefuseAWindowTheyCannotAsk(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an invalid request reached the network")
	})
	now := time.Now()
	if _, err := y.Candles(context.Background(), "AAPL", Daily, now, now); err == nil {
		t.Error("an empty window was accepted")
	}
	if _, err := y.Candles(context.Background(), "AAPL", Interval("2m"), now.Add(-time.Hour), now); err == nil {
		t.Error("an unknown interval was accepted")
	}
	if _, err := y.Candles(context.Background(), "  ", Daily, now.Add(-time.Hour), now); err == nil {
		t.Error("a blank symbol was accepted")
	}
}

func TestA429IsRecognisableAsRateLimiting(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	})
	now := time.Now()
	_, err := y.Candles(context.Background(), "AAPL", Daily, now.Add(-time.Hour), now)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a 429 came back as %v, want ErrRateLimited so the collector waits instead of giving up on the symbol", err)
	}
}

func TestOtherFailuresAreNotRateLimiting(t *testing.T) {
	y := newTestYahoo(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	now := time.Now()
	_, err := y.Candles(context.Background(), "AAPL", Daily, now.Add(-time.Hour), now)
	if err == nil || errors.Is(err, ErrRateLimited) {
		t.Fatalf("a 502 came back as %v, want an ordinary failure", err)
	}
}

func TestParseIntervalRefusesTypos(t *testing.T) {
	for _, s := range []string{"1d", "1h", "5m"} {
		if i, err := ParseInterval(s); err != nil || string(i) != s {
			t.Errorf("ParseInterval(%q) = %q, %v", s, i, err)
		}
	}
	if _, err := ParseInterval("1m"); err == nil {
		t.Error("an interval the archive has no policy for was accepted")
	}
}
