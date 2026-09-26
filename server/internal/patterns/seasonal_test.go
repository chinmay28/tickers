package patterns

import (
	"math"
	"testing"
	"time"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func find(t *testing.T, buckets []Bucket, key string) Bucket {
	t.Helper()
	for _, b := range buckets {
		if b.Key == key {
			return b
		}
	}
	t.Fatalf("no bucket %q in %+v", key, buckets)
	return Bucket{}
}

// weekdays is n weekday bars from Monday 2024-01-01, closing up 1% on
// Mondays and flat otherwise.
func weekdays(n int) []quotes.Candle {
	var out []quotes.Candle
	p := 100.0
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); len(out) < n; d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		if d.Weekday() == time.Monday {
			p *= 1.01
		}
		out = append(out, quotes.Candle{Time: d, Open: p, High: p, Low: p, Close: p})
	}
	return out
}

func TestAMondayEffect(t *testing.T) {
	s, err := NewSeasonality(ByWeekday, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Add(weekdays(100), time.Time{})
	buckets, all := s.Result()
	if len(buckets) != 5 || buckets[0].Key != "Monday" {
		t.Fatalf("buckets = %+v, want Monday to Friday in order", buckets)
	}
	mon, tue := find(t, buckets, "Monday"), find(t, buckets, "Tuesday")
	near(t, "Monday's mean", mon.Mean, 1)
	near(t, "Monday's win rate", mon.WinRate, 100)
	near(t, "Tuesday's mean", tue.Mean, 0)
	if all.Count != 99 || mon.Count != 19 {
		t.Errorf("counts all %d, Monday %d: want every return after the first bar, and the 19 Mondays after it", all.Count, mon.Count)
	}
	// Monday doesn't vary, so its t-stat has no spread to divide by.
	if mon.Stdev != 0 || mon.TStat != 0 {
		t.Errorf("Monday = %+v, want no spread", mon)
	}
}

func TestTheTurnOfTheMonth(t *testing.T) {
	s, _ := NewSeasonality(ByTurnOfMonth, 0)
	bars := weekdays(60) // January's 23, February's 21, and 16 of March
	s.Add(bars, time.Time{})
	buckets, _ := s.Result()
	// January 2024's last trading days are the 29th, 30th and 31st.
	if b := find(t, buckets, "-1"); b.Count != 2 {
		t.Errorf("the last day of the month was seen %d times, want January's and February's — March isn't over", b.Count)
	}
	if b := find(t, buckets, "+1"); b.Count != 2 {
		t.Errorf("the first day of the month was seen %d times, want February's and March's (January's has no close before it)", b.Count)
	}
	dom, _ := NewSeasonality(ByDayOfMonth, 0)
	dom.Add(bars, time.Time{})
	db, _ := dom.Result()
	if db[0].Key != "+1" || db[len(db)-1].Key != "-1" {
		t.Errorf("day-of-month buckets run %s … %s, want +1 first and -1 last", db[0].Key, db[len(db)-1].Key)
	}
}

func TestTimeOfDaySlicesTheSession(t *testing.T) {
	// Two sessions of six 5-minute bars from 14:30 UTC: rising 1% a bar
	// in the first half hour, flat after — and the second opening 2%
	// above the first's close.
	var bars []quotes.Candle
	p := 100.0
	for day := 0; day < 2; day++ {
		open := time.Date(2024, 3, 4+day, 14, 30, 0, 0, time.UTC)
		if day == 1 {
			p *= 1.02
		}
		for k := 0; k < 12; k++ {
			o := p
			if k < 6 {
				p *= 1.01
			}
			bars = append(bars, quotes.Candle{Time: open.Add(time.Duration(5*k) * time.Minute), Open: o, High: p, Low: o, Close: p})
		}
	}
	s, _ := NewSeasonality(ByTimeOfDay, 30)
	s.Add(bars, time.Time{})
	buckets, _ := s.Result()
	if len(buckets) != 3 || buckets[0].Key != "overnight" || buckets[1].Key != "+00:00" || buckets[2].Key != "+00:30" {
		t.Fatalf("buckets = %+v, want overnight, then the first and second half hours", buckets)
	}
	near(t, "the overnight gap", buckets[0].Mean, 2)
	near(t, "the first half hour", buckets[1].Mean, (math.Pow(1.01, 6)-1)*100)
	near(t, "the second half hour", buckets[2].Mean, 0)
	if buckets[1].Count != 2 {
		t.Errorf("the first half hour was seen %d times, want once a session", buckets[1].Count)
	}
	if _, err := NewSeasonality(ByTimeOfDay, 0); err == nil {
		t.Error("a zero-minute bucket was accepted")
	}
	if _, err := NewSeasonality("lunar", 0); err == nil {
		t.Error("an unknown grouping was accepted")
	}
}
