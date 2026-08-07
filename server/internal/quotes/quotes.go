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
