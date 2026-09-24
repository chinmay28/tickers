package quotes

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Interval is a bar width. The archive collects at a fixed set of them, and
// the string is what goes both into the provider's request and into the
// archive's rows, so it is the same short spelling every charting tool uses.
type Interval string

const (
	Daily      Interval = "1d"
	Hourly     Interval = "1h"
	FiveMinute Interval = "5m"
	OneMinute  Interval = "1m"
)

// Intervals is every width the archive knows, widest first.
var Intervals = []Interval{Daily, Hourly, FiveMinute, OneMinute}

// ParseInterval turns a spelling back into an Interval, refusing anything the
// archive has no policy for — a typo in a flag should stop the process, not
// silently collect nothing.
func ParseInterval(s string) (Interval, error) {
	for _, i := range Intervals {
		if string(i) == s {
			return i, nil
		}
	}
	return "", fmt.Errorf("unknown interval %q (want one of 1d, 1h, 5m, 1m)", s)
}

// Step is how much time one bar covers. A daily bar is a calendar day here,
// which is what its key is measured in; the session inside it is shorter.
func (i Interval) Step() time.Duration {
	switch i {
	case Daily:
		return 24 * time.Hour
	case Hourly:
		return time.Hour
	case FiveMinute:
		return 5 * time.Minute
	case OneMinute:
		return time.Minute
	}
	return 0
}

// Intraday reports whether a bar is a slice of a session rather than a whole
// one — the distinction that decides how a bar's time is keyed and whether an
// unfinished one can be kept.
func (i Interval) Intraday() bool { return i != Daily }

// Candle is one OHLCV bar, exactly as the provider printed it.
//
// Prices are split-adjusted as of the moment they were fetched — that is how
// Yahoo serves every series, and there is no unadjusted one to ask for
// instead. They are *not* dividend-adjusted: dividends arrive separately, so
// an adjusted close can be derived at read time instead of rewritten into
// every row each time a stock pays one.
type Candle struct {
	// Time is when the bar opened. For a daily bar it is the exchange's own
	// calendar date at 00:00 UTC rather than an instant, because the instant
	// Yahoo stamps a daily bar with moves — the in-progress bar carries the
	// last trade's time, the settled one the session's open — and a key that
	// moves writes the same day twice.
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
	// VWAP is the bar's volume-weighted average price, and Trades how many
	// trades made it up — both as the source computed them from every trade,
	// which a VWAP rebuilt from OHLC can only approximate. Zero means the
	// source didn't say: Yahoo never does, Polygon always does.
	VWAP   float64
	Trades int64
	// Session says which part of the trading day the bar belongs to. Every
	// bar is Regular unless it was asked for with ExtendedArchivist.
	Session Session
}

// Session is a part of the trading day.
type Session uint8

const (
	// Regular is the exchange's main session — 9:30 to 16:00 in New York —
	// and the whole day for anything that trades around the clock.
	Regular Session = iota
	// PreMarket is before the regular open.
	PreMarket
	// AfterHours is after the regular close.
	AfterHours
)

func (s Session) String() string {
	switch s {
	case PreMarket:
		return "pre"
	case AfterHours:
		return "post"
	}
	return "regular"
}

// Split is a stock split, effective from Time. A 4:1 split has Numerator 4 and
// Denominator 1: a price before it is divided by four to be comparable with a
// price after it.
type Split struct {
	Time        time.Time
	Numerator   float64
	Denominator float64
}

// Ratio is what a pre-split price is multiplied by to be on the post-split
// basis. Zero for a split with no usable ratio, which callers must skip rather
// than multiply by.
func (s Split) Ratio() float64 {
	if s.Numerator <= 0 || s.Denominator <= 0 {
		return 0
	}
	return s.Denominator / s.Numerator
}

// Dividend is one cash distribution per share, dated by its ex-date.
type Dividend struct {
	Time   time.Time
	Amount float64
}

// CandleSeries is one answer from the provider: the bars in a window, and the
// corporate actions that fell inside it.
type CandleSeries struct {
	Candles   []Candle
	Splits    []Split
	Dividends []Dividend
	// FirstTrade is the earliest day the provider has anything for this
	// symbol, or zero when it didn't say. It is what tells a backfill it has
	// reached the beginning rather than a quiet stretch.
	FirstTrade time.Time
}

// Archivist is a provider that can return OHLCV bars over an explicit window
// at a chosen width, for the long-term archive.
//
// It is optional, like Historian: the refresh loop never needs it, and a
// source that can only price today is still a complete Provider. The archive
// collector asserts for it and refuses to start without it.
type Archivist interface {
	// Candles returns the bars in [from, to), oldest first, along with any
	// splits and dividends in the same window. A window with no trading in it
	// comes back empty rather than as an error. The edges are the provider's
	// to round — a daily bar keyed by its date can sit a few hours before
	// from — so callers treat the window as coverage, not as a filter.
	Candles(ctx context.Context, symbol string, interval Interval, from, to time.Time) (CandleSeries, error)
}

// ExtendedArchivist is an Archivist that can also return the bars outside
// the regular session, each tagged with its Session.
//
// A separate method rather than a flag on Candles, so a source that only has
// the regular session is still a complete Archivist, and asking for the
// extended session is a decision the caller visibly makes: a series that is
// regular hours some days and extended on others makes every intraday
// indicator lie.
type ExtendedArchivist interface {
	ExtendedCandles(ctx context.Context, symbol string, interval Interval, from, to time.Time) (CandleSeries, error)
}

// TickerPeriod is one symbol a company traded under, from From until the
// next period's From.
type TickerPeriod struct {
	Symbol string
	From   time.Time
}

// Renamer is a source that knows the symbols a company has traded under —
// FB until 9 June 2022, META from then — so history kept under the old one
// can be read as part of the new one's.
type Renamer interface {
	// TickerHistory returns a symbol's periods, oldest first, the current
	// symbol last. A symbol that was never renamed comes back as one period.
	TickerHistory(ctx context.Context, symbol string) ([]TickerPeriod, error)
}

// ErrRateLimited means the provider has asked us to slow down. It is distinct
// from every other failure because it says nothing about the symbol that was
// asked for: the right response is to wait, not to give up on that symbol.
var ErrRateLimited = errors.New("the quote provider is rate limiting this host")
