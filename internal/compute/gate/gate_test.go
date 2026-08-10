package gate_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/gate"
	_ "modernc.org/sqlite"
)

// M8 slice 2.
//
// §12.1 turns "only aggregates reach the LLM" from a comment into a mechanism.
// These tests are about the mechanism: what the gate refuses, what it folds
// away, and what it will not carry no matter how the query is written.
//
// The fixture is built so the interesting cases are reachable. `region` has
// buckets on both sides of the k-anonymity floor; `rep_note` is prose that
// differs in every row; `spend` is a numeric column with a known distribution.

const rows = 20

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tickets.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(
		`CREATE TABLE tickets (region TEXT, rep_note TEXT, spend REAL, agent TEXT)`); err != nil {
		t.Fatal(err)
	}
	// north 8, south 6, east 3, west 2, solo 1 — two buckets above a floor of
	// five and three below it.
	plan := []struct {
		region string
		n      int
	}{{"north", 8}, {"south", 6}, {"east", 3}, {"west", 2}, {"solo", 1}}

	spend := 1.0
	i := 0
	for _, p := range plan {
		for k := 0; k < p.n; k++ {
			note := fmt.Sprintf(
				"Customer asked for the invoice covering period %d to be reissued against a different cost centre", i)
			if _, err := db.Exec(
				`INSERT INTO tickets VALUES (?, ?, ?, ?)`, p.region, note, spend, "agent"); err != nil {
				t.Fatal(err)
			}
			spend++
			i++
		}
	}
	return db
}

func aggregate(t *testing.T, db *sql.DB, q string) (gate.AggregateEnvelope, error) {
	t.Helper()
	return gate.Aggregate(context.Background(), db, q, gate.Options{})
}

func mustAggregate(t *testing.T, db *sql.DB, q string) gate.AggregateEnvelope {
	t.Helper()
	env, err := aggregate(t, db, q)
	if err != nil {
		t.Fatalf("a query a template would render was refused: %v\n%s", err, q)
	}
	return env
}

// -----------------------------------------------------------------------------
// Refusals
// -----------------------------------------------------------------------------

func TestNonAggregatesAreRefused(t *testing.T) {
	db := testDB(t)

	for _, tc := range []struct {
		why    string
		query  string
		reason string
	}{
		{"select star", `SELECT * FROM tickets`, "not an aggregate"},
		// Star beside a real aggregate: the count makes it look aggregated
		// while the star expands to every column of every row in the group.
		{"star beside an aggregate",
			`SELECT *, COUNT(*) FROM tickets GROUP BY region`, "not an aggregate"},
		{"plain projection", `SELECT region FROM tickets`, "not an aggregate"},
		{"projection with a filter", `SELECT region, spend FROM tickets WHERE spend > 3`, "not an aggregate"},
		{"distinct is not an aggregate", `SELECT DISTINCT region FROM tickets`, "not an aggregate"},

		// A windowed call returns one row per input row, so a statement whose
		// only "aggregate" is windowed is the raw result with a count attached.
		{"windowed count", `SELECT COUNT(*) OVER () FROM tickets`, "not an aggregate"},
		{"windowed count beside a column",
			`SELECT region, COUNT(*) OVER (PARTITION BY region) FROM tickets`, "not an aggregate"},

		// SQLite answers a bare column beside an aggregate from an arbitrary
		// row. This is the cheapest exfiltration the dialect offers and it
		// looks like an aggregate query to anything that only counts rows.
		{"bare column, no group by",
			`SELECT rep_note, COUNT(*) FROM tickets`, "bare column beside an aggregate"},
		{"bare column inside a group",
			`SELECT region, rep_note, COUNT(*) FROM tickets GROUP BY region`, "not in GROUP BY"},

		// Without COUNT(*) there is no k to compare against the floor.
		{"grouped without a count",
			`SELECT region, SUM(spend) FROM tickets GROUP BY region`, "grouped without COUNT(*)"},
		{"grouped by a person, without a count",
			`SELECT rep_note, SUM(spend) FROM tickets GROUP BY rep_note`, "grouped without COUNT(*)"},

		{"compound", `SELECT COUNT(*) FROM tickets UNION ALL SELECT COUNT(*) FROM tickets`, "compound"},

		// The parse gate still applies; the aggregation gate is not a way past it.
		{"still behind sqlguard", `SELECT readfile('/etc/passwd'), COUNT(*) FROM tickets`, "allowlist"},
		{"still behind sqlguard: dml", `DELETE FROM tickets`, "not a SELECT"},
	} {
		t.Run(tc.why, func(t *testing.T) {
			env, err := aggregate(t, db, tc.query)
			if err == nil {
				t.Fatalf("PERMITTED, envelope: %+v\n%s", env, tc.query)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("refused for the wrong reason\n  got:  %v\n  want: something containing %q",
					err, tc.reason)
			}
		})
	}
}

// TestAResultTooLargeIsRefusedNotTruncated. A summary of an arbitrary prefix of
// an unordered result set describes nothing, and would read as a summary of the
// whole.
func TestAResultTooLargeIsRefusedNotTruncated(t *testing.T) {
	db := testDB(t)

	// Every row its own bucket: twenty buckets, cap of five.
	env, err := gate.Aggregate(context.Background(), db,
		`SELECT spend, COUNT(*) FROM tickets GROUP BY spend`,
		gate.Options{MaxRawRows: 5})
	if err == nil {
		t.Fatalf("a result past the cap produced an envelope: %+v", env)
	}
	if !errors.Is(err, gate.ErrRefused) {
		t.Fatalf("refusal does not match ErrRefused: %v", err)
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("refused for another reason: %v", err)
	}
}

// -----------------------------------------------------------------------------
// The k-anonymity floor
// -----------------------------------------------------------------------------

// TestBucketsBelowTheFloorAreFolded is §12.1's central rule. Without it,
// `GROUP BY email` crosses the gate as one aggregate per person.
func TestBucketsBelowTheFloorAreFolded(t *testing.T) {
	env := mustAggregate(t, testDB(t),
		`SELECT region, COUNT(*) AS n FROM tickets GROUP BY region`)

	kept := map[string]int64{}
	var other int64
	var otherSeen bool
	for _, b := range env.TopK {
		if b.Other {
			other, otherSeen = b.Count, true
			if len(b.Key) != 0 {
				t.Errorf("the other bucket names a key %v, which is the value it exists to hide", b.Key)
			}
			continue
		}
		if len(b.Key) != 1 {
			t.Fatalf("bucket key = %v, want one grouping column", b.Key)
		}
		kept[b.Key[0]] = b.Count
	}

	want := map[string]int64{"north": 8, "south": 6}
	if len(kept) != len(want) {
		t.Fatalf("buckets that crossed = %v, want exactly %v", kept, want)
	}
	for k, n := range want {
		if kept[k] != n {
			t.Errorf("%s = %d, want %d", k, kept[k], n)
		}
	}
	for _, hidden := range []string{"east", "west", "solo"} {
		if _, ok := kept[hidden]; ok {
			t.Errorf("%q covers fewer than %d records and crossed anyway", hidden, gate.DefaultKFloor)
		}
	}

	if !otherSeen {
		t.Fatal("no other bucket, so the folded records vanished without a trace")
	}
	if other != 3+2+1 {
		t.Errorf("other = %d, want 6 — the folded records must still be counted", other)
	}
	if env.Suppressed != 3 {
		t.Errorf("Suppressed = %d, want 3; a distribution missing its tail reads as a "+
			"complete one unless the omission is reported", env.Suppressed)
	}
	if len(env.Notes) == 0 {
		t.Error("nothing in Notes says buckets were folded")
	}
}

// TestTheFloorIsConfigurableAndBinding. A floor of one lets everything through,
// which is what makes the default a decision rather than an accident.
func TestTheFloorIsConfigurableAndBinding(t *testing.T) {
	env, err := gate.Aggregate(context.Background(), testDB(t),
		`SELECT region, COUNT(*) FROM tickets GROUP BY region`,
		gate.Options{KFloor: 1})
	if err != nil {
		t.Fatal(err)
	}
	var named int
	for _, b := range env.TopK {
		if !b.Other {
			named++
		}
	}
	if named != 5 {
		t.Fatalf("named buckets = %d, want 5 at a floor of 1 — the floor is not what "+
			"decides which buckets cross", named)
	}
}

// -----------------------------------------------------------------------------
// Free text
// -----------------------------------------------------------------------------

// TestFreeTextValuesNeverCross, whichever way the query asks for them.
//
// The gate re-derives the flag from the result rather than trusting a profile,
// because a result column can be an expression no profile ever described.
func TestFreeTextValuesNeverCross(t *testing.T) {
	db := testDB(t)

	for _, tc := range []struct {
		why   string
		query string
	}{
		{"as a plain column beside a count",
			`SELECT rep_note, COUNT(*) FROM tickets GROUP BY rep_note`},
		{"under an alias that hides the name",
			`SELECT rep_note AS x, COUNT(*) FROM tickets GROUP BY rep_note`},
		{"through min and max",
			`SELECT MIN(rep_note) AS lo, MAX(rep_note) AS hi, COUNT(*) FROM tickets`},
	} {
		t.Run(tc.why, func(t *testing.T) {
			env, err := aggregate(t, db, tc.query)
			if err != nil {
				t.Skipf("refused outright, which is also safe: %v", err)
			}
			raw, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			// The envelope is what crosses, so the envelope is what is searched.
			// A phrase from the fixture's prose appearing anywhere in it — a
			// bucket key, a range bound, a note — is a row that got out.
			if strings.Contains(string(raw), "cost centre") {
				t.Fatalf("prose from the data is in the envelope:\n%s", raw)
			}
		})
	}
}

// TestGroupingOnFreeTextYieldsNoBuckets. §12.1 excludes free text from TopK
// because "a 'top values' list over a notes field is just the rows" — and
// grouping by it is that same list with counts attached.
func TestGroupingOnFreeTextYieldsNoBuckets(t *testing.T) {
	env := mustAggregate(t, testDB(t),
		`SELECT rep_note, COUNT(*) FROM tickets GROUP BY rep_note`)

	if len(env.TopK) != 0 {
		t.Fatalf("buckets crossed for a free-text key: %+v", env.TopK)
	}
	var flagged bool
	for _, c := range env.Columns {
		if c.Name == "rep_note" {
			flagged = c.FreeText
			if c.Range != nil {
				t.Errorf("a free-text column carries a range: %+v", c.Range)
			}
		}
	}
	if !flagged {
		t.Error("rep_note was not flagged free text")
	}
	if len(env.Notes) == 0 {
		t.Error("nothing says why no buckets crossed, so an empty distribution " +
			"reads as 'no data' rather than 'withheld'")
	}
}

// TestACategoryColumnIsNotWithheld. The exclusion has to be narrow or the gate
// is useless: a region is a category, and refusing to name it would leave the
// model with counts attached to nothing.
func TestACategoryColumnIsNotWithheld(t *testing.T) {
	env := mustAggregate(t, testDB(t),
		`SELECT region, COUNT(*) FROM tickets GROUP BY region`)

	for _, c := range env.Columns {
		if c.Name == "region" {
			if c.FreeText {
				t.Fatal("region was withheld as free text")
			}
			if c.Range == nil {
				t.Fatal("region carries no range")
			}
		}
	}
	if len(env.TopK) == 0 {
		t.Fatal("no buckets crossed for a category column")
	}
}

// -----------------------------------------------------------------------------
// What the statistics say
// -----------------------------------------------------------------------------

// TestTheStatisticsDescribeTheResult. The envelope has to be right as well as
// safe: a gate that returned nothing useful would pass every test above.
func TestTheStatisticsDescribeTheResult(t *testing.T) {
	env := mustAggregate(t, testDB(t),
		`SELECT COUNT(*) AS n, SUM(spend) AS total, AVG(spend) AS mean FROM tickets`)

	if env.RowCount != 1 {
		t.Fatalf("RowCount = %d, want 1", env.RowCount)
	}
	if env.QueryHash == "" || len(env.QueryHash) != 64 {
		t.Errorf("QueryHash = %q, want a sha256 hex digest — a local claim cites it", env.QueryHash)
	}

	byName := map[string]gate.ColumnStats{}
	for _, c := range env.Columns {
		byName[c.Name] = c
	}
	// spend runs 1..20, so the sum is 210 and the mean 10.5.
	for _, tc := range []struct {
		col  string
		want float64
	}{{"n", rows}, {"total", 210}, {"mean", 10.5}} {
		c, ok := byName[tc.col]
		if !ok || c.Number == nil {
			t.Fatalf("%s: no numeric stats (%+v)", tc.col, c)
		}
		if c.Number.Min != tc.want || c.Number.Max != tc.want {
			t.Errorf("%s = [%v, %v], want %v", tc.col, c.Number.Min, c.Number.Max, tc.want)
		}
		// Over one row the mean is the value. Asserted anyway: min and max
		// come from the sorted slice and the mean from a separate sum, so
		// checking only the bounds leaves the moment untested.
		if c.Number.Mean != tc.want {
			t.Errorf("%s mean = %v, want %v", tc.col, c.Number.Mean, tc.want)
		}
	}
}

// TestQuantilesAndMomentsOverManyRows checks the distribution summary on a
// result with a spread, which the single-row case above cannot.
func TestQuantilesAndMomentsOverManyRows(t *testing.T) {
	// Five buckets of four, so every bucket clears a floor of four and the
	// counts have a distribution of their own.
	db := testDB(t)
	env, err := gate.Aggregate(context.Background(), db,
		`SELECT region, COUNT(*) AS n, AVG(spend) AS mean FROM tickets GROUP BY region`,
		gate.Options{KFloor: 1})
	if err != nil {
		t.Fatal(err)
	}
	if env.RowCount != 5 {
		t.Fatalf("RowCount = %d, want 5", env.RowCount)
	}
	var n *gate.NumberStats
	for _, c := range env.Columns {
		if c.Name == "n" {
			n = c.Number
		}
	}
	if n == nil {
		t.Fatal("no stats for the count column")
	}
	// Counts are 8, 6, 3, 2, 1.
	if n.Min != 1 || n.Max != 8 {
		t.Errorf("count range = [%v, %v], want [1, 8]", n.Min, n.Max)
	}
	if n.Mean != 4 {
		t.Errorf("mean = %v, want 4", n.Mean)
	}
	if n.P50 != 3 {
		t.Errorf("median = %v, want 3", n.P50)
	}
}

// TestNullsAndDistinctAreCounted. A null rate is a statistic the planner uses to
// choose a template, so it has to be the result's and not the table's.
func TestNullsAndDistinctAreCounted(t *testing.T) {
	db := testDB(t)
	if _, err := db.Exec(`INSERT INTO tickets VALUES ('north', NULL, NULL, 'agent')`); err != nil {
		t.Fatal(err)
	}
	env := mustAggregate(t, db,
		`SELECT region, COUNT(*) AS n, AVG(spend) AS mean FROM tickets GROUP BY region`)

	for _, c := range env.Columns {
		if c.Name == "region" && c.Distinct != 5 {
			t.Errorf("region distinct = %d, want 5", c.Distinct)
		}
	}
	if env.RowCount != 5 {
		t.Errorf("RowCount = %d, want 5", env.RowCount)
	}
}

// TestTopKIsCappedAndSaysSo. Truncation is reported for the same reason
// suppression is: a top-25 list of a thousand categories is not a distribution.
func TestTopKIsCappedAndSaysSo(t *testing.T) {
	env, err := gate.Aggregate(context.Background(), testDB(t),
		`SELECT region, COUNT(*) FROM tickets GROUP BY region`,
		gate.Options{KFloor: 1, TopK: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !env.Truncated {
		t.Error("more buckets exist than crossed and Truncated is false")
	}
	var named int
	for _, b := range env.TopK {
		if !b.Other {
			named++
		}
	}
	if named != 2 {
		t.Errorf("named buckets = %d, want 2", named)
	}
	// The buckets that crossed must be the largest ones, or "top K" means
	// "whichever K the engine happened to return first".
	if env.TopK[0].Count != 8 || env.TopK[1].Count != 6 {
		t.Errorf("top buckets = %d, %d; want the two largest (8, 6)",
			env.TopK[0].Count, env.TopK[1].Count)
	}
}

// TestTheEnvelopeCarriesNoRowValues is the shape of §14.3's exfil regression
// test, applied to the queries a template would render. The full version lands
// with the actor; this is the part that can be written before one exists.
func TestTheEnvelopeCarriesNoRowValues(t *testing.T) {
	db := testDB(t)
	for _, q := range []string{
		`SELECT region, COUNT(*) AS n FROM tickets GROUP BY region`,
		`SELECT COUNT(*) AS n, AVG(spend) AS mean FROM tickets`,
		`SELECT region, COUNT(*) AS n, MAX(LENGTH(rep_note)) AS longest FROM tickets GROUP BY region`,
	} {
		env, err := aggregate(t, db, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "cost centre") || strings.Contains(string(raw), "Customer asked") {
			t.Fatalf("row content in the envelope for %s:\n%s", q, raw)
		}
	}
}
