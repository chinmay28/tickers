package strategy

import (
	"math"
	"testing"
)

func TestDeflatedSharpeWithOneTrialIsTheProbabilisticSharpe(t *testing.T) {
	// Normal returns (skew 0, kurtosis 3), a per-bar Sharpe of 0.1 over 101
	// bars: z = 0.1 × √100 / √(1 + 0.5×0.01) ≈ 0.9975.
	got := DeflatedSharpe(0.1, 101, 0, 3, 1, 0)
	near(t, "probabilistic Sharpe", got, normCDF(1/math.Sqrt(1.005)))
	if p := DeflatedSharpe(0, 500, 0, 3, 1, 0); math.Abs(p-0.5) > 1e-9 {
		t.Errorf("a Sharpe of zero is %v likely to be above zero, want even odds", p)
	}
}

func TestMoreTrialsDeflateMore(t *testing.T) {
	prev := 1.0
	for _, n := range []int{1, 10, 100, 1000} {
		p := DeflatedSharpe(0.08, 1000, 0, 3, n, 0.001)
		if p >= prev {
			t.Errorf("after %d trials the probability is %v, not below %v — more trials must count against a result", n, p, prev)
		}
		prev = p
	}
	// The benchmark is the textbook one: ten thousand trials with unit
	// variance are expected to find a best of about 3.9 standard deviations.
	near4 := ExpectedMaxSharpe(10000, 1)
	if near4 < 3.8 || near4 > 4.0 {
		t.Errorf("expected maximum of 10,000 = %v, want about 3.9", near4)
	}
}

func TestFatTailsAndNegativeSkewCountAgainst(t *testing.T) {
	normal := DeflatedSharpe(0.1, 250, 0, 3, 1, 0)
	if p := DeflatedSharpe(0.1, 250, -1, 3, 1, 0); p >= normal {
		t.Errorf("negative skew gave %v, not below the normal case's %v", p, normal)
	}
	if p := DeflatedSharpe(0.1, 250, 0, 10, 1, 0); p >= normal {
		t.Errorf("fat tails gave %v, not below the normal case's %v", p, normal)
	}
	if !math.IsNaN(DeflatedSharpe(0.1, 1, 0, 3, 1, 0)) {
		t.Error("one return deflated to a number")
	}
}

func TestMetricsCarryTheReturnsMoments(t *testing.T) {
	// Returns of +10% and -10% alternating: mean 0, symmetric.
	m := metrics(100, []float64{110, 99, 108.9, 98.01}, t0, t0.AddDate(1, 0, 0), 252)
	if m.Periods != 4 {
		t.Errorf("periods = %d, want 4", m.Periods)
	}
	near(t, "kurtosis of a two-point distribution", m.Kurtosis, 1)
	near(t, "skew", m.Skew, 0)
	near(t, "Sharpe per bar", m.SharpePerBar, 0)
}
