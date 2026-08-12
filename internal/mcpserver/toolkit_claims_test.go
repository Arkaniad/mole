package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// Toolkit mode, slice 2: the claim a model cannot fabricate.
//
// One rule, and every test here is a way of trying to get around it. A quote is
// checked against the document MOLE stored, never against text the caller supplies,
// because a model that can invent a quote can invent the passage to match it.

// openWithDoc opens a session and fetches the fixture, returning both ids.
func openWithDoc(t *testing.T, r *rig) (session, doc string) {
	t.Helper()
	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "fasting"}, &opened)

	var fetched struct {
		DocID string `json:"doc_id"`
	}
	r.call(t, "mole.fetch", map[string]any{
		"session_id": opened.SessionID, "url": r.pageURL}, &fetched)
	if opened.SessionID == "" || fetched.DocID == "" {
		t.Fatalf("setup failed: session=%q doc=%q", opened.SessionID, fetched.DocID)
	}
	return opened.SessionID, fetched.DocID
}

// A span that really is in the fixture, long enough to be evidence.
const realQuote = "intermittent fasting reduced fasting glucose in adults with prediabetes"

// storedText is mole's own copy of a document, which is what an offset indexes
// into. Read from the store rather than from the tool result: the tool returns
// the text fenced, and the fence is presentation.
func storedText(t *testing.T, r *rig, docID string) string {
	t.Helper()
	var doc core.Document
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var ok bool
		var err error
		doc, ok, err = q.Document(ctx, docID, timeNow())
		if err == nil && !ok {
			t.Fatalf("no stored document %s", docID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return doc.Text
}

// TestAVerifiedClaimIsRecordedWithItsProvenance.
func TestAVerifiedClaimIsRecordedWithItsProvenance(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	var added struct {
		ClaimID string `json:"claim_id"`
		Source  string `json:"source"`
		Offset  int    `json:"offset"`
	}
	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text":  "Intermittent fasting reduced fasting glucose in adults with prediabetes.",
		"quote": realQuote,
	}, &added)
	if res.IsError {
		t.Fatalf("a true claim was refused: %s", errText(res))
	}
	if added.ClaimID == "" {
		t.Fatal("no claim id")
	}
	if added.Source == "" {
		t.Error("the claim carries no source")
	}
	// The offset has to LOCATE the quote, not merely be non-zero — an auditor
	// re-reading the page jumps to it. Checked on the STORED claim rather than on
	// the returned one: the returned offset is computed on the spot, so a wrong
	// value written to the row would not show up here. Mutating the stored offset
	// to a constant left the first version of this assertion green.
	text := storedText(t, r, doc)
	stored := storedClaims(t, r, sess)
	if len(stored) != 1 {
		t.Fatalf("%d claims stored, want 1", len(stored))
	}
	off := int(stored[0].QuoteOffset)
	if end := off + len(stored[0].Quote); off < 0 || end > len(text) ||
		!strings.EqualFold(text[off:end], stored[0].Quote) {
		t.Errorf("stored offset %d does not point at the quote; text there is %.80q",
			off, safeSlice(text, off, 80))
	}
	if added.Offset != off {
		t.Errorf("the offset reported to the caller (%d) is not the one recorded (%d)",
			added.Offset, off)
	}

	var listed struct {
		Claims []struct {
			Quote  string `json:"quote"`
			Source string `json:"source"`
		} `json:"claims"`
	}
	r.call(t, "mole.claims_list", map[string]any{"session_id": sess}, &listed)
	if len(listed.Claims) != 1 {
		t.Fatalf("%d claims listed, want 1", len(listed.Claims))
	}
	if !strings.Contains(listed.Claims[0].Quote, "reduced fasting glucose") {
		t.Errorf("stored quote = %q", listed.Claims[0].Quote)
	}
}

// TestAFabricatedQuoteIsRefused. The reason the mode exists.
func TestAFabricatedQuoteIsRefused(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text": "Intermittent fasting cures type 2 diabetes in ninety percent of cases.",
		"quote": "fasting cured type 2 diabetes in ninety percent of participants " +
			"across every trial reviewed",
	}, nil)
	if !res.IsError {
		t.Fatal("a fabricated quote was recorded; the guarantee does not hold")
	}
	if !strings.Contains(errText(res), "does not appear in the stored document") {
		t.Errorf("the refusal does not say why: %s", errText(res))
	}

	var listed struct {
		Total int `json:"total"`
	}
	r.call(t, "mole.claims_list", map[string]any{"session_id": sess}, &listed)
	if listed.Total != 0 {
		t.Errorf("%d claim(s) stored after a refusal", listed.Total)
	}
}

// TestSupplyingTheSourceTextDoesNotHelp.
//
// The attack the design exists to stop: a caller that passes its own "document"
// alongside the quote. There is no parameter for it, and adding one would make
// verification theatre.
//
// The first version of this test sent extra keys ("document", "source_text") and
// asserted only that the call errored. It passed for the wrong reason — the MCP
// schema rejects unknown properties, so mole's code never ran, and the test
// stayed green when the handler was mutated to verify against caller-supplied
// text. It now sends only real parameters and asserts on the refusal mole itself
// produces. Same defect the authors had already fixed once in
// TestFetchNeedsASession.
func TestSupplyingTheSourceTextDoesNotHelp(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	// The quote is smuggled inside the claim text, which is the only free-text
	// field there is. If verification ever read anything but the stored document,
	// this would be the way in.
	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text": "The study found that a ninety percent remission rate was observed " +
			"in the treatment arm.",
		"quote": "a ninety percent remission rate was observed in the treatment arm",
	}, nil)
	if !res.IsError {
		t.Fatal("caller-supplied text was accepted as evidence")
	}
	if !strings.Contains(errText(res), "stored document") {
		t.Errorf("the refusal did not come from the quote check: %s", errText(res))
	}
}

// TestAShortQuoteIsRefused. A three-word span matches almost any document by
// chance, so accepting one would make verification theatre.
func TestAShortQuoteIsRefused(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text": "Fasting was studied.", "quote": "fasting",
	}, nil)
	if !res.IsError {
		t.Fatal("a one-word quote was accepted as evidence")
	}
	if !strings.Contains(errText(res), "24 characters") {
		t.Errorf("the refusal does not state the rule: %s", errText(res))
	}
}

// TestAClaimNeedsAnAssertion, not just a quote. A claim that is only a quote makes
// the graph a pile of spans nobody can compare.
func TestAClaimNeedsAnAssertion(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc, "text": "  ", "quote": realQuote,
	}, nil)
	if !res.IsError {
		t.Fatal("a claim with no assertion was accepted")
	}
}

// TestADocumentFromAnotherSessionCannotBeCited.
//
// Otherwise a claim's provenance points at text this session never read.
func TestADocumentFromAnotherSessionCannotBeCited(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	_, docA := openWithDoc(t, r)
	sessB, _ := openWithDoc(t, r)

	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sessB, "doc_id": docA,
		"text": "A claim citing another session's document.", "quote": realQuote,
	}, nil)
	if !res.IsError {
		t.Fatal("a document from another session was cited")
	}
	if !strings.Contains(errText(res), "another session") {
		t.Errorf("the refusal does not say why: %s", errText(res))
	}
}

// TestVerifyQuoteAgreesWithClaimAdd, or the pre-check is a trap: a caller told a
// quote is fine and then refused would have no way to proceed.
func TestVerifyQuoteAgreesWithClaimAdd(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	for _, tc := range []struct {
		name  string
		quote string
		want  bool
	}{
		{"verbatim", realQuote, true},
		{"invented", "fasting cured every participant within eight weeks of starting", false},
		{"too short", "fasting", false},
	} {
		var checked struct {
			Found bool `json:"found"`
		}
		r.call(t, "mole.verify_quote", map[string]any{
			"session_id": sess, "doc_id": doc, "quote": tc.quote}, &checked)

		res := r.call(t, "mole.claim_add", map[string]any{
			"session_id": sess, "doc_id": doc,
			"text": "An assertion about fasting and glucose.", "quote": tc.quote}, nil)
		accepted := !res.IsError

		if checked.Found != tc.want || accepted != tc.want {
			t.Errorf("%s: verify_quote=%v claim_add=%v, want both %v",
				tc.name, checked.Found, accepted, tc.want)
		}
	}
}

// TestCitationsNumberSourcesForProse, which is what an agent writing an answer
// needs from mole rather than a list of claim ids.
func TestCitationsNumberSourcesForProse(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text": "Fasting reduced fasting glucose.", "quote": realQuote}, nil)
	r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text":  "Adherence was the most commonly reported limitation.",
		"quote": "Adherence was the most commonly reported limitation"}, nil)

	var cits struct {
		Citations []struct {
			N      int      `json:"n"`
			Source string   `json:"source"`
			Quotes []string `json:"quotes"`
		} `json:"citations"`
		Note string `json:"note"`
	}
	r.call(t, "mole.citations", map[string]any{"session_id": sess}, &cits)

	if len(cits.Citations) != 1 {
		t.Fatalf("%d citations, want 1 — both claims came from one source", len(cits.Citations))
	}
	if cits.Citations[0].N != 1 {
		t.Errorf("numbering starts at %d", cits.Citations[0].N)
	}
	if len(cits.Citations[0].Quotes) != 2 {
		t.Errorf("%d quotes under the source, want 2", len(cits.Citations[0].Quotes))
	}
}

// TestAnExpiredDocumentCannotBeCited, so retention is not quietly undone by a
// claim recorded against text that is supposed to be gone.
func TestAnExpiredDocumentCannotBeCited(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)
	expireDocument(t, r, doc)

	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text": "A claim against expired text.", "quote": realQuote}, nil)
	if !res.IsError {
		t.Fatal("a claim was verified against a document past its retention")
	}
	if !strings.Contains(errText(res), "retention") {
		t.Errorf("the refusal does not explain: %s", errText(res))
	}
}

// safeSlice is a bounded window into a document, for failure messages.
func safeSlice(s string, from, n int) string {
	if from < 0 || from >= len(s) {
		return ""
	}
	if from+n > len(s) {
		n = len(s) - from
	}
	return s[from : from+n]
}

// storedClaims reads a session's claims straight from the database.
func storedClaims(t *testing.T, r *rig, sessionID string) []*core.Claim {
	t.Helper()
	var out []*core.Claim
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		// Not 0: the store reads that as 500, which is the trap this helper's
		// callers are testing for.
		out, err = q.ListClaims(ctx, sessionID, 100_000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
