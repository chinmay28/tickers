package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/expr"
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
	reads := newReadSet()
	now := time.Now()
	for i, def := range defs {
		p, err := strategy.Compile(def, now)
		if err != nil {
			out[i].Err = err
			continue
		}
		plans[i] = p
		reads.want(p.Def.Symbol, p.Interval, p.Interval == quotes.Daily && p.Def.Dividends, p.From, p.To, p.Warmup())
		for _, ref := range p.References() {
			reads.want(ref, p.Interval, false, p.From, p.To, p.Warmup())
		}
	}
	if reads.empty() {
		return out
	}
	reads.run(e, e.archiveReader())
	for i := range defs {
		if out[i].Err != nil {
			continue
		}
		p := plans[i]
		s, err := reads.get(p.Def.Symbol, p.Interval, p.Interval == quotes.Daily && p.Def.Dividends)
		if err != nil {
			out[i].Err = err
			continue
		}
		if p.Others, err = reads.others(p.References(), p.Interval); err != nil {
			out[i].Err = err
			continue
		}
		bars, start, warnings, err := s.window(p.From, p.To, p.Warmup())
		if err != nil {
			out[i].Err = err
			continue
		}
		if expr.Looks(p.Def.Symbol) {
			warnings = append(warnings, fmt.Sprintf("%s is a formula, not something that can be bought: holding it approximates long the first leg and short the rest, without borrowing costs", p.Def.Symbol))
		}
		res := strategy.Simulate(p, bars, start)
		res.Warnings = append(warnings, res.Warnings...)
		out[i].Result = res
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
	dividends := p.Interval == quotes.Daily && p.Def.Dividends
	reads := newReadSet()
	reads.want(p.Def.Symbol, p.Interval, dividends, p.From, p.To, p.Warmup())
	for _, ref := range p.References() {
		reads.want(ref, p.Interval, false, p.From, p.To, p.Warmup())
	}
	reads.run(e, e.archiveReader())
	s, err := reads.get(p.Def.Symbol, p.Interval, dividends)
	if err != nil {
		return strategy.Study{}, err
	}
	if p.Others, err = reads.others(p.References(), p.Interval); err != nil {
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

// readSet is the archive reads a batch needs, one per series however many
// runs share it: the runs' own series, and the ones their rules read.
type readSet struct {
	order []*seriesRead
	byKey map[seriesKey]*seriesRead
	done  map[seriesKey]archived
	errs  map[seriesKey]error
}

func newReadSet() *readSet {
	return &readSet{byKey: map[seriesKey]*seriesRead{}, done: map[seriesKey]archived{}, errs: map[seriesKey]error{}}
}

func (rs *readSet) empty() bool { return len(rs.order) == 0 }

func (rs *readSet) want(symbol string, interval quotes.Interval, dividends bool, from, to time.Time, warm int) {
	key := seriesKey{symbol, interval, dividends}
	rd := rs.byKey[key]
	if rd == nil {
		rd = &seriesRead{key: key}
		rs.byKey[key] = rd
		rs.order = append(rs.order, rd)
	}
	rd.want(from, to, warm)
}

func (rs *readSet) run(e *Engine, r ArchiveReader) {
	for _, rd := range rs.order {
		s, err := e.readSeries(r, rd)
		rs.done[rd.key], rs.errs[rd.key] = s, err
	}
}

func (rs *readSet) get(symbol string, interval quotes.Interval, dividends bool) (archived, error) {
	key := seriesKey{symbol, interval, dividends}
	return rs.done[key], rs.errs[key]
}

// others are the referenced series' bars, whole: a rule reads each as of
// the bar it is judged on, so the reference's own window doesn't matter.
func (rs *readSet) others(refs []string, interval quotes.Interval) (map[string][]quotes.Candle, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := map[string][]quotes.Candle{}
	for _, ref := range refs {
		s, err := rs.get(ref, interval, false)
		if err != nil {
			return nil, fmt.Errorf("reading %s, which the rules refer to: %w", ref, err)
		}
		out[ref] = s.bars
	}
	return out, nil
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
	err := r.Read(func(a *archive.Archive) error {
		if !expr.Looks(rd.key.symbol) {
			leg, err := readSymbol(a, rd.key.symbol, rd)
			s.bars, s.dividends, s.added, s.warnings = leg.bars, leg.dividends, leg.added, leg.warnings
			return err
		}
		f, err := expr.Parse(rd.key.symbol)
		if err != nil {
			return fmt.Errorf("%w: %v", strategy.Invalid("%s isn't a formula", rd.key.symbol), err)
		}
		legs := map[string][]quotes.Candle{}
		for _, sym := range f.Symbols() {
			leg, err := readSymbol(a, sym, rd)
			s.added = s.added || leg.added
			s.warnings = append(s.warnings, leg.warnings...)
			if err != nil {
				return err
			}
			// Each leg is adjusted for its own payouts before they are
			// combined; the formula has none of its own.
			if len(leg.dividends) > 0 {
				leg.bars = adjustCandles(leg.bars, leg.dividends)
			}
			legs[sym] = leg.bars
		}
		s.bars = combine(f, legs)
		return nil
	})
	return s, err
}

// readSymbol reads one symbol's bars for a read, adding it to the archive's
// queue if it isn't being collected.
func readSymbol(a *archive.Archive, symbol string, rd *seriesRead) (archived, error) {
	var s archived
	sym, err := a.Lookup(symbol)
	unknown := errors.Is(err, archive.ErrUnknownSymbol)
	if err != nil && !unknown {
		return s, err
	}
	// A read-only copy can't queue anything; it holds what it holds.
	if a.ReadOnly() && unknown {
		return s, fmt.Errorf("%w: %s isn't in this copy of the archive", ErrNoBars, symbol)
	}
	// Known is not the same as collected: with the exchange lists off,
	// the catalog still names every listed symbol. One nobody excluded
	// is put on the user's list either way.
	if !a.ReadOnly() && (unknown || (!sym.Active && !sym.Excluded)) {
		if err := a.Add(archive.User, archive.Entry{Symbol: symbol}, time.Now()); err != nil {
			return s, err
		}
		s.added = true
	}
	if unknown {
		return s, fmt.Errorf("%w: %s wasn't in the archive; it has been added and goes first in line — try again once it has been collected", ErrNoBars, symbol)
	}
	q := archive.Query{Symbol: symbol, Interval: rd.key.interval, From: rd.from, To: rd.to}
	if s.bars, err = a.Best(q); err != nil {
		return s, err
	}
	if rd.key.dividends {
		if s.dividends, err = a.Dividends(symbol, rd.from, rd.to); err != nil {
			return s, err
		}
	}
	cursors, err := a.SymbolCursors(sym.ID)
	if err != nil {
		return s, err
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
	return s, nil
}

// combine builds a formula's bars from its legs' on the bars every leg
// has. Its open and close are the formula over the legs' opens and closes;
// its high and low are only the larger and smaller of those two, because a
// ratio's true extremes inside a bar can't be known from the legs'. A stop
// on a formula therefore trips only on closes and opens — said, not hidden.
func combine(f *expr.Expr, legs map[string][]quotes.Candle) []quotes.Candle {
	count := map[int64]int{}
	byTime := map[string]map[int64]quotes.Candle{}
	for sym, bars := range legs {
		byTime[sym] = map[int64]quotes.Candle{}
		for _, b := range bars {
			byTime[sym][b.Time.Unix()] = b
			count[b.Time.Unix()]++
		}
	}
	var times []int64
	for t, n := range count {
		if n == len(legs) {
			times = append(times, t)
		}
	}
	slices.Sort(times)
	out := make([]quotes.Candle, 0, len(times))
	opens, closes := map[string]float64{}, map[string]float64{}
	for _, t := range times {
		for sym := range legs {
			b := byTime[sym][t]
			opens[sym], closes[sym] = b.Open, b.Close
		}
		o, err1 := f.Eval(opens)
		c, err2 := f.Eval(closes)
		if err1 != nil || err2 != nil || math.IsNaN(o) || math.IsNaN(c) || math.IsInf(o, 0) || math.IsInf(c, 0) {
			continue
		}
		out = append(out, quotes.Candle{Time: time.Unix(t, 0).UTC(), Open: o, High: math.Max(o, c), Low: math.Min(o, c), Close: c})
	}
	return out
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
