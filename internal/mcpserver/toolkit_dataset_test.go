package mcpserver_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
)

// Toolkit mode, slice 5: a table the agent filled in, under §11.5's rule.
//
// A row is a claim with columns, so every test here is the claim_add suite asked
// again with a header line. The reason it is asked again rather than assumed is
// that a CSV is believed without checking in a way prose is not — nobody reads a
// spreadsheet sceptically — so this is the tool where a dropped quote check would
// do the most damage and the one an agent has most reason to want relaxed.

// datasetSchema is the fixture schema: one key, one number, one text.
var datasetSchema = map[string]any{
	"name": "trials",
	"fields": []map[string]any{
		{"name": "trial", "type": "text", "key": true},
		{"name": "participants", "type": "number"},
		{"name": "finding", "type": "text"},
	},
}

// openDataset opens a dataset session and fetches the fixture.
func openDataset(t *testing.T, r *rig) (session, doc string) {
	t.Helper()
	var opened struct {
		SessionID string `json:"session_id"`
	}
	res := r.call(t, "mole.session_open", map[string]any{
		"question": "trials of time-restricted eating", "schema": datasetSchema}, &opened)
	if res.IsError {
		t.Fatalf("dataset session refused: %s", errText(res))
	}
	var fetched struct {
		DocID string `json:"doc_id"`
	}
	r.call(t, "mole.fetch", map[string]any{
		"session_id": opened.SessionID, "url": r.pageURL}, &fetched)
	return opened.SessionID, fetched.DocID
}

func addRow(t *testing.T, r *rig, sess, doc string, values map[string]string, quote string) (
	accepted int, rejected string,
) {
	t.Helper()
	var out struct {
		Accepted int `json:"accepted"`
		Rejected []struct {
			Reason string `json:"reason"`
		} `json:"rejected"`
		Coerced []string `json:"coerced"`
	}
	res := r.call(t, "mole.rows_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"rows": []map[string]any{{"values": values, "quote": quote}},
	}, &out)
	if res.IsError {
		return 0, errText(res)
	}
	if len(out.Rejected) > 0 {
		return out.Accepted, out.Rejected[0].Reason
	}
	return out.Accepted, ""
}

// TestARowIsRefusedWhenItsQuoteIsNotInTheDocument.
//
// The load-bearing test of the slice, and the same rule as claim_add: mole checks
// against its own stored copy, so a model that invents a figure cannot supply the
// sentence that would justify it.
func TestARowIsRefusedWhenItsQuoteIsNotInTheDocument(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	n, why := addRow(t, r, sess, doc, map[string]string{
		"trial": "Pooled analysis", "participants": "599"},
		"Across ten randomised trials enrolling 12000 participants")
	if n != 0 {
		t.Fatal("a row with a fabricated quote was recorded")
	}
	if !strings.Contains(why, "stored document") {
		t.Errorf("the refusal does not say what was checked: %s", why)
	}
}

// TestAQuotedRowIsRecordedWithItsProvenance.
func TestAQuotedRowIsRecordedWithItsProvenance(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	// A quote from the middle of the document, so the offset test below is about
	// the offset rather than about the first sentence happening to be at zero.
	n, why := addRow(t, r, sess, doc, map[string]string{
		"trial": "Pooled analysis", "finding": "fasting glucose fell"},
		"reduced fasting glucose in adults with prediabetes")
	if n != 1 {
		t.Fatalf("a quoted row was refused: %s", why)
	}

	var rows []dataset.Row
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		rows, err = q.ListRows(ctx, sess)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored %d rows, want 1", len(rows))
	}
	if rows[0].Source == "" || rows[0].Quote == "" {
		t.Errorf("a row landed without provenance: %+v", rows[0])
	}
	if rows[0].QuoteOffset <= 0 {
		t.Errorf("offset = %d; an auditor re-fetching the page has nowhere to look",
			rows[0].QuoteOffset)
	}
}

// TestARowFillingNoKeyFieldIsRefused, because it identifies nothing and so can
// neither be merged nor reported.
func TestARowFillingNoKeyFieldIsRefused(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	n, why := addRow(t, r, sess, doc, map[string]string{"participants": "599"},
		"Across ten randomised trials enrolling 599 participants")
	if n != 0 {
		t.Fatal("a keyless row was recorded")
	}
	if !strings.Contains(why, "key") {
		t.Errorf("the refusal does not name the problem: %s", why)
	}
}

// TestAValueTheFieldTypeCannotHoldIsDroppedAndReported.
//
// The row still lands — it has a key and a verified quote — but the cell is empty,
// and an empty cell reads as "the source did not state it". Saying which value was
// dropped is the difference between a schema whose types are wrong and a source
// that was silent.
func TestAValueTheFieldTypeCannotHoldIsDroppedAndReported(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	var out struct {
		Accepted int      `json:"accepted"`
		Coerced  []string `json:"coerced"`
		Note     string   `json:"note"`
	}
	r.call(t, "mole.rows_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"rows": []map[string]any{{
			"values": map[string]string{
				"trial": "Pooled analysis", "participants": "several hundred"},
			"quote": "Across ten randomised trials enrolling 599 participants",
		}},
	}, &out)

	if out.Accepted != 1 {
		t.Fatalf("the row was dropped entirely; accepted = %d", out.Accepted)
	}
	if !contains(out.Coerced, "participants") {
		t.Errorf("coerced = %v, want the field that was dropped named", out.Coerced)
	}
	if out.Note == "" {
		t.Error("nothing tells the caller a cell is empty because of a type mismatch")
	}
}

// TestOneBadRowDoesNotDiscardTheGoodOnesInTheSameCall.
//
// All-or-nothing here would make the obvious repair — resend the batch — resend
// the rows that were already accepted, and duplicate them.
func TestOneBadRowDoesNotDiscardTheGoodOnesInTheSameCall(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	var out struct {
		Accepted int `json:"accepted"`
		Rejected []struct {
			Index  int    `json:"index"`
			Reason string `json:"reason"`
		} `json:"rejected"`
	}
	r.call(t, "mole.rows_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"rows": []map[string]any{
			{"values": map[string]string{"trial": "Invented"}, "quote": "a sentence no page contains anywhere"},
			{"values": map[string]string{"trial": "Pooled analysis", "participants": "599"},
				"quote": "Across ten randomised trials enrolling 599 participants"},
		},
	}, &out)

	if out.Accepted != 1 {
		t.Errorf("accepted = %d, want the good row kept", out.Accepted)
	}
	if len(out.Rejected) != 1 || out.Rejected[0].Index != 0 {
		t.Errorf("rejected = %+v, want the first row named by index", out.Rejected)
	}
}

// TestRowsAreRefusedWithoutASchema.
//
// A session opened for prose has no schema, and rows stored against none could
// never be rendered — `mole dataset` refuses a session with no stored schema.
func TestRowsAreRefusedWithoutASchema(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	res := r.call(t, "mole.rows_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"rows": []map[string]any{{
			"values": map[string]string{"trial": "Pooled analysis"},
			"quote":  "Across ten randomised trials enrolling 599 participants"}},
	}, nil)
	if !res.IsError {
		t.Fatal("rows were accepted into a session with no schema")
	}
	if !strings.Contains(errText(res), "session_open") {
		t.Errorf("the refusal does not say where a schema comes from: %s", errText(res))
	}
}

// TestAnInvalidSchemaIsRefusedBeforeTheSessionExists.
//
// A schema with no key field cannot be merged, so a session opened with one would
// accept rows for as long as the agent cared to write them and produce nothing.
func TestAnInvalidSchemaIsRefusedBeforeTheSessionExists(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)

	res := r.call(t, "mole.session_open", map[string]any{
		"question": "trials",
		"schema": map[string]any{"fields": []map[string]any{
			{"name": "trial", "type": "text"}}},
	}, nil)
	if !res.IsError {
		t.Fatal("a keyless schema opened a session")
	}
}

// TestTheDatasetMergesRowsFromDifferentDocuments.
//
// The reason mole.dataset exists rather than the agent keeping its own list: one
// entity described by two sources is one row, and de-duplicating by eye is not a
// measured operation.
func TestTheDatasetMergesRowsFromDifferentDocuments(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	addRow(t, r, sess, doc, map[string]string{"trial": "Pooled analysis", "participants": "599"},
		"Across ten randomised trials enrolling 599 participants")
	addRow(t, r, sess, doc, map[string]string{"trial": "pooled analysis", "finding": "glucose fell"},
		"reduced fasting glucose in adults with prediabetes")

	var out struct {
		Table     string `json:"table"`
		Extracted int    `json:"extracted"`
		Merged    int    `json:"merged"`
		Rows      []struct {
			Members int `json:"members"`
		} `json:"rows"`
	}
	res := r.call(t, "mole.dataset", map[string]any{"session_id": sess}, &out)
	if res.IsError {
		t.Fatalf("dataset refused: %s", errText(res))
	}
	if out.Extracted != 2 {
		t.Fatalf("extracted = %d, want 2", out.Extracted)
	}
	if out.Merged != 1 {
		t.Fatalf("merged = %d, want the two spellings folded into one row", out.Merged)
	}
	if out.Rows[0].Members != 2 {
		t.Errorf("members = %d, want both rows counted", out.Rows[0].Members)
	}
	if !strings.Contains(out.Table, "participants") {
		t.Errorf("the table does not render the schema's columns:\n%s", out.Table)
	}
}

// TestSourcesDisagreeingIsReportedRatherThanResolved.
//
// §11's rule with a column header: two sources giving one entity two values keep
// both. Picking whichever arrived first is the mistake the merge exists not to
// make, and an agent that is not told about the conflict writes prose asserting one
// of them.
func TestSourcesDisagreeingIsReportedRatherThanResolved(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	addRow(t, r, sess, doc, map[string]string{"trial": "Pooled analysis", "participants": "599"},
		"Across ten randomised trials enrolling 599 participants")
	addRow(t, r, sess, doc, map[string]string{"trial": "Pooled analysis", "participants": "10"},
		"Two trials reported a reduction of roughly 0.2 percentage points")

	var out struct {
		Contested int    `json:"contested"`
		Note      string `json:"note"`
		Rows      []struct {
			Cells map[string]struct {
				Text   string `json:"text"`
				Others []struct {
					Text string `json:"text"`
				} `json:"others"`
			} `json:"cells"`
		} `json:"rows"`
	}
	r.call(t, "mole.dataset", map[string]any{"session_id": sess}, &out)

	if out.Contested != 1 {
		t.Fatalf("contested = %d, want the disagreement reported", out.Contested)
	}
	if len(out.Rows[0].Cells["participants"].Others) == 0 {
		t.Error("the losing value was discarded rather than kept beside the winner")
	}
	if !strings.Contains(out.Note, "not resolved") {
		t.Errorf("the note does not warn the agent off picking one: %q", out.Note)
	}
}

// TestAToolkitDatasetIsScoredByEval.
//
// The point of the slice, as with slice 4's edges: an agent-built dataset has to
// land where mole's own measurements read it, or the mode is unmeasurable. Row
// integrity is the metric that matters here — it counts rows without a verbatim
// quote, and a non-zero value is a hard regression rather than a statistic.
func TestAToolkitDatasetIsScoredByEval(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	addRow(t, r, sess, doc, map[string]string{"trial": "Pooled analysis", "participants": "599"},
		"Across ten randomised trials enrolling 599 participants")

	card, err := eval.Score(context.Background(), r.db, sess, eval.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range card.Metrics {
		if m.Name != "dataset row integrity" {
			continue
		}
		found = true
		if m.Value != 100 {
			t.Errorf("row integrity = %v%% (%s); every toolkit row is quote-checked, "+
				"so anything below 100 means rows entered another way", m.Value, m.Detail)
		}
		if m.Regression {
			t.Errorf("eval reports a regression on a clean dataset: %s", m.Detail)
		}
	}
	if !found {
		t.Fatal("eval did not measure the dataset at all; it does not see a toolkit session")
	}
}

// TestMoleDatasetRendersAToolkitSession.
//
// `mole dataset` is a different program from the tool above, and the promise of
// the mode is that an agent-built dataset is a mole dataset — exportable, with the
// provenance columns, without the agent being involved. This calls what the CLI
// calls (store.LoadDataset, then WriteCSV) rather than shelling out, which is the
// most of that command reachable from here.
func TestMoleDatasetRendersAToolkitSession(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	addRow(t, r, sess, doc, map[string]string{"trial": "Pooled analysis", "participants": "599"},
		"Across ten randomised trials enrolling 599 participants")

	d, err := store.LoadDataset(context.Background(), r.db, sess, dataset.Options{})
	if err != nil {
		t.Fatalf("mole dataset would refuse this session: %v", err)
	}
	var buf bytes.Buffer
	if err := d.WriteCSV(&buf, true); err != nil {
		t.Fatal(err)
	}
	csv := buf.String()
	for _, want := range []string{"trial", "participants", "599", "sources", "quote"} {
		if !strings.Contains(csv, want) {
			t.Errorf("the exported CSV has no %q:\n%s", want, csv)
		}
	}
}
