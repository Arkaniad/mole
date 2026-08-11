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
	// discarded — each with the sources that gave it.
	//
	// §11's contradiction handling is the precedent: "Contradiction edges are
	// rendered explicitly ('sources disagree: …') rather than silently resolved
	// by whichever claim the model liked." Two sources giving one company two
	// revenues is the same problem with a column header, and picking the one
	// that happened to arrive first is the same mistake.
	//
	// These carry their sources because the winning value carried its own and
	// these did not, which is the asymmetry that makes a disagreement unusable:
	// "revenue is 1.2m (per sec.gov) or 1.35m (per somebody)" is a fact a reader
	// can act on, and "1.2m per sec.gov, or 1.35m" is not.
	Others []Alt `json:"others,omitempty"`
	// Variants are other SPELLINGS of the same value, which is a different thing
	// from a disagreement.
	//
	// Only key fields have them. "Acme Ltd" and "Acme Limited" are why these two
	// rows merged at all, so re-reporting the difference as a conflict would have
	// the merge contradicting its own decision — and would mark almost every
	// merged row as contested, drowning the disagreements that matter. A test
	// caught exactly that.
	Variants []Alt `json:"variants,omitempty"`
	// Sources are the sources that supplied Text.
	Sources []string `json:"sources,omitempty"`
}

// Alt is a value some source gave that is not the one carried forward.
type Alt struct {
	Text    string   `json:"text"`
	Sources []string `json:"sources,omitempty"`
}

// Contested reports whether the sources disagreed about this field.
func (c Cell) Contested() bool { return len(c.Others) > 0 }

// AltTexts returns just the values, for the places that render a list.
func AltTexts(alts []Alt) []string {
	out := make([]string, 0, len(alts))
	for _, a := range alts {
		out = append(out, a.Text)
	}
	return out
}

// Describe renders the cell with its disagreements, for a CSV column that has one
// string to say it all in.
//
// "1200000 (a.example) | 1350000 (b.example)" rather than "1200000": the value
// alone is what a reader would have quoted, and the disagreement is the thing they
// most needed to know before quoting it.
func (c Cell) Describe() string {
	if len(c.Others) == 0 {
		return c.Text
	}
	parts := []string{withSources(c.Text, c.Sources)}
	for _, a := range c.Others {
		parts = append(parts, withSources(a.Text, a.Sources))
	}
	return strings.Join(parts, " | ")
}

func withSources(text string, sources []string) string {
	if len(sources) == 0 {
		return text
	}
	hosts := make([]string, 0, len(sources))
	for _, s := range sources {
		hosts = append(hosts, sourceLabel(s))
	}
	return text + " (" + strings.Join(hosts, ", ") + ")"
}

// sourceLabel shortens a URL to the part a reader recognises. A cell holding three
// full URLs is a cell nobody reads; the host is what says "the regulator" or "a
// blog", and the full URLs are one column over under --provenance.
func sourceLabel(src string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(src, "https://"), "http://")
	s = strings.TrimPrefix(s, "www.")
	if i := strings.IndexAny(s, "/?#"); i > 0 {
		s = s[:i]
	}
	if s == "" {
		return src
	}
	return s
}

// Merged is one entity, assembled from every row that matched it.
type Merged struct {
	Cells map[string]Cell `json:"cells"`
	// Sources is every source that contributed, deduplicated and sorted.
	Sources []string `json:"sources"`
	// Members is how many extracted rows were folded in. One means no
	// corroboration, which is a fact about the row worth reporting.
	Members int `json:"members"`
	// Quotes carries one quote per source, so a reader can check any cell
	// against the sentence it came from — with the offset it was found at, so an
	// auditor re-fetching the page can go to the passage rather than search a
	// document for a sentence that may appear twice.
	Quotes map[string]Quote `json:"quotes,omitempty"`
}

// Get returns the value of a field.
func (m Merged) Get(field string) string { return m.Cells[field].Text }

// SupportingQuote is a quote from a source that actually supplied a value in
// this row.
//
// It used to be the quote of the alphabetically first SOURCE, which need not have
// supplied anything: a row could print a revenue from one page beside a sentence
// from another that never mentioned a number. In a tool whose thesis is §11.5
// grounding, a provenance column pairing a value with unrelated evidence is worse
// than no column — a reader spot-checking it is checking the wrong sentence.
//
// Preference goes to a source that supplied a CONTESTED field's winning value,
// since that is the one a reader is most likely to be checking.
func (m Merged) SupportingQuote() string {
	var contributors []string
	for _, c := range m.Cells {
		if c.Contested() {
			contributors = append(append([]string{}, c.Sources...), contributors...)
		} else {
			contributors = append(contributors, c.Sources...)
		}
	}
	for _, src := range contributors {
		if q := m.Quotes[src]; q.Text != "" {
			return q.Text
		}
	}
	// No cell recorded a source — nothing to be right about, so nothing is
	// claimed.
	return ""
}

// Quote is a verbatim sentence and where it was found.
//
// The offset was extracted, quote-verified, and stored per row — and then thrown
// away at merge time, so nothing downstream could use it. It travels with the text
// now, which is the only reason to have carried it this far.
type Quote struct {
	Text   string `json:"text"`
	Offset int64  `json:"offset,omitempty"`
}

// VariantFields lists the key fields whose spellings differed across sources.
func (m Merged) VariantFields() []string {
	var out []string
	for name, c := range m.Cells {
		if len(c.Variants) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

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

// Disagreements renders every contested field of this row, with the values and
// the sources behind each.
//
// One string, because a CSV has one cell — and without it the `contested` column
// named the fields and nothing carried the rival values, so the CSV said "the
// sources disagree about revenue" and then showed one revenue. A reader with only
// the CSV could not tell what the disagreement was.
func (m Merged) Disagreements() string {
	var parts []string
	for _, name := range m.ContestedFields() {
		parts = append(parts, name+": "+m.Cells[name].Describe())
	}
	return strings.Join(parts, "; ")
}

// Dataset is the finished result.
type Dataset struct {
	Schema Schema   `json:"schema"`
	Rows   []Merged `json:"rows"`

	// Extracted is how many rows were extracted before merging, so a reader can
	// see how much collapsing happened. A dataset of 40 rows from 400
	// extractions is a different result from one of 40 from 41.
	Extracted int `json:"extracted"`
	// Unquoted is how many EXTRACTED rows arrived without a source or a verbatim
	// quote — §11.5's floor, and zero for anything this pipeline produced, since
	// accept() refuses a row whose quote is not in the passage. A non-zero count
	// means rows entered the store another way, and is a hard regression in the
	// eval rather than a statistic.
	Unquoted int `json:"unquoted,omitempty"`
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

// WithVariants counts rows the merge assembled from sources that spelled the key
// differently — the rows whose existence was a judgement rather than an exact
// match.
func (d Dataset) WithVariants() int {
	var n int
	for _, r := range d.Rows {
		if len(r.VariantFields()) > 0 {
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
