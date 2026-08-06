package session_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/lajosdeme/mole/internal/store"
)

// blockingPlanner holds a model call open until released, so a test can observe
// a session that is genuinely mid-flight rather than racing a fast one.
type blockingPlanner struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingPlanner() *blockingPlanner {
	return &blockingPlanner{entered: make(chan struct{}), release: make(chan struct{})}
}

func (*blockingPlanner) Name() string               { return "blocking" }
func (*blockingPlanner) ModelFor(t llm.Tier) string { return "stub-model" }

func (b *blockingPlanner) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		// What a real provider does when the run is cancelled underneath it.
		return nil, ctx.Err()
	}
	return &llm.Response{
		Text:  `{"questions":[],"rationale":"none"}`,
		Model: "stub-model",
		Usage: llm.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

func newSupervisor(t *testing.T, p llm.Provider, max int) (*session.Supervisor, store.Store) {
	t.Helper()
	r, db := newRunner(t)
	r.Actor = &actors.WebActor{LLM: p, Search: emptySearch{}}
	sup := session.NewSupervisor(r, max, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.Shutdown(ctx)
	})
	return sup, db
}

// TestCancellingASessionStrandsNoBudget is the property this milestone turns
// from an edge case into a feature.
//
// Every stranded-hold bug this project has had came from a context cancelled
// mid-reserve, and until now the only way to trigger one was Ctrl-C. research.cancel
// makes it a routine, user-invoked path. A hold left open is budget neither spent
// nor available: the session's ceiling is enforced against a number that no longer
// describes reality, and nothing notices until the next boot's sweep.
func TestCancellingASessionStrandsNoBudget(t *testing.T) {
	p := newBlockingPlanner()
	sup, db := newSupervisor(t, p, 2)

	sess, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait until the run is genuinely inside a model call, so the cancellation
	// lands mid-flight with a reservation open rather than before it starts.
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}

	held := heldNow(t, db, sess.ID)
	if held <= 0 {
		t.Fatalf("nothing was held mid-call (%d); this test cannot observe a strand", held)
	}

	if err := sup.Cancel(sess.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := sup.Wait(ctx, sess.ID); err != nil && !errors.Is(err, session.ErrNoSuchSession) {
		t.Fatalf("wait: %v", err)
	}

	if h := heldNow(t, db, sess.ID); h != 0 {
		t.Errorf("%d still held after cancel; the reservation was stranded", h)
	}
	// And the ledger's own reconciliation must agree — a counter that says zero
	// while the rows say otherwise is the failure the counter exists to catch.
	led := budget.New(db, budget.DefaultConfig())
	v, err := led.Verify(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Consistent() {
		t.Errorf("ledger inconsistent after cancel: %+v", v)
	}
}

// TestACancelledSessionIsNotFinalizedAsFailed. "I stopped this" and "this broke"
// are different answers, and a session list that conflates them is unreadable.
func TestACancelledSessionIsNotFinalizedAsFailed(t *testing.T) {
	p := newBlockingPlanner()
	sup, db := newSupervisor(t, p, 2)

	sess, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}
	if err := sup.Cancel(sess.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = sup.Wait(ctx, sess.ID)

	// Poll: Wait returns when the goroutine finishes, and the status write is the
	// last thing it does.
	var got core.SessionStatus
	for i := 0; i < 100; i++ {
		cur, err := loadSession(t, db, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		got = cur.Status
		if got == core.StatusCancelled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != core.StatusCancelled {
		t.Errorf("status is %q, want %q", got, core.StatusCancelled)
	}
}

// TestARunOutlivesTheRequestThatStartedIt.
//
// MCP is asynchronous: research.report returns a session id and the calling
// agent's context is gone long before the research is. Tying the run to that
// context would cancel every session the moment its caller moved on — the single
// most important thing the supervisor gets right, and invisible in any test that
// passes context.Background().
func TestARunOutlivesTheRequestThatStartedIt(t *testing.T) {
	p := newBlockingPlanner()
	sup, _ := newSupervisor(t, p, 2)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	sess, err := sup.Start(reqCtx, testSpec())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}

	// The caller goes away.
	cancelReq()
	time.Sleep(150 * time.Millisecond)

	if running := sup.Running(); len(running) != 1 || running[0] != sess.ID {
		t.Fatalf("the session died with its caller's context; running = %v", running)
	}

	// Let it finish normally.
	close(p.release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := sup.Wait(ctx, sess.ID); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// TestStartRefusesPastTheConcurrencyBound.
//
// Refusing rather than queueing is the deliberate choice for this milestone: a
// queued session would hand back an id, report "running", and do nothing. The
// bound must be enforced against sessions actually in flight, so a finished one
// frees its slot.
func TestStartRefusesPastTheConcurrencyBound(t *testing.T) {
	p := newBlockingPlanner()
	sup, _ := newSupervisor(t, p, 1)

	first, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}

	if _, err := sup.Start(context.Background(), testSpec()); !errors.Is(err, session.ErrAtCapacity) {
		t.Errorf("second start returned %v, want ErrAtCapacity", err)
	}

	close(p.release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = sup.Wait(ctx, first.ID)
	for i := 0; i < 100 && len(sup.Running()) > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}

	// The slot is free again.
	if _, err := sup.Start(context.Background(), testSpec()); err != nil {
		t.Errorf("a finished session did not free its slot: %v", err)
	}
}

// TestShutdownWaitsForHoldsToBeReleased. A daemon that exits mid-session leaves
// money neither spent nor available until the next boot's sweep reclaims it.
func TestShutdownWaitsForHoldsToBeReleased(t *testing.T) {
	p := newBlockingPlanner()
	sup, db := newSupervisor(t, p, 2)

	sess, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}

	// Establish that something WAS held, or the assertion below passes against a
	// fixture that never reserved. The cancel test does this and explains why.
	if h := heldNow(t, db, sess.ID); h <= 0 {
		t.Fatalf("nothing was held mid-call (%d); this test cannot observe a strand", h)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := sup.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if h := heldNow(t, db, sess.ID); h != 0 {
		t.Errorf("%d still held after shutdown returned", h)
	}
	if running := sup.Running(); len(running) != 0 {
		t.Errorf("still running after shutdown: %v", running)
	}
	// And it refuses new work rather than accepting a session nothing will run.
	if _, err := sup.Start(context.Background(), testSpec()); !errors.Is(err, session.ErrShutdown) {
		t.Errorf("Start after Shutdown returned %v, want ErrShutdown", err)
	}
}

func TestCancelAndWaitRejectUnknownSessions(t *testing.T) {
	sup, _ := newSupervisor(t, emptyPlanner{}, 2)
	if err := sup.Cancel("s_nope"); !errors.Is(err, session.ErrNoSuchSession) {
		t.Errorf("Cancel: %v", err)
	}
	if _, err := sup.Wait(context.Background(), "s_nope"); !errors.Is(err, session.ErrNoSuchSession) {
		t.Errorf("Wait: %v", err)
	}
	// The message must name the id — a daemon logs these and "not running" alone
	// is unactionable.
	if err := sup.Cancel("s_nope"); !strings.Contains(err.Error(), "s_nope") {
		t.Errorf("error does not name the session: %v", err)
	}
}

func heldNow(t *testing.T, db store.Store, id string) int64 {
	t.Helper()
	s, err := loadSession(t, db, id)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	return s.Held
}

// TestACancelledSessionSkipsThePaidSteps.
//
// Cancel means stop spending. Both remaining paid steps — §11.5.2's grounding
// re-read and the synthesized answer — are skipped, while escrow is still
// released and every hold still settles: leaving budget held would be the worst
// of both.
//
// Asserted on the notice the skip emits, NOT on the absence of an output tool
// call. The first version of this test did the latter and was vacuous: the
// fixture's session gathers no claims and has nothing spendable, so no output
// row is written whether the branch runs or not. Re-enabling report generation
// and grounding both left it green. The notice is the observable that only
// exists when the decision is actually taken.
func TestACancelledSessionSkipsThePaidSteps(t *testing.T) {
	p := newBlockingPlanner()
	r, db := newRunner(t)
	r.Actor = &actors.WebActor{LLM: p, Search: emptySearch{}}

	var mu sync.Mutex
	var notices []string
	r.Notice = func(m string) {
		mu.Lock()
		defer mu.Unlock()
		notices = append(notices, m)
	}

	sup := session.NewSupervisor(r, 2, nil)
	sess, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}
	if h := heldNow(t, db, sess.ID); h <= 0 {
		t.Fatalf("nothing was held mid-call (%d); the release assertion below is vacuous", h)
	}
	if err := sup.Cancel(sess.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = sup.Wait(ctx, sess.ID)

	mu.Lock()
	got := append([]string(nil), notices...)
	mu.Unlock()

	var skipped bool
	for _, n := range got {
		if strings.Contains(n, "cancelled") && strings.Contains(n, "without a synthesized answer") {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("the cancelled run did not skip its paid steps; notices were: %v", got)
	}

	// The other half of the bargain: skipping the spend must not skip the
	// release. A cancel that saved money by leaving it held has saved nothing.
	// (Held > 0 was established above, before the cancel.)
	if h := heldNow(t, db, sess.ID); h != 0 {
		t.Errorf("%d still held after a cancelled run", h)
	}

	// A run that is NOT cancelled must not emit it — otherwise the assertion
	// above passes for the wrong reason.
	r2, _ := newRunner(t)
	var mu2 sync.Mutex
	var notices2 []string
	r2.Notice = func(m string) { mu2.Lock(); notices2 = append(notices2, m); mu2.Unlock() }
	sup2 := session.NewSupervisor(r2, 2, nil)
	s2, err := sup2.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("start second: %v", err)
	}
	_, _ = sup2.Wait(ctx, s2.ID)
	mu2.Lock()
	defer mu2.Unlock()
	for _, n := range notices2 {
		if strings.Contains(n, "without a synthesized answer") {
			t.Errorf("an uncancelled run reported skipping its paid steps: %q", n)
		}
	}
}

// TestASessionRefusedAfterCreationIsNotLeftRunning.
//
// Start writes the session row BEFORE it takes the slot, so a refusal after that
// point leaves a row nothing will ever finalize: escrow held, status "running",
// invisible to the supervisor but reported running forever by the store, and
// research.cancel answering "this session is not running (status running);
// nothing to cancel". SweepAbandonedSessions reclaims such rows, but only at
// daemon boot, so a long-lived daemon never does.
//
// The concurrency is load-bearing. A sequential Start at capacity is caught by
// the CHEAP pre-check, before Create, and creates nothing — a first version of
// this test did that, and removing the fix left it green. Only two Starts racing
// for the last slot reach the post-Create branch this pins.
func TestASessionRefusedAfterCreationIsNotLeftRunning(t *testing.T) {
	p := newBlockingPlanner()
	sup, db := newSupervisor(t, p, 2)

	// Occupy one of two slots.
	first, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the run never reached a model call")
	}

	// Two callers race for the last slot: both pass the pre-check, both Create.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var refused int
	var started []string
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := sup.Start(context.Background(), testSpec())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
				return
			}
			started = append(started, s.ID)
		}()
	}
	wg.Wait()

	var sessions []*core.Session
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var rerr error
		sessions, rerr = q.ListSessions(ctx, 100)
		return rerr
	}); err != nil {
		t.Fatal(err)
	}

	live := map[string]bool{first.ID: true}
	for _, id := range started {
		live[id] = true
	}
	for _, s := range sessions {
		if live[s.ID] {
			continue
		}
		if s.Status == core.StatusRunning {
			t.Errorf("session %s was refused after creation but is still %q with %d escrow "+
				"held; nothing will ever finalize it (refused=%d, rows=%d)",
				s.ID, s.Status, s.Escrow, refused, len(sessions))
		}
	}

	close(p.release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = sup.Wait(ctx, first.ID)
}

// TestAPanickingSessionDoesNotTakeTheProcessWithIt.
//
// The daemon's proposition is that sessions run concurrently, so a per-session
// fault must not be a process fault: a panic would strand every OTHER session's
// open reservation — no settle, no release — until the next boot's sweep.
func TestAPanickingSessionDoesNotTakeTheProcessWithIt(t *testing.T) {
	sup, db := newSupervisor(t, panicPlanner{}, 2)

	sess, err := sup.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Reaching here at all means the panic did not kill the test process.
	_, _ = sup.Wait(ctx, sess.ID)

	var got core.SessionStatus
	for i := 0; i < 200; i++ {
		cur, err := loadSession(t, db, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		got = cur.Status
		if got.Terminal() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !got.Terminal() {
		t.Errorf("a panicked session is still %q; its escrow is held forever", got)
	}
	if h := heldNow(t, db, sess.ID); h != 0 {
		t.Errorf("%d still held after a panicked session", h)
	}
	// The supervisor keeps working.
	if _, err := sup.Start(context.Background(), testSpec()); err != nil {
		t.Errorf("the supervisor stopped accepting work after a panic: %v", err)
	}
}

// panicPlanner panics where a model call would be, which is where model-driven
// parsing actually lives.
type panicPlanner struct{}

func (panicPlanner) Name() string               { return "panic" }
func (panicPlanner) ModelFor(t llm.Tier) string { return "stub-model" }
func (panicPlanner) Complete(context.Context, llm.Request) (*llm.Response, error) {
	panic("planner exploded")
}
