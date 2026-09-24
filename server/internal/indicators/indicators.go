// Package indicators computes technical indicators from OHLCV bars.
//
// Every function is pure: slices in, slices out, one output per input bar,
// NaN where the indicator is not yet defined (the first n-1 bars of an n-bar
// average). Nothing here knows about the archive, the API or time — which is
// what lets each formula be checked against values worked by hand.
//
// The smoothing conventions are the ones charting platforms use, so a number
// here matches the number on anyone else's chart: EMAs are seeded with the
// SMA of their first n values, and RSI and ATR use Wilder's smoothing (an EMA
// with α = 1/n), not a plain EMA.
package indicators

import (
	"math"
)

// nan is the value for "not defined yet".
var nan = math.NaN()

func filled(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = nan
	}
	return out
}

// SMA is the simple moving average of the last n values.
func SMA(x []float64, n int) []float64 {
	out := filled(len(x))
	if n <= 0 {
		return out
	}
	sum, count := 0.0, 0
	for i, v := range x {
		if math.IsNaN(v) {
			// A gap restarts the window: an average across an undefined
			// value is not an average of n values.
			sum, count = 0, 0
			continue
		}
		sum += v
		count++
		if count > n {
			sum -= x[i-n]
			count = n
		}
		if count == n {
			out[i] = sum / float64(n)
		}
	}
	return out
}

// EMA is the exponential moving average with α = 2/(n+1), seeded with the
// SMA of the first n defined values.
func EMA(x []float64, n int) []float64 {
	return smooth(x, n, 2/float64(n+1))
}

// wilder is Wilder's smoothing: an EMA with α = 1/n, seeded the same way.
func wilder(x []float64, n int) []float64 {
	return smooth(x, n, 1/float64(n))
}

func smooth(x []float64, n int, alpha float64) []float64 {
	out := filled(len(x))
	if n <= 0 {
		return out
	}
	seed, count := 0.0, 0
	prev := nan
	for i, v := range x {
		if math.IsNaN(v) {
			continue
		}
		if math.IsNaN(prev) {
			seed += v
			count++
			if count == n {
				prev = seed / float64(n)
				out[i] = prev
			}
			continue
		}
		prev = prev + alpha*(v-prev)
		out[i] = prev
	}
	return out
}

// RSI is Wilder's relative strength index over n periods, 0–100.
func RSI(close []float64, n int) []float64 {
	out := filled(len(close))
	if len(close) < 2 || n <= 0 {
		return out
	}
	gains, losses := filled(len(close)), filled(len(close))
	for i := 1; i < len(close); i++ {
		d := close[i] - close[i-1]
		gains[i], losses[i] = math.Max(d, 0), math.Max(-d, 0)
	}
	ag, al := wilder(gains, n), wilder(losses, n)
	for i := range out {
		switch {
		case math.IsNaN(ag[i]) || math.IsNaN(al[i]):
		case al[i] == 0 && ag[i] == 0:
			// Not a single move in the window: neither overbought nor
			// oversold.
			out[i] = 50
		case al[i] == 0:
			out[i] = 100
		default:
			out[i] = 100 - 100/(1+ag[i]/al[i])
		}
	}
	return out
}

// MACD is the difference of a fast and a slow EMA, its signal line (an EMA
// of the difference) and the histogram between them.
func MACD(close []float64, fast, slow, signal int) (macd, sig, hist []float64) {
	f, s := EMA(close, fast), EMA(close, slow)
	macd = filled(len(close))
	for i := range close {
		if !math.IsNaN(f[i]) && !math.IsNaN(s[i]) {
			macd[i] = f[i] - s[i]
		}
	}
	sig = EMA(macd, signal)
	hist = filled(len(close))
	for i := range close {
		if !math.IsNaN(macd[i]) && !math.IsNaN(sig[i]) {
			hist[i] = macd[i] - sig[i]
		}
	}
	return macd, sig, hist
}

// Bollinger is an n-period SMA with bands k population standard deviations
// either side.
func Bollinger(close []float64, n int, k float64) (mid, upper, lower []float64) {
	mid = SMA(close, n)
	upper, lower = filled(len(close)), filled(len(close))
	for i := range close {
		if math.IsNaN(mid[i]) {
			continue
		}
		var ss float64
		for j := i - n + 1; j <= i; j++ {
			d := close[j] - mid[i]
			ss += d * d
		}
		sd := math.Sqrt(ss / float64(n))
		upper[i], lower[i] = mid[i]+k*sd, mid[i]-k*sd
	}
	return mid, upper, lower
}

// ATR is Wilder's average true range over n periods: how far a bar travels,
// counting a gap from the previous close.
func ATR(high, low, close []float64, n int) []float64 {
	tr := filled(len(close))
	for i := range close {
		r := high[i] - low[i]
		if i > 0 {
			r = math.Max(r, math.Max(math.Abs(high[i]-close[i-1]), math.Abs(low[i]-close[i-1])))
		}
		tr[i] = r
	}
	return wilder(tr, n)
}

// Stochastic is %K — where the close sits in the last k bars' range, 0–100 —
// and %D, its d-bar SMA.
func Stochastic(high, low, close []float64, k, d int) (pk, pd []float64) {
	pk = filled(len(close))
	if k <= 0 {
		return pk, filled(len(close))
	}
	for i := k - 1; i < len(close); i++ {
		hi, lo := math.Inf(-1), math.Inf(1)
		for j := i - k + 1; j <= i; j++ {
			hi, lo = math.Max(hi, high[j]), math.Min(lo, low[j])
		}
		if hi == lo {
			pk[i] = 50
		} else {
			pk[i] = 100 * (close[i] - lo) / (hi - lo)
		}
	}
	return pk, SMA(pk, d)
}

// OBV is on-balance volume: a running total that adds a bar's volume when it
// closes up and subtracts it when it closes down. Its level is arbitrary —
// it starts wherever the series does — and only its direction means
// anything.
func OBV(close, volume []float64) []float64 {
	out := filled(len(close))
	if len(close) == 0 {
		return out
	}
	total := 0.0
	out[0] = 0
	for i := 1; i < len(close); i++ {
		switch {
		case close[i] > close[i-1]:
			total += volume[i]
		case close[i] < close[i-1]:
			total -= volume[i]
		}
		out[i] = total
	}
	return out
}

// VWAP is the volume-weighted average price since the start of each session,
// resetting whenever day changes. Each bar contributes its own VWAP where the
// source gave one and its typical price (high+low+close)/3 where it didn't.
func VWAP(high, low, close, volume, vwap []float64, day []int64) []float64 {
	out := filled(len(close))
	var pv, v float64
	for i := range close {
		if i == 0 || day[i] != day[i-1] {
			pv, v = 0, 0
		}
		price := (high[i] + low[i] + close[i]) / 3
		if vwap != nil && vwap[i] > 0 {
			price = vwap[i]
		}
		pv += price * volume[i]
		v += volume[i]
		if v > 0 {
			out[i] = pv / v
		} else {
			out[i] = price
		}
	}
	return out
}
