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

// Bar is one day's closing value, adjusted for splits and dividends where the
// provider reports an adjusted series. Adjusted is what a return wants: an
// unadjusted five-year chart of a stock that split 4:1 shows a 75% crash that
// nobody experienced.
//
// Date is the exchange's own calendar day rather than an instant, because that
// is the key a composite's legs are aligned on — a close in Auckland stamped in
// UTC lands on the previous day and would never line up with a US leg.
type Bar struct {
	// Date is YYYY-MM-DD in the exchange's timezone.
	Date  string
	Close float64
	// Raw is the close *before* adjustment — the price actually printed that
	// day. It exists for one job: a dividend is paid per share in the money of
	// its day, so a yield has to divide it by the price of its day. Dividing a
	// 1998 payout by a 1998 close that has since been marked down by thirty
	// years of distributions gives a yield several times the real one.
	//
	// Zero means the provider didn't distinguish the two; callers should read
	// Close instead, which is what a series with no adjustment already is.
	Raw float64
}

// Distribution is one dividend, per share, in the money of the day it was paid.
type Distribution struct {
	// Date is YYYY-MM-DD, the ex-dividend date.
	Date   string
	Amount float64
}

// Historian is a provider that can also return past closes, for the
// performance view.
//
// It is optional for the same reason Configurable is: the refresh loop only
// ever needs today's price, so a provider that knows nothing else is still a
// complete Provider. Callers type-assert, and say so when the assertion fails.
type Historian interface {
	// History returns daily bars from since until now, oldest first. A symbol
	// the provider has no data for in that window comes back empty rather than
	// as an error — "nothing traded" is an answer.
	History(ctx context.Context, symbol string, since time.Time) ([]Bar, error)
}

// Distributor is a provider that can also say what a symbol paid out.
//
// It is separate from Historian rather than a method on it, and that separation
// is the point: a source can perfectly well have prices and no dividend feed —
// crypto and currency series have nothing to pay — and folding the two together
// would make such a source unable to offer history at all. Callers assert for
// it and drop the feature quietly when it isn't there, the way they do with
// Configurable.
type Distributor interface {
	// Dividends returns every distribution from since until now, oldest first.
	// A symbol that has never paid one comes back empty rather than as an
	// error — "it doesn't pay a dividend" is an answer.
	Dividends(ctx context.Context, symbol string, since time.Time) ([]Distribution, error)
}

// Logo is one symbol's brand image, as bytes. It is bytes rather than a URL
// because the caller caches it: the point of the feature is that a browser
// never talks to whoever drew it, and a URL handed to the client would defeat
// that entirely.
type Logo struct {
	ContentType string
	Bytes       []byte
	// Source is the URL the image came from, kept for the record. Nothing
	// reads it at runtime — it is there so "where did this picture come from?"
	// is answerable from the database.
	Source string
}

// Iconographer is a provider that can also supply a symbol's logo.
//
// Optional like Historian, and for a stronger reason than "not every source
// has one": this is the capability that reaches outside the quote API, so a
// provider that would rather not do that simply doesn't implement it and the
// feature disappears rather than half-works.
type Iconographer interface {
	// Logo returns the image for a symbol, or ErrNoLogo if the source knows
	// the symbol and has no picture of it. Anything else is a real failure and
	// is worth retrying later.
	Logo(ctx context.Context, symbol string) (Logo, error)
}

// ErrNoSearch is returned by providers that can only price a known symbol.
var ErrNoSearch = errors.New("this quote provider does not support symbol search")

// ErrNoLogos is what a caller gets when the quote source cannot supply logos
// at all — the marks stay as they are drawn rather than anything being broken.
var ErrNoLogos = errors.New("this quote provider does not supply logos")

// ErrNoLogo means the source looked and this particular symbol hasn't got one.
// It is a durable answer, not a failure: an index fund has no logo today and
// will have none tomorrow, and a caller is meant to record it and stop asking.
var ErrNoLogo = errors.New("that symbol has no logo")

// ErrNoHistory is what a caller gets when the quote source can only price
// today — the performance view is unavailable rather than broken.
var ErrNoHistory = errors.New("this quote provider does not supply price history")

// ErrNotFound means the provider has no such symbol.
var ErrNotFound = errors.New("no quote for that symbol")
