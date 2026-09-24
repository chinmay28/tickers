package strategy

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

var t0 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// ohlc builds daily bars from [open, high, low, close] quadruples.
func ohlc(rows ...[4]float64) []quotes.Candle {
	out := make([]quotes.Candle, len(rows))
	for i, r := range rows {
		out[i] = quotes.Candle{Time: t0.AddDate(0, 0, i), Open: r[0], High: r[1], Low: r[2], Close: r[3], Volume: 100}
	}
	return out
}

// flat is a bar that opens and closes at p with no range.
func flat(p float64) [4]float64 { return [4]float64{p, p, p, p} }

func plan(t *testing.T, d Definition) Plan {
	t.Helper()
	if d.Symbol == "" {
		d.Symbol = "TEST"
	}
	if d.From == "" {
		d.From = "2024-01-01"
	}
	if d.Initial == 0 {
		d.Initial = 1000
	}
	p, err := Compile(d, t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

func rule(conds ...Condition) Rule { return Rule{Match: "all", Conditions: conds} }

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestASignalOnACloseFillsAtTheNextOpen(t *testing.T) {
	// Close goes above 10 on bar 1; the entry fills at bar 2's open (11), not
	// bar 1's close. Close goes back below 10 on bar 3; the exit fills at
	// bar 4's open (9).
	bars := ohlc(flat(9), [4]float64{9, 10.5, 9, 10.5}, [4]float64{11, 12, 11, 12}, [4]float64{12, 12, 9.5, 9.5}, flat(9), flat(9))
	p := plan(t, Definition{
		Entry: rule(Condition{"close", ">", "10"}),
		Exit:  rule(Condition{"close", "<", "10"}),
	})
	r := Simulate(p, bars, 0)
	if len(r.Trades) != 1 {
		t.Fatalf("%d trades, want 1: %+v", len(r.Trades), r.Trades)
	}
	tr := r.Trades[0]
	if !tr.EntryTime.Equal(bars[2].Time) || tr.EntryPrice != 11 {
		t.Errorf("entry %s at %v, want bar 2's open 11 — filling at the signal's own close would trade on a price nobody could act on", tr.EntryTime, tr.EntryPrice)
	}
	if !tr.ExitTime.Equal(bars[4].Time) || tr.ExitPrice != 9 || tr.Reason != ReasonRule {
		t.Errorf("exit %s at %v (%s), want bar 4's open 9 by rule", tr.ExitTime, tr.ExitPrice, tr.Reason)
	}
	near(t, "trade return", tr.Return, (9.0/11-1)*100)
	near(t, "final equity", r.Strategy.Final, 1000*9.0/11)
	// Buy and hold from the first bar's open, 9, to the last close, 9.
	near(t, "buy and hold", r.Hold.TotalReturn, 0)
}

func TestFeesAreChargedBothWays(t *testing.T) {
	bars := ohlc(flat(10), flat(10), flat(10), flat(10))
	p := plan(t, Definition{
		Entry:      rule(Condition{"close", ">=", "10"}),
		Exit:       rule(Condition{"close", ">=", "10"}),
		FeePercent: 1,
	})
	r := Simulate(p, bars, 0)
	// In at bar 1's open, out at bar 2's open, flat prices: 1000 × 0.99 × 0.99.
	near(t, "the first round trip's return", r.Trades[0].Return, (0.99*0.99-1)*100)
}

func TestAStopGappedThroughFillsAtTheOpen(t *testing.T) {
	// In at 100; a 5% stop sits at 95; bar 2 opens at 90.
	bars := ohlc(flat(100), flat(100), [4]float64{90, 91, 85, 88}, flat(88))
	p := plan(t, Definition{Entry: rule(Condition{"close", ">=", "100"}), StopLoss: 5})
	r := Simulate(p, bars, 0)
	tr := r.Trades[0]
	if tr.Reason != ReasonStop || tr.ExitPrice != 90 {
		t.Errorf("exit = %v by %s, want 90 by stop — the market was already past 95 when it opened", tr.ExitPrice, tr.Reason)
	}
}

func TestAStopInsideTheBarFillsAtTheStopAndBeatsTheTarget(t *testing.T) {
	// In at 100; stop 95, target 110; bar 2 touches both.
	bars := ohlc(flat(100), flat(100), [4]float64{100, 112, 94, 105}, flat(105))
	p := plan(t, Definition{Entry: rule(Condition{"close", ">=", "100"}), StopLoss: 5, TakeProfit: 10})
	tr := Simulate(p, bars, 0).Trades[0]
	if tr.Reason != ReasonStop || tr.ExitPrice != 95 {
		t.Errorf("exit = %v by %s, want 95 by stop — with only OHLC the pessimistic order is the honest one", tr.ExitPrice, tr.Reason)
	}

	bars = ohlc(flat(100), flat(100), [4]float64{100, 112, 99, 105}, flat(105))
	tr = Simulate(p, bars, 0).Trades[0]
	if tr.Reason != ReasonTarget || math.Abs(tr.ExitPrice-110) > 1e-9 {
		t.Errorf("exit = %v by %s, want 110 by target", tr.ExitPrice, tr.Reason)
	}
}

func TestACrossHoldsOnlyOnTheBarItHappens(t *testing.T) {
	// SMA 2 against a constant: it crosses above 10 on bar 2 only.
	bars := ohlc(flat(8), flat(9), flat(12), flat(13), flat(14), flat(15))
	p := plan(t, Definition{Entry: rule(Condition{"sma:2", "crosses_above", "10"})})
	r := Simulate(p, bars, 0)
	if len(r.Trades) != 1 || !r.Trades[0].EntryTime.Equal(bars[3].Time) {
		t.Fatalf("trades = %+v, want one entry at bar 3, the open after the cross", r.Trades)
	}
	if r.Trades[0].Reason != ReasonOpen {
		t.Errorf("with no exit rule the trade should still be open at the end, got %s", r.Trades[0].Reason)
	}
	if r.Stats.Exposure <= 0 || r.Stats.Exposure >= 100 {
		t.Errorf("exposure = %v", r.Stats.Exposure)
	}
}

func TestWarmUpBarsAreComputedOverButNotTraded(t *testing.T) {
	bars := ohlc(flat(1), flat(2), flat(3), flat(4), flat(5), flat(6))
	p := plan(t, Definition{Entry: rule(Condition{"close", ">", "0"})})
	r := Simulate(p, bars, 3)
	if !r.From.Equal(bars[3].Time) || r.Bars != 3 {
		t.Errorf("tested %s over %d bars, want from bar 3 over 3", r.From, r.Bars)
	}
	if !r.Trades[0].EntryTime.Equal(bars[4].Time) {
		t.Errorf("first entry %s, want bar 4 — nothing is traded in the warm-up", r.Trades[0].EntryTime)
	}
}

func TestMetricsFromAKnownCurve(t *testing.T) {
	m := metrics(100, []float64{110, 99, 121}, t0, t0.AddDate(2, 0, 0), 252)
	near(t, "total return", m.TotalReturn, 21)
	near(t, "max drawdown", m.MaxDrawdown, (99.0/110-1)*100)
	// Two calendar years from 2024 hold a leap day, so a hair under 10%.
	if math.Abs(m.CAGR-10) > 0.05 {
		t.Errorf("CAGR over two years = %v, want about 10", m.CAGR)
	}
	if m.Sharpe <= 0 {
		t.Errorf("Sharpe = %v for a curve that ended up", m.Sharpe)
	}
	flatM := metrics(100, []float64{100, 100}, t0, t0.AddDate(1, 0, 0), 252)
	if flatM.Sharpe != 0 {
		t.Errorf("a curve that never moved has Sharpe %v, want 0 rather than a division by nothing", flatM.Sharpe)
	}
}

func TestStatsCountWinsAndProfitFactor(t *testing.T) {
	s := stats([]Trade{{Return: 10, Bars: 2}, {Return: -5, Bars: 4}, {Return: 20, Bars: 6}}, 12, 20)
	near(t, "win rate", s.WinRate, 200.0/3)
	near(t, "profit factor", *s.ProfitFactor, 6)
	near(t, "exposure", s.Exposure, 60)
	near(t, "average bars", s.AvgBars, 4)
	if nl := stats([]Trade{{Return: 1}}, 1, 1); nl.ProfitFactor != nil {
		t.Error("a record with no losses has a profit factor; there is nothing to divide by")
	}
}

func TestCompileRefusesWhatCannotRun(t *testing.T) {
	ok := Definition{Symbol: "AAPL", From: "2020-01-01", Entry: rule(Condition{"close", ">", "sma:200"})}
	bad := map[string]func(d *Definition){
		"a symbol is required":        func(d *Definition) { d.Symbol = " " },
		"start date":                  func(d *Definition) { d.From = "yesterday" },
		"before the end":              func(d *Definition) { d.To = "2019-01-01" },
		"at least one entry":          func(d *Definition) { d.Entry = Rule{} },
		"unknown comparison":          func(d *Definition) { d.Entry = rule(Condition{"close", "=>", "1"}) },
		"always true or always false": func(d *Definition) { d.Entry = rule(Condition{"1", ">", "2"}) },
		"no line":                     func(d *Definition) { d.Entry = rule(Condition{"macd.nope", ">", "0"}) },
		"not a price field":           func(d *Definition) { d.Entry = rule(Condition{"kama:10", ">", "0"}) },
		"can never trigger":           func(d *Definition) { d.StopLoss = 100 },
		"fee":                         func(d *Definition) { d.FeePercent = 50 },
		"\"all\" or \"any\"":          func(d *Definition) { d.Exit = Rule{Match: "most"} },
	}
	for want, mutate := range bad {
		d := ok
		mutate(&d)
		if _, err := Compile(d, t0); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("mutation %q: err = %v", want, err)
		}
	}
	p, err := Compile(Definition{Symbol: "aapl", From: "2020-01-01",
		Entry: rule(Condition{"macd.signal", "crosses_below", "macd"}, Condition{"bb:20:2.lower", ">", "close"})}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Def.Symbol != "AAPL" || len(p.Specs) != 2 || p.Def.Initial != 10000 || p.Interval != quotes.Daily {
		t.Errorf("plan = %+v, want the symbol upper-cased, two indicators, 10,000 and daily bars by default", p.Def)
	}
}

func TestOverlaysAreThePriceIndicatorsTheRulesUse(t *testing.T) {
	var rows [][4]float64
	for i := 0; i < 30; i++ {
		rows = append(rows, flat(float64(100+i)))
	}
	p := plan(t, Definition{Entry: rule(Condition{"close", ">", "bb:5:2.upper"}, Condition{"rsi:5", ">", "50"})})
	r := Simulate(p, ohlc(rows...), 10)
	if len(r.Lines) != 3 {
		t.Fatalf("lines = %d, want Bollinger's three bands and not RSI", len(r.Lines))
	}
	if r.Lines[0].Label != "Bollinger 5, 2 lower" || len(r.Lines[0].Values) != len(r.Prices) {
		t.Errorf("first line = %s with %d values against %d prices", r.Lines[0].Label, len(r.Lines[0].Values), len(r.Prices))
	}
}
