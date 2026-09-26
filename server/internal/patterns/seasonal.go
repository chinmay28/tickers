// Package patterns finds regularities in when returns happen: by weekday,
// by month, around the turn of the month, and through the trading day.
//
// It is pure like strategy and xsection: bars in, statistics out. Each
// observation is one return a trader could have held — a day's, or a slice
// of a session's — and every bucket is reported beside the average of all
// of them, because a Monday effect is only an effect relative to the other
// days.
package patterns

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// Groupings.
const (
	// ByWeekday groups daily returns by day of the week.
	ByWeekday = "weekday"
	// ByMonth groups them by calendar month.
	ByMonth = "month"
	// ByDayOfMonth groups them by the trading day's number in its month,
	// counted from the first ("+1") and, for the last five, from the end
	// ("-1" is the last).
	ByDayOfMonth = "dayOfMonth"
	// ByTurnOfMonth groups the last three and first three trading days of
	// each month apart from the rest: the classic turn-of-the-month effect.
	ByTurnOfMonth = "turnOfMonth"
	// ByTimeOfDay groups intraday returns by how long after the session's
	// first bar they happened, with the overnight gap on its own.
	ByTimeOfDay = "timeOfDay"
)

// Groupings lists them, for a caller to offer.
var Groupings = []string{ByWeekday, ByMonth, ByDayOfMonth, ByTurnOfMonth, ByTimeOfDay}

// Seasonality accumulates observations from one or more symbols' bars.
type Seasonality struct {
	group  string
	bucket int // minutes, for ByTimeOfDay
	obs    map[string][]float64
	all    []float64
}

// NewSeasonality starts an accumulation. bucketMinutes is only read by
// ByTimeOfDay, where it is the width of a slice of the session.
func NewSeasonality(group string, bucketMinutes int) (*Seasonality, error) {
	if !slices.Contains(Groupings, group) {
		return nil, strategy.Invalid("unknown grouping %q (want one of weekday, month, dayOfMonth, turnOfMonth, timeOfDay)", group)
	}
	if group == ByTimeOfDay && (bucketMinutes < 1 || bucketMinutes > 390) {
		return nil, strategy.Invalid("a time-of-day bucket must be from 1 to 390 minutes")
	}
	return &Seasonality{group: group, bucket: bucketMinutes, obs: map[string][]float64{}}, nil
}

// Intraday says whether the grouping reads intraday bars.
func (s *Seasonality) Intraday() bool { return s.group == ByTimeOfDay }

// Add takes one symbol's bars, oldest first, counting returns that end on
// or after from; the bars before it only supply the close each first return
// starts from.
func (s *Seasonality) Add(bars []quotes.Candle, from time.Time) {
	if s.Intraday() {
		s.addIntraday(bars, from)
		return
	}
	// Each trading day's place in its month, from the start and the end.
	pos, left := make([]int, len(bars)), make([]int, len(bars))
	for i := range bars {
		if i > 0 && sameMonth(bars[i].Time, bars[i-1].Time) {
			pos[i] = pos[i-1] + 1
		} else {
			pos[i] = 1
		}
	}
	for i := len(bars) - 1; i >= 0; i-- {
		if i+1 < len(bars) && sameMonth(bars[i].Time, bars[i+1].Time) {
			left[i] = left[i+1] + 1
		} else {
			left[i] = 1
		}
	}
	for i := 1; i < len(bars); i++ {
		b := bars[i]
		if b.Time.Before(from) || bars[i-1].Close <= 0 {
			continue
		}
		r := (b.Close/bars[i-1].Close - 1) * 100
		var key string
		switch s.group {
		case ByWeekday:
			key = b.Time.Weekday().String()
		case ByMonth:
			key = b.Time.Month().String()
		case ByDayOfMonth, ByTurnOfMonth:
			// The bars' last month may not be over — the data ends, not
			// necessarily the month — so its days aren't counted from an
			// end that may not be the end.
			fromEnd := left[i]
			if i+left[i] >= len(bars) {
				fromEnd = 99
			}
			key = monthKey(s.group, pos[i], fromEnd)
		}
		s.observe(key, r)
	}
}

func (s *Seasonality) addIntraday(bars []quotes.Candle, from time.Time) {
	var open time.Time
	var prevClose, sliceStart float64
	sliceKey := ""
	flush := func(close float64) {
		if sliceKey != "" && sliceStart > 0 {
			s.observe(sliceKey, (close/sliceStart-1)*100)
		}
		sliceKey = ""
	}
	var last float64
	for i, b := range bars {
		newSession := i == 0 || dayOf(b.Time) != dayOf(bars[i-1].Time)
		if newSession {
			flush(last)
			open = b.Time
			if i > 0 && !b.Time.Before(from) && prevClose > 0 {
				s.observe("overnight", (b.Open/prevClose-1)*100)
			}
		}
		mins := int(b.Time.Sub(open).Minutes()) / s.bucket * s.bucket
		key := fmt.Sprintf("+%02d:%02d", mins/60, mins%60)
		if key != sliceKey {
			flush(last)
			if !b.Time.Before(from) {
				sliceKey, sliceStart = key, b.Open
			}
		}
		last, prevClose = b.Close, b.Close
	}
	flush(last)
}

func (s *Seasonality) observe(key string, r float64) {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return
	}
	s.obs[key] = append(s.obs[key], r)
	s.all = append(s.all, r)
}

// Bucket is one group's returns, in percent.
type Bucket struct {
	Key     string  `json:"key"`
	Count   int     `json:"count"`
	Mean    float64 `json:"mean"`
	Median  float64 `json:"median"`
	Stdev   float64 `json:"stdev"`
	WinRate float64 `json:"winRate"`
	// TStat is how many standard errors the bucket's mean is from the
	// mean of every observation: whether it differs from the rest, not
	// whether it differs from zero.
	TStat float64 `json:"tStat"`
}

// Result is every bucket in its natural order, and all of them together.
func (s *Seasonality) Result() (buckets []Bucket, all Bucket) {
	all = summarize("all", s.all, 0)
	for _, key := range s.order() {
		if xs := s.obs[key]; len(xs) > 0 {
			buckets = append(buckets, summarize(key, xs, all.Mean))
		}
	}
	return buckets, all
}

func (s *Seasonality) order() []string {
	switch s.group {
	case ByWeekday:
		return []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	case ByMonth:
		var out []string
		for m := time.January; m <= time.December; m++ {
			out = append(out, m.String())
		}
		return out
	case ByTurnOfMonth:
		return []string{"-3", "-2", "-1", "+1", "+2", "+3", "rest"}
	case ByDayOfMonth:
		var out []string
		for d := 1; d <= 23; d++ {
			out = append(out, fmt.Sprintf("+%d", d))
		}
		for d := 5; d >= 1; d-- {
			out = append(out, fmt.Sprintf("-%d", d))
		}
		return out
	}
	keys := make([]string, 0, len(s.obs))
	for k := range s.obs {
		if k != "overnight" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return append([]string{"overnight"}, keys...)
}

// monthKey places a trading day by its number from the month's start and
// end. The last five days are counted from the end, which is where the
// effects people look for are; the rest from the start.
func monthKey(group string, fromStart, fromEnd int) string {
	if group == ByTurnOfMonth {
		switch {
		case fromEnd <= 3:
			return fmt.Sprintf("-%d", fromEnd)
		case fromStart <= 3:
			return fmt.Sprintf("+%d", fromStart)
		}
		return "rest"
	}
	if fromEnd <= 5 {
		return fmt.Sprintf("-%d", fromEnd)
	}
	return fmt.Sprintf("+%d", fromStart)
}

func summarize(key string, xs []float64, overall float64) Bucket {
	b := Bucket{Key: key, Count: len(xs)}
	if len(xs) == 0 {
		return b
	}
	sorted := slices.Sorted(slices.Values(xs))
	var sum float64
	wins := 0
	for _, x := range xs {
		sum += x
		if x > 0 {
			wins++
		}
	}
	n := float64(len(xs))
	b.Mean, b.WinRate = sum/n, float64(wins)/n*100
	if m := len(sorted) / 2; len(sorted)%2 == 1 {
		b.Median = sorted[m]
	} else {
		b.Median = (sorted[m-1] + sorted[m]) / 2
	}
	if len(xs) > 1 {
		var ss float64
		for _, x := range xs {
			ss += (x - b.Mean) * (x - b.Mean)
		}
		b.Stdev = math.Sqrt(ss / (n - 1))
		// Returns that don't vary still leave rounding dust in the sum of
		// squares; dividing by it would report a certainty of 10¹⁵.
		if b.Stdev < 1e-9 {
			b.Stdev = 0
		}
		if b.Stdev > 0 {
			b.TStat = (b.Mean - overall) / (b.Stdev / math.Sqrt(n))
		}
	}
	return b
}

func sameMonth(a, b time.Time) bool { return a.Year() == b.Year() && a.Month() == b.Month() }

// dayOf is a bar's session. Intraday bars are UTC instants and a US session
// never crosses midnight UTC, so the UTC date is the session.
func dayOf(t time.Time) int64 { return t.Unix() / 86400 }
