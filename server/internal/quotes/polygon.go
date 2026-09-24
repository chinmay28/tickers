package quotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the regular session is defined in New York time, on every host
)

// PolygonName is the provenance Polygon's bars carry.
const PolygonName = "polygon"

// PolygonBaseURL is Polygon's API root. Polygon now also answers as Massive;
// either host serves the same API, and the base URL is a setting.
const PolygonBaseURL = "https://api.polygon.io"

// Polygon reads OHLCV bars from Polygon.io's aggregates API, for backfilling
// the archive past what Yahoo keeps — minute bars years back, on a paid plan.
//
// It implements Archivist and nothing else: it is a source of history, never
// the watchlist's live quotes, so the refresh loop and every existing feature
// are untouched by whether a key is configured.
//
// Two things make its bars interchangeable with Yahoo's, which is Archivist's
// contract and what lets the archive mix them:
//
//   - Prices are asked for split-adjusted (`adjusted=true`), the basis Yahoo
//     serves, and the window's splits are reported alongside so the archive
//     can bring older bars onto it.
//   - US equity bars outside the regular session are dropped. Polygon serves
//     4:00–20:00 New York time; Yahoo, asked without pre/post, 9:30–16:00. A
//     series that is regular hours some days and extended on others would
//     make every intraday indicator lie.
type Polygon struct {
	base   string
	key    string
	client *http.Client

	mu     sync.Mutex
	splits map[string]polygonSplits
}

type polygonSplits struct {
	at     time.Time
	splits []Split
}

// splitsTTL is how long a symbol's split history is trusted. A backfill walks
// one symbol through dozens of windows in a row; asking for its splits once
// rather than once per window is the difference between one extra request per
// symbol and doubling the request count.
const splitsTTL = 12 * time.Hour

// NewPolygon builds the source. An empty base URL means Polygon's own.
func NewPolygon(baseURL, key string, timeout time.Duration) *Polygon {
	if baseURL == "" {
		baseURL = PolygonBaseURL
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Polygon{
		base:   strings.TrimRight(baseURL, "/"),
		key:    key,
		client: &http.Client{Timeout: timeout},
		splits: map[string]polygonSplits{},
	}
}

// ErrUnsupportedSymbol means a source has no way to name a symbol — an index
// on a plan without indices, a future. It is an answer about the symbol, so
// the collector backs off on it rather than pausing the source.
var ErrUnsupportedSymbol = errors.New("this source cannot fetch that symbol")

// polygonIndices maps the Yahoo index symbols the archive collects by default
// to Polygon's. Polygon serves indices only on an indices plan; without one
// the request fails and the collector backs off.
var polygonIndices = map[string]string{
	"^GSPC": "I:SPX",
	"^DJI":  "I:DJI",
	"^IXIC": "I:COMP",
	"^RUT":  "I:RUT",
	"^VIX":  "I:VIX",
	"^NDX":  "I:NDX",
}

var cryptoPair = regexp.MustCompile(`^([A-Z0-9]{2,})-(USD|USDT|USDC|EUR|GBP)$`)

// PolygonSymbol translates a Yahoo symbol into Polygon's spelling.
func PolygonSymbol(yahoo string) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(yahoo))
	if idx, ok := polygonIndices[s]; ok {
		return idx, nil
	}
	if m := cryptoPair.FindStringSubmatch(s); m != nil {
		return "X:" + m[1] + m[2], nil
	}
	if s == "" || strings.ContainsAny(s, "^=") {
		return "", fmt.Errorf("%s: %w", yahoo, ErrUnsupportedSymbol)
	}
	// A share class: Yahoo's BRK-B is Polygon's BRK.B.
	return strings.ReplaceAll(s, "-", "."), nil
}

// polygonSpan maps an interval to Polygon's multiplier and timespan.
func polygonSpan(i Interval) (int, string, error) {
	switch i {
	case Daily:
		return 1, "day", nil
	case Hourly:
		return 1, "hour", nil
	case FiveMinute:
		return 5, "minute", nil
	case OneMinute:
		return 1, "minute", nil
	}
	return 0, "", fmt.Errorf("unknown interval %q", i)
}

type polygonAggs struct {
	Status  string `json:"status"`
	Error   string `json:"error"`
	Message string `json:"message"`
	Results []struct {
		O  float64 `json:"o"`
		H  float64 `json:"h"`
		L  float64 `json:"l"`
		C  float64 `json:"c"`
		V  float64 `json:"v"`
		VW float64 `json:"vw"`
		N  int64   `json:"n"`
		T  int64   `json:"t"`
	} `json:"results"`
	NextURL string `json:"next_url"`
}

// maxPolygonPages bounds the pagination of one window. Fifty thousand bars a
// page is a month of extended-hours minute bars; a window that needs more
// than a few pages is a window the collector sized wrong.
const maxPolygonPages = 20

// Candles implements Archivist.
func (p *Polygon) Candles(ctx context.Context, symbol string, interval Interval, from, to time.Time) (CandleSeries, error) {
	return p.candles(ctx, symbol, interval, from, to, false)
}

// ExtendedCandles implements ExtendedArchivist. Polygon serves 4:00–20:00
// New York time whatever is asked; this keeps what Candles drops, tagged.
func (p *Polygon) ExtendedCandles(ctx context.Context, symbol string, interval Interval, from, to time.Time) (CandleSeries, error) {
	return p.candles(ctx, symbol, interval, from, to, true)
}

func (p *Polygon) candles(ctx context.Context, symbol string, interval Interval, from, to time.Time, extended bool) (CandleSeries, error) {
	if p.key == "" {
		return CandleSeries{}, errors.New("polygon: no API key configured")
	}
	ticker, err := PolygonSymbol(symbol)
	if err != nil {
		return CandleSeries{}, err
	}
	mult, span, err := polygonSpan(interval)
	if err != nil {
		return CandleSeries{}, err
	}
	if !from.Before(to) {
		return CandleSeries{}, fmt.Errorf("empty window %s–%s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	// Millisecond bounds: Polygon reads a date as the whole day, and a window
	// has to end where it says, not at the next midnight.
	endpoint := fmt.Sprintf("%s/v2/aggs/ticker/%s/range/%d/%s/%d/%d?adjusted=true&sort=asc&limit=50000",
		p.base, url.PathEscape(ticker), mult, span, from.UnixMilli(), to.Add(-time.Millisecond).UnixMilli())

	equity := !strings.Contains(ticker, ":")
	var out CandleSeries
	for page := 0; endpoint != "" && page < maxPolygonPages; page++ {
		var resp polygonAggs
		if err := p.get(ctx, endpoint, &resp); err != nil {
			return CandleSeries{}, fmt.Errorf("%s: %w", symbol, err)
		}
		for _, r := range resp.Results {
			at := time.UnixMilli(r.T).UTC()
			session := Regular
			if interval.Intraday() {
				if equity {
					session = nySession(at)
				}
				if session != Regular && !extended {
					continue
				}
			} else {
				// Polygon stamps a daily bar at midnight New York time, which
				// is the same calendar date in UTC.
				at = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
			}
			out.Candles = append(out.Candles, Candle{
				Time: at, Open: r.O, High: r.H, Low: r.L, Close: r.C, Volume: int64(r.V),
				VWAP: r.VW, Trades: r.N, Session: session,
			})
		}
		endpoint = resp.NextURL
	}
	sort.SliceStable(out.Candles, func(i, j int) bool { return out.Candles[i].Time.Before(out.Candles[j].Time) })

	if equity {
		splits, err := p.splitsFor(ctx, ticker)
		if err != nil {
			return CandleSeries{}, fmt.Errorf("%s: splits: %w", symbol, err)
		}
		for _, s := range splits {
			if !s.Time.Before(from.Truncate(24*time.Hour)) && s.Time.Before(to) {
				out.Splits = append(out.Splits, s)
			}
		}
	}
	return out, nil
}

var newYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		// time/tzdata is embedded, so this cannot happen; a fixed offset is
		// wrong for half the year, which is better than a panic.
		return time.FixedZone("EST", -5*3600)
	}
	return loc
}()

// nySession places a bar starting at t against 9:30–16:00 New York time.
//
// That is only meaningful for bars that start on the session's grid, which is
// why the collector never asks Polygon for hourly bars: Polygon's hours start
// on the clock, so its 9:00 bar is half pre-market. Hourly bars for the years
// it reaches are built from its minute bars instead, anchored to the open.
func nySession(t time.Time) Session {
	local := t.In(newYork)
	open := time.Date(local.Year(), local.Month(), local.Day(), 9, 30, 0, 0, newYork)
	close := time.Date(local.Year(), local.Month(), local.Day(), 16, 0, 0, 0, newYork)
	switch {
	case local.Before(open):
		return PreMarket
	case !local.Before(close):
		return AfterHours
	}
	return Regular
}

type polygonSplitResponse struct {
	Results []struct {
		ExecutionDate string  `json:"execution_date"`
		SplitFrom     float64 `json:"split_from"`
		SplitTo       float64 `json:"split_to"`
	} `json:"results"`
	NextURL string `json:"next_url"`
}

// splitsFor returns a ticker's whole split history, cached.
func (p *Polygon) splitsFor(ctx context.Context, ticker string) ([]Split, error) {
	p.mu.Lock()
	cached, ok := p.splits[ticker]
	p.mu.Unlock()
	if ok && time.Since(cached.at) < splitsTTL {
		return cached.splits, nil
	}
	endpoint := fmt.Sprintf("%s/v3/reference/splits?ticker=%s&limit=1000", p.base, url.QueryEscape(ticker))
	var out []Split
	for page := 0; endpoint != "" && page < maxPolygonPages; page++ {
		var resp polygonSplitResponse
		if err := p.get(ctx, endpoint, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Results {
			at, err := time.Parse("2006-01-02", r.ExecutionDate)
			if err != nil || r.SplitFrom <= 0 || r.SplitTo <= 0 {
				continue
			}
			// A 4-for-1 split is split_from 1, split_to 4: one old share
			// becomes four, so a pre-split price is divided by four.
			out = append(out, Split{Time: at, Numerator: r.SplitTo, Denominator: r.SplitFrom})
		}
		endpoint = resp.NextURL
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	p.mu.Lock()
	p.splits[ticker] = polygonSplits{at: time.Now(), splits: out}
	p.mu.Unlock()
	return out, nil
}

type polygonEvents struct {
	Results struct {
		Events []struct {
			Type         string `json:"type"`
			Date         string `json:"date"`
			TickerChange struct {
				Ticker string `json:"ticker"`
			} `json:"ticker_change"`
		} `json:"events"`
	} `json:"results"`
}

// TickerHistory implements Renamer, from Polygon's ticker events: every
// symbol the company behind a ticker has traded under, with the date each
// took effect.
func (p *Polygon) TickerHistory(ctx context.Context, symbol string) ([]TickerPeriod, error) {
	if p.key == "" {
		return nil, errors.New("polygon: no API key configured")
	}
	ticker, err := PolygonSymbol(symbol)
	if err != nil {
		return nil, err
	}
	if strings.Contains(ticker, ":") {
		// Crypto pairs and indices are never renamed.
		return []TickerPeriod{{Symbol: strings.ToUpper(symbol)}}, nil
	}
	var resp polygonEvents
	endpoint := fmt.Sprintf("%s/vX/reference/tickers/%s/events?types=ticker_change", p.base, url.PathEscape(ticker))
	if err := p.get(ctx, endpoint, &resp); err != nil {
		return nil, fmt.Errorf("%s: %w", symbol, err)
	}
	var out []TickerPeriod
	for _, e := range resp.Results.Events {
		if e.Type != "ticker_change" || e.TickerChange.Ticker == "" {
			continue
		}
		from, err := time.Parse(time.DateOnly, e.Date)
		if err != nil {
			continue
		}
		// Back into Yahoo's spelling, which is the archive's: BRK.B → BRK-B.
		out = append(out, TickerPeriod{Symbol: strings.ReplaceAll(strings.ToUpper(e.TickerChange.Ticker), ".", "-"), From: from})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	if len(out) == 0 {
		out = []TickerPeriod{{Symbol: strings.ToUpper(symbol)}}
	}
	return out, nil
}

// get fetches one Polygon endpoint into v. The key goes in a header, not the
// URL — URLs end up in logs, and next_url links come back without it.
func (p *Polygon) get(ctx context.Context, endpoint string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("polygon: %w", ErrRateLimited)
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		var e polygonAggs
		_ = json.Unmarshal(body, &e)
		msg := firstNonEmpty(e.Message, e.Error, snippet(body))
		return fmt.Errorf("polygon returned HTTP %d: %s", resp.StatusCode, msg)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("polygon: decode: %w", err)
	}
	return nil
}
