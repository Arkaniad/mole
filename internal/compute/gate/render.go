package gate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/compute/stats"
)

// Text renders the envelope as the passage a model reads.
//
// This is the whole document a local claim can be mined from, which is what
// makes §11.5 apply unchanged: a claim about local data still has to quote its
// source verbatim, and its source is these numbers. A model that writes "sales
// fell 12% in March" without those words appearing here is inventing, and the
// quote check drops the claim exactly as it would for a web page.
//
// So the rendering is written to be quotable. Every figure appears once, on its
// own line, in a form a sentence can lift — "north — 8 records" rather than a
// table a model would have to reassemble before citing.
//
// It carries only what the envelope carries. There is no path from here back to
// the rows: the envelope has already been through the gate, and this reads
// nothing else.
func (e AggregateEnvelope) Text() string {
	var b strings.Builder

	b.WriteString("Result of one query against local data.\n")
	fmt.Fprintf(&b, "Query: %s\n", e.Query)
	fmt.Fprintf(&b, "Rows in the result: %d\n", e.RowCount)

	if len(e.Columns) > 0 {
		b.WriteString("\nColumns:\n")
		for _, c := range e.Columns {
			b.WriteString("  " + columnLine(c) + "\n")
		}
	}

	if len(e.TopK) > 0 {
		b.WriteString("\nGroups, by number of records:\n")
		for _, bucket := range e.TopK {
			b.WriteString("  " + bucketLine(bucket) + "\n")
		}
	}

	if len(e.TestResults) > 0 {
		// Before the notes and after the groups, because this is the sentence a
		// claim about a difference has to quote. A model that reads two means
		// and stops has already written the wrong claim.
		b.WriteString("\nStatistical tests:\n")
		for _, t := range e.TestResults {
			b.WriteString("  " + t.Summary + "\n")
		}
	}

	if len(e.Notes) > 0 {
		// The notes are part of the evidence, not a footer. "grouped on a
		// free-text column, so no buckets crossed" is the difference between a
		// question that had no answer and one whose answer was withheld, and a
		// model summarizing without it would report the second as the first.
		b.WriteString("\nWhat was withheld or folded:\n")
		for _, n := range e.Notes {
			b.WriteString("  - " + n + "\n")
		}
	}
	return b.String()
}

func columnLine(c ColumnStats) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%s (%s)", c.Name, c.Kind))

	if c.Number != nil {
		n := c.Number
		parts = append(parts,
			"lowest "+num(n.Min),
			"highest "+num(n.Max),
			"mean "+num(n.Mean),
			"median "+num(n.P50),
			"standard deviation "+num(n.StdDev),
			fmt.Sprintf("quartiles %s and %s", num(n.P25), num(n.P75)))
	}
	if c.Range != nil {
		parts = append(parts, fmt.Sprintf("values from %q to %q", c.Range.Min, c.Range.Max))
	}
	if c.FreeText {
		parts = append(parts, "free text, values withheld")
	}
	parts = append(parts,
		fmt.Sprintf("%d distinct", c.Distinct),
		fmt.Sprintf("%d null", c.Nulls))
	return strings.Join(parts, ", ")
}

func bucketLine(b Bucket) string {
	if b.Other {
		return fmt.Sprintf("other — %d records, in groups too small to name", b.Count)
	}
	line := fmt.Sprintf("%s — %d records", strings.Join(b.Key, " / "), b.Count)
	if len(b.Measures) == 0 {
		return line
	}
	for _, name := range sortedKeys(b.Measures) {
		line += fmt.Sprintf(", %s %s", name, num(b.Measures[name]))
	}
	return line
}

// num is stats.Num.
//
// It used to be its own formatter at two decimal places, which destroyed the
// figures it rendered: a rate column of 0.0001 to 0.003 reached the model as
// "lowest 0.00, highest 0.00, mean 0.00", and §11.5 then permitted only "0.00"
// as a citation. Three formatters at two precisions, one of which was wrong; now
// one.
func num(f float64) string { return stats.Num(f) }

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Deterministic, because the rendering feeds a model call that a cassette
	// keys on the request body (§14.1). Map order would make replay a coin toss.
	sort.Strings(out)
	return out
}
