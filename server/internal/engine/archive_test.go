package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// openArchive is an ArchiveReader over a real, temporary archive.
type openArchive struct{ a *archive.Archive }

func (o openArchive) Read(fn func(a *archive.Archive) error) error { return fn(o.a) }

func newTestArchive(t *testing.T) *archive.Archive {
	t.Helper()
	root := t.TempDir()
	if err := archive.Init(root); err != nil {
		t.Fatal(err)
	}
	a, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// archiveDaily stores a daily series for symbol ending yesterday, with a
// cursor that says it is complete and current.
func archiveDaily(t *testing.T, a *archive.Archive, symbol string, closes []float64, dividends []quotes.Dividend) []time.Time {
	t.Helper()
	if err := a.Add(archive.User, archive.Entry{Symbol: symbol}, time.Now()); err != nil {
		t.Fatal(err)
	}
	s, _ := a.Lookup(symbol)
	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	var days []time.Time
	var candles []quotes.Candle
	for i := range closes {
		d := end.AddDate(0, 0, i-len(closes)+1)
		days = append(days, d)
		c := closes[i]
		candles = append(candles, quotes.Candle{Time: d, Open: c, High: c, Low: c, Close: c})
	}
	cur := archive.Cursor{Oldest: days[0].AddDate(-1, 0, 0), Newest: time.Now(), Complete: true}
	if err := a.Record(archive.Batch{SymbolID: s.ID, Interval: quotes.Daily, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: candles, Dividends: dividends}, Cursor: &cur}); err != nil {
		t.Fatal(err)
	}
	return days
}

func TestAdjustedBarsScaleEveryCloseBeforeAPayout(t *testing.T) {
	raw := map[string]float64{"2024-01-01": 100, "2024-01-02": 100, "2024-01-03": 98, "2024-01-04": 99}
	divs := []quotes.Dividend{{Time: time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC), Amount: 2}}
	got := adjustedBars(raw, divs)
	if len(got) != 4 || got[0].Date != "2024-01-01" {
		t.Fatalf("bars = %+v, want four, oldest first", got)
	}
	// A $2 payout off a $100 close: everything before the ex-date is scaled
	// by 0.98, and the ex-date and after are untouched.
	want := []float64{98, 98, 98, 99}
	for i, b := range got {
		if math.Abs(b.Close-want[i]) > 1e-9 {
			t.Fatalf("adjusted closes = %v, want %v", closesOfBars(got), want)
		}
	}
	if got[0].Raw != 100 {
		t.Errorf("raw close = %v, want the price actually printed", got[0].Raw)
	}
}

func closesOfBars(bars []quotes.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

func TestHistoryComesFromTheArchiveWhenItHoldsTheWholeSeries(t *testing.T) {
	h := newFakeHistorian(map[string]float64{"VTI": 300})
	eng, _ := newTestEngine(t, h)
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	days := archiveDaily(t, a, "VTI", []float64{100, 101, 102}, nil)

	// The provider's top-up has today's bar, and a revised yesterday.
	today := time.Now().UTC().Format(time.DateOnly)
	h.bars["VTI"] = []quotes.Bar{{Date: days[2].Format(time.DateOnly), Close: 102.5, Raw: 102.5}, {Date: today, Close: 103, Raw: 103}}

	bars, err := eng.symbolHistory(context.Background(), h, "VTI", historyStart())
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 4 || bars[0].Close != 100 || bars[2].Close != 102.5 || bars[3].Date != today {
		t.Errorf("bars = %+v, want the archive's series topped up with the provider's last sessions", bars)
	}

	// A symbol the archive has only half walked is fetched whole instead.
	h.bars["GLD"] = series("2020-01-01", 1.0)
	a.Add(archive.User, archive.Entry{Symbol: "GLD"}, time.Now())
	g, _ := a.Lookup("GLD")
	half := archive.Cursor{Oldest: time.Now().AddDate(-1, 0, 0), Newest: time.Now()}
	a.Record(archive.Batch{SymbolID: g.ID, Interval: quotes.Daily, Source: "yahoo", Cursor: &half,
		Series: quotes.CandleSeries{Candles: []quotes.Candle{{Time: days[0], Open: 5, High: 5, Low: 5, Close: 5}}}})
	gld, _ := eng.symbolHistory(context.Background(), h, "GLD", historyStart())
	if len(gld) != 1 || gld[0].Date != "2020-01-01" {
		t.Errorf("GLD = %+v, want the provider's full series — a half-backfilled archive would truncate 'all time'", gld)
	}
}

func TestDividendsComeFromTheArchive(t *testing.T) {
	h := newFakeHistorian(nil)
	eng, _ := newTestEngine(t, h)
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	archiveDaily(t, a, "VTI", []float64{100, 101}, []quotes.Dividend{{Time: time.Now().AddDate(0, 0, -1).Truncate(24 * time.Hour), Amount: 0.5}})
	got, ok := eng.archivedDividends("VTI")
	if !ok || len(got) != 1 || got[0].Amount != 0.5 {
		t.Errorf("dividends = %+v (ok %v), want the archive's payout", got, ok)
	}
	if _, ok := eng.archivedDividends("NOPE"); ok {
		t.Error("an unarchived symbol claimed archived dividends")
	}
}

func TestSparklineUsesArchivedBarsThenLiveReadings(t *testing.T) {
	eng, st := newTestEngine(t, &fakeProvider{})
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	a.Add(archive.User, archive.Entry{Symbol: "VTI"}, time.Now())
	s, _ := a.Lookup("VTI")

	start := time.Now().Add(-2 * time.Hour).Truncate(time.Minute)
	var bars []quotes.Candle
	for i := 0; i < 30; i++ {
		p := 100 + float64(i)
		bars = append(bars, quotes.Candle{Time: start.Add(time.Duration(i) * time.Minute), Open: p, High: p, Low: p, Close: p})
	}
	a.Record(archive.Batch{SymbolID: s.ID, Interval: quotes.OneMinute, Source: "yahoo", Series: quotes.CandleSeries{Candles: bars}})

	// One reading before the archive's last bar, one after it.
	var vti string
	tickers, _ := st.Tickers()
	for _, tk := range tickers {
		if tk.Symbol == "VTI" {
			vti = tk.ID
		}
	}
	st.SaveQuote(store.Quote{TickerID: vti, Symbol: "VTI", Price: ptr(1.0), Status: store.StatusOK, FetchedAt: start.Add(10 * time.Minute)})
	st.SaveQuote(store.Quote{TickerID: vti, Symbol: "VTI", Price: ptr(200.0), Status: store.StatusOK, FetchedAt: time.Now()})

	got, err := eng.Sparkline("VTI", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 7 || got[0].Price != 104 || got[len(got)-1].Price != 200 {
		t.Errorf("sparkline = %v, want five-minute closes from the archive then the newest live reading", prices(got))
	}
	for _, p := range got {
		if p.Price == 1 {
			t.Error("a reading older than the archive's bars was drawn over them")
		}
	}
}

func prices(points []store.HistoryPoint) []float64 {
	out := make([]float64, len(points))
	for i, p := range points {
		out[i] = p.Price
	}
	return out
}

func ptr(f float64) *float64 { return &f }

func TestSymbolsIsEverythingTheAppUses(t *testing.T) {
	eng, st := newTestEngine(t, &fakeProvider{})
	if _, err := st.CreateTicker(store.NewTicker{Expression: "VTI/GLD"}); err != nil {
		t.Fatalf("composite: %v", err)
	}
	got, err := eng.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, s := range got {
		have[s] = true
	}
	for _, want := range append(store.SeedSymbols, "VTI", "GLD") {
		if !have[want] {
			t.Errorf("%s is missing from %v", want, got)
		}
	}
	if have["VTI/GLD"] {
		t.Error("a composite's formula was listed as a symbol")
	}
}
