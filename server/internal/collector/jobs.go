package collector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
)

// Job states.
const (
	JobQueued  = "queued"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Job is a window somebody asked for by hand: "fetch AAPL's minute bars for
// March from Polygon, and let them replace what's there". It runs ahead of
// the schedule, a span at a time, newest first, under the source's pacer.
//
// Jobs live in memory. A restart forgets a queued one, which costs somebody
// a click; persisting them would cost a table and a migration for a queue
// that is empty almost all the time.
type Job struct {
	ID       int             `json:"id"`
	Symbol   string          `json:"symbol"`
	Interval quotes.Interval `json:"interval"`
	Source   string          `json:"source"`
	From     time.Time       `json:"from"`
	To       time.Time       `json:"to"`
	Replace  bool            `json:"replace"`
	State    string          `json:"state"`
	// Cursor is how far back the job has got; it runs from To toward From.
	Cursor   time.Time `json:"cursor"`
	Requests int       `json:"requests"`
	Bars     int       `json:"bars"`
	Error    string    `json:"error"`
	Created  time.Time `json:"created"`
}

// maxJobs bounds the queue, finished jobs included; the oldest finished ones
// make room.
const maxJobs = 50

// Enqueue queues a job, filling in its ID and state. It validates what it can
// without the loop: the symbol, the interval and the window. Whether the
// source exists and serves the interval is checked when the job runs, against
// the sources configured then.
func (c *Collector) Enqueue(j Job) (Job, error) {
	j.Symbol = archive.NormalizeSymbol(j.Symbol)
	if j.Symbol == "" {
		return j, errors.New("a symbol is required")
	}
	if j.Interval.Step() == 0 {
		return j, fmt.Errorf("unknown interval %q", j.Interval)
	}
	if j.Source == "" {
		return j, errors.New("a source is required")
	}
	if !j.From.Before(j.To) {
		return j, errors.New("the window must start before it ends")
	}
	if _, err := c.archive.Lookup(j.Symbol); err != nil {
		return j, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	active := 0
	for _, q := range c.jobs {
		if q.State == JobQueued || q.State == JobRunning {
			active++
		}
	}
	if active >= maxJobs {
		return j, errors.New("too many fetches queued; wait for some to finish")
	}
	for len(c.jobs) >= maxJobs {
		c.jobs = c.jobs[1:]
	}
	c.jobSeq++
	j.ID, j.State, j.Cursor, j.Created = c.jobSeq, JobQueued, j.To, c.now()
	c.jobs = append(c.jobs, &j)
	c.Nudge()
	return j, nil
}

func (c *Collector) hasJobs() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, j := range c.jobs {
		if j.State == JobQueued || j.State == JobRunning {
			return true
		}
	}
	return false
}

// runnableJob is the oldest unfinished job whose source is ready.
func (c *Collector) runnableJob(ready func(int) bool) *Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, j := range c.jobs {
		if j.State != JobQueued && j.State != JobRunning {
			continue
		}
		si := c.sourceIndex(j.Source)
		if si < 0 {
			j.State, j.Error = JobFailed, fmt.Sprintf("no source named %q is configured", j.Source)
			continue
		}
		if _, ok := c.sources[si].Reach[j.Interval]; !ok {
			j.State, j.Error = JobFailed, fmt.Sprintf("%s doesn't serve %s bars", j.Source, j.Interval)
			continue
		}
		if ready(si) {
			return j
		}
	}
	return nil
}

func (c *Collector) sourceIndex(name string) int {
	for i, s := range c.sources {
		if s.Name == name {
			return i
		}
	}
	return -1
}

// jobStep fetches one span of a job.
func (c *Collector) jobStep(ctx context.Context, j *Job) error {
	c.mu.Lock()
	si := c.sourceIndex(j.Source)
	src := c.sources[si]
	reach := src.Reach[j.Interval]
	job := *j
	j.State = JobRunning
	c.mu.Unlock()

	sym, err := c.archive.Lookup(job.Symbol)
	if err != nil {
		c.finishJob(j, err)
		return nil
	}
	now := c.now()
	to := job.Cursor
	from := job.From
	if reach.Span > 0 && to.Add(-reach.Span).After(from) {
		from = to.Add(-reach.Span)
	}
	if e := edge(reach, now); !e.IsZero() && from.Before(e) {
		from = e
	}
	if !from.Before(to) {
		c.finishJob(j, nil)
		return nil
	}

	c.setState(StateCollecting, "")
	c.setCurrent(fmt.Sprintf("%s %s %s–%s from %s (requested)", job.Symbol, job.Interval,
		from.Format(time.DateOnly), to.Format(time.DateOnly), src.Name))
	series, err := src.Provider.Candles(ctx, job.Symbol, job.Interval, from, to)
	now = c.now()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.pacerFor(src).observe(err, now)
	if errors.Is(err, quotes.ErrRateLimited) {
		c.counts.add(now, 1, 0, 1, 0)
		return nil
	}
	if err != nil {
		c.counts.add(now, 1, 0, 1, 0)
		c.finishJob(j, err)
		return nil
	}
	series.Candles = settled(job.Interval, series.Candles, now)
	if err := c.archive.Record(archive.Batch{
		SymbolID: sym.ID, Interval: job.Interval, Source: src.Name, Replace: job.Replace, Series: series,
	}); err != nil {
		return fmt.Errorf("archive %s %s: %w", job.Symbol, job.Interval, err)
	}
	c.counts.add(now, 1, len(series.Candles), 0, 0)

	c.mu.Lock()
	j.Cursor, j.Requests, j.Bars = from, j.Requests+1, j.Bars+len(series.Candles)
	done := !from.After(job.From) || (reach.Horizon > 0 && !from.After(now.Add(-reach.Horizon)))
	c.mu.Unlock()
	if done {
		c.finishJob(j, nil)
	}
	return nil
}

func (c *Collector) finishJob(j *Job, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		j.State, j.Error = JobFailed, err.Error()
		return
	}
	j.State = JobDone
}
