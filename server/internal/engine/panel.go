package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/xsection"
)

// Universe says which symbols a cross-sectional run ranks.
type Universe struct {
	// Symbols, when set, is the universe.
	Symbols []string
	// Otherwise it is the Size most-traded symbols of Kind ("stock", "etf"
	// or any) over the window.
	Kind string
	Size int
	// Also are loaded whatever the universe is: a benchmark to compare
	// with, which the ranking is told to leave out.
	Also []string
}

// Bounds on a universe. A panel is every symbol's every day in memory; a
// thousand symbols over ten years is about 100 MB, which a Pi can spare and
// is past where more symbols change a ranking's answer.
const (
	DefaultUniverse = 200
	MaxUniverse     = 1000
)

// Panel loads a universe's daily bars over [from, to), reaching back warm
// trading days further so every factor is settled on the first day ranked.
//
// A universe by size is chosen by dollar volume over the whole window. That
// knows a little of the future — a stock that became liquid late is in from
// the start — so it is said in a warning; day-by-day filters, which don't,
// are the ranking's to apply. Delisted symbols are included wherever the
// archive holds their bars.
func (e *Engine) Panel(ctx context.Context, u Universe, from, to time.Time, warm int, dividends bool) (*xsection.Panel, PanelInfo, error) {
	var info PanelInfo
	r := e.archiveReader()
	if r == nil {
		return nil, info, ErrNoArchive
	}
	lead := from.Add(-lookback(quotes.Daily, warm, false))
	var warnings []string
	series := map[string][]quotes.Candle{}
	err := r.Read(func(a *archive.Archive) error {
		symbols := u.Symbols
		if len(symbols) == 0 {
			size := u.Size
			if size <= 0 {
				size = DefaultUniverse
			}
			traded, err := a.MostTraded(ctx, from, to, u.Kind, min(size, MaxUniverse))
			if err != nil {
				return err
			}
			for _, t := range traded {
				symbols = append(symbols, t.Symbol.Symbol)
			}
			warnings = append(warnings, fmt.Sprintf("the universe is the %d most-traded symbols over the whole window, which knows a little of the future; set minDollarVolume to judge liquidity day by day", len(symbols)))
		} else if len(symbols) > MaxUniverse {
			return fmt.Errorf("%w: a universe can have at most %d symbols", ErrBadUniverse, MaxUniverse)
		}
		for _, s := range u.Also {
			if s = store.NormalizeSymbol(s); !slices.ContainsFunc(symbols, func(x string) bool { return store.NormalizeSymbol(x) == s }) {
				info.Extra = append(info.Extra, s)
			}
		}
		symbols = append(slices.Clone(symbols), info.Extra...)
		var missing []string
		for _, sym := range symbols {
			if err := ctx.Err(); err != nil {
				return err
			}
			sym = store.NormalizeSymbol(sym)
			if _, done := series[sym]; done {
				continue
			}
			bars, err := a.Best(archive.Query{Symbol: sym, Interval: quotes.Daily, From: lead, To: to})
			if err != nil {
				return err
			}
			if len(bars) == 0 {
				missing = append(missing, sym)
				continue
			}
			if dividends {
				divs, err := a.Dividends(sym, lead, to)
				if err != nil {
					return err
				}
				if len(divs) > 0 {
					bars = adjustCandles(bars, divs)
				}
			}
			series[sym] = bars
		}
		if len(missing) > 0 {
			warnings = append(warnings, fmt.Sprintf("no daily bars in the window for %d symbol(s): %s", len(missing), abbreviate(missing, 10)))
		}
		return nil
	})
	info.Warnings = warnings
	if err != nil {
		return nil, info, err
	}
	if len(series) == 0 {
		return nil, info, fmt.Errorf("%w: the archive holds no daily bars for that universe between %s and %s", ErrNoBars,
			from.Format(time.DateOnly), to.AddDate(0, 0, -1).Format(time.DateOnly))
	}
	return xsection.NewPanel(series), info, nil
}

// PanelInfo is what loading a panel learned on the way.
type PanelInfo struct {
	Warnings []string
	// Extra are the symbols of Universe.Also that aren't in the universe
	// proper, and so shouldn't be ranked.
	Extra []string
}

// ErrBadUniverse is a universe that can't be loaded as asked.
var ErrBadUniverse = errors.New("bad universe")

func abbreviate(xs []string, n int) string {
	if len(xs) <= n {
		return fmt.Sprint(xs)
	}
	return fmt.Sprintf("%v and %d more", xs[:n], len(xs)-n)
}
