package session_test

import (
	"context"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/lajosdeme/mole/internal/store"
)

// M9 review, second pass.
//
// Rows are persisted per lead on an uncancellable context specifically so a killed
// dataset session keeps what it paid for. The schema was written only at the very
// end of the run, in finishDataset — so killing the process after the first lead
// left rows nobody could read: `mole dataset` refuses a session with no stored
// schema, and the message tells the user the wrong thing ("not a dataset session").
// The uncancellable write was defeated by a missing UPDATE.

func datasetSpec(t *testing.T) session.Spec {
	t.Helper()
	schema, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Question = "company revenues"
	spec.Mode = core.ModeDataset
	spec.Schema = &schema
	return spec
}

// TestTheDatasetSchemaIsStoredAtCreation.
func TestTheDatasetSchemaIsStoredAtCreation(t *testing.T) {
	r, db := newRunner(t)
	ctx := context.Background()

	sess, err := r.Create(ctx, datasetSpec(t))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No Run, deliberately: this is the state a killed process leaves.
	var (
		schema dataset.Schema
		ok     bool
	)
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		schema, ok, err = q.DatasetSchema(ctx, sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a dataset session that was killed before finishing reads back as a " +
			"report session; its rows are unreachable")
	}
	if len(schema.Fields) != 2 || schema.Keys()[0] != "company" {
		t.Errorf("schema = %+v, want the two fields it was created with", schema)
	}
}

// TestTheRowsOfAKilledDatasetSessionStillAssemble, which is the point of storing
// the schema early: the rows are readable through the same path `mole dataset`
// uses.
func TestTheRowsOfAKilledDatasetSessionStillAssemble(t *testing.T) {
	r, db := newRunner(t)
	ctx := context.Background()

	sess, err := r.Create(ctx, datasetSpec(t))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertRows(ctx, sess.ID, []dataset.Row{
			{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
				Source: "https://a.example", Quote: "Acme Ltd reported revenue of $1.2m"},
		})
	}); err != nil {
		t.Fatal(err)
	}

	d, err := store.LoadDataset(ctx, db, sess.ID, dataset.Options{})
	if err != nil {
		t.Fatalf("the dataset of a killed session cannot be assembled: %v", err)
	}
	if len(d.Rows) != 1 || d.Rows[0].Get("company") != "Acme Ltd" {
		t.Errorf("rows = %+v, want the one row that was written", d.Rows)
	}
}

// TestAReportSessionStoresNoSchema, so `mole dataset` can still tell the user they
// ran the wrong command.
func TestAReportSessionStoresNoSchema(t *testing.T) {
	r, db := newRunner(t)
	ctx := context.Background()

	sess, err := r.Create(ctx, testSpec())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.LoadDataset(ctx, db, sess.ID, dataset.Options{}); err != store.ErrNotDataset {
		t.Errorf("err = %v, want ErrNotDataset", err)
	}
}
