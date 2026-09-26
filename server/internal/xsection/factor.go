package xsection

import (
	"math"
	"strconv"
	"strings"

	"github.com/chinmay28/tickers/server/internal/quotes"
	"github.com/chinmay28/tickers/server/internal/strategy"
)

// Factor is what symbols are ranked by. Beside every operand a rule can use
// ("rsi:14", "close"), it has the measures ranking needs and a rule doesn't:
//
//	return:N[:S]     the return over N days, ending S days ago (12-1 momentum is return:252:21)
//	volatility:N     annualised standard deviation of daily returns over N days
//	dollarvolume:N   average close × volume over N days — liquidity
//	drawdown:N       how far below its N-day high the close is
//	distance:X       how far the close is above operand X ("distance:sma:200")
//
// Percentages are percentages. Every value on a day uses only that day's
// bar and earlier ones.
type Factor struct {
	Text string
	kind string
	n, s int
	op   strategy.Operand
}

const (
	kindReturn     = "return"
	kindVolatility = "volatility"
	kindDollar     = "dollarvolume"
	kindDrawdown   = "drawdown"
	kindDistance   = "distance"
	kindOperand    = "operand"
	maxFactorDays  = 1000
)

// ParseFactor reads a factor. Its refusals are strategy.InvalidErrors, so a
// caller maps them the same way as a rule's.
func ParseFactor(raw string) (Factor, error) {
	text := strings.ToLower(strings.TrimSpace(raw))
	f := Factor{Text: text}
	head, rest, _ := strings.Cut(text, ":")
	switch head {
	case kindReturn, kindVolatility, kindDollar, kindDrawdown:
		f.kind = head
		parts := strings.Split(rest, ":")
		if rest == "" || len(parts) > 2 || (head != kindReturn && len(parts) > 1) {
			return f, invalid("%s takes a number of days, like %s:20", head, head)
		}
		var err error
		if f.n, err = days(parts[0]); err != nil {
			return f, err
		}
		if len(parts) == 2 {
			if parts[1] == "0" {
				f.s = 0
			} else if f.s, err = days(parts[1]); err != nil {
				return f, err
			}
		}
		if head == kindVolatility && f.n < 2 {
			return f, invalid("volatility needs at least 2 days")
		}
		return f, nil
	case kindDistance:
		op, err := strategy.ParseOperand(rest)
		if err != nil {
			return f, err
		}
		f.kind, f.op = kindDistance, op
		return f, nil
	}
	op, err := strategy.ParseOperand(text)
	if err != nil {
		return f, invalid("%q is not a factor (return:N, volatility:N, dollarvolume:N, drawdown:N, distance:X, or an operand like rsi:14): %v", raw, err)
	}
	f.kind, f.op = kindOperand, op
	return f, nil
}

func days(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxFactorDays {
		return 0, invalid("a number of days must be a whole number from 1 to %d", maxFactorDays)
	}
	return n, nil
}

func invalid(format string, args ...any) error { return strategy.Invalid(format, args...) }

// Warmup is how many days before the first ranked one the factor needs.
func (f Factor) Warmup() int {
	switch f.kind {
	case kindReturn:
		return f.n + f.s + 1
	case kindVolatility, kindDollar, kindDrawdown:
		return f.n + 1
	}
	return f.op.Warmup() + 1
}

// Values computes the factor for every symbol on every date, NaN where it
// isn't defined.
func (f Factor) Values(p *Panel) [][]float64 {
	out := make([][]float64, len(p.Symbols))
	for s := range p.Symbols {
		col := make([]float64, len(p.Dates))
		for i := range col {
			col[i] = math.NaN()
		}
		bars, at := p.bars(s)
		for k, v := range f.series(bars) {
			col[at[k]] = v
		}
		out[s] = col
	}
	return out
}

func (f Factor) series(bars []quotes.Candle) []float64 {
	n := len(bars)
	v := make([]float64, n)
	for i := range v {
		v[i] = math.NaN()
	}
	closes := make([]float64, n)
	for i, b := range bars {
		closes[i] = b.Close
	}
	switch f.kind {
	case kindReturn:
		for i := f.n + f.s; i < n; i++ {
			if base := closes[i-f.s-f.n]; base > 0 {
				v[i] = (closes[i-f.s]/base - 1) * 100
			}
		}
	case kindVolatility:
		for i := f.n; i < n; i++ {
			var sum, sumSq float64
			for k := i - f.n + 1; k <= i; k++ {
				r := closes[k]/closes[k-1] - 1
				sum += r
				sumSq += r * r
			}
			m := sum / float64(f.n)
			v[i] = math.Sqrt(math.Max(0, (sumSq-float64(f.n)*m*m)/float64(f.n-1))) * math.Sqrt(252) * 100
		}
	case kindDollar:
		for i := f.n - 1; i < n; i++ {
			var sum float64
			for k := i - f.n + 1; k <= i; k++ {
				sum += closes[k] * float64(bars[k].Volume)
			}
			v[i] = sum / float64(f.n)
		}
	case kindDrawdown:
		for i := f.n - 1; i < n; i++ {
			high := 0.0
			for k := i - f.n + 1; k <= i; k++ {
				high = math.Max(high, closes[k])
			}
			if high > 0 {
				v[i] = (closes[i]/high - 1) * 100
			}
		}
	case kindDistance:
		ref := f.op.Series(bars)
		for i := range v {
			if defined(ref[i]) && ref[i] != 0 {
				v[i] = (closes[i]/ref[i] - 1) * 100
			}
		}
	default:
		copy(v, f.op.Series(bars))
	}
	for i, x := range v {
		if !defined(x) {
			v[i] = math.NaN()
		}
	}
	return v
}
