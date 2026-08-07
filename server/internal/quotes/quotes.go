// Package quotes fetches market data.
//
// The original update_minion_quotes.py used yfinance, which is a Python
// wrapper over Yahoo Finance's public JSON endpoints. This package talks to
// the same endpoints directly — one HTTP GET per symbol, no dependency, no
// interpreter — so the whole app stays a single static binary.
package quotes

import (
	"context"
	"errors"
	"time"
)

// Quote is one reading. Price is a pointer because "the market has no price
// for this right now" is a real, common answer (a delisted symbol, a typo, a
// venue that hasn't opened) and is not the same as a price of zero.
type Quote struct {
	Symbol        string
	Price         *float64
	PreviousClose *float64
	Currency      string
	ShortName     string
	MarketState   string
	FetchedAt     time.Time
}

// Settings is the operator-tunable part of a provider: where it talks to, how
// long it waits, and who it says it is.
//
// A zero field means "use the provider's own default" rather than "use zero",
// so a partially-filled Settings from the GUI overlays cleanly on whatever the
// CLI supplied. That is what lets the same struct carry a stored override and
// a start-up fallback.
type Settings struct {
	// BaseURL is the API root. Empty means the provider's own.
	BaseURL string `json:"baseUrl"`
	// Timeout bounds a single request. Zero means the provider's own.
	Timeout time.Duration `json:"-"`
	// UserAgent is sent on every request. Empty means the provider's own.
	UserAgent string `json:"userAgent"`
}

// Merge overlays the non-zero fields of override onto s.
func (s Settings) Merge(override Settings) Settings {
	if override.BaseURL != "" {
		s.BaseURL = override.BaseURL
	}
	if override.Timeout > 0 {
		s.Timeout = override.Timeout
	}
	if override.UserAgent != "" {
		s.UserAgent = override.UserAgent
	}
	return s
}

// Configurable is a provider whose Settings can change while it is running.
//
// The engine re-reads configuration every cycle, so a provider that implements
// this picks up a new base URL or timeout on the next refresh rather than at
// the next restart — which is the whole point of putting those fields in the
// GUI. Providers that can't be reconfigured simply don't implement it.
type Configurable interface {
	// Apply installs new settings. It must be safe to call concurrently with
	// Fetch and Search.
	Apply(Settings)
	// Effective reports the settings actually in force, with every default
	// resolved — this is what the UI displays.
	Effective() Settings
}

// Provider is a source of quotes. One method, so a test can substitute a
// deterministic source and so a second provider can be added later without
// touching the engine.
type Provider interface {
	// Fetch returns a quote per requested symbol. A provider that can't price
	// one symbol returns an error for that symbol only — the map and the error
	// map are both partial, and callers use both.
	Fetch(ctx context.Context, symbols []string) (map[string]Quote, map[string]error)

	// Search resolves free text to candidate symbols, for the web client's
	// "add a ticker" box. A provider without search returns ErrNoSearch.
	Search(ctx context.Context, query string) ([]Match, error)

	// Name identifies the provider in the UI and in logs.
	Name() string
}

// Match is one search hit.
type Match struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Exchange string `json:"exchange"`
	Type     string `json:"type"`
}

// ErrNoSearch is returned by providers that can only price a known symbol.
var ErrNoSearch = errors.New("this quote provider does not support symbol search")

// ErrNotFound means the provider has no such symbol.
var ErrNotFound = errors.New("no quote for that symbol")
