// Package remote is a research server's line home: the collecting server's
// REST API, used as the mcptools.Home where agents' strategies, reports and
// watches are kept.
//
// A research server works on a copy of the archive somewhere with more
// compute than a Pi, but the person reads what agents found in the app on
// the Pi, and a forward test has to be run where today's bars arrive. So
// what agents hand back goes home over the same endpoints the web client
// uses, and forward tests are read back from there. The record of trials
// stays with the research server: it is about the searching, which happened
// there.
package remote

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chinmay28/tickers/server/internal/engine"
	"github.com/chinmay28/tickers/server/internal/store"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// Client talks to a collecting server.
type Client struct {
	base *url.URL
	http *http.Client
}

// timeout bounds one call. Reading forward tests reruns every watched
// strategy on the Pi, which takes seconds, not minutes.
const timeout = 2 * time.Minute

// New returns a client for the server at base, such as
// http://raspberrypi.local:8797. Credentials in the URL — for a proxy with
// basic authentication in front — are sent with every request.
func New(base string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("the home server must be an http(s) address like http://raspberrypi.local:8797, not %q", base)
	}
	return &Client{base: u, http: &http.Client{Timeout: timeout}}, nil
}

// String is the server's address with any password masked, for logs.
func (c *Client) String() string { return c.base.Redacted() }

// Ping checks the server answers its health check.
func (c *Client) Ping() error { return c.call(http.MethodGet, "/api/health", nil, nil) }

// Strategies lists the saved strategies.
func (c *Client) Strategies() ([]store.Strategy, error) {
	var out struct {
		Strategies []store.Strategy `json:"strategies"`
	}
	err := c.call(http.MethodGet, "/api/strategies", nil, &out)
	return out.Strategies, err
}

// SaveStrategy saves a strategy, new when id is empty.
func (c *Client) SaveStrategy(id, name string, def json.RawMessage) (store.Strategy, error) {
	var out store.Strategy
	body := map[string]any{"name": name, "definition": def}
	if id == "" {
		return out, c.call(http.MethodPost, "/api/strategies", body, &out)
	}
	return out, c.call(http.MethodPut, "/api/strategies/"+url.PathEscape(id), body, &out)
}

// CreateReport saves a report.
func (c *Client) CreateReport(title, body, author string) (store.Report, error) {
	var out store.Report
	return out, c.call(http.MethodPost, "/api/reports", map[string]string{"title": title, "body": body, "author": author}, &out)
}

// Reports lists the reports, newest first.
func (c *Client) Reports() ([]store.Report, error) {
	r, err := c.research()
	return r.Reports, err
}

// WatchStrategy starts a forward test on the collecting server, frozen from
// its tomorrow.
func (c *Client) WatchStrategy(name string, def json.RawMessage, note string) (store.Watch, error) {
	var out store.Watch
	return out, c.call(http.MethodPost, "/api/watches", map[string]any{"name": name, "definition": def, "note": note}, &out)
}

// Forward is every watched strategy's forward test, run on the collecting
// server.
func (c *Client) Forward() ([]engine.Forward, error) {
	r, err := c.research()
	if err != nil {
		return nil, err
	}
	out := make([]engine.Forward, len(r.Watches))
	for i, w := range r.Watches {
		out[i] = engine.Forward{Watch: w.Watch, Result: w.Result, Waiting: w.Waiting}
		if w.Error != "" {
			out[i].Err = errors.New(w.Error)
		}
	}
	return out, nil
}

// research is GET /api/research, in the shape the API writes it.
type research struct {
	Reports []store.Report `json:"reports"`
	Watches []struct {
		store.Watch
		Waiting bool             `json:"waiting"`
		Error   string           `json:"error"`
		Result  *strategy.Result `json:"result"`
	} `json:"watches"`
}

func (c *Client) research() (research, error) {
	var out research
	err := c.call(http.MethodGet, "/api/research", nil, &out)
	if out.Reports == nil {
		out.Reports = []store.Report{}
	}
	return out, err
}

// call sends one request and decodes the answer. The API's errors are
// sentences meant for a person, so they are passed on as they are; a 404
// is store.ErrNotFound, so callers can tell a missing row from a failure.
func (c *Client) call(method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	u := *c.base
	u.Path += path
	req, err := http.NewRequest(method, u.String(), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the home server at %s can't be reached: %w", c.base.Redacted(), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("reading the home server's answer: %w", err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %s", store.ErrNotFound, msg)
		}
		return fmt.Errorf("the home server refused it (%s): %s", resp.Status, msg)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the home server at %s didn't answer with what a tickers server sends — is that its address? (%v)", c.base.Redacted(), err)
	}
	return nil
}
