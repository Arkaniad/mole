package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"

	_ "modernc.org/sqlite"
)

// M5 slice 2.
//
// Claim order decides citation numbers (output.citeFindings), which decides the
// synthesis prompt, which under MOLE_RECORD=replay IS the cassette key. A
// reordering is therefore not a cosmetic difference — it is a cassette miss, and
// a miss in replay is an error.
//
// ListClaims ordered by created_at alone, and InsertClaims stamps one timestamp
// for a whole batch, so every intra-lead comparison was a tie. SQLite broke it
// by returning rows in plan order, which for a table scan is rowid — insertion
// order. Stable in practice, unspecified in principle.

func seedClaimSession(t *testing.T, db *sqlite.DB) (sessionID, leadID string) {
	t.Helper()
	sessionID, leadID = "s_order", "l_order"
	insertSession(t, db, sessionID, 1_000_000)
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertLead(ctx, &core.Lead{
			ID: leadID, SessionID: sessionID, ActorType: core.ActorWeb,
			Query: "q", Status: core.LeadDone,
		})
	}); err != nil {
		t.Fatalf("insert lead: %v", err)
	}
	return sessionID, leadID
}

func listClaims(t *testing.T, db *sqlite.DB, sessionID string) []*core.Claim {
	t.Helper()
	var got []*core.Claim
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		got, err = q.ListClaims(ctx, sessionID, 100)
		return err
	}); err != nil {
		t.Fatalf("list claims: %v", err)
	}
	return got
}

// TestInsertClaimsRecordsBatchPosition. seq is the order the actor mined them,
// made explicit so it survives into the query.
//
// The seq assertion here is load-bearing; the ORDER of the texts is NOT. Reverting
// the query to `ORDER BY created_at` leaves this test passing, because rowid order
// and insertion order coincide — measured, not assumed. TestClaimOrderDoesNotDepend
// OnRowid below is the one that guards the ordering.
func TestInsertClaimsRecordsBatchPosition(t *testing.T) {
	db, _ := open(t)
	sid, lid := seedClaimSession(t, db)

	var batch []core.Claim
	for i := 0; i < 5; i++ {
		batch = append(batch, core.Claim{
			SessionID: sid, LeadID: lid,
			Text:   string(rune('A' + i)),
			Source: "https://example.com/a", Quote: "q",
		})
	}
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertClaims(ctx, batch)
	}); err != nil {
		t.Fatal(err)
	}

	got := listClaims(t, db, sid)
	if len(got) != 5 {
		t.Fatalf("read back %d claims, want 5", len(got))
	}
	for i, c := range got {
		if c.Seq != i {
			t.Errorf("claim %d has seq %d, want %d", i, c.Seq, i)
		}
		if want := string(rune('A' + i)); c.Text != want {
			t.Errorf("position %d is %q, want %q — insertion order was not preserved", i, c.Text, want)
		}
	}
}

// TestClaimOrderDoesNotDependOnRowid is the discriminating test.
//
// Reading claims back in insertion order proves nothing on its own: rowid order
// happens to equal insertion order, so that assertion passes with or without the
// fix. This constructs the case where the two disagree — two claims with an
// identical created_at, written so that rowid order is the REVERSE of seq order
// — and asserts the query follows seq.
//
// Written through raw SQL deliberately. InsertClaims assigns seq from the batch
// index and so cannot produce this state; the thing under test is the ORDER BY,
// and the only way to test it is to hand it rows that tell it two different
// stories.
func TestClaimOrderDoesNotDependOnRowid(t *testing.T) {
	db, path := open(t)
	sid, lid := seedClaimSession(t, db)

	// Identical timestamp: exactly what InsertClaims produces for one batch.
	shared := time.Now().UTC().UnixMicro()

	// Written first (lower rowid) but seq 1; written second but seq 0.
	// The ids are chosen so LEXICAL id order is the opposite of seq order.
	// The first version used "c_first"/"c_second", whose id order happened to
	// match seq order — so `ORDER BY created_at, id` produced the same answer and
	// the test passed with seq removed from the query. It was the file's
	// self-declared discriminating test and it discriminated nothing; measured.
	rawInsertClaim(t, path, sid, lid, "a_written_first", "second", shared, 1)
	rawInsertClaim(t, path, sid, lid, "b_written_second", "first", shared, 0)

	got := listClaims(t, db, sid)
	if len(got) != 2 {
		t.Fatalf("read back %d claims, want 2", len(got))
	}
	// rowid order is [second first]; id order is [second first] too, because
	// "a_written_first" sorts before "b_written_second". Only seq gives
	// [first second], so this can be satisfied by nothing else.
	if got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("order is [%s %s], want [first second] — the query is following "+
			"rowid or id, not seq", got[0].Text, got[1].Text)
	}
}

// rawInsertClaim writes a claim with an explicit seq, bypassing InsertClaims.
//
// A second connection to the same file rather than a hole in the store
// interface: the point is to construct a row InsertClaims cannot produce, and
// widening the production API to let a test do that would be the wrong trade.
func rawInsertClaim(t *testing.T, path, sessionID, leadID, id, text string, createdAt int64, seq int) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	_, err = raw.ExecContext(context.Background(), `
		INSERT INTO claims (id, session_id, lead_id, text, source, tool_call_id, quote,
			quote_offset, published_at, retrieved_at, root_claim_id, verify_depth,
			assertion_strength, confidence, grounded, grounding_note, verified_at,
			created_at, seq)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, sessionID, leadID, text, "https://example.com/a", "", "quote",
		0, nil, createdAt, id, 0, 0.0, 0.0, nil, "", nil, createdAt, seq)
	if err != nil {
		t.Fatalf("raw insert %s: %v", id, err)
	}
}
