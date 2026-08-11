package connector_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/connector"
	_ "modernc.org/sqlite"
)

// Regression tests for the M8 review.
//
// Every case is a data shape the milestone never fed the connector: an uppercase
// identifier, an integer too large for float64, a byte-order mark on a file that
// was not a CSV, a non-finite literal, a re-import that fails halfway.

// TestACamelCaseDatabaseSurvivesRegistration.
//
// safeIdent accepted only lower case, so an ordinary database registered with no
// error and a profile of ONE table with ONE column. Tables were skipped
// silently and columns with no record at all, and the `skipped` list surfaced
// only when zero tables survived — so the model planned over a schema that was
// not the user's data.
func TestACamelCaseDatabaseSurvivesRegistration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "warehouse.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE Orders (OrderID INTEGER, Amount REAL, region TEXT)`,
		`INSERT INTO Orders VALUES (1, 9.5, 'north'), (2, 4.0, 'south')`,
		`CREATE TABLE parts (partId INTEGER, qty INTEGER)`,
		`INSERT INTO parts VALUES (1, 10), (2, 20)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	c, err := connector.Ingest(context.Background(), "wh", path, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	orders, ok := c.Table("Orders")
	if !ok {
		t.Fatalf("the Orders table vanished; profile holds %+v", c.Tables)
	}
	for _, want := range []string{"OrderID", "Amount", "region"} {
		if _, ok := orders.Column(want); !ok {
			t.Errorf("column %s vanished", want)
		}
	}
	if _, ok := c.Table("parts"); !ok {
		t.Error("the lower-case table vanished")
	}
	if len(c.Skipped) != 0 {
		t.Errorf("nothing should have been skipped: %v", c.Skipped)
	}

	// And a quoted uppercase identifier is queryable, which is the point.
	handle, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	var n int64
	if err := handle.QueryRow(`SELECT COUNT(*) FROM "Orders"`).Scan(&n); err != nil {
		t.Fatalf("cannot query the table that registered: %v", err)
	}
}

// TestAnUnusableNameIsReportedRatherThanDropped. The other half: when something
// genuinely cannot be used, registration has to say so.
func TestAnUnusableNameIsReportedRatherThanDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "odd.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE "with space" (a INTEGER)`,
		`INSERT INTO "with space" VALUES (1)`,
		`CREATE TABLE fine (a INTEGER, "b c" INTEGER)`,
		`INSERT INTO fine VALUES (1, 2)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	c, err := connector.Ingest(context.Background(), "odd", path, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.Skipped, " | ")
	for _, want := range []string{"with space", "b c"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q was dropped with no record: %v", want, c.Skipped)
		}
	}
}

// TestJSONLIntegersKeepTheirPrecision.
//
// json.Unmarshal into map[string]any decodes every number as float64, so
// 1234567890123456789 and 1234567890123456788 both stored as ...800 and the
// profile reported two distinct values over three rows. Snowflake and order ids
// are exactly what a JSONL export carries.
func TestJSONLIntegersKeepTheirPrecision(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "orders.jsonl")
	if err := os.WriteFile(src, []byte(
		`{"order_id":1234567890123456789,"n":1}`+"\n"+
			`{"order_id":1234567890123456788,"n":2}`+"\n"+
			`{"order_id":9007199254740993,"n":3}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "o", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT order_id FROM orders ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []string{"1234567890123456789", "1234567890123456788", "9007199254740993"}
	var i int
	for rows.Next() {
		var got string
		if err := rows.Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want[i] {
			t.Errorf("row %d stored %s, want %s", i+1, got, want[i])
		}
		i++
	}
	tbl, _ := c.Table("orders")
	if col, _ := tbl.Column("order_id"); col.Distinct != 3 {
		t.Errorf("distinct = %d over 3 rows, want 3 — two ids collapsed", col.Distinct)
	}
}

// TestAByteOrderMarkIsStrippedFromEveryFormat. The strip was wired into the CSV
// path only, so a BOM-prefixed .jsonl failed while the same bytes in a .csv
// imported fine.
func TestAByteOrderMarkIsStrippedFromEveryFormat(t *testing.T) {
	const bom = "\ufeff"
	for _, tc := range []struct{ name, body string }{
		{"b.jsonl", bom + `{"a":1}` + "\n"},
		{"b.csv", bom + "a\n1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, tc.name)
			if err := os.WriteFile(src, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := connector.Ingest(context.Background(), "b", src, filepath.Join(dir, "s.db"))
			if err != nil {
				t.Fatalf("a byte-order mark made the file unimportable: %v", err)
			}
			tbl := c.Tables[0]
			if _, ok := tbl.Column("a"); !ok {
				var got []string
				for _, col := range tbl.Columns {
					got = append(got, col.Name)
				}
				t.Errorf("column `a` is missing; got %v", got)
			}
		})
	}
}

// TestANonFiniteLiteralIsText. ParseFloat accepts "inf" and "NaN", so a column
// of 1.5, inf, 2.5 inferred as real, stored +Inf, and reported max="+Inf" to the
// planner — after which every moment computed over it is Inf or NaN.
func TestANonFiniteLiteralIsText(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rates.csv")
	if err := os.WriteFile(src, []byte("rate\n1.5\ninf\n2.5\nNaN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "r", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	tbl, _ := c.Table("rates")
	col, ok := tbl.Column("rate")
	if !ok {
		t.Fatal("no rate column")
	}
	if col.Type != connector.TypeText {
		t.Errorf("type = %q, want text — a cell spelling a non-finite value is not a number",
			col.Type)
	}
	if strings.Contains(col.Min+col.Max, "Inf") || strings.Contains(col.Min+col.Max, "NaN") {
		t.Errorf("a non-finite value reached the profile: min=%q max=%q", col.Min, col.Max)
	}
}

// TestAFailedReimportLeavesTheConnectorWorking.
//
// importFiles deleted the destination before reading any file, so a failure left
// no scratch database while the registry still pointed at one. The connector went
// from working to unopenable, and the error blamed an active writer.
func TestAFailedReimportLeavesTheConnectorWorking(t *testing.T) {
	dir := t.TempDir()
	scratch := filepath.Join(dir, "sales.db")
	good := filepath.Join(dir, "good.csv")
	if err := os.WriteFile(good, []byte("a,b\n1,2\n3,4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Ingest(context.Background(), "sales", good, scratch); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(dir, "empty.csv")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Ingest(context.Background(), "sales", empty, scratch); err == nil {
		t.Fatal("importing an empty file succeeded")
	}

	// The previous import is still there and still queryable.
	c := connector.Connector{Name: "sales", DBPath: scratch}
	db, err := c.Open()
	if err != nil {
		t.Fatalf("the failed re-import destroyed the working connector: %v", err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM good`).Scan(&n); err != nil {
		t.Fatalf("the previous data is gone: %v", err)
	}
	if n != 2 {
		t.Errorf("rows = %d, want the 2 from the successful import", n)
	}
}

// TestRaggedRowsAreReported. Extra fields past the header can only be dropped —
// but dropping data silently is how a misaligned export reads as a clean import.
func TestRaggedRowsAreReported(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "wide.csv")
	if err := os.WriteFile(src, []byte("a,b\n1,2\n3,4,5,6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "w", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skipped) == 0 {
		t.Fatal("a row with four fields against a two-column header left no record")
	}
	if !strings.Contains(strings.Join(c.Skipped, " "), "more fields") {
		t.Errorf("the note does not say what happened: %v", c.Skipped)
	}
}
