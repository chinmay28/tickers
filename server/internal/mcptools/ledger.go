package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// confident is the deflated Sharpe a result has to reach before the tools
// stop warning that it could be luck.
const confident = 0.95

func (t *tools) registerLedger(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:  "research_log",
		Title: "What has been tried",
		Description: "The record of every strategy backtested through these tools (run_backtest, and each variant of sweep_strategy), " +
			"one entry per distinct definition. Families are the trials on one symbol at one interval: the trials that competed over " +
			"the same data, which is what each result's deflatedSharpe is discounted by. Read it before a new search, to build on what " +
			"was tried rather than repeat it — and to know how much a new winner has to beat.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"symbol":{"type":"string","description":"Only this symbol's trials."},
			"interval":{"type":"string","enum":["1d","1h","5m","1m"]},
			"limit":{"type":"integer","minimum":0,"maximum":200,"description":"How many recent trials to list with their definitions; default 20."}
		},"additionalProperties":false}`),
		ReadOnly: true,
		Handler:  handler(t.researchLog),
	})
}

// overfitView is how far a result can be trusted, given everything tried
// on the same series.
type overfitView struct {
	// Trials is how many distinct strategies have been tested on this
	// symbol at this interval, this one included.
	Trials int `json:"trials"`
	// DeflatedSharpe is the probability the strategy's true Sharpe ratio is
	// above zero, after allowing for Trials and for fat tails.
	DeflatedSharpe *float64 `json:"deflatedSharpe"`
	// LuckBenchmark is the annualised Sharpe ratio the best of Trials
	// strategies with no edge would be expected to show.
	LuckBenchmark float64 `json:"luckBenchmark"`
	Note          string  `json:"note,omitempty"`
}

// judge records results as trials and says of each how far it can be
// trusted. defs and results are aligned; a nil result is one that didn't
// run and is neither recorded nor judged. A failure to record costs the
// judgement, not the result: it comes back as a warning.
func (t *tools) judge(origin string, defs []strategy.Definition, results []*strategy.Result) ([]*overfitView, string) {
	var trials []store.Trial
	for i, r := range results {
		if r == nil {
			continue
		}
		def := defs[i]
		def.Symbol, def.Interval = r.Symbol, r.Interval
		raw, _ := json.Marshal(def)
		trials = append(trials, store.Trial{Symbol: r.Symbol, Interval: r.Interval, Origin: origin, Definition: raw,
			SharpePerBar: r.Strategy.SharpePerBar, Periods: r.Strategy.Periods, TotalReturn: r.Strategy.TotalReturn, Trades: r.Stats.Trades})
	}
	out := make([]*overfitView, len(results))
	if err := t.store.RecordTrials(trials); err != nil {
		return out, "these results could not be added to the research log, so they are not deflated: " + err.Error()
	}
	families := map[[2]string]store.TrialFamily{}
	for i, r := range results {
		if r == nil {
			continue
		}
		key := [2]string{r.Symbol, r.Interval}
		f, ok := families[key]
		if !ok {
			var err error
			if f, err = t.store.Family(r.Symbol, r.Interval); err != nil {
				return out, "the research log could not be read, so these results are not deflated: " + err.Error()
			}
			families[key] = f
		}
		out[i] = deflate(*r, f)
	}
	return out, ""
}

func deflate(r strategy.Result, f store.TrialFamily) *overfitView {
	m := r.Strategy
	v := &overfitView{Trials: f.Trials}
	perYear := 252.0
	if i, err := quotes.ParseInterval(r.Interval); err == nil {
		perYear = strategy.BarsPerYear(i)
	}
	v.LuckBenchmark = round(strategy.ExpectedMaxSharpe(f.Trials, f.SharpeVariance)*math.Sqrt(perYear), 3)
	if p := strategy.DeflatedSharpe(m.SharpePerBar, m.Periods, m.Skew, m.Kurtosis, f.Trials, f.SharpeVariance); !math.IsNaN(p) {
		p = round(p, 3)
		v.DeflatedSharpe = &p
		if p < confident {
			v.Note = fmt.Sprintf("after %d trials on this series, a result this good is not clearly more than luck (%.0f%% < %.0f%%)", f.Trials, p*100, confident*100)
		}
	}
	return v
}

type logArgs struct {
	Symbol   string `json:"symbol"`
	Interval string `json:"interval"`
	Limit    *int   `json:"limit"`
}

type trialView struct {
	Symbol      string          `json:"symbol"`
	Interval    string          `json:"interval"`
	Origin      string          `json:"origin"`
	Sharpe      float64         `json:"sharpe"`
	TotalReturn float64         `json:"totalReturn"`
	Trades      int             `json:"trades"`
	Runs        int             `json:"runs"`
	LastRun     string          `json:"lastRun"`
	Definition  json.RawMessage `json:"definition"`
}

type familyView struct {
	Symbol        string  `json:"symbol"`
	Interval      string  `json:"interval"`
	Trials        int     `json:"trials"`
	BestSharpe    float64 `json:"bestSharpe"`
	LuckBenchmark float64 `json:"luckBenchmark"`
	LastRun       string  `json:"lastRun"`
}

func annualise(perBar float64, interval string) float64 {
	i, err := quotes.ParseInterval(interval)
	if err != nil {
		return perBar
	}
	return perBar * math.Sqrt(strategy.BarsPerYear(i))
}

func (t *tools) researchLog(_ context.Context, in logArgs) (any, error) {
	limit := 20
	if in.Limit != nil {
		limit = min(max(*in.Limit, 0), 200)
	}
	fams, err := t.store.Families()
	if err != nil {
		return nil, err
	}
	out := struct {
		Families []familyView `json:"families"`
		Trials   []trialView  `json:"recentTrials"`
	}{Families: []familyView{}, Trials: []trialView{}}
	symbol := store.NormalizeSymbol(in.Symbol)
	for _, f := range fams {
		if (symbol != "" && f.Symbol != symbol) || (in.Interval != "" && f.Interval != in.Interval) {
			continue
		}
		out.Families = append(out.Families, familyView{Symbol: f.Symbol, Interval: f.Interval, Trials: f.Trials,
			BestSharpe:    round(annualise(f.BestSharpe, f.Interval), 3),
			LuckBenchmark: round(annualise(strategy.ExpectedMaxSharpe(f.Trials, f.SharpeVariance), f.Interval), 3),
			LastRun:       date(f.LastAt)})
	}
	if limit > 0 {
		trials, err := t.store.Trials(symbol, in.Interval, limit)
		if err != nil {
			return nil, err
		}
		for _, tr := range trials {
			out.Trials = append(out.Trials, trialView{Symbol: tr.Symbol, Interval: tr.Interval, Origin: tr.Origin,
				Sharpe: round(annualise(tr.SharpePerBar, tr.Interval), 3), TotalReturn: round(tr.TotalReturn, 2), Trades: tr.Trades,
				Runs: tr.Runs, LastRun: date(tr.LastAt), Definition: tr.Definition})
		}
	}
	return out, nil
}
