package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/chinmay28/tickers/server/internal/archiver"
	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

func (s *Server) routeStrategies(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/strategies", s.handleListStrategies)
	mux.HandleFunc("POST /api/strategies", s.handleSaveStrategy)
	mux.HandleFunc("PUT /api/strategies/{id}", s.handleSaveStrategy)
	mux.HandleFunc("DELETE /api/strategies/{id}", s.handleDeleteStrategy)
	mux.HandleFunc("POST /api/strategies/run", s.handleRunStrategy)
}

func (s *Server) handleListStrategies(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.Strategies()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"strategies": all})
}

func (s *Server) handleSaveStrategy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string          `json:"name"`
		Definition json.RawMessage `json:"definition"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	st, err := s.engine.SaveStrategy(id, body.Name, body.Definition)
	switch {
	case strategy.IsInvalid(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.fail(w, err)
	case id == "":
		writeJSON(w, http.StatusCreated, st)
	default:
		writeJSON(w, http.StatusOK, st)
	}
}

func (s *Server) handleDeleteStrategy(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteStrategy(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunStrategy backtests the rules it is sent, saved or not — the editor
// runs what is on screen, so trying a change doesn't mean saving it first.
func (s *Server) handleRunStrategy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Definition strategy.Definition `json:"definition"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := s.engine.RunStrategy(body.Definition)
	switch {
	case strategy.IsInvalid(err):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, engine.ErrNoArchive), errors.Is(err, archiver.ErrNotOpen):
		writeError(w, http.StatusConflict, "backtests read the market-data archive, which isn't open — switch it on and choose a folder in Settings")
	case errors.Is(err, engine.ErrNoBars):
		// The request was fine; the data isn't there yet. Its message says
		// what to do about it.
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}
