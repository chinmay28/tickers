package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/archive"
	"github.com/chinmay28/tickers/server/internal/archiver"
	"github.com/chinmay28/tickers/server/internal/collector"
	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/store"
)

// The archive's endpoints back the Data page. They are separate from
// /api/state on purpose: the state poll runs every ten seconds on every open
// tab, and nothing about the watchlist should pay for counting a billion bars.

func (s *Server) routeArchive(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/archive", s.handleArchive)
	mux.HandleFunc("PATCH /api/archive/settings", s.handleArchiveSettings)
	mux.HandleFunc("GET /api/archive/folder", s.handleInspectFolder)
	mux.HandleFunc("POST /api/archive/folder", s.handleSetFolder)
	mux.HandleFunc("GET /api/archive/symbols", s.handleArchiveSymbols)
	mux.HandleFunc("POST /api/archive/symbols", s.handleAddArchiveSymbol)
	mux.HandleFunc("GET /api/archive/symbols/{symbol}", s.handleArchiveSymbol)
	mux.HandleFunc("PATCH /api/archive/symbols/{symbol}", s.handlePatchArchiveSymbol)
	mux.HandleFunc("GET /api/archive/symbols/{symbol}/bars", s.handleArchiveBars)
	mux.HandleFunc("POST /api/archive/symbols/{symbol}/fetch", s.handleArchiveFetch)
	mux.HandleFunc("POST /api/archive/symbols/{symbol}/reset", s.handleArchiveReset)
}

// archiveView is GET /api/archive: the manager's status and the settings.
type archiveView struct {
	archiver.Status
	Settings store.ArchiveConfig `json:"settings"`
	// Sources are the names bars can carry, for the fetch form.
	Sources   []string `json:"sources"`
	Intervals []string `json:"intervals"`
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		writeError(w, http.StatusNotImplemented, "this server was started without the market-data archive")
		return
	}
	cfg, err := s.store.ArchiveConfig()
	if err != nil {
		s.fail(w, err)
		return
	}
	sources := []string{collector.YahooName}
	if cfg.PolygonKeySet {
		sources = append(sources, quotes.PolygonName)
	}
	writeJSON(w, http.StatusOK, archiveView{
		Status:    s.archive.Status(r.URL.Query().Get("fresh") == "1"),
		Settings:  cfg,
		Sources:   sources,
		Intervals: store.ArchiveIntervals,
	})
}

func (s *Server) handleArchiveSettings(w http.ResponseWriter, r *http.Request) {
	if s.noArchive(w) {
		return
	}
	var patch store.ArchivePatch
	if !decode(w, r, &patch) {
		return
	}
	cfg, err := s.store.UpdateArchiveConfig(patch)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.archive.Nudge()
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleInspectFolder(w http.ResponseWriter, r *http.Request) {
	if s.noArchive(w) {
		return
	}
	writeJSON(w, http.StatusOK, archiver.Inspect(strings.TrimSpace(r.URL.Query().Get("path"))))
}

// handleSetFolder chooses where the archive lives. "use" opens the folder —
// starting a new archive there if it isn't one — and leaves the old folder
// alone; "move" copies the open archive into an empty folder and switches to
// it; "detach" forgets the stored folder, falling back to the startup flag.
func (s *Server) handleSetFolder(w http.ResponseWriter, r *http.Request) {
	if s.noArchive(w) {
		return
	}
	var body struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	var err error
	switch body.Mode {
	case "use":
		err = s.archive.Use(strings.TrimSpace(body.Path))
	case "move":
		err = s.archive.MoveTo(strings.TrimSpace(body.Path))
	case "detach":
		err = s.archive.Detach()
	default:
		writeError(w, http.StatusBadRequest, `mode must be "use", "move" or "detach"`)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.archive.Status(false))
}

func (s *Server) handleArchiveSymbols(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	var page archive.SymbolPage
	err := s.readArchive(w, func(a *archive.Archive) error {
		var err error
		page, err = a.QuerySymbols(archive.SymbolQuery{
			Text: q.Get("q"), Filter: q.Get("filter"), Kind: q.Get("kind"), Offset: offset, Limit: limit,
		})
		return err
	})
	if err == nil {
		writeJSON(w, http.StatusOK, page)
	}
}

// maxSymbolLen bounds a hand-added symbol. Yahoo's longest real ones — a
// future with an exchange suffix — are well under it.
const maxSymbolLen = 24

func (s *Server) handleAddArchiveSymbol(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Symbol string `json:"symbol"`
		Name   string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	symbol := archive.NormalizeSymbol(body.Symbol)
	if err := validSymbol(symbol); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var added archive.Symbol
	err := s.readArchive(w, func(a *archive.Archive) error {
		if err := a.Add(archive.User, archive.Entry{Symbol: symbol, Name: strings.TrimSpace(body.Name)}, time.Now()); err != nil {
			return err
		}
		// Adding a symbol that was excluded means wanting it back.
		if err := a.SetExcluded(symbol, false); err != nil {
			return err
		}
		var err error
		added, err = a.Lookup(symbol)
		return err
	})
	if err == nil {
		s.archive.Nudge()
		writeJSON(w, http.StatusCreated, added)
	}
}

func validSymbol(symbol string) error {
	if symbol == "" {
		return errors.New("a symbol is required")
	}
	if len(symbol) > maxSymbolLen {
		return errors.New("that is too long to be a symbol")
	}
	for _, r := range symbol {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-^=", r)) {
			return errors.New("a symbol can only contain letters, digits and . - ^ =")
		}
	}
	return nil
}

func (s *Server) handleArchiveSymbol(w http.ResponseWriter, r *http.Request) {
	var cov archive.Coverage
	err := s.readArchive(w, func(a *archive.Archive) error {
		var err error
		cov, err = a.SymbolCoverage(r.PathValue("symbol"))
		return err
	})
	if err == nil {
		writeJSON(w, http.StatusOK, cov)
	}
}

// handlePatchArchiveSymbol excludes, prioritises, or takes a symbol off the
// hand-added list. Nothing here deletes history: an excluded symbol's bars
// stay, and including it again resumes where its cursors left off.
func (s *Server) handlePatchArchiveSymbol(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Excluded *bool `json:"excluded"`
		Priority *bool `json:"priority"`
		Added    *bool `json:"added"`
	}
	if !decode(w, r, &body) {
		return
	}
	symbol := r.PathValue("symbol")
	var out archive.Symbol
	err := s.readArchive(w, func(a *archive.Archive) error {
		if body.Excluded != nil {
			if err := a.SetExcluded(symbol, *body.Excluded); err != nil {
				return err
			}
		}
		if body.Priority != nil {
			if err := a.SetPriority(symbol, *body.Priority); err != nil {
				return err
			}
		}
		if body.Added != nil && !*body.Added {
			if err := a.Remove(archive.User, symbol); err != nil {
				return err
			}
		}
		var err error
		out, err = a.Lookup(symbol)
		return err
	})
	if err == nil {
		s.archive.Nudge()
		writeJSON(w, http.StatusOK, out)
	}
}

// maxBars bounds one bars response. A chart has a few thousand pixels; a
// request for more than this is asking for the wrong interval.
const maxBars = 20_000

func (s *Server) handleArchiveBars(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	interval, err := quotes.ParseInterval(q.Get("interval"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	from, to, err := window(q.Get("from"), q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var bars []quotes.Candle
	err = s.readArchive(w, func(a *archive.Archive) error {
		var err error
		bars, err = a.Bars(r.PathValue("symbol"), interval, from, to)
		return err
	})
	if err != nil {
		return
	}
	if len(bars) > maxBars {
		writeError(w, http.StatusBadRequest, "that is more than "+strconv.Itoa(maxBars)+" bars; ask for a shorter window or a wider interval")
		return
	}
	type bar struct {
		T int64   `json:"t"`
		O float64 `json:"o"`
		H float64 `json:"h"`
		L float64 `json:"l"`
		C float64 `json:"c"`
		V int64   `json:"v"`
	}
	out := make([]bar, len(bars))
	for i, b := range bars {
		out[i] = bar{b.Time.Unix(), b.Open, b.High, b.Low, b.Close, b.Volume}
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": interval, "bars": out})
}

// window parses a from/to pair of YYYY-MM-DD dates, to exclusive of the day
// after — "to 2024-03-31" includes March 31st.
func window(fromRaw, toRaw string) (time.Time, time.Time, error) {
	from, err := time.Parse(time.DateOnly, fromRaw)
	if err != nil {
		return from, from, errors.New("from must be a date like 2024-01-31")
	}
	to, err := time.Parse(time.DateOnly, toRaw)
	if err != nil {
		return from, from, errors.New("to must be a date like 2024-01-31")
	}
	to = to.AddDate(0, 0, 1)
	if !from.Before(to) {
		return from, to, errors.New("from must be on or before to")
	}
	return from, to, nil
}

// handleArchiveFetch queues a hand-requested window from one source.
func (s *Server) handleArchiveFetch(w http.ResponseWriter, r *http.Request) {
	if s.noArchive(w) {
		return
	}
	var body struct {
		Interval string `json:"interval"`
		Source   string `json:"source"`
		From     string `json:"from"`
		To       string `json:"to"`
		Replace  bool   `json:"replace"`
	}
	if !decode(w, r, &body) {
		return
	}
	interval, err := quotes.ParseInterval(body.Interval)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	from, to, err := window(body.From, body.To)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if now := time.Now(); to.After(now) {
		to = now
	}
	job, err := s.archive.Enqueue(collector.Job{
		Symbol: r.PathValue("symbol"), Interval: interval, Source: body.Source, From: from, To: to, Replace: body.Replace,
	})
	switch {
	case errors.Is(err, archiver.ErrNotOpen):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, archive.ErrUnknownSymbol):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

// handleArchiveReset forgets a symbol's cursors, so every source walks it
// again from the start. Bars are kept — a walk skips the days it holds — so
// this is how to have a newly added source look at a symbol it already
// finished, or to retry one that failed for a week.
func (s *Server) handleArchiveReset(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Interval string `json:"interval"`
	}
	if !decode(w, r, &body) {
		return
	}
	var interval quotes.Interval
	if body.Interval != "" {
		var err error
		if interval, err = quotes.ParseInterval(body.Interval); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	err := s.readArchive(w, func(a *archive.Archive) error {
		sym, err := a.Lookup(r.PathValue("symbol"))
		if err != nil {
			return err
		}
		return a.ResetCursors(sym.ID, interval)
	})
	if err == nil {
		s.archive.Nudge()
		writeJSON(w, http.StatusOK, map[string]any{"reset": true})
	}
}

// noArchive answers 501 for a server started without one.
func (s *Server) noArchive(w http.ResponseWriter) bool {
	if s.archive == nil {
		writeError(w, http.StatusNotImplemented, "this server was started without the market-data archive")
		return true
	}
	return false
}

// readArchive runs fn against the open archive and writes the error response
// itself, so a handler only has to write its success.
func (s *Server) readArchive(w http.ResponseWriter, fn func(a *archive.Archive) error) error {
	if s.noArchive(w) {
		return errors.New("no archive")
	}
	err := s.archive.Read(fn)
	switch {
	case err == nil:
	case errors.Is(err, archiver.ErrNotOpen):
		writeError(w, http.StatusConflict, "the archive isn't open — choose a folder, or plug its drive back in")
	case errors.Is(err, archive.ErrUnknownSymbol):
		writeError(w, http.StatusNotFound, err.Error())
	case isValidationError(err), strings.Contains(err.Error(), "unknown filter"), strings.Contains(err.Error(), "unknown interval"):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.log.Error("archive request failed", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
	return err
}
