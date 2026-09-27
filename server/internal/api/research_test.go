package api

import (
	"net/http"
	"testing"
)

func TestTheResearchEndpoints(t *testing.T) {
	h, _ := newArchiveHarness(t)
	if code, body := h.call(t, http.MethodPost, "/api/reports", map[string]any{"title": "Momentum", "body": "It held up.", "author": "agent"}); code != http.StatusCreated || body["id"] == "" {
		t.Fatalf("posting a report = %d %v, want 201 with an id", code, body)
	}
	if code, body := h.call(t, http.MethodPost, "/api/reports", map[string]any{"title": "", "body": "x"}); code != http.StatusBadRequest {
		t.Errorf("a report without a title = %d %v, want 400", code, body)
	}
	code, watch := h.call(t, http.MethodPost, "/api/watches", map[string]any{"name": "Golden cross", "definition": goldenCross("SPY"), "note": "from the editor"})
	if code != http.StatusCreated || watch["since"] == "" {
		t.Fatalf("watching = %d %v, want 201 with the day it starts", code, watch)
	}
	if code, body := h.call(t, http.MethodPost, "/api/watches", map[string]any{"name": "", "definition": goldenCross("SPY")}); code != http.StatusBadRequest {
		t.Errorf("watching without a name = %d %v, want 400", code, body)
	}
	bad := goldenCross("SPY")
	bad["entry"] = map[string]any{"conditions": []map[string]string{{"left": "sma:0", "op": ">", "right": "close"}}}
	if code, _ := h.call(t, http.MethodPost, "/api/watches", map[string]any{"name": "x", "definition": bad}); code != http.StatusBadRequest {
		t.Errorf("watching rules that can't run = %d, want 400", code)
	}

	code, research := h.call(t, http.MethodGet, "/api/research", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/research = %d", code)
	}
	reports, watches := research["reports"].([]any), research["watches"].([]any)
	if len(reports) != 1 || len(watches) != 1 {
		t.Fatalf("research = %v, want the report and the watch", research)
	}
	w := watches[0].(map[string]any)
	if w["waiting"] != true || w["result"] != nil {
		t.Errorf("a watch made today = %v, want it waiting for its first bar", w)
	}

	id := reports[0].(map[string]any)["id"].(string)
	if code, _ := h.call(t, http.MethodDelete, "/api/reports/"+id, nil); code != http.StatusNoContent {
		t.Errorf("deleting a report = %d, want 204", code)
	}
	if code, _ := h.call(t, http.MethodDelete, "/api/watches/"+watch["id"].(string), nil); code != http.StatusNoContent {
		t.Errorf("stopping a watch = %d, want 204", code)
	}
	if code, _ := h.call(t, http.MethodDelete, "/api/watches/nope", nil); code != http.StatusNotFound {
		t.Errorf("stopping a watch that doesn't exist = %d, want 404", code)
	}
}
