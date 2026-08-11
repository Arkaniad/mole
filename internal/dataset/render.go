package dataset

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Rendering, in two formats that are deliberately not equivalent.
//
// JSON is the complete form: every cell, every alternative a source disagreed
// about, every source and every quote. CSV is one value per cell because that is
// what a CSV is, so it is LOSSY — and rather than pretend otherwise it carries
// the columns that say so, and the doc comment says which format to use when the
// answer matters.
//
// A format that quietly dropped the disagreements would be the most convenient
// output and the least honest one.

// WriteCSV renders the dataset as CSV.
//
// Lossy on purpose, and it says so in three ways: a `sources` count, a
// `contested` column naming the fields the sources disagreed about, and a first
// line of comments... no. Comments are not CSV. The columns are the mechanism,
// and the JSON form is the one to use when a disagreement matters.
func (d Dataset) WriteCSV(w io.Writer, provenance bool) error {
	cw := csv.NewWriter(w)

	header := append([]string{}, d.Schema.Names()...)
	header = append(header, "sources", "contested")
	if provenance {
		header = append(header, "source_urls", "quote")
	}
	if err := cw.Write(header); err != nil {
		return err
	}

	for _, row := range d.Rows {
		rec := make([]string, 0, len(header))
		for _, name := range d.Schema.Names() {
			rec = append(rec, cleanCell(row.Get(name)))
		}
		rec = append(rec,
			fmt.Sprint(len(row.Sources)),
			strings.Join(row.ContestedFields(), " "))
		if provenance {
			rec = append(rec,
				strings.Join(row.Sources, " "),
				cleanCell(firstQuote(row)))
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// cleanCell removes what a spreadsheet would misread.
//
// A newline inside a quoted CSV field is legal and survives a correct reader, and
// is misread by enough of them that a dataset opened in the wrong tool looks
// corrupted. A tab likewise. Neither carries meaning in an extracted value.
func cleanCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func firstQuote(m Merged) string {
	for _, src := range m.Sources {
		if q := m.Quotes[src]; q != "" {
			return q
		}
	}
	return ""
}

// WriteJSON renders the complete form.
func (d Dataset) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// Summary is the human-readable account of what came out, for the CLI and for
// the session's stored report.
//
// It leads with the numbers that qualify the dataset rather than the size of it.
// "40 rows" invites more confidence than "40 rows, 31 from a single source, 6
// with sources that disagree" — and the second is the same result described
// honestly.
func (d Dataset) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d row(s) from %d extraction(s)", len(d.Rows), d.Extracted)
	if c := d.Corroborated(); c > 0 {
		fmt.Fprintf(&b, "; %d corroborated by more than one source", c)
	}
	if single := len(d.Rows) - d.Corroborated(); single > 0 {
		fmt.Fprintf(&b, "; %d from a single source", single)
	}
	if c := d.Contested(); c > 0 {
		fmt.Fprintf(&b, "; %d with sources that disagree", c)
	}
	b.WriteString(".")

	if fields := d.contestedFieldCounts(); len(fields) > 0 {
		b.WriteString("\n\nFields the sources disagree about:\n")
		for _, fc := range fields {
			fmt.Fprintf(&b, "  %s — %d row(s)\n", fc.name, fc.n)
		}
		b.WriteString("Every disagreeing value is kept; the JSON output carries all of them.\n")
	}
	for _, n := range d.Notes {
		b.WriteString("\n- " + n)
	}
	return b.String()
}

type fieldCount struct {
	name string
	n    int
}

func (d Dataset) contestedFieldCounts() []fieldCount {
	counts := map[string]int{}
	for _, r := range d.Rows {
		for _, f := range r.ContestedFields() {
			counts[f]++
		}
	}
	out := make([]fieldCount, 0, len(counts))
	for name, n := range counts {
		out = append(out, fieldCount{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	return out
}
