package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReportsAndWatches(t *testing.T) {
	st := newTestStore(t)
	first, err := st.CreateReport("  Momentum in ETFs ", "## Finding\nIt held up out of sample.", "agent")
	if err != nil || first.Title != "Momentum in ETFs" {
		t.Fatalf("report = %+v (%v), want the title trimmed", first, err)
	}
	for name, args := range map[string][2]string{"no title": {"", "x"}, "no body": {"x", " "}, "too long": {"x", strings.Repeat("a", MaxReportBody+1)}} {
		if _, err := st.CreateReport(args[0], args[1], ""); err == nil {
			t.Errorf("a report with %s was saved", name)
		}
	}
	second, _ := st.CreateReport("Later", "body", "")
	all, err := st.Reports()
	if err != nil || len(all) != 2 || all[0].ID != second.ID {
		t.Errorf("reports = %+v (%v), want both, newest first", all, err)
	}
	if err := st.DeleteReport(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteReport(first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice = %v, want ErrNotFound", err)
	}

	def := json.RawMessage(`{"symbol": "SPY", "from": "2026-09-27"}`)
	w, err := st.CreateWatch("SPY dip", def, "2026-09-27", "found by an agent")
	if err != nil || w.Since != "2026-09-27" || string(w.Definition) != `{"symbol":"SPY","from":"2026-09-27"}` {
		t.Fatalf("watch = %+v (%v)", w, err)
	}
	if _, err := st.CreateWatch("x", def, "tomorrow", ""); err == nil {
		t.Error("a watch without a date was saved")
	}
	if _, err := st.CreateWatch("x", json.RawMessage(`[]`), "2026-09-27", ""); err == nil {
		t.Error("a watch whose rules aren't an object was saved")
	}
	watches, _ := st.Watches()
	if len(watches) != 1 || watches[0].Note != "found by an agent" {
		t.Errorf("watches = %+v, want the one", watches)
	}
	if err := st.DeleteWatch(w.ID); err != nil {
		t.Fatal(err)
	}
}
