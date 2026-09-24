package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

func TestChartSettlesIndicatorsBeforeTheFirstBarShown(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	if _, err := eng.Chart(archive.Query{Symbol: "VTI", Interval: quotes.Daily}, nil); !errors.Is(err, ErrNoArchive) {
		t.Fatalf("a chart with no archive gave %v, want ErrNoArchive", err)
	}

	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	// A year of weekday closes rising by one a day.
	var closes []float64
	for i := 0; i < 260; i++ {
		closes = append(closes, float64(100+i))
	}
	days := archiveDaily(t, a, "VTI", closes, nil)

	from := days[200]
	specs, _ := indicators.ParseList("sma:50,rsi")
	c, err := eng.Chart(archive.Query{Symbol: "VTI", Interval: quotes.Daily, From: from, To: days[259].Add(24 * time.Hour)}, specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Bars) != 60 || !c.Bars[0].Time.Equal(from) {
		t.Fatalf("got %d bars from %s, want the 60 in the window, from %s", len(c.Bars), c.Bars[0].Time, from)
	}
	sma := c.Indicators[0].Lines[0].Values
	if len(sma) != 60 || sma[0] == nil {
		t.Fatalf("SMA 50 has %d values, the first %v — it must be settled at the first bar shown", len(sma), sma[0])
	}
	// SMA 50 of a series rising by one: the close 24.5 bars back.
	if got, want := *sma[0], closes[200]-24.5; got != want {
		t.Errorf("SMA 50 at the first bar = %v, want %v", got, want)
	}
	if rsi := c.Indicators[1].Lines[0].Values[0]; rsi == nil || *rsi != 100 {
		t.Errorf("RSI of a series that only rises = %v, want 100 from the first bar", rsi)
	}

	// Asked for more lead-in than the archive has: undefined, never short.
	early, _ := eng.Chart(archive.Query{Symbol: "VTI", Interval: quotes.Daily, From: days[10], To: days[40]}, specs)
	if v := early.Indicators[0].Lines[0].Values[0]; v != nil {
		t.Errorf("SMA 50 ten bars into the series = %v, want undefined rather than averaged over ten", *v)
	}
}

func TestLookbackReachesFarEnough(t *testing.T) {
	// 200 daily bars are ~40 weeks of trading days.
	if d := lookback(quotes.Daily, 200, false); d < 280*24*time.Hour {
		t.Errorf("200 daily bars look back %s, too short to cover the weekends", d)
	}
	// 390 one-minute bars are one regular session.
	if d := lookback(quotes.OneMinute, 390, false); d < 24*time.Hour || d > 7*24*time.Hour {
		t.Errorf("one session of minute bars looks back %s", d)
	}
	if lookback(quotes.Daily, 0, false) != 0 {
		t.Error("no indicator asked for a lookback")
	}
}
