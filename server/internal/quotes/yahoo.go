package quotes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// YahooName is the provider's identifier.
const YahooName = "yahoo"

// DefaultUserAgent is sent on every request.
//
// Yahoo's public endpoints answer a browser and stonewall an obvious script,
// which is the single most common reason a self-hosted quote fetcher starts
// returning nothing after working for months. yfinance sets a browser UA for
// the same reason; this is that, made explicit.
const DefaultUserAgent = "Mozilla/5.0 (X11; Linux aarch64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// maxConcurrent caps in-flight requests. A watchlist is a handful of symbols,
// and firing all of them at once from a Raspberry Pi on a domestic connection
// is a good way to collect timeouts rather than quotes.
const maxConcurrent = 4

// Yahoo reads quotes from Yahoo Finance's public chart endpoint — the same
// data yfinance returns for `history(period="1d", interval="1m")`.
type Yahoo struct {
	// BaseURL is the API root. Overridden in tests; leave empty in production.
	BaseURL string
	// Client is the HTTP client. Leave nil for a sensible default.
	Client *http.Client
	// UserAgent overrides DefaultUserAgent.
	UserAgent string
}

// NewYahoo builds a provider with a timeout-bounded client.
func NewYahoo(timeout time.Duration) *Yahoo {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Yahoo{Client: &http.Client{Timeout: timeout}}
}

// Name implements Provider.
func (y *Yahoo) Name() string { return YahooName }

func (y *Yahoo) baseURL() string {
	if y.BaseURL != "" {
		return strings.TrimRight(y.BaseURL, "/")
	}
	return "https://query1.finance.yahoo.com"
}

func (y *Yahoo) client() *http.Client {
	if y.Client != nil {
		return y.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (y *Yahoo) userAgent() string {
	if y.UserAgent != "" {
		return y.UserAgent
	}
	return DefaultUserAgent
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

// chartResponse is the slice of Yahoo's /v8/finance/chart payload we use.
type chartResponse struct {
	Chart struct {
		Result []struct {
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
			} `json:"meta"`
			Indicators struct {
				Quote []struct {
					Close []*float64 `json:"close"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

func (y *Yahoo) fetchOne(ctx context.Context, symbol string) (Quote, error) {
	endpoint := fmt.Sprintf("%s/v8/finance/chart/%s?range=1d&interval=1m&includePrePost=false",
		y.baseURL(), url.PathEscape(symbol))

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

// searchResponse is the slice of Yahoo's /v1/finance/search payload we use.
type searchResponse struct {
	Quotes []struct {
		Symbol    string `json:"symbol"`
		ShortName string `json:"shortname"`
		LongName  string `json:"longname"`
		ExchDisp  string `json:"exchDisp"`
		TypeDisp  string `json:"typeDisp"`
		QuoteType string `json:"quoteType"`
	} `json:"quotes"`
}

// Search implements Provider.
func (y *Yahoo) Search(ctx context.Context, query string) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []Match{}, nil
	}
	endpoint := fmt.Sprintf("%s/v1/finance/search?q=%s&quotesCount=10&newsCount=0&listsCount=0",
		y.baseURL(), url.QueryEscape(query))

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

// maxBody caps how much of a response we will read. The endpoints return tens
// of kilobytes; anything past this is a proxy's error page or a captive
// portal, and reading it into memory on a Pi helps nobody.
const maxBody = 8 << 20

func (y *Yahoo) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", y.userAgent())
	req.Header.Set("Accept", "application/json")

	resp, err := y.client().Do(req)
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
		return nil, fmt.Errorf("quote provider returned HTTP %d: %s",
			resp.StatusCode, snippet(body))
	}
	return body, nil
}

func lastClose(series []struct {
	Close []*float64 `json:"close"`
}) (float64, bool) {
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
