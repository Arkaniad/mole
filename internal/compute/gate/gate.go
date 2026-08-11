// Package gate is the aggregation gate: the only way anything derived from a
// connector may reach a model (M8, §12.1).
//
// Rev 1 of the sketch said "data never leaves the local machine; only
// aggregates reach the LLM". §12.1 makes that a mechanism instead of a comment,
// and the mechanism is a type: nothing crosses except an AggregateEnvelope.
//
// # Enforced structurally
//
// This package exposes no function that returns rows. Aggregate reads the
// result set, computes statistics from it, and returns the statistics; the rows
// exist only inside that call and are unreachable from anywhere else. A caller
// cannot ask for them, forget to summarize them, or log them by accident —
// there is no API through which to obtain one.
//
// That is the difference between this and a convention. A `Query` that returned
// rows plus a `Summarize` that turned them into an envelope would enforce
// nothing: the privacy property would hold only while every caller remembered
// to call the second function, and §12.1 exists precisely because "only
// aggregates reach the LLM" was already being remembered rather than enforced.
//
// # What crosses
//
//   - counts, null rates, distinct counts
//   - min, max, mean, standard deviation and quartiles of numeric columns
//   - top-K buckets, each covering at least KFloor records
//
// and what does not:
//
//   - any row
//   - any value from a column that looks like prose or a personal identifier
//   - any bucket small enough to identify the records in it
package gate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/compute/sqlguard"
	"github.com/lajosdeme/mole/internal/compute/stats"
)

// ErrRefused matches every refusal to build an envelope.
var ErrRefused = errors.New("gate: refused")

type refusal struct{ reason, detail string }

func (r *refusal) Error() string {
	if r.detail == "" {
		return "gate: " + r.reason
	}
	return "gate: " + r.reason + ": " + r.detail
}
func (r *refusal) Is(target error) bool { return target == ErrRefused }

func refuse(reason, detail string) error { return &refusal{reason: reason, detail: detail} }

// Defaults. §12.1 names the k-anonymity floor and maxRawRows; the rest are the
// bounds that make an envelope a summary rather than a transcript.
const (
	DefaultKFloor     = 5
	DefaultMaxRawRows = 5000
	DefaultTopK       = 25
	DefaultTimeout    = 30 * time.Second
)

// Options tune the gate. The zero value is the defaults above.
type Options struct {
	// KFloor is the minimum number of records a bucket must cover before its
	// key may cross. §12.1: "Otherwise GROUP BY email trivially exfiltrates
	// rows one aggregate at a time."
	KFloor int64
	// MaxRawRows is the largest result the gate will summarize. A result past
	// it is refused, not truncated — a summary of an arbitrary prefix of an
	// unordered result set describes nothing.
	MaxRawRows int64
	// TopK caps how many buckets an envelope carries.
	TopK int
	// Timeout bounds the query. §12.2's third defense asks for a statement
	// timeout alongside the row cap; for a file-backed engine this is it.
	Timeout time.Duration
	// FreeTextColumns are names the connector profile already flagged. The gate
	// re-derives the flag from the result anyway — a result column can be an
	// expression the profile never saw — and takes the union.
	FreeTextColumns []string
	// Log receives one record per crossing. §12.1: "Every crossing is logged,
	// so a user can audit exactly what left their machine."
	Log *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.KFloor <= 0 {
		o.KFloor = DefaultKFloor
	}
	if o.MaxRawRows <= 0 {
		o.MaxRawRows = DefaultMaxRawRows
	}
	if o.TopK <= 0 {
		o.TopK = DefaultTopK
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return o
}

// -----------------------------------------------------------------------------
// The envelope
// -----------------------------------------------------------------------------

// AggregateEnvelope is what may cross to a model.
//
// Every field is a count, a moment, a bound, or a bucket that covers at least
// KFloor records. There is deliberately no field that can hold a row.
type AggregateEnvelope struct {
	// Query is the statement that produced this, and QueryHash identifies it.
	// A local claim cites "connector:<name>#<hash>", so the hash has to be
	// stable and the query has to be readable — a claim nobody can trace back
	// to a query is not evidence.
	Query     string `json:"query"`
	QueryHash string `json:"query_hash"`

	RowCount int64         `json:"row_count"`
	Columns  []ColumnStats `json:"columns"`
	TopK     []Bucket      `json:"top_k,omitempty"`

	// TestResults are §12.1's statistical tests, and §4's "statistical
	// validity: n, effect size, significance". Empty unless the query supplied
	// the sufficient statistics — see stats.SumColumn.
	//
	// Computed rather than asserted, and computed HERE rather than by whoever
	// reads the envelope: a model handed two means will describe a trend, and
	// the only defense against that is for the significance to arrive as
	// evidence alongside them.
	TestResults []stats.Test `json:"test_results,omitempty"`

	// Suppressed is how many buckets fell below the k-anonymity floor.
	//
	// Only the floor. It used to also count buckets dropped by the top-K cap, so
	// a bucket of eight beyond TopK=2 was reported as "1 bucket(s) covering fewer
	// than 5 records" — and the planner read a presentation limit as a privacy
	// outcome.
	Suppressed int `json:"suppressed,omitempty"`
	// BeyondTopK is how many buckets cleared the floor and were folded away
	// anyway, because the envelope carries at most TopK of them.
	BeyondTopK int `json:"beyond_top_k,omitempty"`
	// Truncated marks that more buckets exist than TopK carries.
	Truncated bool `json:"truncated,omitempty"`
	// Notes record what the gate withheld and why, in words a model can read
	// out. A column excluded for being free text is a fact about the answer.
	Notes []string `json:"notes,omitempty"`
}

// Kind is what a result column turned out to hold.
type Kind string

const (
	KindNumber Kind = "number"
	KindText   Kind = "text"
	KindEmpty  Kind = "empty"
)

// ColumnStats describes one result column.
type ColumnStats struct {
	Name     string `json:"name"`
	Kind     Kind   `json:"kind"`
	Nulls    int64  `json:"nulls"`
	Distinct int64  `json:"distinct"`
	// FreeText marks a column whose values were withheld: Range is nil and the
	// column contributes no buckets.
	FreeText bool `json:"free_text,omitempty"`

	Number *NumberStats `json:"number,omitempty"`
	Range  *TextRange   `json:"range,omitempty"`
}

// NumberStats are the moments and quantiles of a numeric column.
type NumberStats struct {
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
	P25    float64 `json:"p25"`
	P50    float64 `json:"p50"`
	P75    float64 `json:"p75"`
}

// TextRange is the bounds of a non-free-text column. Two values, and only from
// a column whose values are categories rather than contents.
type TextRange struct {
	Min string `json:"min"`
	Max string `json:"max"`
}

// Bucket is one group of at least KFloor records.
type Bucket struct {
	// Key is the group key, one entry per grouping column.
	Key []string `json:"key,omitempty"`
	// Count is COUNT(*) for the group, which is what the floor is applied to.
	Count int64 `json:"count"`
	// Measures are the other aggregates selected for this group.
	Measures map[string]float64 `json:"measures,omitempty"`
	// Other marks the bucket that everything below the floor was folded into.
	// It names no key, which is the point.
	Other bool `json:"other,omitempty"`
}

// -----------------------------------------------------------------------------
// Aggregate
// -----------------------------------------------------------------------------

// Aggregate runs query and returns what may cross.
//
// It refuses rather than truncates. A statement that is not an aggregate, a
// grouped statement with no COUNT(*) to apply the floor to, and a result larger
// than MaxRawRows are all errors — each of them means the caller asked for
// something the gate cannot describe safely, and answering with a partial
// summary would hide that.
func Aggregate(ctx context.Context, db *sql.DB, query string, opts Options) (AggregateEnvelope, error) {
	opts = opts.withDefaults()

	// The parse gate first. §12.2 and §12.1 are separate defenses and this is
	// not a substitute for calling Check earlier — it is the guarantee that no
	// path reaches a database through this package without passing it.
	if err := sqlguard.Check(query); err != nil {
		return AggregateEnvelope{}, err
	}

	shape, err := classify(query)
	if err != nil {
		return AggregateEnvelope{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return AggregateEnvelope{}, fmt.Errorf("gate: run query: %w", err)
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		return AggregateEnvelope{}, err
	}
	if len(names) != len(shape.isKey) {
		// The whole accumulator indexes shape.isKey by result-column position,
		// so this correspondence is load-bearing and was unchecked. A star
		// expands to one result column per table column while the parse tree
		// holds one, and the mismatch surfaced as an index panic rather than a
		// refusal — with the result set already in memory.
		//
		// classify refuses every star spelling now. This stays because the
		// invariant is what the code depends on, not the one construct that was
		// found to break it.
		return AggregateEnvelope{}, refuse("result shape does not match the statement",
			fmt.Sprintf("%d result column(s) for %d in the statement; the statement "+
				"expands to something this cannot describe", len(names), len(shape.isKey)))
	}

	acc := newAccumulator(names, shape, opts)
	var n int64
	for rows.Next() {
		n++
		if n > opts.MaxRawRows {
			return AggregateEnvelope{}, refuse("result too large",
				fmt.Sprintf("more than %d rows; a summary of an arbitrary prefix "+
					"describes nothing, so add a GROUP BY or a filter", opts.MaxRawRows))
		}
		if err := acc.scan(rows); err != nil {
			return AggregateEnvelope{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return AggregateEnvelope{}, fmt.Errorf("gate: read result: %w", err)
	}

	env, described := acc.envelope(query)

	// §14.3's exfil assertion, run before the envelope is returned rather than
	// measured after it has been. Nothing crosses that fails it.
	if err := acc.verifyNoLeak(env, described); err != nil {
		opts.Log.ErrorContext(ctx, "envelope withheld: it carried row-level data",
			"query_hash", env.QueryHash, "err", err)
		return AggregateEnvelope{}, err
	}

	logCrossing(ctx, opts.Log, env)
	return env, nil
}

// Record is what §12.1's audit trail holds about one crossing.
//
// One derivation, two consumers: the log line below and the durable row the actor
// writes. They were going to be two hand-written descriptions of the same event,
// which is the construct that has already cost this project a "crossing is logged"
// claim that logged nothing and a doctor line reporting a property nothing
// enforced.
//
// Carries no value from the data — counts, a hash, and the statement, which is
// safe here because §12.3 forbids the model from authoring one.
type Record struct {
	Query           string
	QueryHash       string
	RowsDescribed   int64
	Columns         int
	ColumnsWithheld int
	Buckets         int
	Suppressed      int
	BeyondTopK      int
	Tests           int
	Truncated       bool
}

// Describe summarizes an envelope for the audit trail.
func Describe(env AggregateEnvelope) Record {
	withheld := 0
	for _, c := range env.Columns {
		if c.FreeText {
			withheld++
		}
	}
	return Record{
		Query:           env.Query,
		QueryHash:       env.QueryHash,
		RowsDescribed:   env.RowCount,
		Columns:         len(env.Columns),
		ColumnsWithheld: withheld,
		Buckets:         len(env.TopK),
		Suppressed:      env.Suppressed,
		BeyondTopK:      env.BeyondTopK,
		Tests:           len(env.TestResults),
		Truncated:       env.Truncated,
	}
}

// logCrossing is §12.1's audit line.
//
// Structured, and carrying no value from the data: the query, its hash, how
// much was described, how much was withheld. Someone auditing what left their
// machine needs to be able to read this without it being another copy of the
// thing they were worried about.
func logCrossing(ctx context.Context, log *slog.Logger, env AggregateEnvelope) {
	r := Describe(env)
	log.InfoContext(ctx, "aggregate crossed the gate",
		"query_hash", r.QueryHash,
		"query", r.Query,
		"rows_described", r.RowsDescribed,
		"columns", r.Columns,
		"buckets", r.Buckets,
		"buckets_suppressed", r.Suppressed,
		"buckets_beyond_top_k", r.BeyondTopK,
		"columns_withheld", r.ColumnsWithheld,
		"tests", r.Tests,
		"truncated", r.Truncated)
}

// HashQuery is the identifier a local claim cites, exported because the audit
// trail has to name a statement that was REFUSED — one that never became an
// envelope and so has no hash of its own to read.
func HashQuery(q string) string { return hashQuery(q) }

func hashQuery(q string) string {
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------
// Accumulation
// -----------------------------------------------------------------------------

// accumulator holds the result set while the envelope is decided.
//
// Two phases, and the split is a privacy control rather than a convenience.
// Which rows may be described cannot be known until every row has been read:
// a bucket is only safe once its count is known, and a count is only known at
// the end. Streaming statistics would therefore have already folded suppressed
// records into the numbers by the time the floor was applied — which is
// exactly the leak the §14.3 property test found:
//
//	SELECT code, COUNT(*) FROM records GROUP BY 1
//
// puts every record in its own bucket, so all twenty were suppressed and no
// bucket crossed — and the column range still carried two individual records'
// codes, because it had been computed while reading.
//
// The rows are bounded by MaxRawRows, live only for the length of Aggregate,
// and are unreachable from outside the package.
type accumulator struct {
	opts  Options
	shape shape
	names []string

	rows [][]cell
	// keyOf is each row's group key, empty for an ungrouped statement.
	keyOf []string
	// freeTextKey marks grouping columns that turned out to hold prose. They
	// contribute no buckets, and the column still has to be REPORTED as
	// withheld — a column that simply went quiet reads as an empty result.
	freeTextKey map[int]bool
}

// cell is one scanned value in both the forms the envelope needs.
type cell struct {
	text  string
	num   float64
	isNum bool
	null  bool
}

func newAccumulator(names []string, sh shape, opts Options) *accumulator {
	return &accumulator{opts: opts, shape: sh, names: names, freeTextKey: map[int]bool{}}
}

func (a *accumulator) scan(rows *sql.Rows) error {
	raw := make([]any, len(a.names))
	ptrs := make([]any, len(a.names))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return fmt.Errorf("gate: scan: %w", err)
	}

	row := make([]cell, len(a.names))
	for i, v := range raw {
		text, num, isNum, isNull := coerce(v)
		row[i] = cell{text: text, num: num, isNum: isNum, null: isNull}
	}
	a.rows = append(a.rows, row)
	a.keyOf = append(a.keyOf, a.groupKey(row))
	return nil
}

// NullLabel is how a NULL group key is reported.
//
// A label and not an empty string, because they are different groups and were
// being merged into one. See groupKey.
const NullLabel = "(none)"

// groupKey encodes a row's grouping columns injectively.
//
// Both properties here are repairs. The key used to be the values joined with
// \x1f, with NULL mapped to "", and both halves of that lost information:
//
//   - NULL and "" produced the same key, so two distinct SQL groups merged into
//     one bucket. With three records each and a floor of five, the merged bucket
//     of six CLEARED the floor — a k-anonymity bypass — and reported one group's
//     mean under a label belonging to neither.
//   - A plain join is not injective. ["x", "y\x1fz"] and ["x\x1fy", "z"] both
//     produce "x\x1fy\x1fz", so two result rows merged and the bucket carried
//     the sum of their counts under one of the two keys.
//
// Length-prefixing each part makes the encoding injective, and a distinct
// marker byte keeps NULL apart from every possible string.
func (a *accumulator) groupKey(row []cell) string {
	var b strings.Builder
	for i := range a.names {
		if i >= len(a.shape.isKey) || !a.shape.isKey[i] {
			continue
		}
		if row[i].null {
			b.WriteString("N;")
			continue
		}
		b.WriteString("V")
		b.WriteString(strconv.Itoa(len(row[i].text)))
		b.WriteString(":")
		b.WriteString(row[i].text)
		b.WriteString(";")
	}
	return b.String()
}

// coerce normalizes a scanned cell. The text form is what feeds distinctness
// and the range; the numeric form is what feeds the moments.
func coerce(raw any) (text string, num float64, isNum, isNull bool) {
	switch v := raw.(type) {
	case nil:
		return "", 0, false, true
	case int64:
		return strconv.FormatInt(v, 10), float64(v), true, false
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), v, true, false
	case bool:
		if v {
			return "true", 1, true, false
		}
		return "false", 0, true, false
	case []byte:
		return string(v), 0, false, false
	case string:
		return v, 0, false, false
	case time.Time:
		return v.UTC().Format(time.RFC3339), 0, false, false
	default:
		return fmt.Sprint(v), 0, false, false
	}
}

// group is one bucket under construction.
type group struct {
	key      []string
	count    int64
	measures map[string]float64
	rows     []int
	// collided marks a bucket that two result rows mapped onto, which an
	// injective key makes impossible. If it is ever set the shape analysis was
	// wrong and the bucket describes neither group.
	collided bool
}

// envelope builds the result, and returns the rows it was allowed to describe
// so the exfil check can be told what was permitted rather than inferring it
// from the answer.
func (a *accumulator) envelope(query string) (AggregateEnvelope, []int) {
	env := AggregateEnvelope{
		Query:     query,
		QueryHash: hashQuery(query),
		RowCount:  int64(len(a.rows)),
	}

	// Which rows the envelope is allowed to describe. For an ungrouped
	// statement that is all of them — there is one row and every value in it is
	// an aggregate over the whole table. For a grouped one it is the rows of
	// the buckets that survived the floor and the top-K cap, and only those.
	described := a.describedRows(&env)

	for i, name := range a.names {
		// The notes come back rather than being appended inside, so a future
		// append to env.Columns in statsFor cannot be silently discarded by the
		// append here.
		col, notes := a.statsFor(i, name, described)
		env.Columns = append(env.Columns, col)
		env.Notes = append(env.Notes, notes...)
	}
	a.addTests(&env)
	return env, described
}

// addTests compares the two largest buckets, when the query supplied enough to.
//
// TWO, not every pair. Comparing every pair of k buckets is k(k−1)/2 tests
// against the same alpha, which manufactures a significant result out of noise
// as soon as there are a handful of groups — and it would do it in the one
// place mole reports statistics as though they settled something. One
// comparison, named, with no multiple-testing correction needed because there
// is nothing to correct for.
func (a *accumulator) addTests(env *AggregateEnvelope) {
	var named []Bucket
	for _, b := range env.TopK {
		if !b.Other {
			named = append(named, b)
		}
	}
	if len(named) < 2 {
		return
	}
	// TopK is already ordered by count.
	first, ok := groupFrom(named[0])
	if !ok {
		return
	}
	second, ok := groupFrom(named[1])
	if !ok {
		return
	}
	measure := a.measureName()
	if t, ok := stats.Welch(measure, first, second); ok {
		env.TestResults = append(env.TestResults, t)
		env.Notes = append(env.Notes, "a two-sample comparison was run on the two "+
			"largest groups only; comparing every pair would invent significance")
	}
}

func groupFrom(b Bucket) (stats.Group, bool) {
	sum, ok := b.Measures[stats.SumColumn]
	if !ok {
		return stats.Group{}, false
	}
	sumSq, ok := b.Measures[stats.SumSqColumn]
	if !ok {
		return stats.Group{}, false
	}
	// n is the count of NON-NULL measure values, never COUNT(*).
	//
	// b.Count is COUNT(*) and Sum comes from SUM(measure), which skips NULL. The
	// two were used together, so a group with half its measure missing reported
	// a mean half its real value — and the test called the resulting difference
	// large and significant. Required rather than defaulted: a query that did
	// not supply it cannot support a test.
	n, ok := b.Measures[stats.CountColumn]
	if !ok || n < 2 {
		return stats.Group{}, false
	}
	name := strings.Join(b.Key, " / ")
	if name == "" {
		return stats.Group{}, false
	}
	return stats.Group{
		Name: name, N: int64(n), Sum: sum, SumSq: sumSq,
		// Absent means the query did not centre its measure, which is safe:
		// Variance refuses rather than reporting a cancelled figure.
		Offset: b.Measures[stats.OffsetColumn],
	}, true
}

// measureName is what the compared column is called, for the sentence. The
// template names its mean column `mean`, so that is what a reader recognises;
// the sums are machinery.
func (a *accumulator) measureName() string {
	for _, n := range a.names {
		if n == "mean" {
			return "the mean"
		}
	}
	return "the measure"
}

// describedRows applies the floor and the cap, fills in the buckets, and
// returns the row indexes the statistics may be computed over.
func (a *accumulator) describedRows(env *AggregateEnvelope) []int {
	if !a.shape.grouped {
		// The floor applies here too, and used to not.
		//
		// `describedRows` returned every row for an ungrouped statement without
		// asking how many RECORDS were aggregated — so the overview template
		// over a one-row table published that row's value five times, as the
		// minimum, the maximum, the mean, the median and the total. RowCount is
		// the number of RESULT rows (one), so nothing downstream noticed.
		if n, ok := a.recordCount(); ok && n < a.opts.KFloor {
			env.Notes = append(env.Notes, fmt.Sprintf(
				"the query aggregated %d record(s), fewer than the reporting floor of "+
					"%d, so its numeric summaries describe individual records and were "+
					"withheld (§12.1)", n, a.opts.KFloor))
			return nil
		}
		return a.allRows()
	}

	var order []string
	byKey := map[string]*group{}
	for i, row := range a.rows {
		k := a.keyOf[i]
		g, ok := byKey[k]
		if !ok {
			g = &group{measures: map[string]float64{}}
			for j := range a.names {
				if a.shape.isKey[j] {
					if row[j].null {
						g.key = append(g.key, NullLabel)
					} else {
						g.key = append(g.key, row[j].text)
					}
				}
			}
			byKey[k] = g
			order = append(order, k)
		}
		if a.shape.countCol >= 0 {
			g.count += int64(row[a.shape.countCol].num)
			if len(g.rows) > 0 {
				// Two result rows sharing a bucket key. SQL already grouped the
				// result, so this cannot happen with an injective encoding — and
				// when the encoding was lossy it was how the floor got bypassed.
				// Recorded rather than trusted.
				g.collided = true
			}
		}
		for j, c := range row {
			if a.shape.isKey[j] || j == a.shape.countCol || !c.isNum {
				continue
			}
			// Keyed on the result column name, so two aggregates sharing an alias
			// silently kept only the last. Refused instead: a duplicate alias
			// means the caller cannot tell which figure it is reading.
			if _, dup := g.measures[a.names[j]]; dup {
				g.collided = true
				continue
			}
			g.measures[a.names[j]] = c.num
		}
		g.rows = append(g.rows, i)
	}

	// A grouping key that holds free text contributes no buckets at all. §12.1:
	// "a 'top values' list over a notes field is just the rows" — and grouping
	// by it is that same list with counts attached.
	if a.keyIsFreeText(env) {
		return nil
	}

	kept := make([]*group, 0, len(order))
	other := Bucket{Other: true}
	for _, k := range order {
		g := byKey[k]
		if g.collided {
			// Cannot happen with an injective key. Folded into `other` rather
			// than reported, because a bucket describing two groups is exactly
			// the shape that bypassed the floor when the key was lossy.
			other.Count += g.count
			env.Suppressed++
			env.Notes = append(env.Notes, "a bucket matched more than one result row "+
				"and was withheld; the grouping could not be resolved")
			continue
		}
		if g.count < a.opts.KFloor {
			other.Count += g.count
			env.Suppressed++
			continue
		}
		kept = append(kept, g)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].count > kept[j].count })
	if len(kept) > a.opts.TopK {
		for _, g := range kept[a.opts.TopK:] {
			other.Count += g.count
			env.BeyondTopK++
		}
		kept = kept[:a.opts.TopK]
		env.Truncated = true
	}

	var described []int
	for _, g := range kept {
		env.TopK = append(env.TopK, Bucket{Key: g.key, Count: g.count, Measures: g.measures})
		described = append(described, g.rows...)
	}
	if other.Count > 0 {
		// `other` names no key, so it discloses nothing about which values were
		// folded into it — that is what makes reporting a count for a group too
		// small to name safe.
		env.TopK = append(env.TopK, other)
		// Two notes, because they are two different facts about the answer: one
		// is a privacy floor and the other a presentation limit, and a reader who
		// cannot tell them apart draws the wrong conclusion from a short list.
		if env.Suppressed > 0 {
			env.Notes = append(env.Notes, fmt.Sprintf(
				"%d bucket(s) covering fewer than %d records were folded into \"other\"; "+
					"their rows are excluded from the statistics too (§12.1)",
				env.Suppressed, a.opts.KFloor))
		}
		if env.BeyondTopK > 0 {
			env.Notes = append(env.Notes, fmt.Sprintf(
				"%d further bucket(s) cleared the floor but were folded into \"other\" "+
					"because at most %d are reported", env.BeyondTopK, a.opts.TopK))
		}
	}
	sort.Ints(described)
	return described
}

// keyIsFreeText decides whether the grouping key holds prose, from the whole
// result rather than from the surviving rows — there are none yet at this
// point, and a key that is prose is prose regardless of how it buckets.
func (a *accumulator) keyIsFreeText(env *AggregateEnvelope) bool {
	all := a.allRows()
	for i, name := range a.names {
		if !a.shape.isKey[i] {
			continue
		}
		p := a.profile(i, all)
		if p.kind == KindText && a.isFreeText(name, p) {
			a.freeTextKey[i] = true
			env.Notes = append(env.Notes, fmt.Sprintf(
				"grouped on %q, which holds free text, so no buckets crossed (§12.1)", name))
			return true
		}
	}
	return false
}

// profile is the raw shape of one column over a set of rows.
type profile struct {
	kind     Kind
	nulls    int64
	distinct int64
	avgLen   float64
	textMin  string
	textMax  string
	seenText bool
	numbers  []float64
	n        int64
}

func (a *accumulator) profile(col int, rowIdx []int) profile {
	var p profile
	seen := map[string]bool{}
	var isNum, isText bool
	var totalLen, textN int64

	for _, r := range rowIdx {
		c := a.rows[r][col]
		if c.null {
			p.nulls++
			continue
		}
		seen[c.text] = true
		if c.isNum {
			isNum = true
			p.numbers = append(p.numbers, c.num)
			continue
		}
		isText = true
		totalLen += int64(utf8.RuneCountInString(c.text))
		textN++
		// seenText, not `p.textMin == ""`: an empty string is a value, so using it
		// as the unset marker reported the wrong minimum for a column containing
		// one — values "" and "zz" gave Min "zz".
		if !p.seenText || c.text < p.textMin {
			p.textMin = c.text
		}
		if !p.seenText || c.text > p.textMax {
			p.textMax = c.text
		}
		p.seenText = true
	}
	p.distinct = int64(len(seen))
	p.n = int64(len(rowIdx))
	if textN > 0 {
		p.avgLen = float64(totalLen) / float64(textN)
	}
	switch {
	case isText:
		p.kind = KindText
	case isNum:
		p.kind = KindNumber
	default:
		p.kind = KindEmpty
	}
	return p
}

// isFreeText applies the connector's rule to a result column.
//
// One rule, shared with the profiler. A result column can be an expression no
// profile ever described — MIN(note) AS lo is a column called lo holding
// somebody's note — so it is re-derived here and unioned with what the
// connector already knew.
func (a *accumulator) isFreeText(name string, p profile) bool {
	for _, n := range a.opts.FreeTextColumns {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return connector.IsFreeText(name, "", connector.TypeText, p.n, p.distinct, p.avgLen)
}

func (a *accumulator) statsFor(i int, name string, described []int) (ColumnStats, []string) {
	// Counts come from the whole result; values come only from the rows the
	// envelope is entitled to describe.
	//
	// The split is the point. A count is an aggregate however few records it
	// covers — "twenty distinct codes" discloses no code. A minimum is a
	// record: over the rows of buckets that fell below the floor, MIN(spend) is
	// one person's spend, and computing it over everything is how the §14.3
	// property test found a value crossing while every bucket was suppressed.
	all := a.allRows()
	overall := a.profile(i, all)

	c := ColumnStats{
		Name:     name,
		Kind:     overall.kind,
		Nulls:    overall.nulls,
		Distinct: overall.distinct,
	}
	if overall.kind == KindText {
		c.FreeText = a.freeTextKey[i] || a.isFreeText(name, overall)
	}

	var notes []string
	shown := a.profile(i, described)
	switch {
	case overall.kind == KindNumber:
		c.Number = summarize(shown.numbers)
	case overall.kind != KindText:
		// Nothing but nulls.
	case c.FreeText:
		notes = append(notes, fmt.Sprintf(
			"column %q holds free text; its values were not read (§12.1)", name))
	case a.shape.grouped && a.shape.isKey[i]:
		// A range is two values out of the data, so it may only be reported for
		// a column whose values each describe at least KFloor records — which,
		// once the floor has been applied, means a grouping key and nothing
		// else.
		if shown.distinct > 0 {
			c.Range = &TextRange{Min: shown.textMin, Max: shown.textMax}
		}
	case overall.distinct > 0:
		// A text-valued MEASURE, and this is not a corner case: MIN(code) over
		// a group is one specific record's code, selected by an ordering rather
		// than summarized. §14.3's property test crossed exactly that before
		// this rule existed. Numbers are different — the extremum of a numeric
		// column is the summary statistic §12.1 asks for by name.
		notes = append(notes, fmt.Sprintf(
			"column %q is a text-valued aggregate, so its bounds would be individual "+
				"records; withheld (§12.1)", name))
	}
	return c, notes
}

// recordCount is how many records an ungrouped aggregate summarized, from its
// COUNT(*) column. Reports false when the statement did not select one, in which
// case the floor cannot be applied and the statement is refused by classify.
func (a *accumulator) recordCount() (int64, bool) {
	if a.shape.countCol < 0 || len(a.rows) != 1 {
		return 0, false
	}
	c := a.rows[0][a.shape.countCol]
	if !c.isNum {
		return 0, false
	}
	return int64(c.num), true
}

func (a *accumulator) allRows() []int {
	all := make([]int, len(a.rows))
	for i := range all {
		all[i] = i
	}
	return all
}

func summarize(xs []float64) *NumberStats {
	if len(xs) == 0 {
		return nil
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)

	var sum float64
	for _, x := range sorted {
		sum += x
	}
	mean := sum / float64(len(sorted))

	var sq float64
	for _, x := range sorted {
		sq += (x - mean) * (x - mean)
	}
	// Population standard deviation. A result set is the whole population of
	// what the query returned, not a sample drawn from it.
	sd := math.Sqrt(sq / float64(len(sorted)))

	return &NumberStats{
		Min: sorted[0], Max: sorted[len(sorted)-1],
		Mean: mean, StdDev: sd,
		P25: quantile(sorted, 0.25),
		P50: quantile(sorted, 0.50),
		P75: quantile(sorted, 0.75),
	}
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
