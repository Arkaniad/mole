package gate_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/gate"
	"github.com/lajosdeme/mole/internal/compute/stats"
	_ "modernc.org/sqlite"
)

// M8 slice 5, at the gate.
//
// §4 says a local claim is verified on "n, effect size, significance". The
// arithmetic lives in internal/compute/stats and is checked against published
// critical values there; what is checked here is that the numbers reach an
// envelope at all, that they only appear when the query actually supplied
// enough to compute them, and that they reach the passage a claim gets quoted
// against.

// comparisonQuery is the shape hypothesis.KindGroupCompare renders. Written out
// rather than imported so a change to the template shows up as a failure here
// instead of silently removing the tests — which is what happened when the
// template gained COUNT(measure) and a centring offset.
//
// Both of those are load-bearing, not decoration. COUNT(measure) is n, because
// SUM skips NULL and COUNT(*) does not; and the measure is centred on its own
// global mean so that Σx² does not cancel at large magnitudes.
func comparisonQuery() string {
	centre := `(SELECT AVG(spend) FROM samples)`
	return fmt.Sprintf(
		`SELECT region AS bucket, COUNT(*) AS n, COUNT(spend) AS %s,
		        AVG(spend) AS mean, MIN(%s) AS %s,
		        SUM(spend - %s) AS %s,
		        SUM((spend - %s) * (spend - %s)) AS %s
		   FROM samples GROUP BY 1 ORDER BY n DESC`,
		stats.CountColumn, centre, stats.OffsetColumn,
		centre, stats.SumColumn, centre, centre, stats.SumSqColumn)
}

// samplesDB builds two groups with a known separation. `north` and `south` each
// get n records around their own mean; `tiny` gets six, so an underpowered
// group is reachable without rebuilding the fixture.
func samplesDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "samples.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE samples (region TEXT, spend REAL)`); err != nil {
		t.Fatal(err)
	}

	insert := func(region string, mean float64, count int) {
		for i := 0; i < count; i++ {
			// Alternating either side of the mean, so the spread is known and
			// the separation is the only thing that varies between groups.
			v := mean + 5
			if i%2 == 1 {
				v = mean - 5
			}
			if _, err := db.Exec(`INSERT INTO samples VALUES (?, ?)`, region, v); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert("north", 100, n)
	insert("south", 40, n)
	insert("tiny", 70, 6)
	return db
}

// TestATestReachesTheEnvelope. Two clearly separated groups of forty.
func TestATestReachesTheEnvelope(t *testing.T) {
	env, err := gate.Aggregate(context.Background(), samplesDB(t, 40), comparisonQuery(), gate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.TestResults) != 1 {
		t.Fatalf("%d test(s), want exactly 1: %+v", len(env.TestResults), env.TestResults)
	}
	got := env.TestResults[0]
	if got.Verdict != stats.Significant {
		t.Errorf("verdict = %q, want %q (p = %v)", got.Verdict, stats.Significant, got.P)
	}
	if got.NA != 40 || got.NB != 40 {
		t.Errorf("n = %d and %d, want 40 and 40", got.NA, got.NB)
	}
	// The groups compared must be the two largest, not the first two the engine
	// happened to return.
	pair := got.GroupA + "/" + got.GroupB
	if pair != "north/south" && pair != "south/north" {
		t.Errorf("compared %q, want the two largest groups", pair)
	}

	// The sentence has to be in the passage a claim is quoted against, or no
	// claim can cite the significance and §11.5 drops any that tries.
	text := env.Text()
	if !strings.Contains(text, got.Summary) {
		t.Fatalf("the test summary is not in the rendered passage:\n%s", text)
	}
	if !strings.Contains(text, "Statistical tests") {
		t.Errorf("the passage does not label the tests:\n%s", text)
	}
}

// TestOnlyTheTwoLargestGroupsAreCompared.
//
// Comparing every pair of k groups is k(k−1)/2 tests against the same alpha,
// which manufactures a significant result out of noise as soon as there are a
// few groups — in the one place mole reports statistics as if they settled
// something.
func TestOnlyTheTwoLargestGroupsAreCompared(t *testing.T) {
	env, err := gate.Aggregate(context.Background(), samplesDB(t, 40), comparisonQuery(),
		gate.Options{KFloor: 2})
	if err != nil {
		t.Fatal(err)
	}
	var named int
	for _, b := range env.TopK {
		if !b.Other {
			named++
		}
	}
	if named != 3 {
		t.Fatalf("%d named buckets, want 3 — the fixture is not exercising the choice", named)
	}
	if len(env.TestResults) != 1 {
		t.Fatalf("%d test(s) over 3 groups, want 1", len(env.TestResults))
	}
	if strings.Contains(env.TestResults[0].GroupA+env.TestResults[0].GroupB, "tiny") {
		t.Errorf("the six-record group was compared: %+v", env.TestResults[0])
	}
	var said bool
	for _, n := range env.Notes {
		if strings.Contains(n, "largest groups only") {
			said = true
		}
	}
	if !said {
		t.Error("nothing says only two groups were compared, so a reader takes the " +
			"one test as covering all of them")
	}
}

// TestASmallGroupIsReportedAsUnderpowered rather than as a finding.
func TestASmallGroupIsReportedAsUnderpowered(t *testing.T) {
	// Six records a side: the separation is huge and the sample is not.
	env, err := gate.Aggregate(context.Background(), samplesDB(t, 6), comparisonQuery(),
		gate.Options{KFloor: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.TestResults) == 0 {
		t.Fatal("no test at all; the underpowered case must still be reported")
	}
	got := env.TestResults[0]
	if got.Verdict != stats.Underpowered {
		t.Errorf("verdict = %q, want %q (p = %v, n = %d and %d)",
			got.Verdict, stats.Underpowered, got.P, got.NA, got.NB)
	}
	if !strings.Contains(env.Text(), "UNDERPOWERED") {
		t.Errorf("the passage does not say so:\n%s", env.Text())
	}
}

// TestNoTestWithoutTheSufficientStatistics. A mean per group cannot produce a
// p-value, and the gate must not invent one from what it has.
func TestNoTestWithoutTheSufficientStatistics(t *testing.T) {
	db := samplesDB(t, 40)
	for _, tc := range []struct{ why, query string }{
		{"means only",
			`SELECT region AS bucket, COUNT(*) AS n, AVG(spend) AS mean
			   FROM samples GROUP BY 1 ORDER BY n DESC`},
		{"sum without the sum of squares",
			fmt.Sprintf(`SELECT region AS bucket, COUNT(*) AS n, SUM(spend) AS %s
			   FROM samples GROUP BY 1 ORDER BY n DESC`, stats.SumColumn)},
		// The sums without a measure count. COUNT(*) is not n: SUM skips NULL
		// and COUNT(*) does not, and using the latter reported a large
		// significant difference between two groups that were identical apart
		// from where their blanks were.
		{"sums without a measure count",
			fmt.Sprintf(`SELECT region AS bucket, COUNT(*) AS n,
			        SUM(spend) AS %s, SUM(spend * spend) AS %s
			   FROM samples GROUP BY 1 ORDER BY n DESC`,
				stats.SumColumn, stats.SumSqColumn)},
		{"one group", `SELECT region AS bucket, COUNT(*) AS n FROM samples
			 WHERE region = 'north' GROUP BY 1`},
		{"no grouping at all", `SELECT COUNT(*) AS n, AVG(spend) AS mean FROM samples`},
	} {
		t.Run(tc.why, func(t *testing.T) {
			env, err := gate.Aggregate(context.Background(), db, tc.query, gate.Options{})
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(env.TestResults) != 0 {
				t.Fatalf("a test was reported without the numbers to compute one: %+v",
					env.TestResults)
			}
			if strings.Contains(env.Text(), "Statistical tests") {
				t.Errorf("the passage advertises tests it does not have:\n%s", env.Text())
			}
		})
	}
}

// TestTheSufficientStatisticsAreStillAggregates. Σx and Σx² are sums over a
// group, so adding them cannot open a route past §12.1 — asserted rather than
// assumed, because they were added to the template for the statistics and the
// exfil property is what stops a new measure carrying a value.
func TestTheSufficientStatisticsAreStillAggregates(t *testing.T) {
	env, err := gate.Aggregate(context.Background(), samplesDB(t, 40), comparisonQuery(),
		gate.Options{KFloor: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range env.TopK {
		if b.Other {
			continue
		}
		if b.Count < gate.DefaultKFloor && !b.Other {
			t.Errorf("a bucket below the floor crossed: %+v", b)
		}
		for _, name := range []string{stats.SumColumn, stats.SumSqColumn} {
			if _, ok := b.Measures[name]; !ok {
				t.Errorf("bucket %v is missing %s, so no test could be computed", b.Key, name)
			}
		}
	}
}
