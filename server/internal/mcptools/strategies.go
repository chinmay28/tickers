package mcptools

import (
	"context"
	"encoding/json"

	"github.com/chinmay28/tickers/server/internal/mcp"
)

func (t *tools) registerStrategies(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "list_strategies",
		Title: "Saved strategies",
		Description: "The strategies saved in the app — by the people using it and by agents — with their definitions, ready to pass " +
			"to run_backtest or to use as a sweep_strategy template.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		ReadOnly:    true,
		Handler:     handler(t.listStrategies),
	})
	s.AddTool(mcp.Tool{
		Name:  "save_strategy",
		Title: "Save a strategy",
		Description: "Save a strategy to the app's Strategies page, where a person can open, chart and rerun it — the way to hand over " +
			"something worth a look. Without an id it is saved as new; with one it replaces that saved strategy. The definition is " +
			"validated as run_backtest would, but not run. Name it for what it does, and say in the name that an agent found it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"id":{"type":"string","description":"A saved strategy's id, to replace it."},
			"name":{"type":"string","maxLength":80},
			"definition":` + strategySchema + `
		},"required":["name","definition"],"additionalProperties":false}`),
		Handler: handler(t.saveStrategy),
	})
}

func (t *tools) listStrategies(_ context.Context, _ struct{}) (any, error) {
	all, err := t.home.Strategies()
	if err != nil {
		return nil, err
	}
	return map[string]any{"strategies": all}, nil
}

type saveArgs struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
}

func (t *tools) saveStrategy(_ context.Context, in saveArgs) (any, error) {
	return t.home.SaveStrategy(in.ID, in.Name, in.Definition)
}
