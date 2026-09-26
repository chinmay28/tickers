package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/archiver"
	"github.com/chinmay28/tickers/server/internal/indicators"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Bounds on the archive tools' output.
const (
	defaultRows = 500
	maxRows     = 5000
)

func (t *tools) registerArchive(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "archive_status",
		Title: "Archive status",
		Description: "What the local market-data archive holds: whether it is open, how many symbols it tracks, and for each bar width " +
			"(1d, 1h, 5m, 1m) how many symbols have history, how many are complete back to their listing or the source's horizon, " +
			"and the date range covered. Start here to know what questions the data can answer. The counts are refreshed about once a minute.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ReadOnly:    true,
		Handler:     handler(t.status),
	})
	s.AddTool(mcp.Tool{
		Name:  "search_symbols",
		Title: "Search symbols",
		Description: "Find symbols in the archive's catalog by the start of their ticker or any part of their name. " +
			"Symbols the app itself uses (its watchlist, portfolios) and ones added by hand sort first. " +
			"Use filter \"active\" for symbols being collected.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"query":{"type":"string","description":"Ticker prefix or part of a company name; empty lists everything."},
			"filter":{"type":"string","enum":["","active","priority","failing","retired","excluded","user"],"description":"active: being collected; priority: collected first; failing: last fetch failed; retired: no longer listed."},
			"kind":{"type":"string","enum":["","stock","etf","other"]},
			"limit":{"type":"integer","minimum":1,"maximum":200,"description":"Default 50."},
			"offset":{"type":"integer","minimum":0}
		},"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.search),
	})
	s.AddTool(mcp.Tool{
		Name:  "symbol_info",
		Title: "Symbol coverage",
		Description: "One symbol's detail: name, exchange, first trade date, former tickers, splits, dividend count, and for each bar width " +
			"which sources have collected how far back and how recently, and whether that history is complete. Check this before " +
			"trusting a backtest over a long window.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"}},"required":["symbol"],"additionalProperties":false}`),
		ReadOnly:    true,
		Handler:     handler(t.info),
	})
	s.AddTool(mcp.Tool{
		Name:  "get_bars",
		Title: "Price bars",
		Description: "OHLCV bars for one symbol from the archive, oldest first, as rows. Prices are split-adjusted as of collection " +
			"and not dividend-adjusted; the window's dividends are listed alongside so returns can include them. Intraday bars are " +
			"the regular session (09:30–16:00 New York) unless extended is set; times are UTC bar opens. When the window holds " +
			"more bars than the limit, the most recent ones are returned and truncated is set.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"symbol":{"type":"string"},
			"interval":{"type":"string","enum":["1d","1h","5m","1m"],"description":"Default 1d."},
			"from":{"type":"string","description":"YYYY-MM-DD. Default: a year back for 1d, 30 days for 1h, 5 for 5m, 2 for 1m."},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive. Default today."},
			"extended":{"type":"boolean","description":"Include pre-market and after-hours bars, tagged by session."},
			"limit":{"type":"integer","minimum":1,"maximum":5000,"description":"Default 500."}
		},"required":["symbol"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.bars),
	})
	s.AddTool(mcp.Tool{
		Name:  "get_indicators",
		Title: "Technical indicators",
		Description: "Technical indicators over one symbol's archived bars, as rows of time, close and each indicator line. Each " +
			"indicator is computed from history before the window, so values are settled from the first row. Specs: sma:N, ema:N, " +
			"bb:N:K (Bollinger; lines upper, middle, lower), vwap, rsi:N, macd:F:S:G (lines macd, signal, histogram), " +
			"stoch:K:D (lines k, d), atr:N, obv. Parameters default when omitted (rsi is rsi:14). Up to 12 at once.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"symbol":{"type":"string"},
			"indicators":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":12,"description":"e.g. [\"sma:50\",\"sma:200\",\"rsi:14\"]"},
			"interval":{"type":"string","enum":["1d","1h","5m","1m"]},
			"from":{"type":"string","description":"YYYY-MM-DD"},
			"to":{"type":"string","description":"YYYY-MM-DD, inclusive"},
			"limit":{"type":"integer","minimum":1,"maximum":5000,"description":"Default 500; the most recent rows are kept."}
		},"required":["symbol","indicators"],"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.indicators),
	})
}

type intervalView struct {
	Interval string `json:"interval"`
	Symbols  int    `json:"symbols"`
	Complete int    `json:"complete"`
	Failing  int    `json:"failing"`
	Bars     int64  `json:"bars"`
	Oldest   string `json:"oldest,omitempty"`
	Newest   string `json:"newest,omitempty"`
}

type statusView struct {
	State    string         `json:"state"`
	Error    string         `json:"error,omitempty"`
	Symbols  map[string]int `json:"symbols,omitempty"`
	Sources  []string       `json:"sources,omitempty"`
	Counted  string         `json:"counted,omitempty"`
	Interval []intervalView `json:"intervals"`
	Note     string         `json:"note,omitempty"`
}

func (t *tools) status(_ context.Context, _ struct{}) (any, error) {
	if t.archive == nil {
		return nil, errNoArchive
	}
	st := t.archive.Status(false)
	out := statusView{State: st.State, Error: st.Error, Interval: []intervalView{}}
	if st.State != archiver.StateOpen {
		out.Note = "the archive isn't open, so no tool can read bars until it is"
	}
	if st.Stats == nil {
		if st.State == archiver.StateOpen {
			out.Note = "the archive is open and still being counted; ask again in a minute for the totals"
		}
		return out, nil
	}
	s := st.Stats
	out.Counted = st.StatsAt.UTC().Format(time.RFC3339)
	out.Symbols = map[string]int{"active": s.Active, "priority": s.Priority, "retired": s.Retired, "excluded": s.Excluded}
	for name := range s.Sources {
		out.Sources = append(out.Sources, name)
	}
	slices.Sort(out.Sources)
	for _, i := range s.Intervals {
		v := intervalView{Interval: string(i.Interval), Symbols: i.Started, Complete: i.Complete, Failing: i.Failing, Bars: i.Bars,
			Oldest: date(i.Oldest)}
		if !i.Newest.IsZero() {
			v.Newest = stamp(i.Newest, i.Interval)
		}
		out.Interval = append(out.Interval, v)
	}
	return out, nil
}

type searchArgs struct {
	Query  string `json:"query"`
	Filter string `json:"filter"`
	Kind   string `json:"kind"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

type symbolView struct {
	Symbol     string `json:"symbol"`
	Name       string `json:"name,omitempty"`
	Exchange   string `json:"exchange,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Active     bool   `json:"active"`
	Priority   bool   `json:"priority,omitempty"`
	FirstTrade string `json:"firstTrade,omitempty"`
}

func viewSymbol(s archive.Symbol) symbolView {
	return symbolView{Symbol: s.Symbol, Name: s.Name, Exchange: s.Exchange, Kind: s.Kind, Active: s.Active, Priority: s.Priority, FirstTrade: date(s.FirstTrade)}
}

func (t *tools) search(_ context.Context, in searchArgs) (any, error) {
	var page archive.SymbolPage
	err := t.read(func(a *archive.Archive) (err error) {
		page, err = a.QuerySymbols(archive.SymbolQuery{Text: in.Query, Filter: in.Filter, Kind: in.Kind,
			Offset: max(in.Offset, 0), Limit: clamp(in.Limit, 50, archive.MaxPage)})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := struct {
		Total   int          `json:"total"`
		Symbols []symbolView `json:"symbols"`
	}{Total: page.Total, Symbols: make([]symbolView, len(page.Symbols))}
	for i, s := range page.Symbols {
		out.Symbols[i] = viewSymbol(s)
	}
	return out, nil
}

type symbolArgs struct {
	Symbol string `json:"symbol"`
}

type cursorView struct {
	Source    string `json:"source"`
	Oldest    string `json:"oldest"`
	Newest    string `json:"newest"`
	Complete  bool   `json:"complete"`
	Failures  int    `json:"failures,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

type coverageView struct {
	Interval   string       `json:"interval"`
	Sources    []cursorView `json:"sources"`
	Bars       int64        `json:"bars"`
	FirstMonth string       `json:"firstMonth,omitempty"`
	LastMonth  string       `json:"lastMonth,omitempty"`
	// Months counts months holding any bars — fewer than the span between
	// the first and last says there are holes.
	Months int `json:"months"`
}

type splitView struct {
	Date  string  `json:"date"`
	Ratio string  `json:"ratio"`
	Price float64 `json:"priceFactor"`
}

func (t *tools) info(_ context.Context, in symbolArgs) (any, error) {
	if strings.TrimSpace(in.Symbol) == "" {
		return nil, fmt.Errorf("a symbol is required")
	}
	var cov archive.Coverage
	var splits []quotes.Split
	err := t.read(func(a *archive.Archive) (err error) {
		if cov, err = a.SymbolCoverage(in.Symbol); err != nil {
			return err
		}
		splits, err = a.Splits(in.Symbol)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := struct {
		symbolView
		Excluded  bool           `json:"excluded,omitempty"`
		Aliases   []string       `json:"formerly,omitempty"`
		Splits    []splitView    `json:"splits"`
		Dividends int            `json:"dividends"`
		Intervals []coverageView `json:"intervals"`
	}{symbolView: viewSymbol(cov.Symbol), Excluded: cov.Symbol.Excluded, Splits: []splitView{}, Dividends: cov.Dividends, Intervals: []coverageView{}}
	for _, a := range cov.Aliases {
		out.Aliases = append(out.Aliases, fmt.Sprintf("%s until %s", a.Former, date(a.Until)))
	}
	for _, s := range splits {
		out.Splits = append(out.Splits, splitView{Date: date(s.Time), Ratio: fmt.Sprintf("%g:%g", s.Numerator, s.Denominator), Price: round(s.Ratio(), 6)})
	}
	for _, interval := range quotes.Intervals {
		v := coverageView{Interval: string(interval), Sources: []cursorView{}}
		for _, c := range cov.Cursors {
			if c.Interval == interval {
				v.Sources = append(v.Sources, cursorView{Source: c.Source, Oldest: date(c.Oldest), Newest: stamp(c.Newest, interval),
					Complete: c.Complete, Failures: c.Failures, LastError: c.LastError})
			}
		}
		for _, span := range cov.Timelines[interval] {
			if span.Bars == 0 {
				continue
			}
			if v.FirstMonth == "" {
				v.FirstMonth = span.Month
			}
			v.LastMonth = span.Month
			v.Months++
			v.Bars += span.Bars
		}
		if len(v.Sources) > 0 || v.Bars > 0 {
			out.Intervals = append(out.Intervals, v)
		}
	}
	return out, nil
}

type barsArgs struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	From     string `json:"from"`
	To       string `json:"to"`
	Extended bool   `json:"extended"`
	Limit    int    `json:"limit"`
}

type dividendView struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
}

// seriesView is a window of bars as rows under named columns — the shape a
// model reads most reliably, where parallel arrays ask it to count.
type seriesView struct {
	Symbol    string         `json:"symbol"`
	Interval  string         `json:"interval"`
	Columns   []string       `json:"columns"`
	Rows      [][]any        `json:"rows"`
	Count     int            `json:"count"`
	Truncated bool           `json:"truncated,omitempty"`
	Dividends []dividendView `json:"dividends,omitempty"`
	Note      string         `json:"note,omitempty"`
}

func (t *tools) query(symbol, rawInterval, from, to string, extended bool) (archive.Query, error) {
	symbol = archive.NormalizeSymbol(symbol)
	if symbol == "" {
		return archive.Query{}, fmt.Errorf("a symbol is required")
	}
	interval, err := parseInterval(rawInterval)
	if err != nil {
		return archive.Query{}, err
	}
	start, end, err := t.window(interval, from, to)
	if err != nil {
		return archive.Query{}, err
	}
	return archive.Query{Symbol: symbol, Interval: interval, From: start, To: end, Extended: extended}, nil
}

// emptyNote is what an empty read says, since an empty window is usually a
// symbol not collected yet rather than a market that didn't trade.
func emptyNote(q archive.Query) string {
	return fmt.Sprintf("the archive holds no %s bars for %s in that window; symbol_info shows what it has collected", q.Interval, q.Symbol)
}

func (t *tools) bars(_ context.Context, in barsArgs) (any, error) {
	q, err := t.query(in.Symbol, in.Interval, in.From, in.To, in.Extended)
	if err != nil {
		return nil, err
	}
	var bars []quotes.Candle
	var dividends []quotes.Dividend
	err = t.read(func(a *archive.Archive) (err error) {
		if bars, err = a.Best(q); err != nil {
			return err
		}
		dividends, err = a.Dividends(q.Symbol, q.From, q.To)
		return err
	})
	if err != nil {
		return nil, err
	}
	limit := clamp(in.Limit, defaultRows, maxRows)
	out := seriesView{Symbol: q.Symbol, Interval: string(q.Interval), Columns: []string{"time", "open", "high", "low", "close", "volume"}, Rows: [][]any{}, Count: len(bars)}
	if in.Extended {
		out.Columns = append(out.Columns, "session")
	}
	if len(bars) > limit {
		out.Truncated = true
		out.Note = fmt.Sprintf("the window holds %d bars; the most recent %d are shown — narrow the window or raise the limit for the rest", len(bars), limit)
		bars = bars[len(bars)-limit:]
	}
	for _, b := range bars {
		row := []any{stamp(b.Time, q.Interval), round(b.Open, 4), round(b.High, 4), round(b.Low, 4), round(b.Close, 4), b.Volume}
		if in.Extended {
			row = append(row, b.Session.String())
		}
		out.Rows = append(out.Rows, row)
	}
	for _, d := range dividends {
		out.Dividends = append(out.Dividends, dividendView{Date: date(d.Time), Amount: round(d.Amount, 6)})
	}
	if out.Count == 0 {
		out.Note = emptyNote(q)
	}
	return out, nil
}

type indicatorArgs struct {
	Symbol     string   `json:"symbol"`
	Indicators []string `json:"indicators"`
	Interval   string   `json:"interval"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Limit      int      `json:"limit"`
}

func (t *tools) indicators(_ context.Context, in indicatorArgs) (any, error) {
	q, err := t.query(in.Symbol, in.Interval, in.From, in.To, false)
	if err != nil {
		return nil, err
	}
	if len(in.Indicators) == 0 {
		return nil, fmt.Errorf("name at least one indicator, like sma:50 or rsi:14")
	}
	specs, err := indicators.ParseList(strings.Join(in.Indicators, ","))
	if err != nil {
		return nil, err
	}
	if t.archive == nil {
		return nil, errNoArchive
	}
	chart, err := t.engine.Chart(q, specs)
	if err != nil {
		return nil, err
	}
	out := seriesView{Symbol: q.Symbol, Interval: string(q.Interval), Columns: []string{"time", "close"}, Rows: [][]any{}, Count: len(chart.Bars)}
	var lines [][]*float64
	for _, r := range chart.Indicators {
		for _, l := range r.Lines {
			name := r.Key
			if len(r.Lines) > 1 {
				name += "." + lineName(l.Name)
			}
			out.Columns = append(out.Columns, name)
			lines = append(lines, l.Values)
		}
	}
	first := 0
	if limit := clamp(in.Limit, defaultRows, maxRows); len(chart.Bars) > limit {
		first = len(chart.Bars) - limit
		out.Truncated = true
		out.Note = fmt.Sprintf("the window holds %d bars; the most recent %d are shown", len(chart.Bars), limit)
	}
	for i := first; i < len(chart.Bars); i++ {
		b := chart.Bars[i]
		row := []any{stamp(b.Time, q.Interval), round(b.Close, 4)}
		for _, l := range lines {
			if i < len(l) && l[i] != nil {
				row = append(row, round(*l[i], 4))
			} else {
				row = append(row, nil)
			}
		}
		out.Rows = append(out.Rows, row)
	}
	if out.Count == 0 {
		out.Note = emptyNote(q)
	}
	return out, nil
}

// lineName is an indicator line as the rule language spells it, so a column
// read here can be pasted into a condition: "%K" is "k".
func lineName(name string) string {
	return strings.ToLower(strings.TrimPrefix(name, "%"))
}
