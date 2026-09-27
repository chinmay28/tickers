package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// What agents hand back — reports, and strategies frozen to be judged on
// what came after — shown on the Strategies page.
func (s *Server) routeResearch(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/research", s.handleResearch)
	mux.HandleFunc("POST /api/reports", s.handleCreateReport)
	mux.HandleFunc("DELETE /api/reports/{id}", s.handleDeleteReport)
	mux.HandleFunc("POST /api/watches", s.handleCreateWatch)
	mux.HandleFunc("DELETE /api/watches/{id}", s.handleDeleteWatch)
}

// watchView is a watch and its forward test. Result is the backtest from the
// watch's first day on, nil while it waits for bars or when it failed.
type watchView struct {
	store.Watch
	Waiting bool             `json:"waiting"`
	Error   string           `json:"error,omitempty"`
	Result  *strategy.Result `json:"result"`
}

func (s *Server) handleResearch(w http.ResponseWriter, r *http.Request) {
	reports, err := s.store.Reports()
	if err != nil {
		s.fail(w, err)
		return
	}
	forward, err := s.engine.Forward()
	if err != nil {
		s.fail(w, err)
		return
	}
	watches := make([]watchView, len(forward))
	for i, f := range forward {
		watches[i] = watchView{Watch: f.Watch, Waiting: f.Waiting, Result: f.Result}
		if f.Err != nil {
			watches[i].Error = f.Err.Error()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": reports, "watches": watches})
}

// handleCreateReport takes a report from a research server working on a copy
// of the archive elsewhere, so what it found is read here, where the person
// looks.
func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title  string `json:"title"`
		Body   string `json:"body"`
		Author string `json:"author"`
	}
	if !decode(w, r, &body) {
		return
	}
	report, err := s.store.CreateReport(body.Title, body.Body, body.Author)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, report)
}

func (s *Server) handleDeleteReport(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteReport(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateWatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string          `json:"name"`
		Definition json.RawMessage `json:"definition"`
		Note       string          `json:"note"`
	}
	if !decode(w, r, &body) {
		return
	}
	watch, err := s.engine.WatchStrategy(body.Name, body.Definition, body.Note, time.Now())
	switch {
	case strategy.IsInvalid(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusCreated, watch)
	}
}

func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteWatch(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
