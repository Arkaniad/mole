package dataset

import (
	"sort"
	"strings"
	"time"
)

// Row is one extraction from one source.
//
// The provenance fields are not optional extras. §11.5's rule is that a claim
// carries a verbatim quote from the text it was mined from, and a row is a claim
// with columns: a table of facts nobody can trace back to a sentence is exactly
// what this project refuses to produce for prose, and there is no reason the
// standard should drop when the output is a CSV.
type Row struct {
	// Values holds one entry per schema field. A field the source did not supply
	// is absent rather than empty — "not stated" and "stated as blank" are
	// different facts about a source, and the merge treats them differently.
	Values map[string]string `json:"values"`

	Source      string    `json:"source"`
	Quote       string    `json:"quote"`
	QuoteOffset int64     `json:"quote_offset"`
	LeadID      string    `json:"lead_id,omitempty"`
	RetrievedAt time.Time `json:"retrieved_at"`
}

// Get returns a value and whether the source supplied it.
func (r Row) Get(field string) (string, bool) {
	v, ok := r.Values[field]
	return v, ok && strings.TrimSpace(v) != ""
}

// Cell is one field of a merged row.
type Cell struct {
	// Text is the value carried forward.
	Text string `json:"text"`
	// Others are values OTHER sources gave for the same field, kept rather than
	// discarded.
	//
	// §11's contradiction handling is the precedent: "Contradiction edges are
	// rendered explicitly ('sources disagree: …') rather than silently resolved
	// by whichever claim the model liked." Two sources giving one company two
	// revenues is the same problem with a column header, and picking the one
	// that happened to arrive first is the same mistake.
	Others []string `json:"others,omitempty"`
	// Sources are the sources that supplied Text.
	Sources []string `json:"sources,omitempty"`
}

// Contested reports whether the sources disagreed about this field.
func (c Cell) Contested() bool { return len(c.Others) > 0 }

// Merged is one entity, assembled from every row that matched it.
type Merged struct {
	Cells map[string]Cell `json:"cells"`
	// Sources is every source that contributed, deduplicated and sorted.
	Sources []string `json:"sources"`
	// Members is how many extracted rows were folded in. One means no
	// corroboration, which is a fact about the row worth reporting.
	Members int `json:"members"`
	// Quotes carries one quote per source, so a reader can check any cell
	// against the sentence it came from.
	Quotes map[string]string `json:"quotes,omitempty"`
}

// Get returns the value of a field.
func (m Merged) Get(field string) string { return m.Cells[field].Text }

// ContestedFields lists the fields the sources disagreed about, in order.
func (m Merged) ContestedFields() []string {
	var out []string
	for name, c := range m.Cells {
		if c.Contested() {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Dataset is the finished result.
type Dataset struct {
	Schema Schema   `json:"schema"`
	Rows   []Merged `json:"rows"`

	// Extracted is how many rows were extracted before merging, so a reader can
	// see how much collapsing happened. A dataset of 40 rows from 400
	// extractions is a different result from one of 40 from 41.
	Extracted int `json:"extracted"`
	// Notes record what the pipeline withheld or could not decide.
	Notes []string `json:"notes,omitempty"`
}

// Contested counts rows with at least one disagreement.
func (d Dataset) Contested() int {
	var n int
	for _, r := range d.Rows {
		if len(r.ContestedFields()) > 0 {
			n++
		}
	}
	return n
}

// Corroborated counts rows more than one source agreed on.
func (d Dataset) Corroborated() int {
	var n int
	for _, r := range d.Rows {
		if len(r.Sources) > 1 {
			n++
		}
	}
	return n
}
