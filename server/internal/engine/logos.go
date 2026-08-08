package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// maxLogosPerCycle bounds how many logos one refresh will go and get.
//
// A first cycle on a twenty-symbol watchlist would otherwise fire forty
// requests (a lookup and an image each) at a host that owes us nothing, all
// while the cycle that actually prices the watchlist waits. Spread over a few
// cycles the pictures arrive within a couple of refreshes and nothing else is
// held up; once every symbol has an answer the work stops entirely.
const maxLogosPerCycle = 6

// logoTTL is how long a fetched answer stands before it is asked again.
//
// A logo changes when a company rebrands, so anything shorter is wasted
// requests — but "never" was worse than it looked: it made a wrong URL, an
// expired key and a source that was down for an hour into permanent answers
// that only a manual cache clear could undo. A day is long enough to cost
// nothing and short enough that a mistake fixes itself overnight.
//
// It deliberately covers the noes as well. Most of the cache is symbols with
// no logo, and those are exactly the rows a corrected setting has to be able
// to overturn.
const logoTTL = 24 * time.Hour

// refreshLogos asks about symbols whose answer is missing or a day old.
//
// Every outcome is recorded except a real failure: an image is stored, "this
// symbol hasn't got one" is stored as a tombstone so it is not asked again
// today, and a timeout or a 502 is left unrecorded on purpose so the next cycle
// retries it. Getting that distinction backwards would either re-ask about
// every ETF every cycle or give up on a symbol because the network blinked
// once.
//
// A re-check is conditional wherever it can be, and a re-check that finds
// nothing new only moves the row's age. The daily pass is therefore nearly
// free: on a source with validators it transfers no bytes at all, and on one
// without it at least never rewrites an image that hasn't moved.
func (e *Engine) refreshLogos(ctx context.Context, symbols []string) {
	source, ok := e.provider.(quotes.Iconographer)
	if !ok {
		return
	}
	// Uploaded logos are settled whatever their age: they are the operator's
	// own files, and a refresh cycle overwriting one with whatever a third
	// party happens to serve would be the feature undoing somebody's work.
	settled, err := e.store.SettledLogos(time.Now().Add(-logoTTL))
	if err != nil {
		e.log.Warn("logo cache unreadable", "error", err)
		return
	}
	checks, err := e.store.LogoChecks()
	if err != nil {
		e.log.Warn("logo cache unreadable", "error", err)
		return
	}

	fetched := 0
	for _, symbol := range symbols {
		if fetched >= maxLogosPerCycle {
			return
		}
		if settled[symbol] {
			continue
		}

		known := checks[symbol]
		logo, err := source.Logo(ctx, symbol, quotes.LogoValidators{
			ETag:         known.ETag,
			LastModified: known.LastModified,
		})
		fetched++
		switch {
		case errors.Is(err, quotes.ErrLogoUnchanged):
			// The source answered the question without sending anything. Only
			// the row's age moves, which is what stops it being asked again
			// tomorrow.
			if err := e.store.TouchLogo(symbol, time.Now().UTC()); err != nil {
				e.log.Warn("logo timestamp not updated", "symbol", symbol, "error", err)
			}
		case errors.Is(err, quotes.ErrNoLogo):
			// The reason travels with the tombstone. "No logo" is the same row
			// whether this fund simply hasn't got one or the configured URL
			// answers 404 for everything, and the Settings page can only tell
			// the operator which if the difference was written down.
			if err := e.store.SaveLogo(store.Logo{
				Symbol: symbol, Status: store.LogoNone, Reason: reasonFor(err),
			}); err != nil {
				e.log.Warn("logo tombstone not saved", "symbol", symbol, "error", err)
			}
		case err != nil:
			// Left unrecorded, so the next cycle asks again.
			e.log.Warn("logo fetch failed", "symbol", symbol, "error", err)
		case bytes.Equal(logo.Bytes, known.Bytes) && len(known.Bytes) > 0:
			// The same picture arrived again, from a source with no validators
			// to save the round trip. Rewriting the row would be an identical
			// image and a new version number, and the version is what the
			// client puts in the URL — so every browser would re-download an
			// image it already had, once a day, forever.
			if err := e.store.TouchLogo(symbol, time.Now().UTC()); err != nil {
				e.log.Warn("logo timestamp not updated", "symbol", symbol, "error", err)
			}
		default:
			if err := e.store.SaveLogo(store.Logo{
				Symbol:       symbol,
				Status:       store.LogoOK,
				ContentType:  logo.ContentType,
				Bytes:        logo.Bytes,
				Source:       logo.Source,
				ETag:         logo.Validators.ETag,
				LastModified: logo.Validators.LastModified,
			}); err != nil {
				e.log.Warn("logo not saved", "symbol", symbol, "error", err)
			}
		}
	}
}

// reasonFor is the provider's explanation with the sentinel's own text taken
// off the front — "that symbol has no logo: nothing at https://…" says the
// obvious part twice, and it is the second half that is worth reporting.
func reasonFor(err error) string {
	reason := strings.TrimPrefix(err.Error(), quotes.ErrNoLogo.Error())
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(reason), ":"))
}
