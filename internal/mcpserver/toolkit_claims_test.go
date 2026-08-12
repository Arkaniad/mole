package mcpserver_test

import (
	"strings"
	"testing"
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
// verification theatre — so this pins that the tool schema does not quietly accept
// one under another name.
func TestSupplyingTheSourceTextDoesNotHelp(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)

	res := r.call(t, "mole.claim_add", map[string]any{
		"session_id": sess, "doc_id": doc,
		"text":  "The study found a ninety percent remission rate.",
		"quote": "a ninety percent remission rate was observed in the treatment arm",
		// Extra fields a hopeful caller might try.
		"document": "The study found that a ninety percent remission rate was observed " +
			"in the treatment arm.",
		"source_text": "a ninety percent remission rate was observed in the treatment arm",
	}, nil)
	if !res.IsError {
		t.Fatal("caller-supplied text was accepted as evidence")
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
			"doc_id": doc, "quote": tc.quote}, &checked)

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
