package gate_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/gate"
	_ "modernc.org/sqlite"
)

// M8 slice 3 — §14.3's "Exfil regression: assert no row-level data crosses the
// aggregation gate (§12.1)".
//
// Written as a property over generated query shapes rather than as a list of
// examples, because the examples are the part that gets thought of. What the
// gate has to withstand is a query nobody wrote down, and the cases that have
// actually mattered so far — a bare column beside an aggregate, a windowed
// count, prose reached through MIN() — were all found by asking what else the
// dialect permits rather than by listing what a template would render.
//
// "Row-level data" is two claims, and both are asserted for every shape:
//
//  1. No value from a column holding prose or a personal identifier crosses.
//  2. No value crosses that describes fewer than KFloor records — a bucket of
//     one is a row however it is labelled.

// canary is embedded in every value that must never cross. Distinctive enough
// that a match is a leak and not a coincidence.
const canary = "Zq7Kx"

const exfilRows = 20

// exfilDB has a column for each way a value can fail to be a category.
func exfilDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "records.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(`CREATE TABLE records (
		region  TEXT,   -- a category, with buckets on both sides of the floor
		team    TEXT,   -- a category whose buckets all clear the floor
		code    TEXT,   -- unique per row, but too SHORT to look like free text
		email   TEXT,   -- unique, flagged by name rather than by shape
		note    TEXT,   -- unique prose, flagged by length
		spend   REAL,
		day     TEXT
	)`); err != nil {
		t.Fatal(err)
	}

	plan := []struct {
		region string
		n      int
	}{{"north", 8}, {"south", 6}, {"east", 3}, {"west", 2}, {"solo", 1}}

	i := 0
	for _, p := range plan {
		for k := 0; k < p.n; k++ {
			team := "alpha"
			if i%2 == 0 {
				team = "beta"
			}
			_, err := db.Exec(`INSERT INTO records VALUES (?, ?, ?, ?, ?, ?, ?)`,
				p.region,
				team,
				// Short and unique: the free-text rules do not fire on it, so
				// the k-anonymity floor is the only thing standing between this
				// value and a model.
				fmt.Sprintf("%s%02d", canary, i),
				fmt.Sprintf("%s%02d@example.org", canary, i),
				fmt.Sprintf("Customer %s%02d asked for the invoice to be reissued against another cost centre", canary, i),
				float64(i+1),
				fmt.Sprintf("2024-01-%02d", i+1),
			)
			if err != nil {
				t.Fatal(err)
			}
			i++
		}
	}
	return db
}

// exfilQueries is the cross product of somewhere to group and something to
// select. Most of these are queries no template would render; that is the
// point — the gate is what stands between a rendered query and the data, and
// it has to hold for statements it was not designed around.
func exfilQueries() []string {
	keys := []string{
		"region",
		"team",
		"code",
		"email",
		"note",
		"SUBSTR(note, 1, 24)",
		"LENGTH(note)",
		"day",
		"region, team",
	}
	measures := []string{
		"COUNT(*)",
		"COUNT(*), SUM(spend)",
		"COUNT(*), AVG(spend), MIN(spend), MAX(spend)",
		"COUNT(*), MIN(note), MAX(note)",
		"COUNT(*), MIN(code), MAX(code)",
		"COUNT(*), MIN(email), MAX(email)",
		"COUNT(*), group_concat(note)",
		"COUNT(*), group_concat(code)",
	}

	var out []string
	for _, k := range keys {
		for _, m := range measures {
			out = append(out,
				fmt.Sprintf(`SELECT %s, %s FROM records GROUP BY 1`, k, m),
				fmt.Sprintf(`SELECT %s, %s FROM records GROUP BY 1 HAVING COUNT(*) >= 1`, k, m),
				fmt.Sprintf(`SELECT %s, %s FROM records GROUP BY 1 ORDER BY 2 DESC LIMIT 3`, k, m),
				fmt.Sprintf(`WITH src AS (SELECT * FROM records) SELECT %s, %s FROM src GROUP BY 1`, k, m),
			)
		}
	}
	// Ungrouped aggregates, where there is one row and every value in it is an
	// aggregate over the whole table — except where SQLite lets it not be.
	for _, m := range measures {
		out = append(out, fmt.Sprintf(`SELECT %s FROM records`, m))
	}
	return out
}

// TestNoRowLevelDataCrossesTheGate is the regression itself.
func TestNoRowLevelDataCrossesTheGate(t *testing.T) {
	db := exfilDB(t)
	queries := exfilQueries()

	var crossed, refused int
	for _, q := range queries {
		env, err := gate.Aggregate(context.Background(), db, q, gate.Options{})
		if err != nil {
			// A refusal is a pass. The gate is allowed to answer "no".
			refused++
			continue
		}
		crossed++

		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		// The query is echoed back in the envelope and legitimately contains
		// whatever the statement said; it is the answer that must not carry the
		// data.
		body := strings.Replace(string(raw), mustJSON(t, env.Query), `""`, 1)

		if strings.Contains(body, canary) {
			t.Errorf("VALUE CROSSED\n  query:    %s\n  envelope: %s", q, body)
		}
		for _, b := range env.TopK {
			if b.Other {
				continue
			}
			if b.Count < gate.DefaultKFloor {
				t.Errorf("BUCKET OF %d CROSSED — a bucket below the floor describes "+
					"individual records\n  query:  %s\n  bucket: %+v", b.Count, q, b)
			}
		}
	}

	// A run in which everything was refused would pass both assertions while
	// proving the gate is unusable rather than safe.
	if crossed == 0 {
		t.Fatalf("every one of the %d generated queries was refused; the property "+
			"above held vacuously", len(queries))
	}
	t.Logf("%d queries: %d produced an envelope, %d were refused", len(queries), crossed, refused)
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
