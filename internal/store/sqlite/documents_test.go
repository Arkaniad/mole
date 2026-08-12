package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// Toolkit mode's document store (slice 0).
//
// mole discards source text everywhere else. This table exists so a quote can be
// checked against text MOLE fetched rather than text a caller supplied — and since
// it is the first table holding third-party content, its retention rule is part of
// the feature rather than an afterthought.

func putDoc(t *testing.T, db interface {
	WithTx(context.Context, func(context.Context, store.Tx) error) error
}, d core.Document) {
	t.Helper()
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertDocument(ctx, d)
	}); err != nil {
		t.Fatal(err)
	}
}

func getDoc(t *testing.T, db interface {
	Read(context.Context, func(context.Context, store.Queries) error) error
}, id string, now time.Time) (core.Document, bool) {
	t.Helper()
	var (
		d  core.Document
		ok bool
	)
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		d, ok, err = q.Document(ctx, id, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return d, ok
}

// TestADocumentRoundTripsExactly.
//
// "Exactly" is the whole point: a quote is verified against this text and an offset
// is stored into it, so any normalisation on the way in or out makes stored
// provenance point at the wrong span.
func TestADocumentRoundTripsExactly(t *testing.T) {
	db, path := open(t)
	insertSession(t, db, "s_doc", 1000)

	text := "Line one.\r\n\tIndented — with an em dash, a \"quote\", and a trailing space. \n\nEnd."
	at := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	putDoc(t, db, core.Document{
		ID: "doc_1", SessionID: "s_doc", URL: "https://a.example/x",
		Title: "A page", Text: text, Truncated: true,
		FetchedAt: at, ExpiresAt: at.Add(core.DefaultDocumentTTL),
	})

	got, ok := getDoc(t, db, "doc_1", at)
	if !ok {
		t.Fatal("document not found")
	}
	if got.Text != text {
		t.Errorf("text changed in storage:\n stored %q\n got    %q", text, got.Text)
	}
	if !got.Truncated {
		t.Error("truncated flag lost")
	}
	if !got.FetchedAt.Equal(at) {
		t.Errorf("fetched_at = %v, want %v", got.FetchedAt, at)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var typ string
	if err := raw.QueryRow(`SELECT typeof(fetched_at) FROM documents`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" {
		t.Errorf("typeof(fetched_at) = %q, want integer per 0001_init's invariant", typ)
	}
}

// TestAnExpiredDocumentIsGoneOnRead, not merely eventually swept.
//
// A retention promise that depends on a background job having run is not a
// retention promise. Between sweeps, an expired document must already be
// unreadable — otherwise a quote could be verified against text the user was told
// had been deleted.
func TestAnExpiredDocumentIsGoneOnRead(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_ttl", 1000)

	at := time.Now().UTC().Add(-8 * 24 * time.Hour)
	putDoc(t, db, core.Document{
		ID: "doc_old", SessionID: "s_ttl", URL: "https://a.example/x", Text: "stale",
		FetchedAt: at, ExpiresAt: at.Add(core.DefaultDocumentTTL),
	})

	if _, ok := getDoc(t, db, "doc_old", time.Now().UTC()); ok {
		t.Error("an expired document was readable; the TTL is enforced only by the sweep")
	}
	// And it is readable before expiry, or the test above proves nothing.
	if _, ok := getDoc(t, db, "doc_old", at.Add(time.Hour)); !ok {
		t.Error("the document was unreadable before its TTL; the fixture is wrong")
	}
}

// TestTheSweepReclaimsExpiredDocuments. Reads already hide them; this is about
// disk, and about not accumulating a corpus of other people's pages.
func TestTheSweepReclaimsExpiredDocuments(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_sweep", 1000)

	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	putDoc(t, db, core.Document{ID: "doc_old", SessionID: "s_sweep",
		URL: "https://a.example/1", Text: "stale", FetchedAt: old,
		ExpiresAt: old.Add(core.DefaultDocumentTTL)})
	putDoc(t, db, core.Document{ID: "doc_new", SessionID: "s_sweep",
		URL: "https://a.example/2", Text: "fresh", FetchedAt: now,
		ExpiresAt: now.Add(core.DefaultDocumentTTL)})

	var purged int64
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		var err error
		purged, err = tx.PurgeExpiredDocuments(ctx, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if purged != 1 {
		t.Errorf("purged %d, want 1", purged)
	}
	if _, ok := getDoc(t, db, "doc_new", now); !ok {
		t.Error("the sweep took a document that had not expired")
	}
}

// TestDeletingASessionTakesItsDocuments.
//
// The other half of retention, and it depends on foreign keys actually being
// enforced — a pragma that is easy to assume and silently absent, in which case
// deleting a session would leave its pages on disk forever.
func TestDeletingASessionTakesItsDocuments(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_cascade", 1000)

	now := time.Now().UTC()
	putDoc(t, db, core.Document{ID: "doc_c", SessionID: "s_cascade",
		URL: "https://a.example/x", Text: "body", FetchedAt: now,
		ExpiresAt: now.Add(core.DefaultDocumentTTL)})

	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.DeleteSession(ctx, "s_cascade")
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := getDoc(t, db, "doc_c", now); ok {
		t.Error("a deleted session left its documents behind; ON DELETE CASCADE is not " +
			"in force")
	}
}

// TestADocumentNeedsASourceURL. A stored page nobody can attribute is not
// provenance, and an empty URL would produce a citation pointing nowhere.
func TestADocumentNeedsASourceURL(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_nourl", 1000)

	now := time.Now().UTC()
	err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertDocument(ctx, core.Document{
			ID: "doc_bad", SessionID: "s_nourl", URL: "", Text: "body",
			FetchedAt: now, ExpiresAt: now.Add(core.DefaultDocumentTTL)})
	})
	if err == nil {
		t.Fatal("a document with no URL was accepted")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("err = %v, want the schema's own check", err)
	}
}

// TestTheTTLDefaultsWhenTheCallerGivesNone, so a caller cannot store text with no
// expiry by omission.
func TestTheTTLDefaultsWhenTheCallerGivesNone(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_default", 1000)

	putDoc(t, db, core.Document{ID: "doc_d", SessionID: "s_default",
		URL: "https://a.example/x", Text: "body"})

	got, ok := getDoc(t, db, "doc_d", time.Now().UTC())
	if !ok {
		t.Fatal("not found")
	}
	if got.ExpiresAt.IsZero() {
		t.Fatal("stored with no expiry at all")
	}
	if d := got.ExpiresAt.Sub(got.FetchedAt); d < core.DefaultDocumentTTL-time.Minute ||
		d > core.DefaultDocumentTTL+time.Minute {
		t.Errorf("TTL = %v, want %v", d, core.DefaultDocumentTTL)
	}
}
