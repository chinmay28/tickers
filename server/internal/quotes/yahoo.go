package quotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// YahooName is the provider's identifier.
const YahooName = "yahoo"

// DefaultBaseURL is Yahoo Finance's public API root.
const DefaultBaseURL = "https://query1.finance.yahoo.com"

// DefaultUserAgent is sent on every request unless one is configured.
//
// Yahoo's public endpoints answer a browser and stonewall an obvious script,
// which is the single most common reason a self-hosted quote fetcher starts
// returning nothing after working for months. yfinance sets a browser UA for
// the same reason; this is that, made explicit — and it is configurable
// because the string that works is a moving target.
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// DefaultTimeout bounds a single request when nothing else says otherwise.
const DefaultTimeout = 20 * time.Second

// maxConcurrent caps in-flight requests. A watchlist is a handful of symbols,
// and firing all of them at once from a Raspberry Pi on a domestic connection
// is a good way to collect timeouts rather than quotes.
const maxConcurrent = 4

// Yahoo reads quotes from Yahoo Finance's public chart endpoint — the same
// data yfinance returns for `history(period="1d", interval="1m")`.
//
// It is Configurable: the base URL, timeout and user agent can all be changed
// from the Settings page while the service runs. The mutex is what makes that
// safe — Apply can land while a refresh cycle is mid-flight.
type Yahoo struct {
	mu sync.RWMutex
	// fallback is what the operator supplied at start-up (flags/env). Stored
	// settings overlay it; clearing a stored field reveals it again.
	fallback Settings
	active   Settings
	client   *http.Client
	// jar carries the session cookie the crumbed endpoints require. It outlives
	// the client, which Apply rebuilds whenever the timeout moves — throwing the
	// cookie away on a settings change would mean re-handshaking for no reason.
	jar http.CookieJar

	// crumbMu guards the handshake rather than the settings, so a slow
	// getcrumb round trip cannot block a refresh cycle reading the base URL.
	crumbMu sync.Mutex
	crumb   string
	crumbAt time.Time
}

// NewYahoo builds a provider. The settings given are the start-up fallback —
// any field left zero uses the package default, and a stored override from the
// database wins over both.
func NewYahoo(fallback Settings) *Yahoo {
	y := &Yahoo{fallback: fallback}
	// A jar is only ever needed by the crumbed endpoints, but it is built here
	// rather than lazily so that Apply — which can land at any time — never has
	// to decide whether to install one. cookiejar.New with a nil options is
	// documented never to fail; a nil jar would still leave the chart endpoints
	// working, which is why this doesn't panic.
	if jar, err := cookiejar.New(nil); err == nil {
		y.jar = jar
	}
	y.Apply(Settings{})
	return y
}

// Name implements Provider.
func (y *Yahoo) Name() string { return YahooName }

// Apply implements Configurable.
func (y *Yahoo) Apply(override Settings) {
	next := Settings{
		BaseURL:   DefaultBaseURL,
		Timeout:   DefaultTimeout,
		UserAgent: DefaultUserAgent,
	}.Merge(y.fallback).Merge(override)
	next.BaseURL = strings.TrimRight(next.BaseURL, "/")

	y.mu.Lock()
	defer y.mu.Unlock()
	// Rebuild the client only when the timeout actually moves: mutating a live
	// client's Timeout would race with requests already using it, and building
	// a fresh one per request would throw away connection pooling. Sharing the
	// default transport keeps the pool across rebuilds.
	if y.client == nil || y.active.Timeout != next.Timeout {
		y.client = &http.Client{Timeout: next.Timeout, Transport: http.DefaultTransport, Jar: y.jar}
	}
	// A base URL that moved points at a different Yahoo — or at a stub — and the
	// crumb held for the old one authenticates nothing there.
	if y.active.BaseURL != next.BaseURL {
		y.forgetCrumb()
	}
	y.active = next
}

// Effective implements Configurable.
func (y *Yahoo) Effective() Settings {
	y.mu.RLock()
	defer y.mu.RUnlock()
	return y.active
}

// current returns the settings and client to use for one request, read under
// the lock so a concurrent Apply can't tear them apart.
func (y *Yahoo) current() (Settings, *http.Client) {
	y.mu.RLock()
	defer y.mu.RUnlock()
	return y.active, y.client
}

// Fetch implements Provider, pricing symbols concurrently.
func (y *Yahoo) Fetch(ctx context.Context, symbols []string) (map[string]Quote, map[string]error) {
	quotes := make(map[string]Quote, len(symbols))
	failures := make(map[string]error, 0)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	sem := make(chan struct{}, maxConcurrent)

	for _, raw := range symbols {
		symbol := strings.ToUpper(strings.TrimSpace(raw))
		if symbol == "" {
			continue
		}
		wg.Add(1)
		go func(symbol string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				failures[symbol] = ctx.Err()
				mu.Unlock()
				return
			}

			q, err := y.fetchOne(ctx, symbol)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures[symbol] = err
				return
			}
			quotes[symbol] = q
		}(symbol)
	}
	wg.Wait()
	return quotes, failures
}

// chartResponse is the slice of Yahoo's /v8/finance/chart payload we use. The
// same shape answers both the 1-minute quote request and the daily history
// request; which fields are populated is the only difference.
type chartResponse struct {
	Chart struct {
		Result []chartResult `json:"result"`
		Error  *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

type chartResult struct {
	Meta struct {
		Currency           string  `json:"currency"`
		Symbol             string  `json:"symbol"`
		ExchangeName       string  `json:"exchangeName"`
		ShortName          string  `json:"shortName"`
		LongName           string  `json:"longName"`
		MarketState        string  `json:"marketState"`
		RegularMarketPrice float64 `json:"regularMarketPrice"`
		PreviousClose      float64 `json:"previousClose"`
		ChartPreviousClose float64 `json:"chartPreviousClose"`
		// GMTOffset turns a bar's timestamp into the exchange's own calendar
		// day. Without it a Tokyo close lands on the wrong side of midnight
		// UTC.
		GMTOffset int64 `json:"gmtoffset"`
	} `json:"meta"`
	// Timestamp is one epoch second per bar, parallel to the close series.
	Timestamp  []int64 `json:"timestamp"`
	Indicators struct {
		Quote    []closeSeries    `json:"quote"`
		AdjClose []adjCloseSeries `json:"adjclose"`
	} `json:"indicators"`
	// Events is populated only when the request asked for it. Yahoo keys the
	// dividends by epoch second as a string, which is why this is a map rather
	// than the array every other series here is.
	Events struct {
		Dividends map[string]struct {
			Amount float64 `json:"amount"`
			Date   int64   `json:"date"`
		} `json:"dividends"`
	} `json:"events"`
}

type closeSeries struct {
	Close []*float64 `json:"close"`
}

type adjCloseSeries struct {
	AdjClose []*float64 `json:"adjclose"`
}

func (y *Yahoo) fetchOne(ctx context.Context, symbol string) (Quote, error) {
	settings, _ := y.current()
	endpoint := fmt.Sprintf("%s/v8/finance/chart/%s?range=1d&interval=1m&includePrePost=false",
		settings.BaseURL, url.PathEscape(symbol))

	body, err := y.get(ctx, endpoint)
	if err != nil {
		return Quote{}, err
	}

	var parsed chartResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Quote{}, fmt.Errorf("decode quote for %s: %w", symbol, err)
	}
	if parsed.Chart.Error != nil {
		return Quote{}, fmt.Errorf("%s: %s", symbol, parsed.Chart.Error.Description)
	}
	if len(parsed.Chart.Result) == 0 {
		return Quote{}, fmt.Errorf("%s: %w", symbol, ErrNotFound)
	}

	res := parsed.Chart.Result[0]
	q := Quote{
		Symbol:      symbol,
		Currency:    res.Meta.Currency,
		ShortName:   firstNonEmpty(res.Meta.ShortName, res.Meta.LongName),
		MarketState: res.Meta.MarketState,
		FetchedAt:   time.Now().UTC(),
	}

	// The last non-null close in the 1-minute series is exactly what the
	// original script took (`data['Close'].iloc[-1]`). It trails
	// regularMarketPrice by up to a minute but agrees with the chart the user
	// is looking at; meta is the fallback for symbols whose series comes back
	// empty — common outside trading hours.
	if last, ok := lastClose(res.Indicators.Quote); ok {
		q.Price = &last
	} else if res.Meta.RegularMarketPrice != 0 {
		p := res.Meta.RegularMarketPrice
		q.Price = &p
	}

	if prev := firstNonZero(res.Meta.ChartPreviousClose, res.Meta.PreviousClose); prev != 0 {
		q.PreviousClose = &prev
	}
	if q.Price == nil {
		return Quote{}, fmt.Errorf("%s: %w", symbol, ErrNotFound)
	}
	return q, nil
}

// History implements Historian, reading daily bars from the same chart
// endpoint the quote path uses.
//
// It asks with explicit period1/period2 epochs rather than one of Yahoo's named
// ranges ("5y", "1y"). A five-year return needs the close from *before* five
// years ago, and a named range starts on that boundary at best — so the caller
// has to be able to ask for its own margin.
func (y *Yahoo) History(ctx context.Context, symbol string, since time.Time) ([]Bar, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, errors.New("a symbol is required")
	}
	settings, _ := y.current()
	// period2 is a day out rather than now: the bar for a session still in
	// progress is the one the user is looking at, and an exchange west of here
	// can be trading on tomorrow's date already.
	endpoint := fmt.Sprintf("%s/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&includePrePost=false",
		settings.BaseURL, url.PathEscape(symbol), since.Unix(), time.Now().Add(24*time.Hour).Unix())

	body, err := y.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var parsed chartResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode history for %s: %w", symbol, err)
	}
	if parsed.Chart.Error != nil {
		return nil, fmt.Errorf("%s: %s", symbol, parsed.Chart.Error.Description)
	}
	if len(parsed.Chart.Result) == 0 {
		return nil, fmt.Errorf("%s: %w", symbol, ErrNotFound)
	}

	res := parsed.Chart.Result[0]
	closes := historyCloses(res)
	raw := rawCloses(res)
	bars := make([]Bar, 0, len(res.Timestamp))
	for i, ts := range res.Timestamp {
		// A null close is a session with no trade — a holiday Yahoo still
		// emits a slot for. Dropping it is right: a chart that interpolates
		// across a gap and a return computed off a phantom close both lie.
		if i >= len(closes) || closes[i] == nil {
			continue
		}
		bar := Bar{
			Date:  time.Unix(ts+res.Meta.GMTOffset, 0).UTC().Format("2006-01-02"),
			Close: *closes[i],
		}
		if i < len(raw) && raw[i] != nil {
			bar.Raw = *raw[i]
		}
		bars = append(bars, bar)
	}
	return bars, nil
}

// Dividends implements Distributor.
//
// It asks the same chart endpoint with `events=div`, at a monthly interval: the
// events block does not depend on the bar interval, and asking for daily bars
// would fetch thirty years of prices this call has no use for. Yahoo keys the
// block by epoch second, so the map is walked and sorted rather than read in
// order.
func (y *Yahoo) Dividends(ctx context.Context, symbol string, since time.Time) ([]Distribution, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, errors.New("a symbol is required")
	}
	settings, _ := y.current()
	endpoint := fmt.Sprintf("%s/v8/finance/chart/%s?period1=%d&period2=%d&interval=1mo&events=div",
		settings.BaseURL, url.PathEscape(symbol), since.Unix(), time.Now().Add(24*time.Hour).Unix())

	body, err := y.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var parsed chartResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode dividends for %s: %w", symbol, err)
	}
	if parsed.Chart.Error != nil {
		return nil, fmt.Errorf("%s: %s", symbol, parsed.Chart.Error.Description)
	}
	if len(parsed.Chart.Result) == 0 {
		return nil, fmt.Errorf("%s: %w", symbol, ErrNotFound)
	}

	res := parsed.Chart.Result[0]
	out := make([]Distribution, 0, len(res.Events.Dividends))
	for _, d := range res.Events.Dividends {
		if d.Amount == 0 {
			continue
		}
		out = append(out, Distribution{
			// Same offset as a bar's date, for the same reason: a payout has to
			// land in the exchange's own year, not UTC's.
			Date:   time.Unix(d.Date+res.Meta.GMTOffset, 0).UTC().Format("2006-01-02"),
			Amount: d.Amount,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// historyCloses prefers the split- and dividend-adjusted series and falls back
// to the raw closes, which is all Yahoo returns for instruments that have
// neither (currencies, crypto).
func historyCloses(res chartResult) []*float64 {
	if len(res.Indicators.AdjClose) > 0 && len(res.Indicators.AdjClose[0].AdjClose) > 0 {
		return res.Indicators.AdjClose[0].AdjClose
	}
	if len(res.Indicators.Quote) > 0 {
		return res.Indicators.Quote[0].Close
	}
	return nil
}

// rawCloses is the unadjusted series, or nil when the response has none. It is
// what a dividend has to be divided by to be a yield — see Bar.Raw.
func rawCloses(res chartResult) []*float64 {
	if len(res.Indicators.Quote) > 0 {
		return res.Indicators.Quote[0].Close
	}
	return nil
}

// searchResponse is the slice of Yahoo's /v1/finance/search payload we use.
type searchResponse struct {
	Quotes []struct {
		Symbol    string `json:"symbol"`
		ShortName string `json:"shortname"`
		LongName  string `json:"longname"`
		ExchDisp  string `json:"exchDisp"`
		TypeDisp  string `json:"typeDisp"`
		QuoteType string `json:"quoteType"`
		// LogoURL is present for some equities and absent for most everything
		// else. An absent one is the normal case, not an error.
		LogoURL string `json:"logoUrl"`
	} `json:"quotes"`
}

// Search implements Provider.
func (y *Yahoo) Search(ctx context.Context, query string) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []Match{}, nil
	}
	settings, _ := y.current()
	endpoint := fmt.Sprintf("%s/v1/finance/search?q=%s&quotesCount=10&newsCount=0&listsCount=0",
		settings.BaseURL, url.QueryEscape(query))

	body, err := y.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode search results: %w", err)
	}

	out := make([]Match, 0, len(parsed.Quotes))
	for _, m := range parsed.Quotes {
		if m.Symbol == "" {
			continue
		}
		out = append(out, Match{
			Symbol:   strings.ToUpper(m.Symbol),
			Name:     firstNonEmpty(m.ShortName, m.LongName),
			Exchange: m.ExchDisp,
			Type:     firstNonEmpty(m.TypeDisp, m.QuoteType),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fund composition
// ---------------------------------------------------------------------------

// The chart endpoint is open; everything else on Yahoo's API is not. quoteSummary
// — the only module that says what a fund holds — wants a session cookie and a
// "crumb" minted against it, and answers 401 without them.
//
// That is a real cost, and it is confined to this section on purpose: nothing
// the refresh loop does touches any of it, so a handshake that starts failing
// costs the look-through table and leaves quotes, history and publishing
// exactly as they were.
const (
	// crumbTTL is how long one handshake is reused. Yahoo does not say how long
	// a crumb lives, so this is short enough that an expiry is a once-an-hour
	// event and long enough that browsing several funds costs one handshake.
	// The 401 retry below is what actually makes expiry a non-event; this only
	// keeps it rare.
	crumbTTL = 30 * time.Minute

	// maxHoldings caps what will be read out of one response. Yahoo returns ten;
	// the cap is here so a source that changed its mind and returned five
	// hundred could not turn a fund page into five hundred history fetches.
	maxHoldings = 50

	// anything is the Accept header for the two crumbed requests.
	//
	// Not "application/json", which is right for the chart endpoint and wrong
	// here: the crumb is served as text/plain, and a JSON-only Accept gets an
	// HTTP 406 back instead of a token. Sending it on the summary request too
	// costs nothing and removes the same class of failure from the endpoint
	// that matters.
	anything = "*/*"

	// browserAccept is what the cookie request asks for. It is fetching a page,
	// not an API, and a source that fingerprints its callers should see
	// something consistent with the browser this client claims to be.
	browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
)

// cookieHosts are where a session cookie is looked for, in order, when talking
// to Yahoo itself.
//
// Two of them because the first is what every other client uses and neither is
// documented. fc.yahoo.com answers with a 404 and the cookie; the main site is
// the fallback for the day that stops being true. Both set the cookie on
// `.yahoo.com`, which is what lets the jar send it to the API host — a
// host-only cookie from either would never reach query1.
var cookieHosts = []string{"https://fc.yahoo.com/", "https://finance.yahoo.com/"}

// httpError carries the status code of a non-2xx response, so a caller that
// can do something about a particular code — re-handshake on a 401 — can see
// it without matching on a message.
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("quote provider returned HTTP %d: %s", e.status, e.body)
}

// cookieSources is where to go for a session cookie.
//
// Against real Yahoo those are its own hosts, none of which is the API host.
// Against anything else — a stub, a proxy, a mirror — it is that host's own
// root, because an operator who redirected the API somewhere is not also
// running Yahoo's cookie domain. This is what keeps the whole path exercisable
// with `--quote-base-url` pointed at a throwaway server.
func cookieSources(base string) []string {
	if base == DefaultBaseURL {
		return cookieHosts
	}
	return []string{base + "/"}
}

// seedCookies collects a session cookie, stopping as soon as the jar holds one
// the API host will actually be sent.
//
// Every status is ignored: Yahoo answers the cookie host with a 404 and the
// header that matters, so a version of this that checked would fail on the
// success case.
func (y *Yahoo) seedCookies(ctx context.Context, settings Settings, client *http.Client) {
	for _, source := range cookieSources(settings.BaseURL) {
		if y.hasSessionCookie(settings.BaseURL) {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", settings.UserAgent)
		req.Header.Set("Accept", browserAccept)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
	}
}

// hasSessionCookie reports whether the jar would send anything to the API host.
//
// The question is deliberately asked about the *API* host rather than about the
// host the cookie came from: a cookie scoped to fc.yahoo.com alone is one the
// crumbed endpoints will never see, and would look like a successful handshake
// right up until the 401.
func (y *Yahoo) hasSessionCookie(base string) bool {
	if y.jar == nil {
		return false
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return false
	}
	return len(y.jar.Cookies(parsed)) > 0
}

// authorise returns a crumb, doing the handshake if the one in hand is missing
// or stale.
//
// The settings are read *before* the crumb lock is taken. crumbMu is always the
// inner lock of the two — Apply takes the settings lock and then forgets the
// crumb — and reversing that here is the one way this could deadlock.
func (y *Yahoo) authorise(ctx context.Context) (string, error) {
	settings, client := y.current()

	y.crumbMu.Lock()
	defer y.crumbMu.Unlock()
	if y.crumb != "" && time.Since(y.crumbAt) < crumbTTL {
		return y.crumb, nil
	}

	y.seedCookies(ctx, settings, client)

	// Accepting anything, because the crumb is text and asking for JSON is
	// answered with a 406 rather than a token.
	body, err := y.getAccepting(ctx, settings.BaseURL+"/v1/test/getcrumb", anything)
	if err != nil {
		return "", crumbFailure(err)
	}
	crumb := strings.TrimSpace(string(body))
	// An empty 200 is Yahoo's other way of refusing, and it is worth its own
	// message: "HTTP 200" in a log next to a broken feature helps nobody.
	if crumb == "" {
		return "", errors.New("the quote source issued an empty session token — it is refusing API access from here")
	}

	y.crumb, y.crumbAt = crumb, time.Now()
	return crumb, nil
}

// crumbFailure turns a refused handshake into something an operator can act on.
//
// Three of these are worth naming because they call for three different
// responses, and "HTTP 406" beside a broken card calls for none of them. The
// rest keep the underlying error, which already carries the status and a
// snippet of what came back.
func crumbFailure(err error) error {
	var status *httpError
	if errors.As(err, &status) {
		switch status.status {
		case http.StatusNotAcceptable:
			return fmt.Errorf(
				"the quote source rejected this client's headers when asked for a session token (HTTP 406) — try a different User-Agent in Settings: %w", err)
		case http.StatusTooManyRequests:
			return fmt.Errorf(
				"the quote source is rate-limiting this address (HTTP 429) — fund holdings will work again once it stops: %w", err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf(
				"the quote source would not issue a session token (HTTP %d) — it did not accept the session cookie: %w", status.status, err)
		}
	}
	return fmt.Errorf("could not get a session token from the quote source: %w", err)
}

// forgetCrumb drops the held token so the next call handshakes again.
func (y *Yahoo) forgetCrumb() {
	y.crumbMu.Lock()
	defer y.crumbMu.Unlock()
	y.crumb, y.crumbAt = "", time.Time{}
}

// yahooNumber is a number Yahoo will send either bare or wrapped.
//
// The same field comes back as `0.0921` with `formatted=false` and as
// `{"raw":0.0921,"fmt":"9.21%"}` without it, and which one arrives has changed
// before. Accepting both here costs a few lines once; discovering the
// difference in production costs the whole feature.
type yahooNumber struct {
	Value float64
	Set   bool
}

func (n *yahooNumber) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" || text == "{}" {
		return nil
	}
	if strings.HasPrefix(text, "{") {
		var wrapped struct {
			Raw *float64 `json:"raw"`
		}
		if err := json.Unmarshal(data, &wrapped); err != nil {
			return err
		}
		if wrapped.Raw != nil {
			n.Value, n.Set = *wrapped.Raw, true
		}
		return nil
	}
	if err := json.Unmarshal(data, &n.Value); err != nil {
		return err
	}
	n.Set = true
	return nil
}

// quoteSummaryResponse is the slice of Yahoo's /v10/finance/quoteSummary payload
// this uses.
type quoteSummaryResponse struct {
	QuoteSummary struct {
		Result []struct {
			TopHoldings struct {
				Holdings []struct {
					Symbol         string      `json:"symbol"`
					HoldingName    string      `json:"holdingName"`
					HoldingPercent yahooNumber `json:"holdingPercent"`
				} `json:"holdings"`
			} `json:"topHoldings"`
			QuoteType struct {
				LongName  string `json:"longName"`
				ShortName string `json:"shortName"`
				QuoteType string `json:"quoteType"`
			} `json:"quoteType"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"quoteSummary"`
}

// Constituents implements Compositor.
//
// One request for two modules: the holdings, and the fund's own name to put at
// the top of the page. Asking separately would double the number of crumbed
// requests, which are the fragile ones.
func (y *Yahoo) Constituents(ctx context.Context, symbol string) (Composition, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return Composition{}, errors.New("a symbol is required")
	}

	body, err := y.summary(ctx, symbol)
	if err != nil {
		return Composition{}, err
	}

	var parsed quoteSummaryResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Composition{}, fmt.Errorf("decode holdings for %s: %w", symbol, err)
	}
	if parsed.QuoteSummary.Error != nil {
		return Composition{}, fmt.Errorf("%s: %s", symbol, parsed.QuoteSummary.Error.Description)
	}
	if len(parsed.QuoteSummary.Result) == 0 {
		return Composition{}, fmt.Errorf("%s: %w", symbol, ErrNotFound)
	}

	res := parsed.QuoteSummary.Result[0]
	out := Composition{Name: firstNonEmpty(res.QuoteType.LongName, res.QuoteType.ShortName)}

	for _, h := range res.TopHoldings.Holdings {
		if len(out.Holdings) >= maxHoldings {
			break
		}
		holding := strings.ToUpper(strings.TrimSpace(h.Symbol))
		// A holding with no symbol cannot be priced, and a fund's cash line
		// arrives exactly like that. Dropping it is right — but it is also why
		// the weights on the page can sum to less than the source's own total.
		if holding == "" || !h.HoldingPercent.Set {
			continue
		}
		out.Holdings = append(out.Holdings, Constituent{
			Symbol: holding,
			Name:   strings.TrimSpace(h.HoldingName),
			// Yahoo quotes a fraction of the fund; the interface is in percent.
			Weight: h.HoldingPercent.Value * 100,
		})
	}

	if len(out.Holdings) == 0 {
		// Durable, and worth naming what the source said it was: "AAPL is not a
		// fund" is a complete answer, where "no holdings" reads like an outage.
		// "lists X as equity" rather than "calls X a equity": the kind comes
		// from the source and there is no article that fits all of them.
		kind := strings.ToLower(res.QuoteType.QuoteType)
		if kind == "" {
			kind = "something with no holdings"
		}
		return Composition{}, fmt.Errorf("%w: the quote source lists %s as %s", ErrNotFund, symbol, kind)
	}
	return out, nil
}

// summary makes one crumbed request, handshaking first and once more if the
// answer says the crumb is stale.
//
// The retry is the point of the whole arrangement. A crumb expires on Yahoo's
// schedule rather than ours, so treating the first 401 as an error would make
// the feature fail intermittently for exactly as long as it took somebody to
// reload the page.
func (y *Yahoo) summary(ctx context.Context, symbol string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		crumb, err := y.authorise(ctx)
		if err != nil {
			// The handshake is a means, not the goal. Not every deployment of
			// this API demands a crumb — a mirror, a caching proxy, a region
			// Yahoo treats differently — so a refused token is worth one plain
			// attempt before the feature is declared unavailable.
			if body, plain := y.askSummary(ctx, symbol, ""); plain == nil {
				return body, nil
			}
			// Reported as the handshake failure rather than as whatever the
			// uncrumbed attempt said: the token is the thing that went wrong,
			// and its message is the one with a fix in it.
			return nil, err
		}

		body, err := y.askSummary(ctx, symbol, crumb)
		if err == nil {
			return body, nil
		}

		var status *httpError
		if attempt == 0 && errors.As(err, &status) &&
			(status.status == http.StatusUnauthorized || status.status == http.StatusForbidden) {
			y.forgetCrumb()
			continue
		}
		return nil, err
	}
	// Unreachable: the loop either returns a body or returns the error that
	// stopped it. Present so the compiler does not have to take that on trust.
	return nil, errors.New("the quote source would not accept a session token")
}

// askSummary makes the request itself, with a crumb or without one.
func (y *Yahoo) askSummary(ctx context.Context, symbol, crumb string) ([]byte, error) {
	settings, _ := y.current()
	endpoint := fmt.Sprintf("%s/v10/finance/quoteSummary/%s?modules=%s&formatted=false",
		settings.BaseURL, url.PathEscape(symbol), url.QueryEscape("topHoldings,quoteType"))
	if crumb != "" {
		endpoint += "&crumb=" + url.QueryEscape(crumb)
	}
	return y.getAccepting(ctx, endpoint, anything)
}

// maxLogoBytes caps one image. Yahoo's brand images are a few kilobytes; a
// response past this is not a logo, whatever its content type claims.
const maxLogoBytes = 256 << 10

// Logo implements Iconographer.
//
// Yahoo has no logo endpoint. What it has is a `logoUrl` on some search
// results, so the symbol is looked up and the image behind that URL fetched —
// two requests, once per symbol ever, because the caller caches the answer
// including the answer "there isn't one".
//
// Most symbols hit that last case: funds, indices and crypto pairs have no
// brand image here at all. That is ErrNoLogo rather than a failure, and the
// distinction is the whole reason this returns a sentinel — a caller that
// treated "no logo" as an error would ask again forever.
func (y *Yahoo) Logo(ctx context.Context, symbol string, known LogoValidators) (Logo, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return Logo{}, errors.New("a symbol is required")
	}
	settings, _ := y.current()

	// A configured template wins outright. Yahoo's own `logoUrl` is absent far
	// more often than it is present — on plenty of accounts it is absent
	// always — and an operator who has found a source that works should not
	// have their choice second-guessed by a fallback that mostly returns
	// nothing.
	if settings.LogoURL != "" {
		return y.image(ctx, ExpandLogoURL(settings.LogoURL, symbol, settings.LogoKey), known)
	}

	endpoint := fmt.Sprintf("%s/v1/finance/search?q=%s&quotesCount=6&newsCount=0&listsCount=0",
		settings.BaseURL, url.QueryEscape(symbol))

	body, err := y.get(ctx, endpoint)
	if err != nil {
		// The symbol search not knowing a symbol says nothing about whether it
		// has a logo, and the caller must not cache "none" off the back of it.
		if errors.Is(err, ErrNotFound) {
			return Logo{}, ErrNoLogo
		}
		return Logo{}, err
	}
	var parsed searchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Logo{}, fmt.Errorf("decode search results for %s: %w", symbol, err)
	}

	src := ""
	for _, m := range parsed.Quotes {
		// Only this symbol's own logo will do. Search is fuzzy — asking for
		// GLD returns half a dozen funds — and taking the first result's
		// picture would put another company's mark on the row.
		if strings.EqualFold(m.Symbol, symbol) {
			src = strings.TrimSpace(m.LogoURL)
			break
		}
	}
	if src == "" {
		// Wrapped rather than bare, so the caller can record *why* it gave up
		// on a symbol. "No logo" and "no logo, because this source never
		// mentions one" look identical in a cache and are very different
		// things to have to debug.
		return Logo{}, fmt.Errorf("%w: the quote source's search result carries no logo", ErrNoLogo)
	}
	return y.image(ctx, src, known)
}

// image fetches one picture, given the URL a search result or a template
// produced.
//
// The request is conditional when there is anything to be conditional about.
// A logo is re-checked daily and changes when a company rebrands, so nearly
// every one of those checks should end in a 304 and no bytes at all — which is
// the difference between a re-check that is free and one that downloads the
// whole watchlist's pictures every day.
func (y *Yahoo) image(ctx context.Context, src string, known LogoValidators) (Logo, error) {
	settings, client := y.current()
	// Everything written down about this request uses the redacted form: the
	// real URL is used once, to make the request, and never again.
	safe := RedactLogoURL(src, settings.LogoKey)

	parsed, err := url.Parse(src)
	if err != nil {
		return Logo{}, fmt.Errorf("logo URL %q: %w", safe, err)
	}
	// The URL comes from a third party's JSON or an operator's settings and is
	// fetched by the server, so it is checked before it is followed rather than
	// after: file:// would make this a way to read the host's disk.
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return Logo{}, fmt.Errorf("logo URL %q is not http(s)", safe)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return Logo{}, err
	}
	req.Header.Set("User-Agent", settings.UserAgent)
	req.Header.Set("Accept", "image/*")
	if known.ETag != "" {
		req.Header.Set("If-None-Match", known.ETag)
	}
	if known.LastModified != "" {
		req.Header.Set("If-Modified-Since", known.LastModified)
	}
	// A key with nowhere to go in the URL is a server-side credential, and a
	// bearer header is where those belong: it stays out of the request line,
	// out of the other end's access log, and out of any redirect target.
	if settings.LogoKey != "" && !strings.Contains(settings.LogoURL, LogoKeyToken) {
		req.Header.Set("Authorization", "Bearer "+settings.LogoKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return Logo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return Logo{}, ErrLogoUnchanged
	}
	if resp.StatusCode == http.StatusNotFound {
		return Logo{}, fmt.Errorf("%w: nothing at %s", ErrNoLogo, safe)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Worth its own message: this is the one failure a key is the answer
		// to, and "HTTP 401" alone leaves the operator guessing which of the
		// URL, the key and the network is wrong.
		return Logo{}, fmt.Errorf("%s rejected the request (HTTP %d) — check the logo key",
			safe, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Logo{}, fmt.Errorf("logo fetch returned HTTP %d", resp.StatusCode)
	}

	// One byte past the cap, so a file exactly at the limit is kept and a file
	// over it is detected rather than silently truncated into a broken image.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLogoBytes+1))
	if err != nil {
		return Logo{}, err
	}
	if len(body) > maxLogoBytes {
		// Durable, so it is wrapped: a URL that answers with something this
		// big will do so again next cycle, and re-asking forever hides the
		// misconfiguration instead of reporting it.
		return Logo{}, fmt.Errorf("%w: %s answered with more than %d bytes", ErrNoLogo, safe, maxLogoBytes)
	}
	if len(body) == 0 {
		return Logo{}, fmt.Errorf("%w: %s answered with an empty body", ErrNoLogo, safe)
	}

	// Sniffed rather than trusted: the content type is what the browser will
	// be told, and a server that labels a PNG text/html would otherwise turn
	// the cache into a way to serve markup from this app's own origin.
	kind := http.DetectContentType(body)
	if !strings.HasPrefix(kind, "image/") {
		// Also durable, and the single most useful thing this can report: a
		// mistyped logo URL usually answers 200 with somebody's HTML, and
		// "answered with text/html, not an image" is the whole diagnosis.
		return Logo{}, fmt.Errorf("%w: %s answered with %s, not an image", ErrNoLogo, safe, kind)
	}
	// The stored provenance is the redacted URL too — the cache is read by
	// anyone who can open the database or the Settings page.
	return Logo{
		ContentType: kind,
		Bytes:       body,
		Source:      safe,
		Validators: LogoValidators{
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
		},
	}, nil
}

// maxBody caps how much of a response we will read. The endpoints return tens
// of kilobytes; anything past this is a proxy's error page or a captive
// portal, and reading it into memory on a Pi helps nobody.
const maxBody = 8 << 20

func (y *Yahoo) get(ctx context.Context, endpoint string) ([]byte, error) {
	return y.getAccepting(ctx, endpoint, "application/json")
}

// getAccepting is get with the Accept header spelled out.
//
// It exists because not everything on this API answers with JSON: the crumb is
// served as plain text, and asking for JSON gets an HTTP 406 from Yahoo's edge
// rather than a crumb. That is content negotiation working exactly as
// specified, and it is invisible until the one endpoint that isn't JSON is the
// one you need.
func (y *Yahoo) getAccepting(ctx context.Context, endpoint, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	settings, client := y.current()
	req.Header.Set("User-Agent", settings.UserAgent)
	req.Header.Set("Accept", accept)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A 404 from the chart endpoint is Yahoo's way of saying "no such
		// symbol", which is a user-fixable typo rather than an outage — so it
		// gets the sentinel and the UI can say so.
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		// Typed rather than formatted, so the crumbed path can recognise a 401
		// and re-handshake. The message is unchanged — every other caller still
		// only ever prints it.
		return nil, &httpError{status: resp.StatusCode, body: snippet(body)}
	}
	return body, nil
}

func lastClose(series []closeSeries) (float64, bool) {
	if len(series) == 0 {
		return 0, false
	}
	closes := series[0].Close
	for i := len(closes) - 1; i >= 0; i-- {
		if closes[i] != nil {
			return *closes[i], true
		}
	}
	return 0, false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// snippet trims a response body down to something loggable.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	if s == "" {
		s = "(empty response)"
	}
	return s
}
