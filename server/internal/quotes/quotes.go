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
	"net/url"
	"strings"
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
	// LogoURL is a URL template for a symbol's logo, with `{symbol}` (or
	// `{symbol_lower}`) standing in for the ticker. Empty means "use whatever
	// the provider can work out on its own".
	//
	// It is a setting rather than a constant because there is no standard
	// place to get a logo by ticker: what works is a moving target of free
	// services, and an install that can reach one this binary has never heard
	// of should not have to wait for a release to use it.
	LogoURL string `json:"logoUrl"`
	// LogoKey authenticates the logo request. It never appears in the settings
	// the UI is served — see store.Config — and this struct's own JSON drops it
	// too, because Effective() is what the Settings page renders.
	LogoKey string `json:"-"`
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
	if override.LogoURL != "" {
		s.LogoURL = override.LogoURL
	}
	if override.LogoKey != "" {
		s.LogoKey = override.LogoKey
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

// Constituent is one position inside a fund, as the source reports it today.
//
// Weight is a percentage of the fund rather than a fraction, matching every
// other percentage that crosses this package's boundary. Sources quote it both
// ways; converting at the edge is what keeps the rest of the app from having to
// know which one it is talking to.
type Constituent struct {
	Symbol string
	Name   string
	Weight float64
}

// Composition is what a source can say about what a fund holds.
//
// Holdings is the *top* of the fund, not the whole of it — every source that
// answers this question for free answers with the largest handful — so the sum
// of the weights is meaningfully less than 100 and callers have to say so
// rather than presenting it as the fund.
type Composition struct {
	// Name is the fund's own long name, for a page that is about the fund
	// rather than about a row in a list.
	Name     string
	Holdings []Constituent
}

// Compositor is a provider that can also say what a fund holds.
//
// Optional in the way Historian and Distributor are, and asserted at the call
// site for the same reason — but the degradation is more visible than theirs,
// so it is worth naming. A provider without this leaves the fund page with its
// chart, its summary and its calendar years, and no look-through table. That is
// a page missing a card, not a broken one.
type Compositor interface {
	// Constituents returns what the fund holds now, largest first. A symbol
	// that exists and is not a fund is ErrNotFund, which is a durable answer
	// and not a failure — no amount of retrying makes AAPL a basket.
	Constituents(ctx context.Context, symbol string) (Composition, error)
}

// ErrNoConstituents is what a caller gets when the quote source cannot say what
// any fund holds — the look-through is unavailable rather than broken.
var ErrNoConstituents = errors.New("this quote provider cannot say what a fund holds")

// SectorWeight is one slice of where a symbol's money is invested.
//
// Weight is a percentage of the symbol rather than a fraction, matching
// Constituent.Weight and every other percentage that crosses this package's
// boundary. Sources quote it both ways, and converting at the edge is what
// keeps the rest of the app from having to know which one it is talking to.
type SectorWeight struct {
	Sector string
	Weight float64
}

// SectorNames are the sectors this app knows, in one fixed order.
//
// Fixed, and shared with the web client, because the whole point of the card
// they feed is that two allocations are drawn side by side: "Energy" has to be
// the same colour in both pies, and it has to stay that colour whichever
// sectors happen to be in either of them. An order that came out of the data —
// largest first, say — would repaint every slice the moment a comparison
// changed.
//
// Alphabetical rather than by size or by any of the several conflicting
// "standard" orders, because it is the one order that is stable, obvious, and
// nobody's opinion.
var SectorNames = []string{
	"Basic Materials",
	"Communication Services",
	"Consumer Cyclical",
	"Consumer Defensive",
	"Energy",
	"Financial Services",
	"Healthcare",
	"Industrials",
	"Real Estate",
	"Technology",
	"Utilities",
}

// sectorAliases maps a source's spelling to the canonical name.
//
// The key is the name with everything but its letters removed, because the two
// modules that answer this question disagree with each other: a fund's
// breakdown comes back keyed `consumer_cyclical` and a company's profile says
// "Consumer Cyclical". Folding both to bare letters is what lets one table
// serve the pair — and, later, a second provider that has invented a third
// spelling.
var sectorAliases = map[string]string{
	"basicmaterials":        "Basic Materials",
	"materials":             "Basic Materials",
	"communicationservices": "Communication Services",
	"communication":         "Communication Services",
	"consumercyclical":      "Consumer Cyclical",
	"consumerdiscretionary": "Consumer Cyclical",
	"consumerdefensive":     "Consumer Defensive",
	"consumerstaples":       "Consumer Defensive",
	"energy":                "Energy",
	"financialservices":     "Financial Services",
	"financial":             "Financial Services",
	"financials":            "Financial Services",
	"healthcare":            "Healthcare",
	"industrials":           "Industrials",
	"realestate":            "Real Estate",
	"technology":            "Technology",
	"informationtechnology": "Technology",
	"utilities":             "Utilities",
}

// NormalizeSector maps whatever a source calls a sector onto SectorNames.
//
// A name it has never seen is trimmed and handed back as it came rather than
// dropped or filed under something plausible. It will be drawn without a colour
// of its own, which is a far smaller lie than counting a sector this build has
// not heard of as part of a different one.
func NormalizeSector(name string) string {
	letters := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, name)
	if canonical, ok := sectorAliases[letters]; ok {
		return canonical
	}
	return strings.TrimSpace(name)
}

// Classifier is a provider that can also say what a symbol's money is invested
// in, sector by sector.
//
// Optional in the way Historian, Distributor and Compositor are, and asserted
// at the call site for the same reason. It is deliberately not a method on
// Compositor even though one source answers both from one request: a company is
// not a fund and has no constituents, and it still has a sector — folding the
// two together would make the sector of every individual holding unaskable,
// which is exactly what a portfolio's look-through is made of.
type Classifier interface {
	// Sectors returns where one symbol's money is, largest slice first, in
	// percentages of that symbol. A fund answers with its breakdown and a
	// company with a single 100% slice; anything the source will not classify
	// is ErrUnclassified, which is a durable answer and not a failure.
	Sectors(ctx context.Context, symbol string) ([]SectorWeight, error)
}

// ErrNoSectors is what a caller gets when the quote source cannot say what
// sector anything is in — the allocation card is unavailable rather than
// broken.
var ErrNoSectors = errors.New("this quote provider cannot say what sectors a symbol is invested in")

// ErrUnclassified means the source knows this symbol and will not put it in a
// sector. Durable, like ErrNotFund: a currency pair, a bond fund and a gold
// trust are all permanently sectorless, and a caller is meant to say so and
// stop asking.
var ErrUnclassified = errors.New("that symbol has no sector breakdown")

// ErrNotFund means the source knows this symbol and it has no holdings to
// report. Durable, like ErrNoLogo: a caller is meant to say so and stop asking.
var ErrNotFund = errors.New("that symbol is not a fund")

// LogoValidators is what a previous fetch left behind so the next one can be
// conditional: an ETag, a Last-Modified date, or neither.
//
// They travel back to the provider rather than being interpreted here — their
// only meaning is "what the source said last time", and only the source knows
// whether that is still true.
type LogoValidators struct {
	ETag         string
	LastModified string
}

// Logo is one symbol's brand image, as bytes. It is bytes rather than a URL
// because the caller caches it: the point of the feature is that a browser
// never talks to whoever drew it, and a URL handed to the client would defeat
// that entirely.
type Logo struct {
	ContentType string
	Bytes       []byte
	// Validators are what to send on the next check.
	Validators LogoValidators
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
	// Logo returns the image for a symbol, or ErrNoLogo if the source knows the
	// symbol and has no picture of it. Anything else is a real failure and is
	// worth retrying later.
	//
	// `known` is what the last fetch of this symbol left behind, zero if there
	// wasn't one. A provider that can make the request conditional should, and
	// returns ErrLogoUnchanged when the source says nothing has moved.
	Logo(ctx context.Context, symbol string, known LogoValidators) (Logo, error)
}

// ErrNoSearch is returned by providers that can only price a known symbol.
var ErrNoSearch = errors.New("this quote provider does not support symbol search")

// ErrNoLogos is what a caller gets when the quote source cannot supply logos
// at all — the marks stay as they are drawn rather than anything being broken.
var ErrNoLogos = errors.New("this quote provider does not supply logos")

// ErrLogoUnchanged means the source was asked whether the image had changed and
// said no. It is the good outcome of a re-check: nothing was transferred, and
// the caller's stored copy is still current.
var ErrLogoUnchanged = errors.New("that logo has not changed")

// ErrNoLogo means the source looked and this particular symbol hasn't got one.
// It is a durable answer, not a failure: an index fund has no logo today and
// will have none tomorrow, and a caller is meant to record it and stop asking.
var ErrNoLogo = errors.New("that symbol has no logo")

// ErrNoHistory is what a caller gets when the quote source can only price
// today — the performance view is unavailable rather than broken.
var ErrNoHistory = errors.New("this quote provider does not supply price history")

// ErrNotFound means the provider has no such symbol.
var ErrNotFound = errors.New("no quote for that symbol")

// What a logo URL template may contain. One of the symbol tokens has to be
// there, or every symbol would be given the same picture.
const (
	LogoSymbolToken      = "{symbol}"
	LogoSymbolLowerToken = "{symbol_lower}"
	// LogoKeyToken is where the key goes when the service wants it in the URL.
	// Its absence is meaningful: a key configured with no `{key}` in the
	// template is sent as a bearer token instead, which is how the same field
	// serves both kinds of credential these services hand out.
	LogoKeyToken = "{key}"
)

// ExpandLogoURL fills a logo template in for one symbol.
//
// The symbol is path-escaped: a template can put it in a path segment or a
// query, and `BRK-B` is fine in both but a symbol with a slash in it would
// otherwise silently address a different path.
func ExpandLogoURL(template, symbol, key string) string {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	out := strings.ReplaceAll(template, LogoSymbolToken, url.PathEscape(symbol))
	out = strings.ReplaceAll(out, LogoSymbolLowerToken, url.PathEscape(strings.ToLower(symbol)))
	return strings.ReplaceAll(out, LogoKeyToken, url.QueryEscape(key))
}

// RedactLogoURL is that URL with the credential taken back out, for anywhere it
// might be written down.
//
// A logo URL ends up in three places a key has no business being: an error
// message, the `source` column of the cache, and — through the reason on a
// tombstone — the Settings page of an app with no login on it. Redacting at
// the point of use rather than remembering to do it at each of those is what
// keeps a key out of all three.
func RedactLogoURL(src, key string) string {
	if key != "" {
		src = strings.ReplaceAll(src, key, "…")
		src = strings.ReplaceAll(src, url.QueryEscape(key), "…")
	}
	// Also by name, because a template can carry a token this build never saw:
	// pasted straight into the URL rather than into the key field.
	parsed, err := url.Parse(src)
	if err != nil {
		return src
	}
	query := parsed.Query()
	for _, name := range []string{"token", "key", "apikey", "api_key", "access_token"} {
		if query.Has(name) {
			query.Set(name, "…")
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
