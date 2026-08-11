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

// Regression tests for the M8 review.
//
// Every case here is a shape that reached a model, bypassed the k-anonymity
// floor, or crashed the gate — found by probing DATA shapes rather than by
// re-reading the rules. The milestone's own falsification pass tested the rules
// it had written and never fed the gate a NULL, a qualified star, or a value
// containing its own separator.

func reviewDB(t *testing.T, stmts ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db
}

// TestAMergedGroupKeyCannotClearTheFloor.
//
// NULL used to be encoded as "", so a NULL group and an empty-string group
// shared a bucket. Three records each, a floor of five, and the merged bucket of
// six CROSSED — reporting one group's mean under a label belonging to neither,
// with Suppressed at zero so nothing said so.
func TestAMergedGroupKeyCannotClearTheFloor(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (a TEXT, v REAL)`)
	for i := 0; i < 3; i++ {
		db.Exec(`INSERT INTO t VALUES (NULL, 100)`)
		db.Exec(`INSERT INTO t VALUES ('', 900)`)
	}
	env, err := gate.Aggregate(context.Background(), db,
		`SELECT a AS bucket, COUNT(*) AS n, AVG(v) AS mean FROM t GROUP BY 1`,
		gate.Options{KFloor: 5})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	for _, b := range env.TopK {
		if !b.Other {
			t.Errorf("a three-record group crossed a floor of five: %+v", b)
		}
	}
	if env.Suppressed != 2 {
		t.Errorf("Suppressed = %d, want 2 — both groups were withheld", env.Suppressed)
	}
}

// TestNullAndEmptyAreDifferentGroups. The other half: keeping them apart has to
// preserve both, not refuse both.
func TestNullAndEmptyAreDifferentGroups(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (a TEXT)`)
	for i := 0; i < 6; i++ {
		db.Exec(`INSERT INTO t VALUES (NULL)`)
		db.Exec(`INSERT INTO t VALUES ('')`)
	}
	env, err := gate.Aggregate(context.Background(), db,
		`SELECT a AS bucket, COUNT(*) AS n FROM t GROUP BY 1`, gate.Options{KFloor: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.TopK) != 2 {
		t.Fatalf("buckets = %d, want 2", len(env.TopK))
	}
	var labels []string
	for _, b := range env.TopK {
		if b.Count != 6 {
			t.Errorf("bucket %v count = %d, want 6", b.Key, b.Count)
		}
		labels = append(labels, b.Key[0])
	}
	// A NULL group must be labelled as one. An empty label for a real group and
	// an empty label for a missing value are indistinguishable to a reader.
	if labels[0] != gate.NullLabel && labels[1] != gate.NullLabel {
		t.Errorf("neither bucket is labelled %q: %v", gate.NullLabel, labels)
	}
}

// TestAValueContainingTheKeySeparatorDoesNotCollide.
//
// The key was the values joined with \x1f, which is not injective:
// ["x", "y\x1fz"] and ["x\x1fy", "z"] produced the same string, so two result
// rows merged and the bucket carried the sum of their counts under one key.
func TestAValueContainingTheKeySeparatorDoesNotCollide(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (a TEXT, b TEXT)`)
	for i := 0; i < 6; i++ {
		db.Exec(`INSERT INTO t VALUES (?, ?)`, "x", "y\x1fz")
		db.Exec(`INSERT INTO t VALUES (?, ?)`, "x\x1fy", "z")
	}
	env, err := gate.Aggregate(context.Background(), db,
		`SELECT a, b, COUNT(*) AS n FROM t GROUP BY 1, 2`, gate.Options{KFloor: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(env.TopK) != 2 {
		t.Fatalf("buckets = %d, want 2 — the keys merged", len(env.TopK))
	}
	for _, b := range env.TopK {
		if b.Count != 6 {
			t.Errorf("bucket %v count = %d, want 6", b.Key, b.Count)
		}
	}
}

// TestAnUngroupedAggregateIsSubjectToTheFloor.
//
// describedRows returned every row for an ungrouped statement without asking how
// many RECORDS were aggregated, so the overview template over a one-row table
// published that row's value five times — as the minimum, the maximum, the mean,
// the median and the total. RowCount is the number of RESULT rows, one, so
// nothing downstream noticed.
func TestAnUngroupedAggregateIsSubjectToTheFloor(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (salary REAL)`, `INSERT INTO t VALUES (137250.50)`)
	env, err := gate.Aggregate(context.Background(), db,
		`SELECT COUNT(*) AS n, AVG(salary) AS mean, MIN(salary) AS lowest,
		        MAX(salary) AS highest, SUM(salary) AS total FROM t`, gate.Options{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	text := env.Text()
	if strings.Contains(text, "137250") {
		t.Fatalf("the single record crossed:\n%s", text)
	}
	if !strings.Contains(text, "fewer than the reporting floor") {
		t.Errorf("nothing says why the summaries are missing:\n%s", text)
	}
}

// TestAnUngroupedAggregateMustSupplyItsRecordCount, or the floor has nothing to
// compare against — the same requirement the grouped case already had.
func TestAnUngroupedAggregateMustSupplyItsRecordCount(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (salary REAL)`, `INSERT INTO t VALUES (1)`)
	_, err := gate.Aggregate(context.Background(), db,
		`SELECT AVG(salary) AS mean FROM t`, gate.Options{})
	if err == nil {
		t.Fatal("permitted an ungrouped aggregate with no record count")
	}
	if !strings.Contains(err.Error(), "COUNT(*)") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestAStarInAnySpellingIsRefused.
//
// Only the bare form was checked, so `SELECT t.*, COUNT(*) FROM t GROUP BY 1`
// passed classification with a two-element isKey while the driver returned one
// column per expanded column — and the gate panicked with an index out of range,
// holding the whole result set in memory. Aggregate is exported and documented
// as refusing rather than crashing.
func TestAStarInAnySpellingIsRefused(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (a TEXT, b TEXT)`,
		`INSERT INTO t VALUES ('x','y'),('x','y'),('x','y'),('x','y'),('x','y'),('x','y')`)

	for _, q := range []string{
		`SELECT * FROM t`,
		`SELECT *, COUNT(*) FROM t GROUP BY 1`,
		`SELECT t.*, COUNT(*) FROM t GROUP BY 1`,
		`SELECT main.t.*, COUNT(*) FROM t GROUP BY 1`,
	} {
		t.Run(q, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panicked instead of refusing: %v", r)
				}
			}()
			if _, err := gate.Aggregate(context.Background(), db, q, gate.Options{KFloor: 2}); err == nil {
				t.Fatal("permitted")
			}
		})
	}
}

// TestANullMeasureCannotManufactureSignificance.
//
// The template's COUNT(*) was used as n while its Sum and SumSq came from
// SUM(measure), which skips NULL. Two groups whose every non-null value was 10,
// one with half its measure missing, were reported as
//
//	means 10 and 5 … statistically significant (Welch t = 9.95, p = <0.001),
//	effect size 1.41 (large)
//
// while the same passage printed `mean 10` for the group the test called 5. Every
// blank cell in a CSV becomes a NULL, so this was the default state of a real
// export.
func TestANullMeasureCannotManufactureSignificance(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (grp TEXT, spend REAL)`)
	for i := 0; i < 100; i++ {
		db.Exec(`INSERT INTO t VALUES ('aaa', 10)`)
		if i%2 == 0 {
			db.Exec(`INSERT INTO t VALUES ('bbb', 10)`)
		} else {
			db.Exec(`INSERT INTO t VALUES ('bbb', NULL)`)
		}
	}
	centre := `(SELECT AVG(spend) FROM t)`
	env, err := gate.Aggregate(context.Background(), db, fmt.Sprintf(
		`SELECT grp AS bucket, COUNT(*) AS n, COUNT(spend) AS %s, AVG(spend) AS mean,
		        MIN(%s) AS %s, SUM(spend - %s) AS %s,
		        SUM((spend - %s) * (spend - %s)) AS %s
		   FROM t GROUP BY 1 ORDER BY n DESC`,
		stats.CountColumn, centre, stats.OffsetColumn, centre, stats.SumColumn,
		centre, centre, stats.SumSqColumn), gate.Options{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	for _, tr := range env.TestResults {
		if tr.Verdict == stats.Significant {
			t.Errorf("a significant difference was reported between two groups whose "+
				"every value is 10:\n  %s", tr.Summary)
		}
		// n must be the count of non-null measures, not COUNT(*).
		if tr.NA == 100 && tr.NB == 100 {
			t.Errorf("both groups report n=100; one has only 50 non-null values:\n  %s",
				tr.Summary)
		}
	}
}

// TestALargeMagnitudeMeasureIsNotSilentlyMisreported.
//
// Σx² over uncentred values around ten million cancels so badly that the
// computed variance came out ten times too small, and the test read that as
// p = 6.2e-10 for data whose true p is 0.171. Centring the measure — which the
// template now does, and which is safe because variance is shift-invariant —
// removes the cancellation rather than detecting it.
func TestALargeMagnitudeMeasureIsNotSilentlyMisreported(t *testing.T) {
	db := reviewDB(t, `CREATE TABLE t (grp TEXT, v REAL)`)
	for i := 0; i < 500; i++ {
		db.Exec(`INSERT INTO t VALUES ('a', ?)`, 1e7+float64(i%3)-1)
		db.Exec(`INSERT INTO t VALUES ('b', ?)`, 1e7+0.05+float64(i%3)-1)
	}
	centre := `(SELECT AVG(v) FROM t)`
	env, err := gate.Aggregate(context.Background(), db, fmt.Sprintf(
		`SELECT grp AS bucket, COUNT(*) AS n, COUNT(v) AS %s, AVG(v) AS mean,
		        MIN(%s) AS %s, SUM(v - %s) AS %s,
		        SUM((v - %s) * (v - %s)) AS %s
		   FROM t GROUP BY 1 ORDER BY n DESC`,
		stats.CountColumn, centre, stats.OffsetColumn, centre, stats.SumColumn,
		centre, centre, stats.SumSqColumn), gate.Options{})
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if len(env.TestResults) == 0 {
		t.Fatal("no test at all; centring should have made this computable")
	}
	got := env.TestResults[0]
	if got.Verdict == stats.Significant {
		t.Errorf("a difference of 0.05 on a spread of 1 was called significant:\n  %s",
			got.Summary)
	}
	// And the means must be reported at their real magnitude, not centred.
	if got.MeanA < 9.9e6 || got.MeanA > 1.01e7 {
		t.Errorf("MeanA = %v, want ~1e7 — the offset was not added back", got.MeanA)
	}
}
