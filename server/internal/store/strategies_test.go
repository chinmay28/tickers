package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStrategiesRoundTrip(t *testing.T) {
	s := newTestStore(t)
	def := json.RawMessage(`{ "symbol": "AAPL",  "entry": {"conditions": []} }`)
	st, err := s.CreateStrategy("  Golden cross ", def)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if st.Name != "Golden cross" || string(st.Definition) != `{"symbol":"AAPL","entry":{"conditions":[]}}` {
		t.Errorf("saved = %q %s, want the name trimmed and the JSON compacted", st.Name, st.Definition)
	}
	if _, err := s.CreateStrategy("Another", json.RawMessage(`{"symbol":"MSFT"}`)); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Strategies()
	if len(all) != 2 || all[0].Name != "Another" {
		t.Errorf("list = %+v, want two, alphabetical", all)
	}
	up, err := s.UpdateStrategy(st.ID, "Death cross", json.RawMessage(`{"symbol":"SPY"}`))
	if err != nil || up.Name != "Death cross" || string(up.Definition) != `{"symbol":"SPY"}` {
		t.Errorf("update = %+v, %v", up, err)
	}
	if err := s.DeleteStrategy(st.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteStrategy(st.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting twice gave %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateStrategy("nope", "x", json.RawMessage(`{}`)); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating an unknown strategy gave %v", err)
	}
}

func TestStrategiesRefuseWhatCannotBeSaved(t *testing.T) {
	s := newTestStore(t)
	cases := map[string]struct {
		name string
		def  string
	}{
		"is required":      {"", `{}`},
		"cannot be":        {strings.Repeat("x", 81), `{}`},
		"a JSON object":    {"x", `[1,2]`},
		"a JSON object ":   {"x", `not json`},
		"cannot be larger": {"x", `{"a":"` + strings.Repeat("x", 17<<10) + `"}`},
	}
	for want, c := range cases {
		if _, err := s.CreateStrategy(c.name, json.RawMessage(c.def)); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%q: err = %v", want, err)
		}
	}
}
