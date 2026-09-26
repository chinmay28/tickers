package archiver

import (
	"context"
	"sync"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
)

// Fixed is an archive opened read-only at one path for the life of the
// process: what a research server reads, beside the collector that writes
// it or from a copy of it on a bigger machine. There is nothing to manage —
// no settings to follow, no collector, no moving — so it is not a Manager,
// only the two things readers ask a Manager for.
type Fixed struct {
	a *archive.Archive

	mu       sync.Mutex
	stats    *archive.Stats
	size     int64
	statsAt  time.Time
	counting bool
}

// OpenFixed opens the archive at path read-only. It takes no lock, so it
// opens alongside a live collector, and it never writes.
func OpenFixed(path string) (*Fixed, error) {
	a, err := archive.OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	return &Fixed{a: a}, nil
}

// Read runs fn against the archive.
func (f *Fixed) Read(fn func(a *archive.Archive) error) error { return fn(f.a) }

// Close releases the archive's files.
func (f *Fixed) Close() error { return f.a.Close() }

// Status reports the archive as the Manager does: always open, with the last
// count and a recount started in the background once it is stale, so no
// caller waits on seconds of counting.
func (f *Fixed) Status(fresh bool) Status {
	f.mu.Lock()
	st := Status{State: StateOpen, Enabled: true, Path: f.a.Root(), Stats: f.stats, Size: f.size, StatsAt: f.statsAt}
	stale := fresh || f.stats == nil || time.Since(f.statsAt) > statsEvery
	start := stale && !f.counting
	if start {
		f.counting = true
	}
	f.mu.Unlock()
	if start {
		go f.recount()
	}
	return st
}

func (f *Fixed) recount() {
	stats, err := f.a.StatsContext(context.Background())
	size, _ := f.a.Size()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counting = false
	if err == nil {
		f.stats, f.size, f.statsAt = &stats, size, time.Now()
	}
}
