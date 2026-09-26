package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// Bounds on the research tools.
const (
	defaultTrades    = 20
	maxTradesShown   = 500
	maxEquityPoints  = 500
	defaultTop       = 10
	maxTop           = 50
	maxStudySymbols  = 25
	defaultOccurs    = 10
	maxOccurs        = 200
	failuresReported = 5
)

// strategySchema is a strategy definition as JSON Schema, shared by every
// tool that takes one. The descriptions carry the rule language, since the
// schema is what a model reads when it writes a call.
const strategySchema = `{"type":"object","properties":{
	"symbol":{"type":"string"},
	"interval":{"type":"string","enum":["1d","1h","5m","1m"],"description":"Bar width; default 1d."},
	"from":{"type":"string","description":"First day tested, YYYY-MM-DD. Required."},
	"to":{"type":"string","description":"Last day tested, YYYY-MM-DD, inclusive; default today."},
	"entry":` + ruleSchema + `,
	"exit":` + ruleSchema + `,
	"stopLoss":{"type":"number","description":"Percent below the entry price; 0 is none."},
	"takeProfit":{"type":"number","description":"Percent above the entry price; 0 is none."},
	"feePercent":{"type":"number","description":"Charged on the value of each trade, both ways."},
	"initial":{"type":"number","description":"Starting cash; default 10000."},
	"dividends":{"type":"boolean","description":"Trade on dividend-adjusted daily prices, so holding a payer earns its payouts."}
},"required":["symbol","from","entry"],"additionalProperties":false}`

const ruleSchema = `{"type":"object","properties":{
	"match":{"type":"string","enum":["all","any"],"description":"all (default): every condition must hold; any: one is enough."},
	"conditions":{"type":"array","maxItems":8,"items":{"type":"object","properties":{
		"left":{"type":"string","description":"An operand: open, high, low, close (or price), volume, a number, or an indicator spec with an optional line, e.g. sma:50, rsi:14, bb:20:2.lower, macd:12:26:9.signal, stoch:14:3.k."},
		"op":{"type":"string","enum":[">","<",">=","<=","crosses_above","crosses_below"]},
		"right":{"type":"string","description":"An operand, as for left. Numbers are written as strings: \"70\"."}
	},"required":["left","op","right"],"additionalProperties":false}}
},"required":["conditions"],"additionalProperties":false}`

func (t *tools) registerResearch(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "run_backtest",
		Title: "Backtest a strategy",
		Description: "Backtest a long-only, rule-based strategy on one symbol over the archive's bars, against buying and holding it. " +
			"A rule is conditions compared on each bar's close; an order fills at the next bar's open, all in, and a stop or target " +
			"trips inside the bar (at the open if the bar gaps through it; the stop first if a bar reaches both). With no exit rule a " +
			"position holds until a stop, a target or the end. Returns strategy and buy-and-hold metrics (total return, CAGR, max " +
			"drawdown, Sharpe from per-bar returns at a zero rate), trade statistics, the last trades and warnings about the data. " +
			"A symbol the archive isn't collecting is added and the call says to retry later. Full rule language: resource " + languageURI + ".",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"strategy":` + strategySchema + `,
			"trades":{"type":"integer","minimum":0,"maximum":500,"description":"How many of the most recent trades to list; default 20."},
			"equityPoints":{"type":"integer","minimum":0,"maximum":500,"description":"Sample the equity curves at this many points; default 0 (none)."}
		},"required":["strategy"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.backtest),
	})
	s.AddTool(mcp.Tool{
		Name:  "sweep_strategy",
		Title: "Sweep a strategy's parameters",
		Description: "Backtest every combination of a grid of parameters and rank the results — the way to tune a strategy. The template " +
			"is a run_backtest strategy whose strings hold placeholders like {fast}: \"sma:{fast}\", \"{level}\", or a whole numeric field " +
			"written as a string, \"stopLoss\": \"{stop}\". params gives each placeholder's values; at most 4 parameters and 250 " +
			"combinations. The best of many variants on the same data flatters itself: set holdoutFrom to rank on the data before " +
			"that date and report how the leaders did after it, which they were not chosen on.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"template":{"type":"object","description":"A strategy as for run_backtest, with {name} placeholders."},
			"params":{"type":"object","additionalProperties":{"type":"array","items":{"type":"number"},"minItems":1,"maxItems":50},"description":"e.g. {\"fast\":[10,20,50],\"slow\":[100,200]}"},
			"rankBy":{"type":"string","enum":["sharpe","totalReturn","cagr","maxDrawdown","profitFactor","winRate","excessReturn"],"description":"Default sharpe. excessReturn is total return over buy-and-hold's."},
			"top":{"type":"integer","minimum":1,"maximum":50,"description":"How many leaders to return; default 10."},
			"minTrades":{"type":"integer","minimum":0,"description":"Leave out variants that traded fewer times than this; default 1."},
			"holdoutFrom":{"type":"string","description":"YYYY-MM-DD. Rank on [from, holdoutFrom) and test the leaders on [holdoutFrom, to]."}
		},"required":["template","params"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.sweep),
	})
	s.AddTool(mcp.Tool{
		Name:  "find_signals",
		Title: "Study a pattern",
		Description: "Find every bar where a condition held — on one symbol or pooled across up to 25 — and measure the return that " +
			"followed at each horizon: from the next bar's open (the first price anyone acting on it could get) to the close N bars " +
			"after the signal. Each horizon comes with the baseline — the same measurement from every bar — so the edge is mean minus " +
			"baselineMean. By default only the first bar of each stretch where the condition holds counts. Also says which symbols " +
			"match right now (holdsNow). Use it to learn whether a pattern carries information before building a strategy on it. " +
			"The signal uses the run_backtest rule language.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"symbols":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":25},
			"signal":` + ruleSchema + `,
			"interval":{"type":"string","enum":["1d","1h","5m","1m"]},
			"from":{"type":"string","description":"YYYY-MM-DD. Required."},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive; default today."},
			"horizons":{"type":"array","items":{"type":"integer","minimum":1,"maximum":1000},"maxItems":8,"description":"Bars after the signal; default [1,5,20]."},
			"everyBar":{"type":"boolean","description":"Count every bar the condition holds, not only the first of each stretch."},
			"dividends":{"type":"boolean","description":"Measure on dividend-adjusted daily prices."},
			"occurrences":{"type":"integer","minimum":0,"maximum":200,"description":"How many of the most recent occurrences to list, each with its returns in the order of horizons; default 10."}
		},"required":["symbols","signal","from"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.signals),
	})
	s.AddTool(mcp.Tool{
		Name:  "backtest_portfolio",
		Title: "Backtest a portfolio",
		Description: "Simulate a buy-and-hold allocation month by month, with rebalancing and contributions, against an optional " +
			"benchmark, over dividend-adjusted history from the quote provider (not the archive, so any symbol it knows works). Returns " +
			"CAGR, volatility, max drawdown, Sharpe and Sortino against T-bills, calendar-year returns and each holding's contribution.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"holdings":{"type":"array","minItems":1,"items":{"type":"object","properties":{
				"symbol":{"type":"string"},"weight":{"type":"number","description":"Percent; weights are normalised to 100."}
			},"required":["symbol","weight"],"additionalProperties":false}},
			"initialAmount":{"type":"number","description":"Default 10000."},
			"startYear":{"type":"integer","description":"Default: as far back as every holding has data."},
			"endYear":{"type":"integer"},
			"rebalance":{"type":"string","enum":["none","monthly","quarterly","annually"]},
			"contribution":{"type":"number","description":"Added each period."},
			"contributionFrequency":{"type":"string","enum":["none","monthly","quarterly","annually"]},
			"benchmark":{"type":"string","description":"A symbol to compare with, e.g. SPY."},
			"monthly":{"type":"boolean","description":"Include the month-by-month balances."}
		},"required":["holdings"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.portfolio),
	})
}

// metricsView is a curve's metrics, rounded.
type metricsView struct {
	Final       float64 `json:"final"`
	TotalReturn float64 `json:"totalReturn"`
	CAGR        float64 `json:"cagr"`
	MaxDrawdown float64 `json:"maxDrawdown"`
	Sharpe      float64 `json:"sharpe"`
}

func viewMetrics(m strategy.Metrics) metricsView {
	return metricsView{Final: round(m.Final, 2), TotalReturn: round(m.TotalReturn, 2), CAGR: round(m.CAGR, 2),
		MaxDrawdown: round(m.MaxDrawdown, 2), Sharpe: round(m.Sharpe, 3)}
}

type statsView struct {
	Trades       int      `json:"trades"`
	WinRate      float64  `json:"winRate"`
	ProfitFactor *float64 `json:"profitFactor"`
	AvgTrade     float64  `json:"avgTrade"`
	AvgBars      float64  `json:"avgBars"`
	BestTrade    float64  `json:"bestTrade"`
	WorstTrade   float64  `json:"worstTrade"`
	Exposure     float64  `json:"exposure"`
}

func viewStats(s strategy.Stats) statsView {
	v := statsView{Trades: s.Trades, WinRate: round(s.WinRate, 1), AvgTrade: round(s.AvgTrade, 2), AvgBars: round(s.AvgBars, 1),
		BestTrade: round(s.BestTrade, 2), WorstTrade: round(s.WorstTrade, 2), Exposure: round(s.Exposure, 1)}
	if s.ProfitFactor != nil {
		pf := round(*s.ProfitFactor, 2)
		v.ProfitFactor = &pf
	}
	return v
}

type tradeView struct {
	Entry      string  `json:"entry"`
	EntryPrice float64 `json:"entryPrice"`
	Exit       string  `json:"exit"`
	ExitPrice  float64 `json:"exitPrice"`
	Return     float64 `json:"return"`
	Bars       int     `json:"bars"`
	Reason     string  `json:"reason"`
}

type equityView struct {
	Time   string  `json:"time"`
	Equity float64 `json:"equity"`
	Hold   float64 `json:"hold"`
}

type backtestView struct {
	Symbol     string       `json:"symbol"`
	Interval   string       `json:"interval"`
	From       string       `json:"from"`
	To         string       `json:"to"`
	Bars       int          `json:"bars"`
	Strategy   metricsView  `json:"strategy"`
	BuyAndHold metricsView  `json:"buyAndHold"`
	Stats      statsView    `json:"stats"`
	Trades     []tradeView  `json:"trades,omitempty"`
	Equity     []equityView `json:"equity,omitempty"`
	// Overfitting says how far the result can be trusted given everything
	// tried on the same series; see research_log.
	Overfitting *overfitView `json:"overfitting,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

type backtestArgs struct {
	Strategy     strategy.Definition `json:"strategy"`
	Trades       *int                `json:"trades"`
	EquityPoints int                 `json:"equityPoints"`
}

func (t *tools) backtest(_ context.Context, in backtestArgs) (any, error) {
	res, err := t.engine.RunStrategy(in.Strategy)
	if err != nil {
		return nil, err
	}
	trades := defaultTrades
	if in.Trades != nil {
		trades = min(max(*in.Trades, 0), maxTradesShown)
	}
	v := viewBacktest(res)
	judged, warning := t.judge("run_backtest", []strategy.Definition{in.Strategy}, []*strategy.Result{&res})
	if v.Overfitting = judged[0]; warning != "" {
		v.Warnings = append(v.Warnings, warning)
	}
	shown := res.Trades[max(0, len(res.Trades)-trades):]
	for _, tr := range shown {
		v.Trades = append(v.Trades, tradeView{Entry: stampString(tr.EntryTime, res.Interval), EntryPrice: round(tr.EntryPrice, 4),
			Exit: stampString(tr.ExitTime, res.Interval), ExitPrice: round(tr.ExitPrice, 4), Return: round(tr.Return, 2), Bars: tr.Bars, Reason: tr.Reason})
	}
	if n := min(in.EquityPoints, maxEquityPoints); n > 0 && len(res.Equity) > 0 {
		for _, i := range sample(len(res.Equity), n) {
			p := res.Equity[i]
			v.Equity = append(v.Equity, equityView{Time: stampString(p.T, res.Interval), Equity: round(p.Equity, 2), Hold: round(p.Hold, 2)})
		}
	}
	return v, nil
}

func viewBacktest(res strategy.Result) backtestView {
	return backtestView{Symbol: res.Symbol, Interval: res.Interval, From: stampString(res.From, res.Interval), To: stampString(res.To, res.Interval),
		Bars: res.Bars, Strategy: viewMetrics(res.Strategy), BuyAndHold: viewMetrics(res.Hold), Stats: viewStats(res.Stats), Warnings: res.Warnings}
}

// stampString is stamp for an interval still spelled as a string.
func stampString(t time.Time, interval string) string {
	i, err := parseInterval(interval)
	if err != nil || t.IsZero() {
		return date(t)
	}
	return stamp(t, i)
}

// sample picks n indexes spread evenly over [0, total), always including
// the last.
func sample(total, n int) []int {
	if n >= total {
		out := make([]int, total)
		for i := range out {
			out[i] = i
		}
		return out
	}
	if n == 1 {
		return []int{total - 1}
	}
	out := make([]int, 0, n)
	for k := 0; k < n; k++ {
		out = append(out, k*(total-1)/(n-1))
	}
	return out
}

type sweepArgs struct {
	Template    json.RawMessage      `json:"template"`
	Params      map[string][]float64 `json:"params"`
	RankBy      string               `json:"rankBy"`
	Top         int                  `json:"top"`
	MinTrades   *int                 `json:"minTrades"`
	HoldoutFrom string               `json:"holdoutFrom"`
}

type sweepRow struct {
	Rank     int                `json:"rank"`
	Params   map[string]float64 `json:"params"`
	Score    float64            `json:"score"`
	Strategy metricsView        `json:"strategy"`
	Stats    statsView          `json:"stats"`
	// Excess is the strategy's total return over buy-and-hold's.
	Excess      float64      `json:"excessReturn"`
	Overfitting *overfitView `json:"overfitting,omitempty"`
	Holdout     *holdoutView `json:"holdout,omitempty"`
	variant     int
}

type holdoutView struct {
	Score      *float64    `json:"score"`
	Strategy   metricsView `json:"strategy"`
	BuyAndHold metricsView `json:"buyAndHold"`
	Stats      statsView   `json:"stats"`
	Error      string      `json:"error,omitempty"`
}

type failedParams struct {
	Params map[string]float64 `json:"params"`
	Error  string             `json:"error"`
}

type sweepView struct {
	Variants   int            `json:"variants"`
	Ranked     int            `json:"ranked"`
	TooFew     int            `json:"tooFewTrades,omitempty"`
	Failed     int            `json:"failed,omitempty"`
	Failures   []failedParams `json:"failures,omitempty"`
	RankedBy   string         `json:"rankedBy"`
	Window     string         `json:"window"`
	Holdout    string         `json:"holdoutWindow,omitempty"`
	BuyAndHold *metricsView   `json:"buyAndHold,omitempty"`
	Top        []sweepRow     `json:"top"`
	Notes      []string       `json:"notes,omitempty"`
}

func (t *tools) sweep(_ context.Context, in sweepArgs) (any, error) {
	rankBy := in.RankBy
	if rankBy == "" {
		rankBy = strategy.RankSharpe
	}
	if _, _, err := strategy.Score(strategy.Result{}, rankBy); err != nil {
		return nil, err
	}
	minTrades := 1
	if in.MinTrades != nil {
		minTrades = max(*in.MinTrades, 0)
	}
	variants, err := strategy.Grid(in.Template, in.Params)
	if err != nil {
		return nil, err
	}
	defs := make([]strategy.Definition, len(variants))
	for i, v := range variants {
		defs[i] = v.Definition
	}
	var split time.Time
	if in.HoldoutFrom != "" {
		if split, err = time.Parse(time.DateOnly, in.HoldoutFrom); err != nil {
			return nil, errors.New("holdoutFrom must be a date like 2020-01-01")
		}
		for i := range defs {
			defs[i].To = split.AddDate(0, 0, -1).Format(time.DateOnly)
		}
	}

	runs := t.engine.RunStrategies(defs)
	out := sweepView{Variants: len(variants), RankedBy: rankBy, Top: []sweepRow{}}
	var rows []sweepRow
	var warnings []string
	var firstErr error
	results := make([]*strategy.Result, len(runs))
	for i, run := range runs {
		if run.Err == nil {
			results[i] = &runs[i].Result
		}
	}
	// Every variant that ran is a trial, ranked or not: the ones that lost
	// were tried all the same, and are what the winner is deflated by.
	judged, judgeWarning := t.judge("sweep_strategy", defs, results)
	if judgeWarning != "" {
		warnings = append(warnings, judgeWarning)
	}
	for i, run := range runs {
		if run.Err != nil {
			if firstErr == nil {
				firstErr = run.Err
			}
			out.Failed++
			if len(out.Failures) < failuresReported {
				out.Failures = append(out.Failures, failedParams{Params: variants[i].Params, Error: explain(run.Err).Error()})
			}
			continue
		}
		res := run.Result
		for _, w := range res.Warnings {
			// A variant that never traded is counted, not repeated.
			if !slices.Contains(warnings, w) && w != strategy.WarnNoTrades {
				warnings = append(warnings, w)
			}
		}
		if out.BuyAndHold == nil {
			bh := viewMetrics(res.Hold)
			out.BuyAndHold = &bh
			out.Window = stampString(res.From, res.Interval) + " to " + stampString(res.To, res.Interval)
		}
		if res.Stats.Trades < minTrades {
			out.TooFew++
			continue
		}
		score, ok, _ := strategy.Score(res, rankBy)
		if !ok {
			out.TooFew++
			continue
		}
		rows = append(rows, sweepRow{Params: variants[i].Params, Score: round(score, 3), Strategy: viewMetrics(res.Strategy),
			Stats: viewStats(res.Stats), Excess: round(res.Strategy.TotalReturn-res.Hold.TotalReturn, 2), Overfitting: judged[i], variant: i})
	}
	if out.Failed == len(variants) {
		// Every variant failed: almost always one reason — no bars yet, or
		// a template that can't run — and that reason is the answer.
		return nil, firstErr
	}
	out.Ranked = len(rows)
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].Score > rows[b].Score })
	rows = rows[:min(len(rows), clamp(in.Top, defaultTop, maxTop))]

	if !split.IsZero() && len(rows) > 0 {
		holdDefs := make([]strategy.Definition, len(rows))
		for k, row := range rows {
			d := variants[row.variant].Definition
			d.From = split.Format(time.DateOnly)
			holdDefs[k] = d
		}
		for k, run := range t.engine.RunStrategies(holdDefs) {
			h := &holdoutView{}
			if run.Err != nil {
				h.Error = explain(run.Err).Error()
			} else {
				res := run.Result
				h.Strategy, h.BuyAndHold, h.Stats = viewMetrics(res.Strategy), viewMetrics(res.Hold), viewStats(res.Stats)
				if score, ok, _ := strategy.Score(res, rankBy); ok {
					s := round(score, 3)
					h.Score = &s
				}
				if out.Holdout == "" {
					out.Holdout = stampString(res.From, res.Interval) + " to " + stampString(res.To, res.Interval)
				}
			}
			rows[k].Holdout = h
		}
	}
	for k := range rows {
		rows[k].Rank = k + 1
	}
	out.Top = rows
	switch {
	case split.IsZero() && out.Ranked > 1:
		out.Notes = append(out.Notes, fmt.Sprintf("these are the best of %d variants on the data they were chosen on, which flatters them; set holdoutFrom to see how the leaders do on data they weren't chosen on", out.Ranked))
	case !split.IsZero():
		out.Notes = append(out.Notes, "the leaders were ranked before holdoutFrom only; their holdout numbers are the honest estimate")
	}
	out.Notes = append(out.Notes, warnings...)
	if out.Ranked == 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("no variant made at least %d trades; loosen the entry rule or lower minTrades", minTrades))
	}
	return out, nil
}

type signalArgs struct {
	Symbols     []string      `json:"symbols"`
	Signal      strategy.Rule `json:"signal"`
	Interval    string        `json:"interval"`
	From        string        `json:"from"`
	To          string        `json:"to"`
	Horizons    []int         `json:"horizons"`
	EveryBar    bool          `json:"everyBar"`
	Dividends   bool          `json:"dividends"`
	Occurrences *int          `json:"occurrences"`
}

// horizonView is one horizon's statistics, with its edge worked out.
type horizonView struct {
	Bars            int     `json:"bars"`
	Count           int     `json:"count"`
	Mean            float64 `json:"mean"`
	Median          float64 `json:"median"`
	WinRate         float64 `json:"winRate"`
	Best            float64 `json:"best"`
	Worst           float64 `json:"worst"`
	BaselineMean    float64 `json:"baselineMean"`
	BaselineWinRate float64 `json:"baselineWinRate"`
	// Edge is Mean over BaselineMean: what knowing the signal added.
	Edge float64 `json:"edge"`
}

func viewHorizons(hs []strategy.HorizonStats) []horizonView {
	out := make([]horizonView, len(hs))
	for i, h := range hs {
		out[i] = horizonView{Bars: h.Bars, Count: h.Count, Mean: round(h.Mean, 3), Median: round(h.Median, 3), WinRate: round(h.WinRate, 1),
			Best: round(h.Best, 2), Worst: round(h.Worst, 2), BaselineMean: round(h.Baseline.Mean, 3), BaselineWinRate: round(h.Baseline.WinRate, 1)}
		if h.Count > 0 {
			out[i].Edge = round(h.Mean-h.Baseline.Mean, 3)
		}
	}
	return out
}

type studySymbolView struct {
	Symbol     string        `json:"symbol"`
	Window     string        `json:"window"`
	Bars       int           `json:"bars"`
	Signals    int           `json:"signals"`
	LastSignal string        `json:"lastSignal,omitempty"`
	HoldsNow   bool          `json:"holdsNow"`
	Horizons   []horizonView `json:"horizons"`
	Warnings   []string      `json:"warnings,omitempty"`
}

type occurrenceView struct {
	Symbol  string     `json:"symbol"`
	Time    string     `json:"time"`
	Close   float64    `json:"close"`
	Entry   *float64   `json:"entry"`
	Returns []*float64 `json:"returns"`
	at      time.Time
}

type symbolError struct {
	Symbol string `json:"symbol"`
	Error  string `json:"error"`
}

func (t *tools) signals(_ context.Context, in signalArgs) (any, error) {
	if len(in.Symbols) == 0 {
		return nil, errors.New("name at least one symbol")
	}
	if len(in.Symbols) > maxStudySymbols {
		return nil, fmt.Errorf("a study covers at most %d symbols at once", maxStudySymbols)
	}
	shown := defaultOccurs
	if in.Occurrences != nil {
		shown = min(max(*in.Occurrences, 0), maxOccurs)
	}
	out := struct {
		Horizons    []int             `json:"horizons"`
		Pooled      []horizonView     `json:"pooled,omitempty"`
		HoldsNow    []string          `json:"holdsNow"`
		Symbols     []studySymbolView `json:"symbols"`
		Occurrences []occurrenceView  `json:"recentOccurrences"`
		Errors      []symbolError     `json:"errors,omitempty"`
	}{HoldsNow: []string{}, Symbols: []studySymbolView{}, Occurrences: []occurrenceView{}}

	var studies []strategy.Study
	seen := map[string]bool{}
	for _, sym := range in.Symbols {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if seen[sym] {
			continue
		}
		seen[sym] = true
		st, err := t.engine.RunStudy(strategy.StudyDefinition{Symbol: sym, Interval: in.Interval, From: in.From, To: in.To,
			Dividends: in.Dividends, Signal: in.Signal, Horizons: in.Horizons, EveryBar: in.EveryBar})
		if strategy.IsInvalid(err) || errors.Is(err, engine.ErrNoArchive) {
			return nil, err // the same for every symbol; say it once
		}
		if err != nil {
			out.Errors = append(out.Errors, symbolError{Symbol: sym, Error: explain(err).Error()})
			continue
		}
		studies = append(studies, st)
		v := studySymbolView{Symbol: st.Symbol, Window: stampString(st.From, st.Interval) + " to " + stampString(st.To, st.Interval),
			Bars: st.Bars, Signals: st.Signals, HoldsNow: st.HoldsNow, Horizons: viewHorizons(st.Horizons), Warnings: st.Warnings}
		if n := len(st.Occurrences); n > 0 {
			v.LastSignal = stampString(st.Occurrences[n-1].Time, st.Interval)
		}
		if st.HoldsNow {
			out.HoldsNow = append(out.HoldsNow, st.Symbol)
		}
		out.Symbols = append(out.Symbols, v)
		for _, o := range st.Occurrences {
			ov := occurrenceView{Symbol: st.Symbol, Time: stampString(o.Time, st.Interval), Close: round(o.Close, 4), Returns: make([]*float64, len(o.Returns)), at: o.Time}
			if o.Entry > 0 {
				e := round(o.Entry, 4)
				ov.Entry = &e
			}
			for k, r := range o.Returns {
				if r != nil {
					x := round(*r, 2)
					ov.Returns[k] = &x
				}
			}
			out.Occurrences = append(out.Occurrences, ov)
		}
	}
	if len(studies) == 0 {
		return nil, errors.New(out.Errors[0].Error)
	}
	for _, h := range studies[0].Horizons {
		out.Horizons = append(out.Horizons, h.Bars)
	}
	if len(studies) > 1 {
		out.Pooled = viewHorizons(strategy.Pool(studies))
	}
	sort.SliceStable(out.Occurrences, func(a, b int) bool { return out.Occurrences[a].at.After(out.Occurrences[b].at) })
	out.Occurrences = out.Occurrences[:min(len(out.Occurrences), shown)]
	return out, nil
}

type portfolioArgs struct {
	Holdings []struct {
		Symbol string  `json:"symbol"`
		Weight float64 `json:"weight"`
	} `json:"holdings"`
	InitialAmount         float64 `json:"initialAmount"`
	StartYear             int     `json:"startYear"`
	EndYear               int     `json:"endYear"`
	Rebalance             string  `json:"rebalance"`
	Contribution          float64 `json:"contribution"`
	ContributionFrequency string  `json:"contributionFrequency"`
	Benchmark             string  `json:"benchmark"`
	Monthly               bool    `json:"monthly"`
}

func (t *tools) portfolio(ctx context.Context, in portfolioArgs) (any, error) {
	spec := engine.BacktestSpec{InitialAmount: in.InitialAmount, StartYear: in.StartYear, EndYear: in.EndYear, Rebalance: in.Rebalance,
		Contribution: in.Contribution, ContributionFrequency: in.ContributionFrequency, Benchmark: in.Benchmark}
	if spec.InitialAmount == 0 {
		spec.InitialAmount = 10000
	}
	if spec.Rebalance == "" {
		spec.Rebalance = store.RebalanceNone
	}
	for name, v := range map[string]string{"rebalance": spec.Rebalance, "contributionFrequency": spec.ContributionFrequency} {
		if v != "" && !store.ValidCadence(v) {
			return nil, fmt.Errorf(`%s has to be "none", "annually", "quarterly" or "monthly"`, name)
		}
	}
	for _, h := range in.Holdings {
		spec.Holdings = append(spec.Holdings, store.Holding{Symbol: h.Symbol, Weight: h.Weight})
	}
	res, err := t.engine.Backtest(ctx, spec)
	if err != nil {
		return nil, err
	}
	if !in.Monthly {
		res.Points = nil
	}
	return res, nil
}
