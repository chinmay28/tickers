package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// ErrNoBars means the archive holds nothing for a strategy's symbol in its
// window. It is the caller's to say so: a backtest over no data is not a
// backtest with no trades.
var ErrNoBars = errors.New("no bars")

// RunStrategy backtests a strategy over the archive's bars.
//
// Only the archive: a rule-based strategy needs opens, highs and lows — to
// fill at the next open, to trip a stop inside a bar — which the provider's
// history, closes alone, doesn't have. A symbol the archive has never heard of
// is added to it, first in line, so asking is also how it gets collected.
func (e *Engine) RunStrategy(def strategy.Definition) (strategy.Result, error) {
	plan, err := strategy.Compile(def, time.Now())
	if err != nil {
		return strategy.Result{}, err
	}
	r := e.archiveReader()
	if r == nil {
		return strategy.Result{}, ErrNoArchive
	}
	warm := 1 // a cross looks one bar back
	for _, s := range plan.Specs {
		warm = max(warm, s.Warmup()+1)
	}
	q := archive.Query{
		Symbol: plan.Def.Symbol, Interval: plan.Interval,
		From: plan.From.Add(-lookback(plan.Interval, warm, false)), To: plan.To,
	}

	var bars []quotes.Candle
	var dividends []quotes.Dividend
	var warnings []string
	added := false
	err = r.Read(func(a *archive.Archive) error {
		sym, err := a.Lookup(plan.Def.Symbol)
		unknown := errors.Is(err, archive.ErrUnknownSymbol)
		if err != nil && !unknown {
			return err
		}
		// Known is not the same as collected: with the exchange lists off,
		// the catalog still names every listed symbol. One nobody excluded
		// is put on the user's list either way.
		if unknown || (!sym.Active && !sym.Excluded) {
			if err := a.Add(archive.User, archive.Entry{Symbol: plan.Def.Symbol}, time.Now()); err != nil {
				return err
			}
			added = true
		}
		if unknown {
			return fmt.Errorf("%w: %s wasn't in the archive; it has been added and goes first in line — try again once it has been collected", ErrNoBars, plan.Def.Symbol)
		}
		if bars, err = a.Best(q); err != nil {
			return err
		}
		if plan.Interval == quotes.Daily && plan.Def.Dividends {
			if dividends, err = a.Dividends(plan.Def.Symbol, q.From, q.To); err != nil {
				return err
			}
		}
		cursors, err := a.SymbolCursors(sym.ID)
		if err != nil {
			return err
		}
		complete := false
		for _, c := range cursors {
			if c.Interval == plan.Interval && c.Complete {
				complete = true
			}
		}
		if !complete {
			warnings = append(warnings, fmt.Sprintf("%s's %s history is still being collected; the test covers what the archive holds so far", plan.Def.Symbol, plan.Interval))
		}
		return nil
	})
	if err != nil {
		return strategy.Result{}, err
	}

	start := sort.Search(len(bars), func(i int) bool { return !bars[i].Time.Before(plan.From) })
	if start >= len(bars) && added {
		return strategy.Result{}, fmt.Errorf("%w: %s wasn't being collected; it has been added and goes first in line — try again once it has been collected", ErrNoBars, plan.Def.Symbol)
	}
	if start >= len(bars) {
		return strategy.Result{}, fmt.Errorf("%w: the archive holds no %s bars for %s between %s and %s", ErrNoBars,
			plan.Interval, plan.Def.Symbol, plan.From.Format(time.DateOnly), plan.To.AddDate(0, 0, -1).Format(time.DateOnly))
	}
	if len(dividends) > 0 {
		bars = adjustCandles(bars, dividends)
	}
	if first := bars[start].Time; first.Sub(plan.From) > 7*24*time.Hour {
		warnings = append(warnings, fmt.Sprintf("the archive's bars start on %s, after the start asked for", first.Format(time.DateOnly)))
	}
	if start < warm-1 {
		warnings = append(warnings, "the archive has too little history before the start for every indicator to be settled on the first bars")
	}
	res := strategy.Simulate(plan, bars, start)
	res.Warnings = append(warnings, res.Warnings...)
	return res, nil
}

// adjustCandles scales every bar before each ex-date the way adjustedBars
// scales closes, so a strategy holding through a payout is credited it as a
// holder would be, and a price level a rule compares against isn't broken by
// the drop on the ex-date.
func adjustCandles(bars []quotes.Candle, dividends []quotes.Dividend) []quotes.Candle {
	raw := make(map[string]float64, len(bars))
	for _, b := range bars {
		raw[b.Time.Format(time.DateOnly)] = b.Close
	}
	factor := map[string]float64{}
	for _, adj := range adjustedBars(raw, dividends) {
		if adj.Raw > 0 {
			factor[adj.Date] = adj.Close / adj.Raw
		}
	}
	out := make([]quotes.Candle, len(bars))
	for i, b := range bars {
		f := factor[b.Time.Format(time.DateOnly)]
		if f == 0 {
			f = 1
		}
		b.Open, b.High, b.Low, b.Close = b.Open*f, b.High*f, b.Low*f, b.Close*f
		b.VWAP *= f
		out[i] = b
	}
	return out
}

// SaveStrategy validates a strategy's rules and saves them, creating a new
// one when id is empty. Rules that can't compile are refused here rather than
// on the first run — a saved strategy that fails to open is a trap.
func (e *Engine) SaveStrategy(id, name string, raw json.RawMessage) (store.Strategy, error) {
	var def strategy.Definition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return store.Strategy{}, &strategy.InvalidError{}
	}
	if _, err := strategy.Compile(def, time.Now()); err != nil {
		return store.Strategy{}, err
	}
	if id == "" {
		return e.store.CreateStrategy(name, raw)
	}
	return e.store.UpdateStrategy(id, name, raw)
}
