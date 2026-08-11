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

// §12.1's audit trail at the storage layer (migration 0010).
//
// The table exists because a log line cannot be queried per session and rotates
// away. What is checked here is the part the Go types cannot enforce: an outcome
// the schema does not recognise, and the timestamp representation the rest of the
// database uses.

func aCrossing(sessionID string, outcome core.CrossingOutcome, at time.Time) core.Crossing {
	return core.Crossing{
		SessionID: sessionID, LeadID: "l1", Connector: "sales",
		Query:     `SELECT "region", COUNT(*) AS n FROM "t" GROUP BY 1`,
		QueryHash: "0123456789abcdef", Outcome: outcome,
		RowsDescribed: 40, Columns: 2, Buckets: 2, CreatedAt: at,
	}
}

// TestACrossingRoundTrips, with the timestamp stored the way every other table
// stores one.
func TestACrossingRoundTrips(t *testing.T) {
	db, path := open(t)
	insertSession(t, db, "s_cross", 1000)

	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertCrossings(ctx, []core.Crossing{
			aCrossing("s_cross", core.CrossingCrossed, at),
			aCrossing("s_cross", core.CrossingRefused, at.Add(time.Second)),
		})
	}); err != nil {
		t.Fatal(err)
	}

	var got []core.Crossing
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		got, err = q.ListCrossings(ctx, "s_cross")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d crossing(s), want 2", len(got))
	}
	if got[0].Outcome != core.CrossingCrossed || got[1].Outcome != core.CrossingRefused {
		t.Errorf("order or outcomes wrong: %+v", got)
	}
	if !got[0].CreatedAt.Equal(at) {
		t.Errorf("time = %v, want %v", got[0].CreatedAt, at)
	}
	if got[0].ID == "" {
		t.Error("no id was assigned")
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var typ string
	if err := raw.QueryRow(`SELECT typeof(created_at) FROM compute_crossings LIMIT 1`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" {
		t.Errorf("typeof(created_at) = %q, want integer per 0001_init's invariant", typ)
	}
}

// TestAnUnknownOutcomeIsRefusedByTheSchema.
//
// The three outcomes mean different things — crossed is data leaving, refused is
// the gate working, withheld is a bug the backstop caught — and §14.3's number is
// the count of the third. A fourth spelling arriving from a future writer would
// silently drop out of every count rather than fail.
func TestAnUnknownOutcomeIsRefusedByTheSchema(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_bad", 1000)

	err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertCrossings(ctx, []core.Crossing{
			aCrossing("s_bad", core.CrossingOutcome("leaked-a-bit"), time.Now().UTC()),
		})
	})
	if err == nil {
		t.Fatal("the store accepted an outcome nothing counts")
	}
	if !strings.Contains(err.Error(), "CHECK") {
		t.Errorf("err = %v, want the schema's own check", err)
	}
}

// TestACrossingNeedsAConnectorAndAHash. Both are what makes a row auditable: a
// crossing nobody can attribute is not a trail.
func TestACrossingNeedsAConnectorAndAHash(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_incomplete", 1000)

	for name, mutate := range map[string]func(*core.Crossing){
		"no connector": func(c *core.Crossing) { c.Connector = "" },
		"no hash":      func(c *core.Crossing) { c.QueryHash = "" },
	} {
		c := aCrossing("s_incomplete", core.CrossingCrossed, time.Now().UTC())
		mutate(&c)
		err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
			return tx.InsertCrossings(ctx, []core.Crossing{c})
		})
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestCrossingsAreScopedToTheirSession. An audit answers "what left the machine
// for THIS session", so a leak across sessions would be the wrong answer to the
// only question the table exists for.
func TestCrossingsAreScopedToTheirSession(t *testing.T) {
	db, _ := open(t)
	insertSession(t, db, "s_one", 1000)
	insertSession(t, db, "s_two", 1000)

	now := time.Now().UTC()
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertCrossings(ctx, []core.Crossing{
			aCrossing("s_one", core.CrossingCrossed, now)}); err != nil {
			return err
		}
		return tx.InsertCrossings(ctx, []core.Crossing{
			aCrossing("s_two", core.CrossingRefused, now)})
	}); err != nil {
		t.Fatal(err)
	}

	var got []core.Crossing
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		got, err = q.ListCrossings(ctx, "s_one")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Outcome != core.CrossingCrossed {
		t.Errorf("got %+v, want only this session's crossing", got)
	}
}
