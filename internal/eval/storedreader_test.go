package eval_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/eval"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// Citation accuracy over a toolkit session.
//
// The metric re-fetched every source, which measured the page's stability rather
// than the citation: a source that has since changed or 404'd reported drift or
// "unreachable" while mole was holding, on disk, the exact bytes the quote had
// been verified against.

type deadNetwork struct{ calls int }

func (d *deadNetwork) Text(context.Context, string) (string, error) {
	d.calls++
	return "", errors.New("the page is gone")
}

func storedSession(t *testing.T, text string) (store.Store, string) {
	t.Helper()
	db := openTestDB(t)
	sess := &core.Session{
		ID: core.NewSessionID(), Prompt: "fasting", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetUSD, Budget: 1000, Status: core.StatusDone,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertSession(ctx, sess); err != nil {
			return err
		}
		return tx.InsertDocument(ctx, core.Document{
			ID: core.NewDocumentID(), SessionID: sess.ID,
			URL: "https://example.org/review", Text: text,
			FetchedAt: time.Now().UTC(),
			ExpiresAt: time.Now().UTC().Add(core.DefaultDocumentTTL),
		})
	}); err != nil {
		t.Fatal(err)
	}
	return db, sess.ID
}

func openTestDB(t *testing.T) store.Store {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestAStoredSourceIsReadWithoutTouchingTheNetwork.
func TestAStoredSourceIsReadWithoutTouchingTheNetwork(t *testing.T) {
	const page = "Fasting reduced fasting glucose in adults with prediabetes."
	db, sess := storedSession(t, page)
	net := &deadNetwork{}

	r := eval.NewStoredReader(db, sess, net)
	got, err := r.Text(context.Background(), "https://example.org/review")
	if err != nil {
		t.Fatalf("a stored source was unreadable: %v", err)
	}
	if !strings.Contains(got, "prediabetes") {
		t.Errorf("text = %q, want the stored copy", got)
	}
	if net.calls != 0 {
		t.Errorf("re-fetched %d time(s) for a source mole already holds", net.calls)
	}
}

// TestASourceWithNoStoredCopyFallsBackToTheNetwork, which is every autonomous
// session and any toolkit session past its retention window.
func TestASourceWithNoStoredCopyFallsBackToTheNetwork(t *testing.T) {
	db, sess := storedSession(t, "irrelevant")
	net := &deadNetwork{}

	r := eval.NewStoredReader(db, sess, net)
	if _, err := r.Text(context.Background(), "https://elsewhere.example/x"); err == nil {
		t.Fatal("an unstored source read successfully from nowhere")
	}
	if net.calls != 1 {
		t.Errorf("network reads = %d, want 1", net.calls)
	}
}

// TestAnExpiredStoredCopyIsNotUsed, since retention means the text is gone even
// when the row has not been swept yet.
func TestAnExpiredStoredCopyIsNotUsed(t *testing.T) {
	db, sess := storedSession(t, "Fasting reduced fasting glucose.")
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		docs, err := tx.DocumentsForSession(ctx, sess, time.Now().UTC())
		if err != nil {
			return err
		}
		return tx.ExpireDocumentForTest(ctx, docs[0].ID)
	}); err != nil {
		t.Fatal(err)
	}

	net := &deadNetwork{}
	r := eval.NewStoredReader(db, sess, net)
	if _, err := r.Text(context.Background(), "https://example.org/review"); err == nil {
		t.Fatal("expired text was served to the citation check")
	}
	if net.calls != 1 {
		t.Errorf("network reads = %d; an expired copy should fall through", net.calls)
	}
}
