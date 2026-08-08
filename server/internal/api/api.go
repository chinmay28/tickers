// Package api is the HTTP layer: the REST endpoints the web client calls, and
// the handler that serves the client itself.
//
// It is deliberately thin. Validation lives in store, behaviour lives in
// engine; everything here is decoding, status-code selection and encoding.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/version"
)

// Server wires the store and engine to an http.Handler.
type Server struct {
	store     *store.Store
	engine    *engine.Engine
	publisher *publish.Publisher
	log       *slog.Logger
	started   time.Time
	runtime   Runtime
	// web serves the embedded client; nil means API-only.
	web http.Handler
}

// Runtime is the start-up configuration the Settings page shows read-only.
//
// These are the things that genuinely cannot be changed from a browser: the
// address the server is already listening on, and the file it already has
// open. Showing them is most of the value — "which database is this instance
// actually using?" is otherwise a question you answer by reading a unit file.
type Runtime struct {
	ListenAddr string `json:"listenAddr"`
	DBPath     string `json:"dbPath"`
	// WebSource is "embedded" or the --web-dist directory being served.
	WebSource string `json:"webSource"`
}

// Options configures a Server.
type Options struct {
	Store  *store.Store
	Engine *engine.Engine
	Logger *slog.Logger
	// Publisher backs the "test this destination" endpoint. Nil gets a default.
	Publisher *publish.Publisher
	// Web serves the client at every non-/api path. Nil disables it.
	Web http.Handler
	// Runtime is reported by /api/state for the Settings page.
	Runtime Runtime
}

// New builds the API server.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	publisher := opts.Publisher
	if publisher == nil {
		publisher = publish.New()
	}
	return &Server{
		store:     opts.Store,
		engine:    opts.Engine,
		publisher: publisher,
		log:       log,
		started:   time.Now(),
		runtime:   opts.Runtime,
		web:       opts.Web,
	}
}

// Handler returns the routed handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22+ pattern routing: method and wildcards in the pattern, so there
	// is no hand-rolled dispatch on r.Method or path splitting below.
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/state", s.handleState)

	mux.HandleFunc("GET /api/tickers", s.handleListTickers)
	mux.HandleFunc("POST /api/tickers", s.handleCreateTicker)
	mux.HandleFunc("POST /api/tickers/reorder", s.handleReorderTickers)
	mux.HandleFunc("PATCH /api/tickers/{id}", s.handleUpdateTicker)
	mux.HandleFunc("DELETE /api/tickers/{id}", s.handleDeleteTicker)
	mux.HandleFunc("GET /api/tickers/{id}/history", s.handleTickerHistory)
	mux.HandleFunc("GET /api/tickers/{id}/performance", s.handleTickerPerformance)

	mux.HandleFunc("GET /api/portfolios", s.handleListPortfolios)
	mux.HandleFunc("POST /api/portfolios", s.handleCreatePortfolio)
	mux.HandleFunc("PATCH /api/portfolios/{id}", s.handleUpdatePortfolio)
	mux.HandleFunc("DELETE /api/portfolios/{id}", s.handleDeletePortfolio)
	mux.HandleFunc("POST /api/portfolios/{id}/backtest", s.handleBacktestPortfolio)
	mux.HandleFunc("POST /api/backtest", s.handleBacktest)

	mux.HandleFunc("GET /api/sinks", s.handleListSinks)
	mux.HandleFunc("POST /api/sinks", s.handleCreateSink)
	mux.HandleFunc("PATCH /api/sinks/{id}", s.handleUpdateSink)
	mux.HandleFunc("DELETE /api/sinks/{id}", s.handleDeleteSink)
	mux.HandleFunc("POST /api/sinks/{id}/test", s.handleTestSink)

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PATCH /api/settings", s.handlePatchSettings)
	mux.HandleFunc("POST /api/provider/test", s.handleTestProvider)

	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/publish", s.handlePublish)
	mux.HandleFunc("GET /api/preview", s.handlePreview)
	mux.HandleFunc("GET /api/search", s.handleSearch)

	// Any /api path or method that didn't match above. Without this the
	// catch-all below would answer `PUT /api/tickers` with the HTML shell,
	// and a client would try to JSON-parse a web page.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})

	// Anything not under /api is the web client (or a 404 if it isn't built in).
	if s.web != nil {
		mux.Handle("/", s.web)
	}

	return s.withLogging(s.withCommonHeaders(mux))
}

// ---------------------------------------------------------------------------
// Health & state
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Health has to fail when the database is gone, or a rolled-forward binary
	// pointed at an unreadable data directory would report itself healthy and
	// the quick-start's rollback would never trigger.
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":  "error",
			"version": version.String(),
			"error":   "database unavailable: " + err.Error(),
		})
		return
	}
	migrations, err := s.store.AppliedMigrations()
	if err != nil {
		migrations = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"version":    version.String(),
		"uptimeSec":  int64(time.Since(s.started).Seconds()),
		"migrations": migrations,
	})
}

// stateResponse is everything the client needs to render, in one round trip.
// The UI polls this; a page that had to fan out to five endpoints to redraw
// would spend its life half-updated.
type stateResponse struct {
	Version string       `json:"version"`
	Tickers []tickerView `json:"tickers"`
	Sinks   []store.Sink `json:"sinks"`
	// Portfolios are the saved allocations, not their results. A backtest is
	// slow and large; it is asked for by name when a page wants one, and would
	// turn a ten-second poll into a fan of upstream requests if it rode along.
	Portfolios []store.Portfolio `json:"portfolios"`
	Settings   store.Config      `json:"settings"`
	Engine     engine.Status     `json:"engine"`
	Preview    map[string]any    `json:"preview"`
	Runtime    Runtime           `json:"runtime"`
	// Provider is the quote source's effective settings, with every default
	// resolved — nil for a provider that can't be reconfigured, which is what
	// the UI keys off to hide those fields.
	Provider *providerView  `json:"provider"`
	Meta     map[string]any `json:"meta"`
}

// providerView is what the Settings page renders for the quote source: the
// values in force right now, so an empty override field can show the default
// it is falling back to as its placeholder.
type providerView struct {
	Name             string `json:"name"`
	BaseURL          string `json:"baseUrl"`
	UserAgent        string `json:"userAgent"`
	TimeoutSeconds   int    `json:"timeoutSeconds"`
	DefaultBaseURL   string `json:"defaultBaseUrl"`
	DefaultUserAgent string `json:"defaultUserAgent"`
	DefaultTimeout   int    `json:"defaultTimeoutSeconds"`
}

func (s *Server) providerView() *providerView {
	effective, ok := s.engine.ProviderSettings()
	if !ok {
		return nil
	}
	return &providerView{
		Name:             s.engine.Provider().Name(),
		BaseURL:          effective.BaseURL,
		UserAgent:        effective.UserAgent,
		TimeoutSeconds:   int(effective.Timeout / time.Second),
		DefaultBaseURL:   quotes.DefaultBaseURL,
		DefaultUserAgent: quotes.DefaultUserAgent,
		DefaultTimeout:   int(quotes.DefaultTimeout / time.Second),
	}
}

// tickerView is a ticker with its latest reading attached — the shape the
// watchlist actually renders, assembled here so the client doesn't have to
// join two collections on every repaint.
type tickerView struct {
	store.Ticker
	Quote     *store.Quote `json:"quote"`
	Change    *float64     `json:"change"`
	ChangePct *float64     `json:"changePercent"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	views, err := s.tickerViews()
	if err != nil {
		s.fail(w, err)
		return
	}
	sinks, err := s.store.Sinks()
	if err != nil {
		s.fail(w, err)
		return
	}
	portfolios, err := s.store.Portfolios()
	if err != nil {
		s.fail(w, err)
		return
	}
	cfg, err := s.store.Config()
	if err != nil {
		s.fail(w, err)
		return
	}

	// The preview is the exact payload the default format would publish right
	// now. Showing it next to the destination list is the cheapest way to make
	// "what does my home automation actually receive?" answerable without
	// reading the source.
	preview := map[string]any{}
	if snap, err := s.engine.Snapshot(); err == nil {
		preview = publish.Payload(snap, store.FormatMinion)
	}

	writeJSON(w, http.StatusOK, stateResponse{
		Version:    version.String(),
		Tickers:    views,
		Sinks:      sinks,
		Portfolios: portfolios,
		Settings:   cfg,
		Engine:     s.engine.Status(),
		Preview:    preview,
		Runtime:    s.runtime,
		Provider:   s.providerView(),
		Meta: map[string]any{
			"minRefreshSeconds": store.MinRefreshSeconds,
			"minQuoteTimeout":   store.MinQuoteTimeout,
			"maxQuoteTimeout":   store.MaxQuoteTimeout,
			"maxPinnedSymbols":  store.MaxPinnedSymbols,
			"maxHoldings":       store.MaxHoldings,
			"formats":           []string{store.FormatMinion, store.FormatDetailed},
			"rebalances": []string{
				store.RebalanceNone, store.RebalanceAnnually,
				store.RebalanceQuarterly, store.RebalanceMonthly,
			},
			"seedSymbols": store.SeedSymbols,
		},
	})
}

func (s *Server) tickerViews() ([]tickerView, error) {
	tickers, err := s.store.Tickers()
	if err != nil {
		return nil, err
	}
	quotes, err := s.store.Quotes()
	if err != nil {
		return nil, err
	}

	views := make([]tickerView, 0, len(tickers))
	for _, t := range tickers {
		v := tickerView{Ticker: t}
		if q, ok := quotes[t.ID]; ok {
			quote := q
			v.Quote = &quote
			if change, ok := q.Change(); ok {
				v.Change = &change
			}
			if pct, ok := q.ChangePercent(); ok {
				v.ChangePct = &pct
			}
		}
		views = append(views, v)
	}
	return views, nil
}

// ---------------------------------------------------------------------------
// Tickers
// ---------------------------------------------------------------------------

func (s *Server) handleListTickers(w http.ResponseWriter, r *http.Request) {
	views, err := s.tickerViews()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickers": views})
}

type tickerBody struct {
	Symbol *string `json:"symbol"`
	// Expression makes the row a composite priced from a formula over other
	// symbols. It is an alternative to Symbol, not an addition to it — and a
	// Symbol that reads as a formula is treated as one, so a client that only
	// knows about the symbol field can still add "VTI/GLD".
	Expression *string `json:"expression"`
	Label      *string `json:"label"`
	Enabled    *bool   `json:"enabled"`
}

func (s *Server) handleCreateTicker(w http.ResponseWriter, r *http.Request) {
	var body tickerBody
	if !decode(w, r, &body) {
		return
	}
	if body.Symbol == nil && body.Expression == nil {
		writeError(w, http.StatusBadRequest, "symbol is required")
		return
	}
	in := store.NewTicker{Enabled: body.Enabled}
	if body.Symbol != nil {
		in.Symbol = *body.Symbol
	}
	if body.Expression != nil {
		in.Expression = *body.Expression
	}
	if body.Label != nil {
		in.Label = *body.Label
	}

	t, err := s.store.CreateTicker(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Price it right away rather than making the user stare at "—" until the
	// next scheduled poll. Errors don't fail the create: the ticker is on the
	// list either way, and the next cycle will try again.
	s.engine.Kick()
	if _, err := s.engine.RunCycle(r.Context(), store.TriggerManual); err != nil {
		s.log.Warn("refresh after adding ticker failed", "symbol", t.Symbol, "error", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"ticker": t})
}

func (s *Server) handleUpdateTicker(w http.ResponseWriter, r *http.Request) {
	var body tickerBody
	if !decode(w, r, &body) {
		return
	}
	t, err := s.store.UpdateTicker(r.PathValue("id"), store.TickerPatch{
		Symbol:     body.Symbol,
		Expression: body.Expression,
		Label:      body.Label,
		Enabled:    body.Enabled,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	// A changed symbol or formula dropped its stale quote (see
	// store.UpdateTicker), so refill it now — otherwise the row sits blank
	// until the next poll.
	if body.Symbol != nil || body.Expression != nil {
		if _, err := s.engine.RunCycle(r.Context(), store.TriggerManual); err != nil {
			s.log.Warn("refresh after replacing ticker failed", "symbol", t.Symbol, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticker": t})
}

func (s *Server) handleDeleteTicker(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteTicker(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReorderTickers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.store.ReorderTickers(body.IDs); err != nil {
		s.fail(w, err)
		return
	}
	views, err := s.tickerViews()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickers": views})
}

func (s *Server) handleTickerHistory(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.Ticker(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	points, err := s.store.History(t.Symbol, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"symbol": t.Symbol, "points": points})
}

// handleTickerPerformance answers the watchlist's history sheet: the daily
// series behind a row and its returns over the usual windows.
//
// Unlike /history, which reads the stored sparkline points, this one goes
// upstream — the stored series is pruned to a window measured in hours and can
// never answer "how has this done in five years". It is therefore the one
// read-only endpoint that can be slow, and the engine caches per symbol so a
// repeated double-tap doesn't repeat the fetch.
func (s *Server) handleTickerPerformance(w http.ResponseWriter, r *http.Request) {
	perf, err := s.engine.Performance(r.Context(), r.PathValue("id"))
	if err != nil {
		// A provider that can only price today is a configuration a user chose,
		// not a fault: say what is missing rather than logging a 500.
		if errors.Is(err, quotes.ErrNoHistory) {
			writeError(w, http.StatusNotImplemented, err.Error())
			return
		}
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"performance": perf})
}

// ---------------------------------------------------------------------------
// Portfolios
// ---------------------------------------------------------------------------

func (s *Server) handleListPortfolios(w http.ResponseWriter, r *http.Request) {
	portfolios, err := s.store.Portfolios()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolios": portfolios})
}

// portfolioBody is both the create/patch payload and the ad-hoc backtest
// payload, which is deliberate: "run this without saving it" and "save this"
// take exactly the same fields, and a client that has built one has built the
// other.
type portfolioBody struct {
	Name          *string          `json:"name"`
	Holdings      *[]store.Holding `json:"holdings"`
	InitialAmount *float64         `json:"initialAmount"`
	StartYear     *int             `json:"startYear"`
	EndYear       *int             `json:"endYear"`
	Rebalance     *string          `json:"rebalance"`
	Benchmark     *string          `json:"benchmark"`
}

func (s *Server) handleCreatePortfolio(w http.ResponseWriter, r *http.Request) {
	var body portfolioBody
	if !decode(w, r, &body) {
		return
	}
	in := store.NewPortfolio{}
	if body.Name != nil {
		in.Name = *body.Name
	}
	if body.Holdings != nil {
		in.Holdings = *body.Holdings
	}
	if body.InitialAmount != nil {
		in.InitialAmount = *body.InitialAmount
	}
	if body.StartYear != nil {
		in.StartYear = *body.StartYear
	}
	if body.EndYear != nil {
		in.EndYear = *body.EndYear
	}
	if body.Rebalance != nil {
		in.Rebalance = *body.Rebalance
	}
	if body.Benchmark != nil {
		in.Benchmark = *body.Benchmark
	}

	p, err := s.store.CreatePortfolio(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"portfolio": p})
}

func (s *Server) handleUpdatePortfolio(w http.ResponseWriter, r *http.Request) {
	var body portfolioBody
	if !decode(w, r, &body) {
		return
	}
	p, err := s.store.UpdatePortfolio(r.PathValue("id"), store.PortfolioPatch{
		Name:          body.Name,
		Holdings:      body.Holdings,
		InitialAmount: body.InitialAmount,
		StartYear:     body.StartYear,
		EndYear:       body.EndYear,
		Rebalance:     body.Rebalance,
		Benchmark:     body.Benchmark,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolio": p})
}

func (s *Server) handleDeletePortfolio(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeletePortfolio(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleBacktestPortfolio runs a saved allocation.
//
// It is a POST rather than a GET even though it changes nothing: it fans out to
// one full-history request per holding the first time it runs, and a URL a
// browser is free to prefetch, retry or cache is the wrong shape for that.
func (s *Server) handleBacktestPortfolio(w http.ResponseWriter, r *http.Request) {
	result, err := s.engine.BacktestPortfolio(r.Context(), r.PathValue("id"))
	if err != nil {
		s.failBacktest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backtest": result})
}

// handleBacktest runs an allocation that has not been saved — the editor's
// "try it" path, so nobody has to commit a portfolio to the database to find
// out whether the symbols in it have any overlapping history.
func (s *Server) handleBacktest(w http.ResponseWriter, r *http.Request) {
	var body portfolioBody
	if !decode(w, r, &body) {
		return
	}
	spec := engine.BacktestSpec{}
	if body.Holdings != nil {
		spec.Holdings = *body.Holdings
	}
	if body.InitialAmount != nil {
		spec.InitialAmount = *body.InitialAmount
	}
	if body.StartYear != nil {
		spec.StartYear = *body.StartYear
	}
	if body.EndYear != nil {
		spec.EndYear = *body.EndYear
	}
	if body.Rebalance != nil {
		spec.Rebalance = *body.Rebalance
	}
	if body.Benchmark != nil {
		spec.Benchmark = *body.Benchmark
	}

	result, err := s.engine.Backtest(r.Context(), spec)
	if err != nil {
		s.failBacktest(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backtest": result})
}

// failBacktest maps the two failures a backtest has that nothing else does: a
// quote source that can only price today, and a portfolio whose own contents
// make it unrunnable.
func (s *Server) failBacktest(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, quotes.ErrNoHistory):
		// Not a fault — a provider that can only price today is a configuration
		// somebody chose. Say what is missing, as the performance sheet does.
		writeError(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, engine.ErrBadSpec):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.fail(w, err)
	}
}

// ---------------------------------------------------------------------------
// Sinks
// ---------------------------------------------------------------------------

func (s *Server) handleListSinks(w http.ResponseWriter, r *http.Request) {
	sinks, err := s.store.Sinks()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sinks": sinks})
}

type sinkBody struct {
	Name      *string `json:"name"`
	BaseURL   *string `json:"baseUrl"`
	Key       *string `json:"key"`
	Category  *string `json:"category"`
	Format    *string `json:"format"`
	Enabled   *bool   `json:"enabled"`
	TimeoutMS *int    `json:"timeoutMs"`
}

func (s *Server) handleCreateSink(w http.ResponseWriter, r *http.Request) {
	var body sinkBody
	if !decode(w, r, &body) {
		return
	}
	in := store.NewSink{Enabled: body.Enabled}
	if body.Name != nil {
		in.Name = *body.Name
	}
	if body.BaseURL != nil {
		in.BaseURL = *body.BaseURL
	}
	if body.Key != nil {
		in.Key = *body.Key
	}
	if body.Category != nil {
		in.Category = *body.Category
	}
	if body.Format != nil {
		in.Format = *body.Format
	}
	if body.TimeoutMS != nil {
		in.TimeoutMS = *body.TimeoutMS
	}

	sink, err := s.store.CreateSink(in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sink": sink})
}

func (s *Server) handleUpdateSink(w http.ResponseWriter, r *http.Request) {
	var body sinkBody
	if !decode(w, r, &body) {
		return
	}
	sink, err := s.store.UpdateSink(r.PathValue("id"), store.SinkPatch{
		Name:      body.Name,
		BaseURL:   body.BaseURL,
		Key:       body.Key,
		Category:  body.Category,
		Format:    body.Format,
		Enabled:   body.Enabled,
		TimeoutMS: body.TimeoutMS,
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sink": sink})
}

func (s *Server) handleDeleteSink(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSink(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestSink publishes the current snapshot to one sink, enabled or not,
// and reports the outcome. This is the "does my endpoint actually accept
// this?" button, and it deliberately sends the real payload rather than a
// synthetic one — a test that doesn't exercise the real body isn't a test.
func (s *Server) handleTestSink(w http.ResponseWriter, r *http.Request) {
	sink, err := s.store.Sink(r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	snap, err := s.engine.Snapshot()
	if err != nil {
		s.fail(w, err)
		return
	}
	result := s.publisher.Publish(r.Context(), sink, snap)
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// ---------------------------------------------------------------------------
// Settings, runs, actions
// ---------------------------------------------------------------------------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.Config()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":          cfg,
		"provider":          s.providerView(),
		"minRefreshSeconds": store.MinRefreshSeconds,
		"minQuoteTimeout":   store.MinQuoteTimeout,
		"maxQuoteTimeout":   store.MaxQuoteTimeout,
		"maxPinnedSymbols":  store.MaxPinnedSymbols,
	})
}

func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var patch store.ConfigPatch
	if !decode(w, r, &patch) {
		return
	}
	cfg, err := s.store.UpdateConfig(patch)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Push the quote-source fields into the provider straight away, so the
	// response already reflects what the next request will use — and nudge the
	// loop so a shortened interval takes effect from now rather than after the
	// old, longer wait finishes.
	s.engine.ApplyConfig(cfg)
	s.engine.Kick()
	writeJSON(w, http.StatusOK, map[string]any{"settings": cfg, "provider": s.providerView()})
}

// handleTestProvider fetches one symbol through the settings as they stand and
// reports the outcome verbatim.
//
// This is the Settings page's "Test connection", and it is the fastest way to
// tell the three failures apart that all look identical from the watchlist: a
// wrong base URL, a network that blocks the provider, and a symbol that simply
// doesn't exist. It always answers 200 — the *result* carries the failure,
// because a 502 here would just be a second error to interpret.
func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Symbol string `json:"symbol"`
	}
	// An empty body is fine: the engine falls back to a known-good symbol.
	if r.ContentLength > 0 && !decode(w, r, &body) {
		return
	}

	start := time.Now()
	quote, err := s.engine.CheckProvider(r.Context(), body.Symbol)
	result := map[string]any{
		"provider":   s.engine.Provider().Name(),
		"durationMs": time.Since(start).Milliseconds(),
		"settings":   s.providerView(),
	}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
	} else {
		result["ok"] = true
		result["symbol"] = quote.Symbol
		result["price"] = quote.Price
		result["currency"] = quote.Currency
		result["name"] = quote.ShortName
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.store.Runs(limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	run, err := s.engine.RunCycle(r.Context(), store.TriggerManual)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.engine.Kick()
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	results, err := s.engine.Publish(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handlePreview renders the payload that would be published, without sending
// it anywhere.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = store.FormatMinion
	}
	if format != store.FormatMinion && format != store.FormatDetailed {
		writeError(w, http.StatusBadRequest, `format must be "minion" or "detailed"`)
		return
	}
	snap, err := s.engine.Snapshot()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"format":  format,
		"payload": publish.Payload(snap, format),
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, map[string]any{"matches": []any{}})
		return
	}
	matches, err := s.engine.Provider().Search(r.Context(), query)
	if err != nil {
		// Search is a convenience on top of a third-party endpoint that can be
		// down, rate-limited, or blocked by the network the Pi is on. Failing
		// it as a 502 would block adding a ticker; instead say so and let the
		// user type the symbol, which always works.
		writeJSON(w, http.StatusOK, map[string]any{
			"matches": []any{},
			"warning": "symbol search is unavailable (" + err.Error() + ") — type the exact symbol instead",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// maxRequestBody bounds decoded request bodies. Every payload this API takes
// is a few hundred bytes; the limit exists so a stray upload can't be buffered
// into a Raspberry Pi's memory.
const maxRequestBody = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	// Reject unknown fields: a client sending `{"symbl": "VTI"}` should be
	// told, not silently given a ticker with an empty symbol.
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// fail maps a domain error to a status code. Everything the store can
// meaningfully blame on the caller gets a 4xx; anything else is ours.
func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrDuplicateSymbol):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalidExpression):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrInvalidPortfolio):
		writeError(w, http.StatusBadRequest, err.Error())
	case isValidationError(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("request failed", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// isValidationError recognises the plain errors.New values the store returns
// for bad input. They have no sentinel of their own because each is a
// one-line, user-facing sentence; matching on shape keeps the store's
// validation readable at the cost of this one list.
func isValidationError(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"is required", "must be", "cannot be", "cannot contain", "not a valid", "needs a host",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func (s *Server) withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The API is state-changing and unauthenticated by design (see
		// README: run it on a trusted network). Refusing to be framed and
		// refusing MIME sniffing are the two headers that still buy something
		// in that model.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static asset requests would drown the journal; only the API is logged.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Debug("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}
