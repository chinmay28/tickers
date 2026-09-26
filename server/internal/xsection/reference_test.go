package xsection

import (
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/chinmay28/tickers/server/internal/quotes"
)

// TestRotateMatchesANaiveBook checks the book against the plainest possible
// reimplementation on irregular prices: rank on day d's close, put all the
// equity into the leader at d+1's open, mark at closes.
func TestRotateMatchesANaiveBook(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	series := map[string][]quotes.Candle{}
	for _, sym := range []string{"A", "B", "C"} {
		p := 100.0
		var bars []quotes.Candle
		for i := 0; i < 300; i++ {
			o := p * (1 + 0.01*rng.NormFloat64())
			c := o * (1 + 0.02*rng.NormFloat64())
			bars = append(bars, quotes.Candle{Time: t0.AddDate(0, 0, i), Open: o, High: math.Max(o, c), Low: math.Min(o, c), Close: c, Volume: 1})
			p = c
		}
		series[sym] = bars
	}
	p := NewPanel(series)
	f := mustFactor(t, "return:20")
	const from, every = 30, 7
	rot, err := Rotate(p, RotationSpec{Factor: f, From: from, Every: every, Hold: 1, Initial: 1000})
	if err != nil {
		t.Fatal(err)
	}

	values := f.Values(p)
	equity, held, shares := 1000.0, -1, 0.0
	var want []float64
	for i := from; i < len(p.Dates); i++ {
		if i > from && (i-from-1)%every == 0 {
			d := i - 1
			order := []int{0, 1, 2}
			sort.SliceStable(order, func(a, b int) bool { return values[order[a]][d] > values[order[b]][d] })
			if held >= 0 {
				equity = shares * p.Open[held][i]
			}
			held, shares = order[0], equity/p.Open[order[0]][i]
		}
		if held >= 0 {
			equity = shares * p.Close[held][i]
		}
		want = append(want, equity)
	}
	for i := range want {
		if math.Abs(rot.Equity[i]-want[i]) > 1e-6*want[i] {
			t.Fatalf("day %d: the book says %v, the naive book %v", from+i, rot.Equity[i], want[i])
		}
	}
}
