package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// WatchStrategy freezes a strategy's rules to be judged on the bars that
// come after now: its window is set to start tomorrow and run on, so no bar
// it is judged on existed when the rules were chosen. Everything else about
// the rules is kept as given.
func (e *Engine) WatchStrategy(name string, raw json.RawMessage, note string, now time.Time) (store.Watch, error) {
	var def strategy.Definition
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return store.Watch{}, &strategy.InvalidError{}
	}
	since := now.UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	def.From, def.To = since, ""
	// Compiled as of the first day it will run, so a window starting
	// tomorrow isn't refused for starting after today.
	if _, err := strategy.Compile(def, now.AddDate(0, 0, 2)); err != nil {
		return store.Watch{}, err
	}
	frozen, _ := json.Marshal(def)
	return e.store.CreateWatch(name, frozen, since, note)
}

// Forward is one watched strategy's forward test.
type Forward struct {
	Watch store.Watch
	// Result is the test from the watch's first day to now; nil while
	// Waiting or on Err.
	Result *strategy.Result
	// Waiting says no bar has arrived since the watch began: the test
	// starts as they do.
	Waiting bool
	Err     error
}

// Forward runs every watched strategy from the day after it was frozen to
// now, in one batch. It is recomputed each time rather than kept: the rules
// and the date are the whole of it, and the archive has the bars.
func (e *Engine) Forward() ([]Forward, error) {
	watches, err := e.store.Watches()
	if err != nil {
		return nil, err
	}
	out := make([]Forward, len(watches))
	defs := make([]strategy.Definition, len(watches))
	for i, w := range watches {
		out[i].Watch = w
		out[i].Err = json.Unmarshal(w.Definition, &defs[i])
	}
	runs := e.RunStrategies(defs)
	for i, run := range runs {
		switch {
		case out[i].Err != nil:
		case errors.Is(run.Err, ErrNoBars):
			out[i].Waiting = true
		case strategy.IsInvalid(run.Err) && time.Now().UTC().Format(time.DateOnly) < watches[i].Since:
			// Its first day hasn't come: the window is still in the future.
			out[i].Waiting = true
		case run.Err != nil:
			out[i].Err = run.Err
		default:
			res := run.Result
			out[i].Result = &res
		}
	}
	return out, nil
}
