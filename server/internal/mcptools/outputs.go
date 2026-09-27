package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

func (t *tools) registerOutputs(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "save_report",
		Title: "Write up a finding",
		Description: "Save a research report for the person running the app, who reads it on the Strategies page. Write it for someone " +
			"deciding whether to act: the question, what was tested (tools, symbols, windows, how many variants — research_log has the " +
			"count), what held up out of sample and what didn't, the deflated Sharpe, the caveats, and what to watch next. Markdown.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"title":{"type":"string","maxLength":120},
			"body":{"type":"string","description":"Markdown, up to 64 KB."},
			"author":{"type":"string","description":"Who wrote it — the agent's name or task."}
		},"required":["title","body"],"additionalProperties":false}`),
		Handler: handler(t.saveReport),
	})
	s.AddTool(mcp.Tool{
		Name:        "list_reports",
		Title:       "Reports so far",
		Description: "The research reports saved so far, newest first — to build on earlier work instead of redoing it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"full":{"type":"boolean","description":"Include each report's whole body; otherwise its first 300 characters."}
		},"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.listReports),
	})
	s.AddTool(mcp.Tool{
		Name:  "watch_strategy",
		Title: "Start a forward test",
		Description: "Freeze a strategy's rules and start judging them on bars that don't exist yet: from tomorrow on, the strategy runs " +
			"on each new bar as it arrives, and forward_tests (and the app's Strategies page) shows how it has done since and what its " +
			"rules say to do at the next open. The only test a search can't have overfitted. Any window in the definition is replaced. " +
			"Watch the few strategies worth it, not every variant — at most 50.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","maxLength":80},
			"definition":` + strategySchema + `,
			"note":{"type":"string","description":"Why it's worth watching: what it was found by, and its backtest."}
		},"required":["name","definition"],"additionalProperties":false}`),
		Handler: handler(t.watch),
	})
	s.AddTool(mcp.Tool{
		Name:  "forward_tests",
		Title: "How the watched strategies are doing",
		Description: "Every watched strategy's forward test: its return and buy-and-hold's since it was frozen, trades, whether it holds a " +
			"position now, and next — what its rules say to do at the next open (enter, exit, or nothing). A strategy that looked " +
			"brilliant in its backtest and is flat or behind here is the usual story; say so rather than explaining it away.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ReadOnly:    true,
		Handler:     handler(t.forward),
	})
}

type reportArgs struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Author string `json:"author"`
}

func (t *tools) saveReport(_ context.Context, in reportArgs) (any, error) {
	r, err := t.home.CreateReport(in.Title, in.Body, in.Author)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": r.ID, "title": r.Title, "createdAt": r.CreatedAt}, nil
}

func (t *tools) listReports(_ context.Context, in struct {
	Full bool `json:"full"`
}) (any, error) {
	all, err := t.home.Reports()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if r := []rune(all[i].Body); !in.Full && len(r) > 300 {
			all[i].Body = strings.TrimSpace(string(r[:300])) + "…"
		}
	}
	return map[string]any{"reports": all}, nil
}

type watchArgs struct {
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
	Note       string          `json:"note"`
}

func (t *tools) watch(_ context.Context, in watchArgs) (any, error) {
	w, err := t.home.WatchStrategy(in.Name, in.Definition, in.Note)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": w.ID, "name": w.Name, "since": w.Since,
		"note": "the forward test counts bars from " + w.Since + "; forward_tests shows it once they arrive"}, nil
}

// forwardView is one watch's forward test as the tools and the app show it.
type forwardView struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Symbol     string       `json:"symbol"`
	Since      string       `json:"since"`
	Note       string       `json:"note,omitempty"`
	Status     string       `json:"status"`
	Bars       int          `json:"bars,omitempty"`
	Strategy   *metricsView `json:"strategy,omitempty"`
	BuyAndHold *metricsView `json:"buyAndHold,omitempty"`
	Trades     int          `json:"trades"`
	Holding    bool         `json:"holding"`
	Next       string       `json:"next,omitempty"`
	Error      string       `json:"error,omitempty"`
}

// Forward states.
const (
	forwardWaiting = "waiting"
	forwardRunning = "running"
	forwardFailed  = "failed"
)

// forwardViews is every watch's forward test, shaped for a model.
func forwardViews(home Home) ([]forwardView, error) {
	fw, err := home.Forward()
	if err != nil {
		return nil, err
	}
	out := make([]forwardView, 0, len(fw))
	for _, f := range fw {
		var def strategy.Definition
		json.Unmarshal(f.Watch.Definition, &def)
		v := forwardView{ID: f.Watch.ID, Name: f.Watch.Name, Symbol: def.Symbol, Since: f.Watch.Since, Note: f.Watch.Note}
		switch {
		case f.Waiting:
			v.Status = forwardWaiting
		case f.Err != nil:
			v.Status, v.Error = forwardFailed, explain(f.Err).Error()
		default:
			r := f.Result
			sm, hm := viewMetrics(r.Strategy), viewMetrics(r.Hold)
			v.Status, v.Bars, v.Strategy, v.BuyAndHold = forwardRunning, r.Bars, &sm, &hm
			v.Trades = r.Stats.Trades
			v.Holding = len(r.Trades) > 0 && r.Trades[len(r.Trades)-1].Reason == strategy.ReasonOpen
			v.Next = r.Next
		}
		out = append(out, v)
	}
	return out, nil
}

func (t *tools) forward(context.Context, struct{}) (any, error) {
	views, err := forwardViews(t.home)
	if err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, errors.New("no strategy is being watched; watch_strategy starts a forward test")
	}
	return map[string]any{"watches": views}, nil
}
