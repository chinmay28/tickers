package collector

import (
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// kind is what a task does to a series.
type kind int

const (
	// seed is a series' first fetch: the most recent Span, or everything the
	// provider keeps.
	seed kind = iota
	// forward extends a series to now.
	forward
	// backward extends a series one Span further into the past.
	backward
)

func (k kind) String() string {
	return [...]string{"seed", "forward", "backward"}[k]
}

// target is one symbol at one interval: what the scheduler chooses between.
type target struct {
	symbol     string
	firstTrade time.Time
	policy     Policy
	// order is the policy's position in the configured list, for tie-breaks.
	order    int
	coverage archive.Coverage
}

// task is one request to make.
type task struct {
	kind     kind
	target   int // index into the target slice
	from, to time.Time
}

// riskMargin is how close to the horizon a series may drift before its
// forward fetch outranks everything else. A week: enough slack to ride out a
// run of rate limiting without losing a single bar.
const riskMargin = 7 * day

// Priority tiers, lowest first.
const (
	tierAtRisk = iota
	tierForward
	tierSeed
	tierBackward
)

// next chooses the most urgent task, or reports false when nothing is due.
//
// The order is what makes the collection gradual in the right way:
//
//  1. A series about to lose bars off the end of the provider's horizon.
//  2. Forward fetches, stalest first — the archive keeps up before it digs.
//  3. First fetches, in the configured order of intervals.
//  4. Backfill, shallowest series first. Every symbol gets its second decade
//     before any symbol gets its third, so a half-finished archive is evenly
//     deep rather than complete for A–F and empty for the rest.
func next(targets []target, now time.Time) (task, bool) {
	best, found := task{}, false
	var bestTier int
	var bestKey time.Duration
	var bestOrder int

	for i := range targets {
		t := &targets[i]
		c := t.coverage
		if c.NextAttempt.After(now) {
			continue
		}
		var tk task
		var tier int
		var key time.Duration // smaller is more urgent, within a tier
		switch {
		case c.Newest.IsZero():
			tk, tier = seedTask(i, t.policy, now), tierSeed
		case forwardDue(t.policy, c, now):
			tk = forwardTask(i, t.policy, c, now)
			tier, key = tierForward, c.Newest.Sub(now) // older newest → more negative → first
			if atRisk(t.policy, c, now) {
				tier = tierAtRisk
			}
		case !c.Complete:
			tk = backwardTask(i, t.policy, c, now)
			if !tk.from.Before(tk.to) {
				continue
			}
			tier, key = tierBackward, now.Sub(c.Oldest)
		default:
			continue
		}
		if !found || tier < bestTier ||
			tier == bestTier && (key < bestKey || key == bestKey && t.order < bestOrder) {
			best, found = tk, true
			bestTier, bestKey, bestOrder = tier, key, t.order
		}
	}
	return best, found
}

// wake is the earliest moment something becomes due, or zero if nothing ever
// will — which only happens with no targets at all.
func wake(targets []target, now time.Time) time.Time {
	var earliest time.Time
	consider := func(at time.Time) {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	for i := range targets {
		c := targets[i].coverage
		due := now
		if !c.Newest.IsZero() && c.Complete {
			due = c.Newest.Add(targets[i].policy.Every)
		}
		if c.NextAttempt.After(due) {
			due = c.NextAttempt
		}
		consider(due)
	}
	return earliest
}

func forwardDue(p Policy, c archive.Coverage, now time.Time) bool {
	return p.Every > 0 && now.Sub(c.Newest) >= p.Every
}

func atRisk(p Policy, c archive.Coverage, now time.Time) bool {
	return p.Horizon > 0 && now.Sub(c.Newest) > p.Horizon-riskMargin
}

// edge is the oldest instant the provider will serve, or zero for no limit.
func edge(p Policy, now time.Time) time.Time {
	if p.Horizon == 0 {
		return time.Time{}
	}
	return now.Add(-p.Horizon)
}

func seedTask(i int, p Policy, now time.Time) task {
	return task{kind: seed, target: i, from: now.Add(-p.Span), to: now}
}

func forwardTask(i int, p Policy, c archive.Coverage, now time.Time) task {
	from := c.Newest.Add(-p.Overlap)
	// A series that fell past the horizon — the collector was off for longer
	// than the provider keeps this interval — has a gap nothing can fill.
	// Resume from the edge rather than ask for a window that will be refused.
	if e := edge(p, now); !e.IsZero() && from.Before(e) {
		from = e
	}
	to := now
	if from.Add(p.Span).Before(to) {
		to = from.Add(p.Span)
	}
	return task{kind: forward, target: i, from: from, to: to}
}

func backwardTask(i int, p Policy, c archive.Coverage, now time.Time) task {
	from := c.Oldest.Add(-p.Span)
	if e := edge(p, now); !e.IsZero() && from.Before(e) {
		from = e
	}
	return task{kind: backward, target: i, from: from, to: c.Oldest}
}

// edgeSlack is how close to the horizon a backfill has to reach to count as
// complete. The edge moves every second, so "reached it exactly" never holds.
const edgeSlack = day

// advance is the coverage a successful fetch leaves behind.
func advance(t target, tk task, got quotes.CandleSeries, now time.Time) archive.Coverage {
	c := t.coverage
	c.Interval = t.policy.Interval
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
		case t.policy.Horizon > 0 && !tk.from.After(now.Add(-t.policy.Horizon+edgeSlack)):
			// Reached the provider's edge.
			c.Complete = true
		case firstTrade.IsZero() && tk.kind == backward && len(got.Candles) == 0:
			// No first trade date to go by, and a window with nothing in it:
			// the beginning is somewhere behind us.
			c.Complete = true
		}
	}
	c.Failures, c.NextAttempt, c.LastError = 0, time.Time{}, ""
	return c
}

// maxBackoff caps how long a failing series waits between attempts. A week:
// a symbol Yahoo has never heard of costs one request a week, not one a cycle,
// and one that was only briefly broken is back within days.
const maxBackoff = 7 * day

// fail is the coverage a failed fetch leaves behind: an hour's wait, doubling.
func fail(t target, err error, now time.Time) archive.Coverage {
	c := t.coverage
	c.Interval = t.policy.Interval
	c.Failures++
	wait := time.Hour
	for i := 1; i < c.Failures && wait < maxBackoff; i++ {
		wait *= 2
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	c.NextAttempt = now.Add(wait)
	msg := err.Error()
	if len(msg) > 240 {
		msg = msg[:240] + "…"
	}
	c.LastError = msg
	return c
}

// settled drops the bars that hadn't finished when they were fetched.
//
// An intraday bar still in progress is keyed by Yahoo's latest trade rather
// than the bar's start, so it would land as an extra row between two real
// ones. The next forward fetch overlaps it and reads it finished. A daily bar
// is keyed by its date instead, so the in-progress one simply gets
// overwritten, and is kept for its freshness.
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
