package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// M9 slice 3/4, at the command level.
//
// The merge runs on READ rather than at collection time, which is the property
// worth testing here: the rows and the schema are what a session persisted, so a
// later fix to the matching rules improves every dataset already gathered rather
// than only the next one. That is only true if `mole dataset` re-derives.

func seedDataset(t *testing.T, home string) (string, *sqlite.DB) {
	t.Helper()
	path := filepath.Join(home, "mole.db")
	if err := cmdMigrate(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	db, err := openDBNoMigrate(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	schema, err := dataset.ParseSpec("company:text!,revenue:number")
	if err != nil {
		t.Fatal(err)
	}
	sess := &core.Session{
		ID: core.NewSessionID(), Prompt: "company revenues",
		Mode: core.ModeDataset, ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens, Budget: 1000,
		Status: core.StatusDone, CreatedAt: time.Now().UTC(),
	}
	ctx := context.Background()
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertSession(ctx, sess); err != nil {
			return err
		}
		if err := tx.SetDatasetSchema(ctx, sess.ID, schema); err != nil {
			return err
		}
		// Two sources for one company, disagreeing on revenue; one more company.
		return tx.InsertRows(ctx, sess.ID, []dataset.Row{
			{Values: map[string]string{"company": "Acme Ltd", "revenue": "1200000"},
				Source: "https://a.example", Quote: "Acme Ltd reported revenue of $1.2m"},
			{Values: map[string]string{"company": "Acme Limited", "revenue": "1350000"},
				Source: "https://b.example", Quote: "Acme Limited turnover was 1.35m"},
			{Values: map[string]string{"company": "Beta GmbH", "revenue": "900000"},
				Source: "https://c.example", Quote: "Beta GmbH made 900,000"},
			// A threshold-sensitive pair: these score 0.67, so they merge at the
			// default and separate above it. "Acme Ltd" and "Acme Limited" cannot
			// serve for that — they normalise to the same string and merge at any
			// threshold, which is what the first version of the re-derivation test
			// got wrong.
			{Values: map[string]string{"company": "Gamma Foods", "revenue": "500000"},
				Source: "https://d.example", Quote: "Gamma Foods turned over 500,000"},
			{Values: map[string]string{"company": "Gamma Foods Europe", "revenue": "480000"},
				Source: "https://e.example", Quote: "Gamma Foods Europe reported 480,000"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	return sess.ID, db
}

func runDataset(t *testing.T, home string, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("MOLE_CONFIG_DIR", t.TempDir())
	t.Setenv("MOLE_DB", filepath.Join(home, "mole.db"))

	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

func TestDatasetWritesCSVAndSaysWhatItCannotHold(t *testing.T) {
	home := t.TempDir()
	id, _ := seedDataset(t, home)

	out, errOut, err := runDataset(t, home, "dataset", id)
	if err != nil {
		t.Fatalf("dataset: %v\n%s", err, errOut)
	}
	recs, cerr := csv.NewReader(strings.NewReader(out)).ReadAll()
	if cerr != nil {
		t.Fatalf("not valid CSV: %v\n%s", cerr, out)
	}
	if len(recs) != 4 {
		t.Fatalf("records = %d, want header + 3 merged rows:\n%s", len(recs), out)
	}
	// The two Acme rows merged; the disagreement is named rather than dropped.
	var acme []string
	for _, r := range recs[1:] {
		if strings.HasPrefix(r[0], "Acme") {
			acme = r
		}
	}
	if acme == nil {
		t.Fatalf("no merged Acme row:\n%s", out)
	}
	if acme[2] != "2" {
		t.Errorf("sources = %q, want 2", acme[2])
	}
	if acme[3] != "revenue" {
		t.Errorf("contested = %q, want revenue", acme[3])
	}
	// What qualifies the dataset goes to stderr, so a redirect gets only data.
	if !strings.Contains(errOut, "disagree") {
		t.Errorf("the summary does not reach stderr:\n%s", errOut)
	}
	if strings.Contains(out, "disagree") {
		t.Errorf("prose leaked into the data stream:\n%s", out)
	}
}

func TestDatasetJSONCarriesEveryValue(t *testing.T) {
	home := t.TempDir()
	id, _ := seedDataset(t, home)

	out, _, err := runDataset(t, home, "dataset", id, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	var d dataset.Dataset
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	for _, row := range d.Rows {
		if !strings.HasPrefix(row.Get("company"), "Acme") {
			continue
		}
		cell := row.Cells["revenue"]
		if !cell.Contested() || len(cell.Others) != 1 {
			t.Errorf("the second figure was dropped: %+v", cell)
		}
		if len(row.Quotes) != 2 {
			t.Errorf("quotes = %v, want one per source", row.Quotes)
		}
	}
}

// TestTheMergeRunsOnRead. A different threshold must produce a different dataset
// from the same stored rows — that is what makes a later fix to the matching rules
// improve data already collected.
func TestTheMergeRunsOnRead(t *testing.T) {
	home := t.TempDir()
	id, _ := seedDataset(t, home)

	loose, _, err := runDataset(t, home, "dataset", id, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	// Above the score of the Gamma pair, so they stop merging.
	strict, _, err := runDataset(t, home, "dataset", id, "--format", "json", "--threshold", "0.8")
	if err != nil {
		t.Fatal(err)
	}
	var a, b dataset.Dataset
	if err := json.Unmarshal([]byte(loose), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strict), &b); err != nil {
		t.Fatal(err)
	}
	if len(a.Rows) != 3 {
		t.Errorf("default produced %d rows, want 3 (Acme merged, Beta, Gamma merged)",
			len(a.Rows))
	}
	if len(b.Rows) != 4 {
		t.Errorf("a threshold of 0.8 produced %d rows, want 4 — the merge is not "+
			"re-derived on read", len(b.Rows))
	}
}

// TestDatasetRefusesAReportSession, rather than writing an empty file: the user
// asked the wrong command about the right session.
func TestDatasetRefusesAReportSession(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "mole.db")
	if err := cmdMigrate(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	db, err := openDBNoMigrate(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	sess := &core.Session{
		ID: core.NewSessionID(), Prompt: "q", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens, Budget: 100, Status: core.StatusDone,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertSession(ctx, sess)
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, _, err = runDataset(t, home, "dataset", sess.ID)
	if err == nil {
		t.Fatal("a report session was accepted")
	}
	if !strings.Contains(err.Error(), "not a dataset session") {
		t.Errorf("err = %v", err)
	}
}

func TestDatasetRefusesAnUnknownFormat(t *testing.T) {
	home := t.TempDir()
	id, _ := seedDataset(t, home)
	if _, _, err := runDataset(t, home, "dataset", id, "--format", "xlsx"); err == nil {
		t.Fatal("accepted an unknown format")
	}
}
