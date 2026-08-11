package sqlguard_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/sqlguard"
	"github.com/rqlite/sql"
)

// M8 slice 1.
//
// This is a security gate, so the tests are adversarial rather than
// illustrative: the permitted cases exist to show the gate is usable, and the
// refused cases are the point. Two of them pin measured behaviour of the
// parser itself, because the guard's structure was chosen in response to what
// that library actually does rather than what its API suggests.

func TestPermittedStatements(t *testing.T) {
	for _, q := range []string{
		`SELECT COUNT(*) FROM sales`,
		`SELECT region, COUNT(*), AVG(revenue) FROM sales GROUP BY region`,
		`select region, count(*) from sales group by region`,
		`WITH monthly AS (SELECT strftime('%Y-%m', closed_at) AS m, SUM(revenue) AS r FROM sales GROUP BY 1)
		 SELECT m, r FROM monthly ORDER BY m`,
		`SELECT AVG(units) OVER (ORDER BY closed_at ROWS 6 PRECEDING) FROM sales`,
		`SELECT region, COUNT(*) FROM sales WHERE units > 3 GROUP BY region HAVING COUNT(*) >= 5`,
		`SELECT COUNT(*) FROM sales WHERE region IN (SELECT region FROM targets)`,
		`SELECT a.region, COUNT(*) FROM sales a JOIN targets b ON a.region = b.region GROUP BY 1`,
		`SELECT COUNT(*) FROM sales UNION ALL SELECT COUNT(*) FROM archive`,
		// A comment and a trailing semicolon are both normal in a rendered
		// template and neither is an attack on its own.
		`SELECT COUNT(*) FROM sales -- how many`,
		`SELECT COUNT(*) FROM sales;`,
		// Keyword-token functions, permitted without being on the allowlist.
		`SELECT CAST(units AS REAL), REPLACE(region, 'x', 'y') FROM sales`,
	} {
		t.Run(label(q), func(t *testing.T) {
			if err := sqlguard.Check(q); err != nil {
				t.Fatalf("a statement a template would render was refused: %v\n%s", err, q)
			}
		})
	}
}

func TestRefusedStatements(t *testing.T) {
	for _, tc := range []struct {
		why    string
		query  string
		reason string
	}{
		// DDL and DML. §12.2's "a generated DROP TABLE would execute".
		{"drop", `DROP TABLE sales`, "not a SELECT"},
		{"delete", `DELETE FROM sales`, "not a SELECT"},
		{"update", `UPDATE sales SET units = 0`, "not a SELECT"},
		{"insert", `INSERT INTO sales VALUES (1)`, "not a SELECT"},
		{"create", `CREATE TABLE x (a TEXT)`, "not a SELECT"},
		{"create as select", `CREATE TABLE x AS SELECT * FROM sales`, "not a SELECT"},
		{"alter", `ALTER TABLE sales RENAME TO gone`, "not a SELECT"},
		{"trigger", `CREATE TRIGGER t AFTER INSERT ON sales BEGIN SELECT 1; END`, "not a SELECT"},

		// A CTE is not a way around the type check.
		{"cte wrapping delete", `WITH m AS (SELECT 1) DELETE FROM sales`, "not a SELECT"},
		{"cte wrapping update", `WITH m AS (SELECT 1) UPDATE sales SET units = 0`, "not a SELECT"},

		// Stacking. The reason ParseStatements is used and not ParseStatement.
		{"stacked", `SELECT 1; DROP TABLE sales`, "multiple statements"},
		{"stacked trailing", `SELECT 1; DROP TABLE sales;`, "multiple statements"},
		{"stacked whitespace", "SELECT 1 ;\n\n DROP TABLE sales", "multiple statements"},

		// Statements the parser has no type for.
		{"attach", `ATTACH DATABASE '/etc/passwd' AS leak`, "not parseable"},
		{"vacuum", `VACUUM INTO '/tmp/copy.db'`, "not parseable"},

		// PRAGMA. Slice 0 measured that `PRAGMA query_only = 0` succeeds on the
		// connector's own handle, so this is not a harmless read-only verb —
		// it is the one statement that can switch off a defense.
		{"pragma", `PRAGMA query_only = 0`, "not a SELECT"},
		{"pragma read", `PRAGMA table_info(sales)`, "not a SELECT"},

		// Escape hatches. These parse as perfectly ordinary SELECTs.
		{"readfile", `SELECT readfile('/etc/passwd')`, "allowlist"},
		{"writefile", `SELECT writefile('/tmp/x', 'y')`, "allowlist"},
		{"load_extension", `SELECT load_extension('/tmp/evil.so')`, "allowlist"},
		{"edit", `SELECT edit(region) FROM sales`, "allowlist"},
		{"fts3_tokenizer", `SELECT fts3_tokenizer('x')`, "allowlist"},

		// …including where Walk cannot see them. These are the cases that
		// decided the guard's structure; see TestWalkIsNotAnExhaustiveTraversal.
		{"readfile inside a CTE",
			`WITH m AS (SELECT readfile('/etc/passwd') AS r FROM sales) SELECT COUNT(*) FROM m`,
			"allowlist"},
		{"load_extension inside a scalar subquery",
			`SELECT (SELECT load_extension('/tmp/evil.so')) FROM sales`, "allowlist"},
		{"readfile inside a derived table",
			`SELECT COUNT(*) FROM (SELECT readfile('/etc/passwd') AS r)`, "allowlist"},
		{"readfile inside a window filter",
			`SELECT COUNT(*) FILTER (WHERE readfile('/etc/passwd') IS NOT NULL) FROM sales`,
			"allowlist"},

		// Adjacency, not source text: neither a space nor a comment separates a
		// name from its parenthesis once the statement is tokens.
		{"space before paren", `SELECT readfile ('/etc/passwd')`, "allowlist"},
		{"block comment before paren", "SELECT readfile/* nothing to see */('/etc/passwd')", "allowlist"},
		{"line comment before paren", "SELECT readfile -- nothing to see\n('/etc/passwd')", "allowlist"},
		{"quoted identifier", `SELECT "readfile"('/etc/passwd')`, "allowlist"},
		{"backticked identifier", "SELECT `readfile`('/etc/passwd')", "allowlist"},
		{"mixed case", `SELECT ReadFile('/etc/passwd')`, "allowlist"},

		// Reserved names, as calls and as plain references.
		{"pragma function", `SELECT * FROM pragma_table_info('sales')`, "reserved"},
		{"sqlite_master", `SELECT sql FROM sqlite_master`, "reserved"},
		{"sqlite_master in a subquery",
			`SELECT COUNT(*) FROM sales WHERE region IN (SELECT name FROM sqlite_master)`, "reserved"},
		{"sqlite_ function", `SELECT sqlite_compileoption_get(0)`, "reserved"},
		// The table-valued pragma functions can be spelled WITHOUT parentheses,
		// which the identifier-followed-by-`(` rule therefore missed — and the
		// bare-name rule covered only the sqlite_ prefix. Verified reachable: it
		// passed Check and then executed on the read-only handle.
		{"bare pragma function", `SELECT COUNT(*) FROM pragma_table_list`, "reserved"},
		{"bare pragma in a subquery",
			`SELECT COUNT(*) FROM sales WHERE region IN (SELECT name FROM pragma_table_list)`,
			"reserved"},

		// An unknown function is refused even when it is harmless, because the
		// allowlist is what makes the escape hatches unreachable by default.
		{"unknown function", `SELECT some_new_builtin(units) FROM sales`, "allowlist"},

		{"empty", `   `, "empty"},
		{"comment only", `-- nothing`, "no statement"},
		// Refused by the parser, not by the vocabulary check: a statement
		// carrying an illegal token does not parse at all.
		{"bracket quoting", `SELECT [readfile]('/etc/passwd')`, "not parseable"},
	} {
		t.Run(tc.why, func(t *testing.T) {
			err := sqlguard.Check(tc.query)
			if err == nil {
				t.Fatalf("PERMITTED: %s", tc.query)
			}
			if !errors.Is(err, sqlguard.ErrRejected) {
				t.Fatalf("refusal does not match ErrRejected, so a caller cannot tell "+
					"a rejection from a guard failure: %v", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("refused for the wrong reason\n  got:  %v\n  want: something containing %q",
					err, tc.reason)
			}
		})
	}
}

// TestAStatementTooLongIsRefused. A rendered template is bounded; something
// approaching the cap is not one of ours, and parsing it is work done on
// input that has already failed to look like the thing it claims to be.
func TestAStatementTooLongIsRefused(t *testing.T) {
	q := `SELECT COUNT(*) FROM sales WHERE region IN (` +
		strings.Repeat(`'x',`, 4000) + `'y')`
	err := sqlguard.Check(q)
	if err == nil {
		t.Fatal("a statement past the cap was permitted")
	}
	if !strings.Contains(err.Error(), "too long") {
		t.Fatalf("refused for another reason first: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Pinned behaviour of the parser
// -----------------------------------------------------------------------------

// TestParseStatementSilentlyDropsWhatFollows records why the guard uses
// ParseStatements.
//
// The single-statement parser returns the first statement and no error for a
// stacked pair, so a guard written on it would inspect `SELECT 1`, approve, and
// hand the whole string — DROP included — to the driver. That is the exact
// shape of the bug this package exists to prevent, and it is one identifier
// away in the API.
func TestParseStatementSilentlyDropsWhatFollows(t *testing.T) {
	const stacked = `SELECT 1; DROP TABLE sales`

	p := sql.NewParser(strings.NewReader(stacked))
	first, err := p.ParseStatement()
	if err != nil {
		t.Skipf("ParseStatement now reports %v; the note in checkShape needs revisiting", err)
	}
	if _, ok := first.(*sql.SelectStatement); !ok {
		t.Fatalf("ParseStatement returned %T, expected the leading SELECT", first)
	}
	t.Log("ParseStatement approved the leading SELECT of a stacked pair without error")

	if err := sqlguard.Check(stacked); err == nil {
		t.Fatal("the guard permitted a stacked statement")
	}
}

// TestWalkIsNotAnExhaustiveTraversal is why the function check reads tokens.
//
// It is not a criticism of the library — Walk is fine for what it is for. It is
// a statement about what may be built on it: a security check that assumes Walk
// visits every expression has holes in CTE bodies and subqueries, which is
// precisely where an escape hatch would be placed. If a later version closes
// these gaps this test fails loudly, which is the right outcome: it means the
// AST becomes a viable second check rather than that the token check should go.
func TestWalkIsNotAnExhaustiveTraversal(t *testing.T) {
	// Positive control first. Without it a collector that recognised nothing at
	// all would report "Walk did not reach the call" for every query below and
	// this test would pass while asserting nothing.
	if !walkSees(t, `SELECT readfile('/etc/passwd') FROM sales`) {
		t.Fatal("the collector does not see a call Walk certainly visits, so the " +
			"negative results below mean nothing")
	}

	for _, q := range []string{
		`WITH m AS (SELECT readfile('/etc/passwd') AS r FROM sales) SELECT COUNT(*) FROM m`,
		`SELECT (SELECT load_extension('/tmp/evil.so')) FROM sales`,
	} {
		p := sql.NewParser(strings.NewReader(q))
		stmts, err := p.ParseStatements()
		if err != nil || len(stmts) != 1 {
			t.Fatalf("setup: %v (%d statements)", err, len(stmts))
		}
		v := &callCollector{}
		if _, err := sql.Walk(v, stmts[0]); err != nil {
			t.Fatal(err)
		}
		if v.sawEscape {
			t.Errorf("Walk now reaches the call in %q — the AST has become usable "+
				"as a second check; the token check should stay regardless", q)
		}
		// The guard catches it anyway, which is the property that matters.
		if err := sqlguard.Check(q); err == nil {
			t.Fatalf("the guard permitted %q", q)
		}
	}
}

// walkSees reports whether a Walk over q reaches an escape-hatch call.
func walkSees(t *testing.T, q string) bool {
	t.Helper()
	p := sql.NewParser(strings.NewReader(q))
	stmts, err := p.ParseStatements()
	if err != nil || len(stmts) != 1 {
		t.Fatalf("setup: %v (%d statements)", err, len(stmts))
	}
	v := &callCollector{}
	if _, err := sql.Walk(v, stmts[0]); err != nil {
		t.Fatal(err)
	}
	return v.sawEscape
}

type callCollector struct{ sawEscape bool }

func (c *callCollector) Visit(n sql.Node) (sql.Visitor, sql.Node, error) {
	if call, ok := n.(*sql.Call); ok && call.Name != nil {
		switch strings.ToLower(call.Name.Name) {
		case "readfile", "load_extension":
			c.sawEscape = true
		}
	}
	return c, n, nil
}

func (c *callCollector) VisitEnd(n sql.Node) (sql.Node, error) { return n, nil }

// TestEscapeHatchesScanAsIdentifiers guards the one assumption the vocabulary
// check rests on.
//
// Keyword tokens followed by `(` are permitted without appearing on the
// allowlist, on the grounds that the parser's keyword set holds no route to the
// filesystem or a shell. That is only safe while the dangerous names are NOT
// keywords. If a parser upgrade promoted readfile to a keyword, the allowlist
// would stop seeing it and this test is what says so.
func TestEscapeHatchesScanAsIdentifiers(t *testing.T) {
	for _, name := range []string{
		"readfile", "writefile", "load_extension", "edit", "fts3_tokenizer",
		"sqlite_compileoption_get", "pragma_table_info",
	} {
		sc := sql.NewScanner(strings.NewReader(name + "(1)"))
		_, tok, lit := sc.Scan()
		switch tok {
		case sql.IDENT, sql.QIDENT, sql.BIDENT:
		default:
			t.Errorf("%s now scans as %s, not an identifier — the vocabulary check "+
				"no longer sees it and permits it as a keyword function", name, tok)
		}
		if !strings.EqualFold(lit, name) {
			t.Errorf("%s scanned as literal %q", name, lit)
		}
	}
}

// TestTheAllowlistHoldsNoEscapeHatch reads the list back rather than trusting
// the literal, so a name added during a merge cannot slip past review.
func TestTheAllowlistHoldsNoEscapeHatch(t *testing.T) {
	banned := map[string]string{
		"readfile":       "reads any file the process can open",
		"writefile":      "writes any file the process can open",
		"load_extension": "loads arbitrary native code",
		"edit":           "runs $EDITOR",
		"fts3_tokenizer": "takes a pointer as a blob",
		"hex":            "moves bytes no statistic needs",
		"quote":          "renders values as literals",
		"randomblob":     "moves bytes no statistic needs",
		"zeroblob":       "moves bytes no statistic needs",
	}
	for _, name := range sqlguard.AllowedFunctions() {
		if why, bad := banned[name]; bad {
			t.Errorf("%s() is on the allowlist: %s", name, why)
		}
		if strings.HasPrefix(name, "sqlite_") || strings.HasPrefix(name, "pragma_") {
			t.Errorf("%s() is on the allowlist and reserved", name)
		}
	}
	if len(sqlguard.AllowedFunctions()) == 0 {
		t.Fatal("the allowlist is empty, so every statement is refused and the " +
			"refusal tests above pass for the wrong reason")
	}
}

func label(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 44 {
		q = q[:44]
	}
	return q
}
