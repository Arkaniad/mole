package mcpserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	. "github.com/lajosdeme/mole/internal/mcpserver"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/lajosdeme/mole/internal/store"
)

// Retention, publication dates and output bounds — the review findings about what
// the document store keeps and what it hands back.

// TestExpiredSourceTextIsActuallyDeleted.
//
// Reads refused expired text from the start, so a quote could not be verified
// against it. Nothing deleted the rows: PurgeExpiredDocuments and DeleteSession
// had no caller outside their own tests, and the migration's promise of "two
// mechanisms" was a promise neither of them kept. A machine running toolkit mode
// kept every page it had ever read, at up to 120,000 characters each.
func TestExpiredSourceTextIsActuallyDeleted(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openWithDoc(t, r)
	expireDocument(t, r, doc)

	runner := &session.Runner{Store: r.db}
	n, err := runner.PurgeDocuments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged %d documents, want 1", n)
	}

	var rows int
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		// Past the expiry filter: this asks whether the bytes are gone, not
		// whether they are readable.
		docs, err := q.DocumentsForSession(ctx, sess, time.Unix(0, 0))
		rows = len(docs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d expired document(s) still on disk", rows)
	}
}

// TestARecoveringDaemonReclaimsExpiredText, since the sweep only counts if
// something runs it.
func TestARecoveringDaemonReclaimsExpiredText(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	_, doc := openWithDoc(t, r)
	expireDocument(t, r, doc)

	rc := (&session.Runner{Store: r.db}).Recover(context.Background())
	if rc.Documents != 1 {
		t.Errorf("boot recovery purged %d documents, want 1", rc.Documents)
	}
	if !rc.Any() {
		t.Error("a recovery that reclaimed disk reports nothing happened")
	}
}

// TestAPublicationDateSurvivesFetch.
//
// The extractor recovers the date and the toolkit threw it away, so no toolkit
// session could produce a supersedes edge — §11's staleness rule needs a date on
// both claims — and `mole eval` advised the user to check whether the sources
// carried dates at all, which they had.
func TestAPublicationDateSurvivesFetch(t *testing.T) {
	page := strings.Replace(testPage, "<head>",
		`<head><meta property="article:published_time" content="2019-04-05T00:00:00Z">`, 1)
	r := connectToolkitStubFetch(t, page)

	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "fasting"}, &opened)

	var fetched struct {
		DocID       string `json:"doc_id"`
		PublishedAt string `json:"published_at"`
	}
	r.call(t, "mole.fetch", map[string]any{
		"session_id": opened.SessionID, "url": r.pageURL}, &fetched)
	if !strings.HasPrefix(fetched.PublishedAt, "2019") {
		t.Errorf("published_at = %q, want the date the page states", fetched.PublishedAt)
	}

	r.call(t, "mole.claim_add", map[string]any{
		"session_id": opened.SessionID, "doc_id": fetched.DocID,
		"text": "Fasting reduced fasting glucose.", "quote": realQuote}, nil)

	claims := storedClaims(t, r, opened.SessionID)
	if len(claims) != 1 {
		t.Fatalf("%d claims, want 1", len(claims))
	}
	if claims[0].PublishedAt == nil || claims[0].PublishedAt.Year() != 2019 {
		t.Errorf("the claim carries %v; the staleness rule has nothing to compare",
			claims[0].PublishedAt)
	}
}

// TestAPageWithNoDateClaimsNone, because "unstated" and "published at the epoch"
// are different facts and the staleness rule acts on the difference.
func TestAPageWithNoDateClaimsNone(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, docID := openWithDoc(t, r)

	var doc core.Document
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		var ok bool
		doc, ok, err = q.Document(ctx, docID, timeNow())
		if err == nil && !ok {
			t.Fatal("no document")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !doc.PublishedAt.IsZero() {
		t.Errorf("published_at = %v for a page that states none", doc.PublishedAt)
	}
	_ = sess
}

// TestTheDatasetToolBoundsWhatItReturns.
//
// Every other list tool caps. This one returned the whole merged table twice —
// markdown and JSON — however large the session had grown.
func TestTheDatasetToolBoundsWhatItReturns(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage)
	sess, doc := openDataset(t, r)

	text := storedText(t, r, doc)
	const window = 40
	rows := make([]map[string]any, 0, MaxDatasetRows+20)
	for i := 0; i < MaxDatasetRows+20; i++ {
		start := (i * 3) % (len(text) - window)
		rows = append(rows, map[string]any{
			"values": map[string]string{"trial": fmt.Sprintf("Trial %03d", i)},
			"quote":  text[start : start+window],
		})
	}
	// In batches: one call is capped at MaxRowsPerCall, which is a different
	// bound from the one under test here.
	accepted := 0
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		var added struct {
			Accepted int `json:"accepted"`
		}
		res := r.call(t, "mole.rows_add", map[string]any{
			"session_id": sess, "doc_id": doc, "rows": rows[start:end]}, &added)
		if res.IsError {
			t.Fatalf("rows_add refused: %s", errText(res))
		}
		accepted += added.Accepted
	}
	if accepted <= MaxDatasetRows {
		t.Fatalf("only %d rows accepted; the fixture cannot exercise the bound", accepted)
	}

	var out struct {
		Rows      []map[string]any `json:"rows"`
		Merged    int              `json:"merged"`
		Truncated bool             `json:"truncated"`
		Note      string           `json:"note"`
		Table     string           `json:"table"`
	}
	r.call(t, "mole.dataset", map[string]any{"session_id": sess}, &out)

	if len(out.Rows) > MaxDatasetRows {
		t.Errorf("returned %d rows, limit %d", len(out.Rows), MaxDatasetRows)
	}
	if out.Merged <= MaxDatasetRows {
		t.Errorf("merged = %d; the total should still report the whole table", out.Merged)
	}
	if !out.Truncated || !strings.Contains(out.Note, "mole dataset") {
		t.Errorf("a truncated table does not say so or where the rest is: %q", out.Note)
	}
	if strings.Count(out.Table, "\n") > MaxDatasetRows+10 {
		t.Error("the markdown table was not truncated with the rows")
	}
}
