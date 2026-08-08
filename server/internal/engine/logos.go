package engine

import (
	"context"
	"errors"

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

// refreshLogos asks about symbols the cache has no answer for yet.
//
// Every outcome is recorded except a real failure: an image is stored, "this
// symbol hasn't got one" is stored as a tombstone so it is never asked again,
// and a timeout or a 502 is left unrecorded on purpose so the next cycle
// retries it. Getting that distinction backwards would either re-ask about
// every ETF forever or give up on a symbol because the network blinked once.
func (e *Engine) refreshLogos(ctx context.Context, symbols []string) {
	source, ok := e.provider.(quotes.Iconographer)
	if !ok {
		return
	}
	asked, err := e.store.AskedAboutLogos()
	if err != nil {
		e.log.Warn("logo cache unreadable", "error", err)
		return
	}

	fetched := 0
	for _, symbol := range symbols {
		if fetched >= maxLogosPerCycle {
			return
		}
		if asked[symbol] {
			continue
		}

		logo, err := source.Logo(ctx, symbol)
		fetched++
		switch {
		case errors.Is(err, quotes.ErrNoLogo):
			if err := e.store.SaveLogo(store.Logo{Symbol: symbol, Status: store.LogoNone}); err != nil {
				e.log.Warn("logo tombstone not saved", "symbol", symbol, "error", err)
			}
		case err != nil:
			// Left unrecorded, so the next cycle asks again.
			e.log.Warn("logo fetch failed", "symbol", symbol, "error", err)
		default:
			if err := e.store.SaveLogo(store.Logo{
				Symbol:      symbol,
				Status:      store.LogoOK,
				ContentType: logo.ContentType,
				Bytes:       logo.Bytes,
				Source:      logo.Source,
			}); err != nil {
				e.log.Warn("logo not saved", "symbol", symbol, "error", err)
			}
		}
	}
}
