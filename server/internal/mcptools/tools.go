// Package mcptools is the catalogue of tools the MCP endpoint offers an
// agent: the archive's bars, the indicators over them, the backtester, and
// the two things an agent looking for an edge needs that a person clicking
// through the app doesn't — a parameter sweep with a hold-out, and a study of
// what followed a pattern.
//
// It stands to mcp as api stands to net/http: decoding arguments, calling
// engine, and shaping what comes back. What comes back is shaped for a model
// rather than a chart — rows instead of parallel arrays, rounded numbers, the
// most recent stretch of a long series rather than a thinned one, and a limit
// on everything, because every byte of a result is read.
package mcptools

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/archiver"
	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/mcp"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/version"
)

// Options is what the tools read from.
type Options struct {
	Store  *store.Store
	Engine *engine.Engine
	// Archive is where bars are read. Nil, the archive tools say there is
	// no archive rather than failing to register.
	Archive ArchiveSource
	// Home is where what agents hand back is kept. Nil is this server's
	// own database; a research server sets the collecting server's.
	Home Home
}

// Home keeps what agents hand back — saved strategies, reports, watched
// strategies and their forward tests — where the person will look for it.
// On the collecting server that is its own database; a research server
// working on a copy of the archive sends them to the collecting server's
// API instead (see the remote package), which also runs the forward tests,
// since it is the one with today's bars.
type Home interface {
	Strategies() ([]store.Strategy, error)
	SaveStrategy(id, name string, def json.RawMessage) (store.Strategy, error)
	CreateReport(title, body, author string) (store.Report, error)
	Reports() ([]store.Report, error)
	WatchStrategy(name string, def json.RawMessage, note string) (store.Watch, error)
	Forward() ([]engine.Forward, error)
}

// localHome is Home in this server's own database.
type localHome struct {
	store  *store.Store
	engine *engine.Engine
}

func (h localHome) Strategies() ([]store.Strategy, error) { return h.store.Strategies() }
func (h localHome) SaveStrategy(id, name string, def json.RawMessage) (store.Strategy, error) {
	return h.engine.SaveStrategy(id, name, def)
}
func (h localHome) CreateReport(title, body, author string) (store.Report, error) {
	return h.store.CreateReport(title, body, author)
}
func (h localHome) Reports() ([]store.Report, error) { return h.store.Reports() }
func (h localHome) WatchStrategy(name string, def json.RawMessage, note string) (store.Watch, error) {
	return h.engine.WatchStrategy(name, def, note, time.Now())
}
func (h localHome) Forward() ([]engine.Forward, error) { return h.engine.Forward() }

// ArchiveSource is what the tools need of an archive: the server's
// archiver.Manager, or an archiver.Fixed that a research server opens
// read-only.
type ArchiveSource interface {
	Read(fn func(a *archive.Archive) error) error
	Status(fresh bool) archiver.Status
}

// tools holds the dependencies every handler shares.
type tools struct {
	store   *store.Store
	engine  *engine.Engine
	archive ArchiveSource
	home    Home
	// now is the clock the default windows are measured back from.
	now func() time.Time
}

// New builds the MCP server with every tool and document registered.
func New(opts Options) *mcp.Server {
	t := &tools{store: opts.Store, engine: opts.Engine, archive: opts.Archive, home: opts.Home, now: time.Now}
	if t.home == nil {
		t.home = localHome{store: opts.Store, engine: opts.Engine}
	}
	s := mcp.NewServer(mcp.Info{
		Name:         "tickers",
		Title:        "Tickers market-data archive",
		Version:      version.String(),
		Instructions: instructions,
	})
	t.registerArchive(s)
	t.registerSQL(s)
	t.registerResearch(s)
	t.registerXSection(s)
	t.registerPatterns(s)
	t.registerStrategies(s)
	t.registerLedger(s)
	t.registerOutputs(s)
	s.AddResource(mcp.Resource{
		URI:         languageURI,
		Name:        "strategy-language",
		Title:       "The rule language",
		Description: "Operands, indicators, comparisons and the fill model shared by run_backtest, sweep_strategy and find_signals.",
		MIMEType:    "text/markdown",
		Text:        languageDoc,
	})
	return s
}

// handler adapts a typed handler to mcp's: it decodes the arguments strictly
// into a fresh In, and puts every error into words a model can act on.
func handler[In any](fn func(ctx context.Context, in In) (any, error)) func(context.Context, json.RawMessage) (any, error) {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in In
		if err := mcp.Decode(raw, &in); err != nil {
			return nil, err
		}
		out, err := fn(ctx, in)
		return out, explain(err)
	}
}

// errNoArchive is what every archive tool says on a server without one.
var errNoArchive = errors.New("the market-data archive isn't open, so there are no bars to read — someone has to switch it on and choose a folder on the app's Data page")

// explain turns the engine's sentinels into what a model can do about them.
// Everything else already reads as a sentence: the strategy package's
// refusals name the field, and ErrNoBars says to come back later.
func explain(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrNoArchive), errors.Is(err, archiver.ErrNotOpen):
		return errNoArchive
	case errors.Is(err, archive.ErrUnknownSymbol):
		return errors.New("the archive has never heard of that symbol; search_symbols finds what it holds, and asking run_backtest or find_signals for it adds it to the collection queue")
	case errors.Is(err, store.ErrNotFound):
		return errors.New("there is no saved strategy with that id; list_strategies shows them")
	}
	return err
}

// read runs fn against the open archive.
func (t *tools) read(fn func(a *archive.Archive) error) error {
	if t.archive == nil {
		return errNoArchive
	}
	return t.archive.Read(fn)
}

// round keeps a number's places decimals. Prices from the provider arrive as
// float32s widened to float64 — 187.44000244140625 — and every one of those
// digits is read and paid for; none of them means anything.
func round(x float64, places int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	p := math.Pow10(places)
	return math.Round(x*p) / p
}

// stamp is a bar's time as a model should read it: a daily bar is a date,
// an intraday one an instant in UTC.
func stamp(t time.Time, interval quotes.Interval) string {
	if interval.Intraday() {
		return t.UTC().Format("2006-01-02T15:04Z")
	}
	return t.UTC().Format(time.DateOnly)
}

func date(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}

// clamp bounds a requested count, taking def for zero.
func clamp(n, def, most int) int {
	if n <= 0 {
		return def
	}
	return min(n, most)
}

// window parses the dates a read covers. to is inclusive and defaults to
// today; from defaults to a span back from it that suits the interval.
func (t *tools) window(interval quotes.Interval, from, to string) (time.Time, time.Time, error) {
	end := t.now().UTC().Add(24 * time.Hour)
	if to != "" {
		d, err := time.Parse(time.DateOnly, to)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to must be a date like 2024-12-31")
		}
		end = d.AddDate(0, 0, 1)
	}
	if from == "" {
		return end.Add(-defaultSpan(interval)), end, nil
	}
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("from must be a date like 2015-01-31")
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("from must be before to")
	}
	return start, end, nil
}

// defaultSpan is how far back a read with no start reaches: about a screen
// of bars at each width.
func defaultSpan(i quotes.Interval) time.Duration {
	const day = 24 * time.Hour
	switch i {
	case quotes.Hourly:
		return 30 * day
	case quotes.FiveMinute:
		return 5 * day
	case quotes.OneMinute:
		return 2 * day
	}
	return 365 * day
}

func parseInterval(s string) (quotes.Interval, error) {
	if s == "" {
		return quotes.Daily, nil
	}
	return quotes.ParseInterval(s)
}
