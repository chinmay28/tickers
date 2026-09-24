package collector

import (
	"fmt"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Policy is how one interval is collected: how far back the provider keeps it,
// how much one request may ask for, and how often to go forward.
type Policy struct {
	Interval quotes.Interval
	// Horizon is how far back the provider serves this interval at all. Zero
	// means forever. Past it, a backfill is complete — not because the symbol
	// has no older history, but because nobody will hand it over.
	Horizon time.Duration
	// Span is the widest window one request asks for. For an interval with a
	// horizon it is the horizon, so a first fetch takes everything there is
	// in one request.
	Span time.Duration
	// Every is how stale a series may get before it is fetched forward again.
	Every time.Duration
	// Overlap is how far behind the newest covered point a forward fetch
	// starts, so the last bars are re-read — a provider revises a bar for a
	// while after it prints, and the in-progress one is not final at all.
	Overlap time.Duration
}

const day = 24 * time.Hour

// Yahoo's limits. They are not documented anywhere and have moved before; these
// are what the chart endpoint enforces as of writing, a day inside the edge so
// a request never lands on it. Past the horizon Yahoo answers with an error,
// not an empty series, so a window straddling it would fail every time.
var yahooPolicies = map[quotes.Interval]Policy{
	// Daily history goes back to the listing, so the backfill walks there a
	// decade per request: everything gets a decade before anything gets two.
	quotes.Daily: {Interval: quotes.Daily, Span: 3652 * day, Every: 20 * time.Hour, Overlap: 5 * day},
	// Hourly bars are kept for 730 days. The one request that takes all of
	// them is the whole backfill; after that a weekly top-up is plenty, and
	// leaves the budget to the finer series.
	quotes.Hourly: {Interval: quotes.Hourly, Horizon: 729 * day, Span: 729 * day, Every: 7 * day, Overlap: day},
	// Five-minute bars are kept for 60 days. Every day a series isn't fetched,
	// a day falls off the far end for good — which is why the forward pass is
	// daily, and why the scheduler puts a series near that edge first.
	quotes.FiveMinute: {Interval: quotes.FiveMinute, Horizon: 59 * day, Span: 59 * day, Every: 20 * time.Hour, Overlap: day},
}

// Policies returns the policy for each interval, in the order given.
func Policies(intervals []quotes.Interval) ([]Policy, error) {
	if len(intervals) == 0 {
		return nil, fmt.Errorf("no intervals to collect")
	}
	seen := map[quotes.Interval]bool{}
	out := make([]Policy, 0, len(intervals))
	for _, i := range intervals {
		p, ok := yahooPolicies[i]
		if !ok {
			return nil, fmt.Errorf("no collection policy for interval %q", i)
		}
		if seen[i] {
			continue
		}
		seen[i] = true
		out = append(out, p)
	}
	return out, nil
}
