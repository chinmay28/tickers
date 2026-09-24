package archive

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func newTestArchive(t *testing.T) *Archive {
	t.Helper()
	root := t.TempDir()
	if err := Init(root); err != nil {
		t.Fatalf("init: %v", err)
	}
	a, err := Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

var t0 = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func day(n int) time.Time { return t0.AddDate(0, 0, n) }

// session is a US session's open on day n: 13:30 UTC.
func session(n int) time.Time { return day(n).Add(13*time.Hour + 30*time.Minute) }

func track(t *testing.T, a *Archive, list List, symbols ...string) {
	t.Helper()
	for _, s := range symbols {
		if err := a.Add(list, Entry{Symbol: s, Kind: KindStock}, t0); err != nil {
			t.Fatalf("add %s: %v", s, err)
		}
	}
}

func idOf(t *testing.T, a *Archive, symbol string) int64 {
	t.Helper()
	s, err := a.Lookup(symbol)
	if err != nil {
		t.Fatalf("lookup %s: %v", symbol, err)
	}
	return s.ID
}

func candle(at time.Time, price float64, volume int64) quotes.Candle {
	return quotes.Candle{Time: at, Open: price, High: price, Low: price, Close: price, Volume: volume}
}

func minutes(start time.Time, n int, price float64) []quotes.Candle {
	out := make([]quotes.Candle, n)
	for i := range out {
		out[i] = candle(start.Add(time.Duration(i)*time.Minute), price, 1)
	}
	return out
}

func record(t *testing.T, a *Archive, b Batch) {
	t.Helper()
	if err := a.Record(b); err != nil {
		t.Fatalf("record: %v", err)
	}
}

func TestOpenRefusesAFolderThatIsNotAnArchive(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("opening an empty folder gave %v, want ErrUnavailable — it is what an unmounted drive looks like", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("a refused open wrote %d files into the folder; an empty mount point must stay empty", len(entries))
	}

	missing := filepath.Join(root, "not-mounted")
	if err := Init(missing); err == nil {
		t.Fatal("init created a folder that did not exist")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("init left a folder behind at a path that did not exist")
	}

	if err := Init(root); err != nil {
		t.Fatalf("init: %v", err)
	}
	for i := 0; i < 2; i++ {
		a, err := Open(root)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		a.Close()
	}
	if err := Init(root); err != nil {
		t.Errorf("re-initialising an archive failed: %v", err)
	}
}

func TestPingNoticesTheDriveGoing(t *testing.T) {
	a := newTestArchive(t)
	if err := a.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	os.Remove(filepath.Join(a.Root(), MarkerFile))
	if err := a.Ping(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ping without the marker gave %v, want ErrUnavailable", err)
	}
}

func TestListsDecideWhatIsTracked(t *testing.T) {
	a := newTestArchive(t)
	joined, left, err := a.SetList(Listed, []Entry{
		{Symbol: "aapl", Name: "Apple", Exchange: "NASDAQ", Kind: KindStock},
		{Symbol: "GLD", Name: "SPDR Gold", Kind: KindETF},
		{Symbol: "IBM", Kind: KindStock},
	}, t0)
	if err != nil || joined != 3 || left != 0 {
		t.Fatalf("first listing: joined %d left %d err %v", joined, left, err)
	}
	// The watchlist knows GLD too, with no name: it must not blank the name.
	if _, _, err := a.SetList(Watchlist, []Entry{{Symbol: "GLD"}, {Symbol: "BTC-USD"}}, t0); err != nil {
		t.Fatal(err)
	}
	// Next day's listing drops GLD and IBM.
	joined, left, err = a.SetList(Listed, []Entry{{Symbol: "AAPL"}}, day(1))
	if err != nil || joined != 0 || left != 2 {
		t.Fatalf("second listing: joined %d left %d err %v, want 0 and 2", joined, left, err)
	}

	gld, _ := a.Lookup("gld")
	if !gld.Active || gld.Name != "SPDR Gold" || !gld.Priority {
		t.Errorf("GLD = %+v, want still active (it is on the watchlist), named, and priority", gld)
	}
	ibm, _ := a.Lookup("IBM")
	if ibm.Active {
		t.Errorf("IBM is on no list and still active")
	}

	// Excluding wins over every list; history is kept.
	if err := a.SetExcluded("AAPL", true); err != nil {
		t.Fatal(err)
	}
	active, _ := a.ActiveSymbols()
	var names []string
	for _, s := range active {
		names = append(names, s.Symbol)
	}
	if len(names) != 2 || names[0] != "BTC-USD" || names[1] != "GLD" {
		t.Errorf("active = %v, want BTC-USD and GLD", names)
	}
	if err := a.SetExcluded("NOPE", true); !errors.Is(err, ErrUnknownSymbol) {
		t.Errorf("excluding an unknown symbol gave %v", err)
	}

	st, err := a.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Active != 2 || st.Retired != 1 || st.Excluded != 1 || st.Priority != 2 || st.Lists[Watchlist] != 2 {
		t.Errorf("stats = %+v", st)
	}
}

func TestQuerySymbolsSearchesFiltersAndPages(t *testing.T) {
	a := newTestArchive(t)
	a.SetList(Listed, []Entry{
		{Symbol: "AAPL", Name: "Apple Inc.", Kind: KindStock},
		{Symbol: "AMZN", Name: "Amazon.com", Kind: KindStock},
		{Symbol: "SPY", Name: "SPDR S&P 500", Kind: KindETF},
		{Symbol: "APPLX", Name: "100% Pineapple", Kind: KindStock},
	}, t0)
	track(t, a, User, "ZZZZ")

	page, err := a.QuerySymbols(SymbolQuery{Text: "ap"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Errorf("'ap' matched %d, want AAPL and APPLX by symbol prefix (Pineapple's name matches too, once)", page.Total)
	}
	page, _ = a.QuerySymbols(SymbolQuery{Text: "100%"})
	if page.Total != 1 {
		t.Errorf("a literal %% matched %d symbols, want 1 — LIKE wildcards in the query must be escaped", page.Total)
	}
	page, _ = a.QuerySymbols(SymbolQuery{Kind: KindETF})
	if page.Total != 1 || page.Symbols[0].Symbol != "SPY" {
		t.Errorf("ETF filter = %+v", page)
	}
	page, _ = a.QuerySymbols(SymbolQuery{Limit: 2})
	if page.Total != 5 || len(page.Symbols) != 2 || page.Symbols[0].Symbol != "ZZZZ" {
		t.Errorf("first page = %d of %d starting %v, want a hand-added symbol first", len(page.Symbols), page.Total, page.Symbols)
	}
	page, _ = a.QuerySymbols(SymbolQuery{Offset: 4, Limit: 2})
	if len(page.Symbols) != 1 {
		t.Errorf("last page has %d, want 1", len(page.Symbols))
	}
	if _, err := a.QuerySymbols(SymbolQuery{Filter: "bogus"}); err == nil {
		t.Error("an unknown filter was accepted")
	}
}

func TestRecordPartitionsByYearAndKeepsTheLedger(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")

	// Minute bars either side of new year land in two files.
	eve := time.Date(2025, 12, 31, 20, 55, 0, 0, time.UTC)
	cur := &Cursor{Source: "yahoo", Oldest: eve, Newest: eve.Add(24 * time.Hour)}
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo", Cursor: cur,
		Series: quotes.CandleSeries{Candles: append(minutes(eve, 5, 10), minutes(eve.Add(24*time.Hour-40*time.Minute), 3, 11)...)}})

	for _, key := range []string{"1m/2025", "1m/2026"} {
		if _, err := os.Stat(a.partitionPath(key)); err != nil {
			t.Errorf("partition %s was not written: %v", key, err)
		}
	}
	got, err := a.Candles("AAPL", quotes.OneMinute, eve, eve.Add(48*time.Hour))
	if err != nil || len(got) != 8 {
		t.Fatalf("read back %d bars across the year boundary (err %v), want 8", len(got), err)
	}
	held, _ := a.HeldDays(id, quotes.OneMinute, eve.Add(-48*time.Hour), eve.Add(48*time.Hour))
	if len(held) != 2 {
		t.Errorf("ledger holds %d days, want 2", len(held))
	}
	st, _ := a.Stats()
	if len(st.Intervals) != 1 || st.Intervals[0].Bars != 8 || st.Intervals[0].Started != 1 {
		t.Errorf("stats = %+v, want 8 one-minute bars on one started series", st.Intervals)
	}
	cursors, _ := a.SymbolCursors(id)
	if len(cursors) != 1 || cursors[0].Source != "yahoo" || !cursors[0].Newest.Equal(cur.Newest) {
		t.Errorf("cursors = %+v", cursors)
	}
}

func TestAnotherSourceFillsGapsButOnlyReplacesWhenAsked(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")
	open := session(0)

	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: minutes(open, 3, 10)}})
	// Yahoo revises its own last bar.
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: []quotes.Candle{candle(open.Add(2*time.Minute), 10.5, 2)}}})
	// A paid feed covers the same three minutes and two more.
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "polygon",
		Series: quotes.CandleSeries{Candles: minutes(open, 5, 20)}})

	got, _ := a.Candles("AAPL", quotes.OneMinute, open, open.Add(time.Hour))
	want := []float64{10, 10, 10.5, 20, 20}
	for i, c := range got {
		if c.Close != want[i] {
			t.Fatalf("closes = %v, want %v — a source revises its own bars and fills only others' gaps", closes(got), want)
		}
	}

	// Asked to, the paid feed replaces.
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "polygon", Replace: true,
		Series: quotes.CandleSeries{Candles: minutes(open, 5, 20)}})
	got, _ = a.Candles("AAPL", quotes.OneMinute, open, open.Add(time.Hour))
	for _, c := range got {
		if c.Close != 20 {
			t.Fatalf("closes after a replace = %v, want all 20", closes(got))
		}
	}
	cov, _ := a.SymbolCoverage("AAPL")
	if tl := cov.Timelines[quotes.OneMinute]; len(tl) != 1 || tl[0].Bars != 5 || len(tl[0].Sources) != 1 || tl[0].Sources[0] != "polygon" {
		t.Errorf("timeline = %+v, want one month of 5 bars all from polygon", tl)
	}
}

func closes(cs []quotes.Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Close
	}
	return out
}

func TestASplitRescalesEveryPartitionOnce(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")

	// Stored before a 4:1 split on day 2: daily bars, and minute bars in two
	// yearly files.
	record(t, a, Batch{SymbolID: id, Interval: quotes.Daily, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: []quotes.Candle{candle(day(0), 400, 100), candle(day(1), 404, 100)},
			Dividends: []quotes.Dividend{{Time: day(1), Amount: 0.8}}}})
	lastYear := time.Date(2025, 6, 2, 13, 30, 0, 0, time.UTC)
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: append(minutes(lastYear, 1, 300), minutes(session(1), 1, 402)...)}})

	split := quotes.Split{Time: day(2), Numerator: 4, Denominator: 1}
	// The first response to report it: already on the new basis, overlapping
	// day 1.
	record(t, a, Batch{SymbolID: id, Interval: quotes.Daily, Source: "yahoo", Series: quotes.CandleSeries{
		Splits:  []quotes.Split{split},
		Candles: []quotes.Candle{candle(day(1), 101, 400), candle(day(2), 102, 400)},
	}})

	daily, _ := a.Candles("AAPL", quotes.Daily, day(0), day(10))
	if daily[0].Close != 100 || daily[0].Volume != 400 {
		t.Errorf("day 0 = %+v, want 400/4 and volume ×4", daily[0])
	}
	if daily[1].Close != 101 || daily[2].Close != 102 {
		t.Errorf("days 1–2 = %v, %v, want the response's own adjusted bars untouched", daily[1].Close, daily[2].Close)
	}
	mins, _ := a.Candles("AAPL", quotes.OneMinute, lastYear.Add(-time.Hour), day(10))
	if mins[0].Close != 75 || mins[1].Close != 100.5 {
		t.Errorf("minute bars = %v, want 75 and 100.5 — every interval and every yearly file is rescaled", closes(mins))
	}

	// Reported again by another interval: never re-applied.
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo", Series: quotes.CandleSeries{Splits: []quotes.Split{split}}})
	daily, _ = a.Candles("AAPL", quotes.Daily, day(0), day(10))
	if daily[0].Close != 100 {
		t.Errorf("day 0 = %v after a second report, want 100", daily[0].Close)
	}
	if divs, _ := a.Dividends("AAPL", day(-10), day(10)); len(divs) != 1 || divs[0].Amount != 0.2 {
		t.Errorf("dividends = %+v, want 0.8/4 — a payout is per share, and a split changes the share", divs)
	}
	splits, _ := a.Splits("AAPL")
	if len(splits) != 1 {
		t.Errorf("recorded %d splits, want 1", len(splits))
	}
}

func TestAnInterruptedSplitResumesWithoutDoubling(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")
	lastYear := time.Date(2025, 6, 2, 13, 30, 0, 0, time.UTC)
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: append(minutes(lastYear, 1, 400), minutes(session(1), 1, 400)...)}})

	// Simulate a crash after the split was marked pending and one file was
	// rescaled, but not the other.
	split := quotes.Split{Time: day(2), Numerator: 2, Denominator: 1}
	a.catalog.Exec(`INSERT INTO splits (symbol_id, ts, numerator, denominator, state) VALUES (?, ?, 2, 1, 'pending')`, id, split.Time.Unix())
	db, _ := a.partition("1m/2025", false)
	db.Exec(`UPDATE bars SET close = close * 0.5, open = open * 0.5, high = high * 0.5, low = low * 0.5 WHERE symbol_id = ?`, id)
	db.Exec(`INSERT INTO applied_splits (symbol_id, ts) VALUES (?, ?)`, id, split.Time.Unix())
	root := a.Root()
	a.Close()

	b, err := Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b.Close()
	mins, _ := b.Candles("AAPL", quotes.OneMinute, lastYear.Add(-time.Hour), day(10))
	if mins[0].Close != 200 || mins[1].Close != 200 {
		t.Errorf("after resuming, closes = %v, want both 200 — the done file untouched, the other finished", closes(mins))
	}
	var state string
	b.catalog.QueryRow(`SELECT state FROM splits WHERE symbol_id = ?`, id).Scan(&state)
	if state != "applied" {
		t.Errorf("split state = %q after resuming, want applied", state)
	}
}

func TestBarsBuildsMissingDaysFromAFinerInterval(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")

	// Day 0 has native five-minute bars; day 1 only one-minute bars.
	record(t, a, Batch{SymbolID: id, Interval: quotes.FiveMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: []quotes.Candle{candle(session(0), 50, 5)}}})
	record(t, a, Batch{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: append(minutes(session(0), 5, 1), minutes(session(1), 10, 7)...)}})

	got, err := a.Bars("AAPL", quotes.FiveMinute, day(0), day(3))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d five-minute bars, want day 0's native one and two built for day 1: %+v", len(got), got)
	}
	if got[0].Close != 50 {
		t.Errorf("day 0 = %v, want the native bar, not one built over it", got[0].Close)
	}
	if !got[1].Time.Equal(session(1)) || got[1].Volume != 5 || !got[2].Time.Equal(session(1).Add(5*time.Minute)) {
		t.Errorf("built bars = %+v, want two five-minute buckets from the open", got[1:])
	}
}

func TestResampleAnchorsToTheSessionOpen(t *testing.T) {
	var in []quotes.Candle
	for i := 0; i < 120; i++ {
		in = append(in, quotes.Candle{Time: session(0).Add(time.Duration(i) * time.Minute),
			Open: float64(i), High: float64(i) + 1, Low: float64(i) - 1, Close: float64(i) + 0.5, Volume: 1})
	}
	got := Resample(in, quotes.Hourly)
	if len(got) != 2 {
		t.Fatalf("got %d hourly bars, want 2", len(got))
	}
	h := got[0]
	if !h.Time.Equal(session(0)) || h.Open != 0 || h.High != 60 || h.Low != -1 || h.Close != 59.5 || h.Volume != 60 {
		t.Errorf("first hour = %+v, want 9:30–10:30 with open 0, high 60, low −1, close 59.5, volume 60", h)
	}
}

func TestTradingDaysComeFromTheDailySeries(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")
	record(t, a, Batch{SymbolID: id, Interval: quotes.Daily, Source: "yahoo",
		Series: quotes.CandleSeries{Candles: []quotes.Candle{candle(day(0), 1, 1), candle(day(3), 1, 1)}}})
	got, err := a.TradingDays(id, day(0), day(5))
	if err != nil || len(got) != 2 || got[1] != day(3).Unix() {
		t.Errorf("trading days = %v (err %v), want day 0 and day 3", got, err)
	}
}

func TestRecordRejectsBadInputAndWritesNothing(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "AAPL")
	id := idOf(t, a, "AAPL")
	one := quotes.CandleSeries{Candles: []quotes.Candle{candle(day(1), 1, 1)}}
	bad := []Batch{
		{Interval: quotes.Daily, Source: "yahoo", Series: one},
		{SymbolID: id, Interval: "2m", Source: "yahoo", Series: one},
		{SymbolID: id, Interval: quotes.Daily, Series: one},
		{SymbolID: id, Interval: quotes.Daily, Source: "yahoo", Series: one, Cursor: &Cursor{Oldest: day(3), Newest: day(1)}},
	}
	for _, b := range bad {
		if err := a.Record(b); err == nil {
			t.Errorf("batch %+v was accepted", b)
		}
	}
	if got, _ := a.Candles("AAPL", quotes.Daily, day(0), day(10)); len(got) != 0 {
		t.Errorf("rejected batches wrote %d bars", len(got))
	}
}

func TestFailedCursorRoundTripsAndCountsAsFailing(t *testing.T) {
	a := newTestArchive(t)
	track(t, a, Listed, "ZZZZ")
	id := idOf(t, a, "ZZZZ")
	want := Cursor{SymbolID: id, Interval: quotes.OneMinute, Source: "yahoo", Failures: 2, NextAttempt: day(1), LastError: "no such symbol"}
	if err := a.SaveCursor(want); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Cursors()
	if len(got) != 1 || got[0].Failures != 2 || !got[0].NextAttempt.Equal(day(1)) || !got[0].Newest.IsZero() {
		t.Errorf("cursor = %+v", got)
	}
	st, _ := a.Stats()
	if len(st.Intervals) != 1 || st.Intervals[0].Failing != 1 || st.Intervals[0].Started != 0 {
		t.Errorf("stats = %+v, want one failing series and none started", st.Intervals)
	}
	page, _ := a.QuerySymbols(SymbolQuery{Filter: FilterFailing})
	if page.Total != 1 {
		t.Errorf("failing filter found %d", page.Total)
	}
	if err := a.ResetCursors(id, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Cursors(); len(got) != 0 {
		t.Errorf("reset left %d cursors", len(got))
	}
}

func TestSizeAndDisk(t *testing.T) {
	a := newTestArchive(t)
	if n, err := a.Size(); err != nil || n == 0 {
		t.Errorf("size = %d (err %v), want the catalog's bytes", n, err)
	}
	if free, total, err := Disk(a.Root()); err == nil && (total == 0 || free > total) {
		t.Errorf("disk free %d of %d", free, total)
	}
}
