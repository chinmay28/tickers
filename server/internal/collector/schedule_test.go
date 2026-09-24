package collector

import (
	"errors"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

var now = time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)

func policy(t *testing.T, i quotes.Interval) Policy {
	t.Helper()
	ps, err := Policies([]quotes.Interval{i})
	if err != nil {
		t.Fatal(err)
	}
	return ps[0]
}

func tgt(t *testing.T, symbol string, i quotes.Interval, cov archive.Coverage) target {
	t.Helper()
	p := policy(t, i)
	cov.Interval = i
	order := map[quotes.Interval]int{quotes.Daily: 0, quotes.FiveMinute: 1, quotes.Hourly: 2}[i]
	return target{symbol: symbol, policy: p, order: order, coverage: cov}
}

func TestNextPrefersAtRiskThenForwardThenSeedThenBackfill(t *testing.T) {
	targets := []target{
		// A daily series ten years deep, still digging.
		tgt(t, "DEEP", quotes.Daily, archive.Coverage{Oldest: now.AddDate(-10, 0, 0), Newest: now.Add(-time.Hour)}),
		// A daily series only one year deep.
		tgt(t, "SHALLOW", quotes.Daily, archive.Coverage{Oldest: now.AddDate(-1, 0, 0), Newest: now.Add(-time.Hour)}),
		// Never fetched.
		tgt(t, "NEW", quotes.FiveMinute, archive.Coverage{}),
		// Due forward, a day stale.
		tgt(t, "STALE", quotes.Daily, archive.Coverage{Oldest: now.AddDate(-30, 0, 0), Newest: now.Add(-25 * time.Hour), Complete: true}),
		// Five-minute bars 55 days stale: four days from losing bars for good.
		tgt(t, "RISK", quotes.FiveMinute, archive.Coverage{Oldest: now.Add(-80 * day), Newest: now.Add(-55 * day), Complete: true}),
	}

	var order []string
	for len(order) < len(targets) {
		tk, ok := next(targets, now)
		if !ok {
			break
		}
		order = append(order, targets[tk.target].symbol+"/"+tk.kind.String())
		// Take it off the board.
		targets[tk.target].coverage.NextAttempt = now.Add(time.Hour)
	}
	want := []string{"RISK/forward", "STALE/forward", "NEW/seed", "SHALLOW/backward", "DEEP/backward"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v — breadth-first backfill after keeping up, and anything near the horizon first", order, want)
		}
	}
}

func TestNextSkipsSeriesWaitingOutABackoffAndFinishedOnes(t *testing.T) {
	targets := []target{
		tgt(t, "WAIT", quotes.Daily, archive.Coverage{NextAttempt: now.Add(time.Minute)}),
		tgt(t, "DONE", quotes.Daily, archive.Coverage{Oldest: now.AddDate(-40, 0, 0), Newest: now.Add(-time.Hour), Complete: true}),
	}
	if tk, ok := next(targets, now); ok {
		t.Fatalf("next = %s/%v, want nothing due", targets[tk.target].symbol, tk.kind)
	}
	w := wake(targets, now)
	if !w.Equal(now.Add(time.Minute)) {
		t.Errorf("wake = %s, want the backoff's end a minute out", w)
	}
}

func TestSeedWindows(t *testing.T) {
	targets := []target{tgt(t, "A", quotes.Daily, archive.Coverage{}), tgt(t, "B", quotes.FiveMinute, archive.Coverage{})}
	tk, _ := next(targets, now)
	if tk.kind != seed || !tk.to.Equal(now) || !tk.from.Equal(now.Add(-3652*day)) {
		t.Errorf("daily seed = %v %s–%s, want the last decade", tk.kind, tk.from, tk.to)
	}
	targets[0].coverage.NextAttempt = now.Add(time.Hour)
	tk, _ = next(targets, now)
	if !tk.from.Equal(now.Add(-59 * day)) {
		t.Errorf("5m seed starts %s, want the whole 59-day horizon in one request", tk.from)
	}
}

func TestForwardOverlapsAndClampsToTheHorizon(t *testing.T) {
	p := policy(t, quotes.FiveMinute)
	fresh := forwardTask(0, p, archive.Coverage{Newest: now.Add(-2 * day)}, now)
	if !fresh.from.Equal(now.Add(-3*day)) || !fresh.to.Equal(now) {
		t.Errorf("forward = %s–%s, want a day of overlap before the newest point", fresh.from, fresh.to)
	}
	// Off for three months: the gap past the horizon is gone for good, and
	// asking for it would fail every time.
	lost := forwardTask(0, p, archive.Coverage{Newest: now.Add(-90 * day)}, now)
	if !lost.from.Equal(now.Add(-59 * day)) {
		t.Errorf("a series past the horizon resumes at %s, want the horizon's edge", lost.from)
	}
	// A daily series years behind catches up a span at a time.
	d := policy(t, quotes.Daily)
	behind := forwardTask(0, d, archive.Coverage{Newest: now.AddDate(-20, 0, 0)}, now)
	if got := behind.to.Sub(behind.from); got != d.Span {
		t.Errorf("catch-up window is %s, want one span", got)
	}
}

func TestAdvanceMarksTheBeginning(t *testing.T) {
	daily := tgt(t, "AAPL", quotes.Daily, archive.Coverage{Oldest: now.AddDate(-10, 0, 0), Newest: now})
	back := backwardTask(0, daily.policy, daily.coverage, now)
	if !back.to.Equal(daily.coverage.Oldest) || back.to.Sub(back.from) != daily.policy.Span {
		t.Fatalf("backward = %s–%s, want one span ending where the coverage starts", back.from, back.to)
	}

	// Still more history behind this window.
	got := advance(daily, back, quotes.CandleSeries{FirstTrade: back.from.AddDate(-5, 0, 0), Candles: make([]quotes.Candle, 1)}, now)
	if got.Complete || !got.Oldest.Equal(back.from) || !got.Newest.Equal(now) {
		t.Errorf("mid-history backfill = %+v, want oldest moved and not complete", got)
	}
	// This window reaches the listing.
	got = advance(daily, back, quotes.CandleSeries{FirstTrade: back.from.AddDate(2, 0, 0)}, now)
	if !got.Complete {
		t.Error("a window reaching the first trade date did not complete the backfill")
	}
	// No first trade date and an empty window: nothing further back.
	got = advance(daily, back, quotes.CandleSeries{}, now)
	if !got.Complete {
		t.Error("an empty window with no first trade date did not complete the backfill")
	}
	// A seed that comes back empty is a new listing, not the end of history.
	seeded := advance(tgt(t, "IPO", quotes.Daily, archive.Coverage{}), seedTask(0, daily.policy, now), quotes.CandleSeries{}, now)
	if seeded.Complete {
		t.Error("an empty first fetch completed a backfill that has not been tried")
	}
	// An intraday seed reaches the horizon in one go.
	five := tgt(t, "AAPL", quotes.FiveMinute, archive.Coverage{})
	got = advance(five, seedTask(0, five.policy, now), quotes.CandleSeries{}, now)
	if !got.Complete || !got.Newest.Equal(now) {
		t.Errorf("5m seed = %+v, want complete — there is nothing past the horizon to ask for", got)
	}
}

func TestAdvanceClearsAndFailBacksOff(t *testing.T) {
	x := tgt(t, "X", quotes.Daily, archive.Coverage{})
	var waits []time.Duration
	for i := 0; i < 10; i++ {
		x.coverage = fail(x, errors.New("boom"), now)
		waits = append(waits, x.coverage.NextAttempt.Sub(now))
	}
	if waits[0] != time.Hour || waits[1] != 2*time.Hour || waits[9] != maxBackoff {
		t.Errorf("backoff = %v, want 1h doubling to a %s cap", waits, maxBackoff)
	}
	if x.coverage.Failures != 10 || x.coverage.LastError != "boom" {
		t.Errorf("failure record = %+v", x.coverage)
	}
	ok := advance(x, seedTask(0, x.policy, now), quotes.CandleSeries{}, now)
	if ok.Failures != 0 || !ok.NextAttempt.IsZero() || ok.LastError != "" {
		t.Errorf("a success left %+v, want the failure record cleared", ok)
	}
}

func TestSettledDropsTheBarInProgress(t *testing.T) {
	bars := []quotes.Candle{{Time: now.Add(-10 * time.Minute)}, {Time: now.Add(-5 * time.Minute)}, {Time: now.Add(-2 * time.Minute)}}
	if got := settled(quotes.FiveMinute, bars, now); len(got) != 2 {
		t.Errorf("kept %d five-minute bars, want 2 — the one still open is keyed by its last trade", len(got))
	}
	if got := settled(quotes.Daily, bars, now); len(got) != 3 {
		t.Errorf("kept %d daily bars, want all 3 — a daily bar is keyed by date and overwrites itself", len(got))
	}
}

func TestPoliciesRefuseUnknownIntervals(t *testing.T) {
	if _, err := Policies(nil); err == nil {
		t.Error("an empty interval list was accepted")
	}
	if _, err := Policies([]quotes.Interval{"1m"}); err == nil {
		t.Error("an interval with no policy was accepted")
	}
	ps, err := Policies([]quotes.Interval{quotes.Daily, quotes.Daily, quotes.FiveMinute})
	if err != nil || len(ps) != 2 {
		t.Errorf("duplicates gave %d policies (err %v), want 2", len(ps), err)
	}
}
