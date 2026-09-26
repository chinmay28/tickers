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
	run := e.RunStrategies([]strategy.Definition{def})[0]
	return run.Result, run.Err
}

// StrategyRun is one backtest of a batch: its result, or why it has none.
type StrategyRun struct {
	Result strategy.Result
	Err    error
}

// RunStrategies backtests several strategies, reading the archive once per
// symbol, interval and adjustment they share rather than once each — a
// parameter sweep is a hundred backtests of one series. Each result is
// exactly what RunStrategy gives for its definition alone: every plan is cut
// its own window and warm-up from the shared read, and adjusted for only the
// dividends inside it.
func (e *Engine) RunStrategies(defs []strategy.Definition) []StrategyRun {
	out := make([]StrategyRun, len(defs))
	plans := make([]strategy.Plan, len(defs))
	var reads []*seriesRead
	byKey := map[seriesKey]*seriesRead{}
	now := time.Now()
	for i, def := range defs {
		p, err := strategy.Compile(def, now)
		if err != nil {
			out[i].Err = err
			continue
		}
		plans[i] = p
		key := seriesKey{p.Def.Symbol, p.Interval, p.Interval == quotes.Daily && p.Def.Dividends}
		rd := byKey[key]
		if rd == nil {
			rd = &seriesRead{key: key}
			byKey[key] = rd
			reads = append(reads, rd)
		}
		rd.want(p.From, p.To, p.Warmup())
		rd.plans = append(rd.plans, i)
	}
	if len(reads) == 0 {
		return out
	}
	r := e.archiveReader()
	for _, rd := range reads {
		s, err := e.readSeries(r, rd)
		for _, i := range rd.plans {
			if err != nil {
				out[i].Err = err
				continue
			}
			p := plans[i]
			bars, start, warnings, err := s.window(p.From, p.To, p.Warmup())
			if err != nil {
				out[i].Err = err
				continue
			}
			res := strategy.Simulate(p, bars, start)
			res.Warnings = append(warnings, res.Warnings...)
			out[i].Result = res
		}
	}
	return out
}

// RunStudy finds a pattern in the archive's bars and measures what followed
// it; see strategy.StudyPlan. The archive is read, and an unknown symbol
// added to it, exactly as for a backtest.
func (e *Engine) RunStudy(def strategy.StudyDefinition) (strategy.Study, error) {
	p, err := strategy.CompileStudy(def, time.Now())
	if err != nil {
		return strategy.Study{}, err
	}
	rd := &seriesRead{key: seriesKey{p.Def.Symbol, p.Interval, p.Interval == quotes.Daily && p.Def.Dividends}}
	rd.want(p.From, p.To, p.Warmup())
	s, err := e.readSeries(e.archiveReader(), rd)
	if err != nil {
		return strategy.Study{}, err
	}
	bars, start, warnings, err := s.window(p.From, p.To, p.Warmup())
	if err != nil {
		return strategy.Study{}, err
	}
	st := p.Run(bars, start)
	st.Warnings = append(warnings, st.Warnings...)
	return st, nil
}

// seriesKey is what makes two runs' reads the same read.
type seriesKey struct {
	symbol    string
	interval  quotes.Interval
	dividends bool
}

// seriesRead is one read of the archive on behalf of several runs: the
// union of their windows, reaching back as far as the most demanding
// warm-up.
type seriesRead struct {
	key      seriesKey
	from, to time.Time
	plans    []int
}

func (rd *seriesRead) want(from, to time.Time, warm int) {
	lead := from.Add(-lookback(rd.key.interval, warm, false))
	if rd.from.IsZero() || lead.Before(rd.from) {
		rd.from = lead
	}
	if to.After(rd.to) {
		rd.to = to
	}
}

// archived is what one read found: raw bars and dividends, and what the read
// learned about the symbol on the way.
type archived struct {
	key       seriesKey
	bars      []quotes.Candle
	dividends []quotes.Dividend
	// added says the symbol wasn't being collected until this read asked
	// for it.
	added    bool
	warnings []string
}

func (e *Engine) readSeries(r ArchiveReader, rd *seriesRead) (archived, error) {
	s := archived{key: rd.key}
	if r == nil {
		return s, ErrNoArchive
	}
	symbol := rd.key.symbol
	err := r.Read(func(a *archive.Archive) error {
		sym, err := a.Lookup(symbol)
		unknown := errors.Is(err, archive.ErrUnknownSymbol)
		if err != nil && !unknown {
			return err
		}
		// Known is not the same as collected: with the exchange lists off,
		// the catalog still names every listed symbol. One nobody excluded
		// is put on the user's list either way.
		// A read-only copy can't queue anything; it holds what it holds.
		if a.ReadOnly() && unknown {
			return fmt.Errorf("%w: %s isn't in this copy of the archive", ErrNoBars, symbol)
		}
		if !a.ReadOnly() && (unknown || (!sym.Active && !sym.Excluded)) {
			if err := a.Add(archive.User, archive.Entry{Symbol: symbol}, time.Now()); err != nil {
				return err
			}
			s.added = true
		}
		if unknown {
			return fmt.Errorf("%w: %s wasn't in the archive; it has been added and goes first in line — try again once it has been collected", ErrNoBars, symbol)
		}
		q := archive.Query{Symbol: symbol, Interval: rd.key.interval, From: rd.from, To: rd.to}
		if s.bars, err = a.Best(q); err != nil {
			return err
		}
		if rd.key.dividends {
			if s.dividends, err = a.Dividends(symbol, rd.from, rd.to); err != nil {
				return err
			}
		}
		cursors, err := a.SymbolCursors(sym.ID)
		if err != nil {
			return err
		}
		complete := false
		for _, c := range cursors {
			if c.Interval == rd.key.interval && c.Complete {
				complete = true
			}
		}
		if !complete {
			s.warnings = append(s.warnings, fmt.Sprintf("%s's %s history is still being collected; the test covers what the archive holds so far", symbol, rd.key.interval))
		}
		return nil
	})
	return s, err
}

// window cuts one run's bars from a read: its warm-up lead-in, then its
// window from start on, adjusted for the dividends inside that span alone.
func (s archived) window(from, to time.Time, warm int) (bars []quotes.Candle, start int, warnings []string, err error) {
	lead := from.Add(-lookback(s.key.interval, warm, false))
	lo := sort.Search(len(s.bars), func(i int) bool { return !s.bars[i].Time.Before(lead) })
	hi := sort.Search(len(s.bars), func(i int) bool { return !s.bars[i].Time.Before(to) })
	bars = s.bars[lo:hi]
	start = sort.Search(len(bars), func(i int) bool { return !bars[i].Time.Before(from) })
	if start >= len(bars) && s.added {
		return nil, 0, nil, fmt.Errorf("%w: %s wasn't being collected; it has been added and goes first in line — try again once it has been collected", ErrNoBars, s.key.symbol)
	}
	if start >= len(bars) {
		return nil, 0, nil, fmt.Errorf("%w: the archive holds no %s bars for %s between %s and %s", ErrNoBars,
			s.key.interval, s.key.symbol, from.Format(time.DateOnly), to.AddDate(0, 0, -1).Format(time.DateOnly))
	}
	var dividends []quotes.Dividend
	for _, d := range s.dividends {
		if !d.Time.Before(lead) && d.Time.Before(to) {
			dividends = append(dividends, d)
		}
	}
	if len(dividends) > 0 {
		bars = adjustCandles(bars, dividends)
	}
	warnings = append([]string(nil), s.warnings...)
	if first := bars[start].Time; first.Sub(from) > 7*24*time.Hour {
		warnings = append(warnings, fmt.Sprintf("the archive's bars start on %s, after the start asked for", first.Format(time.DateOnly)))
	}
	if start < warm-1 {
		warnings = append(warnings, "the archive has too little history before the start for every indicator to be settled on the first bars")
	}
	return bars, start, warnings, nil
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
