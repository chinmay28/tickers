package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// historyStub is a stubProvider that also has a past, so the backtest
// endpoints have something to simulate.
type historyStub struct {
	stubProvider
	bars map[string][]quotes.Bar
}

func (h historyStub) History(_ context.Context, symbol string, _ time.Time) ([]quotes.Bar, error) {
	bars, ok := h.bars[symbol]
	if !ok {
		return nil, quotes.ErrNotFound
	}
	return bars, nil
}

func threeMonths(first, second, third float64) []quotes.Bar {
	return []quotes.Bar{
		{Date: "2020-01-31", Close: first},
		{Date: "2020-02-28", Close: second},
		{Date: "2020-03-31", Close: third},
	}
}

func backtestHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, historyStub{
		stubProvider: stubProvider{prices: map[string]float64{"VTI": 300, "BND": 70}},
		bars: map[string][]quotes.Bar{
			"VTI": threeMonths(100, 110, 121),
			"BND": threeMonths(100, 90, 81),
		},
	})
}

func twoFund() map[string]any {
	return map[string]any{
		"name": "Two fund",
		"holdings": []map[string]any{
			{"symbol": "VTI", "weight": 60},
			{"symbol": "BND", "weight": 40},
		},
		"initialAmount": 10000,
		"rebalance":     "annually",
	}
}

func TestPortfolioCRUDAndItsPlaceInState(t *testing.T) {
	h := backtestHarness(t)

	rec, body := h.do(t, http.MethodPost, "/api/portfolios", twoFund())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, want 201: %v", rec.Code, body)
	}
	created, _ := body["portfolio"].(map[string]any)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %v", body)
	}

	// The client renders from /api/state alone, so a saved portfolio that isn't
	// in it doesn't exist as far as the page is concerned.
	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	portfolios, _ := state["portfolios"].([]any)
	if len(portfolios) != 1 {
		t.Fatalf("state carries %d portfolios, want 1", len(portfolios))
	}

	rec, body = h.do(t, http.MethodPatch, "/api/portfolios/"+id, map[string]any{"name": "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: status %d: %v", rec.Code, body)
	}
	updated, _ := body["portfolio"].(map[string]any)
	if updated["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", updated["name"])
	}

	if rec, _ := h.do(t, http.MethodDelete, "/api/portfolios/"+id, nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete: status %d, want 204", rec.Code)
	}
	if rec, _ := h.do(t, http.MethodDelete, "/api/portfolios/"+id, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleting twice: status %d, want 404", rec.Code)
	}
}

func TestPortfolioValidationIsABadRequestNotAServerFault(t *testing.T) {
	h := backtestHarness(t)

	bad := twoFund()
	bad["holdings"] = []map[string]any{{"symbol": "VTI", "weight": 60}}

	rec, body := h.do(t, http.MethodPost, "/api/portfolios", bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("weights summing to 60 gave status %d, want 400: %v", rec.Code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "100") {
		t.Errorf("error %q does not say what is wrong with the weights", message)
	}
}

func TestBacktestRunsAnUnsavedAllocation(t *testing.T) {
	h := backtestHarness(t)

	// No portfolio saved: this is the editor asking "would this even run?".
	rec, body := h.do(t, http.MethodPost, "/api/backtest", twoFund())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %v", rec.Code, body)
	}

	result, _ := body["backtest"].(map[string]any)
	if result == nil {
		t.Fatalf("no backtest in %v", body)
	}
	if result["start"] != "2020-01" || result["end"] != "2020-03" {
		t.Errorf("ran %v → %v, want 2020-01 → 2020-03", result["start"], result["end"])
	}
	points, _ := result["points"].([]any)
	if len(points) != 3 {
		t.Errorf("got %d monthly points, want 3", len(points))
	}
	portfolio, _ := result["portfolio"].(map[string]any)
	// 6,000 up 21% and 4,000 down 19%, with no December to rebalance at.
	if end, _ := portfolio["end"].(float64); end < 10500 || end > 10510 {
		t.Errorf("ended at %v, want about 10,500", portfolio["end"])
	}
}

func TestBacktestRunsASavedPortfolio(t *testing.T) {
	h := backtestHarness(t)
	_, body := h.do(t, http.MethodPost, "/api/portfolios", twoFund())
	created, _ := body["portfolio"].(map[string]any)
	id, _ := created["id"].(string)

	rec, body := h.do(t, http.MethodPost, "/api/portfolios/"+id+"/backtest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %v", rec.Code, body)
	}
	if body["backtest"] == nil {
		t.Errorf("no backtest for a saved portfolio: %v", body)
	}

	rec, _ = h.do(t, http.MethodPost, "/api/portfolios/missing/backtest", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("backtesting a portfolio that isn't there: status %d, want 404", rec.Code)
	}
}

func TestBacktestBlamesTheSpecForASymbolTheSourceDoesNotKnow(t *testing.T) {
	h := backtestHarness(t)

	spec := twoFund()
	spec["holdings"] = []map[string]any{
		{"symbol": "VTI", "weight": 50},
		{"symbol": "NOSUCH", "weight": 50},
	}

	rec, body := h.do(t, http.MethodPost, "/api/backtest", spec)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — a symbol nobody has heard of is a typo in the form, "+
			"not a server fault: %v", rec.Code, body)
	}
	if message, _ := body["error"].(string); !strings.Contains(message, "NOSUCH") {
		t.Errorf("error %q does not name the symbol", message)
	}
}

func TestBacktestIsUnavailableRatherThanBrokenWithoutHistory(t *testing.T) {
	// A provider that can only price today is a configuration somebody chose.
	h := newHarness(t, stubProvider{prices: map[string]float64{"VTI": 300}})

	rec, body := h.do(t, http.MethodPost, "/api/backtest", twoFund())
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d, want 501: %v", rec.Code, body)
	}
}

func TestPortfolioRejectsFieldsItDoesNotHave(t *testing.T) {
	h := backtestHarness(t)

	rec := h.raw(t, http.MethodPost, "/api/portfolios",
		`{"name":"Typo","holdings":[{"symbol":"VTI","weight":100}],"rebalence":"annually"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a misspelled field gave status %d, want 400 — silently saving the default "+
			"instead is how a setting appears not to work", rec.Code)
	}
}

func TestSavingAPortfolioPutsItOnTheWatchlist(t *testing.T) {
	h := backtestHarness(t)

	rec, body := h.do(t, http.MethodPost, "/api/portfolios", twoFund())
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d: %v", rec.Code, body)
	}
	created, _ := body["portfolio"].(map[string]any)
	id, _ := created["id"].(string)

	_, state := h.do(t, http.MethodGet, "/api/state", nil)
	tickers, _ := state["tickers"].([]any)
	var row map[string]any
	for _, entry := range tickers {
		candidate, _ := entry.(map[string]any)
		if candidate["portfolioId"] == id {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("no watchlist row for the saved portfolio")
	}
	// The key a downstream dashboard reads the value under.
	if row["symbol"] != "TWO-FUND" {
		t.Errorf("row symbol = %v, want TWO-FUND", row["symbol"])
	}
	if row["expression"] != "" {
		t.Errorf("the row carries a formula %q; a portfolio is not a composite", row["expression"])
	}

	// And it publishes with everything else — which is most of the point of
	// putting it on the watchlist at all.
	_, preview := h.do(t, http.MethodGet, "/api/preview", nil)
	payload, _ := preview["payload"].(map[string]any)
	if _, ok := payload["TWO-FUND"]; !ok {
		t.Errorf("the portfolio is missing from the published payload: %v", payload)
	}

	// Renaming moves the key rather than adding a second row.
	if rec, body := h.do(t, http.MethodPatch, "/api/portfolios/"+id,
		map[string]any{"name": "Renamed fund"}); rec.Code != http.StatusOK {
		t.Fatalf("rename: status %d: %v", rec.Code, body)
	}
	_, state = h.do(t, http.MethodGet, "/api/state", nil)
	tickers, _ = state["tickers"].([]any)
	rows := 0
	for _, entry := range tickers {
		candidate, _ := entry.(map[string]any)
		if candidate["portfolioId"] == id {
			rows++
			if candidate["symbol"] != "RENAMED-FUND" {
				t.Errorf("row symbol = %v after rename, want RENAMED-FUND", candidate["symbol"])
			}
		}
	}
	if rows != 1 {
		t.Errorf("found %d rows for one portfolio, want 1", rows)
	}

	// Deleting the portfolio takes the row with it.
	if rec, _ := h.do(t, http.MethodDelete, "/api/portfolios/"+id, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete failed")
	}
	_, state = h.do(t, http.MethodGet, "/api/state", nil)
	tickers, _ = state["tickers"].([]any)
	for _, entry := range tickers {
		candidate, _ := entry.(map[string]any)
		if candidate["portfolioId"] == id {
			t.Error("the watchlist row outlived its portfolio")
		}
	}
}

func TestAPortfolioNameThatCollidesWithASymbolIsRejected(t *testing.T) {
	h := backtestHarness(t)

	// VTI is on the seeded watchlist. Two rows cannot share a published key,
	// and the portfolio must not be left saved but invisible.
	clashing := twoFund()
	clashing["name"] = "VTI"

	rec, body := h.do(t, http.MethodPost, "/api/portfolios", clashing)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %v", rec.Code, body)
	}
	_, listed := h.do(t, http.MethodGet, "/api/portfolios", nil)
	if portfolios, _ := listed["portfolios"].([]any); len(portfolios) != 0 {
		t.Errorf("a portfolio whose row could not be created was left saved: %v", portfolios)
	}
}

func TestRenamingAPortfolioKeepsTheRowsBaseline(t *testing.T) {
	h := backtestHarness(t)

	_, body := h.do(t, http.MethodPost, "/api/portfolios", twoFund())
	created, _ := body["portfolio"].(map[string]any)
	id, _ := created["id"].(string)

	unitsOf := func(p map[string]any) []float64 {
		holdings, _ := p["holdings"].([]any)
		out := make([]float64, 0, len(holdings))
		for _, entry := range holdings {
			h, _ := entry.(map[string]any)
			units, _ := h["units"].(float64)
			out = append(out, units)
		}
		return out
	}
	before := unitsOf(created)
	if len(before) != 2 || before[0] == 0 {
		t.Fatalf("the portfolio was saved without units: %v", before)
	}

	// The editor posts the whole allocation on every save, and posts it without
	// units — so a rename arrives looking exactly like an allocation edit. If
	// that re-priced, the watchlist row would drop back to the initial amount
	// and throw away everything it had grown by.
	renamed := twoFund()
	renamed["name"] = "Renamed fund"
	rec, body := h.do(t, http.MethodPatch, "/api/portfolios/"+id, renamed)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status %d: %v", rec.Code, body)
	}
	updated, _ := body["portfolio"].(map[string]any)
	after := unitsOf(updated)
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("units moved from %v to %v across a rename", before, after)
			break
		}
	}

	// Changing a weight is a different allocation, and does re-base.
	reweighted := twoFund()
	reweighted["name"] = "Renamed fund"
	reweighted["holdings"] = []map[string]any{
		{"symbol": "VTI", "weight": 70},
		{"symbol": "BND", "weight": 30},
	}
	rec, body = h.do(t, http.MethodPatch, "/api/portfolios/"+id, reweighted)
	if rec.Code != http.StatusOK {
		t.Fatalf("reweight: status %d: %v", rec.Code, body)
	}
	updated, _ = body["portfolio"].(map[string]any)
	if reweightedUnits := unitsOf(updated); reweightedUnits[0] == before[0] {
		t.Errorf("units %v survived a weight change; a different allocation is different units",
			reweightedUnits)
	}
}
