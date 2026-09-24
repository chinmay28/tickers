// Package archiver keeps the market-data archive running: it opens the archive
// wherever the settings say it is, runs the collector over it, notices when
// the drive under it goes away and when it comes back, and moves it to a new
// folder on request.
//
// It is the glue between the packages that know nothing of each other —
// store (where the settings live), quotes (the sources), archive (the files)
// and collector (the loop) — the way engine is for the watchlist. Everything
// the API and the rest of the app need from the archive goes through it, so
// none of them has to cope with the archive being open one minute and on an
// unplugged drive the next.
package archiver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/collector"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/universe"
)

// States the manager reports.
const (
	// StateOff: no archive folder is configured.
	StateOff = "off"
	// StateDisabled: switched off in Settings.
	StateDisabled = "disabled"
	// StateUnavailable: one is configured and can't be opened — the drive is
	// unplugged, the folder was deleted, the files are unreadable.
	StateUnavailable = "unavailable"
	// StateOpen: the archive is open and the collector is running over it
	// (which includes it being paused).
	StateOpen = "open"
	// StateMoving: the archive is being copied to a new folder.
	StateMoving = "moving"
)

// retryEvery is how often an unavailable archive is tried again. A drive
// plugged back in is picked up within this, or at once from the Data page.
const retryEvery = 30 * time.Second

// Options wires a manager.
type Options struct {
	Store *store.Store
	// Yahoo is the live source — the same provider the watchlist uses, so a
	// user agent fixed on the Settings page fixes both.
	Yahoo quotes.Archivist
	// Symbols is everything the app itself uses, collected first.
	Symbols func() ([]string, error)
	// FallbackPath is the startup --archive flag, used when no folder is
	// stored.
	FallbackPath string
	// UniverseURL is where the exchange lists are read.
	UniverseURL string
	Log         *slog.Logger
}

// Manager owns the archive's lifecycle. It is safe for concurrent use.
type Manager struct {
	opts Options
	log  *slog.Logger
	wake chan struct{}

	// open guards the archive handle against being closed under a reader:
	// readers hold it shared for the length of a query, and the run loop
	// takes it exclusively to close.
	open sync.RWMutex
	arch *archive.Archive

	mu        sync.Mutex
	state     string
	path      string
	err       string
	coll      *collector.Collector
	last      collector.Status
	cancelRun context.CancelFunc
	move      *Move
	polygon   *quotes.Polygon
	polyKey   string
	symbols   []string
	symbolsAt time.Time
	stats     *archive.Stats
	size      int64
	statsAt   time.Time
}

// New builds a manager. Run starts it.
func New(opts Options) *Manager {
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Manager{opts: opts, log: opts.Log, wake: make(chan struct{}, 1), state: StateOff}
}

// Run keeps the archive open and collected until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if m.moving() {
			m.wait(ctx, time.Second)
			continue
		}
		path, err := m.resolvePath()
		if err != nil {
			m.set(StateUnavailable, "", err.Error())
			m.wait(ctx, retryEvery)
			continue
		}
		if cfg, err := m.opts.Store.ArchiveConfig(); err == nil && !cfg.Enabled {
			m.set(StateDisabled, path, "")
			m.wait(ctx, time.Minute)
			continue
		}
		if path == "" {
			m.set(StateOff, "", "")
			m.wait(ctx, time.Minute)
			continue
		}
		a, err := archive.Open(path)
		if err != nil {
			m.mu.Lock()
			changed := m.state != StateUnavailable || m.err != err.Error()
			m.mu.Unlock()
			if changed {
				m.log.Warn("market-data archive unavailable", "path", path, "error", err)
			}
			m.set(StateUnavailable, path, err.Error())
			m.wait(ctx, retryEvery)
			continue
		}
		m.runArchive(ctx, a, path)
	}
}

func (m *Manager) runArchive(ctx context.Context, a *archive.Archive, path string) {
	plan := m.planner(path)
	// Every step checks the drive is still there. An idle collector writes
	// nothing, so without this an unplugged drive would go unnoticed — and
	// reported as open — until the next write failed, which could be a day.
	checked := func(ctx context.Context) (collector.Plan, error) {
		if err := a.Ping(); err != nil {
			return collector.Plan{}, err
		}
		return plan(ctx)
	}
	coll, err := collector.New(a, checked, m.log)
	if err != nil {
		a.Close()
		m.set(StateUnavailable, path, err.Error())
		m.wait(ctx, retryEvery)
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.open.Lock()
	m.arch = a
	m.open.Unlock()
	m.mu.Lock()
	m.coll, m.cancelRun, m.stats = coll, cancel, nil
	m.mu.Unlock()
	m.set(StateOpen, path, "")
	m.log.Info("market-data archive open", "path", path)

	err = coll.Run(runCtx)

	m.mu.Lock()
	m.last, m.coll, m.cancelRun = coll.Status(), nil, nil
	m.mu.Unlock()
	m.open.Lock()
	m.arch = nil
	a.Close()
	m.open.Unlock()

	switch {
	case ctx.Err() != nil, errors.Is(err, errRelocated), err == nil:
		// Shutting down, or the folder changed: the loop reopens wherever
		// the settings now say.
	default:
		m.log.Warn("market-data archive stopped", "path", path, "error", err)
		m.set(StateUnavailable, path, err.Error())
		m.wait(ctx, retryEvery)
	}
}

// wait sleeps, waking early on Nudge.
func (m *Manager) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	case <-m.wake:
	}
}

// Nudge makes the manager and its collector act on changed settings now.
func (m *Manager) Nudge() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
	m.mu.Lock()
	coll := m.coll
	m.mu.Unlock()
	if coll != nil {
		coll.Nudge()
	}
}

func (m *Manager) set(state, path, err string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.path, m.err = state, path, err
}

func (m *Manager) moving() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.move != nil && m.move.State == MoveCopying
}

// resolvePath is the folder to use: the stored one, else the startup flag.
func (m *Manager) resolvePath() (string, error) {
	cfg, err := m.opts.Store.ArchiveConfig()
	if err != nil {
		return "", err
	}
	if cfg.Path != "" {
		return cfg.Path, nil
	}
	if m.opts.FallbackPath == "" {
		return "", nil
	}
	return filepath.Abs(m.opts.FallbackPath)
}

// errRelocated ends a collector run whose folder is no longer the configured
// one, or that has been switched off.
var errRelocated = errors.New("the archive folder changed")

// planner builds the collector's plan from the stored settings on every step,
// so a setting changed on the Data page applies at the next request.
func (m *Manager) planner(path string) collector.Planner {
	return func(ctx context.Context) (collector.Plan, error) {
		cfg, err := m.opts.Store.ArchiveConfig()
		if err != nil {
			return collector.Plan{}, err
		}
		if now, err := m.resolvePath(); err != nil || now != path || m.moving() || !cfg.Enabled {
			return collector.Plan{}, errRelocated
		}
		var intervals []quotes.Interval
		for _, s := range cfg.Intervals {
			if i, err := quotes.ParseInterval(s); err == nil {
				intervals = append(intervals, i)
			}
		}
		plan := collector.Plan{
			Paused:    cfg.Paused,
			Intervals: intervals,
			Extras:    cfg.Extras,
			Watchlist: m.appSymbols(),
			MinFree:   uint64(cfg.MinFreeGB) << 30,
			// The watchlist's sparklines are drawn from these bars, so they
			// are kept a quarter of an hour fresh rather than a day.
			PriorityEvery: 15 * time.Minute,
			Sources:       []collector.Source{collector.Yahoo(m.opts.Yahoo, time.Duration(cfg.SpacingMS)*time.Millisecond)},
		}
		if cfg.Listed {
			plan.Universe = universe.Source{BaseURL: m.opts.UniverseURL, Client: &http.Client{Timeout: time.Minute}}
		}
		if cfg.PolygonKey != "" {
			plan.Sources = append(plan.Sources, collector.Polygon(m.polygonFor(cfg), cfg.PolygonYears, cfg.PolygonPerMinute))
		}
		return plan, nil
	}
}

// polygonFor keeps one Polygon client while its key and URL stay the same,
// so its split cache survives from one step to the next.
func (m *Manager) polygonFor(cfg store.ArchiveConfig) *quotes.Polygon {
	key := cfg.PolygonKey + "\x00" + cfg.PolygonBaseURL
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.polygon == nil || m.polyKey != key {
		m.polygon, m.polyKey = quotes.NewPolygon(cfg.PolygonBaseURL, cfg.PolygonKey, 0), key
	}
	return m.polygon
}

// appSymbols is the app's own symbol list, re-read at most once a minute.
func (m *Manager) appSymbols() []string {
	m.mu.Lock()
	fresh := time.Since(m.symbolsAt) < time.Minute
	cached := m.symbols
	m.mu.Unlock()
	if fresh || m.opts.Symbols == nil {
		return cached
	}
	symbols, err := m.opts.Symbols()
	if err != nil {
		m.log.Warn("could not read the watchlist's symbols for the archive", "error", err)
		return cached
	}
	m.mu.Lock()
	m.symbols, m.symbolsAt = symbols, time.Now()
	m.mu.Unlock()
	return symbols
}

// ---------------------------------------------------------------------------
// For the rest of the app
// ---------------------------------------------------------------------------

// ErrNotOpen is what a read gets when there is no archive to read — none is
// configured, or its drive is unplugged. Callers fall back to the provider.
var ErrNotOpen = errors.New("the market-data archive is not open")

// Read runs fn against the open archive, holding it open for fn's duration.
func (m *Manager) Read(fn func(a *archive.Archive) error) error {
	m.open.RLock()
	defer m.open.RUnlock()
	if m.arch == nil {
		return ErrNotOpen
	}
	return fn(m.arch)
}

// Enqueue queues a hand-requested fetch.
func (m *Manager) Enqueue(j collector.Job) (collector.Job, error) {
	m.mu.Lock()
	coll := m.coll
	m.mu.Unlock()
	if coll == nil {
		return j, ErrNotOpen
	}
	return coll.Enqueue(j)
}

// Status is the archive at a glance, for the Data page.
type Status struct {
	State   string `json:"state"`
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
	// FromFlag says the folder came from the startup flag rather than the
	// Data page.
	FromFlag  bool              `json:"fromFlag"`
	Error     string            `json:"error"`
	Collector *collector.Status `json:"collector"`
	Stats     *archive.Stats    `json:"stats"`
	Size      int64             `json:"size"`
	DiskFree  uint64            `json:"diskFree"`
	DiskTotal uint64            `json:"diskTotal"`
	Move      *Move             `json:"move"`
}

// statsEvery is how long a stats snapshot is served. Stats counts the daily
// file and walks the folder — a second or two on a full archive on a Pi —
// and the Data page polls every ten seconds.
const statsEvery = time.Minute

// Status reports the archive's state. Stats are refreshed at most once a
// minute; fresh forces it.
func (m *Manager) Status(fresh bool) Status {
	m.mu.Lock()
	st := Status{State: m.state, Path: m.path, Error: m.err}
	if m.coll != nil {
		cs := m.coll.Status()
		st.Collector = &cs
	} else if m.last.State != "" {
		cs := m.last
		st.Collector = &cs
	}
	if m.move != nil {
		mv := *m.move
		st.Move = &mv
	}
	stale := fresh || m.stats == nil || time.Since(m.statsAt) > statsEvery
	st.Stats, st.Size = m.stats, m.size
	m.mu.Unlock()

	if cfg, err := m.opts.Store.ArchiveConfig(); err == nil {
		st.FromFlag = cfg.Path == "" && m.opts.FallbackPath != ""
		st.Enabled = cfg.Enabled
		if st.Path == "" {
			st.Path, _ = m.resolvePath()
		}
	}
	if st.Path != "" {
		if free, total, err := archive.Disk(st.Path); err == nil {
			st.DiskFree, st.DiskTotal = free, total
		}
	}
	if st.State == StateOpen && stale {
		m.Read(func(a *archive.Archive) error {
			stats, err := a.Stats()
			if err != nil {
				return err
			}
			size, _ := a.Size()
			m.mu.Lock()
			m.stats, m.size, m.statsAt = &stats, size, time.Now()
			m.mu.Unlock()
			st.Stats, st.Size = &stats, size
			return nil
		})
	}
	return st
}

// ---------------------------------------------------------------------------
// Choosing and moving the folder
// ---------------------------------------------------------------------------

// Move states.
const (
	MoveCopying = "copying"
	MoveDone    = "done"
	MoveFailed  = "failed"
)

// Move is a copy of the archive to a new folder, in progress or finished.
type Move struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	State    string    `json:"state"`
	Copied   int64     `json:"copied"`
	Total    int64     `json:"total"`
	Error    string    `json:"error"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// Folder describes a folder somebody is thinking of putting the archive in.
type Folder struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Writable  bool   `json:"writable"`
	IsArchive bool   `json:"isArchive"`
	Empty     bool   `json:"empty"`
	DiskFree  uint64 `json:"diskFree"`
	DiskTotal uint64 `json:"diskTotal"`
	// SameDiskAsRoot is true when the folder is on the same filesystem as
	// the server's own root — which, on a Pi with the drive unplugged, is the
	// SD card under an empty mount point. The Data page warns about it.
	SameDiskAsRoot bool   `json:"sameDiskAsRoot"`
	Problem        string `json:"problem"`
}

// Inspect reports on a candidate folder without changing anything.
func Inspect(path string) Folder {
	f := Folder{Path: filepath.Clean(path)}
	if !filepath.IsAbs(path) {
		f.Problem = "the path must be absolute, like /mnt/usb/tickers-archive"
		return f
	}
	info, err := os.Stat(f.Path)
	if err != nil {
		f.Problem = "the folder does not exist — create it, or mount the drive, first"
		return f
	}
	if !info.IsDir() {
		f.Problem = "that is a file, not a folder"
		return f
	}
	f.Exists = true
	f.IsArchive = archive.IsArchive(f.Path)
	entries, _ := os.ReadDir(f.Path)
	f.Empty = len(entries) == 0
	if probe, err := os.CreateTemp(f.Path, ".tickers-probe-*"); err == nil {
		probe.Close()
		os.Remove(probe.Name())
		f.Writable = true
	} else {
		f.Problem = "the server cannot write there — check the folder is owned by the service's user, and that the systemd unit allows writing to it (DEPLOYMENT.md, ReadWritePaths)"
	}
	f.DiskFree, f.DiskTotal, _ = archive.Disk(f.Path)
	f.SameDiskAsRoot = sameDevice(f.Path, "/")
	return f
}

// Use points the archive at a folder that already is one, or starts a new,
// empty archive there. Nothing is copied: the old folder is left as it was.
func (m *Manager) Use(path string) error {
	f := Inspect(path)
	if f.Problem != "" {
		return errors.New(f.Problem)
	}
	if m.moving() {
		return errors.New("the archive is being moved; wait for that to finish")
	}
	if !f.IsArchive {
		if err := archive.Init(f.Path); err != nil {
			return err
		}
	}
	if err := m.opts.Store.SetArchivePath(f.Path); err != nil {
		return err
	}
	// Choosing a folder is asking for an archive in it.
	on := true
	if _, err := m.opts.Store.UpdateArchiveConfig(store.ArchivePatch{Enabled: &on}); err != nil {
		return err
	}
	m.log.Info("market-data archive folder set", "path", f.Path, "new", !f.IsArchive)
	m.Nudge()
	return nil
}

// Detach stops using any stored folder, falling back to the startup flag.
func (m *Manager) Detach() error {
	if m.moving() {
		return errors.New("the archive is being moved; wait for that to finish")
	}
	if err := m.opts.Store.SetArchivePath(""); err != nil {
		return err
	}
	m.Nudge()
	return nil
}

// MoveTo copies the open archive into an empty folder and switches to it once
// every file has arrived. The copy runs in the background; Status reports its
// progress.
//
// The marker file is copied last. A copy that fails half way leaves a folder
// that is not an archive, so it can never be opened by mistake — and the old
// folder, untouched, stays in use.
func (m *Manager) MoveTo(dest string) error {
	f := Inspect(dest)
	if f.Problem != "" {
		return errors.New(f.Problem)
	}
	if f.IsArchive {
		return errors.New("that folder already holds an archive; use it instead of moving into it")
	}
	if !f.Empty {
		return errors.New("move into an empty folder, so nothing already there is mixed into the archive")
	}
	m.mu.Lock()
	from := m.path
	open := m.state == StateOpen
	if m.move != nil && m.move.State == MoveCopying {
		m.mu.Unlock()
		return errors.New("a move is already under way")
	}
	if !open || from == "" {
		m.mu.Unlock()
		return errors.New("the archive has to be open to be moved")
	}
	if filepath.Clean(from) == f.Path {
		m.mu.Unlock()
		return errors.New("that is where the archive already is")
	}
	if rel, err := filepath.Rel(from, f.Path); err == nil && !startsWithDotDot(rel) {
		m.mu.Unlock()
		return errors.New("the new folder cannot be inside the archive")
	}
	mv := &Move{From: from, To: f.Path, State: MoveCopying, Started: time.Now()}
	m.move = mv
	cancel := m.cancelRun
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	go m.copyArchive(mv)
	return nil
}

func startsWithDotDot(rel string) bool {
	return rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator)
}

func (m *Manager) copyArchive(mv *Move) {
	finish := func(err error) {
		m.mu.Lock()
		mv.Finished = time.Now()
		if err != nil {
			mv.State, mv.Error = MoveFailed, err.Error()
			m.log.Warn("archive move failed", "to", mv.To, "error", err)
		} else {
			mv.State = MoveDone
			m.log.Info("archive moved", "from", mv.From, "to", mv.To, "bytes", mv.Copied)
		}
		m.mu.Unlock()
		m.Nudge()
	}

	// Wait for the run loop to close the archive, which checkpoints every
	// WAL into its main file: what is copied is then complete.
	for i := 0; ; i++ {
		m.open.RLock()
		closed := m.arch == nil
		m.open.RUnlock()
		if closed {
			break
		}
		if i > 600 {
			finish(errors.New("the collector did not stop within a minute"))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	var files []string
	var total int64
	err := filepath.WalkDir(mv.From, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) == archive.MarkerFile {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, path)
		total += info.Size()
		return nil
	})
	if err != nil {
		finish(err)
		return
	}
	marker := filepath.Join(mv.From, archive.MarkerFile)
	if info, err := os.Stat(marker); err == nil {
		total += info.Size()
	}
	m.mu.Lock()
	mv.Total = total
	m.mu.Unlock()
	if free, _, err := archive.Disk(mv.To); err == nil && free < uint64(total) {
		finish(fmt.Errorf("the new folder's disk has %s free and the archive is %s", humanBytes(free), humanBytes(uint64(total))))
		return
	}

	for _, src := range append(files, marker) {
		rel, err := filepath.Rel(mv.From, src)
		if err != nil {
			finish(err)
			return
		}
		if err := copyFile(src, filepath.Join(mv.To, rel), func(n int64) {
			m.mu.Lock()
			mv.Copied += n
			m.mu.Unlock()
		}); err != nil {
			finish(fmt.Errorf("copy %s: %w", rel, err))
			return
		}
	}
	if err := m.opts.Store.SetArchivePath(mv.To); err != nil {
		finish(err)
		return
	}
	finish(nil)
}

// copyFile copies one file, reporting progress, and syncs it before
// returning: the switch to the new folder must not outrun the data.
func copyFile(src, dst string, progress func(int64)) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				out.Close()
				return err
			}
			progress(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
