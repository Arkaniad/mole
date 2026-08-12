package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/mcpserver"
	"github.com/lajosdeme/mole/internal/store"
)

// Toolkit mode, slice 1: session_open/close, search, fetch.
//
// The property under test throughout is that mole keeps what it needs to verify a
// claim later, and that untrusted text is labelled as untrusted on the way out.

// TestTheToolkitIsOffUnlessAskedFor.
//
// Every tool definition costs an MCP client context, and a caller who wants
// autonomous research should not have to read past four extra tools to find
// research.report.
func TestTheToolkitIsOffUnlessAskedFor(t *testing.T) {
	r := connect(t, 0)
	tools := r.tools(t)
	for _, name := range tools {
		if strings.HasPrefix(name, "mole.") {
			t.Errorf("%q is exposed without --toolkit", name)
		}
	}
	if len(tools) == 0 {
		t.Fatal("no tools at all; the fixture is wrong")
	}
}

// TestTheToolkitAppearsWhenEnabled, and the research tools stay.
func TestTheToolkitAppearsWhenEnabled(t *testing.T) {
	r := connectToolkit(t)
	tools := r.tools(t)

	for _, want := range []string{
		"mole.session_open", "mole.session_close", "mole.search", "mole.fetch",
		"mole.verify_quote", "mole.claim_add", "mole.claims_list", "mole.citations",
		"mole.connect_list", "mole.aggregate",
		"mole.pairs_candidates", "mole.edge_add",
		"mole.rows_add", "mole.dataset",
	} {
		if !contains(tools, want) {
			t.Errorf("%q is missing with --toolkit", want)
		}
	}
	if !contains(tools, "research.report") {
		t.Error("enabling the toolkit removed the autonomous tools")
	}
}

// TestAFetchedDocumentIsStoredForLaterVerification.
//
// The whole point of slice 0's table. A quote is checked against text MOLE holds,
// because verifying against text the caller supplied proves nothing.
func TestAFetchedDocumentIsStoredForLaterVerification(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)

	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "a question"}, &opened)
	if opened.SessionID == "" {
		t.Fatal("no session id")
	}

	var fetched struct {
		DocID string `json:"doc_id"`
		Text  string `json:"text"`
		Chars int    `json:"chars"`
	}
	r.call(t, "mole.fetch", map[string]any{
		"session_id": opened.SessionID, "url": r.pageURL,
	}, &fetched)

	if fetched.DocID == "" {
		t.Fatal("no doc id returned")
	}

	// Stored UNFENCED: the fence is presentation for the agent's prompt, and
	// storing the wrapper would put every offset out by its length.
	var stored string
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		doc, ok, err := q.Document(ctx, fetched.DocID, timeNow())
		if err != nil || !ok {
			t.Fatalf("stored document not readable: ok=%v err=%v", ok, err)
		}
		stored = doc.Text
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "untrusted-") {
		t.Error("the fence was stored with the text; every quote offset would be wrong")
	}
	if !strings.Contains(stored, "reduced fasting glucose") {
		t.Errorf("stored text does not look like the page: %.120q", stored)
	}
}

// TestFetchedTextComesBackFenced.
//
// mole cannot control the prompt an agent assembles, so this is the one control it
// has: the text is labelled as data, inside a per-call nonce a page cannot guess.
func TestFetchedTextComesBackFenced(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "q"}, &opened)

	var fetched struct {
		Text string `json:"text"`
		Note string `json:"note"`
	}
	r.call(t, "mole.fetch", map[string]any{
		"session_id": opened.SessionID, "url": r.pageURL,
	}, &fetched)

	if !strings.Contains(fetched.Text, "<untrusted-") ||
		!strings.Contains(fetched.Text, "</untrusted-") {
		t.Fatalf("text is not fenced:\n%.200q", fetched.Text)
	}
	if !strings.Contains(fetched.Text, "UNTRUSTED DATA") {
		t.Error("the fence does not say what the block is")
	}
	// Nothing after the closing tag: text appended past it would be read as
	// instructions again, which is the failure the fence exists to prevent.
	if idx := strings.LastIndex(fetched.Text, "</untrusted-"); idx >= 0 {
		tail := fetched.Text[idx:]
		if strings.Count(tail, "\n") > 1 {
			t.Errorf("content follows the closing fence: %q", tail)
		}
	}
	if !strings.Contains(fetched.Note, "verbatim") {
		t.Errorf("the note does not tell the caller quotes must be verbatim: %q", fetched.Note)
	}
}

// TestTwoFencesDoNotShareANonce, or a page that saw one could close the next.
func TestTwoFencesDoNotShareANonce(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "q"}, &opened)

	var a, b struct {
		Text string `json:"text"`
	}
	r.call(t, "mole.fetch", map[string]any{"session_id": opened.SessionID, "url": r.pageURL}, &a)
	r.call(t, "mole.fetch", map[string]any{"session_id": opened.SessionID, "url": r.pageURL}, &b)

	if tagOf(a.Text) == "" || tagOf(a.Text) == tagOf(b.Text) {
		t.Errorf("fence tags repeat across fetches: %q and %q", tagOf(a.Text), tagOf(b.Text))
	}
}

// TestFetchNeedsASession, because a document with no session is a document with no
// retention rule.
// TestFetchNeedsASession, because a document with no session has no retention rule
// and no owner.
//
// The session id is sent EMPTY rather than omitted. Omitting it is caught by the
// tool's own JSON schema before any of mole's code runs, so a test that omitted it
// passed with the check deleted — it was measuring the SDK. An empty string
// satisfies the schema and reaches the check that exists.
func TestFetchNeedsASession(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	res := r.call(t, "mole.fetch", map[string]any{
		"session_id": "", "url": r.pageURL}, nil)
	if !res.IsError {
		t.Fatal("a fetch with no session was accepted")
	}
	// The message has to be the actionable one, not a foreign-key violation that
	// happens to mention the column.
	if !strings.Contains(errText(res), "stored against a session") {
		t.Errorf("the refusal does not explain why a session is needed: %s", errText(res))
	}
}

// TestSessionOpenSaysWhatTheBudgetCovers.
//
// A caller who assumes mole is metering their model spend would be wrong in the
// direction that costs them money.
func TestSessionOpenSaysWhatTheBudgetCovers(t *testing.T) {
	r := connectToolkit(t)
	var opened struct {
		Note string `json:"note"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "q"}, &opened)
	if !strings.Contains(opened.Note, "cannot") || !strings.Contains(opened.Note, "model") {
		t.Errorf("note does not say the budget cannot bound the caller's model use: %q",
			opened.Note)
	}
}

// TestSessionCloseReportsWhatWasCollected.
func TestSessionCloseReportsWhatWasCollected(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "q"}, &opened)
	r.call(t, "mole.fetch", map[string]any{
		"session_id": opened.SessionID, "url": r.pageURL}, nil)

	var closed struct {
		Documents int `json:"documents"`
	}
	r.call(t, "mole.session_close", map[string]any{"session_id": opened.SessionID}, &closed)
	if closed.Documents != 1 {
		t.Errorf("documents = %d, want 1", closed.Documents)
	}
}

// testPage is deliberately long-winded. The extractor runs readability, which
// discards short blocks as boilerplate — a three-sentence fixture extracted to the
// empty string and made the storage test fail for a reason that had nothing to do
// with storage.
const testPage = `<html><head><title>A review</title></head><body><article>
<h1>Time-restricted eating and metabolic health</h1>
<p>Across ten randomised trials enrolling 599 participants, intermittent fasting
reduced fasting glucose in adults with prediabetes and type 2 diabetes. The pooled
estimate was consistent across trial designs, though the confidence intervals were
wide in the smaller studies and the follow-up rarely extended beyond six months.</p>
<p>Effects on HbA1c were inconsistent between cohorts. Two trials reported a
reduction of roughly 0.2 percentage points; three reported no measurable change at
all. Subgroup analysis suggested the difference tracked baseline glycaemic control
rather than the length of the eating window, but the analysis was not pre-registered
and should be read as hypothesis-generating rather than as a finding.</p>
<p>Adherence was the most commonly reported limitation. Participants assigned to an
eight-hour window reported more difficulty sustaining the schedule on weekends, and
several trials permitted a wider window on two days per week to keep dropout within
acceptable bounds. Whether the observed metabolic effects survive that flexibility
is not established by the evidence reviewed here.</p>
<p>Limitations include short follow-up, heterogeneous eating windows, and the
absence of blinding, which is difficult to achieve in a dietary intervention. The
authors call for longer trials with pre-registered subgroup analyses before the
approach is recommended as a first-line intervention.</p>
</article></body></html>`

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func tagOf(fenced string) string {
	i := strings.Index(fenced, "<untrusted-")
	if i < 0 {
		return ""
	}
	rest := fenced[i+len("<untrusted-"):]
	j := strings.Index(rest, ">")
	if j < 0 {
		return ""
	}
	return rest[:j]
}

var _ = mcpserver.MaxFetchChars

// TestFetchIsBehindTheEgressGuard.
//
// Discovered by the storage test failing: the guard refuses 127.0.0.1 on a high
// port, which is exactly right and matters MORE here than in autonomous mode.
// Without it `mole.fetch` would be an SSRF proxy — an agent could point it at
// localhost:8080 or a cloud metadata endpoint and read whatever answered, using
// mole's process as the fetcher.
func TestFetchIsBehindTheEgressGuard(t *testing.T) {
	r := connectToolkit(t)
	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "q"}, &opened)

	for _, target := range []string{
		r.pageURL,                        // loopback, high port
		"http://169.254.169.254/latest/", // cloud metadata
		"http://192.168.1.1/",            // private range
	} {
		res := r.call(t, "mole.fetch", map[string]any{
			"session_id": opened.SessionID, "url": target}, nil)
		if !res.IsError {
			t.Errorf("%s was fetched; mole.fetch is an SSRF proxy", target)
		}
	}
}
