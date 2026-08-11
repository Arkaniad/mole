package stats_test

import (
	"math"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/stats"
)

// M8 slice 5.
//
// A wrong p-value is the sort of error that gets believed, so the test
// statistic and the distribution behind it are checked against published
// values rather than against whatever this implementation happens to produce.
// The rest of the file is about the verdict, which is where a correct number
// still turns into a wrong conclusion.

// group builds a summary from actual values, so the fixtures say what they mean
// instead of carrying precomputed sums nobody can check by eye.
func group(name string, xs ...float64) stats.Group {
	g := stats.Group{Name: name, N: int64(len(xs))}
	for _, x := range xs {
		g.Sum += x
		g.SumSq += x * x
	}
	return g
}

// repeated builds n values whose mean is exactly `mean` and whose SAMPLE
// variance is exactly spread².
//
// Two corrections, both found by the fixture failing its own sanity check.
// Alternating mean±spread puts every deviation at spread, so the sum of squares
// is n·spread² and the sample variance is spread²·n/(n−1) — twice too large at
// n=2. And for ODD n the alternation is lopsided: one more value above the mean
// than below it, so neither the mean nor the variance is what it says.
//
// So: an odd count gets one value sitting exactly on the mean, which makes the
// deviations cancel and the sum of squares (n−1)·spread² — already the sample
// variance. An even count needs the deviation scaled down instead.
func repeated(name string, mean, spread float64, n int) stats.Group {
	if n < 2 {
		return group(name, mean)
	}
	xs := make([]float64, 0, n)
	d := spread
	if n%2 == 0 {
		d = spread * math.Sqrt(float64(n-1)/float64(n))
	} else {
		xs = append(xs, mean)
	}
	for len(xs) < n {
		xs = append(xs, mean+d, mean-d)
	}
	return group(name, xs...)
}

// TestTheTDistributionMatchesPublishedCriticalValues.
//
// Every entry is a value from a t-table: at these t and df the two-tailed p is
// 0.05 by definition, or is a figure any table lists. If the continued fraction
// behind the incomplete beta is wrong, it is wrong here.
//
// Sizes rather than degrees of freedom, because two equal groups give
// df = 2n−2 and nothing else — df is even by construction, so asking for df=1
// silently produced df=2 and a fixture that failed its own sanity check.
func TestTheTDistributionMatchesPublishedCriticalValues(t *testing.T) {
	for _, tc := range []struct {
		n         int
		t         float64
		wantP     float64
		tol       float64
		published string
	}{
		{2, 4.303, 0.05, 1e-4, "df=2 critical value"},
		// Two closed-form points. At df=2 the two-tailed p is exactly
		// 1 − |t|/sqrt(t²+2), so these are identities rather than table
		// lookups, and they sit above the incomplete beta's symmetry point
		// where the other rows sit below it.
		{2, 0.5, 1 - 0.5/math.Sqrt(2.25), 1e-9, "df=2 closed form"},
		{2, 1.0, 1 - 1.0/math.Sqrt(3.0), 1e-9, "df=2 closed form"},
		{6, 2.228, 0.05, 1e-4, "df=10 critical value"},
		{11, 2.086, 0.05, 1e-4, "df=20 critical value"},
		{25, 3.5355, 0.00091, 1e-5, "df=48, scipy ttest_ind"},
		{6, 2.0, 0.0734, 1e-4, "df=10, a non-critical point"},
		{5000, 1.960, 0.05, 1e-3, "df→∞ is the normal"},
		{6, 0, 1.0, 1e-9, "no separation at all"},
	} {
		// Reached through Welch, the only exported route: testing the
		// unexported helper would leave the wiring unchecked.
		a, b := syntheticPair(tc.t, tc.n)
		got, ok := stats.Welch("x", a, b)
		if !ok {
			t.Fatalf("n=%d t=%v: no test", tc.n, tc.t)
		}
		if wantDF := float64(2*tc.n - 2); math.Abs(got.DF-wantDF) > 1e-6 {
			t.Fatalf("fixture is wrong: df = %v, meant to be %v", got.DF, wantDF)
		}
		if math.Abs(math.Abs(got.Statistic)-tc.t) > 1e-6 {
			t.Fatalf("fixture is wrong: statistic = %v, meant to be %v", got.Statistic, tc.t)
		}
		if math.Abs(got.P-tc.wantP) > tc.tol {
			t.Errorf("n=%d t=%v (df=%v): p = %.6f, want %.6f — %s",
				tc.n, tc.t, got.DF, got.P, tc.wantP, tc.published)
		}
	}
}

// syntheticPair builds two equal groups whose Welch statistic is `want` and
// whose degrees of freedom are 2n−2. Equal size and equal variance make the
// Welch–Satterthwaite denominator collapse to the Student one, so the fixture
// can be checked against a published table.
func syntheticPair(want float64, n int) (stats.Group, stats.Group) {
	const spread = 1.0
	// For equal groups, se = spread·sqrt(2/n), so the gap in means is t·se.
	se := spread * math.Sqrt(2/float64(n))
	return repeated("a", want*se, spread, n), repeated("b", 0, spread, n)
}

// TestTheMeanAndVarianceComeOutOfTheSums. Everything else rests on Σx and Σx²
// being enough, which is what lets this run behind the aggregation gate at all.
func TestTheMeanAndVarianceComeOutOfTheSums(t *testing.T) {
	g := group("g", 2, 4, 4, 4, 5, 5, 7, 9)
	if got := g.Mean(); got != 5 {
		t.Errorf("mean = %v, want 5", got)
	}
	// Sample variance of that set is 32/7.
	if got := g.Variance(); math.Abs(got-32.0/7.0) > 1e-9 {
		t.Errorf("variance = %v, want %v (SAMPLE variance, n−1)", got, 32.0/7.0)
	}
}

// TestAVerdictNeedsEnoughRecords is the check §4 asks for that a p-value alone
// does not provide.
func TestAVerdictNeedsEnoughRecords(t *testing.T) {
	// A large, obvious separation — but six records against six.
	small, ok := stats.Welch("revenue", repeated("north", 100, 5, 6), repeated("south", 40, 5, 6))
	if !ok {
		t.Fatal("no test")
	}
	if small.P >= stats.Alpha {
		t.Fatalf("fixture is wrong: p = %v, meant to be significant on the numbers", small.P)
	}
	if small.Verdict != stats.Underpowered {
		t.Errorf("verdict = %q, want %q — p < alpha on twelve records is exactly the "+
			"result this check exists to refuse", small.Verdict, stats.Underpowered)
	}
	if !strings.Contains(small.Summary, "UNDERPOWERED") {
		t.Errorf("the sentence a model reads does not say so: %s", small.Summary)
	}

	// The same separation with enough records.
	big, ok := stats.Welch("revenue", repeated("north", 100, 5, 40), repeated("south", 40, 5, 40))
	if !ok {
		t.Fatal("no test")
	}
	if big.Verdict != stats.Significant {
		t.Errorf("verdict = %q, want %q", big.Verdict, stats.Significant)
	}
}

// TestNoDifferenceIsReportedAsNoDifference, and specifically not as absence.
func TestNoDifferenceIsReportedAsNoDifference(t *testing.T) {
	got, ok := stats.Welch("revenue", repeated("north", 100, 20, 40), repeated("south", 101, 20, 40))
	if !ok {
		t.Fatal("no test")
	}
	if got.Verdict != stats.NotSignificant {
		t.Fatalf("verdict = %q, want %q (p = %v)", got.Verdict, stats.NotSignificant, got.P)
	}
	if !strings.Contains(got.Summary, "NOT distinguishable from chance") {
		t.Errorf("the sentence does not say what the test showed: %s", got.Summary)
	}
	// A test cannot demonstrate absence, and the sentence must not read as if
	// it had — a model quoting "no difference" would be quoting a claim the
	// statistics do not make.
	if strings.Contains(got.Summary, "no difference") {
		t.Errorf("the sentence claims absence, which no test can show: %s", got.Summary)
	}
}

// TestEffectSizeIsReportedAlongsideSignificance. §4 asks for both, and for the
// usual reason: a large enough sample makes a trivial difference significant.
func TestEffectSizeIsReportedAlongsideSignificance(t *testing.T) {
	// A tiny separation, and enough records to detect it. Six hundredths of a
	// standard deviation, four thousand records a side: p is well under alpha
	// and the effect is nothing anyone would act on.
	got, ok := stats.Welch("revenue", repeated("north", 100.6, 10, 4000), repeated("south", 100, 10, 4000))
	if !ok {
		t.Fatal("no test")
	}
	if got.Verdict != stats.Significant {
		t.Fatalf("fixture is wrong: verdict = %q, p = %v", got.Verdict, got.P)
	}
	if math.Abs(got.EffectSize) > 0.2 {
		t.Fatalf("fixture is wrong: d = %v, meant to be negligible", got.EffectSize)
	}
	if !strings.Contains(got.Summary, "negligible") {
		t.Errorf("a significant but negligible difference does not say so, so a model "+
			"reads it as a finding: %s", got.Summary)
	}
}

// TestGroupsThatCannotSupportATestSaySo rather than returning a number.
func TestGroupsThatCannotSupportATestSaySo(t *testing.T) {
	for _, tc := range []struct {
		why  string
		a, b stats.Group
	}{
		{"one record", group("a", 1), group("b", 1, 2, 3)},
		{"no variation in either", repeated("a", 5, 0, 30), repeated("b", 9, 0, 30)},
		{"empty", stats.Group{Name: "a"}, stats.Group{Name: "b"}},
	} {
		t.Run(tc.why, func(t *testing.T) {
			if got, ok := stats.Welch("x", tc.a, tc.b); ok {
				t.Fatalf("a test was reported: %+v", got)
			}
		})
	}
}

// TestTheSummaryCarriesEveryNumberAClaimWouldCite. The sentence IS the evidence
// — §11.5 checks a claim's quote against it — so a figure missing here is a
// figure no claim can be made about.
func TestTheSummaryCarriesEveryNumberAClaimWouldCite(t *testing.T) {
	got, ok := stats.Welch("the mean", repeated("north", 100, 5, 40), repeated("south", 40, 5, 40))
	if !ok {
		t.Fatal("no test")
	}
	for _, want := range []string{"north", "south", "n = 40 and 40", "p ", "CI", "effect size"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("the summary is missing %q: %s", want, got.Summary)
		}
	}
}

// -----------------------------------------------------------------------------
// M8 review regressions
// -----------------------------------------------------------------------------

// TestTheIntervalNeverContradictsTheVerdict.
//
// The interval used 1.96 rather than the t quantile for its own degrees of
// freedom, justified in a comment as "under 3%". The percentage was not the
// point: at n=20/20 (df=38, t* = 2.0244) any 1.96 < |t| < 2.0244 produced
//
//	NOT distinguishable from chance (Welch t = 1.96, p = 0.057), 95% CI 0.00 to 6.36
//
// an interval excluding zero beside a null verdict, in one sentence a claim has
// to quote. The quantile is now inverted from the same distribution the p-value
// comes from, so the two cannot disagree by construction.
func TestTheIntervalNeverContradictsTheVerdict(t *testing.T) {
	var disagreements int
	for _, n := range []int64{20, 25, 40, 100, 500} {
		for gap := 0.0; gap <= 6.0; gap += 0.01 {
			a := stats.Group{Name: "a", N: n}
			b := stats.Group{Name: "b", N: n}
			for i := int64(0); i < n; i++ {
				d := 5.0
				if i%2 == 1 {
					d = -5.0
				}
				va, vb := gap+d, d
				a.Sum += va
				a.SumSq += va * va
				b.Sum += vb
				b.SumSq += vb * vb
			}
			got, ok := stats.Welch("x", a, b)
			if !ok {
				continue
			}
			excludesZero := (got.CILow > 0) == (got.CIHigh > 0)
			if excludesZero != (got.P < stats.Alpha) {
				disagreements++
				if disagreements == 1 {
					t.Errorf("n=%d gap=%.2f: p = %.5f but the 95%% CI is [%.4f, %.4f]\n  %s",
						n, gap, got.P, got.CILow, got.CIHigh, got.Summary)
				}
			}
		}
	}
	if disagreements > 0 {
		t.Errorf("%d parameter combinations disagree", disagreements)
	}
}

// TestALostVarianceIsRefusedRatherThanReported.
//
// Σx² − (Σx)²/n subtracts two nearly equal large numbers. The old guard clamped a
// negative result to zero and missed the regime that matters — measured on
// 1000-row groups with true variance 0.667 and a true p of 0.171:
//
//	mean 1e6   variance 0.667668   not significant, p = 0.177   correct
//	mean 1e7   variance 0.064064   SIGNIFICANT,     p = 6.2e-10  fabricated
//
// so at values around ten million the test reported a large significant effect
// for data with none.
func TestALostVarianceIsRefusedRatherThanReported(t *testing.T) {
	// spread scales with the magnitude for the cases that must still work: what
	// decides whether the sums can resolve a variance is the RATIO of the two,
	// not the magnitude alone. See MaxMagnitudeRatio.
	build := func(offset, mean, spread float64, n int) stats.Group {
		g := stats.Group{N: int64(n)}
		for i := 0; i < n; i++ {
			v := mean + offset + spread*(float64(i%3)-1)
			g.Sum += v
			g.SumSq += v * v
		}
		return g
	}

	// An ordinary column must still be computable, or the guard is just a
	// refusal. A spread of 100 around a million is a ratio of ten thousand.
	for _, mean := range []float64{1e3, 1e6, 1e7} {
		got, err := stats.WelchOrReason("x",
			build(0, mean, 100, 1000), build(5, mean, 100, 1000))
		if err != nil {
			t.Fatalf("an ordinary column at mean %g was refused: %v", mean, err)
		}
		if got.Verdict == stats.Underpowered {
			t.Errorf("mean %g: n=1000 a side reported as underpowered", mean)
		}
	}

	// A spread float64 cannot resolve at this magnitude must be refused, with the
	// fix in the message rather than a silent zero or a wrong figure.
	for _, mean := range []float64{1e7, 1e8, 1e9} {
		_, err := stats.WelchOrReason("x",
			build(0, mean, 1, 1000), build(0.05, mean, 1, 1000))
		if err == nil {
			t.Errorf("mean %g produced a test from a variance float64 cannot resolve", mean)
			continue
		}
		if !strings.Contains(err.Error(), "subtract a constant") {
			t.Errorf("mean %g: the error does not say how to fix it: %v", mean, err)
		}
	}
}

// TestCentringMakesALargeMagnitudeComputable, which is the actual fix — the
// refusal above is only the backstop. Variance is shift-invariant, so a query
// that subtracts a constant gets the right answer and the mean is recovered from
// Offset.
func TestCentringMakesALargeMagnitudeComputable(t *testing.T) {
	const mean = 1e7
	build := func(offset float64, n int) stats.Group {
		g := stats.Group{N: int64(n), Offset: mean}
		for i := 0; i < n; i++ {
			v := offset + float64(i%3) - 1 // already centred
			g.Sum += v
			g.SumSq += v * v
		}
		return g
	}
	got, err := stats.WelchOrReason("x", build(0, 1000), build(0.05, 1000))
	if err != nil {
		t.Fatalf("centred data was refused: %v", err)
	}
	if got.Verdict == stats.Significant {
		t.Errorf("a difference of 0.05 on a spread of 1 was called significant (p = %v)", got.P)
	}
	// The means must be reported at their real magnitude.
	if got.MeanA < 0.99e7 || got.MeanA > 1.01e7 {
		t.Errorf("MeanA = %v, want ~1e7 — Offset was not added back", got.MeanA)
	}
}

// TestOneVerdictRuleForBothPaths. It was written twice and the copies already
// differed: this required BOTH groups to clear MinGroupN and coderunner's checked
// one, under a comment claiming they could not disagree.
func TestOneVerdictRuleForBothPaths(t *testing.T) {
	// One group short of the floor is underpowered even when the other is not.
	if got := stats.VerdictFor(0.0001, 500, 5); got != stats.Underpowered {
		t.Errorf("VerdictFor(p, 500, 5) = %q, want %q", got, stats.Underpowered)
	}
	if got := stats.VerdictFor(0.0001, 500); got != stats.Significant {
		t.Errorf("VerdictFor(p, 500) = %q, want %q", got, stats.Significant)
	}
	if got := stats.VerdictFor(0.4, 500, 500); got != stats.NotSignificant {
		t.Errorf("VerdictFor(0.4, 500, 500) = %q, want %q", got, stats.NotSignificant)
	}
}

// TestSmallMagnitudesKeepTheirDigits. Three formatters existed at two
// precisions, and the gate's rendered a rate column of 0.0001 to 0.003 as
// "0.00" — so §11.5 then permitted only a wrong number as a citation.
func TestSmallMagnitudesKeepTheirDigits(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0.0001, 0.0001}, {0.003, 0.003}, {0.00155, 0.00155},
	} {
		got := stats.Num(tc.in)
		if got == "0.00" || got == "0" {
			t.Errorf("Num(%v) = %q, which destroys the figure a claim must quote",
				tc.in, got)
		}
	}
	// Whole numbers and ordinary magnitudes keep reading naturally.
	for in, want := range map[float64]string{20: "20", 10.5: "10.50", 1050: "1050"} {
		if got := stats.Num(in); got != want {
			t.Errorf("Num(%v) = %q, want %q", in, got, want)
		}
	}
}
