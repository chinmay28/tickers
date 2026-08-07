// Command tickers is the whole application: a REST API, a scheduled quote
// refresh loop, a downstream publisher, and the web client — one static
// binary, no runtime dependencies.
//
//	tickers serve --db /var/lib/tickers/tickers.sqlite --port 8797
//	tickers version
//	tickers publish        # one cycle, then exit (the original script's job)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/chinmay28/tickers/server/internal/api"
	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/version"
	"github.com/chinmay28/tickers/server/internal/web"
)

// DefaultDB is where the database lives when nothing says otherwise. It is a
// relative path on purpose: a bare `tickers serve` in a checkout should not
// write to a system directory. The systemd unit always passes an absolute one.
const DefaultDB = "./data/tickers.sqlite"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tickers: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("no command given")
	}

	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "publish":
		return publishOnce(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `tickers %s — a self-hosted watchlist that publishes quotes downstream

Usage:
  tickers serve [flags]     run the API, the web client and the refresh loop
  tickers publish [flags]   run one refresh + publish cycle, then exit
  tickers version           print the version
  tickers help              show this message

Run 'tickers serve -h' for the serve flags.
`, version.String())
}

// flagSet builds the flag set shared by serve and publish.
//
// Flags win over environment variables, which win over defaults. The env vars
// exist because systemd units are easier to template that way, and because the
// original deployment was configured entirely through the environment; the
// flags exist because they are self-documenting. Both are supported, and the
// precedence is the boring one people expect.
type config struct {
	db             string
	port           int
	host           string
	webDist        string
	quoteBaseURL   string
	quoteUserAgent string
	quoteTimeout   int
	verbose        bool
}

func bindFlags(fs *flag.FlagSet, cfg *config) {
	fs.StringVar(&cfg.db, "db", envOr("TICKERS_DB", DefaultDB), "path to the SQLite database")
	// 8797, not 8787: CountRoster owns 8787, and the two are meant to coexist
	// on the same Raspberry Pi.
	fs.IntVar(&cfg.port, "port", envInt("PORT", 8797), "port to listen on")
	fs.StringVar(&cfg.host, "host", envOr("HOST", "0.0.0.0"), "address to bind")
	fs.StringVar(&cfg.webDist, "web-dist", envOr("WEB_DIST", ""),
		"serve the web client from this directory instead of the embedded copy")
	// The three quote-source flags are fallbacks the Settings page can override
	// (see newProvider). They exist so a systemd unit can be templated; the GUI
	// is where they are normally changed.
	fs.StringVar(&cfg.quoteBaseURL, "quote-base-url", envOr("TICKERS_QUOTE_BASE_URL", ""),
		"quote API root to use instead of Yahoo's (a mirror, a caching proxy, or a test double)")
	fs.StringVar(&cfg.quoteUserAgent, "quote-user-agent", envOr("TICKERS_QUOTE_USER_AGENT", ""),
		"User-Agent sent to the quote provider (empty uses a browser default)")
	fs.IntVar(&cfg.quoteTimeout, "quote-timeout", envInt("TICKERS_QUOTE_TIMEOUT", 0),
		"seconds to wait for one quote request (0 uses the provider default)")
	fs.BoolVar(&cfg.verbose, "verbose", envOr("TICKERS_VERBOSE", "") != "", "log every API request")
}

// newProvider builds the quote source.
//
// What the flags supply here is a *fallback*, not a fixed value: the same
// fields are editable on the Settings page, and a stored setting wins. Clearing
// the field in the GUI reveals this fallback again, and clearing both reveals
// the provider's own default. That ordering — stored > flag > env > built-in —
// is what lets an operator template a systemd unit and still let someone fix a
// blocked user agent from a phone.
func newProvider(cfg config) *quotes.Yahoo {
	return quotes.NewYahoo(quotes.Settings{
		BaseURL:   cfg.quoteBaseURL,
		UserAgent: cfg.quoteUserAgent,
		Timeout:   time.Duration(cfg.quoteTimeout) * time.Second,
	})
}

func serve(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	bindFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(cfg.verbose)

	st, err := store.Open(cfg.db)
	if err != nil {
		return err
	}
	defer st.Close()

	provider := newProvider(cfg)
	eng := engine.New(st, provider, publish.New(), log)

	webHandler, err := web.Handler(cfg.webDist)
	if err != nil {
		return fmt.Errorf("web client: %w", err)
	}

	server := &http.Server{
		Addr: net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)),
		Handler: api.New(api.Options{
			Store:   st,
			Engine:  eng,
			Logger:  log,
			Web:     webHandler,
			Runtime: runtimeInfo(cfg),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a manual refresh can legitimately take longer than
		// a default would allow while it waits on the quote provider. The
		// per-request contexts and the provider's own client timeout bound it.
		IdleTimeout: 120 * time.Second,
	}

	// SIGINT/SIGTERM stops the refresh loop and drains in-flight requests.
	// systemd sends SIGTERM on `systemctl stop`, which is exactly what the
	// quick-start does before snapshotting the database — an unclean stop
	// there would mean snapshotting mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go eng.Start(ctx)

	errs := make(chan error, 1)
	go func() {
		log.Info("tickers listening",
			"version", version.String(), "addr", server.Addr, "db", cfg.db)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

// publishOnce is the original script, preserved as a subcommand: fetch every
// enabled symbol, publish the snapshot, exit. Useful from cron on a host that
// would rather not run a daemon, and useful for testing a destination from a
// shell.
func publishOnce(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	bindFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger(true)

	st, err := store.Open(cfg.db)
	if err != nil {
		return err
	}
	defer st.Close()

	eng := engine.New(st, newProvider(cfg), publish.New(), log)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	run, err := eng.RunCycle(ctx, store.TriggerManual)
	if err != nil {
		return err
	}
	fmt.Printf("%d quotes ok, %d failed\n", run.OKCount, run.ErrorCount)
	for _, p := range run.Publishes {
		if p.OK {
			fmt.Printf("  %-24s %s %d (%d ms)\n", p.SinkName, p.Method, p.StatusCode, p.DurationMS)
		} else {
			fmt.Printf("  %-24s FAILED: %s\n", p.SinkName, p.Error)
		}
	}
	// A cycle where every symbol failed is a failure worth a non-zero exit, so
	// a cron wrapper or a systemd timer notices.
	if run.ErrorCount > 0 && run.OKCount == 0 {
		return errors.New("every symbol failed to fetch")
	}
	return nil
}

// runtimeInfo is the start-up configuration the Settings page shows read-only:
// the things a browser genuinely cannot change about a process that is already
// listening and already has a file open.
func runtimeInfo(cfg config) api.Runtime {
	webSource := "embedded"
	if cfg.webDist != "" {
		webSource = cfg.webDist
	}
	dbPath := cfg.db
	if abs, err := filepath.Abs(dbPath); err == nil {
		dbPath = abs
	}
	return api.Runtime{
		ListenAddr: net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)),
		DBPath:     dbPath,
		WebSource:  webSource,
	}
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
