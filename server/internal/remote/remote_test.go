package remote

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chinmay28/tickers/server/internal/api"
	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/mcptools"
	"github.com/chinmay28/tickers/server/internal/publish"
	"github.com/chinmay28/tickers/server/internal/store"
)

var _ mcptools.Home = (*Client)(nil)

// home is a collecting server's API over a store of its own.
func home(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/home.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(api.New(api.Options{Store: st, Engine: engine.New(st, nil, publish.New(), nil)}).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

const rules = `{"symbol":"SPY","from":"2020-01-01","entry":{"conditions":[{"left":"rsi","op":"<","right":"30"}]}}`

func TestWhatAgentsHandBackGoesHome(t *testing.T) {
	srv, st := home(t)
	c, err := New(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	saved, err := c.SaveStrategy("", "RSI dip", json.RawMessage(rules))
	if err != nil || saved.ID == "" {
		t.Fatalf("save = %+v (%v)", saved, err)
	}
	if _, err := c.SaveStrategy(saved.ID, "RSI dip, renamed", json.RawMessage(rules)); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.Strategies(); len(all) != 1 || all[0].Name != "RSI dip, renamed" {
		t.Errorf("the home store has %+v, want the one strategy, renamed", all)
	}
	if listed, err := c.Strategies(); err != nil || len(listed) != 1 {
		t.Errorf("strategies = %+v (%v)", listed, err)
	}
	if _, err := c.SaveStrategy("nope", "x", json.RawMessage(rules)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("replacing a strategy the home server lacks = %v, want ErrNotFound", err)
	}
	if _, err := c.SaveStrategy("", "Broken", json.RawMessage(`{"symbol":"SPY","from":"2020-01-01","entry":{"conditions":[{"left":"sma:0","op":">","right":"1"}]}}`)); err == nil || !strings.Contains(err.Error(), "entry condition 1") {
		t.Errorf("rules that can't run = %v, want the home server's own sentence", err)
	}

	if _, err := c.CreateReport("Found", "Something.", "agent"); err != nil {
		t.Fatal(err)
	}
	if reports, _ := st.Reports(); len(reports) != 1 || reports[0].Author != "agent" {
		t.Errorf("the home store's reports = %+v", reports)
	}
	if _, err := c.CreateReport("", "x", ""); err == nil {
		t.Error("a report without a title went home")
	}
	if reports, err := c.Reports(); err != nil || len(reports) != 1 {
		t.Errorf("reports = %+v (%v)", reports, err)
	}

	w, err := c.WatchStrategy("RSI dip", json.RawMessage(rules), "from the research server")
	if err != nil || w.Since == "" {
		t.Fatalf("watch = %+v (%v)", w, err)
	}
	fw, err := c.Forward()
	if err != nil || len(fw) != 1 || fw[0].Watch.ID != w.ID || !fw[0].Waiting {
		t.Errorf("forward = %+v (%v), want the watch waiting for its first bar", fw, err)
	}
}

func TestTheClientSaysWhatWentWrong(t *testing.T) {
	if _, err := New("raspberrypi.local:8797"); err == nil {
		t.Error("an address without a scheme was accepted")
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	c, _ := New(down.URL)
	if err := c.Ping(); err == nil || !strings.Contains(err.Error(), "can't be reached") {
		t.Errorf("an unreachable server = %v", err)
	}
	notTickers := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }))
	defer notTickers.Close()
	c, _ = New(notTickers.URL)
	if _, err := c.Reports(); err == nil || !strings.Contains(err.Error(), "is that its address") {
		t.Errorf("a server that isn't tickers = %v", err)
	}

	// Credentials in the address are sent, for a proxy with basic auth in
	// front of the home server — and never repeated in an error.
	var user, pass string
	guarded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		http.Error(w, `{"error":"nope"}`, http.StatusTeapot)
	}))
	defer guarded.Close()
	c, _ = New(strings.Replace(guarded.URL, "http://", "http://me:secret@", 1))
	err := c.Ping()
	if user != "me" || pass != "secret" {
		t.Errorf("basic auth sent as %q/%q, want the address's", user, pass)
	}
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("a refusal = %v, want the server's sentence", err)
	}
	down2, _ := New(strings.Replace(down.URL, "http://", "http://me:secret@", 1))
	if err := down2.Ping(); strings.Contains(err.Error(), "secret") {
		t.Errorf("an error repeated the password: %v", err)
	}
}
