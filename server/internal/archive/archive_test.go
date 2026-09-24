package archive

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func newTestArchive(t *testing.T) *Archive {
	t.Helper()
	a, err := Open(filepath.Join(t.TempDir(), "archive.sqlite"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return t0.AddDate(0, 0, n) }

// trackOne tracks a symbol and returns its ID.
func trackOne(t *testing.T, a *Archive, symbol string) int64 {
	t.Helper()
	if err := a.Track([]Entry{{Symbol: symbol, Kind: KindStock}}, t0); err != nil {
		t.Fatalf("track %s: %v", symbol, err)
	}
	syms, err := a.ActiveSymbols()
	if err != nil {
		t.Fatalf("active symbols: %v", err)
	}
	for _, s := range syms {
		if s.Symbol == symbol {
			return s.ID
		}
	}
	t.Fatalf("%s is not active after tracking it", symbol)
	return 0
}

func candle(at time.Time, price float64, volume int64) quotes.Candle {
	return quotes.Candle{Time: at, Open: price, High: price, Low: price, Close: price, Volume: volume}
}

func TestReopeningIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.sqlite")
	for i := 0; i < 2; i++ {
		a, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v — migrations must be skipped once recorded", i+1, err)
		}
		a.Close()
	}
}

func TestTrackUpsertsAndRetireKeepsHistory(t *testing.T) {
	a := newTestArchive(t)
	if err := a.Track([]Entry{
		{Symbol: "aapl", Name: "Apple", Exchange: "NASDAQ", Kind: KindStock},
		{Symbol: "GLD", Kind: KindETF},
		{Symbol: "BTC-USD", Kind: KindExtra},
	}, t0); err != nil {
		t.Fatalf("track: %v", err)
	}
	// A re-track with no name must not blank the one already known.
	if err := a.Track([]Entry{{Symbol: "AAPL", Kind: KindStock}}, day(1)); err != nil {
		t.Fatalf("re-track: %v", err)
	}

	retired, err := a.RetireAllExcept([]string{"aapl", "BTC-USD"})
	if err != nil || retired != 1 {
		t.Fatalf("retired %d (err %v), want GLD alone", retired, err)
	}
	active, _ := a.ActiveSymbols()
	if len(active) != 2 || active[0].Symbol != "AAPL" || active[0].Name != "Apple" {
		t.Fatalf("active = %+v, want AAPL (name kept) and BTC-USD", active)
	}

	// Coming back onto the list reactivates the same row, and its ID with it.
	gldBefore := trackOne(t, a, "GLD")
	if err := a.Track([]Entry{{Symbol: "GLD"}}, day(2)); err != nil {
		t.Fatal(err)
	}
	if again := trackOne(t, a, "GLD"); again != gldBefore {
		t.Errorf("GLD's ID moved from %d to %d; its history would be orphaned", gldBefore, again)
	}
	st, _ := a.Stats()
	if st.Active != 3 || st.Retired != 0 {
		t.Errorf("stats = %d active, %d retired; want 3 and 0", st.Active, st.Retired)
	}
}

func TestTrackRefusesABlankSymbol(t *testing.T) {
	a := newTestArchive(t)
	if err := a.Track([]Entry{{Symbol: "  "}}, t0); err == nil {
		t.Error("a blank symbol was tracked")
	}
}

func TestRecordWritesBarsAndCoverageTogether(t *testing.T) {
	a := newTestArchive(t)
	id := trackOne(t, a, "AAPL")

	cov := Coverage{SymbolID: id, Interval: quotes.Daily, Oldest: day(0), Newest: day(3)}
	err := a.Record(Batch{Coverage: cov, Series: quotes.CandleSeries{
		Candles:    []quotes.Candle{candle(day(0), 10, 100), candle(day(1), 11, 110), candle(day(2), 12, 120)},
		Dividends:  []quotes.Dividend{{Time: day(1), Amount: 0.25}},
		FirstTrade: day(-3650),
	}})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	// A second fetch overlapping the first revises rather than duplicates.
	cov.Newest = day(4)
	if err := a.Record(Batch{Coverage: cov, Series: quotes.CandleSeries{
		Candles: []quotes.Candle{candle(day(2), 12.5, 125), candle(day(3), 13, 130)},
	}}); err != nil {
		t.Fatalf("record overlap: %v", err)
	}

	got, err := a.Candles("aapl", quotes.Daily, day(0), day(10))
	if err != nil {
		t.Fatalf("candles: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d bars, want 4 — an overlapping fetch must overwrite, not duplicate", len(got))
	}
	if got[2].Close != 12.5 || got[2].Volume != 125 {
		t.Errorf("day 2 = %+v, want the later fetch's revision", got[2])
	}
	if other, _ := a.Candles("AAPL", quotes.Hourly, day(0), day(10)); len(other) != 0 {
		t.Errorf("hourly read returned %d daily bars", len(other))
	}

	covs, _ := a.Coverages()
	if len(covs) != 1 || !covs[0].Oldest.Equal(day(0)) || !covs[0].Newest.Equal(day(4)) {
		t.Errorf("coverage = %+v, want day 0 to day 4", covs)
	}
	syms, _ := a.ActiveSymbols()
	if !syms[0].FirstTrade.Equal(day(-3650)) {
		t.Errorf("first trade = %s, want it recorded from the series", syms[0].FirstTrade)
	}
}

func TestASplitRescalesEverythingStoredBeforeItOnce(t *testing.T) {
	a := newTestArchive(t)
	id := trackOne(t, a, "AAPL")

	// Before the split: a daily and an hourly series at the old basis.
	if err := a.Record(Batch{
		Coverage: Coverage{SymbolID: id, Interval: quotes.Daily, Oldest: day(0), Newest: day(2)},
		Series:   quotes.CandleSeries{Candles: []quotes.Candle{candle(day(0), 400, 100), candle(day(1), 404, 100)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Record(Batch{
		Coverage: Coverage{SymbolID: id, Interval: quotes.Hourly, Oldest: day(1), Newest: day(2)},
		Series:   quotes.CandleSeries{Candles: []quotes.Candle{candle(day(1).Add(14*time.Hour), 402, 10)}},
	}); err != nil {
		t.Fatal(err)
	}

	// The daily forward fetch after a 4:1 split on day 2: Yahoo's response is
	// already on the new basis, including the day-1 bar it overlaps.
	split := quotes.Split{Time: day(2), Numerator: 4, Denominator: 1}
	forward := Batch{
		Coverage: Coverage{SymbolID: id, Interval: quotes.Daily, Oldest: day(0), Newest: day(3)},
		Series: quotes.CandleSeries{
			Splits:  []quotes.Split{split},
			Candles: []quotes.Candle{candle(day(1), 101, 400), candle(day(2), 102, 400)},
		},
	}
	if err := a.Record(forward); err != nil {
		t.Fatalf("record split: %v", err)
	}

	daily, _ := a.Candles("AAPL", quotes.Daily, day(0), day(10))
	if daily[0].Close != 100 || daily[0].Volume != 400 {
		t.Errorf("day 0 = %+v, want 400/4 = 100 and volume ×4 — a stored bar on the old basis must be rescaled", daily[0])
	}
	if daily[1].Close != 101 {
		t.Errorf("day 1 = %v, want the response's own 101 — fetched bars are already adjusted and must not be rescaled on top", daily[1].Close)
	}
	if daily[2].Close != 102 {
		t.Errorf("ex-date bar = %v, want 102 untouched", daily[2].Close)
	}
	hourly, _ := a.Candles("AAPL", quotes.Hourly, day(0), day(10))
	if hourly[0].Close != 100.5 || hourly[0].Volume != 40 {
		t.Errorf("hourly bar = %+v, want 100.5 × volume 40 — a split rescales every interval", hourly[0])
	}

	// Another interval's fetch reporting the same split must not apply it again.
	if err := a.Record(Batch{
		Coverage: Coverage{SymbolID: id, Interval: quotes.Hourly, Oldest: day(1), Newest: day(3)},
		Series:   quotes.CandleSeries{Splits: []quotes.Split{split}},
	}); err != nil {
		t.Fatal(err)
	}
	daily, _ = a.Candles("AAPL", quotes.Daily, day(0), day(10))
	if daily[0].Close != 100 {
		t.Errorf("day 0 = %v after the split was reported twice, want 100 — a known split is never re-applied", daily[0].Close)
	}
}

func TestRecordRejectsBadCoverageAndWritesNothing(t *testing.T) {
	a := newTestArchive(t)
	id := trackOne(t, a, "AAPL")
	bad := []Coverage{
		{Interval: quotes.Daily},
		{SymbolID: id, Interval: "2m"},
		{SymbolID: id, Interval: quotes.Daily, Oldest: day(3), Newest: day(1)},
	}
	for _, c := range bad {
		if err := a.Record(Batch{Coverage: c, Series: quotes.CandleSeries{Candles: []quotes.Candle{candle(day(1), 1, 1)}}}); err == nil {
			t.Errorf("coverage %+v was accepted", c)
		}
	}
	if got, _ := a.Candles("AAPL", quotes.Daily, day(0), day(10)); len(got) != 0 {
		t.Errorf("a rejected batch wrote %d bars", len(got))
	}
}

func TestFailureCoverageRoundTrips(t *testing.T) {
	a := newTestArchive(t)
	id := trackOne(t, a, "ZZZZ")
	want := Coverage{SymbolID: id, Interval: quotes.FiveMinute, Failures: 2, NextAttempt: day(1), LastError: "no quote for that symbol"}
	if err := a.SaveCoverage(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	covs, _ := a.Coverages()
	if len(covs) != 1 {
		t.Fatalf("got %d coverage rows, want 1", len(covs))
	}
	got := covs[0]
	if got.Failures != 2 || !got.NextAttempt.Equal(day(1)) || got.LastError != want.LastError || !got.Oldest.IsZero() || !got.Newest.IsZero() {
		t.Errorf("coverage = %+v, want %+v — nothing fetched must read back as zero times", got, want)
	}

	st, _ := a.Stats()
	if len(st.Intervals) != 1 || st.Intervals[0].Started != 0 || st.Intervals[0].Failing != 1 {
		t.Errorf("stats = %+v, want 5m with nothing started and one failing — a symbol that has never succeeded is the one to show", st.Intervals)
	}
}

func TestStatsSummariseEachInterval(t *testing.T) {
	a := newTestArchive(t)
	aapl, msft := trackOne(t, a, "AAPL"), trackOne(t, a, "MSFT")
	must := func(c Coverage) {
		t.Helper()
		if err := a.SaveCoverage(c); err != nil {
			t.Fatal(err)
		}
	}
	must(Coverage{SymbolID: aapl, Interval: quotes.Daily, Oldest: day(-1000), Newest: day(5), Complete: true})
	must(Coverage{SymbolID: msft, Interval: quotes.Daily, Oldest: day(-10), Newest: day(3), Failures: 1})
	must(Coverage{SymbolID: aapl, Interval: quotes.FiveMinute, Oldest: day(-59), Newest: day(5)})

	st, err := a.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Intervals) != 2 || st.Intervals[0].Interval != quotes.Daily {
		t.Fatalf("intervals = %+v, want 1d then 5m", st.Intervals)
	}
	d := st.Intervals[0]
	if d.Started != 2 || d.Complete != 1 || d.Failing != 1 {
		t.Errorf("1d counts = %+v", d)
	}
	if !d.Oldest.Equal(day(-1000)) || !d.Newest.Equal(day(5)) || !d.Laggard.Equal(day(3)) {
		t.Errorf("1d span = %s…%s laggard %s", d.Oldest, d.Newest, d.Laggard)
	}
}

func TestUniverseSyncTimeRoundTrips(t *testing.T) {
	a := newTestArchive(t)
	if got, err := a.UniverseSyncedAt(); err != nil || !got.IsZero() {
		t.Fatalf("a fresh archive reports a sync at %s (err %v)", got, err)
	}
	if err := a.MarkUniverseSynced(day(2)); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.UniverseSyncedAt(); !got.Equal(day(2)) {
		t.Errorf("synced at %s, want %s", got, day(2))
	}
}
