package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/strategy"
)

func TestAWatchIsJudgedOnlyOnWhatCameAfter(t *testing.T) {
	eng, _ := newTestEngine(t, &fakeProvider{})
	a := newTestArchive(t)
	eng.UseArchive(openArchive{a})
	closes := make([]float64, 60)
	for i := range closes {
		closes[i] = 100 + float64(i)
	}
	archiveDaily(t, a, "VTI", closes, nil)

	def := `{"symbol":"VTI","from":"2000-01-01","to":"2001-01-01","entry":{"conditions":[{"left":"close","op":">","right":"0"}]}}`
	now := time.Now()
	w, err := eng.WatchStrategy("Always in", json.RawMessage(def), "", now)
	if err != nil {
		t.Fatal(err)
	}
	var frozen strategy.Definition
	json.Unmarshal(w.Definition, &frozen)
	if frozen.From != now.UTC().AddDate(0, 0, 1).Format(time.DateOnly) || frozen.To != "" || w.Since != frozen.From {
		t.Errorf("frozen = %+v since %s, want it to start tomorrow and run on, whatever window it came with", frozen, w.Since)
	}
	if _, err := eng.WatchStrategy("Broken", json.RawMessage(`{"symbol":"VTI","entry":{"conditions":[{"left":"nope","op":">","right":"1"}]}}`), "", now); !strategy.IsInvalid(err) {
		t.Errorf("watching rules that can't run = %v, want refused", err)
	}

	// Nothing has come after yet: the archive's last bar is yesterday's.
	fw, err := eng.Forward()
	if err != nil || len(fw) != 1 || !fw[0].Waiting {
		t.Fatalf("forward = %+v (%v), want the watch waiting for its first bar", fw, err)
	}

	// Frozen a month ago, it has a month of bars to be judged on.
	past, err := eng.WatchStrategy("A month ago", json.RawMessage(def), "", now.AddDate(0, -1, 0))
	if err != nil {
		t.Fatal(err)
	}
	fw, _ = eng.Forward()
	var got *Forward
	for i := range fw {
		if fw[i].Watch.ID == past.ID {
			got = &fw[i]
		}
	}
	if got == nil || got.Result == nil {
		t.Fatalf("forward = %+v, want the month-old watch tested", fw)
	}
	if got.Result.From.Before(now.AddDate(0, -1, 0)) || got.Result.Bars > 31 {
		t.Errorf("tested from %s over %d bars, want only the month since it was frozen", got.Result.From, got.Result.Bars)
	}
	if !strings.Contains(string(got.Watch.Definition), `"to":""`) {
		t.Errorf("definition = %s, want no end", got.Watch.Definition)
	}
}
