// Package sqlguard decides whether a statement may run against a connector
// (M8, §12.2).
//
// §12.2 lists four defenses and says all four are required. This is the second:
// "parse the statement; permit a single SELECT or WITH … SELECT; reject DDL,
// DML, multiple statements, COPY, and any dialect escape hatch to the
// filesystem or shell."
//
// It is a gate, not a sanitizer. Nothing here rewrites a statement into a safe
// one — a rejected statement is rejected, and the caller's job is to not build
// that statement in the first place. §12.3 already says the model cannot author
// SQL: templates render it. So every rejection here is a bug in mole, not a
// blocked attack, and the error text is written for whoever has to find it.
//
// # Two checks, deliberately independent
//
// The statement SHAPE is checked against the parse tree. The function
// VOCABULARY is checked against the token stream, and that split is not a
// stylistic choice — it is forced.
//
// github.com/rqlite/sql's Walk does not descend into CTE bodies or into
// subquery expressions. Measured, and pinned by TestWalkIsNotAnExhaustive
// Traversal: for
//
//	WITH m AS (SELECT readfile('/etc/passwd') FROM s) SELECT COUNT(*) FROM m
//
// a Walk sees COUNT and the reference to m, and never sees readfile at all. An
// AST-based function check would therefore have a hole in exactly the place an
// attacker would put the call. The token stream has no such gaps: the scanner
// emits every token in the statement, so a rule expressed over tokens is
// complete by construction even though it knows nothing about grammar.
package sqlguard

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rqlite/sql"
)

// ErrRejected matches every refusal from Check, so a caller can tell "this
// statement is not allowed" from "the guard itself failed".
var ErrRejected = errors.New("sqlguard: rejected")

// rejection says what was refused and why.
//
// Unexported, matching gate.refusal, which is the identical construct. It was
// exported with Reason documented as "stable enough to switch on" and nothing
// ever switched on it — every caller uses errors.Is(err, ErrRejected). Two copies
// of one error wrapper in a single milestone differing only in visibility is a
// coin flip for whoever writes the third.
type rejection struct {
	reason string
	detail string
}

func (r *rejection) Error() string {
	if r.detail == "" {
		return "sqlguard: " + r.reason
	}
	return "sqlguard: " + r.reason + ": " + r.detail
}

func (r *rejection) Is(target error) bool { return target == ErrRejected }

func reject(reason, detail string) error { return &rejection{reason: reason, detail: detail} }

// MaxLength caps statement size. A template renders a bounded query; anything
// approaching this is not one of ours.
const MaxLength = 8192

// Check reports whether query may be executed against a connector.
//
// A nil return means: one statement, it is a SELECT, and every function it
// names is on the allowlist. It does NOT mean the query is cheap, bounded, or
// that its result may be shown to a model — the row cap and the aggregation
// gate (§12.1) are separate and neither is here.
func Check(query string) error {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return reject("empty statement", "")
	}
	if len(trimmed) > MaxLength {
		return reject("statement too long",
			fmt.Sprintf("%d bytes, limit %d", len(trimmed), MaxLength))
	}

	// Shape first. The vocabulary check reads `name (` as a call, which is only
	// true inside a SELECT: run it first and `CREATE TABLE x (a TEXT)` is
	// refused for naming an unknown function x(), which sends whoever has to
	// fix the template looking in the wrong place entirely.
	if err := checkShape(trimmed); err != nil {
		return err
	}
	return checkVocabulary(trimmed)
}

// -----------------------------------------------------------------------------
// Shape
// -----------------------------------------------------------------------------

// checkShape permits exactly one statement, and only a SELECT.
//
// ParseStatements, not ParseStatement: the single-statement parser returns the
// first statement and no error for `SELECT 1; DROP TABLE sales`, so a guard
// built on it would pass the stacked form and the caller would then hand the
// whole string to the driver. Measured, and pinned by a test.
//
// The type check is an allowlist of one. Rejecting a list of bad statement
// types would mean a statement type added to the parser in a later version is
// permitted by default, and a security gate that fails open on an upgrade is
// not a gate.
func checkShape(query string) error {
	p := sql.NewParser(strings.NewReader(query))
	stmts, err := p.ParseStatements()
	if err != nil {
		// ATTACH and VACUUM land here: they are keywords the parser knows but
		// has no statement type for, so they fail to parse rather than parsing
		// into something to reject. Either way they do not run.
		return reject("not parseable as a single SELECT", err.Error())
	}
	switch len(stmts) {
	case 0:
		return reject("no statement", "")
	case 1:
	default:
		return reject("multiple statements",
			fmt.Sprintf("%d statements; exactly one SELECT is allowed", len(stmts)))
	}

	sel, ok := stmts[0].(*sql.SelectStatement)
	if !ok {
		return reject("not a SELECT", statementKind(stmts[0]))
	}
	// A CTE is permitted and needs no separate handling: `WITH … SELECT` parses
	// to a SelectStatement, and `WITH … DELETE` parses to a DeleteStatement and
	// was already refused above. The CTE is not a way around the type check.
	_ = sel
	return nil
}

// statementKind renders a parser type as something a reader recognises.
func statementKind(stmt sql.Statement) string {
	name := fmt.Sprintf("%T", stmt)
	name = strings.TrimPrefix(name, "*sql.")
	name = strings.TrimSuffix(name, "Statement")

	var b strings.Builder
	for i, r := range name {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

// -----------------------------------------------------------------------------
// Vocabulary
// -----------------------------------------------------------------------------

// checkVocabulary walks the token stream and refuses anything it does not
// recognise as a function.
//
// The rule is: an identifier immediately followed by `(` is a call, and must be
// on the allowlist. Token adjacency rather than source text, so `COUNT (*)`
// with a space is still one call and a comment cannot separate a name from its
// parenthesis.
//
// Keyword tokens followed by `(` — CAST, REPLACE, LEFT, GLOB, MATCH — are
// permitted without being listed. The keyword set is fixed by the parser and
// contains no route to the filesystem or a shell; every escape hatch that
// matters (load_extension, readfile, writefile, edit, fts3_tokenizer) scans as
// an ordinary identifier, which TestEscapeHatchesScanAsIdentifiers pins so that
// a parser upgrade cannot quietly move one of them out of this rule's reach.
func checkVocabulary(query string) error {
	sc := sql.NewScanner(strings.NewReader(query))

	var prevTok sql.Token
	var prevLit string
	for {
		_, tok, lit := sc.Scan()
		if tok == sql.EOF {
			return nil
		}
		if tok == sql.ILLEGAL {
			// Unreachable today: the shape check runs first and a statement
			// carrying an illegal token does not parse, so it is refused there
			// with a position. Kept anyway — it costs one comparison, and the
			// day the parser learns to tolerate a token it cannot name is the
			// day this branch is the only thing between that token and the
			// database.
			return reject("illegal token", quoteForError(lit))
		}

		if tok == sql.COMMENT {
			// Not recorded as the previous token, and this is not cosmetic: the
			// scanner emits a comment as a token of its own, so
			//
			//	SELECT readfile/* nothing to see */('/etc/passwd')
			//
			// puts a COMMENT between the name and its parenthesis. A check that
			// tracked the raw previous token would see COMMENT before `(`,
			// decide this was not a call, and permit it. It did, until a test
			// said otherwise.
			continue
		}

		if tok == sql.LP && isIdentifier(prevTok) {
			name := strings.ToLower(prevLit)
			// Reserved-prefix tables and their table-valued forms:
			// sqlite_master, sqlite_stat1, pragma_table_info and friends. The
			// schema mole is willing to expose is the connector profile, not
			// whatever SQLite will recite about itself.
			switch {
			case strings.HasPrefix(name, "sqlite_"), strings.HasPrefix(name, "pragma_"):
				return reject("reserved function", name+"()")
			case !allowedFunctions[name]:
				return reject("function not on the allowlist", name+"()")
			}
		}
		if isIdentifier(tok) {
			// Not only as a call: `FROM sqlite_master` is a plain table
			// reference, and so is `FROM pragma_table_list` — the table-valued
			// pragma functions can be spelled without parentheses, which the
			// call rule above therefore missed. SQLite reserves both prefixes,
			// so no user table can collide and the rule costs nothing.
			if name := strings.ToLower(lit); strings.HasPrefix(name, "sqlite_") ||
				strings.HasPrefix(name, "pragma_") {
				return reject("reserved name", name)
			}
		}

		prevTok, prevLit = tok, lit
	}
}

func isIdentifier(tok sql.Token) bool {
	return tok == sql.IDENT || tok == sql.QIDENT || tok == sql.BIDENT
}

func quoteForError(lit string) string {
	if lit == "" {
		return "(unprintable)"
	}
	if len(lit) > 32 {
		lit = lit[:32] + "…"
	}
	return `"` + lit + `"`
}

// allowedFunctions is the vocabulary §12.3's templates may render.
//
// An allowlist, and a short one on purpose. Denying the escape hatches by name
// would mean every function SQLite adds — and every extension a future build
// links in — is permitted until someone remembers to deny it. Adding a name
// here when a template needs it is one reviewable line; that asymmetry is the
// whole point.
//
// Deliberately absent, and worth saying why rather than leaving to inference:
//
//	load_extension, readfile, writefile, edit, fts3_tokenizer
//	    the escape hatches §12.2 names. readfile() alone turns a SELECT into
//	    arbitrary file disclosure.
//	hex, quote, randomblob, zeroblob
//	    ways to move bytes that no statistic needs.
//	json_*
//	    no template produces JSON; if one does, add what it uses.
//
// aggregateFunctions is the subset that collapses rows. The aggregation gate
// derives its own set from Aggregates() rather than restating these.
var aggregateFunctions = []string{
	"count", "sum", "total", "avg", "min", "max", "group_concat",
}

// Aggregates returns the allowlisted functions that collapse rows.
func Aggregates() []string {
	out := make([]string, len(aggregateFunctions))
	copy(out, aggregateFunctions)
	return out
}

var allowedFunctions = newSet(append(aggregateFunctions,

	// Window functions, for trend and seasonality templates.
	"row_number", "rank", "dense_rank", "percent_rank", "cume_dist", "ntile",
	"lag", "lead", "first_value", "last_value", "nth_value",

	// Numbers.
	"abs", "round", "sign", "mod", "sqrt", "pow", "power", "exp",
	"ln", "log", "log2", "log10", "floor", "ceil", "ceiling",

	// Nulls and conditionals — a null rate is a statistic and needs these.
	"coalesce", "ifnull", "nullif", "iif", "typeof",

	// Text, for grouping keys. Not for reading contents: the aggregation gate
	// decides what may cross, and no amount of substr() here changes that.
	"length", "lower", "upper", "substr", "substring",
	"trim", "ltrim", "rtrim", "instr", "char", "unicode", "printf", "format",

	// Time, for bucketing a time axis.
	"date", "time", "datetime", "julianday", "unixepoch", "strftime", "timediff",
)...)

func newSet(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// AllowedFunctions returns the vocabulary, sorted. For `doctor` and for tests
// that assert what is reachable rather than trusting the literal above.
func AllowedFunctions() []string {
	out := make([]string, 0, len(allowedFunctions))
	for n := range allowedFunctions {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
