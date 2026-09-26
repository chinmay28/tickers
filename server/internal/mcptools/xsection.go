package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/expr"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
	"github.com/chinmay28/tickers/server/internal/xsection"
)

// Schemas the cross-sectional tools share.
const (
	universeSchema = `{"type":"object","description":"Which symbols to rank: a list, or the most-traded of a kind over the window.","properties":{
		"symbols":{"type":"array","items":{"type":"string"},"maxItems":1000},
		"kind":{"type":"string","enum":["","stock","etf","other"],"description":"With no symbols: only this kind."},
		"size":{"type":"integer","minimum":1,"maximum":1000,"description":"With no symbols: the most-traded this many; default 200."}
	},"additionalProperties":false}`
	factorDoc = "What to rank by: return:N[:S] (return over N days ending S days ago; 12-1 momentum is return:252:21), " +
		"volatility:N (annualised), dollarvolume:N, drawdown:N (below the N-day high), distance:X (close above operand X, e.g. distance:sma:200), " +
		"or any rule operand such as rsi:14. Percentages are percentages."
	filterProps = `"minPrice":{"type":"number","description":"Skip symbols closing below this on the day."},
		"minDollarVolume":{"type":"number","description":"Skip symbols whose 20-day average close × volume is below this on the day."},
		"where":` + ruleSchema
)

func (t *tools) registerXSection(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "screen",
		Title: "Screen a universe",
		Description: "Rank a universe of symbols by a factor on one day (default the latest): the leaders by momentum today, the most " +
			"oversold names above their 200-day average, the least volatile ETFs. Filters are judged on that day's data. " + factorDoc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"universe":` + universeSchema + `,
			"factor":{"type":"string"},
			"date":{"type":"string","description":"YYYY-MM-DD; the last trading day on or before it. Default: the latest."},
			"ascending":{"type":"boolean","description":"Lowest first."},
			"top":{"type":"integer","minimum":1,"maximum":500,"description":"Default 25."},
			` + filterProps + `
		},"required":["factor"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.screen),
	})
	s.AddTool(mcp.Tool{
		Name:  "factor_study",
		Title: "Does a factor predict returns?",
		Description: "Rank a universe by a factor every N days and measure the return that followed each ranking: from the next open to " +
			"the close horizon days later. Reports the information coefficient (rank correlation of factor and next return; a steady 0.03–0.05 " +
			"is useful, with a t-statistic above 2 that allows for overlapping horizons), the mean return of each quantile, and the top-minus-" +
			"bottom spread. The cross-sectional counterpart of find_signals: whether ranking by it says anything, before trading it with " +
			"rotation_backtest. Daily bars only. " + factorDoc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"universe":` + universeSchema + `,
			"factor":{"type":"string"},
			"from":{"type":"string","description":"YYYY-MM-DD, required."},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive; default today."},
			"every":{"type":"integer","minimum":1,"maximum":252,"description":"Trading days between rankings; default 21 (monthly)."},
			"horizon":{"type":"integer","minimum":1,"maximum":252,"description":"Trading days the return is measured over; default every."},
			"quantiles":{"type":"integer","minimum":2,"maximum":10,"description":"Default 5."},
			"dividends":{"type":"boolean","description":"Measure on dividend-adjusted prices."},
			"points":{"type":"integer","minimum":0,"maximum":500,"description":"How many of the most recent rankings to list; default 12."},
			` + filterProps + `
		},"required":["factor","from"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.factorStudy),
	})
	s.AddTool(mcp.Tool{
		Name:  "rotation_backtest",
		Title: "Backtest a rank-and-hold rotation",
		Description: "Every N trading days, rank a universe by a factor and hold the top few (or the bottom, with ascending), equally " +
			"weighted, until the next ranking; rank on a close, trade at the next open, fees on what is traded. Compared with a benchmark " +
			"symbol bought and held, or by default every eligible symbol equally weighted — what ranking added over owning the lot. A holding " +
			"that delists is valued at its last close. Recorded in research_log under the universe. " + factorDoc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"universe":` + universeSchema + `,
			"factor":{"type":"string"},
			"from":{"type":"string","description":"YYYY-MM-DD, required."},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive; default today."},
			"every":{"type":"integer","minimum":1,"maximum":252,"description":"Trading days between rebalances; default 21 (monthly)."},
			"hold":{"type":"integer","minimum":1,"maximum":100,"description":"How many to hold; default 10."},
			"ascending":{"type":"boolean","description":"Hold the lowest-ranked instead."},
			"feePercent":{"type":"number","minimum":0,"maximum":10},
			"initial":{"type":"number"},
			"benchmark":{"type":"string","description":"A symbol to buy and hold beside it, e.g. SPY; it is loaded but never held."},
			"dividends":{"type":"boolean","description":"Trade dividend-adjusted prices."},
			"picks":{"type":"integer","minimum":0,"maximum":100,"description":"How many of the latest rebalances' holdings to list; default 3."},
			"equityPoints":{"type":"integer","minimum":0,"maximum":500},
			` + filterProps + `
		},"required":["factor","from"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.rotation),
	})
}

type universeArgs struct {
	Symbols []string `json:"symbols"`
	Kind    string   `json:"kind"`
	Size    int      `json:"size"`
}

func (u universeArgs) engine() engine.Universe {
	return engine.Universe{Symbols: u.Symbols, Kind: u.Kind, Size: u.Size}
}

// family names a universe in the research log: rotations over the same
// universe are the trials competing over the same data.
func (u universeArgs) family() string {
	if len(u.Symbols) == 0 {
		size := u.Size
		if size <= 0 {
			size = engine.DefaultUniverse
		}
		kind := u.Kind
		if kind == "" {
			kind = "any"
		}
		return fmt.Sprintf("TOP%d-%s", size, strings.ToUpper(kind))
	}
	syms := make([]string, len(u.Symbols))
	for i, s := range u.Symbols {
		syms[i] = store.NormalizeSymbol(s)
	}
	slices.Sort(syms)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(syms), ",")))
	return fmt.Sprintf("LIST%d-%s", len(slices.Compact(syms)), strings.ToUpper(hex.EncodeToString(sum[:4])))
}

type filterArgs struct {
	MinPrice        float64        `json:"minPrice"`
	MinDollarVolume float64        `json:"minDollarVolume"`
	Where           *strategy.Rule `json:"where"`
}

func (f filterArgs) compile() (xsection.Filter, error) {
	out := xsection.Filter{MinPrice: f.MinPrice, MinDollarVolume: f.MinDollarVolume}
	if f.Where != nil {
		sig, err := strategy.CompileSignal(*f.Where)
		if err != nil {
			return out, err
		}
		for _, ref := range sig.References() {
			if expr.Looks(ref) {
				return out, strategy.Invalid("a where rule can read another symbol (close@SPY) but not a formula (%s)", ref)
			}
		}
		out.Where = &sig
	}
	return out, nil
}

// withRefs adds the series a filter's rule reads to what a panel loads;
// Panel reports them back as extras, which are never ranked.
func withRefs(u engine.Universe, f xsection.Filter) engine.Universe {
	if f.Where != nil {
		u.Also = append(slices.Clone(u.Also), f.Where.References()...)
	}
	return u
}

type screenArgs struct {
	Universe  universeArgs `json:"universe"`
	Factor    string       `json:"factor"`
	Date      string       `json:"date"`
	Ascending bool         `json:"ascending"`
	Top       int          `json:"top"`
	filterArgs
}

func (t *tools) screen(ctx context.Context, in screenArgs) (any, error) {
	factor, err := xsection.ParseFactor(in.Factor)
	if err != nil {
		return nil, err
	}
	filter, err := in.compile()
	if err != nil {
		return nil, err
	}
	day := t.now().UTC().Truncate(24 * time.Hour)
	if in.Date != "" {
		if day, err = time.Parse(time.DateOnly, in.Date); err != nil {
			return nil, errors.New("date must be a date like 2024-06-28")
		}
	}
	// A month back is the window the universe's liquidity is judged over,
	// and holds the day asked for across any holiday.
	p, info, err := t.engine.Panel(ctx, withRefs(in.Universe.engine(), filter), day.AddDate(0, 0, -30), day.AddDate(0, 0, 1), factor.Warmup()+filter.Warmup(), false)
	if err != nil {
		return nil, err
	}
	warnings := info.Warnings
	i := p.First(day.AddDate(0, 0, 1)) - 1
	if i < 0 {
		return nil, fmt.Errorf("no bars on or before %s", day.Format(time.DateOnly))
	}
	filter.Exclude = info.Extra
	ranked := xsection.Screen(p, factor, filter, i, in.Ascending)
	out := struct {
		Date     string   `json:"date"`
		Factor   string   `json:"factor"`
		Eligible int      `json:"eligible"`
		Of       int      `json:"of"`
		Columns  []string `json:"columns"`
		Rows     [][]any  `json:"rows"`
		Warnings []string `json:"warnings,omitempty"`
	}{Date: date(p.Dates[i]), Factor: factor.Text, Eligible: len(ranked), Of: len(p.Symbols), Columns: []string{"rank", "symbol", "value", "close"},
		Rows: [][]any{}, Warnings: warnings}
	for k, r := range ranked[:min(len(ranked), clamp(in.Top, 25, 500))] {
		out.Rows = append(out.Rows, []any{k + 1, r.Symbol, round(r.Value, 4), round(r.Close, 4)})
	}
	return out, nil
}

type factorStudyArgs struct {
	Universe  universeArgs `json:"universe"`
	Factor    string       `json:"factor"`
	From      string       `json:"from"`
	To        string       `json:"to"`
	Every     int          `json:"every"`
	Horizon   int          `json:"horizon"`
	Quantiles int          `json:"quantiles"`
	Dividends bool         `json:"dividends"`
	Points    *int         `json:"points"`
	filterArgs
}

// crossWindow reads a cross-sectional run's dates; from is required.
func crossWindow(from, to string, now time.Time) (time.Time, time.Time, error) {
	if from == "" {
		return time.Time{}, time.Time{}, errors.New("from is required: the first day ranked, like 2015-01-01")
	}
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return start, start, errors.New("from must be a date like 2015-01-31")
	}
	end := now.UTC().Add(24 * time.Hour)
	if to != "" {
		d, err := time.Parse(time.DateOnly, to)
		if err != nil {
			return start, start, errors.New("to must be a date like 2024-12-31")
		}
		end = d.AddDate(0, 0, 1)
	}
	if !start.Before(end) {
		return start, end, errors.New("from must be before to")
	}
	return start, end, nil
}

func (t *tools) factorStudy(ctx context.Context, in factorStudyArgs) (any, error) {
	factor, err := xsection.ParseFactor(in.Factor)
	if err != nil {
		return nil, err
	}
	filter, err := in.compile()
	if err != nil {
		return nil, err
	}
	from, to, err := crossWindow(in.From, in.To, t.now())
	if err != nil {
		return nil, err
	}
	every := clamp(in.Every, 21, 252)
	spec := xsection.StudySpec{Factor: factor, Every: every, Horizon: clamp(in.Horizon, every, 252), Quantiles: clamp(in.Quantiles, 5, 10), Filter: filter}
	p, info, err := t.engine.Panel(ctx, withRefs(in.Universe.engine(), filter), from, to, factor.Warmup()+filter.Warmup(), in.Dividends)
	if err != nil {
		return nil, err
	}
	warnings := info.Warnings
	spec.From = p.First(from)
	spec.Filter.Exclude = info.Extra
	st, err := xsection.RunStudy(p, spec)
	if err != nil {
		return nil, err
	}
	shown := 12
	if in.Points != nil {
		shown = min(max(*in.Points, 0), 500)
	}
	type summary struct {
		Mean     float64 `json:"mean"`
		Stdev    float64 `json:"stdev"`
		TStat    float64 `json:"tStat"`
		Positive float64 `json:"positive"`
	}
	view := func(s xsection.Summary, places int) summary {
		return summary{round(s.Mean, places), round(s.Stdev, places), round(s.TStat, 2), round(s.Positive, 1)}
	}
	out := struct {
		Factor     string   `json:"factor"`
		Every      int      `json:"every"`
		Horizon    int      `json:"horizon"`
		Rankings   int      `json:"rankings"`
		AvgSymbols float64  `json:"avgSymbols"`
		IC         summary  `json:"ic"`
		Spread     summary  `json:"spread"`
		Quantiles  [][]any  `json:"quantiles"`
		Columns    []string `json:"pointColumns"`
		Points     [][]any  `json:"recentPoints"`
		Warnings   []string `json:"warnings,omitempty"`
		Notes      []string `json:"notes,omitempty"`
	}{Factor: factor.Text, Every: spec.Every, Horizon: spec.Horizon, Rankings: st.Rankings, AvgSymbols: round(st.AvgSymbols, 1),
		IC: view(st.IC, 4), Spread: view(st.Spread, 3), Quantiles: [][]any{}, Columns: []string{"date", "ic", "spread", "symbols"}, Points: [][]any{}, Warnings: warnings}
	for _, q := range st.Quantiles {
		out.Quantiles = append(out.Quantiles, []any{q.N, round(q.Mean, 3)})
	}
	for _, pt := range st.Points[max(0, len(st.Points)-shown):] {
		out.Points = append(out.Points, []any{date(pt.Date), round(pt.IC, 4), round(pt.Spread, 3), pt.Symbols})
	}
	switch {
	case st.Rankings == 0:
		out.Notes = append(out.Notes, fmt.Sprintf("no ranking had at least %d eligible symbols with a return to measure", 2*spec.Quantiles))
	case st.Rankings < 24:
		out.Notes = append(out.Notes, fmt.Sprintf("only %d rankings: too few to tell a steady effect from a lucky stretch", st.Rankings))
	}
	out.Notes = append(out.Notes, "quantile 1 is the lowest factor values; returns of symbols that delisted inside a horizon are left out, which flatters whatever tends to hold them")
	return out, nil
}

type rotationArgs struct {
	Universe     universeArgs `json:"universe"`
	Factor       string       `json:"factor"`
	From         string       `json:"from"`
	To           string       `json:"to"`
	Every        int          `json:"every"`
	Hold         int          `json:"hold"`
	Ascending    bool         `json:"ascending"`
	FeePercent   float64      `json:"feePercent"`
	Initial      float64      `json:"initial"`
	Benchmark    string       `json:"benchmark"`
	Dividends    bool         `json:"dividends"`
	Picks        *int         `json:"picks"`
	EquityPoints int          `json:"equityPoints"`
	filterArgs
}

func (t *tools) rotation(ctx context.Context, in rotationArgs) (any, error) {
	factor, err := xsection.ParseFactor(in.Factor)
	if err != nil {
		return nil, err
	}
	filter, err := in.compile()
	if err != nil {
		return nil, err
	}
	from, to, err := crossWindow(in.From, in.To, t.now())
	if err != nil {
		return nil, err
	}
	u := withRefs(in.Universe.engine(), filter)
	bench := store.NormalizeSymbol(in.Benchmark)
	if bench != "" {
		u.Also = append(u.Also, bench)
	}
	p, info, err := t.engine.Panel(ctx, u, from, to, factor.Warmup()+filter.Warmup(), in.Dividends)
	if err != nil {
		return nil, err
	}
	warnings := info.Warnings
	spec := xsection.RotationSpec{Factor: factor, From: p.First(from), Every: clamp(in.Every, 21, 252), Hold: clamp(in.Hold, 10, 100),
		Ascending: in.Ascending, Filter: filter, FeePercent: in.FeePercent, Initial: in.Initial, Benchmark: bench}
	// Loaded only to be compared with or read by a rule, so not ranked.
	spec.Filter.Exclude = info.Extra
	rot, err := xsection.Rotate(p, spec)
	if err != nil {
		return nil, err
	}
	// What was tested, without the options that only shape the answer.
	def := in
	def.Picks, def.EquityPoints = nil, 0
	judged, warning := t.judge("rotation_backtest", []*trial{{symbol: in.Universe.family(), interval: string(quotes.Daily),
		definition: def, metrics: rot.Strategy, trades: rot.Rebalances}})
	if warning != "" {
		warnings = append(warnings, warning)
	}
	type pickView struct {
		Date    string    `json:"date"`
		Symbols []string  `json:"symbols"`
		Values  []float64 `json:"values"`
	}
	out := struct {
		Universe    string       `json:"universe"`
		From        string       `json:"from"`
		To          string       `json:"to"`
		Days        int          `json:"days"`
		Strategy    metricsView  `json:"strategy"`
		Benchmark   metricsView  `json:"benchmark"`
		BenchmarkIs string       `json:"benchmarkIs"`
		Rebalances  int          `json:"rebalances"`
		Turnover    float64      `json:"turnover"`
		AvgHeld     float64      `json:"avgHeld"`
		Overfitting *overfitView `json:"overfitting,omitempty"`
		Picks       []pickView   `json:"recentPicks"`
		Equity      []equityView `json:"equity,omitempty"`
		Warnings    []string     `json:"warnings,omitempty"`
	}{Universe: in.Universe.family(), From: date(rot.From), To: date(rot.To), Days: rot.Days, Strategy: viewMetrics(rot.Strategy),
		Benchmark: viewMetrics(rot.Benchmark), BenchmarkIs: "every eligible symbol, equally weighted", Rebalances: rot.Rebalances,
		Turnover: round(rot.Turnover, 1), AvgHeld: round(rot.AvgHeld, 1), Overfitting: judged[0], Picks: []pickView{},
		Warnings: append(warnings, rot.Warnings...)}
	if bench != "" {
		out.BenchmarkIs = bench + ", bought and held"
	}
	shown := 3
	if in.Picks != nil {
		shown = min(max(*in.Picks, 0), 100)
	}
	for _, pk := range rot.Picks[max(0, len(rot.Picks)-shown):] {
		vals := make([]float64, len(pk.Values))
		for k, v := range pk.Values {
			vals[k] = round(v, 3)
		}
		out.Picks = append(out.Picks, pickView{Date: date(pk.Date), Symbols: pk.Symbols, Values: vals})
	}
	if n := min(in.EquityPoints, maxEquityPoints); n > 0 {
		for _, i := range sample(len(rot.Equity), n) {
			out.Equity = append(out.Equity, equityView{Time: date(rot.Dates[i]), Equity: round(rot.Equity[i], 2), Hold: round(rot.Bench[i], 2)})
		}
	}
	return out, nil
}
