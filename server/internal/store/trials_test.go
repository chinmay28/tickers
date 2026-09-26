package store

import (
	"encoding/json"
	"math"
	"testing"
)

func TestTheRecordCountsDistinctTrialsPerFamily(t *testing.T) {
	st := newTestStore(t)
	trial := func(def string, sharpe float64) Trial {
		return Trial{Symbol: "spy", Interval: "1d", Origin: "run_backtest", Definition: json.RawMessage(def), SharpePerBar: sharpe, Periods: 100}
	}
	if err := st.RecordTrials([]Trial{trial(`{"a": 1}`, 0.1), trial(`{"a": 2}`, 0.3)}); err != nil {
		t.Fatal(err)
	}
	// The same rules again, spelt with different whitespace: a rerun.
	if err := st.RecordTrials([]Trial{trial(`{"a":1}`, 0.2)}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordTrials([]Trial{{Symbol: "QQQ", Interval: "1d", Definition: json.RawMessage(`{"a": 1}`)}}); err != nil {
		t.Fatal(err)
	}
	f, err := st.Family("SPY", "1d")
	if err != nil {
		t.Fatal(err)
	}
	if f.Trials != 2 || f.BestSharpe != 0.3 {
		t.Errorf("family = %+v, want two distinct trials — a rerun is not another", f)
	}
	// Sample variance of 0.2 (the rerun's new number) and 0.3.
	if math.Abs(f.SharpeVariance-0.005) > 1e-12 {
		t.Errorf("variance = %v, want 0.005", f.SharpeVariance)
	}
	trials, err := st.Trials("SPY", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 2 {
		t.Fatalf("trials = %+v, want SPY's two", trials)
	}
	for _, tr := range trials {
		if string(tr.Definition) == `{"a":1}` && tr.Runs != 2 {
			t.Errorf("the rerun trial has %d runs, want 2", tr.Runs)
		}
	}
	fams, err := st.Families()
	if err != nil || len(fams) != 2 {
		t.Errorf("families = %+v (%v), want SPY and QQQ", fams, err)
	}
	if one, _ := st.Family("QQQ", "1d"); one.Trials != 1 || one.SharpeVariance != 0 {
		t.Errorf("a family of one = %+v, want no variance", one)
	}
	if err := st.RecordTrials([]Trial{{Symbol: "SPY"}}); err == nil {
		t.Error("a trial without a definition was recorded")
	}
}
