package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/patterns"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/xsection"
)

func (t *tools) registerPatterns(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "seasonality",
		Title: "When returns happen",
		Description: "Group returns by when they happened and compare each group with all of them: by weekday, month, trading day of " +
			"the month (+1 first, -1 last), turn of the month (last three and first three days against the rest) — from daily bars — or by " +
			"time of day from intraday bars, in slices from the session's first bar, with the overnight gap on its own. Pooled across up to " +
			"25 symbols. Each group's tStat is its distance from the overall mean in standard errors; with many groups, expect one near 2 by " +
			"chance.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"symbols":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":25},
			"groupBy":{"type":"string","enum":["weekday","month","dayOfMonth","turnOfMonth","timeOfDay"]},
			"interval":{"type":"string","enum":["1d","1h","5m","1m"],"description":"1d for every grouping but timeOfDay, which needs 1h, 5m or 1m. Default: whichever fits."},
			"from":{"type":"string","description":"YYYY-MM-DD, required."},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive; default today."},
			"bucketMinutes":{"type":"integer","minimum":1,"maximum":390,"description":"timeOfDay slice width; default 30."},
			"dividends":{"type":"boolean","description":"Daily returns including payouts."}
		},"required":["symbols","groupBy","from"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.seasonality),
	})
	s.AddTool(mcp.Tool{
		Name:  "compare_symbols",
		Title: "Compare symbols",
		Description: "Several symbols side by side over a window of daily bars: each one's buy-and-hold return, CAGR, volatility, max " +
			"drawdown, Sharpe, skew, best and worst day, beta and correlation to the first symbol listed, and the correlation matrix of " +
			"daily returns. The starting point for pairs, hedges and diversification — and for knowing whether ten winners are one bet.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"symbols":{"type":"array","items":{"type":"string"},"minItems":2,"maxItems":25,"description":"The first is the reference for beta."},
			"from":{"type":"string","description":"YYYY-MM-DD, required."},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive; default today."},
			"dividends":{"type":"boolean","description":"Measure total returns, payouts included."}
		},"required":["symbols","from"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.compare),
	})
}

type seasonalityArgs struct {
	Symbols       []string `json:"symbols"`
	GroupBy       string   `json:"groupBy"`
	Interval      string   `json:"interval"`
	From          string   `json:"from"`
	To            string   `json:"to"`
	BucketMinutes int      `json:"bucketMinutes"`
	Dividends     bool     `json:"dividends"`
}

func (t *tools) seasonality(_ context.Context, in seasonalityArgs) (any, error) {
	if len(in.Symbols) == 0 || len(in.Symbols) > maxStudySymbols {
		return nil, fmt.Errorf("name from 1 to %d symbols", maxStudySymbols)
	}
	s, err := patterns.NewSeasonality(in.GroupBy, clamp(in.BucketMinutes, 30, 390))
	if err != nil {
		return nil, err
	}
	interval := quotes.Daily
	if s.Intraday() {
		interval = quotes.FiveMinute
	}
	if in.Interval != "" {
		if interval, err = quotes.ParseInterval(in.Interval); err != nil {
			return nil, err
		}
	}
	if s.Intraday() != interval.Intraday() {
		if s.Intraday() {
			return nil, errors.New("timeOfDay needs intraday bars: interval 1h, 5m or 1m")
		}
		return nil, fmt.Errorf("%s groups daily returns: use interval 1d, or timeOfDay for intraday bars", in.GroupBy)
	}
	from, to, err := crossWindow(in.From, in.To, t.now())
	if err != nil {
		return nil, err
	}
	var used []string
	var errs []symbolError
	for _, sym := range dedupe(in.Symbols) {
		// A week before the window supplies the close the first return
		// starts from.
		bars, err := t.engine.Bars(sym, interval, from.AddDate(0, 0, -7), to, in.Dividends)
		if err != nil {
			return nil, err
		}
		if len(bars) == 0 {
			errs = append(errs, symbolError{Symbol: sym, Error: fmt.Sprintf("no %s bars in the window", interval)})
			continue
		}
		s.Add(bars, from)
		used = append(used, sym)
	}
	buckets, all := s.Result()
	if all.Count == 0 {
		return nil, fmt.Errorf("the archive holds no %s bars for those symbols in that window; symbol_info shows what it has", interval)
	}
	out := struct {
		GroupBy  string        `json:"groupBy"`
		Interval string        `json:"interval"`
		Symbols  []string      `json:"symbols"`
		Columns  []string      `json:"columns"`
		All      []any         `json:"all"`
		Rows     [][]any       `json:"groups"`
		Errors   []symbolError `json:"errors,omitempty"`
		Note     string        `json:"note"`
	}{GroupBy: in.GroupBy, Interval: string(interval), Symbols: used,
		Columns: []string{"group", "count", "mean", "median", "stdev", "winRate", "tStat"}, Rows: [][]any{}, Errors: errs,
		Note: "returns are percentages; a group's tStat compares it with all returns, and all's compares it with zero"}
	row := func(b patterns.Bucket) []any {
		return []any{b.Key, b.Count, round(b.Mean, 4), round(b.Median, 4), round(b.Stdev, 4), round(b.WinRate, 1), round(b.TStat, 2)}
	}
	out.All = row(all)
	for _, b := range buckets {
		out.Rows = append(out.Rows, row(b))
	}
	return out, nil
}

type compareArgs struct {
	Symbols   []string `json:"symbols"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Dividends bool     `json:"dividends"`
}

func (t *tools) compare(ctx context.Context, in compareArgs) (any, error) {
	symbols := dedupe(in.Symbols)
	if len(symbols) < 2 || len(symbols) > maxStudySymbols {
		return nil, fmt.Errorf("name from 2 to %d different symbols", maxStudySymbols)
	}
	from, to, err := crossWindow(in.From, in.To, t.now())
	if err != nil {
		return nil, err
	}
	p, info, err := t.engine.Panel(ctx, engine.Universe{Symbols: symbols}, from, to, 1, in.Dividends)
	if err != nil {
		return nil, err
	}
	cmp := xsection.Compare(p, p.First(from), symbols)
	num := func(x float64, places int) any {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		return round(x, places)
	}
	out := struct {
		Columns     []string `json:"columns"`
		Rows        [][]any  `json:"rows"`
		Order       []string `json:"correlationOrder"`
		Correlation [][]any  `json:"correlation"`
		Warnings    []string `json:"warnings,omitempty"`
	}{Columns: []string{"symbol", "from", "to", "totalReturn", "cagr", "volatility", "maxDrawdown", "sharpe", "skew", "bestDay", "worstDay", "beta", "correlation"},
		Rows: [][]any{}, Correlation: [][]any{}, Warnings: info.Warnings}
	for _, st := range cmp.Stats {
		m := st.Metrics
		out.Rows = append(out.Rows, []any{st.Symbol, date(st.From), date(st.To), round(m.TotalReturn, 2), round(m.CAGR, 2), round(st.Volatility, 2),
			round(m.MaxDrawdown, 2), round(m.Sharpe, 3), round(m.Skew, 2), round(st.BestDay, 2), round(st.WorstDay, 2), num(st.Beta, 3), num(st.Correlation, 3)})
		out.Order = append(out.Order, st.Symbol)
	}
	for _, r := range cmp.Correlation {
		row := make([]any, len(r))
		for k, x := range r {
			row[k] = num(x, 3)
		}
		out.Correlation = append(out.Correlation, row)
	}
	return out, nil
}

// dedupe normalises symbols and drops repeats, keeping the first order.
func dedupe(symbols []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range symbols {
		s = store.NormalizeSymbol(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
