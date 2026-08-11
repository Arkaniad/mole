package connector_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/connector"
)

// M8 slice 0.
//
// The property under test is §12.2's first defense: the connector's own handle
// cannot write, whatever statement reaches it. Everything else here — type
// inference, folder intake, the free-text flag — exists to serve §12.3's
// "hypothesis templates over the connector's known schema" and is tested for
// the same reason: a wrong type or a missing flag becomes a wrong query, and a
// wrong query is what the gates downstream are trying to make impossible.

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func ingest(t *testing.T, source string) connector.Connector {
	t.Helper()
	c, err := connector.Ingest(context.Background(), "local", source,
		filepath.Join(t.TempDir(), "scratch.db"))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return c
}

// The first revenue value is a whole number and a later one is not. That
// ordering is the fixture's job: a sampled inference that stopped after row one
// would call the column INTEGER and round every value under it, and a fixture
// whose first row already forces REAL cannot tell the two apart.
const salesCSV = `region,units,revenue,closed_at,repeat
north,10,1050,2024-01-05,true
south,4,220,2024-02-11,false
north,7,880.25,2024-03-02,true
east,,15.5,2024-03-09,false
`

// TestTheHandleRefusesToWrite is the §12.2 defense-in-depth proof.
//
// It asserts the refusal, not the flag that produced it. A test that checked
// the DSN would pass while a driver quietly ignored what it said — and one of
// the two flags turns out to be worth less than it looks, which is the reason
// this is written as a behavioural test. See the query_only subtest.
func TestTheHandleRefusesToWrite(t *testing.T) {
	dir := t.TempDir()
	c := ingest(t, write(t, dir, "sales.csv", salesCSV))

	db, err := c.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, tc := range []struct{ name, stmt string }{
		{"drop", `DROP TABLE sales`},
		{"delete", `DELETE FROM sales`},
		{"update", `UPDATE sales SET units = 0`},
		{"insert", `INSERT INTO sales (region) VALUES ('x')`},
		{"create", `CREATE TABLE exfil (a TEXT)`},
		{"alter", `ALTER TABLE sales RENAME TO gone`},
		{"index", `CREATE INDEX i ON sales(region)`},
		{"vacuum", `VACUUM`},
		// writable_schema is accepted as a setting but buys nothing: the write
		// it is meant to unlock is refused below.
		{"schema_update", `UPDATE sqlite_master SET sql = 'CREATE TABLE sales (x)'`},
		{"schema_delete", `DELETE FROM sqlite_master`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(context.Background(), tc.stmt); err == nil {
				t.Fatalf("the read-only handle executed %q; §12.2's least-privilege "+
					"defense is not holding, so a bypassed sqlguard is the only thing "+
					"standing between LLM-authored SQL and the user's data", tc.stmt)
			}
		})
	}

	// The two flags are not equals, and finding that out is why this test runs
	// statements instead of inspecting the DSN.
	//
	// `PRAGMA query_only = 0` SUCCEEDS on this handle — the SQL-layer defense
	// can be switched off by the very statements it is meant to constrain, so
	// on its own it stops an accident and not an attempt. `mode=ro` refuses at
	// the VFS layer and cannot be reached by SQL at all; it is the defense that
	// actually holds, and query_only is the belt to its braces rather than the
	// other way round.
	//
	// The consequence for slice 1: `sqlguard` must reject PRAGMA outright. It
	// is not a harmless read-only verb.
	if _, err := db.Exec(`PRAGMA query_only = 0`); err != nil {
		t.Logf("note: query_only is no longer settable from a query (%v) — "+
			"the driver has become stricter, which is fine, but the assertion "+
			"below is the one that matters", err)
	}
	if _, err := db.Exec(`INSERT INTO sales (region) VALUES ('after')`); err == nil {
		t.Fatal("a write succeeded after PRAGMA query_only = 0: the handle's only " +
			"real protection was a setting that SQL can switch off")
	}

	// The data is intact and readable, which is the other half: a handle that
	// refused everything would pass the loop above and be useless.
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM sales`).Scan(&n); err != nil {
		t.Fatalf("read through the read-only handle: %v", err)
	}
	if n != 4 {
		t.Fatalf("row count = %d, want 4", n)
	}
}

// TestTypesAreInferredFromEveryRow. The narrowing pass has to see the whole
// file: revenue is an integer for one row and a decimal for the next, and a
// sampled inference that stopped early would declare it INTEGER and then round
// every value it stored.
func TestTypesAreInferredFromEveryRow(t *testing.T) {
	dir := t.TempDir()
	c := ingest(t, write(t, dir, "sales.csv", salesCSV))

	tbl, ok := c.Table("sales")
	if !ok {
		t.Fatalf("no table named sales in %+v", c.Tables)
	}
	want := map[string]connector.ColumnType{
		"region":    connector.TypeText,
		"units":     connector.TypeInteger,
		"revenue":   connector.TypeReal,
		"closed_at": connector.TypeTime,
		"repeat":    connector.TypeBool,
	}
	for name, wantType := range want {
		col, ok := tbl.Column(name)
		if !ok {
			t.Fatalf("column %s missing", name)
		}
		if col.Type != wantType {
			t.Errorf("%s: type = %q, want %q", name, col.Type, wantType)
		}
	}

	// A blank cell is missing data, not zero. Storing it as zero would move
	// every mean the aggregation gate reports for the column.
	if col, _ := tbl.Column("units"); col.Nulls != 1 {
		t.Errorf("units nulls = %d, want 1 (the blank cell)", col.Nulls)
	}

	db, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var total float64
	if err := db.QueryRow(`SELECT SUM(revenue) FROM sales`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total < 2165.7 || total > 2165.8 {
		t.Errorf("SUM(revenue) = %v, want 2165.75 — the column did not store decimals", total)
	}
}

// TestAFolderBecomesOneTablePerFile. Registering a directory is what people
// actually have: a year of monthly exports, not one tidy file.
func TestAFolderBecomesOneTablePerFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "jan.csv", "region,units\nnorth,3\n")
	write(t, dir, "feb.csv", "region,units\nsouth,5\nnorth,1\n")
	write(t, dir, "events.jsonl", `{"kind":"signup","n":2}`+"\n"+`{"kind":"churn","n":1}`+"\n")
	write(t, dir, "notes.md", "not data")

	c := ingest(t, dir)

	rows := map[string]int64{}
	for _, tb := range c.Tables {
		rows[tb.Name] = tb.Rows
	}
	want := map[string]int64{"jan": 1, "feb": 2, "events": 2}
	if len(rows) != len(want) {
		t.Fatalf("tables = %v, want exactly %v — an unreadable file was imported "+
			"or a readable one was dropped", rows, want)
	}
	for name, n := range want {
		if rows[name] != n {
			t.Errorf("%s: %d rows, want %d", name, rows[name], n)
		}
	}
}

// TestFilesThatCollideOnNameBothSurvive. A folder holding sales.csv and
// sales.tsv — the same export in two formats, which is what a folder of
// exports actually looks like — gives both files the same base name. Quietly
// keeping one would report on half the data while looking like a complete
// answer.
func TestFilesThatCollideOnNameBothSurvive(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "sales.csv", "a\n1\n")
	write(t, dir, "sales.tsv", "a\n2\n3\n")

	c := ingest(t, dir)
	if len(c.Tables) != 2 {
		t.Fatalf("tables = %+v, want 2", c.Tables)
	}
	var total int64
	for _, tb := range c.Tables {
		total += tb.Rows
	}
	if total != 3 {
		t.Fatalf("total rows = %d, want 3 — a colliding file was dropped", total)
	}
}

// TestFreeTextColumnsAreFlaggedAndTheirValuesWithheld.
//
// §12.1 excludes free text from TopK because "a 'top values' list over a notes
// field is just the rows". The exclusion happens at the gate, but the decision
// has to happen here — the gate only ever sees aggregates and cannot recompute
// it. Min/Max are withheld for the same reason: they are values, and on a prose
// column two of them are two rows.
func TestFreeTextColumnsAreFlaggedAndTheirValuesWithheld(t *testing.T) {
	dir := t.TempDir()
	// Every rule needs a column only it can flag, or the rule is untested.
	//
	//   resolution      long prose that REPEATS — length rule only
	//   session_ref     short, unique, unremarkable name — near-unique rule only
	//   customer_email  short and unique but under a matching name — name rule only
	//   ssn             a NUMBER under a matching name, which the type rule used
	//                   to exempt before the name rule was ever consulted
	//   order_id        a unique number: not prose, but its extremes are two
	//                   specific records, so it gets no range
	//   region, status  categories with enough records per value to name
	//
	// Twenty rows, because the range floor needs rows/distinct >= 5 and a
	// four-row fixture cannot clear it for anything.
	var body strings.Builder
	body.WriteString("region,order_id,ssn,session_ref,customer_email,resolution,status\n")
	regions := []string{
		"north", "north", "north", "north", "north", "north", "north", "north", "north", "north",
		"south", "south", "south", "south", "south", "south",
		"east", "east", "east", "east",
	}
	prose := []string{
		"Reissued the invoice against the correct billing period and credited the difference",
		"Shipped a second replacement unit after the first also failed on arrival",
	}
	for i, region := range regions {
		status := "open"
		if i%2 == 1 {
			status = "closed"
		}
		fmt.Fprintf(&body, "%s,9021000000000%04d,%09d,sess-%012x,p%02d@example.org,%q,%s\n",
			region, i, 100000000+i*7, i*2654435761, i, prose[i%2], status)
	}
	write(t, dir, "tickets.csv", body.String())

	tbl, ok := ingest(t, dir).Table("tickets")
	if !ok {
		t.Fatal("no tickets table")
	}
	for _, tc := range []struct {
		col           string
		freeText      bool
		rangeWithheld bool
		why           string
	}{
		{"resolution", true, true, "prose, flagged on average length alone"},
		{"session_ref", true, true, "different in every row — listing it lists the rows"},
		{"customer_email", true, true, "flagged by name; listing its top values lists people"},
		// The finding this test missed. A social security number is a personal
		// identifier whether it is stored as text or as an integer, and the name
		// rule used to sit behind a type check that exempted every number.
		{"ssn", true, true, "flagged by name, and its type is the least relevant fact about it"},
		// Not prose, so not free text — but every value is unique, so its
		// minimum and maximum are two specific records and no range crosses.
		{"order_id", false, true, "unique per row: its extremes identify two records"},
		{"region", false, false, "a category with four or more records per value"},
		{"status", false, false, "a category"},
	} {
		col, ok := tbl.Column(tc.col)
		if !ok {
			t.Fatalf("column %s missing", tc.col)
		}
		if col.FreeText != tc.freeText {
			t.Errorf("%s: FreeText = %v, want %v (%s)", tc.col, col.FreeText, tc.freeText, tc.why)
		}
		// The range is a separate decision from the flag, and has to be:
		// order_id is not prose and still must not have its extremes reported.
		if withheld := col.Min == "" && col.Max == ""; withheld != tc.rangeWithheld {
			t.Errorf("%s: range withheld = %v, want %v (%s) — min=%q max=%q",
				tc.col, withheld, tc.rangeWithheld, tc.why, col.Min, col.Max)
		}
	}
}

// TestASmallTableGetsNoRangesAtAll. Under a floor of five, a table with fewer
// than five rows has no value that describes five records — so nothing in it can
// be named, whatever the column looks like.
func TestASmallTableGetsNoRangesAtAll(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "tiny.csv",
		"name,dept,salary\n"+
			"Ada Lovelace,eng,100\n"+
			"Jan Kowalski,ops,200\n"+
			"Grace Hopper,eng,150\n")

	tbl, ok := ingest(t, dir).Table("tiny")
	if !ok {
		t.Fatal("no tiny table")
	}
	for _, col := range tbl.Columns {
		if col.Min != "" || col.Max != "" {
			t.Errorf("%s carries a range from a three-row table: min=%q max=%q — "+
				"these are individual records", col.Name, col.Min, col.Max)
		}
	}
}

// TestHostileHeadersCannotAuthorSQL. A CSV header is untrusted input in exactly
// the sense §12.3 means: it reaches the schema, the schema reaches a rendered
// query, and nothing between them asks where it came from.
func TestHostileHeadersCannotAuthorSQL(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "evil.csv",
		`ok,"a"" ); DROP TABLE evil; --","",  spaced  out  `+"\n"+
			"1,2,3,4\n")

	c := ingest(t, dir)
	tbl, ok := c.Table("evil")
	if !ok {
		t.Fatalf("no evil table in %+v", c.Tables)
	}
	if len(tbl.Columns) != 4 {
		t.Fatalf("columns = %+v, want 4", tbl.Columns)
	}
	for _, col := range tbl.Columns {
		for _, bad := range []string{`"`, `'`, ";", "-", " ", "(", ")"} {
			if strings.Contains(col.Name, bad) {
				t.Fatalf("identifier %q survived sanitization carrying %q", col.Name, bad)
			}
		}
	}
	// The table is still there, which it would not be had the header executed.
	db, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM evil`).Scan(&n); err != nil {
		t.Fatalf("table gone: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestJSONLTakesTheUnionOfKeys. Lines are objects, not rows, and a later line
// carrying a key the first did not is normal in an event log.
func TestJSONLTakesTheUnionOfKeys(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "events.jsonl",
		`{"kind":"signup","n":2}`+"\n"+
			`{"kind":"churn","n":1,"plan":"pro"}`+"\n"+
			`{"plan":"free","kind":"signup","n":4}`+"\n")

	tbl, ok := ingest(t, dir).Table("events")
	if !ok {
		t.Fatal("no events table")
	}
	if len(tbl.Columns) != 3 {
		t.Fatalf("columns = %+v, want kind, n, plan", tbl.Columns)
	}
	if col, _ := tbl.Column("n"); col.Type != connector.TypeInteger {
		t.Errorf("n: type = %q, want integer", col.Type)
	}
	// Keys out of order on the third line must still land in their own column.
	if col, _ := tbl.Column("plan"); col.Nulls != 1 || col.Distinct != 2 {
		t.Errorf("plan: nulls=%d distinct=%d, want 1 and 2 — keys were positioned by "+
			"order of appearance rather than by name", col.Nulls, col.Distinct)
	}
}

// TestAnExistingDatabaseIsAttachedNotCopied. Copying someone's database to
// profile it makes a second copy of their data to look after, in the milestone
// whose subject is the privacy boundary.
func TestAnExistingDatabaseIsAttachedNotCopied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "warehouse.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE orders (id INTEGER, placed_at DATETIME, total REAL, note TEXT)`,
		`INSERT INTO orders VALUES (1, '2024-01-01', 9.5, 'left by the door as usual please')`,
		`INSERT INTO orders VALUES (2, '2024-02-01', 4.0, 'ring the bell twice, the first is broken')`,
		`CREATE VIEW big AS SELECT * FROM orders WHERE total > 5`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	c := ingest(t, path)
	if c.Kind != connector.KindSQLite {
		t.Fatalf("kind = %q, want %q", c.Kind, connector.KindSQLite)
	}
	if c.DBPath != path {
		t.Fatalf("DBPath = %q, want the source itself (%q)", c.DBPath, path)
	}
	orders, ok := c.Table("orders")
	if !ok {
		t.Fatalf("no orders table in %+v", c.Tables)
	}
	if orders.Rows != 2 {
		t.Errorf("orders rows = %d, want 2", orders.Rows)
	}
	if col, _ := orders.Column("placed_at"); col.Type != connector.TypeTime {
		t.Errorf("placed_at: type = %q, want timestamp — a DATETIME column is a time axis "+
			"and a template needs to know it", col.Type)
	}
	if col, _ := orders.Column("total"); !col.Type.Numeric() {
		t.Errorf("total: type = %q, want a numeric type", col.Type)
	}
	if col, _ := orders.Column("note"); !col.FreeText {
		t.Error("note was not flagged free text")
	}
	if _, ok := c.Table("big"); !ok {
		t.Error("the view was skipped; a user who shaped their data into a view already " +
			"did the analyst's first job")
	}
}

// TestRegistryRoundTrip, including the duplicate-name refusal: a name is what
// "connector:<name>#<hash>" cites, so two of them make a local claim ambiguous
// about which data it came from.
func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "connectors.json")

	r, err := connector.LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.List()) != 0 {
		t.Fatal("a missing registry should read as empty, not as an error")
	}
	if err := r.Add(connector.Connector{Name: "sales", Source: "/data/sales.csv"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(connector.Connector{Name: "sales", Source: "/elsewhere"}); !errors.Is(err, connector.ErrDuplicateName) {
		t.Fatalf("second Add returned %v, want ErrDuplicateName", err)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("registry mode = %04o, want 0600 — it lists exactly which files "+
			"someone pointed a research tool at", perm)
	}

	again, err := connector.LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get("sales")
	if !ok || got.Source != "/data/sales.csv" {
		t.Fatalf("Get after reload = %+v, %v", got, ok)
	}
	if _, ok := again.Remove("sales"); !ok {
		t.Fatal("Remove reported nothing to remove")
	}
	if _, ok := again.Get("sales"); ok {
		t.Fatal("Get found a removed connector")
	}
}

// TestADirectoryWithNothingReadableIsRefused, and says what it saw. "no data"
// with no detail sends someone looking for a bug in the importer when the
// answer is that their exports are .xlsx.
func TestADirectoryWithNothingReadableIsRefused(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report.xlsx", "binary")
	write(t, dir, "readme.md", "words")

	_, err := connector.Ingest(context.Background(), "local", dir,
		filepath.Join(t.TempDir(), "scratch.db"))
	if !errors.Is(err, connector.ErrNoData) {
		t.Fatalf("err = %v, want ErrNoData", err)
	}
	if !strings.Contains(err.Error(), "2 other entries") {
		t.Errorf("the refusal does not say how many entries it ignored: %v", err)
	}
}
