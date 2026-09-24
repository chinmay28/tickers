package collector

import (
	"fmt"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Source is one provider the collector fetches from, and what it can give.
//
// There are two roles. The *live* source keeps every series current — it
// fetches forward — and walks back as far as it reaches. Every other source
// only fills gaps: it walks each series backward, skips the days the archive
// already holds, and asks only for the rest. That is how a paid feed added a
// year from now backfills the years Yahoo never kept, without re-downloading
// the months Yahoo already did.
type Source struct {
	// Name is what the archive records as each bar's provenance. Renaming a
	// source orphans its cursors, so it is an identifier, not a label.
	Name     string
	Provider quotes.Archivist
	// Reach says, per interval, how far back this source goes and how much
	// one request may ask for. An interval missing from it is one this
	// source doesn't serve.
	Reach map[quotes.Interval]Reach
	// Spacing is the gap between two of this source's requests. Each source
	// is paced on its own: a paid feed's limit is not Yahoo's.
	Spacing time.Duration
	// Live marks the source that fetches forward. At most one is.
	Live bool
}

// Reach is how one source serves one interval.
type Reach struct {
	// Horizon is how far back the source serves this interval at all. Zero
	// means forever. Past it a walk is complete — not because the symbol has
	// no older history, but because this source won't hand it over.
	Horizon time.Duration
	// Span is the widest window one request asks for.
	Span time.Duration
	// Every is how stale the live source lets a series get before fetching
	// it forward. Zero means never forward — a one-shot backfill.
	Every time.Duration
	// Overlap is how far behind the newest covered point a forward fetch
	// starts, so the last bars are re-read: a provider revises a bar for a
	// while after printing it, and the in-progress one is not final at all.
	Overlap time.Duration
}

const day = 24 * time.Hour

// YahooName is the provenance Yahoo's bars carry.
const YahooName = "yahoo"

// YahooReach is what Yahoo's chart endpoint serves. None of it is documented
// and all of it has moved before; each horizon is set a day inside the edge,
// because a window straddling it is refused outright rather than truncated.
//
// One-minute bars are the finest Yahoo serves and the series kept current;
// every coarser intraday interval can be built from them, so five-minute and
// hourly bars are one-shot backfills of the stretch minute bars can't reach —
// days 30 to 60, and the two years before that. The collector turns their
// forward pass back on if minute bars are switched off.
var YahooReach = map[quotes.Interval]Reach{
	// Daily history goes back to the listing, a decade per request: every
	// symbol gets a decade before any symbol gets two.
	quotes.Daily: {Span: 3652 * day, Every: 20 * time.Hour, Overlap: 5 * day},
	// Hourly bars are kept for 730 days, and one request takes them all.
	quotes.Hourly: {Horizon: 729 * day, Span: 729 * day, Every: 7 * day, Overlap: day},
	// Five-minute bars are kept for 60 days.
	quotes.FiveMinute: {Horizon: 59 * day, Span: 59 * day, Every: 20 * time.Hour, Overlap: day},
	// Minute bars are kept for 30 days, at most 7 per request. Every day a
	// series isn't fetched, a day falls off the far end for good — which is
	// why the forward pass is daily and a series near the edge goes first.
	quotes.OneMinute: {Horizon: 29 * day, Span: 7 * day, Every: 20 * time.Hour, Overlap: day},
}

// Yahoo builds the Yahoo source.
func Yahoo(p quotes.Archivist, spacing time.Duration) Source {
	return Source{Name: YahooName, Provider: p, Reach: YahooReach, Spacing: spacing, Live: true}
}

// validSources checks a source list could be collected from.
func validSources(sources []Source) error {
	seen := map[string]bool{}
	live := 0
	for _, s := range sources {
		if s.Name == "" || s.Provider == nil {
			return fmt.Errorf("a source needs a name and a provider")
		}
		if seen[s.Name] {
			return fmt.Errorf("source %q is listed twice", s.Name)
		}
		seen[s.Name] = true
		if s.Spacing < MinSpacing {
			return fmt.Errorf("source %s: request spacing %s is below the %s floor", s.Name, s.Spacing, MinSpacing)
		}
		if s.Live {
			live++
		}
	}
	if live > 1 {
		return fmt.Errorf("%d sources are marked live; at most one fetches forward", live)
	}
	return nil
}

// reachFor is a source's reach for an interval with forward fetching turned
// off where a finer interval the same source keeps current makes it
// redundant, and turned off entirely for a source that isn't live.
func reachFor(s Source, i quotes.Interval, enabled []quotes.Interval) (Reach, bool) {
	r, ok := s.Reach[i]
	if !ok {
		return r, false
	}
	if !s.Live {
		r.Every = 0
		return r, true
	}
	if i.Intraday() {
		for _, finer := range enabled {
			if f, ok := s.Reach[finer]; ok && finer.Intraday() && finer.Step() < i.Step() && f.Every > 0 {
				r.Every = 0
				break
			}
		}
	}
	return r, true
}

// PolygonReach is what a Polygon plan serves, given how many years of history
// it includes. The spans keep one window inside a page of Polygon's 50,000
// results: a day of minute bars is up to 960 with the extended session, which
// the adapter drops but Polygon still counts.
//
// There are no hourly bars from Polygon. Its hours start on the clock, so
// every 9:00 bar is half pre-market; hourly bars for the years it reaches are
// built from its minute bars instead, anchored to the open.
func PolygonReach(years int) map[quotes.Interval]Reach {
	horizon := time.Duration(years) * 365 * day
	return map[quotes.Interval]Reach{
		quotes.Daily:      {Horizon: horizon, Span: 3652 * day},
		quotes.FiveMinute: {Horizon: horizon, Span: 120 * day},
		quotes.OneMinute:  {Horizon: horizon, Span: 30 * day},
	}
}

// Polygon builds a gap-filling Polygon source paced to a plan's per-minute
// request limit. Each window can cost a second request — the symbol's split
// history, asked once per symbol per half day — so the spacing leaves room
// for it.
func Polygon(p quotes.Archivist, years, perMinute int) Source {
	spacing := time.Minute / time.Duration(max(perMinute, 1))
	return Source{Name: quotes.PolygonName, Provider: p, Reach: PolygonReach(years), Spacing: max(spacing, MinSpacing)}
}
