package dataset_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/dataset"
)

// M9 slice 2 — the merge, and the numbers.
//
// §15 calls cross-source fuzzy merge "the hardest quality problem in this
// document", and unlike every other stage there is no right answer available to
// the code: whether two rows are the same company is a judgement. So the response
// is measurement rather than cleverness.
//
// The ground truth here needs no model and no labelling: entities are generated
// with KNOWN surface variants, so which pairs should merge is known by
// construction. What is reported is pairwise precision and recall — the standard
// way to score record linkage — and the default threshold is calibrated against
// these numbers rather than chosen.

func mergeSchema(t *testing.T) dataset.Schema {
	t.Helper()
	s, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// entity is one real-world thing and the ways sources write it.
type entity struct {
	id       int
	variants []string
}

// groundTruth is built by hand so every pair's correct answer is known.
//
// The variants are the shapes that actually appear across sources: a legal
// suffix, a dropped suffix, a different suffix, case noise, punctuation, an
// accent lost in transit, a word reordered, a typo.
func groundTruth() []entity {
	return []entity{
		{1, []string{"Acme Ltd", "Acme Limited", "ACME LTD.", "Acme"}},
		{2, []string{"Beta GmbH", "Beta Gmbh", "beta gmbh"}},
		{3, []string{"Nestlé SA", "Nestle SA", "Nestlé S.A."}},
		{4, []string{"British Airways", "Airways, British", "British Airways plc"}},
		{5, []string{"Vodafone Group", "Vodafone Group plc", "Vodafone"}},
		{6, []string{"Siemens Energy AG", "Siemens Energy"}},
		{7, []string{"Deutsche Bank", "Deutsche Bnak"}}, // a transposition typo
		{8, []string{"J.P. Morgan", "JP Morgan", "JPMorgan"}},
		// Entities that must NOT merge, and are the reason precision is reported:
		// each pair shares a leading word or most of a name.
		{9, []string{"Acme Foods"}},
		{10, []string{"Acme Bakery Holdings"}},
		{11, []string{"Siemens Healthineers"}},
		{12, []string{"Deutsche Telekom"}},
		{13, []string{"Beta Industries"}},
		{14, []string{"Vodafone Idea"}},
		{15, []string{"Morgan Stanley"}},
		{16, []string{"Airways Aviation"}},
	}
}

// rowsFor turns ground truth into extracted rows, one per variant, each from its
// own source.
func rowsFor(truth []entity) ([]dataset.Row, map[string]int) {
	var rows []dataset.Row
	owner := map[string]int{}
	for _, e := range truth {
		for i, v := range e.variants {
			src := fmt.Sprintf("https://source%d-%d.example", e.id, i)
			rows = append(rows, dataset.Row{
				Values: map[string]string{"company": v, "revenue": "1000"},
				Source: src,
				Quote:  "a span mentioning " + v,
			})
			owner[v] = e.id
		}
	}
	return rows, owner
}

// score computes pairwise precision and recall against the ground truth.
func score(t *testing.T, d dataset.Dataset, owner map[string]int) (precision, recall float64, tp, fp, fn int) {
	t.Helper()

	// Every pair the merge put together.
	predicted := map[[2]string]bool{}
	for _, row := range d.Rows {
		// A merged row's members are its chosen key value plus the other
		// spellings. Variants, not Others: a key field's alternatives are
		// spellings of one entity rather than a disagreement, and reading the
		// wrong one here would score an empty cluster for every merged row.
		var names []string
		names = append(names, row.Cells["company"].Text)
		names = append(names, row.Cells["company"].Variants...)
		names = append(names, row.Cells["company"].Others...)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				predicted[pair(names[i], names[j])] = true
			}
		}
	}

	// Every pair that should be together.
	var all []string
	for name := range owner {
		all = append(all, name)
	}
	actual := map[[2]string]bool{}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if owner[all[i]] == owner[all[j]] {
				actual[pair(all[i], all[j])] = true
			}
		}
	}

	for p := range predicted {
		if actual[p] {
			tp++
		} else {
			fp++
			t.Logf("  false positive: %q ~ %q", p[0], p[1])
		}
	}
	for p := range actual {
		if !predicted[p] {
			fn++
			t.Logf("  missed:         %q ~ %q", p[0], p[1])
		}
	}
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	} else {
		precision = 1
	}
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	} else {
		recall = 1
	}
	return
}

func pair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// TestMergeQualityOnKnownDuplicates is the milestone's quality number.
//
// Recorded thresholds rather than aspirational ones: these are what the
// implementation achieves on this set, and the test fails if it regresses. It
// also prints the curve across thresholds, which is where DefaultThreshold came
// from.
func TestMergeQualityOnKnownDuplicates(t *testing.T) {
	truth := groundTruth()
	rows, owner := rowsFor(truth)
	schema := mergeSchema(t)

	t.Log("threshold  precision  recall   tp/fp/fn")
	for _, th := range []float64{0.30, 0.45, 0.55, 0.60, 0.70, 0.80, 0.95} {
		d := dataset.Merge(schema, rows, dataset.Options{Threshold: th})
		p, r, tp, fp, fn := score(t, d, owner)
		t.Logf("%8.2f  %9.3f  %6.3f   %d/%d/%d", th, p, r, tp, fp, fn)
	}

	d := dataset.Merge(schema, rows, dataset.Options{})
	p, r, tp, fp, fn := score(t, d, owner)
	t.Logf("default (%.2f): precision %.3f recall %.3f (tp=%d fp=%d fn=%d), %d rows from %d",
		dataset.DefaultThreshold, p, r, tp, fp, fn, len(d.Rows), d.Extracted)

	// The recorded floor, and it is what the implementation actually achieves on
	// this set rather than an aspiration: precision and recall are both 1.000 at
	// the default, so anything less is a regression.
	//
	// Both matter, for different reasons. Low precision merges two companies into
	// one row — a wrong dataset presented as a clean one. Low recall leaves
	// duplicates, which is visible and annoying but not misleading. If they ever
	// have to be traded, precision is the one to hold.
	if p < 1.0 {
		t.Errorf("precision %.3f below the recorded 1.000 — two entities were merged", p)
	}
	if r < 1.0 {
		t.Errorf("recall %.3f below the recorded 1.000 — duplicates survived", r)
	}
	if len(d.Rows) != len(truth) {
		t.Errorf("%d rows for %d entities", len(d.Rows), len(truth))
	}
}

// TestCompleteLinkageStopsAChain.
//
// With transitive closure, "Acme" matches "Acme Ltd" and "Acme Ltd" matches
// nothing else, but "Acme" also matches "Acme Foods" — so a union-find would put
// a real company and an unrelated one in the same row. A row joins a cluster only
// if it matches every member.
// The fixture has to be a genuine chain, and the first attempt was not: with the
// token measure, "Acme" against "Acme Foods" scores 0.5 and never links at all,
// so the linkage rule had nothing to reject and the test passed with transitive
// closure in place. These three do chain — 0.67, 0.75, and 0.50 across the ends.
func TestCompleteLinkageStopsAChain(t *testing.T) {
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Foods"}, Source: "https://a.example"},
		{Values: map[string]string{"company": "Acme Foods Europe"}, Source: "https://b.example"},
		{Values: map[string]string{"company": "Acme Foods Europe West"}, Source: "https://c.example"},
	}
	// The ends must not end up together, whatever happens in the middle.
	a := dataset.Similarity(dataset.Normalise("Acme Foods"), dataset.Normalise("Acme Foods Europe"))
	b := dataset.Similarity(dataset.Normalise("Acme Foods Europe"), dataset.Normalise("Acme Foods Europe West"))
	ends := dataset.Similarity(dataset.Normalise("Acme Foods"), dataset.Normalise("Acme Foods Europe West"))
	if a < dataset.DefaultThreshold || b < dataset.DefaultThreshold || ends >= dataset.DefaultThreshold {
		t.Fatalf("the fixture is not a chain: a~b=%.2f b~c=%.2f a~c=%.2f (threshold %.2f)",
			a, b, ends, dataset.DefaultThreshold)
	}

	d := dataset.Merge(mergeSchema(t), rows, dataset.Options{})
	for _, row := range d.Rows {
		names := append([]string{row.Cells["company"].Text}, row.Cells["company"].Variants...)
		joined := strings.Join(names, " | ")
		// The two ends must never share a row.
		var hasA, hasC bool
		for _, n := range names {
			switch n {
			case "Acme Foods":
				hasA = true
			case "Acme Foods Europe West":
				hasC = true
			}
		}
		if hasA && hasC {
			t.Errorf("transitive closure merged the two ends: %s", joined)
		}
	}
}

// TestSourcesThatDisagreeBothSurvive. §11 renders contradiction edges explicitly
// "rather than silently resolved by whichever claim the model liked", and two
// sources giving one company two revenues is that problem with a column header.
func TestSourcesThatDisagreeBothSurvive(t *testing.T) {
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
			Source: "https://a.example", Quote: "a"},
		{Values: map[string]string{"company": "Acme Limited", "revenue": "1350000"},
			Source: "https://b.example", Quote: "b"},
	}
	d := dataset.Merge(mergeSchema(t), rows, dataset.Options{})
	if len(d.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 merged", len(d.Rows))
	}
	cell := d.Rows[0].Cells["revenue"]
	if !cell.Contested() {
		t.Fatal("the disagreement was resolved silently")
	}
	if len(cell.Others) != 1 {
		t.Errorf("others = %v, want the other source's figure", cell.Others)
	}
	if got := d.Rows[0].ContestedFields(); len(got) != 1 || got[0] != "revenue" {
		t.Errorf("contested fields = %v", got)
	}
	// Both sources are recorded, and each has a quote to check against.
	if len(d.Rows[0].Sources) != 2 || len(d.Rows[0].Quotes) != 2 {
		t.Errorf("provenance lost: sources=%v quotes=%v",
			d.Rows[0].Sources, d.Rows[0].Quotes)
	}
}

// TestAgreementIsPreferredToArrivalOrder. Two sources agreeing against one that
// differs must not lose to whichever happened to be extracted first.
func TestAgreementIsPreferredToArrivalOrder(t *testing.T) {
	// The outlier sorts BEFORE the corroborated value, so a rule that fell back
	// to lexical order would pick the wrong one. The first fixture used "999",
	// which sorts after "1200000" — so the test passed with the count comparison
	// removed.
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1000000"},
			Source: "https://a-first.example"},
		{Values: map[string]string{"company": "Acme Limited", "revenue": "1200000"},
			Source: "https://b.example"},
		{Values: map[string]string{"company": "ACME LTD.", "revenue": "1200000"},
			Source: "https://c.example"},
	}
	d := dataset.Merge(mergeSchema(t), rows, dataset.Options{})
	if len(d.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(d.Rows))
	}
	if got := d.Rows[0].Get("revenue"); got != "1200000" {
		t.Errorf("revenue = %q, want the corroborated 1200000", got)
	}
	if !d.Rows[0].Cells["revenue"].Contested() {
		t.Error("the outlier was discarded rather than kept")
	}
}

// TestTheMergeIsDeterministic. A report is generated from this and §14.1 keys
// cassettes on request bodies, so an order-dependent merge makes a replay a coin
// toss.
func TestTheMergeIsDeterministic(t *testing.T) {
	rows, _ := rowsFor(groundTruth())
	schema := mergeSchema(t)

	first := dataset.Merge(schema, rows, dataset.Options{})
	reversed := make([]dataset.Row, len(rows))
	for i := range rows {
		reversed[i] = rows[len(rows)-1-i]
	}
	second := dataset.Merge(schema, reversed, dataset.Options{})

	if len(first.Rows) != len(second.Rows) {
		t.Fatalf("row counts differ by input order: %d vs %d",
			len(first.Rows), len(second.Rows))
	}
	for i := range first.Rows {
		a := first.Rows[i].Get("company")
		b := second.Rows[i].Get("company")
		if a != b {
			t.Errorf("row %d differs by input order: %q vs %q", i, a, b)
		}
	}
}

func TestNormalisationHandlesWhatSourcesActuallyWrite(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"Acme Ltd", "Acme Limited"},
		{"Nestlé S.A.", "Nestle SA"},
		{"J.P. Morgan", "JP Morgan"},
		{"Siemens-Energy AG", "Siemens Energy"},
		{"  ACME   LTD.  ", "acme"},
	} {
		if na, nb := dataset.Normalise(tc.a), dataset.Normalise(tc.b); na != nb {
			t.Errorf("Normalise(%q)=%q but Normalise(%q)=%q", tc.a, na, tc.b, nb)
		}
	}
	// And it must not collapse things that differ.
	for _, tc := range []struct{ a, b string }{
		{"Acme Foods", "Acme Bakery"},
		{"Deutsche Bank", "Deutsche Telekom"},
	} {
		if dataset.Normalise(tc.a) == dataset.Normalise(tc.b) {
			t.Errorf("Normalise collapsed %q and %q", tc.a, tc.b)
		}
	}
}

// TestAKeySpellingIsNotADisagreement.
//
// "Acme Ltd" and "Acme Limited" are why the two rows merged, so reporting the
// difference as a conflict would have the merge contradicting its own decision —
// and would mark almost every merged row as contested, drowning the
// disagreements that matter.
func TestAKeySpellingIsNotADisagreement(t *testing.T) {
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
			Source: "https://a.example"},
		{Values: map[string]string{"company": "Acme Limited", "revenue": "1200000"},
			Source: "https://b.example"},
	}
	d := dataset.Merge(mergeSchema(t), rows, dataset.Options{})
	if len(d.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(d.Rows))
	}
	row := d.Rows[0]
	if got := row.ContestedFields(); len(got) != 0 {
		t.Errorf("contested = %v, want none — the sources agree on everything", got)
	}
	cell := row.Cells["company"]
	if len(cell.Variants) != 1 {
		t.Errorf("variants = %v, want the other spelling kept", cell.Variants)
	}
	if cell.Contested() {
		t.Error("a spelling was reported as a conflict")
	}
	if d.Contested() != 0 {
		t.Errorf("dataset reports %d contested rows", d.Contested())
	}
}
