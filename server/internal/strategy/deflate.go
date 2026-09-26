package strategy

import "math"

// eulerGamma is the Euler–Mascheroni constant, which the expected maximum
// of many normal draws is written in.
const eulerGamma = 0.5772156649015329

// DeflatedSharpe is the probability that a strategy's true Sharpe ratio is
// above zero, allowing for how many strategies were tried to find it and for
// its returns not being normal (Bailey and López de Prado, "The Deflated
// Sharpe Ratio", 2014).
//
// The best of many backtests on the same data has a Sharpe ratio above zero
// even when none of them has an edge: that is what taking a maximum does. So
// the bar it is measured against is not zero but the Sharpe ratio the best of
// trials strategies with no edge would be expected to reach, given how widely
// the trials' Sharpe ratios varied. Fat tails and negative skew widen the
// uncertainty of the estimate, and so lower the probability further.
//
// sharpe is per bar, not annualised; periods is how many returns it came
// from; kurtosis is plain, not excess; variance is of the trials' per-bar
// Sharpe ratios. One trial deflates by nothing and gives the probabilistic
// Sharpe ratio.
func DeflatedSharpe(sharpe float64, periods int, skew, kurtosis float64, trials int, variance float64) float64 {
	if periods < 2 {
		return math.NaN()
	}
	return normCDF(deflation(sharpe, periods, skew, kurtosis, ExpectedMaxSharpe(trials, variance)))
}

// ExpectedMaxSharpe is the per-bar Sharpe ratio the best of trials
// strategies with no edge is expected to show, when their Sharpe ratios vary
// with the given variance.
func ExpectedMaxSharpe(trials int, variance float64) float64 {
	if trials < 2 || variance <= 0 {
		return 0
	}
	n := float64(trials)
	return math.Sqrt(variance) * ((1-eulerGamma)*normInv(1-1/n) + eulerGamma*normInv(1-1/(n*math.E)))
}

func deflation(sharpe float64, periods int, skew, kurtosis, benchmark float64) float64 {
	spread := 1 - skew*sharpe + (kurtosis-1)/4*sharpe*sharpe
	if spread <= 0 {
		spread = 1e-12
	}
	return (sharpe - benchmark) * math.Sqrt(float64(periods-1)) / math.Sqrt(spread)
}

func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

func normInv(p float64) float64 { return math.Sqrt2 * math.Erfinv(2*p-1) }
