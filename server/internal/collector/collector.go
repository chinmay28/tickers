// Package collector fills the archive: every listed US symbol plus a few
// extras, at every configured interval, forward from now and backward as far
// as the provider will go — at a pace the provider tolerates.
//
// It is a loop around three pure pieces, each tested on its own: next picks
// the most urgent request, advance and fail work out what a response means for
// a series' coverage, and pacer says when the next request may go out. The
// loop itself only moves data between them, the provider and the archive.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/universe"
)

// Lister is where the list of symbols comes from. universe.Source is one; a
// test supplies its own.
type Lister interface {
	Listings(ctx context.Context) ([]universe.Listing, error)
}

// Config is what to collect and how fast.
type Config struct {
	// Intervals to collect, each under its Policy.
	Intervals []quotes.Interval
	// Extras are collected alongside the exchange lists — crypto, indices,
	// anything Yahoo prices that no US exchange lists.
	Extras []string
	// Universe lists the exchange-traded symbols. Nil collects the extras
	// alone.
	Universe Lister
	// Spacing is the gap between two requests to the provider. It is the
	// whole of the rate limit: 2s is 1,800 requests an hour.
	Spacing time.Duration
}

// DefaultSpacing is the default gap between requests.
const DefaultSpacing = 2 * time.Second

// MinSpacing is the floor. Below it the collector is a load test.
const MinSpacing = 250 * time.Millisecond

// universeEvery is how often the exchange lists are re-read. They change by a
// handful of listings a day.
const universeEvery = 24 * time.Hour

// universeRetry is how soon a failed read is retried.
const universeRetry = time.Hour

// maxIdle bounds a sleep with nothing due, so a clock jump or a new symbol
// never leaves the loop asleep for a day.
const maxIdle = time.Hour

// minIdle floors it, so a scheduling edge case costs a minute's nap rather
// than a spinning core.
const minIdle = time.Minute

// Collector is the loop. It is not safe for concurrent use; run one.
type Collector struct {
	archive  *archive.Archive
	provider quotes.Archivist
	cfg      Config
	policies []Policy
	log      *slog.Logger

	// now and sleep are the clock, swapped out in tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	pacer        pacer
	targets      []target
	nextUniverse time.Time

	// Counters for the periodic progress line.
	fetched, failed, bars int
	reportedAt            time.Time
}

// New builds a collector. It fails on a configuration that could never
// collect anything, rather than starting a loop that idles forever.
func New(a *archive.Archive, provider quotes.Archivist, cfg Config, log *slog.Logger) (*Collector, error) {
	if a == nil || provider == nil {
		return nil, errors.New("collector needs an archive and a provider")
	}
	policies, err := Policies(cfg.Intervals)
	if err != nil {
		return nil, err
	}
	if cfg.Universe == nil && len(cfg.Extras) == 0 {
		return nil, errors.New("nothing to collect: no exchange list and no extra symbols")
	}
	if cfg.Spacing == 0 {
		cfg.Spacing = DefaultSpacing
	}
	if cfg.Spacing < MinSpacing {
		return nil, fmt.Errorf("request spacing %s is below the %s floor", cfg.Spacing, MinSpacing)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Collector{
		archive:  a,
		provider: provider,
		cfg:      cfg,
		policies: policies,
		log:      log,
		now:      time.Now,
		sleep:    sleepCtx,
		pacer:    pacer{spacing: cfg.Spacing},
	}, nil
}

// Run collects until ctx is cancelled. It returns nil on cancellation and an
// error only when the archive itself fails — a provider failure is a series'
// problem, recorded against it, and never stops the loop.
func (c *Collector) Run(ctx context.Context) error {
	c.log.Info("archive collector started", "intervals", c.cfg.Intervals, "spacing", c.cfg.Spacing)
	for ctx.Err() == nil {
		if _, err := c.Step(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
	}
	return nil
}

// Step does one unit of work — re-reading the symbol lists if they are due,
// then one request, or a nap when nothing is due — and reports whether it
// made a request.
func (c *Collector) Step(ctx context.Context) (bool, error) {
	now := c.now()
	if c.targets == nil || !now.Before(c.nextUniverse) {
		if err := c.syncUniverse(ctx, now); err != nil {
			return false, err
		}
	}

	now = c.now()
	tk, ok := next(c.targets, now)
	if !ok {
		idle := maxIdle
		if at := wake(c.targets, now); !at.IsZero() {
			idle = at.Sub(now)
		}
		if at := c.nextUniverse.Sub(now); at < idle {
			idle = at
		}
		idle = min(max(idle, minIdle), maxIdle)
		return false, c.sleep(ctx, idle)
	}

	if d := c.pacer.delay(now); d > 0 {
		if c.pacer.paused(now) {
			c.log.Info("archive collector pausing", "for", d.Round(time.Second))
		}
		if err := c.sleep(ctx, d); err != nil {
			return false, err
		}
	}
	return true, c.fetch(ctx, tk)
}

func (c *Collector) fetch(ctx context.Context, tk task) error {
	t := &c.targets[tk.target]
	series, err := c.provider.Candles(ctx, t.symbol, t.policy.Interval, tk.from, tk.to)
	now := c.now()
	if ctx.Err() != nil {
		// Cancelled mid-request: nothing was learned about the symbol.
		return ctx.Err()
	}
	c.pacer.observe(err, now)

	if errors.Is(err, quotes.ErrRateLimited) {
		c.log.Warn("archive collector rate limited", "symbol", t.symbol)
		return nil
	}
	if err != nil {
		c.failed++
		t.coverage = fail(*t, err, now)
		c.log.Debug("archive fetch failed", "symbol", t.symbol, "interval", t.policy.Interval,
			"kind", tk.kind, "failures", t.coverage.Failures, "error", err)
		c.report(now)
		return c.archive.SaveCoverage(t.coverage)
	}

	series.Candles = settled(t.policy.Interval, series.Candles, now)
	cov := advance(*t, tk, series, now)
	if err := c.archive.Record(archive.Batch{Coverage: cov, Series: series}); err != nil {
		return fmt.Errorf("archive %s %s: %w", t.symbol, t.policy.Interval, err)
	}
	t.coverage = cov
	if !series.FirstTrade.IsZero() {
		t.firstTrade = series.FirstTrade
	}
	c.fetched++
	c.bars += len(series.Candles)
	c.log.Debug("archived", "symbol", t.symbol, "interval", t.policy.Interval, "kind", tk.kind,
		"from", tk.from.Format(time.DateOnly), "to", tk.to.Format(time.DateOnly), "bars", len(series.Candles))
	c.report(now)
	return nil
}

// reportEvery is how often progress is logged. Per-request lines are debug:
// at a request every two seconds they would be most of the journal.
const reportEvery = 15 * time.Minute

func (c *Collector) report(now time.Time) {
	if c.reportedAt.IsZero() {
		c.reportedAt = now
		return
	}
	if now.Sub(c.reportedAt) < reportEvery {
		return
	}
	pending := 0
	for i := range c.targets {
		if cov := c.targets[i].coverage; cov.Newest.IsZero() || !cov.Complete {
			pending++
		}
	}
	c.log.Info("archive progress", "fetched", c.fetched, "failed", c.failed, "bars", c.bars,
		"series", len(c.targets), "backfilling", pending)
	c.fetched, c.failed, c.bars = 0, 0, 0
	c.reportedAt = now
}

// syncUniverse re-reads the symbol lists when due and rebuilds the targets.
//
// Retiring is the dangerous half. A symbol missing from a list it was on is
// delisted and should stop being fetched — but a list that comes back short
// for any other reason would retire half the market, so a list less than
// half the size of what is already active is treated as broken, not believed.
func (c *Collector) syncUniverse(ctx context.Context, now time.Time) error {
	if c.nextUniverse.IsZero() {
		synced, err := c.archive.UniverseSyncedAt()
		if err != nil {
			return err
		}
		c.nextUniverse = synced.Add(universeEvery)
	}

	extras := make([]archive.Entry, 0, len(c.cfg.Extras))
	for _, s := range c.cfg.Extras {
		extras = append(extras, archive.Entry{Symbol: s, Kind: archive.KindExtra})
	}
	if now.Before(c.nextUniverse) {
		// Not due, but the extras come from this process's flags rather than
		// from the network, so a newly added one is tracked from startup
		// instead of from tomorrow's re-read.
		if c.targets == nil {
			if err := c.archive.Track(extras, now); err != nil {
				return err
			}
		}
	} else {
		entries := extras
		complete := c.cfg.Universe == nil
		if c.cfg.Universe != nil {
			listings, err := c.cfg.Universe.Listings(ctx)
			if err != nil {
				c.log.Warn("could not read the exchange symbol lists; keeping the last ones", "error", err)
			} else {
				complete = true
				for _, l := range listings {
					kind := archive.KindStock
					if l.ETF {
						kind = archive.KindETF
					}
					entries = append(entries, archive.Entry{Symbol: l.Symbol, Name: l.Name, Exchange: l.Exchange, Kind: kind})
				}
			}
		}
		if err := c.archive.Track(entries, now); err != nil {
			return err
		}
		if complete {
			active, err := c.archive.ActiveSymbols()
			if err != nil {
				return err
			}
			if len(entries)*2 < len(active) {
				c.log.Warn("symbol list is less than half the archive's; not retiring anything",
					"listed", len(entries), "active", len(active))
			} else {
				keep := make([]string, len(entries))
				for i, e := range entries {
					keep[i] = e.Symbol
				}
				retired, err := c.archive.RetireAllExcept(keep)
				if err != nil {
					return err
				}
				if retired > 0 {
					c.log.Info("retired symbols no longer listed", "count", retired)
				}
			}
			if err := c.archive.MarkUniverseSynced(now); err != nil {
				return err
			}
			c.nextUniverse = now.Add(universeEvery)
		} else {
			c.nextUniverse = now.Add(universeRetry)
		}
	}
	return c.loadTargets()
}

// loadTargets rebuilds the in-memory schedule from the archive: every active
// symbol at every interval, with whatever coverage it has.
func (c *Collector) loadTargets() error {
	symbols, err := c.archive.ActiveSymbols()
	if err != nil {
		return err
	}
	coverages, err := c.archive.Coverages()
	if err != nil {
		return err
	}
	type key struct {
		id       int64
		interval quotes.Interval
	}
	byKey := make(map[key]archive.Coverage, len(coverages))
	for _, cov := range coverages {
		byKey[key{cov.SymbolID, cov.Interval}] = cov
	}
	targets := make([]target, 0, len(symbols)*len(c.policies))
	for _, s := range symbols {
		for order, p := range c.policies {
			cov, ok := byKey[key{s.ID, p.Interval}]
			if !ok {
				cov = archive.Coverage{SymbolID: s.ID, Interval: p.Interval}
			}
			targets = append(targets, target{symbol: s.Symbol, firstTrade: s.FirstTrade, policy: p, order: order, coverage: cov})
		}
	}
	c.targets = targets
	c.log.Info("archive schedule loaded", "symbols", len(symbols), "series", len(targets))
	return nil
}

// ParseSymbols splits a comma-separated list, upper-cased, blanks dropped.
func ParseSymbols(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.ToUpper(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
