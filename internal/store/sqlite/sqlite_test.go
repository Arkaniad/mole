package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

func open(t *testing.T) (*sqlite.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, path
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, path := open(t)

	for i := 0; i < 3; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("repeat migrate %d: %v", i, err)
		}
	}

	// And across a reopen, which is the case that actually happens: every CLI
	// invocation migrates on open.
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	db2, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if err := db2.Migrate(ctx); err != nil {
		t.Fatalf("migrate after reopen: %v", err)
	}
}

func TestGetMissingSessionIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)

	err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		_, err := q.GetSession(ctx, "s_nope")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func insertSession(t *testing.T, db *sqlite.DB, id string, budget int64) {
	t.Helper()
	ctx := context.Background()
	err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertSession(ctx, &core.Session{
			ID:         id,
			Prompt:     "p",
			Mode:       core.ModeReport,
			ActorTypes: []core.ActorType{core.ActorWeb},
			BudgetUnit: core.BudgetUSD,
			Budget:     budget,
			Status:     core.StatusRunning,
		})
	})
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// TestBudgetDeltaRejectsNegativeInvariant proves the schema is the last line of
// defence: even a caller that computes a wrong delta cannot drive the ledger
// negative, it gets ErrConflict instead.
func TestBudgetDeltaRejectsNegativeInvariant(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_neg", 1000)

	err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.ApplyBudgetDelta(ctx, "s_neg", store.BudgetDelta{Held: -1})
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}

	// State is unchanged: the transaction rolled back.
	var s *core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, "s_neg")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if s.Held != 0 {
		t.Fatalf("held = %d after rejected delta, want 0", s.Held)
	}
}

func TestSumCostsByRole(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_roles", 1_000_000)

	rows := []struct {
		role core.Role
		usd  int64
	}{
		{core.RolePlanner, 1000},
		{core.RolePlanner, 2000},
		{core.RoleExecutor, 5000},
		{core.RoleVerifier, 500},
	}
	err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		for _, r := range rows {
			if err := tx.InsertToolCall(ctx, &core.ToolCall{
				SessionID: "s_roles",
				Role:      r.role,
				Type:      core.CallLLM,
				Cost:      core.Cost{USDMicros: r.usd, InputTokens: 10},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert calls: %v", err)
	}

	var byRole map[core.Role]core.Cost
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		byRole, err = q.SumCostsByRole(ctx, "s_roles")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if got := byRole[core.RolePlanner].USDMicros; got != 3000 {
		t.Errorf("planner = %d, want 3000", got)
	}
	if got := byRole[core.RoleExecutor].USDMicros; got != 5000 {
		t.Errorf("executor = %d, want 5000", got)
	}
	if got := byRole[core.RoleVerifier].USDMicros; got != 500 {
		t.Errorf("verifier = %d, want 500", got)
	}
	if _, ok := byRole[core.RoleOutput]; ok {
		t.Error("output role present with no rows")
	}
}

// TestConcurrentWritesDoNotLock is the regression test for SQLite's classic
// failure mode. A naive pool produces "database is locked" here; the
// single-writer pool serializes instead.
func TestConcurrentWritesDoNotLock(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_conc", 10_000_000)

	const writers = 24
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	start := make(chan struct{})

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
				if err := tx.InsertToolCall(ctx, &core.ToolCall{
					SessionID: "s_conc",
					Role:      core.RoleExecutor,
					Type:      core.CallFetch,
					Cost:      core.Cost{USDMicros: 100},
				}); err != nil {
					return err
				}
				return tx.ApplyBudgetDelta(ctx, "s_conc", store.BudgetDelta{Spent: 100, ToolCallCount: 1})
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent write failed: %v", err)
	}

	var s *core.Session
	var total core.Cost
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		if s, err = q.GetSession(ctx, "s_conc"); err != nil {
			return err
		}
		total, err = q.SumCosts(ctx, "s_conc")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if want := int64(writers * 100); s.Spent != want {
		t.Errorf("spent = %d, want %d (lost update)", s.Spent, want)
	}
	if total.USDMicros != s.Spent {
		t.Errorf("materialized spent %d != ledger sum %d", s.Spent, total.USDMicros)
	}
	if s.ToolCallCount != writers {
		t.Errorf("tool_call_count = %d, want %d", s.ToolCallCount, writers)
	}
}

// TestReadsProceedDuringOpenWrite is the property that makes SQLite sufficient
// for this workload, and the reason `mole trace` can run against a database the
// daemon is actively writing.
//
// Under WAL, readers see the last committed snapshot and never block on an open
// write transaction. Without WAL this deadlocks; with it, the read returns the
// pre-transaction state immediately.
func TestReadsProceedDuringOpenWrite(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_wal", 1_000_000)

	writing := make(chan struct{})
	readDone := make(chan error, 1)

	go func() {
		_ = db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			if err := tx.InsertToolCall(ctx, &core.ToolCall{
				SessionID: "s_wal", Role: core.RoleExecutor, Type: core.CallLLM,
				Cost: core.Cost{USDMicros: 999},
			}); err != nil {
				return err
			}
			close(writing)
			// Hold the write transaction open while a reader runs.
			<-readDone
			return nil
		})
	}()

	<-writing

	// This must not block on the open writer.
	var n int64
	err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		n, err = q.CountToolCalls(ctx, "s_wal")
		return err
	})
	readDone <- err
	if err != nil {
		t.Fatalf("read during open write transaction: %v", err)
	}
	// The reader sees the committed snapshot, not the in-flight write.
	if n != 0 {
		t.Fatalf("reader saw %d uncommitted rows, want 0", n)
	}
}

func TestRollbackDiscardsPartialWork(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_rb", 1_000_000)

	sentinel := errors.New("abort")
	err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := tx.InsertToolCall(ctx, &core.ToolCall{
			SessionID: "s_rb", Role: core.RoleExecutor, Type: core.CallLLM,
			Cost: core.Cost{USDMicros: 5000},
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}

	var n int64
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		n, err = q.CountToolCalls(ctx, "s_rb")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rolled-back transaction left %d rows", n)
	}
}

func TestSpanLifecycle(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_span", 1000)

	span := &core.Span{
		SessionID: "s_span",
		Name:      "executor",
		Attrs:     map[string]string{"actor": "web"},
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.StartSpan(ctx, span)
	}); err != nil {
		t.Fatalf("start span: %v", err)
	}

	end := time.Now().UTC()
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.EndSpan(ctx, span.ID, end, "ok")
	}); err != nil {
		t.Fatalf("end span: %v", err)
	}

	// Ending twice must not silently succeed — it would mask a double-close bug.
	err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.EndSpan(ctx, span.ID, end, "ok")
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second EndSpan error = %v, want ErrNotFound", err)
	}

	var spans []*core.Span
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		spans, err = q.ListSpans(ctx, "s_span")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].EndedAt == nil {
		t.Fatal("span not closed")
	}
	if spans[0].Attrs["actor"] != "web" {
		t.Errorf("attrs lost: %v", spans[0].Attrs)
	}
}

func TestInvalidEnumIsRejectedBySchema(t *testing.T) {
	ctx := context.Background()
	db, _ := open(t)
	insertSession(t, db, "s_enum", 1000)

	// core.ToolCall.Validate catches this first, so go around it to prove the
	// CHECK constraint is really there.
	err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertToolCall(ctx, &core.ToolCall{
			SessionID: "s_enum",
			Role:      core.Role("not-a-role"),
			Type:      core.CallLLM,
		})
	})
	if err == nil {
		t.Fatal("invalid role was accepted")
	}
}
