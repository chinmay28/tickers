// Package engine runs the refresh cycle: fetch every enabled symbol, store the
// readings, publish the snapshot downstream, record what happened.
//
// It is the whole of update_minion_quotes.py's main() — the part cron used to
// run every five minutes — turned into a supervised loop inside a long-lived
// process, with the interval, the watchlist and the destinations all read from
// the database on each pass so the web UI can change them without a restart.
package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// Engine owns the refresh loop.
type Engine struct {
	store     *store.Store
	provider  quotes.Provider
	publisher *publish.Publisher
	log       *slog.Logger

	// cycle serialises runs. A manual refresh from the UI landing on top of a
	// scheduled one would double every history point and publish twice; the
	// mutex makes the second caller wait for the first instead.
	cycle sync.Mutex

	mu      sync.Mutex
	running bool
	nextRun time.Time
	lastRun *store.Run

	// kick asks the loop to run now and then resume its schedule. Buffered by
	// one: several nudges in quick succession collapse into a single run,
	// which is the behaviour you want behind a button somebody can double-tap.
	kick chan struct{}
}

// New builds an engine. A nil logger discards output.
func New(st *store.Store, provider quotes.Provider, publisher *publish.Publisher, log *slog.Logger) *Engine {
	if publisher == nil {
		publisher = publish.New()
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{
		store:     st,
		provider:  provider,
		publisher: publisher,
		log:       log,
		kick:      make(chan struct{}, 1),
	}
}

// Provider exposes the quote source, for the API's symbol-search endpoint.
func (e *Engine) Provider() quotes.Provider { return e.provider }

// ApplyConfig pushes the stored quote-source settings into the provider, so a
// base URL, timeout or user agent edited in the GUI takes effect on the next
// request rather than at the next restart. Providers that can't be
// reconfigured are left alone.
//
// Every path that talks upstream calls this first — the refresh cycle, symbol
// search and the connection test — because "I changed the setting and it did
// nothing" is the failure a settings page must never have.
func (e *Engine) ApplyConfig(cfg store.Config) {
	configurable, ok := e.provider.(quotes.Configurable)
	if !ok {
		return
	}
	configurable.Apply(quotes.Settings{
		BaseURL:   cfg.QuoteBaseURL,
		Timeout:   cfg.QuoteTimeout(),
		UserAgent: cfg.QuoteUserAgent,
	})
}

// ProviderSettings reports the settings the provider is actually using, with
// every default resolved. Returns ok=false for a provider that isn't
// configurable, which is what the UI keys off to hide the fields.
func (e *Engine) ProviderSettings() (quotes.Settings, bool) {
	configurable, ok := e.provider.(quotes.Configurable)
	if !ok {
		return quotes.Settings{}, false
	}
	return configurable.Effective(), true
}

// syncProvider reads the stored config and applies it. Used by the paths that
// don't already have a Config in hand.
func (e *Engine) syncProvider() {
	cfg, err := e.store.Config()
	if err != nil {
		e.log.Warn("could not read config before an upstream call", "error", err)
		return
	}
	e.ApplyConfig(cfg)
}

// CheckProvider fetches one symbol through the current settings and reports
// what happened. It is the Settings page's "Test connection" — the fastest way
// to tell a wrong base URL from a blocked network from a bad symbol.
func (e *Engine) CheckProvider(ctx context.Context, symbol string) (quotes.Quote, error) {
	e.syncProvider()
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		symbol = "VTI"
	}
	found, failures := e.provider.Fetch(ctx, []string{symbol})
	if q, ok := found[symbol]; ok {
		return q, nil
	}
	if err, ok := failures[symbol]; ok && err != nil {
		return quotes.Quote{}, err
	}
	return quotes.Quote{}, fmt.Errorf("no quote returned for %s", symbol)
}

// Status is what the UI shows about the loop itself.
type Status struct {
	Running  bool       `json:"running"`
	NextRun  *time.Time `json:"nextRun"`
	LastRun  *store.Run `json:"lastRun"`
	Provider string     `json:"provider"`
}

// Status reports the loop's current state.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{Running: e.running, LastRun: e.lastRun, Provider: e.provider.Name()}
	if !e.nextRun.IsZero() {
		next := e.nextRun
		s.NextRun = &next
	}
	return s
}

// Kick asks the loop to run a cycle as soon as it can. It never blocks.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Start runs the loop until ctx is cancelled. It blocks, so callers run it in
// a goroutine.
//
// The interval is re-read from the database every pass rather than captured
// once, so changing it in Settings takes effect after the current wait instead
// of at the next restart.
func (e *Engine) Start(ctx context.Context) {
	// One cycle immediately, so a freshly started service has prices before
	// the first interval elapses — otherwise a five-minute poll means five
	// minutes of an empty-looking app after every upgrade.
	if _, err := e.RunCycle(ctx, store.TriggerStartup); err != nil && ctx.Err() == nil {
		e.log.Warn("startup refresh failed", "error", err)
	}

	for {
		interval := e.interval()
		e.setNextRun(time.Now().Add(interval))

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			e.setNextRun(time.Time{})
			return
		case <-timer.C:
			if _, err := e.RunCycle(ctx, store.TriggerSchedule); err != nil && ctx.Err() == nil {
				e.log.Warn("scheduled refresh failed", "error", err)
			}
		case <-e.kick:
			timer.Stop()
			// A kick is the UI's "refresh now" button, which runs the cycle
			// itself and only nudges the loop so the *next* scheduled run is
			// measured from now. Nothing to run here — fall through and reset
			// the timer.
		}
	}
}

func (e *Engine) interval() time.Duration {
	cfg, err := e.store.Config()
	if err != nil {
		e.log.Warn("could not read config; using default interval", "error", err)
		cfg = store.DefaultConfig()
	}
	return cfg.RefreshInterval()
}

func (e *Engine) setNextRun(t time.Time) {
	e.mu.Lock()
	e.nextRun = t
	e.mu.Unlock()
}

// RunCycle performs one refresh, and one publish if configured. It returns the
// recorded run.
//
// A cycle never fails as a whole because a symbol failed: an unpriceable
// symbol is stored as an error quote, published as "N/A" (which is what the
// original script did), and counted. Only something structural — the database
// being unreadable — comes back as an error.
func (e *Engine) RunCycle(ctx context.Context, trigger string) (store.Run, error) {
	e.cycle.Lock()
	defer e.cycle.Unlock()

	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	run := store.Run{StartedAt: time.Now().UTC(), Trigger: trigger, Publishes: []store.PublishResult{}}

	cfg, err := e.store.Config()
	if err != nil {
		return e.finish(run, err)
	}
	// Before anything talks upstream: a base URL, timeout or user agent edited
	// in the GUI has to be in force for this cycle, not the next restart.
	e.ApplyConfig(cfg)

	tickers, err := e.store.EnabledTickers()
	if err != nil {
		return e.finish(run, err)
	}

	if len(tickers) > 0 {
		symbols := make([]string, 0, len(tickers))
		for _, t := range tickers {
			symbols = append(symbols, t.Symbol)
		}

		fetched, failures := e.provider.Fetch(ctx, symbols)
		now := time.Now().UTC()

		for _, t := range tickers {
			q := store.Quote{TickerID: t.ID, Symbol: t.Symbol, FetchedAt: now}
			if got, ok := fetched[t.Symbol]; ok {
				q.Status = store.StatusOK
				q.Price = got.Price
				q.PreviousClose = got.PreviousClose
				q.Currency = got.Currency
				q.ShortName = got.ShortName
				q.MarketState = got.MarketState
				if !got.FetchedAt.IsZero() {
					q.FetchedAt = got.FetchedAt
				}
				run.OKCount++
			} else {
				q.Status = store.StatusError
				if err, ok := failures[t.Symbol]; ok && err != nil {
					q.Error = err.Error()
				} else {
					q.Error = "no quote returned"
				}
				run.ErrorCount++
			}
			if err := e.store.SaveQuote(q); err != nil {
				return e.finish(run, err)
			}
		}
	}

	if _, err := e.store.PruneHistory(cfg.HistoryRetention()); err != nil {
		e.log.Warn("history prune failed", "error", err)
	}

	if cfg.PublishOnRefresh {
		results, err := e.Publish(ctx)
		if err != nil {
			return e.finish(run, err)
		}
		run.Publishes = results
	}

	e.log.Info("refresh complete",
		"trigger", trigger, "ok", run.OKCount, "errors", run.ErrorCount, "publishes", len(run.Publishes))
	return e.finish(run, nil)
}

// Publish sends the current snapshot to every enabled sink, without refetching.
// The API exposes it as "publish now" for testing a freshly added destination.
func (e *Engine) Publish(ctx context.Context) ([]store.PublishResult, error) {
	sinks, err := e.store.EnabledSinks()
	if err != nil {
		return nil, err
	}
	if len(sinks) == 0 {
		return []store.PublishResult{}, nil
	}
	snap, err := e.Snapshot()
	if err != nil {
		return nil, err
	}
	return e.publisher.PublishAll(ctx, sinks, snap), nil
}

// Snapshot assembles the current readings for the enabled watchlist, in
// display order.
//
// Enabled tickers with no stored quote yet are included as error readings, so
// a symbol added seconds ago publishes as "N/A" rather than vanishing from the
// payload — a consumer watching for a key is better served by a key that says
// "unknown" than by a key that intermittently isn't there.
func (e *Engine) Snapshot() (publish.Snapshot, error) {
	tickers, err := e.store.EnabledTickers()
	if err != nil {
		return publish.Snapshot{}, err
	}
	stored, err := e.store.Quotes()
	if err != nil {
		return publish.Snapshot{}, err
	}

	// No re-sorting here: EnabledTickers already returns display order, pinned
	// symbols first, and sorting by position again would drop them back into
	// the middle of the payload.
	snap := publish.Snapshot{At: time.Now(), Quotes: make([]store.Quote, 0, len(tickers))}
	for _, t := range tickers {
		if q, ok := stored[t.ID]; ok {
			snap.Quotes = append(snap.Quotes, q)
			continue
		}
		snap.Quotes = append(snap.Quotes, store.Quote{
			TickerID: t.ID,
			Symbol:   t.Symbol,
			Status:   store.StatusError,
			Error:    "not fetched yet",
		})
	}
	return snap, nil
}

// finish stamps, records and caches a run.
func (e *Engine) finish(run store.Run, cause error) (store.Run, error) {
	run.FinishedAt = time.Now().UTC()
	if cause != nil {
		run.Error = cause.Error()
	}
	saved, err := e.store.AppendRun(run)
	if err != nil {
		// The cycle's own failure is the more useful one to report; losing the
		// audit row is worth a log line, not a replaced error.
		e.log.Error("could not record run", "error", err)
		saved = run
	}

	e.mu.Lock()
	e.lastRun = &saved
	e.mu.Unlock()

	return saved, cause
}
