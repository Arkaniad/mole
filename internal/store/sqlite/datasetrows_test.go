package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// M9 review, second pass.
//
// Two things migration 0009 got wrong that only a probe of the stored bytes shows:
// a timestamp that was not in this database's timestamp representation, and a
// mandatory column that admitted the empty string.

func rowFor(source, quote string, at time.Time) dataset.Row {
	return dataset.Row{
		Values:      map[string]string{"company": "Acme Ltd"},
		Source:      source,
		Quote:       quote,
		QuoteOffset: 512,
		RetrievedAt: at,
	}
}

func insertRows(t *testing.T, db *sqlite.DB, sessionID string, rows []dataset.Row) error {
	t.Helper()
	return db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertRows(ctx, sessionID, rows)
	})
}

// TestARowTimeIsStoredAsUnixMicros.
//
// 0001_init's first stated invariant is "Timestamps are INTEGER unix microseconds
// (UTC). Sortable, no parsing." dataset_rows declared retrieved_at as TIMESTAMP
// and the driver wrote formatted text, making this the one table in the database
// whose times neither sort nor compare against any other table's. The test reads
// SQLite's own typeof(), because the round trip passed either way — which is
// exactly why the mistake survived review.
func TestARowTimeIsStoredAsUnixMicros(t *testing.T) {
	db, path := open(t)
	insertSession(t, db, "s_rows", 1000)

	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := insertRows(t, db, "s_rows", []dataset.Row{
		rowFor("https://a.example", "Acme Ltd exists", at),
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	var typ string
	var stored int64
	if err := raw.QueryRow(
		`SELECT typeof(retrieved_at), retrieved_at FROM dataset_rows`).Scan(&typ, &stored); err != nil {
		t.Fatalf("read the stored column: %v", err)
	}
	if typ != "integer" {
		t.Errorf("typeof(retrieved_at) = %q, wanted integer per 0001_init's invariant", typ)
	}
	if stored != at.UnixMicro() {
		t.Errorf("stored = %d, want %d", stored, at.UnixMicro())
	}

	// And it comes back as the same instant.
	var got []dataset.Row
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		got, err = q.ListRows(ctx, "s_rows")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].RetrievedAt.Equal(at) {
		t.Errorf("round trip = %+v, want %v", got, at)
	}
}

// TestTheSchemaRefusesARowWithNoEvidence.
//
// §11.5 makes the quote mandatory, and `quote TEXT NOT NULL` admits ” — so the
// one shape the column exists to forbid was the one it allowed. The check is the
// schema stating the project's own rule rather than trusting every present and
// future writer to remember it.
func TestTheSchemaRefusesARowWithNoEvidence(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_noquote", 1000)

	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		row  dataset.Row
	}{
		{"no quote", rowFor("https://a.example", "", now)},
		{"no source", rowFor("", "Acme Ltd exists", now)},
	} {
		err := insertRows(t, db, "s_noquote", []dataset.Row{tc.row})
		if err == nil {
			t.Errorf("%s: the store accepted it; §11.5's floor is not enforced", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "CHECK") {
			t.Errorf("%s: error = %v, want the schema's own check", tc.name, err)
		}
	}
}

// TestABatchIsAllOrNothing. A dataset is a thing somebody counts, so a
// half-written batch would report a row count that never existed — and the check
// constraint above makes a mid-batch failure reachable rather than theoretical.
func TestABatchIsAllOrNothing(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_batch", 1000)

	now := time.Now().UTC()
	err := insertRows(t, db, "s_batch", []dataset.Row{
		rowFor("https://a.example", "Acme Ltd exists", now),
		rowFor("https://b.example", "", now), // refused
	})
	if err == nil {
		t.Fatal("the batch was accepted")
	}

	var got []dataset.Row
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		got, err = q.ListRows(ctx, "s_batch")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("%d row(s) survived a failed batch, want none", len(got))
	}
}
