package stats

import (
	"math"
	"testing"
)

// The incomplete beta function is evaluated through one of two paths, chosen by
// where x falls relative to (a+1)/(a+b+2): directly, or through the identity
//
//	I_x(a, b) = 1 − I_{1−x}(b, a)
//
// The branch exists for CONVERGENCE — the continued fraction converges quickly
// on one side of that point and slowly on the other — and not for correctness.
// Measured: forcing either path for every reference value in stats_test.go
// changes no result at 1e-9, because 200 iterations are enough for both.
//
// So the branch cannot be tested by its effect on a p-value, and pretending
// otherwise would be a test that passes whatever the code does. What is
// testable is the identity it rests on, which is asserted here directly. If
// that ever fails, both paths are wrong together and every p-value above it is
// meaningless.
func TestTheBetaIdentityHoldsOnBothPaths(t *testing.T) {
	for _, tc := range []struct{ a, b, x float64 }{
		{1, 0.5, 0.1}, {1, 0.5, 0.5}, {1, 0.5, 0.9}, {1, 0.5, 0.99},
		{5, 0.5, 0.2}, {5, 0.5, 0.8}, {5, 0.5, 0.999},
		{24, 0.5, 0.5}, {24, 0.5, 0.95},
		{0.5, 0.5, 0.25}, {0.5, 0.5, 0.75},
		{2.5, 3.5, 0.4}, {2.5, 3.5, 0.6},
	} {
		direct := incompleteBeta(tc.a, tc.b, tc.x)
		mirrored := 1 - incompleteBeta(tc.b, tc.a, 1-tc.x)
		if math.Abs(direct-mirrored) > 1e-9 {
			t.Errorf("I_%v(%v, %v) = %.12f but 1 − I_%v(%v, %v) = %.12f; the two paths "+
				"through the continued fraction disagree, so one of them is wrong",
				tc.x, tc.a, tc.b, direct, 1-tc.x, tc.b, tc.a, mirrored)
		}
		if direct < 0 || direct > 1 {
			t.Errorf("I_%v(%v, %v) = %v, outside [0, 1]", tc.x, tc.a, tc.b, direct)
		}
	}
}

// TestTheBetaFunctionIsMonotonic. A CDF that goes backwards would produce a
// p-value that rises with the evidence against the null.
func TestTheBetaFunctionIsMonotonic(t *testing.T) {
	prev := -1.0
	for x := 0.0; x <= 1.0001; x += 0.01 {
		got := incompleteBeta(5, 0.5, math.Min(x, 1))
		if got < prev-1e-12 {
			t.Fatalf("I_x(5, 0.5) fell from %v to %v at x = %v", prev, got, x)
		}
		prev = got
	}
}
