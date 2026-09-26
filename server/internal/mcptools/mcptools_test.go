package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/archiver"
	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// stubProvider prices nothing and has no history: the tools under test
// read the archive, and the one that doesn't must say what's missing.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Fetch(context.Context, []string) (map[string]quotes.Quote, map[string]error) {
	return nil, nil
}
func (stubProvider) Search(context.Context, string) ([]quotes.Match, error) {
	return nil, quotes.ErrNoSearch
}

type emptyArchivist struct{}

func (emptyArchivist) Candles(context.Context, string, quotes.Interval, time.Time, time.Time) (quotes.CandleSeries, error) {
	return quotes.CandleSeries{}, nil
}

type harness struct {
	server  *mcp.Server
	store   *store.Store
	archive *archiver.Manager
	days    []time.Time
}

// newHarness builds the tools over a real store and engine. With an
// archive, it holds 400 daily bars of VTI — a slow sine wave on a rising
// trend, with one dividend — and 300 of GLD.
func newHarness(t *testing.T, withArchive bool) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/tools.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, stubProvider{}, publish.New(), nil)
	h := &harness{store: st}
	if !withArchive {
		h.server = New(Options{Store: st, Engine: eng})
		return h
	}

	root := t.TempDir()
	if err := archive.Init(root); err != nil {
		t.Fatal(err)
	}
	paused, listed := true, false
	st.UpdateArchiveConfig(store.ArchivePatch{Paused: &paused, Listed: &listed})
	m := archiver.New(archiver.Options{Store: st, Yahoo: emptyArchivist{}, Symbols: eng.Symbols, FallbackPath: root})
	eng.UseArchive(m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for m.Status(false).State != archiver.StateOpen {
		if time.Now().After(deadline) {
			t.Fatalf("the archive never opened: %+v", m.Status(false))
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.archive = m

	var closes []float64
	for i := 0; i < 400; i++ {
		closes = append(closes, 100+10*math.Sin(float64(i)/15)+float64(i)/20)
	}
	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	for i := range closes {
		h.days = append(h.days, end.AddDate(0, 0, i-len(closes)+1))
	}
	h.record(t, "VTI", h.days, closes, []quotes.Dividend{{Time: h.days[300], Amount: 0.5}})
	h.record(t, "GLD", h.days[100:], closes[100:], nil)
	h.server = New(Options{Store: st, Engine: eng, Archive: m})
	return h
}

func (h *harness) record(t *testing.T, symbol string, days []time.Time, closes []float64, dividends []quotes.Dividend) {
	t.Helper()
	err := h.archive.Read(func(a *archive.Archive) error {
		if err := a.Add(archive.User, archive.Entry{Symbol: symbol, Name: symbol + " Fund", Kind: archive.KindETF}, time.Now()); err != nil {
			return err
		}
		s, err := a.Lookup(symbol)
		if err != nil {
			return err
		}
		candles := make([]quotes.Candle, len(closes))
		for i, c := range closes {
			o := c
			if i > 0 {
				o = closes[i-1]
			}
			candles[i] = quotes.Candle{Time: days[i], Open: o, High: max(o, c) + 0.123456789, Low: min(o, c) - 0.5, Close: c, Volume: 1000}
		}
		cur := archive.Cursor{Oldest: days[0].AddDate(-1, 0, 0), Newest: time.Now(), Complete: true}
		return a.Record(archive.Batch{SymbolID: s.ID, Interval: quotes.Daily, Source: "yahoo",
			Series: quotes.CandleSeries{Candles: candles, Dividends: dividends}, Cursor: &cur})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// call runs a tool through the protocol, as a client would, and returns its
// decoded result and whether it was an error. An error's text comes back
// as the "error" key.
func (h *harness) call(t *testing.T, name string, args any) (map[string]any, bool) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
		Error *struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(h.server.Handle(context.Background(), raw), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("%s: protocol error %s", name, resp.Error.Message)
	}
	text := resp.Result.Content[0].Text
	if resp.Result.IsError {
		return map[string]any{"error": text}, true
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: result %s is not a JSON object", name, text)
	}
	return out, false
}

// mustCall is call for a tool expected to succeed.
func (h *harness) mustCall(t *testing.T, name string, args any) map[string]any {
	t.Helper()
	out, isErr := h.call(t, name, args)
	if isErr {
		t.Fatalf("%s(%v) failed: %s", name, args, out["error"])
	}
	return out
}

func (h *harness) day(i int) string { return h.days[i].Format(time.DateOnly) }

func golden(from string) map[string]any {
	return map[string]any{"symbol": "VTI", "from": from,
		"entry": map[string]any{"conditions": []map[string]string{{"left": "ema:5", "op": "crosses_above", "right": "sma:30"}}},
		"exit":  map[string]any{"conditions": []map[string]string{{"left": "ema:5", "op": "crosses_below", "right": "sma:30"}}}}
}

func TestEveryToolIsListedWithAStrictSchema(t *testing.T) {
	h := newHarness(t, false)
	var resp struct {
		Result struct {
			Tools []struct {
				Name        string
				Description string
				InputSchema map[string]any
			}
		}
	}
	json.Unmarshal(h.server.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &resp)
	want := []string{"archive_status", "search_symbols", "symbol_info", "get_bars", "get_indicators",
		"query_sql", "run_backtest", "sweep_strategy", "find_signals", "backtest_portfolio", "list_strategies", "save_strategy", "research_log"}
	if len(resp.Result.Tools) != len(want) {
		t.Fatalf("listed %d tools, want %d", len(resp.Result.Tools), len(want))
	}
	for i, tool := range resp.Result.Tools {
		if tool.Name != want[i] || tool.Description == "" {
			t.Errorf("tool %d = %q, want %q with a description", i, tool.Name, want[i])
		}
		// A misspelt argument must be refused, not ignored, and the schema
		// should say so to the model before it tries.
		if tool.InputSchema["additionalProperties"] != false {
			t.Errorf("%s's schema allows arguments it doesn't take", tool.Name)
		}
	}
	read := h.server.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"`+languageURI+`"}}`))
	if !strings.Contains(string(read), "crosses_above") {
		t.Error("the rule language document doesn't describe crosses")
	}
}

func TestWithoutAnArchiveTheToolsSaySo(t *testing.T) {
	h := newHarness(t, false)
	for name, args := range map[string]any{
		"archive_status": map[string]any{},
		"get_bars":       map[string]any{"symbol": "VTI"},
		"get_indicators": map[string]any{"symbol": "VTI", "indicators": []string{"rsi"}},
		"run_backtest":   map[string]any{"strategy": golden("2024-01-01")},
		"find_signals":   map[string]any{"symbols": []string{"VTI"}, "from": "2024-01-01", "signal": map[string]any{"conditions": []map[string]string{{"left": "rsi", "op": "<", "right": "30"}}}},
	} {
		out, isErr := h.call(t, name, args)
		if !isErr || !strings.Contains(out["error"].(string), "archive isn't open") {
			t.Errorf("%s without an archive = %v, want an error saying the archive isn't open", name, out)
		}
	}
	if out := h.mustCall(t, "list_strategies", map[string]any{}); len(out["strategies"].([]any)) != 0 {
		t.Errorf("strategies = %v, want none — and no archive needed to list them", out)
	}
}

func TestArchiveTools(t *testing.T) {
	h := newHarness(t, true)

	if st := h.mustCall(t, "archive_status", map[string]any{}); st["state"] != archiver.StateOpen {
		t.Errorf("status = %v, want open", st)
	}
	found := h.mustCall(t, "search_symbols", map[string]any{"query": "VT"})
	if found["total"].(float64) != 1 || found["symbols"].([]any)[0].(map[string]any)["symbol"] != "VTI" {
		t.Errorf("search VT = %v, want VTI alone", found)
	}
	if out, isErr := h.call(t, "search_symbols", map[string]any{"filter": "sideways"}); !isErr {
		t.Errorf("an unknown filter = %v, want an error", out)
	}

	info := h.mustCall(t, "symbol_info", map[string]any{"symbol": "vti"})
	intervals := info["intervals"].([]any)
	if info["symbol"] != "VTI" || info["dividends"].(float64) != 1 || len(intervals) != 1 {
		t.Fatalf("info = %v, want VTI's one dividend and its daily coverage alone", info)
	}
	daily := intervals[0].(map[string]any)
	if daily["bars"].(float64) != 400 || daily["sources"].([]any)[0].(map[string]any)["complete"] != true {
		t.Errorf("daily coverage = %v, want 400 bars from a complete source", daily)
	}
	if out, isErr := h.call(t, "symbol_info", map[string]any{"symbol": "NOPE"}); !isErr || !strings.Contains(out["error"].(string), "search_symbols") {
		t.Errorf("an unknown symbol = %v, want an error pointing at search_symbols", out)
	}

	bars := h.mustCall(t, "get_bars", map[string]any{"symbol": "VTI", "from": h.day(290), "to": h.day(309), "limit": 5})
	rows := bars["rows"].([]any)
	if bars["count"].(float64) != 20 || bars["truncated"] != true || len(rows) != 5 {
		t.Fatalf("bars = %v, want 20 in the window, the last 5 shown and truncation said", bars)
	}
	last := rows[4].([]any)
	if last[0] != h.day(309) {
		t.Errorf("the last row is %v, want the window's last day %s — a truncated read keeps the most recent", last[0], h.day(309))
	}
	if high := last[2].(float64); high != math.Round(high*1e4)/1e4 {
		t.Errorf("high = %v, want it rounded to 4 places", high)
	}
	if divs := bars["dividends"].([]any); len(divs) != 1 || divs[0].(map[string]any)["date"] != h.day(300) {
		t.Errorf("dividends = %v, want the one in the window", divs)
	}
	if out, isErr := h.call(t, "get_bars", map[string]any{"symbol": "VTI", "interval": "3d"}); !isErr {
		t.Errorf("an unknown interval = %v, want an error", out)
	}
	if out, isErr := h.call(t, "get_bars", map[string]any{"symbol": "VTI", "form": "2024-01-01"}); !isErr || !strings.Contains(out["error"].(string), `"form"`) {
		t.Errorf("a misspelt argument = %v, want it named", out)
	}
	empty := h.mustCall(t, "get_bars", map[string]any{"symbol": "GLD", "from": h.day(0), "to": h.day(50)})
	if empty["count"].(float64) != 0 || !strings.Contains(empty["note"].(string), "symbol_info") {
		t.Errorf("an empty window = %v, want a note saying where to look", empty)
	}

	ind := h.mustCall(t, "get_indicators", map[string]any{"symbol": "VTI", "indicators": []string{"sma:50", "bb", "stoch"}, "from": h.day(300)})
	cols := fmt.Sprint(ind["columns"])
	if cols != "[time close sma:50 bb:20:2.upper bb:20:2.middle bb:20:2.lower stoch:14:3.k stoch:14:3.d]" {
		t.Errorf("columns = %s, want each line named as a rule would spell it", cols)
	}
	for i, v := range ind["rows"].([]any)[0].([]any) {
		if v == nil {
			t.Errorf("column %d is empty on the first row — the warm-up should have settled it", i)
		}
	}
	q := h.mustCall(t, "query_sql", map[string]any{"sql": "SELECT symbol, count(*) AS n, avg(close) AS mean FROM bars GROUP BY symbol ORDER BY symbol"})
	if fmt.Sprint(q["columns"]) != "[symbol n mean]" || len(q["rows"].([]any)) != 2 {
		t.Fatalf("query_sql = %v, want a row per symbol", q)
	}
	if row := q["rows"].([]any)[1].([]any); row[0] != "VTI" || row[1].(float64) != 400 || row[2].(float64) != math.Round(row[2].(float64)*1e6)/1e6 {
		t.Errorf("VTI's row = %v, want 400 bars and a mean rounded to 6 places", row)
	}
	if out, isErr := h.call(t, "query_sql", map[string]any{"sql": "DELETE FROM symbols"}); !isErr || !strings.Contains(out["error"].(string), "SELECT") {
		t.Errorf("a write = %v, want it refused", out)
	}
	if out, isErr := h.call(t, "get_indicators", map[string]any{"symbol": "VTI", "indicators": []string{"sma:0"}}); !isErr {
		t.Errorf("an invalid indicator = %v, want an error", out)
	}
}

func TestBacktestAndSweep(t *testing.T) {
	h := newHarness(t, true)

	res := h.mustCall(t, "run_backtest", map[string]any{"strategy": golden(h.day(100)), "trades": 2, "equityPoints": 10})
	stats := res["stats"].(map[string]any)
	if stats["trades"].(float64) < 3 {
		t.Fatalf("stats = %v, want a sine wave crossed several times", stats)
	}
	if len(res["trades"].([]any)) != 2 || len(res["equity"].([]any)) != 10 {
		t.Errorf("trades %d, equity %d: want the 2 and 10 asked for", len(res["trades"].([]any)), len(res["equity"].([]any)))
	}
	if res["from"] != h.day(100) || res["buyAndHold"] == nil {
		t.Errorf("result = %v, want it to start on the day asked and compare with buy-and-hold", res)
	}
	bad := golden(h.day(100))
	bad["entry"] = map[string]any{"conditions": []map[string]string{{"left": "sma:0", "op": ">", "right": "close"}}}
	if out, isErr := h.call(t, "run_backtest", map[string]any{"strategy": bad}); !isErr || !strings.Contains(out["error"].(string), "entry condition 1") {
		t.Errorf("an invalid rule = %v, want an error naming it", out)
	}
	missing := golden(h.day(100))
	missing["symbol"] = "NEWCO"
	if out, isErr := h.call(t, "run_backtest", map[string]any{"strategy": missing}); !isErr || !strings.Contains(out["error"].(string), "has been added") {
		t.Errorf("an uncollected symbol = %v, want it queued and said so", out)
	}

	template := golden(h.day(50))
	template["entry"] = map[string]any{"conditions": []map[string]string{{"left": "ema:{fast}", "op": "crosses_above", "right": "sma:{slow}"}}}
	template["exit"] = map[string]any{"conditions": []map[string]string{{"left": "ema:{fast}", "op": "crosses_below", "right": "sma:{slow}"}}}
	sweep := h.mustCall(t, "sweep_strategy", map[string]any{"template": template, "params": map[string][]float64{"fast": {3, 5, 8}, "slow": {20, 30}},
		"rankBy": "totalReturn", "top": 3, "holdoutFrom": h.day(300)})
	top := sweep["top"].([]any)
	if sweep["variants"].(float64) != 6 || len(top) != 3 {
		t.Fatalf("sweep = %v, want 6 variants and the 3 leaders", sweep)
	}
	prev := math.Inf(1)
	for i, r := range top {
		row := r.(map[string]any)
		score := row["score"].(float64)
		if row["rank"].(float64) != float64(i+1) || score > prev {
			t.Errorf("row %d = %v, want ranks in order of falling score", i, row)
		}
		prev = score
		hold, _ := row["holdout"].(map[string]any)
		if hold == nil || hold["strategy"] == nil || hold["error"] != nil {
			t.Errorf("row %d's holdout = %v, want the leader retested after %s", i, hold, h.day(300))
		}
	}
	if !strings.HasPrefix(sweep["holdoutWindow"].(string), h.day(300)) || !strings.HasSuffix(sweep["window"].(string), h.day(299)) {
		t.Errorf("windows %v / %v, want ranking to end the day before the holdout starts", sweep["window"], sweep["holdoutWindow"])
	}
	// The sweep's top row, rerun alone over the ranking window, is the same
	// backtest.
	best := top[0].(map[string]any)
	p := best["params"].(map[string]any)
	alone := golden(h.day(50))
	alone["to"] = h.day(299)
	alone["entry"] = map[string]any{"conditions": []map[string]string{{"left": fmt.Sprintf("ema:%v", p["fast"]), "op": "crosses_above", "right": fmt.Sprintf("sma:%v", p["slow"])}}}
	alone["exit"] = map[string]any{"conditions": []map[string]string{{"left": fmt.Sprintf("ema:%v", p["fast"]), "op": "crosses_below", "right": fmt.Sprintf("sma:%v", p["slow"])}}}
	rerun := h.mustCall(t, "run_backtest", map[string]any{"strategy": alone})
	if rerun["strategy"].(map[string]any)["totalReturn"] != best["strategy"].(map[string]any)["totalReturn"] {
		t.Errorf("the leader rerun alone returned %v, the sweep said %v", rerun["strategy"], best["strategy"])
	}

	early := map[string]any{}
	for k, v := range template {
		early[k] = v
	}
	early["from"] = "2000-01-01"
	notes := h.mustCall(t, "sweep_strategy", map[string]any{"template": early, "params": map[string][]float64{"fast": {3, 5}, "slow": {20}}})["notes"]
	if n := fmt.Sprint(notes); strings.Count(n, "bars start on") != 1 {
		t.Errorf("notes = %v, want the data's late start said once, not once per variant", notes)
	}

	if out, isErr := h.call(t, "sweep_strategy", map[string]any{"template": template, "params": map[string][]float64{"fast": {3}}}); !isErr || !strings.Contains(out["error"].(string), "{slow}") {
		t.Errorf("a placeholder with no values = %v, want it named", out)
	}
	if out, isErr := h.call(t, "sweep_strategy", map[string]any{"template": template, "params": map[string][]float64{"fast": {3}, "slow": {20}}, "rankBy": "luck"}); !isErr {
		t.Errorf("an unknown ranking = %v, want an error", out)
	}
	template["symbol"] = "NEWCO"
	if out, isErr := h.call(t, "sweep_strategy", map[string]any{"template": template, "params": map[string][]float64{"fast": {3, 5}, "slow": {20}}}); !isErr || !strings.Contains(out["error"].(string), "NEWCO") {
		t.Errorf("a sweep where every variant fails = %v, want the reason", out)
	}
}

func TestFindSignals(t *testing.T) {
	h := newHarness(t, true)
	signal := map[string]any{"conditions": []map[string]string{{"left": "close", "op": "crosses_below", "right": "sma:20"}}}
	out := h.mustCall(t, "find_signals", map[string]any{"symbols": []string{"VTI", "gld", "NOPE", "VTI"}, "signal": signal,
		"from": h.day(150), "horizons": []int{10, 1}, "occurrences": 3})
	if fmt.Sprint(out["horizons"]) != "[1 10]" {
		t.Errorf("horizons = %v, want them sorted", out["horizons"])
	}
	symbols := out["symbols"].([]any)
	if len(symbols) != 2 {
		t.Fatalf("symbols = %v, want VTI and GLD once each", symbols)
	}
	errs := out["errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["symbol"] != "NOPE" {
		t.Errorf("errors = %v, want NOPE alone", errs)
	}
	pooled := out["pooled"].([]any)
	var sum float64
	for _, s := range symbols {
		sum += s.(map[string]any)["horizons"].([]any)[0].(map[string]any)["count"].(float64)
	}
	if pooled[0].(map[string]any)["count"].(float64) != sum || sum == 0 {
		t.Errorf("pooled count = %v, want the symbols' %v added", pooled[0], sum)
	}
	occ := out["recentOccurrences"].([]any)
	if len(occ) != 3 {
		t.Fatalf("occurrences = %v, want the 3 asked for", occ)
	}
	if a, b := occ[0].(map[string]any)["time"].(string), occ[2].(map[string]any)["time"].(string); a < b {
		t.Errorf("occurrences run %s … %s, want the most recent first", a, b)
	}
	if r := occ[len(occ)-1].(map[string]any)["returns"].([]any); len(r) != 2 || r[0] == nil {
		t.Errorf("occurrence = %v, want its returns in the order of horizons", occ[len(occ)-1])
	}

	bad := map[string]any{"conditions": []map[string]string{{"left": "close", "op": "near", "right": "sma:20"}}}
	if out, isErr := h.call(t, "find_signals", map[string]any{"symbols": []string{"VTI", "GLD"}, "signal": bad, "from": h.day(150)}); !isErr || strings.Count(out["error"].(string), "unknown comparison") != 1 {
		t.Errorf("an invalid signal = %v, want one error, not one per symbol", out)
	}
}

func TestSavedStrategies(t *testing.T) {
	h := newHarness(t, false)
	saved := h.mustCall(t, "save_strategy", map[string]any{"name": "Agent: EMA cross", "definition": golden("2020-01-01")})
	id, _ := saved["id"].(string)
	if id == "" {
		t.Fatalf("save = %v, want an id", saved)
	}
	def := golden("2021-01-01")
	h.mustCall(t, "save_strategy", map[string]any{"id": id, "name": "Agent: EMA cross, later", "definition": def})
	list := h.mustCall(t, "list_strategies", map[string]any{})["strategies"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["definition"].(map[string]any)["from"] != "2021-01-01" {
		t.Errorf("strategies = %v, want the one, replaced", list)
	}
	if out, isErr := h.call(t, "save_strategy", map[string]any{"id": "nope", "name": "x", "definition": def}); !isErr || !strings.Contains(out["error"].(string), "list_strategies") {
		t.Errorf("replacing a strategy that doesn't exist = %v, want an error pointing at list_strategies", out)
	}
	def["entry"] = map[string]any{"conditions": []any{}}
	if out, isErr := h.call(t, "save_strategy", map[string]any{"name": "x", "definition": def}); !isErr || !strings.Contains(out["error"].(string), "entry condition") {
		t.Errorf("saving rules that can't run = %v, want an error", out)
	}
}

func TestPortfolioBacktestNeedsAHistorian(t *testing.T) {
	h := newHarness(t, false)
	holdings := []map[string]any{{"symbol": "VTI", "weight": 60}, {"symbol": "BND", "weight": 40}}
	if out, isErr := h.call(t, "backtest_portfolio", map[string]any{"holdings": holdings, "rebalance": "weekly"}); !isErr || !strings.Contains(out["error"].(string), "rebalance") {
		t.Errorf("an unknown cadence = %v, want it named", out)
	}
	if out, isErr := h.call(t, "backtest_portfolio", map[string]any{"holdings": holdings}); !isErr {
		t.Errorf("a provider with no history = %v, want an error", out)
	}
}

func TestSampleSpreadsEvenlyAndKeepsTheLast(t *testing.T) {
	for _, tc := range []struct {
		total, n int
		want     string
	}{
		{10, 1, "[9]"}, {10, 3, "[0 4 9]"}, {3, 5, "[0 1 2]"}, {5, 5, "[0 1 2 3 4]"},
	} {
		if got := fmt.Sprint(sample(tc.total, tc.n)); got != tc.want {
			t.Errorf("sample(%d, %d) = %s, want %s", tc.total, tc.n, got, tc.want)
		}
	}
}

func TestTheResearchLogDeflatesByWhatWasTried(t *testing.T) {
	h := newHarness(t, true)
	first := h.mustCall(t, "run_backtest", map[string]any{"strategy": golden(h.day(100)), "trades": 0})
	of := first["overfitting"].(map[string]any)
	if of["trials"].(float64) != 1 || of["luckBenchmark"].(float64) != 0 || of["deflatedSharpe"] == nil {
		t.Fatalf("a first trial = %v, want one trial, no luck to discount and a probability", of)
	}
	alone := of["deflatedSharpe"].(float64)
	// Running it again is not trying something new.
	again := h.mustCall(t, "run_backtest", map[string]any{"strategy": golden(h.day(100))})["overfitting"].(map[string]any)
	if again["trials"].(float64) != 1 {
		t.Errorf("a rerun = %v, want it still one trial", again)
	}

	template := golden(h.day(100))
	template["entry"] = map[string]any{"conditions": []map[string]string{{"left": "ema:{fast}", "op": "crosses_above", "right": "sma:{slow}"}}}
	template["exit"] = map[string]any{"conditions": []map[string]string{{"left": "ema:{fast}", "op": "crosses_below", "right": "sma:{slow}"}}}
	sweep := h.mustCall(t, "sweep_strategy", map[string]any{"template": template, "params": map[string][]float64{"fast": {2, 3, 4, 6, 8}, "slow": {15, 20, 25, 30}}})
	top := sweep["top"].([]any)[0].(map[string]any)["overfitting"].(map[string]any)
	// None of the sweep's 20 is the first strategy (ema:5 over sma:30), so
	// all are new trials on top of it.
	if top["trials"].(float64) != 21 {
		t.Errorf("the sweep's leader = %v, want it judged against all 21 trials", top)
	}
	if top["luckBenchmark"].(float64) <= 0 {
		t.Errorf("after 21 trials the luck benchmark is %v, want above zero", top["luckBenchmark"])
	}
	// A sine wave is too easy for any of these to look like luck, so the
	// probability may round to 1 either way; the bar it is measured
	// against is what must have risen (strategy's deflation tests pin the
	// probability falling).
	after := h.mustCall(t, "run_backtest", map[string]any{"strategy": golden(h.day(100))})["overfitting"].(map[string]any)
	if after["deflatedSharpe"].(float64) > alone || after["luckBenchmark"].(float64) <= 0 {
		t.Errorf("after the sweep the first strategy = %v, before %v — trying more must count against it", after, alone)
	}

	log := h.mustCall(t, "research_log", map[string]any{"symbol": "vti", "limit": 3})
	fams := log["families"].([]any)
	if len(fams) != 1 || fams[0].(map[string]any)["trials"].(float64) != 21 {
		t.Errorf("families = %v, want VTI's 21", fams)
	}
	recent := log["recentTrials"].([]any)
	if len(recent) != 3 || recent[0].(map[string]any)["definition"] == nil {
		t.Errorf("recent trials = %v, want the 3 asked for, with their definitions", recent)
	}
	if other := h.mustCall(t, "research_log", map[string]any{"symbol": "GLD"}); len(other["families"].([]any)) != 0 {
		t.Errorf("GLD's log = %v, want nothing tried on it", other)
	}
}
