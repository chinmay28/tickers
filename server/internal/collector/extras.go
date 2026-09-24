package collector

import (
	"context"
	"errors"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// candles asks a source for a window, including the extended session when the
// plan says so and the source can give it. A daily bar has no extended
// session, so daily windows always take the plain path.
func (c *Collector) candles(ctx context.Context, src Source, symbol string, interval quotes.Interval, from, to time.Time) (quotes.CandleSeries, error) {
	if c.extended && interval.Intraday() {
		if ext, ok := src.Provider.(quotes.ExtendedArchivist); ok {
			return ext.ExtendedCandles(ctx, symbol, interval, from, to)
		}
	}
	return src.Provider.Candles(ctx, symbol, interval, from, to)
}

// maxRenameQueue bounds the rename checks waiting. A day's new listings are a
// handful; a queue this long means the lists changed wholesale, and checking
// thousands of symbols one request at a time would crowd out collecting.
const maxRenameQueue = 500

func (c *Collector) queueRenameCheck(symbol string) {
	if len(c.renames) < maxRenameQueue {
		c.renames = append(c.renames, symbol)
	}
}

// renameCheck is the next queued rename check and the source to ask, when a
// source that knows renames is configured and ready.
func (c *Collector) renameCheck(ready func(int) bool) (int, string, bool) {
	if len(c.renames) == 0 {
		return 0, "", false
	}
	for i, s := range c.sources {
		if _, ok := s.Provider.(quotes.Renamer); ok {
			if ready(i) {
				return i, c.renames[0], true
			}
			return 0, "", false
		}
	}
	// No source can answer; the checks would wait forever.
	c.renames = nil
	return 0, "", false
}

// checkRename asks whether a newly listed symbol used to trade under another
// name, and if so records the aliases that make its history one series.
func (c *Collector) checkRename(ctx context.Context, si int, symbol string) error {
	src := c.sources[si]
	c.setCurrent(symbol + " former names from " + src.Name)
	history, err := src.Provider.(quotes.Renamer).TickerHistory(ctx, symbol)
	now := c.now()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.pacerFor(src).observe(err, now)
	if errors.Is(err, quotes.ErrRateLimited) {
		return nil // stays queued
	}
	c.renames = c.renames[1:]
	if err != nil {
		c.log.Debug("rename check failed", "symbol", symbol, "error", err)
		return nil
	}
	aliases := formerNames(symbol, history)
	if len(aliases) == 0 {
		return nil
	}
	sym, err := c.archive.Lookup(symbol)
	if err != nil {
		return nil
	}
	c.log.Info("renamed symbol found; its former history is read as its own", "symbol", symbol, "formerly", aliases)
	return c.archive.SetAliases(sym.ID, aliases)
}

// formerNames turns a ticker history into aliases: each earlier symbol, valid
// until the next period began.
func formerNames(symbol string, history []quotes.TickerPeriod) []archive.Alias {
	var out []archive.Alias
	for i := 0; i+1 < len(history); i++ {
		if history[i].Symbol == "" || history[i].Symbol == symbol || history[i+1].From.IsZero() {
			continue
		}
		out = append(out, archive.Alias{Former: history[i].Symbol, Until: history[i+1].From})
	}
	return out
}
