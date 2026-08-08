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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chinmay28/tickers/server/internal/expr"
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
	// historyCache holds fetched daily series by symbol, for the performance
	// sheet. See performance.go — it is under this mutex because it is read
	// from HTTP handlers while the loop is running.
	historyCache map[string]historyEntry
	// dividendCache is the same, for the payout series a portfolio's yield
	// needs. Separate because it comes from a separate upstream call, and a
	// symbol can perfectly well have one and not the other.
	dividendCache map[string]dividendEntry

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
		store:         st,
		provider:      provider,
		publisher:     publisher,
		log:           log,
		kick:          make(chan struct{}, 1),
		historyCache:  map[string]historyEntry{},
		dividendCache: map[string]dividendEntry{},
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
		plan := newFetchPlan(tickers)
		fetched, failures := e.provider.Fetch(ctx, plan.symbols)
		now := time.Now().UTC()
		prices, closes := readingMaps(fetched)

		for _, t := range tickers {
			var q store.Quote
			if t.IsComposite() {
				q = compositeQuote(t, plan.formulas[t.ID], prices, closes, failures, now)
			} else {
				q = directQuote(t, fetched, failures, now)
			}
			if q.Status == store.StatusOK {
				run.OKCount++
			} else {
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

// ---------------------------------------------------------------------------
// Pricing one cycle's watchlist
// ---------------------------------------------------------------------------

// formula is a composite's parsed expression, or the reason it wouldn't parse.
// A formula that has stopped parsing is reported on its own row rather than
// failing the cycle — the rest of the watchlist is still perfectly priceable.
type formula struct {
	expr *expr.Expr
	err  error
}

// fetchPlan is what one cycle asks the provider for, and how each composite
// gets priced from the answer.
type fetchPlan struct {
	// symbols is every symbol that has to be fetched: the plain rows, plus the
	// components of every composite, deduplicated. A ratio over a symbol that
	// is already on the watchlist therefore costs no extra request, and a
	// component that isn't on the watchlist is fetched without becoming a row.
	symbols []string
	// formulas is keyed by composite ticker ID.
	formulas map[string]formula
}

func newFetchPlan(tickers []store.Ticker) fetchPlan {
	plan := fetchPlan{symbols: []string{}, formulas: map[string]formula{}}
	seen := map[string]bool{}
	add := func(sym string) {
		if sym == "" || seen[sym] {
			return
		}
		seen[sym] = true
		plan.symbols = append(plan.symbols, sym)
	}

	for _, t := range tickers {
		if !t.IsComposite() {
			add(t.Symbol)
			continue
		}
		parsed, err := expr.Parse(t.Expression)
		plan.formulas[t.ID] = formula{expr: parsed, err: err}
		if err != nil {
			continue
		}
		for _, sym := range parsed.Symbols() {
			add(sym)
		}
	}
	return plan
}

// readingMaps splits the fetched quotes into the two symbol → number maps a
// formula is evaluated against: today's prices, and yesterday's closes.
func readingMaps(fetched map[string]quotes.Quote) (prices, closes map[string]float64) {
	prices = make(map[string]float64, len(fetched))
	closes = make(map[string]float64, len(fetched))
	for sym, q := range fetched {
		if q.Price != nil {
			prices[sym] = *q.Price
		}
		if q.PreviousClose != nil {
			closes[sym] = *q.PreviousClose
		}
	}
	return prices, closes
}

// directQuote is the reading for an ordinary symbol: whatever the provider
// said, success or failure.
func directQuote(t store.Ticker, fetched map[string]quotes.Quote, failures map[string]error, now time.Time) store.Quote {
	q := store.Quote{TickerID: t.ID, Symbol: t.Symbol, FetchedAt: now}
	got, ok := fetched[t.Symbol]
	if !ok {
		q.Status = store.StatusError
		if err, ok := failures[t.Symbol]; ok && err != nil {
			q.Error = err.Error()
		} else {
			q.Error = "no quote returned"
		}
		return q
	}
	q.Status = store.StatusOK
	q.Price = got.Price
	q.PreviousClose = got.PreviousClose
	q.Currency = got.Currency
	q.ShortName = got.ShortName
	q.MarketState = got.MarketState
	if !got.FetchedAt.IsZero() {
		q.FetchedAt = got.FetchedAt
	}
	return q
}

// compositeQuote is the reading for a formula row: the same shape as any other
// quote, which is what makes composites free everywhere downstream — history,
// sparklines, change percentages, the published payload.
//
// Currency is deliberately left empty. A ratio is dimensionless, and a sum is
// only meaningful in one currency if every leg shares it, which nothing here
// can promise.
func compositeQuote(t store.Ticker, f formula, prices, closes map[string]float64, failures map[string]error, now time.Time) store.Quote {
	q := store.Quote{
		TickerID: t.ID, Symbol: t.Symbol, FetchedAt: now,
		Status: store.StatusError, Composite: true,
	}
	if f.err != nil {
		q.Error = f.err.Error()
		return q
	}

	price, err := f.expr.Eval(prices)
	if err != nil {
		q.Error = explainEval(err, failures)
		return q
	}
	q.Status = store.StatusOK
	q.Price = &price

	// The previous close is best-effort. Without it the row still shows a
	// price, just no change — which is better than failing the whole composite
	// because one leg's provider didn't report a previous close.
	if prev, err := f.expr.Eval(closes); err == nil {
		q.PreviousClose = &prev
	}
	return q
}

// explainEval swaps "no price for GLD" for the provider's own reason that GLD
// has no price — the difference between someone re-reading a formula that is
// fine and someone seeing the typo in one of its legs.
func explainEval(err error, failures map[string]error) string {
	var missing *expr.MissingError
	if errors.As(err, &missing) {
		if cause, ok := failures[missing.Symbol]; ok && cause != nil {
			reason := cause.Error()
			// Providers vary on whether they name the symbol themselves;
			// "NOPE: NOPE: no data found" helps nobody.
			if strings.Contains(reason, missing.Symbol) {
				return reason
			}
			return missing.Symbol + ": " + reason
		}
	}
	return err.Error()
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
			// Composite-ness lives on the ticker, not on the quote row, so this
			// join is where it gets stamped back on — it decides how many
			// decimals the payload gives the value.
			q.Composite = t.IsComposite()
			snap.Quotes = append(snap.Quotes, q)
			continue
		}
		snap.Quotes = append(snap.Quotes, store.Quote{
			TickerID:  t.ID,
			Symbol:    t.Symbol,
			Status:    store.StatusError,
			Error:     "not fetched yet",
			Composite: t.IsComposite(),
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
