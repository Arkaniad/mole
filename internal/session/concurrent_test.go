package session_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/session"
)

// barrierPlanner holds every caller until n of them have arrived, guaranteeing
// the runs genuinely overlap. Without it the stub is fast enough that sessions
// can complete one after another and never contend at all — a concurrency test
// that never runs anything concurrently.
type barrierPlanner struct {
	mu      sync.Mutex
	arrived int
	n       int
	gate    chan struct{}
	all     chan struct{}
	once    sync.Once
}

func newBarrierPlanner(n int) *barrierPlanner {
	return &barrierPlanner{n: n, gate: make(chan struct{}), all: make(chan struct{})}
}

func (*barrierPlanner) Name() string               { return "barrier" }
func (*barrierPlanner) ModelFor(t llm.Tier) string { return "stub-model" }

func (b *barrierPlanner) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	b.mu.Lock()
	b.arrived++
	reached := b.arrived >= b.n
	b.mu.Unlock()
	if reached {
		b.once.Do(func() { close(b.all) })
	}
	select {
	case <-b.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Response{
		Text:  `{"questions":[],"rationale":"none"}`,
		Model: "stub-model",
		Usage: llm.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

// TestConcurrentSessionsDoNotShareActorState.
//
// The supervisor exists to run sessions at the same time, and Runner.Run sets
// per-session state on its actor — the session id every claim is filed under,
// and the fetch cache. A single actor shared across concurrent runs is both a
// data race and a correctness bug: one session's claims get filed under
// another's id, which no amount of budget accounting would catch.
//
// The barrier is load-bearing. An earlier version of this test used the instant
// stub, and the three sessions completed in sequence without ever overlapping —
// green, and testing nothing.
func TestConcurrentSessionsDoNotShareActorState(t *testing.T) {
	const n = 3
	p := newBarrierPlanner(n)

	r, _ := newRunner(t)
	r.Actor = &actors.WebActor{LLM: p, Search: emptySearch{}}
	sup := session.NewSupervisor(r, n, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = sup.Shutdown(ctx) })

	var ids []string
	for i := 0; i < n; i++ {
		s, err := sup.Start(context.Background(), testSpec())
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		ids = append(ids, s.ID)
	}

	// All three are now inside a model call at once.
	select {
	case <-p.all:
	case <-time.After(15 * time.Second):
		t.Fatal("the sessions never overlapped")
	}
	close(p.gate)

	for _, id := range ids {
		if _, err := sup.Wait(ctx, id); err != nil {
			t.Errorf("wait %s: %v", id, err)
		}
	}

	// The observable proof: Run must leave the Runner's own actor untouched. If it
	// mutates the shared one, these fields carry whichever session wrote last —
	// and every claim in flight was filed under that id.
	//
	// Asserted this way because the race detector does not settle it. The writes
	// happen at the top of each run, before the goroutines reach the barrier, so
	// three concurrent sessions never produced a report — the bug is real by
	// inspection and invisible to -race in this fixture.
	if got := r.Actor.SessionID; got != "" {
		t.Errorf("Run mutated the shared actor's SessionID to %q; concurrent sessions "+
			"would file claims under each other's ids", got)
	}
	if r.Actor.Cache != nil {
		t.Error("Run installed a session cache on the shared actor; unrelated research shares it")
	}
}
