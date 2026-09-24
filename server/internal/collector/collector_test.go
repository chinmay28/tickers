package collector

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/universe"
)

// call is one request the fake provider saw.
type call struct {
	at       time.Time
	symbol   string
	interval quotes.Interval
	from, to time.Time
}

// fakeProvider answers every window with one bar a week inside it, and
// reports a first trade date so a daily backfill has somewhere to stop.
type fakeProvider struct {
	clock      *clock
	calls      []call
	firstTrade time.Time
	// fail, when set, decides a request's error.
	fail func(symbol string, n int) error
}

func (f *fakeProvider) Candles(_ context.Context, symbol string, interval quotes.Interval, from, to time.Time) (quotes.CandleSeries, error) {
	f.calls = append(f.calls, call{f.clock.t, symbol, interval, from, to})
	if f.fail != nil {
		if err := f.fail(symbol, len(f.calls)); err != nil {
			return quotes.CandleSeries{}, err
		}
	}
	var out quotes.CandleSeries
	out.FirstTrade = f.firstTrade
	start := from
	if start.Before(f.firstTrade) {
		start = f.firstTrade
	}
	for d := start.Truncate(day); d.Before(to); d = d.Add(7 * day) {
		out.Candles = append(out.Candles, quotes.Candle{Time: d, Open: 1, High: 1, Low: 1, Close: 1, Volume: 1})
	}
	return out, nil
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
	c        *Collector
	archive  *archive.Archive
	provider *fakeProvider
	lister   *fakeLister
	clock    *clock
}

func newHarness(t *testing.T, cfg Config, listings ...string) *harness {
	t.Helper()
	a, err := archive.Open(filepath.Join(t.TempDir(), "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	clk := &clock{t: now}
	p := &fakeProvider{clock: clk, firstTrade: now.AddDate(-25, 0, 0)}
	l := &fakeLister{}
	for _, s := range listings {
		l.listings = append(l.listings, universe.Listing{Symbol: s, Exchange: "NASDAQ"})
	}
	if cfg.Universe == nil && len(listings) > 0 {
		cfg.Universe = l
	}
	c, err := New(a, p, cfg, nil)
	if err != nil {
		t.Fatalf("new collector: %v", err)
	}
	c.now, c.sleep = clk.now, clk.sleep
	return &harness{c: c, archive: a, provider: p, lister: l, clock: clk}
}

// run steps until n requests have gone out, failing if the loop stops making
// them first.
func (h *harness) run(t *testing.T, n int) {
	t.Helper()
	for idle := 0; len(h.provider.calls) < n; {
		did, err := h.c.Step(context.Background())
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		if !did {
			if idle++; idle > 1000 {
				t.Fatalf("stopped after %d requests, want %d", len(h.provider.calls), n)
			}
		}
	}
}

func coverageOf(t *testing.T, a *archive.Archive, symbol string, i quotes.Interval) archive.Coverage {
	t.Helper()
	syms, _ := a.ActiveSymbols()
	covs, _ := a.Coverages()
	for _, s := range syms {
		if s.Symbol != symbol {
			continue
		}
		for _, c := range covs {
			if c.SymbolID == s.ID && c.Interval == i {
				return c
			}
		}
	}
	return archive.Coverage{}
}

func TestCollectorSeedsEverythingThenDigsBackToTheListing(t *testing.T) {
	h := newHarness(t, Config{
		Intervals: []quotes.Interval{quotes.Daily, quotes.FiveMinute},
		Extras:    []string{"btc-usd"},
		Spacing:   2 * time.Second,
	}, "AAPL", "GLD")

	// 3 symbols × 2 intervals of seeds, then two more daily decades each (the
	// listing is 25 years back and a seed covers one).
	h.run(t, 6+3*2)

	seen := map[string]bool{}
	for i, c := range h.provider.calls {
		if i > 0 {
			if gap := c.at.Sub(h.provider.calls[i-1].at); gap < 2*time.Second {
				t.Fatalf("requests %d and %d were %s apart, want the 2s spacing", i-1, i, gap)
			}
		}
		seen[c.symbol+"/"+string(c.interval)] = true
	}
	for _, s := range []string{"AAPL", "GLD", "BTC-USD"} {
		for _, i := range []quotes.Interval{quotes.Daily, quotes.FiveMinute} {
			if !seen[s+"/"+string(i)] {
				t.Errorf("%s at %s was never fetched", s, i)
			}
		}
	}
	// Seeds — windows ending now — come before any backfill.
	for i, c := range h.provider.calls {
		if isSeed := c.at.Sub(c.to) < time.Minute; isSeed != (i < 6) {
			t.Fatalf("request %d (%s %s, %s–%s) is out of order: every series gets its first fetch before any is backfilled",
				i, c.symbol, c.interval, c.from.Format(time.DateOnly), c.to.Format(time.DateOnly))
		}
	}

	for _, s := range []string{"AAPL", "GLD", "BTC-USD"} {
		d := coverageOf(t, h.archive, s, quotes.Daily)
		if !d.Complete || d.Oldest.After(h.provider.firstTrade) {
			t.Errorf("%s daily coverage = %+v, want complete back to the listing", s, d)
		}
		f := coverageOf(t, h.archive, s, quotes.FiveMinute)
		if !f.Complete {
			t.Errorf("%s 5m coverage = %+v, want complete after one request", s, f)
		}
	}
	bars, _ := h.archive.Candles("AAPL", quotes.Daily, h.provider.firstTrade, now.Add(day))
	if len(bars) < 52*25 {
		t.Errorf("AAPL has %d weekly-spaced bars, want 25 years of them", len(bars))
	}

	// Caught up: the next request is a forward fetch, a day later.
	before := len(h.provider.calls)
	h.run(t, before+1)
	c := h.provider.calls[before]
	if c.at.Sub(now) < 20*time.Hour || c.from.After(c.to) {
		t.Errorf("first forward fetch went at %s for %s–%s, want once the series was 20h stale", c.at, c.from, c.to)
	}
}

func TestCollectorWaitsOutRateLimitingWithoutBlamingTheSymbol(t *testing.T) {
	h := newHarness(t, Config{Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"AAPL"}, Spacing: time.Second})
	h.provider.fail = func(symbol string, n int) error {
		if n == 1 {
			return fmt.Errorf("%s: %w", symbol, quotes.ErrRateLimited)
		}
		return nil
	}
	h.run(t, 2)
	if gap := h.provider.calls[1].at.Sub(h.provider.calls[0].at); gap < time.Minute {
		t.Errorf("retried %s after a 429, want at least a minute", gap)
	}
	if cov := coverageOf(t, h.archive, "AAPL", quotes.Daily); cov.Failures != 0 || cov.Newest.IsZero() {
		t.Errorf("coverage after a 429 then a success = %+v, want no failure charged and the seed recorded", cov)
	}
}

func TestCollectorBacksOffAFailingSymbolAndCarriesOn(t *testing.T) {
	h := newHarness(t, Config{Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"ZZZZ", "AAPL"}, Spacing: time.Second})
	h.provider.fail = func(symbol string, n int) error {
		if symbol == "ZZZZ" {
			return fmt.Errorf("%s: %w", symbol, quotes.ErrNotFound)
		}
		return nil
	}
	h.run(t, 2)
	cov := coverageOf(t, h.archive, "ZZZZ", quotes.Daily)
	if cov.Failures != 1 || cov.LastError == "" || !cov.NextAttempt.After(h.clock.t) {
		t.Fatalf("failing symbol's coverage = %+v, want one failure and a retry in the future", cov)
	}
	if aapl := coverageOf(t, h.archive, "AAPL", quotes.Daily); aapl.Newest.IsZero() {
		t.Error("one symbol failing held up the next")
	}
	// ZZZZ is not asked again until its backoff runs out.
	first := -1
	for i, c := range h.provider.calls {
		if c.symbol == "ZZZZ" {
			first = i
			break
		}
	}
	h.run(t, 6)
	for _, c := range h.provider.calls[first+1:] {
		if c.symbol == "ZZZZ" && c.at.Before(cov.NextAttempt) {
			t.Fatalf("ZZZZ retried at %s, before its backoff ended at %s", c.at, cov.NextAttempt)
		}
	}
}

func TestCollectorRetiresDelistingsButNotOnABrokenList(t *testing.T) {
	h := newHarness(t, Config{Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"BTC-USD"}, Spacing: time.Second},
		"AAPL", "GLD", "MSFT", "IBM")
	h.run(t, 1)
	if st, _ := h.archive.Stats(); st.Active != 5 {
		t.Fatalf("%d active after the first sync, want 4 listed + 1 extra", st.Active)
	}

	// A day later the directory fails outright: nothing is retired, the
	// extras keep going, and it is retried within the hour.
	h.lister.err = errors.New("timeout")
	h.clock.t = h.clock.t.Add(25 * time.Hour)
	calls := h.lister.calls
	h.run(t, len(h.provider.calls)+1)
	if st, _ := h.archive.Stats(); st.Active != 5 {
		t.Errorf("%d active after a failed read, want all 5 kept", st.Active)
	}
	if h.lister.calls != calls+1 || h.c.nextUniverse.Sub(h.clock.t) > universeRetry {
		t.Errorf("failed read retries at %s, want within %s", h.c.nextUniverse, universeRetry)
	}

	// Then it comes back with one symbol gone: that one is retired.
	h.lister.err = nil
	h.lister.listings = h.lister.listings[:3]
	h.clock.t = h.c.nextUniverse
	h.run(t, len(h.provider.calls)+1)
	if st, _ := h.archive.Stats(); st.Active != 4 || st.Retired != 1 {
		t.Errorf("stats = %d active, %d retired; want IBM retired", st.Active, st.Retired)
	}

	// And a list that comes back a fraction of its size is not believed.
	h.lister.listings = nil
	h.clock.t = h.c.nextUniverse
	h.run(t, len(h.provider.calls)+1)
	if st, _ := h.archive.Stats(); st.Active != 4 {
		t.Errorf("%d active after an empty list, want nothing retired", st.Active)
	}
}

func TestNewRefusesAConfigurationThatCollectsNothing(t *testing.T) {
	a, err := archive.Open(filepath.Join(t.TempDir(), "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	p := &fakeProvider{clock: &clock{}}
	cases := map[string]Config{
		"no symbols":        {Intervals: []quotes.Interval{quotes.Daily}},
		"no intervals":      {Extras: []string{"AAPL"}},
		"spacing too tight": {Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"AAPL"}, Spacing: time.Millisecond},
	}
	for name, cfg := range cases {
		if _, err := New(a, p, cfg, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := New(nil, p, Config{Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"AAPL"}}, nil); err == nil {
		t.Error("no archive: accepted")
	}
}

func TestRunStopsCleanlyOnCancel(t *testing.T) {
	h := newHarness(t, Config{Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"AAPL"}, Spacing: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	h.c.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- h.c.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on cancellation, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestParseSymbols(t *testing.T) {
	got := ParseSymbols(" btc-usd, ,^gspc,GLD ")
	want := []string{"BTC-USD", "^GSPC", "GLD"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ParseSymbols = %v, want %v", got, want)
	}
}

func TestANewExtraIsTrackedAtStartupNotAtTheNextListRead(t *testing.T) {
	h := newHarness(t, Config{Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"BTC-USD"}, Spacing: time.Second}, "AAPL")
	h.run(t, 1)

	// Restarted an hour later with another extra: the lists aren't due for a
	// day, and the new symbol must not wait for them.
	c, err := New(h.archive, h.provider, Config{
		Intervals: []quotes.Interval{quotes.Daily}, Extras: []string{"BTC-USD", "ETH-USD"}, Spacing: time.Second, Universe: h.lister,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.t = h.clock.t.Add(time.Hour)
	c.now, c.sleep = h.clock.now, h.clock.sleep
	h.c = c
	reads := h.lister.calls
	h.run(t, 4)
	if h.lister.calls != reads {
		t.Errorf("a restart re-read the lists %d times; they were read an hour ago", h.lister.calls-reads)
	}
	if cov := coverageOf(t, h.archive, "ETH-USD", quotes.Daily); cov.Newest.IsZero() {
		t.Error("the new extra was not collected after a restart")
	}
}
