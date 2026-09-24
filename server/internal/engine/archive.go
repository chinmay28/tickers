package engine

import (
	"context"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/expr"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// Symbols is every symbol the app itself uses: each watchlist row, enabled or
// not, the legs of every composite, and every saved portfolio's holdings and
// benchmark. It is what the archive collects first, so that every chart and
// backtest the app can draw is backed by local history before the rest of
// the market is.
func (e *Engine) Symbols() ([]string, error) {
	tickers, err := e.store.Tickers()
	if err != nil {
		return nil, err
	}
	portfolios, err := e.store.Portfolios()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" {
			seen[s] = true
		}
	}
	for _, t := range tickers {
		switch {
		case t.IsPortfolio():
			// Its holdings are covered by the portfolio loop below.
		case t.IsComposite():
			if parsed, err := expr.Parse(t.Expression); err == nil {
				for _, s := range parsed.Symbols() {
					add(s)
				}
			}
		default:
			add(t.Symbol)
		}
	}
	for _, p := range portfolios {
		for _, h := range p.Holdings {
			add(h.Symbol)
		}
		add(p.Benchmark)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// ArchiveReader is what the engine needs from the market-data archive: a way
// to read it while it is open. archiver.Manager is one; a nil one means there
// is no archive, and every feature reads from the provider as it always has.
type ArchiveReader interface {
	Read(fn func(a *archive.Archive) error) error
}

// UseArchive makes the performance sheet, backtests and sparklines read
// local history wherever the archive holds it. Call it before Start.
func (e *Engine) UseArchive(r ArchiveReader) {
	e.mu.Lock()
	e.archive = r
	e.mu.Unlock()
}

func (e *Engine) archiveReader() ArchiveReader {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.archive
}

// archiveEpoch is where an archive read of "everything" starts. Not the unix
// epoch — daily history for the oldest listings starts in the 1960s.
var archiveEpoch = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// dailyStale is how old the archive's newest daily bar may be for its series
// to be trusted: a long weekend, and a day for the collector to get round to
// it. Past that the provider is asked instead — a chart that silently stops
// last week is worse than a request.
const dailyStale = 5 * 24 * time.Hour

// archivedDaily reads a symbol's daily bars and dividends from the archive,
// reporting false unless some source has walked its daily series all the way
// back and kept it current. A half-backfilled series would make "all time"
// mean "as far as the collector has got", which is exactly the silent
// truncation the performance sheet refuses to do.
func (e *Engine) archivedDaily(symbol string) ([]quotes.Candle, []quotes.Dividend, bool) {
	r := e.archiveReader()
	if r == nil {
		return nil, nil, false
	}
	var candles []quotes.Candle
	var dividends []quotes.Dividend
	ok := false
	err := r.Read(func(a *archive.Archive) error {
		s, err := a.Lookup(symbol)
		if err != nil {
			return err
		}
		cursors, err := a.SymbolCursors(s.ID)
		if err != nil {
			return err
		}
		for _, c := range cursors {
			if c.Interval == quotes.Daily && c.Complete && time.Since(c.Newest) < dailyStale {
				ok = true
			}
		}
		if !ok {
			return nil
		}
		far := time.Now().Add(48 * time.Hour)
		if candles, err = a.Candles(symbol, quotes.Daily, archiveEpoch, far); err != nil {
			return err
		}
		dividends, err = a.Dividends(symbol, archiveEpoch, far)
		return err
	})
	return candles, dividends, err == nil && ok && len(candles) > 0
}

// archiveHistory is a symbol's daily series from the archive, topped up with
// the last few sessions from the provider so today's move is on it — the
// archive fetches forward once a day, and the sheet's shortest row is today.
// One small request instead of the whole history.
func (e *Engine) archiveHistory(ctx context.Context, h quotes.Historian, symbol string) ([]quotes.Bar, bool) {
	candles, dividends, ok := e.archivedDaily(symbol)
	if !ok {
		return nil, false
	}
	raw := make(map[string]float64, len(candles)+8)
	for _, c := range candles {
		raw[c.Time.Format(time.DateOnly)] = c.Close
	}
	last := candles[len(candles)-1].Time
	if tail, err := h.History(ctx, symbol, last.AddDate(0, 0, -7)); err == nil {
		for _, b := range tail {
			if b.Raw > 0 {
				raw[b.Date] = b.Raw
			} else {
				raw[b.Date] = b.Close
			}
		}
	} else {
		e.log.Debug("could not top up archived history; serving it as held", "symbol", symbol, "error", err)
	}
	return adjustedBars(raw, dividends), true
}

// adjustedBars turns raw closes into Bars whose Close is adjusted for
// dividends — the series a return is measured on — the way Yahoo's adjusted
// close is: each payout scales every earlier close by one minus the payout
// over the close before its ex-date. Splits need no work here; the archive's
// prices are already on the latest basis.
func adjustedBars(raw map[string]float64, dividends []quotes.Dividend) []quotes.Bar {
	bars := make([]quotes.Bar, 0, len(raw))
	for date, close := range raw {
		bars = append(bars, quotes.Bar{Date: date, Close: close, Raw: close})
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Date < bars[j].Date })
	divs := append([]quotes.Dividend(nil), dividends...)
	sort.Slice(divs, func(i, j int) bool { return divs[i].Time.After(divs[j].Time) })

	factor, d := 1.0, 0
	for i := len(bars) - 1; i >= 0; i-- {
		for d < len(divs) && bars[i].Date < divs[d].Time.Format(time.DateOnly) {
			if p := bars[i].Raw; p > 0 && divs[d].Amount < p {
				factor *= 1 - divs[d].Amount/p
			}
			d++
		}
		bars[i].Close = bars[i].Raw * factor
	}
	return bars
}

// archivedDividends is a symbol's payouts from the archive, when its daily
// series is trusted.
func (e *Engine) archivedDividends(symbol string) ([]quotes.Distribution, bool) {
	_, dividends, ok := e.archivedDaily(symbol)
	if !ok {
		return nil, false
	}
	out := make([]quotes.Distribution, len(dividends))
	for i, d := range dividends {
		out[i] = quotes.Distribution{Date: d.Time.Format(time.DateOnly), Amount: d.Amount}
	}
	return out, true
}

// Sparkline is the recent price series a watchlist row draws: the archive's
// intraday bars where it has them, and the refresh loop's own readings for
// anything newer. The archive gives a sparkline real bars — a close every five
// minutes through the session, whatever the refresh interval — and the
// readings keep its last stretch live between collector passes.
func (e *Engine) Sparkline(symbol string, limit int) ([]store.HistoryPoint, error) {
	if limit <= 0 {
		limit = 120
	}
	readings, err := e.store.History(symbol, limit)
	if err != nil {
		return nil, err
	}
	r := e.archiveReader()
	if r == nil {
		return readings, nil
	}
	cfg, err := e.store.Config()
	if err != nil {
		return readings, nil
	}
	now := time.Now()
	var bars []quotes.Candle
	r.Read(func(a *archive.Archive) error {
		bars, err = a.Bars(symbol, quotes.FiveMinute, now.Add(-cfg.HistoryRetention()), now)
		return err
	})
	if len(bars) == 0 {
		return readings, nil
	}
	sym := store.NormalizeSymbol(symbol)
	out := make([]store.HistoryPoint, 0, len(bars)+len(readings))
	for _, b := range bars {
		out = append(out, store.HistoryPoint{Symbol: sym, Price: b.Close, At: b.Time.Add(quotes.FiveMinute.Step())})
	}
	newest := out[len(out)-1].At
	for _, p := range readings {
		if p.At.After(newest) {
			out = append(out, p)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}
