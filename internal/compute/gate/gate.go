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

	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/compute/sqlguard"
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

	// Suppressed is how many buckets fell below the floor and were folded into
	// the `other` bucket. Reported rather than silently dropped: a distribution
	// missing its tail reads as a complete one otherwise.
	Suppressed int `json:"suppressed,omitempty"`
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

	env := acc.envelope(query)
	logCrossing(ctx, opts.Log, env)
	return env, nil
}

// logCrossing is §12.1's audit line.
//
// Structured, and carrying no value from the data: the query, its hash, how
// much was described, how much was withheld. Someone auditing what left their
// machine needs to be able to read this without it being another copy of the
// thing they were worried about.
func logCrossing(ctx context.Context, log *slog.Logger, env AggregateEnvelope) {
	withheld := 0
	for _, c := range env.Columns {
		if c.FreeText {
			withheld++
		}
	}
	log.InfoContext(ctx, "aggregate crossed the gate",
		"query_hash", env.QueryHash,
		"query", env.Query,
		"rows_described", env.RowCount,
		"columns", len(env.Columns),
		"buckets", len(env.TopK),
		"buckets_suppressed", env.Suppressed,
		"columns_withheld", withheld,
		"truncated", env.Truncated)
}

func hashQuery(q string) string {
	sum := sha256.Sum256([]byte(q))
	return hex.EncodeToString(sum[:])
}

// -----------------------------------------------------------------------------
// Accumulation
// -----------------------------------------------------------------------------

// accumulator folds rows into statistics as they are scanned.
//
// It keeps numeric values so quantiles can be computed, and distinct keys so
// cardinality is exact — both bounded by MaxRawRows, which is why that cap is a
// refusal and not a truncation. It never keeps a row.
type accumulator struct {
	opts  Options
	shape shape
	names []string

	nulls    []int64
	distinct []map[string]bool
	// numbers holds numeric values per column for the quantile pass. A column
	// that turns out to be text stops collecting.
	numbers  []([]float64)
	isNumber []bool
	isText   []bool
	textMin  []string
	textMax  []string
	totalLen []int64
	textN    []int64

	// groups is the bucket table, keyed by the joined group key.
	groups   map[string]*Bucket
	groupSeq []string
	rows     int64
}

func newAccumulator(names []string, sh shape, opts Options) *accumulator {
	n := len(names)
	a := &accumulator{
		opts: opts, shape: sh, names: names,
		nulls:    make([]int64, n),
		distinct: make([]map[string]bool, n),
		numbers:  make([][]float64, n),
		isNumber: make([]bool, n),
		isText:   make([]bool, n),
		textMin:  make([]string, n),
		textMax:  make([]string, n),
		totalLen: make([]int64, n),
		textN:    make([]int64, n),
		groups:   map[string]*Bucket{},
	}
	for i := range a.distinct {
		a.distinct[i] = map[string]bool{}
	}
	return a
}

func (a *accumulator) scan(rows *sql.Rows) error {
	cells := make([]any, len(a.names))
	ptrs := make([]any, len(a.names))
	for i := range cells {
		ptrs[i] = &cells[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return fmt.Errorf("gate: scan: %w", err)
	}
	a.rows++

	var key []string
	var count int64
	measures := map[string]float64{}

	for i, raw := range cells {
		text, num, isNum, isNull := coerce(raw)
		if isNull {
			a.nulls[i]++
			continue
		}
		a.distinct[i][text] = true

		if isNum {
			a.isNumber[i] = true
			a.numbers[i] = append(a.numbers[i], num)
		} else {
			a.isText[i] = true
			a.totalLen[i] += int64(len(text))
			a.textN[i]++
			if a.textMin[i] == "" || text < a.textMin[i] {
				a.textMin[i] = text
			}
			if text > a.textMax[i] {
				a.textMax[i] = text
			}
		}

		switch {
		case a.shape.isKey[i]:
			key = append(key, text)
		case i == a.shape.countCol:
			count = int64(num)
		case isNum:
			measures[a.names[i]] = num
		}
	}

	if a.shape.grouped {
		k := strings.Join(key, "\x1f")
		if b, ok := a.groups[k]; ok {
			b.Count += count
		} else {
			a.groups[k] = &Bucket{Key: key, Count: count, Measures: measures}
			a.groupSeq = append(a.groupSeq, k)
		}
	}
	return nil
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

func (a *accumulator) envelope(query string) AggregateEnvelope {
	env := AggregateEnvelope{
		Query:     query,
		QueryHash: hashQuery(query),
		RowCount:  a.rows,
	}

	freeTextByName := map[string]bool{}
	for _, n := range a.opts.FreeTextColumns {
		freeTextByName[strings.ToLower(n)] = true
	}

	for i, name := range a.names {
		c := ColumnStats{
			Name:     name,
			Nulls:    a.nulls[i],
			Distinct: int64(len(a.distinct[i])),
		}
		switch {
		case a.isNumber[i] && !a.isText[i]:
			c.Kind = KindNumber
		case a.isText[i]:
			c.Kind = KindText
		default:
			c.Kind = KindEmpty
		}

		if c.Kind == KindText {
			var avgLen float64
			if a.textN[i] > 0 {
				avgLen = float64(a.totalLen[i]) / float64(a.textN[i])
			}
			// One rule, shared with the connector's profiler. A result column
			// can be an expression no profile ever described, so the flag is
			// re-derived here and unioned with what the profile already knew.
			c.FreeText = freeTextByName[strings.ToLower(name)] ||
				connector.IsFreeText(name, "", connector.TypeText,
					a.rows, c.Distinct, avgLen)
			if !c.FreeText {
				c.Range = &TextRange{Min: a.textMin[i], Max: a.textMax[i]}
			} else {
				env.Notes = append(env.Notes, fmt.Sprintf(
					"column %q holds free text; its values were not read (§12.1)", name))
			}
		}
		if c.Kind == KindNumber {
			c.Number = summarize(a.numbers[i])
		}
		env.Columns = append(env.Columns, c)
	}

	if a.shape.grouped {
		a.buildBuckets(&env)
	}
	return env
}

// buildBuckets applies the k-anonymity floor and the top-K cap.
func (a *accumulator) buildBuckets(env *AggregateEnvelope) {
	// A grouping key that is free text contributes no buckets at all. §12.1:
	// "a 'top values' list over a notes field is just the rows" — and grouping
	// by it is the same list with counts attached.
	for i := range a.names {
		if a.shape.isKey[i] {
			for _, c := range env.Columns {
				if c.Name == a.names[i] && c.FreeText {
					env.Notes = append(env.Notes, fmt.Sprintf(
						"grouped on %q, which holds free text, so no buckets crossed", c.Name))
					return
				}
			}
		}
	}

	var kept []Bucket
	other := Bucket{Other: true}
	for _, k := range a.groupSeq {
		b := a.groups[k]
		if b.Count < a.opts.KFloor {
			other.Count += b.Count
			env.Suppressed++
			continue
		}
		kept = append(kept, *b)
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Count > kept[j].Count })
	if len(kept) > a.opts.TopK {
		for _, b := range kept[a.opts.TopK:] {
			other.Count += b.Count
			env.Suppressed++
		}
		kept = kept[:a.opts.TopK]
		env.Truncated = true
	}

	env.TopK = kept
	if other.Count > 0 {
		// `other` names no key, so it discloses nothing about which values were
		// folded into it — that is what makes it safe to report a count for a
		// group that would otherwise be too small to name.
		env.TopK = append(env.TopK, other)
		env.Notes = append(env.Notes, fmt.Sprintf(
			"%d bucket(s) covering fewer than %d records were folded into \"other\" (§12.1)",
			env.Suppressed, a.opts.KFloor))
	}
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
