package collector

import (
	"errors"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// pacer decides when the next request may go out. It holds no clock of its
// own — every method takes now — so its behaviour is a function of what it
// has been told, and a test can drive a week of it in a microsecond.
//
// Three rules, in order of how often they matter:
//
//   - Requests are spaced evenly, never bursted. Yahoo has no published limit;
//     a steady trickle is what stays under the unpublished one.
//   - A 429 pauses everything, starting at a minute and doubling to an hour,
//     and a success clears it. Rate limiting is about this host, not about the
//     symbol that happened to be asked, so no symbol is charged for it.
//   - A run of ordinary failures pauses everything for a while too. Ten
//     different symbols failing in a row is the network, not ten symbols.
type pacer struct {
	spacing    time.Duration
	last       time.Time
	pauseUntil time.Time
	penalty    time.Duration
	streak     int
}

const (
	minPenalty  = time.Minute
	maxPenalty  = time.Hour
	streakLimit = 10
	streakPause = 5 * time.Minute
)

// delay is how long to wait before the next request.
func (p *pacer) delay(now time.Time) time.Duration {
	at := p.last.Add(p.spacing)
	if p.pauseUntil.After(at) {
		at = p.pauseUntil
	}
	if d := at.Sub(now); d > 0 {
		return d
	}
	return 0
}

// observe records a request's outcome.
func (p *pacer) observe(err error, now time.Time) {
	p.last = now
	switch {
	case err == nil:
		p.penalty, p.streak = 0, 0
	case errors.Is(err, quotes.ErrRateLimited):
		p.penalty *= 2
		if p.penalty < minPenalty {
			p.penalty = minPenalty
		}
		if p.penalty > maxPenalty {
			p.penalty = maxPenalty
		}
		p.pauseUntil = now.Add(p.penalty)
	case errors.Is(err, quotes.ErrNotFound):
		// An answer about one symbol, not about the connection.
	default:
		p.streak++
		if p.streak >= streakLimit {
			p.streak = 0
			p.pauseUntil = now.Add(streakPause)
		}
	}
}

// paused reports whether the pacer is holding back for a reason other than
// ordinary spacing — what the log says when it goes quiet.
func (p *pacer) paused(now time.Time) bool { return p.pauseUntil.After(now) }
