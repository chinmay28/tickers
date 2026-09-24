package indicators

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.IsNaN(want) != math.IsNaN(got) || !math.IsNaN(want) && math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestSMAAndEMAMatchHandWorkedValues(t *testing.T) {
	x := []float64{1, 2, 3, 4, 5}
	sma := SMA(x, 3)
	near(t, "SMA[1]", sma[1], math.NaN())
	near(t, "SMA[2]", sma[2], 2)
	near(t, "SMA[4]", sma[4], 4)

	// Seeded with the SMA of the first three (2), then α = 0.5:
	// 2 + 0.5×(4−2) = 3, 3 + 0.5×(5−3) = 4.
	ema := EMA(x, 3)
	near(t, "EMA[1]", ema[1], math.NaN())
	near(t, "EMA[2]", ema[2], 2)
	near(t, "EMA[3]", ema[3], 3)
	near(t, "EMA[4]", ema[4], 4)
}

func TestSMARestartsAcrossAGap(t *testing.T) {
	x := []float64{1, 2, math.NaN(), 4, 5, 6}
	got := SMA(x, 2)
	near(t, "the first value after the gap", got[3], math.NaN())
	near(t, "the second", got[4], 4.5)
}

func TestRSIAtItsEdgesAndInBalance(t *testing.T) {
	up := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	near(t, "RSI of a series that only rises", RSI(up, 3)[7], 100)
	down := []float64{8, 7, 6, 5, 4, 3, 2, 1}
	near(t, "RSI of a series that only falls", RSI(down, 3)[7], 0)
	flat := []float64{5, 5, 5, 5, 5}
	near(t, "RSI of a series that never moves", RSI(flat, 3)[4], 50)
	// Equal moves up and down: gains and losses average the same.
	zig := []float64{10, 11, 10, 11, 10, 11, 10}
	r := RSI(zig, 2)
	if r[6] <= 30 || r[6] >= 70 {
		t.Errorf("RSI of an even zig-zag = %v, want mid-range", r[6])
	}
	near(t, "RSI before n moves", RSI(up, 3)[2], math.NaN())
}

func TestMACDIsTheDifferenceOfItsEMAs(t *testing.T) {
	var x []float64
	for i := 0; i < 60; i++ {
		x = append(x, 100+10*math.Sin(float64(i)/5))
	}
	m, s, h := MACD(x, 12, 26, 9)
	f, sl := EMA(x, 12), EMA(x, 26)
	near(t, "MACD[40]", m[40], f[40]-sl[40])
	near(t, "histogram[50]", h[50], m[50]-s[50])
	near(t, "MACD before the slow EMA exists", m[24], math.NaN())
	near(t, "signal before nine MACD values", s[25+7], math.NaN())
}

func TestBollingerBandsAreSymmetricAndCollapseOnAFlatSeries(t *testing.T) {
	x := []float64{1, 2, 3, 4, 5}
	mid, up, lo := Bollinger(x, 5, 2)
	near(t, "middle", mid[4], 3)
	// Population standard deviation of 1..5 is √2.
	near(t, "upper", up[4], 3+2*math.Sqrt2)
	near(t, "lower", lo[4], 3-2*math.Sqrt2)
	_, fu, fl := Bollinger([]float64{7, 7, 7}, 3, 2)
	near(t, "a flat series' band width", fu[2]-fl[2], 0)
}

func TestATRCountsGapsFromThePreviousClose(t *testing.T) {
	high := []float64{11, 11, 21}
	low := []float64{9, 9, 19}
	close := []float64{10, 10, 20}
	atr := ATR(high, low, close, 1)
	near(t, "an ordinary bar's range", atr[1], 2)
	// Opens 9 above the last close: the true range is 21 − 10 = 11.
	near(t, "a gapping bar's true range", atr[2], 11)
}

func TestStochasticPlacesTheCloseInItsRange(t *testing.T) {
	high := []float64{10, 12, 14}
	low := []float64{8, 9, 10}
	k, d := Stochastic(high, low, []float64{9, 12, 14}, 3, 2)
	near(t, "%K closing on the high", k[2], 100)
	near(t, "%D before two %K values", d[2], math.NaN())
	k2, _ := Stochastic([]float64{5, 5}, []float64{5, 5}, []float64{5, 5}, 2, 1)
	near(t, "%K of a bar range of nothing", k2[1], 50)
}

func TestOBVAddsUpDaysAndSubtractsDownDays(t *testing.T) {
	obv := OBV([]float64{10, 11, 11, 9}, []float64{100, 50, 70, 20})
	want := []float64{0, 50, 50, 30}
	for i := range want {
		near(t, "OBV", obv[i], want[i])
	}
}

func TestVWAPResetsEachDayAndPrefersTheSourcesOwn(t *testing.T) {
	high := []float64{11, 13, 21}
	low := []float64{9, 11, 19}
	close := []float64{10, 12, 20}
	vol := []float64{100, 300, 10}
	day := []int64{1, 1, 2}
	got := VWAP(high, low, close, vol, nil, day)
	near(t, "first bar", got[0], 10)
	near(t, "running, by volume", got[1], (10*100+12*300)/400.0)
	near(t, "a new day starts again", got[2], 20)

	withSource := VWAP(high, low, close, vol, []float64{10.5, 0, 0}, day)
	near(t, "the source's VWAP used where it gave one", withSource[0], 10.5)
}

func TestParseFillsDefaultsAndRefusesNonsense(t *testing.T) {
	s, err := Parse(" MACD ")
	if err != nil || s.Key() != "macd:12:26:9" || s.Label() != "MACD 12, 26, 9" {
		t.Errorf("Parse(MACD) = %+v, %v", s, err)
	}
	s, _ = Parse("bb:50")
	if s.Key() != "bb:50:2" {
		t.Errorf("bb:50 = %s, want the band width defaulted", s.Key())
	}
	bad := map[string]string{
		"sma:0":      "whole number",
		"sma:2.5":    "whole number",
		"sma:5000":   "1000",
		"sma:x":      "not a number",
		"rsi:14:3":   "at most",
		"macd:26:12": "fast period",
		"bb:20:0":    "band width",
		"kama":       "unknown indicator",
	}
	for raw, want := range bad {
		if _, err := Parse(raw); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want an error mentioning %q", raw, err, want)
		}
	}
	list, err := ParseList("sma:20, ema, sma:20,,rsi")
	if err != nil || len(list) != 3 {
		t.Errorf("ParseList = %v, %v; want three, deduplicated", list, err)
	}
	if _, err := ParseList(strings.Repeat("sma:1,", 0) + "sma:1,sma:2,sma:3,sma:4,sma:5,sma:6,sma:7,sma:8,sma:9,sma:10,sma:11,sma:12,sma:13"); err == nil {
		t.Error("thirteen indicators were accepted")
	}
}

func TestComputeAndTrimKeepTheLinesAlignedWithTheBars(t *testing.T) {
	var candles []quotes.Candle
	start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		p := float64(100 + i)
		candles = append(candles, quotes.Candle{Time: start.AddDate(0, 0, i), Open: p, High: p + 1, Low: p - 1, Close: p, Volume: 10})
	}
	specs, _ := ParseList("sma:5,macd,rsi")
	results := Trim(Compute(specs, candles), 10)
	for _, r := range results {
		for _, l := range r.Lines {
			if len(l.Values) != 20 {
				t.Fatalf("%s %s has %d values after trimming 10 of 30, want 20", r.Key, l.Name, len(l.Values))
			}
		}
	}
	if results[0].Pane != PanePrice || results[2].Pane != PaneLower || results[2].Bounds == nil {
		t.Errorf("panes = %s/%s, bounds %v", results[0].Pane, results[2].Pane, results[2].Bounds)
	}
	if v := results[0].Lines[0].Values[0]; v == nil || *v != 108 {
		t.Errorf("SMA 5 at bar 10 = %v, want 108 — (106+…+110)/5", v)
	}
	if specs[1].Warmup() != 3*26+9 {
		t.Errorf("MACD warm-up = %d", specs[1].Warmup())
	}
}
