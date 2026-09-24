package api

import (
	"net/http"
	"strings"
	"testing"
)

func goldenCross(symbol string) map[string]any {
	return map[string]any{
		"symbol": symbol, "interval": "1d", "from": "2020-01-01", "to": "2024-12-31",
		"entry": map[string]any{"match": "all", "conditions": []map[string]string{{"left": "sma:50", "op": "crosses_above", "right": "sma:200"}}},
		"exit":  map[string]any{"match": "any", "conditions": []map[string]string{{"left": "rsi", "op": ">", "right": "70"}}},
	}
}

func TestStrategyCRUD(t *testing.T) {
	h := newHarness(t, stubProvider{})

	code, created := h.call(t, http.MethodPost, "/api/strategies", map[string]any{"name": "Golden cross", "definition": goldenCross("SPY")})
	if code != http.StatusCreated || created["id"] == "" {
		t.Fatalf("creating a valid strategy = %d %v, want 201 with an id", code, created)
	}
	id := created["id"].(string)

	bad := goldenCross("SPY")
	bad["entry"] = map[string]any{"conditions": []map[string]string{{"left": "sma:0", "op": ">", "right": "close"}}}
	code, body := h.call(t, http.MethodPost, "/api/strategies", map[string]any{"name": "Broken", "definition": bad})
	if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "entry condition 1") {
		t.Errorf("saving rules that can't compile = %d %v, want 400 naming the condition", code, body)
	}
	if code, body := h.call(t, http.MethodPost, "/api/strategies", map[string]any{"name": "Extra", "definition": map[string]any{"symbl": "SPY"}}); code != http.StatusBadRequest {
		t.Errorf("saving rules with an unknown field = %d %v, want 400", code, body)
	}
	if code, body := h.call(t, http.MethodPost, "/api/strategies", map[string]any{"name": "", "definition": goldenCross("SPY")}); code != http.StatusBadRequest {
		t.Errorf("saving without a name = %d %v, want 400", code, body)
	}

	code, updated := h.call(t, http.MethodPut, "/api/strategies/"+id, map[string]any{"name": "Golden cross QQQ", "definition": goldenCross("QQQ")})
	if code != http.StatusOK || updated["name"] != "Golden cross QQQ" {
		t.Fatalf("renaming = %d %v, want 200 and the new name", code, updated)
	}
	if code, _ := h.call(t, http.MethodPut, "/api/strategies/nope", map[string]any{"name": "x", "definition": goldenCross("QQQ")}); code != http.StatusNotFound {
		t.Errorf("updating a strategy that doesn't exist = %d, want 404", code)
	}

	_, list := h.call(t, http.MethodGet, "/api/strategies", nil)
	all := list["strategies"].([]any)
	if len(all) != 1 {
		t.Fatalf("listed %d strategies, want the one saved", len(all))
	}
	def := all[0].(map[string]any)["definition"].(map[string]any)
	if def["symbol"] != "QQQ" {
		t.Errorf("the saved rules came back as %v, want the updated ones", def)
	}

	if code, _ := h.call(t, http.MethodDelete, "/api/strategies/"+id, nil); code != http.StatusNoContent {
		t.Errorf("deleting = %d, want 204", code)
	}
	if code, _ := h.call(t, http.MethodDelete, "/api/strategies/"+id, nil); code != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", code)
	}
}

func TestRunStrategyErrors(t *testing.T) {
	h := newHarness(t, stubProvider{})
	if code, body := h.call(t, http.MethodPost, "/api/strategies/run", map[string]any{"definition": goldenCross("SPY")}); code != http.StatusConflict {
		t.Errorf("running without an archive = %d %v, want 409", code, body)
	}

	h, _ = newArchiveHarness(t)
	bad := goldenCross("SPY")
	bad["from"] = "yesterday"
	if code, body := h.call(t, http.MethodPost, "/api/strategies/run", map[string]any{"definition": bad}); code != http.StatusBadRequest {
		t.Errorf("running with a bad date = %d %v, want 400", code, body)
	}
}
