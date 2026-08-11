package connector

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// Profiling is what makes §12.3 possible. Local leads are "instantiated from
// hypothesis templates over the connector's known schema", so something has to
// know the schema well enough to choose between templates — which columns are
// numeric, which is a time axis, which have few enough distinct values to group
// by — without any row ever being read by the thing doing the choosing.
//
// Everything here runs as aggregates. The profile holds counts, rates and
// bounded scalars, and for a column flagged FreeText it holds no values at all.

// freeTextName matches columns whose contents are prose or an identifier for a
// person. §12.1 excludes them from TopK because "a 'top values' list over a
// notes field is just the rows".
//
// `name` on its own is deliberately absent: it is as often a product or a
// category as it is a person, and the k-anonymity floor already collapses any
// bucket that identifies a single row. Flagging it would cost real analysis for
// protection that is already there.
var freeTextName = regexp.MustCompile(
	`(^|_)(note|notes|comment|comments|description|desc|body|message|msg|summary|` +
		`review|reviews|feedback|content|detail|details|` +
		`email|e_mail|mail|address|addr|phone|mobile|ssn|` +
		`full_name|first_name|last_name|surname|username|user_name)($|_)`)

const (
	// freeTextAvgLen is prose by length alone. Roughly a sentence.
	freeTextAvgLen = 40.0
	// nearUniqueRatio plus a moderate length is the other shape free text
	// takes: short-ish strings that are nonetheless different in every row,
	// which is what a per-row identifier or a one-line note looks like.
	nearUniqueRatio     = 0.8
	nearUniqueAvgLen    = 16.0
	profileScalarMaxLen = 64
)

// IsFreeText decides the flag from the profile and the column's name.
//
// Exported because the aggregation gate applies the same rule to the columns of
// a RESULT set, which no profile describes — a grouping key can be an
// expression, an alias, or a column from a table that was never registered. Two
// implementations of this rule would drift, and the one that drifted would be
// the one deciding whether prose crosses to a model.
func IsFreeText(name, label string, t ColumnType, rows, distinct int64, avgLen float64) bool {
	// The NAME rule first, and for every type. It used to sit behind a
	// `t != TypeText` early return, on the reasoning that "a number or a
	// timestamp has a range, not contents" — which is true about shape and
	// false about disclosure. A review probe registered a CSV with headers
	// `ssn,phone,dob` and the profile sent
	//
	//	ssn: integer, 3 distinct, from "123456789" to "623456789"
	//	dob: timestamp, from "1987-04-02T00:00:00Z" to "1994-05-15T00:00:00Z"
	//
	// to a hosted API. A social security number is a personal identifier
	// whether it is stored as text or as an integer, and the type it happens to
	// parse as is the least relevant fact about it.
	if freeTextName.MatchString(name) || (label != "" && freeTextName.MatchString(sanitize(label))) {
		return true
	}
	if t != TypeText {
		// The SHAPE rules below are about prose, which only text can be.
		return false
	}
	if avgLen >= freeTextAvgLen {
		return true
	}
	if rows > 0 && avgLen >= nearUniqueAvgLen &&
		float64(distinct) >= nearUniqueRatio*float64(rows) {
		return true
	}
	return false
}

// profileTable fills in the per-column statistics for one table.
func profileTable(ctx context.Context, db *sql.DB, t *Table) error {
	qt, err := quoteIdent(t.Name)
	if err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qt).Scan(&t.Rows); err != nil {
		return fmt.Errorf("connector: count %s: %w", t.Name, err)
	}

	for i := range t.Columns {
		if err := profileColumn(ctx, db, qt, &t.Columns[i], t.Rows); err != nil {
			return err
		}
	}
	return nil
}

// profileColumn runs the statistics for one column in two steps.
//
// The split is the privacy control, not a style choice. Step one asks only for
// counts and an average length — nothing that can carry a value. FreeText is
// decided from that. Only then, and only if the column is not free text, does
// step two ask for MIN and MAX. A single combined query would pull the longest
// note in the table into this process before anything had decided whether it
// was allowed to.
func profileColumn(ctx context.Context, db *sql.DB, qt string, c *Column, rows int64) error {
	qc, err := quoteIdent(c.Name)
	if err != nil {
		return err
	}

	var nulls, distinct sql.NullInt64
	var avgLen sql.NullFloat64
	err = db.QueryRowContext(ctx,
		"SELECT SUM(CASE WHEN "+qc+" IS NULL THEN 1 ELSE 0 END), "+
			"COUNT(DISTINCT "+qc+"), "+
			"AVG(LENGTH("+qc+")) FROM "+qt).
		Scan(&nulls, &distinct, &avgLen)
	if err != nil {
		return fmt.Errorf("connector: profile %s: %w", c.Name, err)
	}
	c.Nulls = nulls.Int64
	c.Distinct = distinct.Int64
	c.AvgLen = avgLen.Float64
	c.FreeText = IsFreeText(c.Name, c.Label, c.Type, rows, c.Distinct, c.AvgLen)

	if c.FreeText || !rangeIsSafe(rows, c.Distinct) {
		return nil
	}

	var min, max sql.NullString
	err = db.QueryRowContext(ctx, "SELECT MIN("+qc+"), MAX("+qc+") FROM "+qt).Scan(&min, &max)
	if err != nil {
		return fmt.Errorf("connector: range %s: %w", c.Name, err)
	}
	c.Min = truncateScalar(min.String)
	c.Max = truncateScalar(max.String)
	return nil
}

// RangeFloor is the number of records a value must describe before the profile
// will record it.
//
// The same number as the aggregation gate's k-anonymity floor, and for the same
// reason — but this is a SECOND place it has to be applied, which was the whole
// finding. §12.1's floor guards the envelope; the profile is a different path to
// a model (it is rendered into the planning prompt on every local run, before
// any query exists) and nothing on it consulted a floor at all.
const RangeFloor = 5

// rangeIsSafe reports whether MIN and MAX describe values rather than records.
//
// A minimum is one specific record's value. It is safe to report only when
// every distinct value in the column covers enough records that naming the
// extremes identifies nobody: with 5 regions over 10,000 rows each value covers
// 2,000 records, and with 3 names over 3 rows each covers one.
//
// The old rule was "report it unless the column is free text", which published
// a real person's name, their employer's smallest salary, and the earliest date
// of birth in the table.
func rangeIsSafe(rows, distinct int64) bool {
	if rows < RangeFloor || distinct <= 0 {
		return false
	}
	// Each distinct value covers rows/distinct records on average.
	return rows/distinct >= RangeFloor
}

func truncateScalar(s string) string {
	if len(s) <= profileScalarMaxLen {
		return s
	}
	return s[:profileScalarMaxLen] + "…"
}

// -----------------------------------------------------------------------------
// Existing databases
// -----------------------------------------------------------------------------

// profileDatabase reads the schema of a database mole did not create, over a
// read-only handle. Views are included: a user who has already shaped their
// data into a view has done the analyst's first job, and reading it costs
// nothing extra. Internal `sqlite_%` tables are not.
func profileDatabase(ctx context.Context, c Connector) ([]Table, error) {
	db, err := c.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		  WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
		  ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("connector: read schema: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: %s has no tables", ErrNoData, c.DBPath)
	}

	var tables []Table
	var skipped []string
	for _, name := range names {
		// A table whose name is not a safe identifier is skipped rather than
		// quoted around, because every query built later interpolates the name
		// and the safety of that rests on this check.
		if !safeIdent(name) {
			skipped = append(skipped, name)
			continue
		}
		cols, err := describeTable(ctx, db, name)
		if err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			continue
		}
		t := Table{Name: name, Columns: cols}
		if err := profileTable(ctx, db, &t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	if len(tables) == 0 {
		return nil, fmt.Errorf("%w: no table in %s has a usable name (skipped: %s)",
			ErrNoData, c.DBPath, strings.Join(skipped, ", "))
	}
	return tables, nil
}

func describeTable(ctx context.Context, db *sql.DB, table string) ([]Column, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT name, type FROM pragma_table_info("+sqlString(table)+")")
	if err != nil {
		return nil, fmt.Errorf("connector: describe %s: %w", qt, err)
	}
	defer rows.Close()

	var cols []Column
	for rows.Next() {
		var name, declared string
		if err := rows.Scan(&name, &declared); err != nil {
			return nil, err
		}
		if !safeIdent(name) {
			continue
		}
		cols = append(cols, Column{Name: name, Type: affinity(declared)})
	}
	return cols, rows.Err()
}

// sqlString quotes a literal for the one place a name is passed as a value
// rather than an identifier. pragma_table_info takes its argument as a string.
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// affinity maps a declared SQLite type onto the inference vocabulary, following
// SQLite's own affinity rules where they apply and the declared name where they
// do not — SQLite has no date affinity, but a column declared DATETIME was
// meant as one.
func affinity(declared string) ColumnType {
	d := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case d == "":
		return TypeText
	case strings.Contains(d, "DATE"), strings.Contains(d, "TIME"):
		return TypeTime
	case strings.Contains(d, "BOOL"):
		return TypeBool
	case strings.Contains(d, "INT"):
		return TypeInteger
	case strings.Contains(d, "REAL"), strings.Contains(d, "FLOA"), strings.Contains(d, "DOUB"),
		strings.Contains(d, "NUM"), strings.Contains(d, "DEC"):
		return TypeReal
	default:
		return TypeText
	}
}
