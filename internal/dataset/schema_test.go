package dataset_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/dataset"
)

// M9 slice 0.
//
// A schema is the one part of dataset mode that can be judged without running
// anything, so it is checked strictly: everything downstream — the extraction
// prompt, the merge key, the CSV header — is derived from it, and a schema that
// validates but cannot be merged produces a list of quotes wearing a table's
// clothes.

func TestTheCompactFormParses(t *testing.T) {
	s, err := dataset.ParseSpec(
		"company:text!,revenue:number=annual revenue in USD for the last full year,founded:date")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if got := s.Names(); strings.Join(got, ",") != "company,revenue,founded" {
		t.Errorf("names = %v", got)
	}
	if got := s.Keys(); len(got) != 1 || got[0] != "company" {
		t.Errorf("keys = %v, want [company]", got)
	}
	f, _ := s.Field("revenue")
	if f.Type != dataset.TypeNumber {
		t.Errorf("revenue type = %q", f.Type)
	}
	// The description is the single most useful thing a user supplies, so it has
	// to survive parsing and reach the prompt.
	if !strings.Contains(f.Description, "annual revenue in USD") {
		t.Errorf("description lost: %q", f.Description)
	}
	if !strings.Contains(s.Describe(), "annual revenue in USD") {
		t.Errorf("the description does not reach the prompt:\n%s", s.Describe())
	}
}

func TestASchemaWithoutAKeyIsRefused(t *testing.T) {
	_, err := dataset.ParseSpec("company:text,revenue:number")
	if err == nil {
		t.Fatal("accepted a schema with no key field")
	}
	if !errors.Is(err, dataset.ErrSchema) {
		t.Fatalf("err = %v, want ErrSchema", err)
	}
	// The refusal has to explain the consequence, not just the rule: without a
	// key there is nothing to merge on, and merging is what makes this a dataset.
	if !strings.Contains(err.Error(), "merged") {
		t.Errorf("the refusal does not say why a key is needed: %v", err)
	}
}

// TestCaseIsNormalisedRatherThanRefused. A user typing `Company` means
// `company`, and the file loader already normalises — requiring them to know the
// rule in one form and not the other would be a rule about mole rather than
// about the data. The connector does the same with CSV headers.
func TestCaseIsNormalisedRatherThanRefused(t *testing.T) {
	s, err := dataset.ParseSpec("Company:TEXT!,Revenue:Number")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if got := strings.Join(s.Names(), ","); got != "company,revenue" {
		t.Errorf("names = %q, want normalised", got)
	}
	if f, _ := s.Field("revenue"); f.Type != dataset.TypeNumber {
		t.Errorf("type = %q, want normalised", f.Type)
	}
}

func TestHostileFieldNamesAreRefused(t *testing.T) {
	for _, spec := range []string{
		`"a,b":text!`,
		`company name:text!`,
		`1st:text!`,
		`:text!`,
		`company:text!,company:number`,
		`company:colour!`,
	} {
		if _, err := dataset.ParseSpec(spec); err == nil {
			t.Errorf("accepted %q", spec)
		}
	}
}

func TestAWideSchemaIsRefused(t *testing.T) {
	var parts []string
	parts = append(parts, "k:text!")
	for i := 0; i < dataset.MaxFields; i++ {
		parts = append(parts, "f"+string(rune('a'+i))+":text")
	}
	_, err := dataset.ParseSpec(strings.Join(parts, ","))
	if err == nil {
		t.Fatal("accepted a schema past the field limit")
	}
	if !strings.Contains(err.Error(), "worse extraction") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestASchemaFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.json")
	body := `{"name":"companies","fields":[
	  {"name":"COMPANY","type":"TEXT","key":true,"description":"legal entity name"},
	  {"name":"revenue","type":"number"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := dataset.LoadSchema(path)
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	// Case is normalised on the way in, so a hand-written file cannot produce a
	// header the compact form would have refused.
	if got := s.Names(); strings.Join(got, ",") != "company,revenue" {
		t.Errorf("names = %v", got)
	}
	if f, _ := s.Field("company"); f.Type != dataset.TypeText || !f.Key {
		t.Errorf("company = %+v", f)
	}
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

func sampleDataset(t *testing.T) dataset.Dataset {
	t.Helper()
	s, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	return dataset.Dataset{
		Schema:    s,
		Extracted: 5,
		Rows: []dataset.Merged{
			{
				Cells: map[string]dataset.Cell{
					"company": {Text: "Acme Ltd", Sources: []string{"https://a.example"}},
					// Two sources, two revenues, both kept.
					"revenue": {Text: "1200000",
						Others: []dataset.Alt{{Text: "1350000",
							Sources: []string{"https://b.example"}}},
						Sources: []string{"https://a.example"}},
				},
				Sources: []string{"https://a.example", "https://b.example"},
				Members: 2,
				Quotes: map[string]dataset.Quote{
					"https://a.example": {
						Text:   "Acme Ltd reported revenue of $1.2m in 2024",
						Offset: 120,
					},
				},
			},
			{
				Cells: map[string]dataset.Cell{
					"company": {Text: "Beta GmbH"},
					"revenue": {Text: "900000"},
				},
				Sources: []string{"https://c.example"},
				Members: 1,
			},
		},
	}
}

func TestCSVCarriesTheDisagreementsItCannotHold(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleDataset(t).WriteCSV(&buf, false); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("the output is not valid CSV: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %d, want header + 2 rows", len(recs))
	}
	header := strings.Join(recs[0], ",")
	if header != "company,revenue,sources,contested" {
		t.Fatalf("header = %q", header)
	}
	// A CSV holds one value per cell, so the disagreement cannot be IN the cell.
	// It must still be visible, or the most convenient output is the one that
	// hides what the sources could not agree on.
	if recs[1][3] != "revenue" {
		t.Errorf("the contested field is not named: %q", recs[1][3])
	}
	if recs[1][2] != "2" {
		t.Errorf("source count = %q, want 2", recs[1][2])
	}
	if recs[2][3] != "" {
		t.Errorf("an uncontested row is marked contested: %q", recs[2][3])
	}
}

func TestCSVCellsSurviveASpreadsheet(t *testing.T) {
	s, _ := dataset.ParseSpec("company:text!")
	// Runs of whitespace as well as newlines and tabs: replacing the separators
	// one for one leaves "as  Acme", which is what the collapse is for.
	d := dataset.Dataset{Schema: s, Rows: []dataset.Merged{{
		Cells: map[string]dataset.Cell{
			"company": {Text: "  Acme\nLtd\ttrading   as\r\n\nAcme  "},
		},
	}}}
	var buf bytes.Buffer
	if err := d.WriteCSV(&buf, false); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	got := recs[1][0]
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("a newline or tab reached a cell: %q", got)
	}
	if got != "Acme Ltd trading as Acme" {
		t.Errorf("cell = %q", got)
	}
}

func TestJSONIsTheCompleteForm(t *testing.T) {
	var buf bytes.Buffer
	if err := sampleDataset(t).WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back dataset.Dataset
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("the output is not valid JSON: %v", err)
	}
	cell := back.Rows[0].Cells["revenue"]
	if !cell.Contested() {
		t.Fatal("the disagreement did not survive the round trip")
	}
	if len(cell.Others) != 1 || cell.Others[0].Text != "1350000" {
		t.Errorf("others = %v, want the second source's figure", cell.Others)
	}
	// Provenance has to survive too: a table nobody can trace to a sentence is
	// what §11.5 exists to prevent, and a CSV is not an exception.
	if q := back.Rows[0].Quotes["https://a.example"]; q.Text == "" || q.Offset != 120 {
		t.Errorf("quote = %+v, want the text and the offset it was found at", q)
	}
}

func TestTheSummaryQualifiesTheDataset(t *testing.T) {
	got := sampleDataset(t).Summary()
	for _, want := range []string{
		"2 row(s) from 5 extraction(s)",
		"1 corroborated",
		"1 from a single source",
		"1 with sources that disagree",
		"revenue",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary omits %q:\n%s", want, got)
		}
	}
}
