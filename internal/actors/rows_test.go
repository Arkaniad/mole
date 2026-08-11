package actors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/tools/search"
)

// M9 slice 1.
//
// A row is a claim with columns, so the property that matters is the same one:
// §11.5 drops anything whose quote is not in the text it came from. A CSV is more
// likely to be believed without checking than a paragraph is — nobody reads a
// spreadsheet sceptically — so these tests are about what the extractor REFUSES.

const companyPage = `Acme Ltd, incorporated on 3 March 1998, reported revenue of
$1.2m for the year ending December 2024. Its head office is in Leeds.

Beta GmbH was founded in 2004 and had revenue of 900,000 euros last year.

Gamma Inc did not disclose its revenue.`

func rowSchema(t *testing.T) dataset.Schema {
	t.Helper()
	s, err := dataset.ParseSpec("company:text!,revenue:number,founded:date")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rowModel replies with whatever JSON the test scripts.
func rowModel(reply string) *fakeLLM {
	return &fakeLLM{mineFunc: func(string) string { return reply }}
}

func mineRows(t *testing.T, reply string) actors.RowOutput {
	t.Helper()
	m := &actors.RowMiner{LLM: rowModel(reply), SessionID: "s1", Schema: rowSchema(t)}
	out, err := m.Mine(context.Background(), actors.RowInput{
		Lead:      core.Lead{ID: "l1", SessionID: "s1", Query: "company revenues"},
		SourceURL: "https://example.org/a",
		Text:      companyPage,
	})
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}
	return out
}

func TestRowsAreKeptWhenTheQuoteIsInThePage(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Acme Ltd","revenue":"$1.2m","founded":"3 March 1998"},
	   "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"},
	  {"values":{"company":"Beta GmbH","revenue":"900,000","founded":"2004"},
	   "quote":"Beta GmbH was founded in 2004 and had revenue of 900,000 euros last year."}
	]}`)

	if len(out.Rows) != 2 {
		t.Fatalf("rows = %d, want 2; rejected %d", len(out.Rows), out.Rejected)
	}
	// Values are normalised at extraction, not at merge time: two sources that
	// agree must not be recorded as disagreeing over punctuation.
	first := out.Rows[0]
	if got := first.Values["revenue"]; got != "1200000" {
		t.Errorf("revenue = %q, want 1200000 — $1.2m was not normalised", got)
	}
	if got := first.Values["founded"]; got != "1998-03-03" {
		t.Errorf("founded = %q, want 1998-03-03", got)
	}
	if got := out.Rows[1].Values["revenue"]; got != "900000" {
		t.Errorf("revenue = %q, want 900000 — the thousands separator survived", got)
	}
	if got := out.Rows[1].Values["founded"]; got != "2004" {
		t.Errorf("founded = %q, want the bare year the page gave", got)
	}
	// Provenance, because a table nobody can trace to a sentence is what §11.5
	// exists to prevent.
	for _, r := range out.Rows {
		if r.Source != "https://example.org/a" || strings.TrimSpace(r.Quote) == "" {
			t.Errorf("row has no usable provenance: %+v", r)
		}
		if !strings.Contains(companyPage, r.Quote) {
			t.Errorf("the stored quote is not in the page: %q", r.Quote)
		}
	}
	if out.HasCall == false || out.Call.Type != core.CallLLM {
		t.Error("the model call was not recorded, so the extraction looks free")
	}
}

func TestAFabricatedRowIsDropped(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Delta plc","revenue":"5000000"},
	   "quote":"Delta plc reported revenue of five million"}
	]}`)

	if len(out.Rows) != 0 {
		t.Fatalf("a row whose quote is not in the page was kept: %+v", out.Rows)
	}
	if out.Rejected != 1 || out.Proposed != 1 {
		t.Errorf("proposed = %d rejected = %d, want 1 and 1 — a model that invents "+
			"every row must not look like one that found nothing",
			out.Proposed, out.Rejected)
	}
}

// TestARowWithNoKeyIsDropped. A row that identifies nothing can neither be
// merged nor reported, and a page that produced one was not answering the
// question.
func TestARowWithNoKeyIsDropped(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"revenue":"1200000"},
	   "quote":"reported revenue of\n$1.2m for the year ending December 2024"}
	]}`)
	if len(out.Rows) != 0 {
		t.Fatalf("a row with no key field was kept: %+v", out.Rows)
	}
	if out.Rejected != 1 {
		t.Errorf("rejected = %d, want 1", out.Rejected)
	}
}

// TestAValueTheTypeCannotHoldIsDroppedNotCarried. A number column holding
// "roughly $1.2m" merges against nothing and renders as a value somebody will
// sort.
func TestAValueTheTypeCannotHoldIsDroppedNotCarried(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Acme Ltd","revenue":"roughly one point two million",
	             "founded":"some time in the nineties"},
	   "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"}
	]}`)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the key is present)", len(out.Rows))
	}
	r := out.Rows[0]
	if _, ok := r.Values["revenue"]; ok {
		t.Errorf("an unparseable number was carried: %q", r.Values["revenue"])
	}
	if _, ok := r.Values["founded"]; ok {
		t.Errorf("an unparseable date was carried: %q", r.Values["founded"])
	}
	if r.Values["company"] != "Acme Ltd" {
		t.Errorf("the usable field was lost: %+v", r.Values)
	}
}

// TestAFieldTheSourceDidNotStateIsAbsentNotEmpty. "Not stated" and "stated as
// blank" are different facts about a source, and the merge treats them
// differently.
func TestAFieldTheSourceDidNotStateIsAbsentNotEmpty(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Gamma Inc","revenue":""},
	   "quote":"Gamma Inc did not disclose its revenue."}
	]}`)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(out.Rows))
	}
	if _, present := out.Rows[0].Values["revenue"]; present {
		t.Error("an empty value was recorded as a stated one")
	}
	if _, ok := out.Rows[0].Get("revenue"); ok {
		t.Error("Get reports a value the source did not give")
	}
}

// TestFieldsOutsideTheSchemaAreIgnored. The reply is model output; a key nobody
// asked for is not a column, and carrying it would put text the model chose into
// the header of a file somebody opens.
func TestFieldsOutsideTheSchemaAreIgnored(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Acme Ltd","ceo_home_address":"12 Elm Street"},
	   "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"}
	]}`)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %d", len(out.Rows))
	}
	if _, ok := out.Rows[0].Values["ceo_home_address"]; ok {
		t.Error("a field outside the schema crossed into the dataset")
	}
	raw, _ := json.Marshal(out.Rows[0])
	if strings.Contains(string(raw), "Elm Street") {
		t.Errorf("text the model chose reached the row: %s", raw)
	}
}

// TestARowCountCeilingBinds. One verbose page must not dominate the dataset, for
// the same reason MaxClaimsPerSource exists.
func TestARowCountCeilingBinds(t *testing.T) {
	var rows []string
	for i := 0; i < 40; i++ {
		rows = append(rows, `{"values":{"company":"Acme Ltd"},
		  "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"}`)
	}
	m := &actors.RowMiner{LLM: rowModel(`{"rows":[` + strings.Join(rows, ",") + `]}`),
		SessionID: "s1", Schema: rowSchema(t)}
	out, err := m.Mine(context.Background(), actors.RowInput{
		Lead:      core.Lead{ID: "l1", SessionID: "s1"},
		SourceURL: "https://example.org/a",
		Text:      companyPage,
		MaxRows:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rows) != 5 {
		t.Errorf("rows = %d, want the ceiling of 5", len(out.Rows))
	}
}

// TestNumberFormsAPageActuallyUses. Dropping these would lose the fact rather
// than the noise.
func TestNumberFormsAPageActuallyUses(t *testing.T) {
	s, _ := dataset.ParseSpec("k:text!,n:number")
	for _, tc := range []struct{ in, want string }{
		{"1,200,000", "1200000"},
		{"$1.2m", "1200000"},
		{"1.5bn", "1500000000"},
		{"12k", "12000"},
		{"(4500)", "-4500"},
		{"98.6", "98.6"},
		{"€900,000", "900000"},
	} {
		m := &actors.RowMiner{LLM: rowModel(`{"rows":[{"values":{"k":"x","n":"` + tc.in +
			`"},"quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"}]}`),
			SessionID: "s1", Schema: s}
		out, err := m.Mine(context.Background(), actors.RowInput{
			Lead: core.Lead{ID: "l1"}, SourceURL: "u", Text: companyPage})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Rows) != 1 {
			t.Fatalf("%q: rows = %d", tc.in, len(out.Rows))
		}
		if got := out.Rows[0].Values["n"]; got != tc.want {
			t.Errorf("%q normalised to %q, want %q", tc.in, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------------
// Dataset mode through the whole actor (M9, slice 3)
// -----------------------------------------------------------------------------

// TestTheActorInDatasetModeStoresRowsAndNoClaims.
//
// The write path end to end: search, fetch, chunk, extract rows, persist. Verified
// against fakes rather than against the network — the only reachable model cannot
// produce structured output (the same blocker M6 and M8 record), so a live run
// would prove the wiring and burn a search quota to do it.
//
// Rows INSTEAD of claims is the property: one model call per chunk either way, and
// asking for both would double the cost of every chunk to produce a report nobody
// requested.
func TestTheActorInDatasetModeStoresRowsAndNoClaims(t *testing.T) {
	const page = `Acme Ltd reported revenue of $1.2m for 2024. Beta GmbH made 900,000 euros in the same period.`

	h := newHarness(t,
		[]search.Result{{URL: "https://example.org/a", Title: "Revenues"}},
		map[string]*fetch.Result{
			"https://example.org/a": {
				URL: "https://example.org/a", Outcome: fetch.OutcomeOK, StatusCode: 200,
				ContentType: "text/html",
				Content: []byte("<html><body><h1>Revenues</h1><p>" + page + "</p><p>" +
					strings.Repeat("Filler sentence to give the extractor a document. ", 20) +
					"</p></body></html>"),
			},
		},
		func(prompt string) string {
			// The row extractor is the only thing called in dataset mode, so any
			// mine-shaped reply here would be a claim call that should not happen.
			if !strings.Contains(prompt, `"rows"`) {
				t.Errorf("the claim miner was called in dataset mode:\n%s", prompt[:200])
				return `{"claims":[]}`
			}
			return `{"rows":[
			  {"values":{"company":"Acme Ltd","revenue":"$1.2m"},
			   "quote":"Acme Ltd reported revenue of $1.2m for 2024."},
			  {"values":{"company":"Beta GmbH","revenue":"900,000"},
			   "quote":"Beta GmbH made 900,000 euros in the same period."}
			]}`
		})

	schema := rowSchema(t)
	h.actor.Rows = &actors.RowMiner{
		LLM: h.llm, SessionID: h.session.ID, Schema: schema,
	}

	res, err := h.actor.Run(context.Background(), h.lead)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2; claims = %d", len(res.Rows), len(res.Claims))
	}
	if len(res.Claims) != 0 {
		t.Errorf("dataset mode produced %d claims as well as rows", len(res.Claims))
	}

	// Persisted, and readable back through the schema the session used.
	var stored []dataset.Row
	if err := h.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		stored, err = q.ListRows(ctx, h.session.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("stored rows = %d, want 2", len(stored))
	}
	// In insertion order, and with the normalisation applied at extraction.
	if stored[0].Values["revenue"] != "1200000" {
		t.Errorf("stored revenue = %q, want the normalised figure",
			stored[0].Values["revenue"])
	}
	for _, r := range stored {
		if r.Source == "" || r.Quote == "" {
			t.Errorf("a stored row lost its provenance: %+v", r)
		}
	}

	// And the merge over what was stored gives one row per company.
	d := dataset.Merge(schema, stored, dataset.Options{})
	if len(d.Rows) != 2 || d.Extracted != 2 {
		t.Errorf("merge gave %d rows from %d extractions, want 2 from 2",
			len(d.Rows), d.Extracted)
	}
}

// -----------------------------------------------------------------------------
// M9 review, second pass
// -----------------------------------------------------------------------------

// TestATruncatedRowReplyKeepsTheRowsThatCompleted.
//
// The claim path was given a salvage step after a live 3B model lost three mining
// calls in four to output truncation; the row path was written without one, and a
// row is BIGGER than a claim — one quote plus a value per field — so asking for
// twenty-five makes truncation the normal case rather than a corner. Every
// complete row in the reply below was being discarded along with the cut-off one.
//
// Safe for the §11.5 reason: a salvaged row still has to carry a quote that is in
// the passage and still has to fill a key.
func TestATruncatedRowReplyKeepsTheRowsThatCompleted(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Acme Ltd","revenue":"1200000"},
	   "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"},
	  {"values":{"company":"Beta GmbH","revenue":"900000"},
	   "quote":"Beta GmbH was founded in 2004 and had revenue of 900,000 euros"},
	  {"values":{"company":"Gamma Inc"},"quote":"Gamma Inc did not disc`)

	if len(out.Rows) != 2 {
		t.Fatalf("rows = %d, want the two that completed: %+v", len(out.Rows), out.Rows)
	}
	if out.Rows[0].Values["company"] != "Acme Ltd" || out.Rows[1].Values["company"] != "Beta GmbH" {
		t.Errorf("wrong rows salvaged: %+v", out.Rows)
	}
}

// TestATruncatedRowKeepsFailingTheQuoteCheck. Salvage must not become a way in.
func TestATruncatedRowKeepsFailingTheQuoteCheck(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Delta SA","revenue":"5000000"},
	   "quote":"Delta SA reported revenue of five million"},
	  {"values":{"company":"Acme Ltd"},"quote":"Acme Ltd, incorporated on 3 March 1998, reported`)
	for _, r := range out.Rows {
		if r.Values["company"] == "Delta SA" {
			t.Error("a fabricated row survived because the reply was truncated")
		}
	}
}

// TestAnEmptyRowSetIsNotAFailedChunk. "No rows here" is a legitimate answer and
// the prompt asks for it; reporting it as a parse failure turns a correct response
// into a failed chunk.
func TestAnEmptyRowSetIsNotAFailedChunk(t *testing.T) {
	for _, reply := range []string{`{"rows":[]}`, `[]`, "```json\n[]\n```"} {
		m := &actors.RowMiner{LLM: rowModel(reply), SessionID: "s1", Schema: rowSchema(t)}
		out, err := m.Mine(context.Background(), actors.RowInput{
			Lead:      core.Lead{ID: "l1", SessionID: "s1", Query: "company revenues"},
			SourceURL: "https://example.org/a",
			Text:      companyPage,
		})
		if err != nil {
			t.Errorf("reply %q: %v", reply, err)
		}
		if len(out.Rows) != 0 {
			t.Errorf("reply %q produced rows: %+v", reply, out.Rows)
		}
	}
}

// TestValuesDroppedByTheirTypeAreCounted.
//
// A row could arrive with three fields, lose two to coercion, keep its key and be
// reported as accepted — so the run said the extraction was clean and delivered
// empty cells. Rejected rows had a counter; silently emptied ones did not, and the
// honest reading (the schema's types do not match what these sources write) was
// invisible.
func TestValuesDroppedByTheirTypeAreCounted(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"company":"Acme Ltd","revenue":"roughly one point two million",
	             "founded":"some time in the nineties"},
	   "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"}
	]}`)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(out.Rows))
	}
	if out.Coerced != 2 {
		t.Errorf("coerced = %d, want 2 (the revenue and the date)", out.Coerced)
	}
	if out.Rejected != 0 {
		t.Errorf("rejected = %d; a coerced value is not a rejected row", out.Rejected)
	}
}

// TestCoercionIsCountedEvenForARowThatIsThenRejected: the count is about the
// schema, so a row that also fails the key check must not hide it.
func TestCoercionIsCountedEvenForARowThatIsThenRejected(t *testing.T) {
	out := mineRows(t, `{"rows":[
	  {"values":{"revenue":"roughly one point two million"},
	   "quote":"Acme Ltd, incorporated on 3 March 1998, reported revenue of"}
	]}`)
	if len(out.Rows) != 0 {
		t.Fatalf("a row with no key was kept: %+v", out.Rows)
	}
	if out.Coerced != 1 {
		t.Errorf("coerced = %d, want 1", out.Coerced)
	}
	if out.Rejected != 1 {
		t.Errorf("rejected = %d, want 1", out.Rejected)
	}
}

// TestAScaledFigureIsExact.
//
// Found in a live DeepSeek dataset run: "£32.7 billion" reached the CSV as
// 32700000000.000004. The multiplier was a float, 32.7 × 1e9 is not representable,
// so the value was not equal to its own truncation and the integer path was skipped
// — putting a number with a floating-point tail in a revenue column somebody was
// about to sum.
func TestAScaledFigureIsExact(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"£32.7 billion", "32700000000"},
		{"£32.7bn", "32700000000"},
		{"32.7bn", "32700000000"},
		{"1.1bn", "1100000000"},
		{"17.9bn", "17900000000"},
		{"8.2m", "8200000"},
		{"1.15m", "1150000"},
		{"4.7k", "4700"},
		{"0.5bn", "500000000"},
		// A fraction longer than the shift keeps its remainder rather than
		// rounding: 1.2345k is 1234.5, not 1234 and not 1235.
		{"1.2345k", "1234.5"},
		// A bracketed loss keeps its sign through the shift.
		{"(1.2bn)", "-1200000000"},
		// Whole numbers and unscaled values are unaffected.
		{"61470000000", "61470000000"},
		{"12", "12"},
	} {
		got, ok := actors.CoerceNumberForTest(tc.in)
		if !ok {
			t.Errorf("%q was refused", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("%q → %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(got, "e") || strings.Contains(got, "E") {
			t.Errorf("%q → %q, which is not a figure a spreadsheet sums", tc.in, got)
		}
	}
}

// TestNoScaledFigureCarriesAFloatingPointTail, over every magnitude and one decimal
// place — the shape the live bug had.
func TestNoScaledFigureCarriesAFloatingPointTail(t *testing.T) {
	for _, suffix := range []string{"k", "m", "b", "bn"} {
		for d := 1; d <= 9; d++ {
			in := fmt.Sprintf("%d.%dbn", 10+d, d)
			in = strings.Replace(in, "bn", suffix, 1)
			got, ok := actors.CoerceNumberForTest(in)
			if !ok {
				t.Fatalf("%q was refused", in)
			}
			if strings.Contains(got, ".") {
				t.Errorf("%q → %q; a one-decimal figure scaled by a power of ten "+
					"is a whole number", in, got)
			}
		}
	}
}

// TestASpelledOutMagnitudeIsRead.
//
// "£32.7 billion" is how a page states a revenue, and the attached-only rule
// refused it — dropping the value rather than reading it. The rule exists because
// "5 m" might be metres; that ambiguity does not exist for a word.
func TestASpelledOutMagnitudeIsRead(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"£32.7 billion", "32700000000"},
		{"32.7 Billion", "32700000000"},
		{"1.2 million", "1200000"},
		{"450 thousand", "450000"},
		{"2 trillion", "2000000000000"},
		{"$8.5 billion", "8500000000"},
	} {
		got, ok := actors.CoerceNumberForTest(tc.in)
		if !ok {
			t.Errorf("%q was refused", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("%q → %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestASpacedLetterIsStillAmbiguousAndRefused. The reason the attached rule exists:
// "5 m" is meters as often as millions.
func TestASpacedLetterIsStillAmbiguousAndRefused(t *testing.T) {
	for _, in := range []string{"5 m", "5 k", "5 b"} {
		if got, ok := actors.CoerceNumberForTest(in); ok && got != "5" {
			t.Errorf("%q → %q; a spaced single letter must not be read as a magnitude",
				in, got)
		}
	}
}
