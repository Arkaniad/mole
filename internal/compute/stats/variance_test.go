package stats_test

import (
	"math"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/stats"
)

// TestNoVarianceMoreThanOnePercentWrongIsEverReported.
//
// A sweep rather than an example, because the failure lives in a regime rather
// than at a point: Σx² − (Σx)²/n loses precision as the ratio of magnitude to
// spread grows, and the first version of this guard accepted variances up to 18%
// wrong while refusing nothing that mattered. The true variance is computed in a
// numerically stable way and compared against what the sums produce.
//
// Refusing an accurate case is permitted here and refusing nothing is not: the
// fix for a refusal is one line in the query, and the cost of accepting a wrong
// variance was a fabricated p-value of 6e-10.
func TestNoVarianceMoreThanOnePercentWrongIsEverReported(t *testing.T) {
	build := func(offset, mean, spread float64, n int) (stats.Group, float64) {
		g := stats.Group{N: int64(n)}
		var vals []float64
		for i := 0; i < n; i++ {
			v := mean + offset + spread*(float64(i%3)-1)
			vals = append(vals, v)
			g.Sum += v
			g.SumSq += v * v
		}
		var m float64
		for _, v := range vals {
			m += v
		}
		m /= float64(len(vals))
		var ss float64
		for _, v := range vals {
			ss += (v - m) * (v - m)
		}
		return g, ss / float64(len(vals)-1)
	}
	var bad int
	for _, spread := range []float64{1, 10, 100} {
		for _, mean := range []float64{1e6, 3e6, 1e7, 3e7, 1e8, 3e8, 1e9} {
			a, trueVar := build(0, mean, spread, 1000)
			b, _ := build(spread*0.05, mean, spread, 1000)
			n := 1000.0
			ssA := a.SumSq - a.Sum*a.Sum/n
			relerr := math.Inf(1)
			if ssA > 0 {
				relerr = math.Abs(ssA/(n-1)-trueVar) / trueVar
			}
			_, err := stats.WelchOrReason("x", a, b)
			if relerr > 0.01 && err == nil {
				bad++
				t.Errorf("spread=%g mean=%g: a variance %.1f%% wrong was reported",
					spread, mean, relerr*100)
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d case(s) accepted a variance more than 1%% wrong", bad)
	}

	// And the guard must not refuse everything: a spread that is a meaningful
	// fraction of the magnitude has to remain computable, or no real column can
	// be tested.
	a, _ := build(0, 1e6, 100, 1000)
	b, _ := build(5, 1e6, 100, 1000)
	if _, err := stats.WelchOrReason("x", a, b); err != nil {
		t.Errorf("an ordinary column was refused: %v", err)
	}
}
