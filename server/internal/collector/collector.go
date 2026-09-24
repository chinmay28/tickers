// Package collector fills the archive: every listed US symbol, the extras
// and everything the app itself shows, at every configured interval, forward
// from now and backward as far as each source goes — at a pace each source
// tolerates.
//
// It is a loop around pure pieces, each tested on its own: next picks the
// most urgent window, advance and fail work out what a response means for a
// cursor, missing narrows a backfill to the days not yet held, and pacer says
// when a source may be asked again. The loop moves data between them, the
// sources and the archive, and re-reads its Plan every pass so a setting
// changed in the GUI applies without a restart.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/universe"
)

// Lister is where the exchange-listed symbols come from. universe.Source is
// one; a test supplies its own.
type Lister interface {
	Listings(ctx context.Context) ([]universe.Listing, error)
}

// Plan is everything the collector is told to do, as of now.
type Plan struct {
	// Paused stops all fetching. Lists are still kept current.
	Paused bool
	// Intervals to collect.
	Intervals []quotes.Interval
	// Sources to fetch from; see Source.
	Sources []Source
	// Universe lists the exchange-traded symbols. Nil collects none of them,
	// and retires any collected before.
	Universe Lister
	// Extras are symbols collected besides the exchange lists.
	Extras []string
	// Watchlist is what the app itself prices: watchlist rows, composite
	// legs, portfolio holdings. It goes first.
	Watchlist []string
	// MinFree pauses collection when the archive's disk has less than this
	// many bytes free. Zero never pauses.
	MinFree uint64
	// Extended collects the bars before the open and after the close too,
	// from every source that can give them (quotes.ExtendedArchivist). They
	// are stored tagged with their session and read only when asked for.
	Extended bool
	// PriorityEvery is how stale the live source lets a priority symbol's
	// intraday series get — shorter than everyone else's daily pass, so the
	// watchlist's sparklines are drawn from bars at most this old. Zero
	// leaves priority symbols on the ordinary cadence.
	PriorityEvery time.Duration
}

// Planner produces the current Plan. It is called before every request, so
// it should be cheap; the caller caches whatever is expensive to build.
type Planner func(ctx context.Context) (Plan, error)

// DefaultSpacing is the default gap between two requests to Yahoo.
const DefaultSpacing = 2 * time.Second

// MinSpacing is the floor for any source. Below it the collector is a load
// test.
const MinSpacing = 250 * time.Millisecond

// Timing of the loop's housekeeping.
const (
	// universeEvery is how often the exchange lists are re-read; they
	// change by a handful of listings a day.
	universeEvery = 24 * time.Hour
	// universeRetry is how soon a failed read is retried.
	universeRetry = time.Hour
	// maxIdle bounds a sleep with nothing due, so a new symbol or a changed
	// setting never waits long.
	maxIdle = time.Minute
	// minIdle floors it, so an edge case costs a nap rather than a spinning
	// core.
	minIdle = 2 * time.Second
	// diskEvery is how often free space is checked.
	diskEvery = time.Minute
	// reportEvery is how often progress is logged. Per-request lines are
	// debug: at a request every two seconds they would be most of the log.
	reportEvery = 15 * time.Minute
)

// metaListsRead is the archive meta key the exchange lists' last read is
// kept under, so a restart doesn't re-read them.
const metaListsRead = "universe_synced_at"

// Collector is the loop. Run it from one goroutine; Status, Enqueue and
// Nudge are safe to call from any.
type Collector struct {
	archive *archive.Archive
	planner Planner
	log     *slog.Logger

	// now and sleep are the clock, swapped out in tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	// Loop state, touched only by Run's goroutine.
	sources      []Source
	pacers       map[string]*pacer
	targets      []target
	signature    string
	extras       []string
	watchlist    []string
	nextUniverse time.Time
	listedOff    bool
	diskAt       time.Time
	diskLow      bool
	reportedAt   time.Time
	counts       counters
	// extended is this step's Plan.Extended.
	extended bool
	// renames are newly listed symbols waiting to be asked about former
	// names. In memory: a restart forgets a few, which costs a join the next
	// rename check would have made; persisting them would cost a table.
	renames []string

	mu     sync.Mutex
	status Status
	jobs   []*Job
	jobSeq int
	nudge  chan struct{}
	dirty  atomic.Bool
}

// New builds a collector over an open archive.
func New(a *archive.Archive, planner Planner, log *slog.Logger) (*Collector, error) {
	if a == nil || planner == nil {
		return nil, errors.New("collector needs an archive and a planner")
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Collector{
		archive: a,
		planner: planner,
		log:     log,
		now:     time.Now,
		sleep:   sleepCtx,
		pacers:  map[string]*pacer{},
		nudge:   make(chan struct{}, 1),
		status:  Status{State: StateStarting},
	}, nil
}

// Run collects until ctx is cancelled or the archive fails. It returns nil on
// cancellation. A source failing is a series' problem, recorded against it;
// only the archive failing — an unplugged drive, a full disk — stops the loop,
// and the caller decides what to do about that.
func (c *Collector) Run(ctx context.Context) error {
	c.log.Info("archive collector started", "archive", c.archive.Root())
	for ctx.Err() == nil {
		if _, err := c.Step(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			c.setState(StateStopped, err.Error())
			return err
		}
	}
	return nil
}

// Nudge wakes the loop from an idle or paused sleep and has it re-read the
// archive's symbols, so a symbol added, excluded or marked on the Data page
// is acted on now rather than at the next list read.
func (c *Collector) Nudge() {
	c.dirty.Store(true)
	select {
	case c.nudge <- struct{}{}:
	default:
	}
}

// Step does one unit of work and reports whether it made a request.
func (c *Collector) Step(ctx context.Context) (bool, error) {
	plan, err := c.planner(ctx)
	if err != nil {
		return false, fmt.Errorf("collector plan: %w", err)
	}
	if err := validSources(plan.Sources); err != nil {
		c.setState(StatePaused, err.Error())
		return false, c.idle(ctx, maxIdle)
	}
	now := c.now()
	if err := c.syncLists(ctx, plan, now); err != nil {
		return false, err
	}
	if err := c.syncTargets(plan); err != nil {
		return false, err
	}

	if plan.Paused {
		c.setState(StatePaused, "paused on the Data page")
		return false, c.idle(ctx, maxIdle)
	}
	if low, why := c.checkDisk(plan, now); low {
		c.setState(StatePaused, why)
		return false, c.idle(ctx, maxIdle)
	}
	if len(plan.Sources) == 0 {
		c.setState(StatePaused, "no source to collect from")
		return false, c.idle(ctx, maxIdle)
	}

	now = c.now()
	c.publishSchedule(now)
	ready := func(source int) bool { return c.pacerFor(c.sources[source]).delay(now) == 0 }

	c.extended = plan.Extended
	if job := c.runnableJob(ready); job != nil {
		return true, c.jobStep(ctx, job)
	}
	if si, symbol, ok := c.renameCheck(ready); ok {
		return true, c.checkRename(ctx, si, symbol)
	}
	if tk, ok := next(c.targets, now, ready); ok {
		return true, c.fetch(ctx, tk)
	}

	// Nothing ready: either every source with work is waiting on its pacer,
	// or there is no work at all.
	if _, ok := next(c.targets, now, nil); ok || c.hasJobs() {
		wait := maxIdle
		for _, s := range c.sources {
			if d := c.pacerFor(s).delay(now); d > 0 && d < wait {
				wait = d
			}
		}
		if c.anyPaused(now) {
			c.setState(StateWaiting, "the source asked us to slow down")
		}
		return false, c.sleep(ctx, wait)
	}
	idle := maxIdle
	if at := wake(c.targets, now); !at.IsZero() && at.Sub(now) < idle {
		idle = at.Sub(now)
	}
	c.setState(StateIdle, "every series is current")
	return false, c.idle(ctx, min(max(idle, minIdle), maxIdle))
}

// idle sleeps, but wakes early on a nudge.
func (c *Collector) idle(ctx context.Context, d time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.nudge:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := c.sleep(ctx, d)
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		// Our own cancel — a nudge — is not the caller's cancellation.
		return nil
	}
	return err
}

func (c *Collector) pacerFor(s Source) *pacer {
	p, ok := c.pacers[s.Name]
	if !ok {
		p = &pacer{}
		c.pacers[s.Name] = p
	}
	p.spacing = s.Spacing
	return p
}

func (c *Collector) anyPaused(now time.Time) bool {
	for _, p := range c.pacers {
		if p.paused(now) {
			return true
		}
	}
	return false
}

// checkDisk pauses when the archive's drive is nearly full. An archive that
// fills the disk it shares with nothing is merely stuck; one that fills the
// Pi's root filesystem takes the whole machine with it.
func (c *Collector) checkDisk(plan Plan, now time.Time) (bool, string) {
	if plan.MinFree == 0 {
		c.diskLow = false
		return false, ""
	}
	if now.Sub(c.diskAt) >= diskEvery {
		c.diskAt = now
		free, total, err := archive.Disk(c.archive.Root())
		c.mu.Lock()
		c.status.DiskFree, c.status.DiskTotal = free, total
		c.mu.Unlock()
		c.diskLow = err == nil && free < plan.MinFree
	}
	if c.diskLow {
		return true, fmt.Sprintf("less than %s free on the archive's disk", humanBytes(plan.MinFree))
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Lists and targets
// ---------------------------------------------------------------------------

// syncLists keeps the archive's lists in step with the plan: the extras and
// the watchlist whenever they change, the exchange lists once a day.
//
// Retiring is the dangerous half of the exchange lists. A symbol missing from
// a list it was on is delisted and should stop being fetched — but a list
// that comes back short for any other reason would retire half the market, so
// a list less than half the size of the one already held is not believed.
func (c *Collector) syncLists(ctx context.Context, plan Plan, now time.Time) error {
	extras := normalized(plan.Extras)
	if !slices.Equal(extras, c.extras) || c.targets == nil {
		if _, _, err := c.archive.SetList(archive.Extra, entries(extras, archive.KindOther), now); err != nil {
			return err
		}
		c.extras = extras
		c.signature = ""
	}
	watchlist := normalized(plan.Watchlist)
	if !slices.Equal(watchlist, c.watchlist) || c.targets == nil {
		if _, _, err := c.archive.SetList(archive.Watchlist, entries(watchlist, ""), now); err != nil {
			return err
		}
		c.watchlist = watchlist
		c.signature = ""
	}

	if plan.Universe == nil {
		// Switched off: the listed symbols stop being fetched. Their history
		// stays, and switching back on brings them back with it.
		if !c.listedOff {
			if _, left, err := c.archive.SetList(archive.Listed, nil, now); err != nil {
				return err
			} else if left > 0 {
				c.log.Info("stopped collecting the exchange lists", "symbols", left)
				c.signature = ""
			}
			c.listedOff, c.nextUniverse = true, time.Time{}
		}
		return nil
	}
	c.listedOff = false
	if c.nextUniverse.IsZero() {
		v, err := c.archive.Meta(metaListsRead)
		if err != nil {
			return err
		}
		read, _ := time.Parse(time.RFC3339, v)
		c.nextUniverse = read.Add(universeEvery)
	}
	if now.Before(c.nextUniverse) {
		return nil
	}

	listings, err := plan.Universe.Listings(ctx)
	if err != nil {
		c.log.Warn("could not read the exchange symbol lists; keeping the last ones", "error", err)
		c.nextUniverse = now.Add(universeRetry)
		return nil
	}
	list := make([]archive.Entry, 0, len(listings))
	for _, l := range listings {
		kind := archive.KindStock
		if l.ETF {
			kind = archive.KindETF
		}
		list = append(list, archive.Entry{Symbol: l.Symbol, Name: l.Name, Exchange: l.Exchange, Kind: kind})
	}
	stats, err := c.archive.Stats()
	if err != nil {
		return err
	}
	if listed := stats.Lists[archive.Listed]; len(list)*2 < listed {
		c.log.Warn("symbol list is less than half the one held; not believing it",
			"listed", len(list), "held", listed)
		c.nextUniverse = now.Add(universeRetry)
		return nil
	}
	before, err := c.archive.Members(archive.Listed)
	if err != nil {
		return err
	}
	joined, left, err := c.archive.SetList(archive.Listed, list, now)
	if err != nil {
		return err
	}
	// A symbol new to the lists may be an old company under a new name —
	// META the day FB disappeared. The very first read is every symbol, and
	// nothing collected yet sits under a former name, so only later reads
	// are worth asking about.
	if len(before) > 0 && joined > 0 {
		for _, e := range list {
			if !before[archive.NormalizeSymbol(e.Symbol)] {
				c.queueRenameCheck(archive.NormalizeSymbol(e.Symbol))
			}
		}
	}
	if joined > 0 || left > 0 {
		c.log.Info("exchange lists read", "listed", len(list), "new", joined, "retired", left)
		c.signature = ""
	}
	if err := c.archive.SetMeta(metaListsRead, now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	c.nextUniverse = now.Add(universeEvery)
	c.mu.Lock()
	c.status.ListsReadAt = now
	c.mu.Unlock()
	return nil
}

func normalized(symbols []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range symbols {
		s = archive.NormalizeSymbol(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func entries(symbols []string, kind string) []archive.Entry {
	out := make([]archive.Entry, len(symbols))
	for i, s := range symbols {
		out[i] = archive.Entry{Symbol: s, Kind: kind}
	}
	return out
}

// syncTargets rebuilds the schedule when what it is built from has changed:
// the symbols, the intervals or the sources. Otherwise the in-memory targets
// are kept, with the cursors the loop itself has been advancing.
func (c *Collector) syncTargets(plan Plan) error {
	var sig strings.Builder
	for _, i := range plan.Intervals {
		sig.WriteString(string(i) + ",")
	}
	for _, s := range plan.Sources {
		fmt.Fprintf(&sig, "|%s:%v:%p", s.Name, s.Live, s.Provider)
	}
	fmt.Fprintf(&sig, "|%s", plan.PriorityEvery)
	if c.dirty.Swap(false) {
		c.signature = ""
	}
	if sig.String() == c.signature && c.targets != nil {
		c.sources = plan.Sources
		return nil
	}
	symbols, err := c.archive.ActiveSymbols()
	if err != nil {
		return err
	}
	cursors, err := c.archive.Cursors()
	if err != nil {
		return err
	}
	type key struct {
		id       int64
		interval quotes.Interval
		source   string
	}
	byKey := make(map[key]archive.Cursor, len(cursors))
	for _, cur := range cursors {
		byKey[key{cur.SymbolID, cur.Interval, cur.Source}] = cur
	}
	targets := make([]target, 0, len(symbols)*len(plan.Intervals))
	for _, s := range symbols {
		for order, i := range plan.Intervals {
			for si, src := range plan.Sources {
				reach, ok := reachFor(src, i, plan.Intervals)
				if !ok {
					continue
				}
				if s.Priority && i.Intraday() && reach.Every > 0 && plan.PriorityEvery > 0 {
					reach.Every = min(reach.Every, plan.PriorityEvery)
				}
				cur, ok := byKey[key{s.ID, i, src.Name}]
				if !ok {
					cur = archive.Cursor{SymbolID: s.ID, Interval: i, Source: src.Name}
				}
				targets = append(targets, target{
					symbolID: s.ID, symbol: s.Symbol, firstTrade: s.FirstTrade, priority: s.Priority,
					interval: i, source: si, reach: reach, order: order, cursor: cur,
				})
			}
		}
	}
	c.targets, c.sources, c.signature = targets, plan.Sources, sig.String()
	c.log.Info("archive schedule loaded", "symbols", len(symbols), "series", len(targets))
	return nil
}

// ---------------------------------------------------------------------------
// Fetching
// ---------------------------------------------------------------------------

func (c *Collector) fetch(ctx context.Context, tk task) error {
	t := &c.targets[tk.target]
	src := c.sources[t.source]
	from, to := tk.from, tk.to

	c.setState(StateCollecting, "")
	c.setCurrent(fmt.Sprintf("%s %s %s from %s", t.symbol, t.interval, tk.kind, src.Name))

	// A backfill window the archive already holds all of — another source
	// got there first — costs no request at all.
	if tk.kind != forward && t.interval.Intraday() {
		f, tt, any, err := c.gap(t, from, to)
		if err != nil {
			return err
		}
		if !any {
			cur := advance(*t, tk, quotes.CandleSeries{}, false, c.now())
			if err := c.archive.SaveCursor(cur); err != nil {
				return err
			}
			t.cursor = cur
			c.counts.add(c.now(), 0, 0, 0, 1)
			return nil
		}
		from, to = f, tt
	}

	series, err := c.candles(ctx, src, t.symbol, t.interval, from, to)
	now := c.now()
	if ctx.Err() != nil {
		// Cancelled mid-request: nothing was learned about the symbol.
		return ctx.Err()
	}
	c.pacerFor(src).observe(err, now)

	if errors.Is(err, quotes.ErrRateLimited) {
		c.log.Warn("archive source rate limited", "source", src.Name, "symbol", t.symbol)
		c.counts.add(now, 1, 0, 1, 0)
		return nil
	}
	if err != nil {
		cur := fail(*t, err, now)
		c.log.Debug("archive fetch failed", "symbol", t.symbol, "interval", t.interval, "source", src.Name,
			"kind", tk.kind, "failures", cur.Failures, "error", err)
		c.counts.add(now, 1, 0, 1, 0)
		c.report(now)
		if err := c.archive.SaveCursor(cur); err != nil {
			return err
		}
		t.cursor = cur
		return nil
	}

	series.Candles = settled(t.interval, series.Candles, now)
	cur := advance(*t, tk, series, true, now)
	if err := c.archive.Record(archive.Batch{
		SymbolID: t.symbolID, Interval: t.interval, Source: src.Name, Series: series, Cursor: &cur,
	}); err != nil {
		return fmt.Errorf("archive %s %s: %w", t.symbol, t.interval, err)
	}
	t.cursor = cur
	if !series.FirstTrade.IsZero() {
		for i := range c.targets {
			if c.targets[i].symbolID == t.symbolID {
				c.targets[i].firstTrade = series.FirstTrade
			}
		}
	}
	c.counts.add(now, 1, len(series.Candles), 0, 0)
	c.log.Debug("archived", "symbol", t.symbol, "interval", t.interval, "source", src.Name, "kind", tk.kind,
		"from", from.Format(time.DateOnly), "to", to.Format(time.DateOnly), "bars", len(series.Candles))
	c.report(now)
	return nil
}

// gap narrows a backfill window to the days the archive lacks.
func (c *Collector) gap(t *target, from, to time.Time) (time.Time, time.Time, bool, error) {
	trading, err := c.archive.TradingDays(t.symbolID, from, to)
	if err != nil {
		return from, to, true, err
	}
	held, err := c.archive.HeldDays(t.symbolID, t.interval, from, to)
	if err != nil {
		return from, to, true, err
	}
	f, tt, any := missing(from, to, trading, held)
	return f, tt, any, nil
}

func (c *Collector) report(now time.Time) {
	if c.reportedAt.IsZero() {
		c.reportedAt = now
		return
	}
	if now.Sub(c.reportedAt) < reportEvery {
		return
	}
	h := c.counts.lastHour(now)
	c.log.Info("archive progress", "requests/h", h.Requests, "bars/h", h.Bars, "failures/h", h.Failures,
		"skipped/h", h.Skipped, "series", len(c.targets))
	c.reportedAt = now
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

// ParseSymbols splits a comma-separated list, upper-cased, blanks dropped.
func ParseSymbols(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := archive.NormalizeSymbol(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
