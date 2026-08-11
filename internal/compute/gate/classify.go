package gate

import (
	"fmt"
	"strings"

	"github.com/rqlite/sql"
)

// Classification decides whether a statement produces an aggregate at all, and
// which of its result columns are group keys.
//
// §12.1's first rule is that "a result set that is not an aggregate (no GROUP
// BY/aggregate function …) cannot be passed to an LLM". Deciding that has to be
// done on the statement, not on the result: a query with no GROUP BY over a
// three-row table returns three rows, which would sail under any row cap while
// being exactly the raw data the gate exists to stop.

// shape is what classify worked out about a statement.
type shape struct {
	grouped bool
	// isKey marks result columns that are group keys rather than aggregates.
	isKey []bool
	// countCol is the index of the COUNT(*) column, or -1. The k-anonymity
	// floor is applied to it, so a grouped statement without one is refused.
	countCol int
}

// aggregateFunctions are the calls that collapse rows.
//
// A windowed call is deliberately not one of these regardless of its name:
// COUNT(*) OVER () returns one row per input row, so a statement whose only
// "aggregate" is windowed produces the raw result set with a count stapled to
// each row.
var aggregateFunctions = map[string]bool{
	"count": true, "sum": true, "total": true, "avg": true,
	"min": true, "max": true, "group_concat": true,
}

func classify(query string) (shape, error) {
	p := sql.NewParser(strings.NewReader(query))
	stmts, err := p.ParseStatements()
	if err != nil || len(stmts) != 1 {
		// sqlguard.Check ran first, so this cannot happen from Aggregate. It is
		// still an error rather than a panic: classify is reachable from tests.
		return shape{}, refuse("not parseable", fmt.Sprint(err))
	}
	sel, ok := stmts[0].(*sql.SelectStatement)
	if !ok {
		return shape{}, refuse("not a SELECT", "")
	}
	if sel.Compound != nil {
		// The arms of a UNION are classified independently or not at all, and
		// "not at all" is what an aggregate arm unioned with a raw one gets
		// you. Run them separately.
		return shape{}, refuse("compound SELECT",
			"UNION/INTERSECT/EXCEPT cannot be classified as one aggregate; run each arm separately")
	}

	sh := shape{grouped: len(sel.GroupByExprs) > 0, countCol: -1}
	sh.isKey = make([]bool, len(sel.Columns))

	var aggregates int
	for i, col := range sel.Columns {
		if starred(col) {
			// A star in any spelling. The bare form was the only one checked,
			// and `SELECT t.*, COUNT(*) FROM t GROUP BY 1` therefore passed
			// classification with a two-element isKey while the driver returned
			// one column per expanded column — then panicked with an index out
			// of range, holding the whole result set in memory.
			return shape{}, refuse("not an aggregate",
				"a star expands to rows; §12.1 lets only aggregates cross")
		}
		if isAggregateCall(col.Expr) {
			aggregates++
			if sh.countCol < 0 && isCountStar(col.Expr) {
				sh.countCol = i
			}
			continue
		}
		sh.isKey[i] = true
	}

	if aggregates == 0 {
		return shape{}, refuse("not an aggregate",
			"no GROUP BY aggregate function in the result columns")
	}

	if !sh.grouped {
		// SQLite permits a bare column beside an aggregate with no GROUP BY and
		// answers with a value from an arbitrary row. `SELECT rep_note,
		// COUNT(*) FROM tickets` is therefore one row of real data wearing an
		// aggregate's clothes, and it is the cheapest exfiltration in the
		// dialect.
		for i, key := range sh.isKey {
			if key {
				return shape{}, refuse("bare column beside an aggregate",
					fmt.Sprintf("column %d is not aggregated and there is no GROUP BY, so "+
						"SQLite answers it from an arbitrary row", i+1))
			}
		}
		if sh.countCol < 0 {
			// The same requirement as the grouped case, and for the same reason:
			// without COUNT(*) there is no record count to compare against the
			// k-anonymity floor, so an aggregate over one record is
			// indistinguishable from an aggregate over a million.
			return shape{}, refuse("ungrouped without COUNT(*)",
				"the reporting floor needs a record count; select COUNT(*)")
		}
		return sh, nil
	}

	// Grouped: every non-aggregate column must be a group key. The same
	// permissiveness applies within a group — a bare column not in GROUP BY is
	// answered from an arbitrary row of that group.
	for i, key := range sh.isKey {
		if !key {
			continue
		}
		if !groupedBy(sel, i) {
			return shape{}, refuse("column not in GROUP BY",
				fmt.Sprintf("column %d is neither aggregated nor grouped, so SQLite "+
					"answers it from an arbitrary row of each group", i+1))
		}
	}

	if sh.countCol < 0 {
		// Without COUNT(*) there is no k to compare against the floor. A SUM of
		// five is not five records, so `SELECT email, SUM(spend) … GROUP BY
		// email` would otherwise cross with one bucket per person.
		return shape{}, refuse("grouped without COUNT(*)",
			"the k-anonymity floor needs a record count per bucket; select COUNT(*)")
	}
	return sh, nil
}

// starred reports whether a result column is a star in any of its spellings:
// bare `*`, or qualified `t.*` / `main.t.*`.
func starred(col *sql.ResultColumn) bool {
	if col.Star.IsValid() {
		return true
	}
	if ref, ok := col.Expr.(*sql.QualifiedRef); ok && ref.Star.IsValid() {
		return true
	}
	return false
}

func isAggregateCall(e sql.Expr) bool {
	call, ok := e.(*sql.Call)
	if !ok || call.Name == nil {
		return false
	}
	if call.Over != nil {
		return false // a window function, which returns a row per row
	}
	return aggregateFunctions[strings.ToLower(call.Name.Name)]
}

func isCountStar(e sql.Expr) bool {
	call, ok := e.(*sql.Call)
	if !ok || call.Name == nil || call.Over != nil {
		return false
	}
	return strings.EqualFold(call.Name.Name, "count") && call.Star.IsValid()
}

// groupedBy reports whether result column i is covered by the GROUP BY clause.
//
// Three ways to be covered, because all three appear in ordinary SQL: by
// position (`GROUP BY 1`), by repeating the expression, or by naming the result
// column's alias (`… AS month … GROUP BY month`).
func groupedBy(sel *sql.SelectStatement, i int) bool {
	col := sel.Columns[i]
	want := normalize(col.Expr)

	for _, g := range sel.GroupByExprs {
		if lit, ok := g.(*sql.NumberLit); ok {
			if lit.Value == fmt.Sprint(i+1) {
				return true
			}
			continue
		}
		if normalize(g) == want {
			return true
		}
		if col.Alias != nil {
			if ident, ok := g.(*sql.Ident); ok && strings.EqualFold(ident.Name, col.Alias.Name) {
				return true
			}
		}
	}
	return false
}

// normalize renders an expression for comparison. Whitespace and case vary
// between two spellings of the same expression and nothing else here should.
func normalize(e sql.Expr) string {
	if e == nil {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(e.String()), " "))
}
