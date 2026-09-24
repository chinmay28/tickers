package collector

import (
	"time"
)

// States the collector reports.
const (
	StateStarting   = "starting"
	StateCollecting = "collecting"
	StateIdle       = "idle"
	StateWaiting    = "waiting"
	StatePaused     = "paused"
	StateStopped    = "stopped"
)

// Status is what the Data page shows about the loop itself.
type Status struct {
	State  string    `json:"state"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
	// Current is the request in flight or last made.
	Current     string    `json:"current"`
	ListsReadAt time.Time `json:"listsReadAt"`
	DiskFree    uint64    `json:"diskFree"`
	DiskTotal   uint64    `json:"diskTotal"`
	// Series counts the schedule: every symbol × interval × source, those
	// still walking back, and those due forward.
	Series      int      `json:"series"`
	Backfilling int      `json:"backfilling"`
	Behind      int      `json:"behind"`
	LastHour    Counters `json:"lastHour"`
	Jobs        []Job    `json:"jobs"`
}

// Counters is a tally of the collector's work.
type Counters struct {
	Requests int `json:"requests"`
	Bars     int `json:"bars"`
	Failures int `json:"failures"`
	// Skipped counts backfill windows the archive already held — work
	// another source had done, costing no request.
	Skipped int `json:"skipped"`
}

// counters keeps a minute-by-minute tally for the last hour.
type counters struct {
	minute [60]int64
	counts [60]Counters
}

func (c *counters) add(now time.Time, requests, bars, failures, skipped int) {
	m := now.Unix() / 60
	i := m % 60
	if c.minute[i] != m {
		c.minute[i], c.counts[i] = m, Counters{}
	}
	c.counts[i].Requests += requests
	c.counts[i].Bars += bars
	c.counts[i].Failures += failures
	c.counts[i].Skipped += skipped
}

func (c *counters) lastHour(now time.Time) Counters {
	var out Counters
	m := now.Unix() / 60
	for i := range c.minute {
		if m-c.minute[i] < 60 {
			out.Requests += c.counts[i].Requests
			out.Bars += c.counts[i].Bars
			out.Failures += c.counts[i].Failures
			out.Skipped += c.counts[i].Skipped
		}
	}
	return out
}

func (c *Collector) setState(state, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.status.State != state || c.status.Reason != reason {
		c.status.State, c.status.Reason, c.status.Since = state, reason, c.now()
	}
}

func (c *Collector) setCurrent(s string) {
	c.mu.Lock()
	c.status.Current = s
	c.mu.Unlock()
}

// Status reports the loop's state. The schedule counts are computed by the
// loop between steps, so they are at most one step stale.
func (c *Collector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Jobs = make([]Job, 0, len(c.jobs))
	for _, j := range c.jobs {
		s.Jobs = append(s.Jobs, *j)
	}
	return s
}

// publishSchedule refreshes the schedule counts Status reports.
func (c *Collector) publishSchedule(now time.Time) {
	backfilling, behind := 0, 0
	for i := range c.targets {
		t := &c.targets[i]
		if t.cursor.Newest.IsZero() || !t.cursor.Complete {
			backfilling++
		} else if forwardDue(t.reach, t.cursor, now) {
			behind++
		}
	}
	c.mu.Lock()
	c.status.Series, c.status.Backfilling, c.status.Behind = len(c.targets), backfilling, behind
	c.status.LastHour = c.counts.lastHour(now)
	c.mu.Unlock()
}
