package collector

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

var now = time.Date(2026, 9, 24, 18, 0, 0, 0, time.UTC)

// tgt is a Yahoo target with the reach the collector would give it when
// minute bars are collected too.
func tgt(symbol string, i quotes.Interval, cur archive.Cursor) target {
	all := []quotes.Interval{quotes.Daily, quotes.OneMinute, quotes.FiveMinute, quotes.Hourly}
	r, _ := reachFor(Source{Reach: YahooReach, Live: true}, i, all)
	order := map[quotes.Interval]int{quotes.Daily: 0, quotes.OneMinute: 1, quotes.FiveMinute: 2, quotes.Hourly: 3}[i]
	cur.Interval = i
	return target{symbol: symbol, interval: i, reach: r, order: order, cursor: cur}
}

func drain(targets []target) []string {
	var order []string
	for {
		tk, ok := next(targets, now, nil)
		if !ok {
			return order
		}
		order = append(order, targets[tk.target].symbol+"/"+tk.kind.String())
		targets[tk.target].cursor.NextAttempt = now.Add(time.Hour)
	}
}

func TestNextOrdersRiskThenPriorityThenForwardSeedBackfill(t *testing.T) {
	vip := tgt("VIP", quotes.Daily, archive.Cursor{Oldest: now.AddDate(-20, 0, 0), Newest: now.Add(-time.Hour)})
	vip.priority = true
	targets := []target{
		tgt("DEEP", quotes.Daily, archive.Cursor{Oldest: now.AddDate(-10, 0, 0), Newest: now.Add(-time.Hour)}),
		tgt("SHALLOW", quotes.Daily, archive.Cursor{Oldest: now.AddDate(-1, 0, 0), Newest: now.Add(-time.Hour)}),
		tgt("NEW", quotes.OneMinute, archive.Cursor{}),
		tgt("STALE", quotes.Daily, archive.Cursor{Oldest: now.AddDate(-30, 0, 0), Newest: now.Add(-25 * time.Hour), Complete: true}),
		// Minute bars 24 days stale: within a quarter-horizon of losing bars.
		tgt("RISK", quotes.OneMinute, archive.Cursor{Oldest: now.Add(-40 * day), Newest: now.Add(-24 * day), Complete: true}),
		vip,
	}
	got := drain(targets)
	want := []string{"RISK/forward", "VIP/backward", "STALE/forward", "NEW/seed", "SHALLOW/backward", "DEEP/backward"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v — near-horizon first, then priority symbols, then keeping up, then breadth-first digging", got, want)
	}
}

func TestNextOnlyChoosesReadySources(t *testing.T) {
	a := tgt("A", quotes.Daily, archive.Cursor{})
	b := tgt("B", quotes.Daily, archive.Cursor{})
	b.source = 1
	targets := []target{a, b}
	tk, ok := next(targets, now, func(s int) bool { return s == 1 })
	if !ok || targets[tk.target].symbol != "B" {
		t.Fatalf("with source 0 waiting, next = %v/%v, want B from the ready source", ok, tk)
	}
	if _, ok := next(targets, now, func(int) bool { return false }); ok {
		t.Error("a task was chosen with no source ready")
	}
}

func TestFinerForwardMakesCoarserIntradayOneShot(t *testing.T) {
	yahoo := Source{Reach: YahooReach, Live: true}
	all := []quotes.Interval{quotes.Daily, quotes.OneMinute, quotes.FiveMinute, quotes.Hourly}
	if r, _ := reachFor(yahoo, quotes.FiveMinute, all); r.Every != 0 {
		t.Error("five-minute bars go forward although minute bars do — they can be built from them")
	}
	if r, _ := reachFor(yahoo, quotes.Daily, all); r.Every == 0 {
		t.Error("daily bars stopped going forward; a daily bar is the official session and is never built")
	}
	noMinutes := []quotes.Interval{quotes.Daily, quotes.FiveMinute, quotes.Hourly}
	if r, _ := reachFor(yahoo, quotes.FiveMinute, noMinutes); r.Every == 0 {
		t.Error("with minute bars off, five-minute bars must go forward themselves")
	}
	if r, _ := reachFor(Source{Reach: YahooReach}, quotes.Daily, all); r.Every != 0 {
		t.Error("a source that isn't live went forward; it only fills gaps")
	}
	if _, ok := reachFor(Source{Reach: map[quotes.Interval]Reach{quotes.Daily: {}}}, quotes.OneMinute, all); ok {
		t.Error("an interval the source doesn't serve got a reach")
	}
}

func TestOneShotSeriesNeverWakeTheLoop(t *testing.T) {
	five := tgt("A", quotes.FiveMinute, archive.Cursor{Oldest: now.Add(-59 * day), Newest: now.Add(-30 * day), Complete: true})
	if _, ok := next([]target{five}, now, nil); ok {
		t.Error("a finished one-shot backfill was scheduled again")
	}
	if w := wake([]target{five}, now); !w.IsZero() {
		t.Errorf("wake = %s for a finished one-shot series, want never", w)
	}
	waiting := tgt("B", quotes.Daily, archive.Cursor{NextAttempt: now.Add(time.Minute)})
	if w := wake([]target{five, waiting}, now); !w.Equal(now.Add(time.Minute)) {
		t.Errorf("wake = %s, want the backoff's end", w)
	}
}

func TestMinuteBarsWalkBackAWeekAtATimeToTheHorizon(t *testing.T) {
	m := tgt("A", quotes.OneMinute, archive.Cursor{})
	var windows []string
	for i := 0; i < 10; i++ {
		tk, ok := next([]target{m}, now, nil)
		if !ok {
			break
		}
		windows = append(windows, fmt.Sprintf("%s:%s", tk.kind, tk.to.Sub(tk.from)))
		m.cursor = advance(m, tk, quotes.CandleSeries{Candles: make([]quotes.Candle, 1)}, true, now)
	}
	if len(windows) != 4 || windows[0] != "seed:168h0m0s" {
		t.Errorf("windows = %v, want a week's seed then weeks back to within a day of the 29-day horizon, four requests in all", windows)
	}
	if !m.cursor.Complete {
		t.Error("the walk reached the horizon and isn't complete")
	}
}

func TestForwardOverlapsAndClampsToTheHorizon(t *testing.T) {
	r := YahooReach[quotes.OneMinute]
	fresh := forwardTask(0, r, archive.Cursor{Newest: now.Add(-2 * day)}, now)
	if !fresh.from.Equal(now.Add(-3*day)) || !fresh.to.Equal(now) {
		t.Errorf("forward = %s–%s, want a day of overlap before the newest point", fresh.from, fresh.to)
	}
	lost := forwardTask(0, r, archive.Cursor{Newest: now.Add(-90 * day)}, now)
	if !lost.from.Equal(now.Add(-29*day)) || lost.to.Sub(lost.from) != r.Span {
		t.Errorf("a series past the horizon resumes %s–%s, want the edge and one span", lost.from, lost.to)
	}
}

func TestAdvanceKnowsWhereHistoryBegins(t *testing.T) {
	d := tgt("AAPL", quotes.Daily, archive.Cursor{Oldest: now.AddDate(-10, 0, 0), Newest: now})
	back := backwardTask(0, d.reach, d.cursor, now)
	if got := advance(d, back, quotes.CandleSeries{FirstTrade: back.from.AddDate(-5, 0, 0), Candles: make([]quotes.Candle, 1)}, true, now); got.Complete {
		t.Error("completed with history still behind the window")
	}
	if got := advance(d, back, quotes.CandleSeries{FirstTrade: back.from.AddDate(2, 0, 0)}, true, now); !got.Complete {
		t.Error("a window reaching the first trade date did not complete")
	}
	if got := advance(d, back, quotes.CandleSeries{}, true, now); !got.Complete {
		t.Error("an empty window with no first trade date did not complete")
	}
	if got := advance(d, back, quotes.CandleSeries{}, false, now); got.Complete {
		t.Error("a window skipped because it was held completed the walk — skipping says nothing about where history begins")
	}
	if got := advance(tgt("IPO", quotes.Daily, archive.Cursor{}), seedTask(0, d.reach, now), quotes.CandleSeries{}, true, now); got.Complete {
		t.Error("an empty first fetch completed a walk that hasn't started")
	}
}

func TestFailBacksOffAndAdvanceClears(t *testing.T) {
	x := tgt("X", quotes.Daily, archive.Cursor{})
	var waits []time.Duration
	for i := 0; i < 10; i++ {
		x.cursor = fail(x, errors.New("boom"), now)
		waits = append(waits, x.cursor.NextAttempt.Sub(now))
	}
	if waits[0] != time.Hour || waits[1] != 2*time.Hour || waits[9] != maxBackoff {
		t.Errorf("backoff = %v, want 1h doubling to %s", waits, maxBackoff)
	}
	ok := advance(x, seedTask(0, x.reach, now), quotes.CandleSeries{}, true, now)
	if ok.Failures != 0 || !ok.NextAttempt.IsZero() || ok.LastError != "" {
		t.Errorf("a success left %+v", ok)
	}
}

func TestSettledDropsTheBarInProgress(t *testing.T) {
	bars := []quotes.Candle{{Time: now.Add(-3 * time.Minute)}, {Time: now.Add(-time.Minute)}, {Time: now.Add(-20 * time.Second)}}
	if got := settled(quotes.OneMinute, bars, now); len(got) != 2 {
		t.Errorf("kept %d minute bars, want 2 — the open one is keyed by its last trade", len(got))
	}
	if got := settled(quotes.Daily, bars, now); len(got) != 3 {
		t.Errorf("kept %d daily bars, want all 3", len(got))
	}
}

func TestMissingNarrowsToTheDaysNotHeld(t *testing.T) {
	d := func(n int) int64 { return now.Truncate(day).AddDate(0, 0, n).Unix() }
	from, to := time.Unix(d(-10), 0).UTC(), time.Unix(d(0), 0).UTC()
	trading := []int64{d(-9), d(-8), d(-7), d(-3), d(-2)}

	f, tt, any := missing(from, to, trading, map[int64]bool{d(-9): true, d(-2): true})
	if !any || f.Unix() != d(-8) || tt.Unix() != d(-2) {
		t.Errorf("window = %s–%s, want day −8 up to the start of day −2 (days −8…−3 are missing)", f, tt)
	}
	if _, _, any := missing(from, to, trading, map[int64]bool{d(-9): true, d(-8): true, d(-7): true, d(-3): true, d(-2): true}); any {
		t.Error("a window whose every trading day is held still asked for something")
	}
	if f, tt, any := missing(from, to, nil, nil); !any || !f.Equal(from) || !tt.Equal(to) {
		t.Error("with no calendar the whole window must be asked for")
	}
}

func TestValidSources(t *testing.T) {
	p := &fakeProvider{}
	cases := map[string][]Source{
		"nameless":     {{Provider: p, Spacing: time.Second}},
		"duplicate":    {{Name: "a", Provider: p, Spacing: time.Second}, {Name: "a", Provider: p, Spacing: time.Second}},
		"too fast":     {{Name: "a", Provider: p, Spacing: time.Millisecond}},
		"two live":     {{Name: "a", Provider: p, Spacing: time.Second, Live: true}, {Name: "b", Provider: p, Spacing: time.Second, Live: true}},
		"providerless": {{Name: "a", Spacing: time.Second}},
	}
	for name, s := range cases {
		if err := validSources(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validSources([]Source{Yahoo(p, time.Second), {Name: "polygon", Provider: p, Spacing: time.Second}}); err != nil {
		t.Errorf("a live source and a gap-filler were refused: %v", err)
	}
}
