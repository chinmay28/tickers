package collector

import (
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// kind is what a task does to a series.
type kind int

const (
	// seed is a series' first fetch from a source: the most recent span.
	seed kind = iota
	// forward extends a series to now. Only the live source does it.
	forward
	// backward extends a series one span further into the past.
	backward
)

func (k kind) String() string {
	return [...]string{"seed", "forward", "backward"}[k]
}

// target is one symbol at one interval from one source: what the scheduler
// chooses between.
type target struct {
	symbolID   int64
	symbol     string
	firstTrade time.Time
	priority   bool
	interval   quotes.Interval
	source     int // index into the collector's sources
	reach      Reach
	// order is the interval's position in the configured list, for
	// tie-breaks: the first configured interval goes first.
	order  int
	cursor archive.Cursor
}

// task is one window to fetch.
type task struct {
	kind     kind
	target   int // index into the target slice
	from, to time.Time
}

// riskMargin is how close to the horizon a series may drift before its
// forward fetch outranks everything else — enough slack to ride out a run of
// rate limiting without losing a bar, but never more than a quarter of the
// horizon, or a short one would be at risk from the day it was fetched.
func riskMargin(horizon time.Duration) time.Duration {
	return min(7*day, horizon/4)
}

// Priority tiers within a rank, lowest first.
const (
	tierForward = iota
	tierSeed
	tierBackward
)

// rank orders candidate tasks: lower is more urgent, compared field by field.
type rank struct {
	safe     int // 0 when a series is about to lose bars off its horizon
	ordinary int // 0 for a priority symbol
	tier     int
	key      time.Duration // within a tier: stalest, or shallowest, first
	order    int
	id       int64
}

func (r rank) less(o rank) bool {
	switch {
	case r.safe != o.safe:
		return r.safe < o.safe
	case r.ordinary != o.ordinary:
		return r.ordinary < o.ordinary
	case r.tier != o.tier:
		return r.tier < o.tier
	case r.key != o.key:
		return r.key < o.key
	case r.order != o.order:
		return r.order < o.order
	}
	return r.id < o.id
}

// next chooses the most urgent task among the sources ready to make a
// request, or reports false when nothing is due from any of them.
//
// The order is what makes the collection gradual in the right way:
//
//  1. A series about to lose bars off the end of a source's horizon.
//  2. Priority symbols — what the app itself shows, and what was added by
//     hand — ahead of the rest of the market, in the order below.
//  3. Forward fetches, stalest first: the archive keeps up before it digs.
//  4. First fetches, in the configured order of intervals.
//  5. Backfill, shallowest series first. Every symbol gets its second decade
//     before any symbol gets its third, so a half-finished archive is evenly
//     deep rather than complete for A–F and empty for the rest.
func next(targets []target, now time.Time, ready func(source int) bool) (task, bool) {
	var best task
	var bestRank rank
	found := false
	for i := range targets {
		t := &targets[i]
		c := t.cursor
		if c.NextAttempt.After(now) || (ready != nil && !ready(t.source)) {
			continue
		}
		r := rank{safe: 1, ordinary: 1, order: t.order, id: t.symbolID}
		if t.priority {
			r.ordinary = 0
		}
		var tk task
		switch {
		case c.Newest.IsZero():
			tk, r.tier = seedTask(i, t.reach, now), tierSeed
		case forwardDue(t.reach, c, now):
			tk = forwardTask(i, t.reach, c, now)
			r.tier, r.key = tierForward, c.Newest.Sub(now) // older newest → more negative → first
			if atRisk(t.reach, c, now) {
				r.safe = 0
			}
		case !c.Complete:
			tk = backwardTask(i, t.reach, c, now)
			if !tk.from.Before(tk.to) {
				continue
			}
			r.tier, r.key = tierBackward, now.Sub(c.Oldest)
		default:
			continue
		}
		if !found || r.less(bestRank) {
			best, bestRank, found = tk, r, true
		}
	}
	return best, found
}

// wake is the earliest moment something becomes due, or zero if nothing ever
// will.
func wake(targets []target, now time.Time) time.Time {
	var earliest time.Time
	for i := range targets {
		t := &targets[i]
		c := t.cursor
		var due time.Time
		switch {
		case c.Newest.IsZero() || !c.Complete:
			due = now
		case t.reach.Every > 0:
			due = c.Newest.Add(t.reach.Every)
		default:
			continue
		}
		if c.NextAttempt.After(due) {
			due = c.NextAttempt
		}
		if earliest.IsZero() || due.Before(earliest) {
			earliest = due
		}
	}
	return earliest
}

func forwardDue(r Reach, c archive.Cursor, now time.Time) bool {
	return r.Every > 0 && now.Sub(c.Newest) >= r.Every
}

func atRisk(r Reach, c archive.Cursor, now time.Time) bool {
	return r.Horizon > 0 && now.Sub(c.Newest) > r.Horizon-riskMargin(r.Horizon)
}

// edge is the oldest instant a source will serve, or zero for no limit.
func edge(r Reach, now time.Time) time.Time {
	if r.Horizon == 0 {
		return time.Time{}
	}
	return now.Add(-r.Horizon)
}

func seedTask(i int, r Reach, now time.Time) task {
	from := now.Add(-r.Span)
	if e := edge(r, now); !e.IsZero() && from.Before(e) {
		from = e
	}
	return task{kind: seed, target: i, from: from, to: now}
}

func forwardTask(i int, r Reach, c archive.Cursor, now time.Time) task {
	from := c.Newest.Add(-r.Overlap)
	// A series that fell past the horizon — the collector was off for longer
	// than the source keeps this interval — has a gap this source can't fill.
	// Resume from the edge rather than ask for a window that will be refused;
	// another source may fill the gap later.
	if e := edge(r, now); !e.IsZero() && from.Before(e) {
		from = e
	}
	to := now
	if from.Add(r.Span).Before(to) {
		to = from.Add(r.Span)
	}
	return task{kind: forward, target: i, from: from, to: to}
}

func backwardTask(i int, r Reach, c archive.Cursor, now time.Time) task {
	from := c.Oldest.Add(-r.Span)
	if e := edge(r, now); !e.IsZero() && from.Before(e) {
		from = e
	}
	return task{kind: backward, target: i, from: from, to: c.Oldest}
}

// edgeSlack is how close to the horizon a backfill has to reach to count as
// complete. The edge moves every second, so "reached it exactly" never holds.
const edgeSlack = day

// advance is the cursor a successful window leaves behind. fetched is false
// for a window skipped because the archive already held all of it: that
// moves the cursor like a fetch, but says nothing about where history begins.
func advance(t target, tk task, got quotes.CandleSeries, fetched bool, now time.Time) archive.Cursor {
	c := t.cursor
	switch tk.kind {
	case seed:
		c.Oldest, c.Newest = tk.from, tk.to
	case forward:
		c.Newest = tk.to
	case backward:
		c.Oldest = tk.from
	}
	if tk.kind != forward && !c.Complete {
		firstTrade := got.FirstTrade
		if firstTrade.IsZero() {
			firstTrade = t.firstTrade
		}
		switch {
		case !firstTrade.IsZero() && !tk.from.After(firstTrade):
			// Reached the listing.
			c.Complete = true
		case t.reach.Horizon > 0 && !tk.from.After(now.Add(-t.reach.Horizon+edgeSlack)):
			// Reached the source's edge.
			c.Complete = true
		case fetched && firstTrade.IsZero() && tk.kind == backward && len(got.Candles) == 0:
			// No first trade date to go by, and a window with nothing in it:
			// the beginning is somewhere behind us.
			c.Complete = true
		}
	}
	c.Failures, c.NextAttempt, c.LastError = 0, time.Time{}, ""
	return c
}

// maxBackoff caps how long a failing series waits between attempts. A week:
// a symbol a source has never heard of costs one request a week, not one a
// cycle, and one that was only briefly broken is back within days.
const maxBackoff = 7 * day

// fail is the cursor a failed fetch leaves behind: an hour's wait, doubling.
func fail(t target, err error, now time.Time) archive.Cursor {
	c := t.cursor
	c.Failures++
	wait := time.Hour
	for i := 1; i < c.Failures && wait < maxBackoff; i++ {
		wait *= 2
	}
	c.NextAttempt = now.Add(min(wait, maxBackoff))
	msg := err.Error()
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	c.LastError = msg
	return c
}

// settled drops the bars that hadn't finished when they were fetched.
//
// An intraday bar still in progress is keyed by the latest trade rather than
// the bar's start, so it would land as an extra row between two real ones.
// The next forward fetch overlaps it and reads it finished. A daily bar is
// keyed by its date instead, so the in-progress one simply gets overwritten,
// and is kept for its freshness.
func settled(interval quotes.Interval, candles []quotes.Candle, now time.Time) []quotes.Candle {
	if !interval.Intraday() {
		return candles
	}
	step := interval.Step()
	out := candles[:0:0]
	for _, c := range candles {
		if !c.Time.Add(step).After(now) {
			out = append(out, c)
		}
	}
	return out
}

// missing narrows a backfill window to the trading days the archive doesn't
// hold yet, reporting false when it holds them all.
//
// trading is the symbol's own calendar, from its daily bars. Without one
// there is no telling a gap from a holiday, so the whole window is asked for;
// the archive's fill-only writes mean asking again costs a request, never a
// bar.
func missing(from, to time.Time, trading []int64, held map[int64]bool) (time.Time, time.Time, bool) {
	if len(trading) == 0 {
		return from, to, true
	}
	var first, last int64 = -1, -1
	for _, d := range trading {
		if d < from.Unix()-86400 || d >= to.Unix() || held[d] {
			continue
		}
		if first < 0 || d < first {
			first = d
		}
		if d > last {
			last = d
		}
	}
	if first < 0 {
		return from, to, false
	}
	f, t := time.Unix(first, 0).UTC(), time.Unix(last+86400, 0).UTC()
	if f.Before(from) {
		f = from
	}
	if t.After(to) {
		t = to
	}
	return f, t, f.Before(t)
}
