package collector

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/universe"
)

// call is one request a fake source saw.
type call struct {
	at       time.Time
	source   string
	symbol   string
	interval quotes.Interval
	from, to time.Time
}

// fakeProvider answers with a bar per weekday for daily requests and three
// bars at each weekday's open for intraday ones, priced at price so a test can
// tell which source wrote a bar.
type fakeProvider struct {
	name       string
	price      float64
	log        *[]call
	clock      *clock
	firstTrade time.Time
	fail       func(symbol string, interval quotes.Interval) error
}

func (f *fakeProvider) Candles(_ context.Context, symbol string, interval quotes.Interval, from, to time.Time) (quotes.CandleSeries, error) {
	if f.log != nil {
		*f.log = append(*f.log, call{f.clock.now(), f.name, symbol, interval, from, to})
	}
	if f.fail != nil {
		if err := f.fail(symbol, interval); err != nil {
			return quotes.CandleSeries{}, err
		}
	}
	out := quotes.CandleSeries{FirstTrade: f.firstTrade}
	start := from
	if start.Before(f.firstTrade) {
		start = f.firstTrade
	}
	for d := start.Truncate(day); d.Before(to); d = d.Add(day) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		if !interval.Intraday() {
			if !d.Before(from.Truncate(day)) {
				out.Candles = append(out.Candles, bar(d, f.price))
			}
			continue
		}
		open := d.Add(13*time.Hour + 30*time.Minute)
		for i := 0; i < 3; i++ {
			at := open.Add(time.Duration(i) * interval.Step())
			if !at.Before(from) && at.Before(to) {
				out.Candles = append(out.Candles, bar(at, f.price))
			}
		}
	}
	return out, nil
}

func bar(at time.Time, price float64) quotes.Candle {
	return quotes.Candle{Time: at, Open: price, High: price, Low: price, Close: price, Volume: 1}
}

type fakeLister struct {
	listings []universe.Listing
	err      error
	calls    int
}

func (f *fakeLister) Listings(context.Context) ([]universe.Listing, error) {
	f.calls++
	return f.listings, f.err
}

// clock is a fake time source whose sleeps return at once, having moved it.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return ctx.Err()
}

type harness struct {
	c       *Collector
	archive *archive.Archive
	clock   *clock
	calls   []call
	yahoo   *fakeProvider
	lister  *fakeLister
	plan    Plan
}

func newHarness(t *testing.T, intervals []quotes.Interval, listed ...string) *harness {
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
	h := &harness{archive: a, clock: &clock{t: now}}
	h.yahoo = &fakeProvider{name: "yahoo", price: 1, log: &h.calls, clock: h.clock, firstTrade: now.AddDate(-25, 0, 0)}
	h.lister = &fakeLister{}
	for _, s := range listed {
		h.lister.listings = append(h.lister.listings, universe.Listing{Symbol: s, Exchange: "NASDAQ"})
	}
	h.plan = Plan{Intervals: intervals, Sources: []Source{Yahoo(h.yahoo, 2*time.Second)}}
	if len(listed) > 0 {
		h.plan.Universe = h.lister
	}
	c, err := New(a, func(context.Context) (Plan, error) { return h.plan, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.now, c.sleep = h.clock.now, h.clock.sleep
	h.c = c
	return h
}

// run steps until n requests have gone out in all, failing if the loop stops
// making them first.
func (h *harness) run(t *testing.T, n int) {
	t.Helper()
	for idle := 0; len(h.calls) < n; {
		did, err := h.c.Step(context.Background())
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		if !did {
			if idle++; idle > 2000 {
				t.Fatalf("stopped after %d requests, want %d", len(h.calls), n)
			}
		}
	}
}

// settle steps until the loop goes idle.
func (h *harness) settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 5000; i++ {
		before := len(h.calls)
		if _, err := h.c.Step(context.Background()); err != nil {
			t.Fatalf("step: %v", err)
		}
		if st := h.c.Status(); st.State == StateIdle && len(h.calls) == before {
			return
		}
	}
	t.Fatal("never went idle")
}

func cursorOf(t *testing.T, a *archive.Archive, symbol string, i quotes.Interval, source string) archive.Cursor {
	t.Helper()
	s, err := a.Lookup(symbol)
	if err != nil {
		t.Fatalf("lookup %s: %v", symbol, err)
	}
	cs, _ := a.SymbolCursors(s.ID)
	for _, c := range cs {
		if c.Interval == i && c.Source == source {
			return c
		}
	}
	return archive.Cursor{}
}

func TestTheWatchlistGoesFirstThenEverySeriesIsSeededThenDug(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.Daily, quotes.OneMinute, quotes.FiveMinute, quotes.Hourly}, "AAPL", "MSFT")
	h.plan.Extras = []string{"btc-usd"}
	h.plan.Watchlist = []string{"MSFT"}
	h.settle(t)

	// MSFT's requests all come before anyone else's.
	seenOther := false
	for _, c := range h.calls {
		if c.symbol != "MSFT" {
			seenOther = true
		} else if seenOther {
			t.Fatalf("a MSFT request came after another symbol's; the watchlist goes first: %+v", c)
		}
	}
	for i := 1; i < len(h.calls); i++ {
		if gap := h.calls[i].at.Sub(h.calls[i-1].at); gap < 2*time.Second {
			t.Fatalf("requests %d and %d were %s apart, want the 2s spacing", i-1, i, gap)
		}
	}
	for _, s := range []string{"AAPL", "MSFT", "BTC-USD"} {
		d := cursorOf(t, h.archive, s, quotes.Daily, "yahoo")
		if !d.Complete || d.Oldest.After(h.yahoo.firstTrade) {
			t.Errorf("%s daily = %+v, want walked back to the listing", s, d)
		}
		for _, i := range []quotes.Interval{quotes.OneMinute, quotes.FiveMinute, quotes.Hourly} {
			if c := cursorOf(t, h.archive, s, i, "yahoo"); !c.Complete {
				t.Errorf("%s %s = %+v, want walked back to the horizon", s, i, c)
			}
		}
	}
	// Per symbol: 3 daily decades, 4 minute-bar weeks, one 5m and one 1h.
	if len(h.calls) != 3*(3+4+1+1) {
		t.Errorf("made %d requests, want %d", len(h.calls), 3*(3+4+1+1))
	}

	// A day later only daily and minute bars go forward; the five-minute and
	// hourly backfills were one-shot.
	before := len(h.calls)
	h.clock.t = h.clock.t.Add(21 * time.Hour)
	h.settle(t)
	intervals := map[quotes.Interval]int{}
	for _, c := range h.calls[before:] {
		intervals[c.interval]++
	}
	if intervals[quotes.Daily] != 3 || intervals[quotes.OneMinute] != 3 || len(intervals) != 2 {
		t.Errorf("next day's requests by interval = %v, want 3 daily and 3 minute", intervals)
	}
}

func TestAPaidSourceFillsOnlyTheDaysNotHeld(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.Daily, quotes.OneMinute})
	h.plan.Extras = []string{"AAPL"}
	h.settle(t) // Yahoo: daily history and 29 days of minute bars.
	yahooCalls := len(h.calls)

	paid := &fakeProvider{name: "polygon", price: 2, log: &h.calls, clock: h.clock, firstTrade: h.yahoo.firstTrade}
	h.plan.Sources = append(h.plan.Sources, Source{
		Name: "polygon", Provider: paid, Spacing: time.Second,
		Reach: map[quotes.Interval]Reach{quotes.OneMinute: {Horizon: 365 * day, Span: 7 * day}},
	})
	h.settle(t)

	var paidCalls []call
	for _, c := range h.calls[yahooCalls:] {
		if c.source == "polygon" {
			paidCalls = append(paidCalls, c)
		}
	}
	if len(paidCalls) == 0 {
		t.Fatal("the paid source made no requests")
	}
	for _, c := range paidCalls {
		if c.from.After(now.Add(-27 * day)) {
			t.Errorf("the paid source asked for %s–%s, which Yahoo already holds", c.from, c.to)
		}
	}
	if st := h.c.Status(); st.LastHour.Skipped == 0 {
		t.Error("no window was skipped as held; the recent month should have cost nothing")
	}
	if c := cursorOf(t, h.archive, "AAPL", quotes.OneMinute, "polygon"); !c.Complete {
		t.Errorf("paid cursor = %+v, want walked to its one-year horizon", c)
	}

	recent, _ := h.archive.Candles("AAPL", quotes.OneMinute, now.Add(-7*day), now)
	old, _ := h.archive.Candles("AAPL", quotes.OneMinute, now.Add(-300*day), now.Add(-290*day))
	if len(recent) == 0 || recent[0].Close != 1 {
		t.Errorf("recent minute bars = %v, want Yahoo's kept", closesOf(recent))
	}
	if len(old) == 0 || old[0].Close != 2 {
		t.Errorf("old minute bars = %v, want the paid source's backfill", closesOf(old))
	}
}

func closesOf(cs []quotes.Candle) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.Close
	}
	return out
}

func TestOneSourceRateLimitedDoesNotHoldUpAnother(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.OneMinute})
	h.plan.Extras = []string{"AAPL", "MSFT"}
	h.yahoo.fail = func(string, quotes.Interval) error { return fmt.Errorf("x: %w", quotes.ErrRateLimited) }
	paid := &fakeProvider{name: "polygon", price: 2, log: &h.calls, clock: h.clock}
	h.plan.Sources = append(h.plan.Sources, Source{Name: "polygon", Provider: paid, Spacing: time.Second,
		Reach: map[quotes.Interval]Reach{quotes.OneMinute: {Horizon: 60 * day, Span: 60 * day}}})
	h.run(t, 6)

	// Polygon's two seeds must not wait behind Yahoo's growing pause.
	var polygonAt []int
	yahooSecond := -1
	yahoo := 0
	for i, c := range h.calls {
		if c.source == "yahoo" {
			if yahoo++; yahoo == 2 {
				yahooSecond = i
			}
		} else {
			polygonAt = append(polygonAt, i)
		}
	}
	if len(polygonAt) != 2 || polygonAt[1] > yahooSecond {
		t.Errorf("calls = %+v, want both polygon seeds made while yahoo sat out its first pause", h.calls)
	}
	if c := cursorOf(t, h.archive, "AAPL", quotes.OneMinute, "yahoo"); c.Failures != 0 {
		t.Errorf("a 429 was charged to the symbol: %+v", c)
	}
}

func TestAFailingSymbolBacksOffWithoutHoldingUpOthers(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.Daily})
	h.plan.Extras = []string{"AAPL", "ZZZZ"}
	h.yahoo.fail = func(symbol string, _ quotes.Interval) error {
		if symbol == "ZZZZ" {
			return fmt.Errorf("%s: %w", symbol, quotes.ErrNotFound)
		}
		return nil
	}
	h.run(t, 2)
	z := cursorOf(t, h.archive, "ZZZZ", quotes.Daily, "yahoo")
	if z.Failures != 1 || !z.NextAttempt.After(h.clock.t) {
		t.Fatalf("ZZZZ = %+v, want one failure and a retry later", z)
	}
	h.run(t, 5)
	for _, c := range h.calls[2:] {
		if c.symbol == "ZZZZ" && c.at.Before(z.NextAttempt) {
			t.Fatalf("ZZZZ retried at %s, before its backoff ended at %s", c.at, z.NextAttempt)
		}
	}
}

func TestPausingStopsRequestsAndResumes(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.Daily})
	h.plan.Extras = []string{"AAPL"}
	h.plan.Paused = true
	for i := 0; i < 5; i++ {
		if _, err := h.c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.calls) != 0 {
		t.Fatalf("made %d requests while paused", len(h.calls))
	}
	if st := h.c.Status(); st.State != StatePaused {
		t.Errorf("state = %q while paused", st.State)
	}
	h.plan.Paused = false
	h.run(t, 1)

	// A disk threshold no disk meets pauses too.
	h.plan.MinFree = 1 << 62
	h.clock.t = h.clock.t.Add(time.Hour)
	before := len(h.calls)
	for i := 0; i < 5; i++ {
		h.c.Step(context.Background())
	}
	if st := h.c.Status(); st.State != StatePaused || len(h.calls) != before {
		t.Errorf("state %q with %d new requests on a full disk, want paused and none", st.State, len(h.calls)-before)
	}
}

func TestAJobReplacesARangeFromTheSourceAskedFor(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.OneMinute})
	h.plan.Extras = []string{"AAPL"}
	h.settle(t)
	paid := &fakeProvider{name: "polygon", price: 2, log: &h.calls, clock: h.clock}
	h.plan.Sources = append(h.plan.Sources, Source{Name: "polygon", Provider: paid, Spacing: time.Second,
		Reach: map[quotes.Interval]Reach{quotes.OneMinute: {Horizon: 30 * day, Span: 5 * day}}})

	j, err := h.c.Enqueue(Job{Symbol: "aapl", Interval: quotes.OneMinute, Source: "polygon",
		From: now.Add(-10 * day), To: now.Add(-day), Replace: true})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < 50 && h.c.Status().Jobs[0].State != JobDone; i++ {
		if _, err := h.c.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	got := h.c.Status().Jobs[0]
	if got.ID != j.ID || got.State != JobDone || got.Requests != 2 {
		t.Fatalf("job = %+v, want done in two five-day requests", got)
	}
	bars, _ := h.archive.Candles("AAPL", quotes.OneMinute, now.Add(-10*day), now.Add(-day))
	for _, b := range bars {
		if b.Close != 2 {
			t.Fatalf("closes = %v, want the paid source's bars to have replaced Yahoo's", closesOf(bars))
		}
	}

	if _, err := h.c.Enqueue(Job{Symbol: "NOPE", Interval: quotes.OneMinute, Source: "polygon", From: now.Add(-day), To: now}); err == nil {
		t.Error("a job for an unknown symbol was queued")
	}
	bad, _ := h.c.Enqueue(Job{Symbol: "AAPL", Interval: quotes.Daily, Source: "polygon", From: now.Add(-day), To: now})
	h.c.Step(context.Background())
	for _, js := range h.c.Status().Jobs {
		if js.ID == bad.ID && js.State != JobFailed {
			t.Errorf("a job for an interval the source doesn't serve is %q, want failed", js.State)
		}
	}
}

func TestExchangeListsAreReadDailyAndNotBelievedShort(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.Daily}, "AAPL", "GLD", "MSFT", "IBM")
	h.run(t, 1)
	if st, _ := h.archive.Stats(); st.Active != 4 {
		t.Fatalf("%d active after the first read, want 4", st.Active)
	}

	// A failed read keeps everything and retries within the hour.
	h.lister.err = errors.New("timeout")
	h.clock.t = h.clock.t.Add(25 * time.Hour)
	reads := h.lister.calls
	h.c.Step(context.Background())
	if st, _ := h.archive.Stats(); st.Active != 4 || h.lister.calls != reads+1 {
		t.Errorf("after a failed read: %d active, %d reads", st.Active, h.lister.calls-reads)
	}

	// One symbol gone: retired.
	h.lister.err, h.lister.listings = nil, h.lister.listings[:3]
	h.clock.t = h.clock.t.Add(2 * time.Hour)
	h.c.Step(context.Background())
	if st, _ := h.archive.Stats(); st.Active != 3 || st.Retired != 1 {
		t.Errorf("stats = %+v, want IBM retired", st)
	}

	// An empty list is not believed.
	h.lister.listings = nil
	h.clock.t = h.clock.t.Add(25 * time.Hour)
	h.c.Step(context.Background())
	if st, _ := h.archive.Stats(); st.Active != 3 {
		t.Errorf("%d active after an empty list, want nothing retired", st.Active)
	}

	// Switching the exchange lists off retires them all; a watchlist symbol
	// among them stays.
	h.plan.Universe, h.plan.Watchlist = nil, []string{"GLD"}
	h.c.Step(context.Background())
	if st, _ := h.archive.Stats(); st.Active != 1 {
		t.Errorf("%d active with the lists off, want GLD alone", st.Active)
	}
}

func TestRunStopsCleanlyOnCancelAndFailsOnABrokenArchive(t *testing.T) {
	h := newHarness(t, []quotes.Interval{quotes.Daily})
	h.plan.Extras = []string{"AAPL"}
	ctx, cancel := context.WithCancel(context.Background())
	h.c.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	if err := h.c.Run(ctx); err != nil {
		t.Errorf("Run returned %v on cancellation, want nil", err)
	}

	h2 := newHarness(t, []quotes.Interval{quotes.Daily})
	h2.plan.Extras = []string{"AAPL"}
	h2.archive.Close()
	if err := h2.c.Run(context.Background()); err == nil {
		t.Error("Run kept going on a closed archive")
	}
	if st := h2.c.Status(); st.State != StateStopped {
		t.Errorf("state = %q after the archive failed, want stopped", st.State)
	}
}

func TestParseSymbols(t *testing.T) {
	got := ParseSymbols(" btc-usd, ,^gspc,GLD ")
	if fmt.Sprint(got) != "[BTC-USD ^GSPC GLD]" {
		t.Errorf("ParseSymbols = %v", got)
	}
}
