package quotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// candleResponse is the slice of /v8/finance/chart the archive reads. It is
// its own shape rather than an extension of chartResponse because it wants
// everything the quote path ignores — the whole OHLCV block, splits, the first
// trade date — and widening the shared struct would have every watchlist
// refresh decode columns it throws away.
type candleResponse struct {
	Chart struct {
		Result []struct {
			Meta struct {
				GMTOffset      int64  `json:"gmtoffset"`
				FirstTradeDate *int64 `json:"firstTradeDate"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
			Events struct {
				Dividends map[string]struct {
					Amount float64 `json:"amount"`
					Date   int64   `json:"date"`
				} `json:"dividends"`
				Splits map[string]struct {
					Date        int64   `json:"date"`
					Numerator   float64 `json:"numerator"`
					Denominator float64 `json:"denominator"`
				} `json:"splits"`
			} `json:"events"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// Candles implements Archivist.
//
// It always asks for `events=div,splits`, whatever the width. That is not only
// for the dividends: the archive has to learn about a split from the same
// response whose prices already reflect it, or it cannot tell which of its
// stored rows are on the old basis. See archive.Record.
func (y *Yahoo) Candles(ctx context.Context, symbol string, interval Interval, from, to time.Time) (CandleSeries, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return CandleSeries{}, errors.New("a symbol is required")
	}
	if interval.Step() == 0 {
		return CandleSeries{}, fmt.Errorf("unknown interval %q", interval)
	}
	if !from.Before(to) {
		return CandleSeries{}, fmt.Errorf("empty window %s–%s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	settings, _ := y.current()
	endpoint := fmt.Sprintf("%s/v8/finance/chart/%s?period1=%d&period2=%d&interval=%s&includePrePost=false&events=div%%2Csplits",
		settings.BaseURL, url.PathEscape(symbol), from.Unix(), to.Unix(), interval)

	body, err := y.get(ctx, endpoint)
	if err != nil {
		return CandleSeries{}, err
	}
	var parsed candleResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return CandleSeries{}, fmt.Errorf("decode candles for %s: %w", symbol, err)
	}
	if parsed.Chart.Error != nil {
		return CandleSeries{}, fmt.Errorf("%s: %s", symbol, parsed.Chart.Error.Description)
	}
	if len(parsed.Chart.Result) == 0 {
		return CandleSeries{}, fmt.Errorf("%s: %w", symbol, ErrNotFound)
	}
	res := parsed.Chart.Result[0]
	offset := res.Meta.GMTOffset

	out := CandleSeries{}
	if res.Meta.FirstTradeDate != nil {
		out.FirstTrade = exchangeDay(*res.Meta.FirstTradeDate, offset)
	}

	if len(res.Indicators.Quote) > 0 {
		q := res.Indicators.Quote[0]
		out.Candles = make([]Candle, 0, len(res.Timestamp))
		for i, ts := range res.Timestamp {
			// A bar missing any of its four prices is a slot Yahoo emits for a
			// halt or a holiday. Keeping it with a price filled in from a
			// neighbour would be inventing a trade, and an archive is exactly
			// where an invented number is never found again.
			o, h, l, c := at(q.Open, i), at(q.High, i), at(q.Low, i), at(q.Close, i)
			if o == nil || h == nil || l == nil || c == nil {
				continue
			}
			candle := Candle{Open: *o, High: *h, Low: *l, Close: *c}
			// Indices and currencies have no volume, and Yahoo says so with a
			// null or a zero. Both are stored as zero.
			if v := at(q.Volume, i); v != nil {
				candle.Volume = int64(*v)
			}
			if interval.Intraday() {
				candle.Time = time.Unix(ts, 0).UTC()
			} else {
				candle.Time = exchangeDay(ts, offset)
			}
			out.Candles = append(out.Candles, candle)
		}
		sort.SliceStable(out.Candles, func(i, j int) bool { return out.Candles[i].Time.Before(out.Candles[j].Time) })
	}

	// Corporate actions are keyed by the exchange's day, not the instant Yahoo
	// stamps them with (the ex-date's open). A daily bar is keyed by that same
	// day, so "every bar before the split" excludes the ex-date's own bar —
	// which is already on the new basis — for daily and intraday rows alike.
	for _, s := range res.Events.Splits {
		out.Splits = append(out.Splits, Split{
			Time:        exchangeDay(s.Date, offset),
			Numerator:   s.Numerator,
			Denominator: s.Denominator,
		})
	}
	sort.Slice(out.Splits, func(i, j int) bool { return out.Splits[i].Time.Before(out.Splits[j].Time) })
	for _, d := range res.Events.Dividends {
		if d.Amount == 0 {
			continue
		}
		out.Dividends = append(out.Dividends, Dividend{Time: exchangeDay(d.Date, offset), Amount: d.Amount})
	}
	sort.Slice(out.Dividends, func(i, j int) bool { return out.Dividends[i].Time.Before(out.Dividends[j].Time) })
	return out, nil
}

// exchangeDay is the exchange's calendar date for an instant, as midnight UTC.
func exchangeDay(ts, offset int64) time.Time {
	d := time.Unix(ts+offset, 0).UTC()
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
}

func at(series []*float64, i int) *float64 {
	if i < len(series) {
		return series[i]
	}
	return nil
}
