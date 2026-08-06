package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/lajosdeme/mole/internal/core"
)

// DefaultMaxConcurrent bounds sessions running at once.
//
// Small on purpose. Each session holds a lead queue, a cache and a model client,
// and they all contend for one SQLite writer — the ceiling that matters is not
// CPU. M5's executor pool is where this becomes a real scheduler; until then a
// low bound that refuses honestly beats a high one that thrashes.
const DefaultMaxConcurrent = 4

// ErrAtCapacity is returned by Start when MaxConcurrent sessions are running.
//
// Refusing rather than queueing is deliberate for this milestone. A queued
// session would hand the caller an id, report "running", and do nothing for an
// unknown period — a status that lies. M5 adds a real queue with a state to
// match; until then the honest answer is no.
var ErrAtCapacity = errors.New("session: at capacity")

// ErrNoSuchSession is returned for an id the supervisor is not running.
var ErrNoSuchSession = errors.New("session: not running")

// ErrShutdown is returned once the supervisor has been shut down.
var ErrShutdown = errors.New("session: supervisor is shut down")

// Supervisor runs sessions in the background.
//
// The daemon needs this because MCP is asynchronous: research.report returns a
// session id immediately and the work continues long after the call — and long
// after the calling agent's own context is gone. So a session's lifetime cannot
// be tied to the request that started it, which is the single most important
// thing this type gets right. Start uses the caller's context only to create the
// row; the run itself hangs off a context the supervisor owns.
type Supervisor struct {
	runner *Runner
	max    int
	log    *slog.Logger

	// base is cancelled by Shutdown, which cancels every running session.
	base context.Context
	stop context.CancelFunc

	mu sync.Mutex
	// sessions holds both running and recently finished handles; see maxFinished.
	sessions map[string]*handle
	closed   bool
	wg       sync.WaitGroup
}

// maxFinished caps how many completed sessions the supervisor remembers.
//
// Finished handles are retained so Wait works on a session that ended between
// Start and the call — otherwise Wait is a race, which for a daemon whose whole
// interaction model is "kick off, poll later" makes it useless. Bounded because
// the daemon is long-lived and the store, not this map, is the durable record.
const maxFinished = 64

// handle is one session, running or recently finished.
type handle struct {
	cancel context.CancelFunc
	done   chan struct{}

	// finished and finishedAt are guarded by Supervisor.mu.
	finished   bool
	finishedAt time.Time

	// cancelled records that a human asked for this, so the final status is
	// "cancelled" rather than the "failed" a context error would otherwise
	// produce. The two mean different things to somebody reading the list.
	cancelled bool

	res *Result
	err error
}

// NewSupervisor returns a supervisor that runs at most max sessions at once.
func NewSupervisor(r *Runner, max int, log *slog.Logger) *Supervisor {
	if max <= 0 {
		max = DefaultMaxConcurrent
	}
	if log == nil {
		log = slog.Default()
	}
	base, stop := context.WithCancel(context.Background())
	return &Supervisor{
		runner:   r,
		max:      max,
		log:      log,
		base:     base,
		stop:     stop,
		sessions: map[string]*handle{},
	}
}

// Start creates a session and begins running it in the background.
//
// The returned session exists and has its budget reserved before this returns,
// so a caller can hand the id back immediately and poll. ctx bounds only the
// creation: the run continues after it is cancelled, which is the whole point.
func (s *Supervisor) Start(ctx context.Context, spec Spec) (*core.Session, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrShutdown
	}
	if s.liveLocked() >= s.max {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %d session(s) already running", ErrAtCapacity, s.max)
	}
	s.mu.Unlock()

	sess, err := s.runner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}

	// Derived from the supervisor's context, NOT the caller's. Headroom over the
	// wall-clock ceiling so the two do not fire together: when they do, the loop
	// is killed mid-lead instead of stopping at its own check, and the run ends
	// in a cascade of context errors from whatever was in flight.
	runCtx, cancel := context.WithTimeout(s.base, spec.Timeout+30*time.Second)
	h := &handle{cancel: cancel, done: make(chan struct{})}

	s.mu.Lock()
	// Re-check under the same lock that registers the handle. Between the check
	// above and here, another Start could have taken the last slot and Shutdown
	// could have closed the supervisor.
	switch {
	case s.closed:
		s.mu.Unlock()
		cancel()
		return nil, ErrShutdown
	case s.liveLocked() >= s.max:
		s.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("%w: %d session(s) already running", ErrAtCapacity, s.max)
	}
	s.reapLocked()
	s.sessions[sess.ID] = h
	s.wg.Add(1)
	s.mu.Unlock()

	go s.run(runCtx, h, sess, spec)
	return sess, nil
}

func (s *Supervisor) run(ctx context.Context, h *handle, sess *core.Session, spec Spec) {
	defer s.wg.Done()
	defer close(h.done)
	defer h.cancel()

	res, err := s.runner.Run(ctx, sess, spec)

	// No status write here. The executor already derives one from the context
	// (statusForContext: cancelled for a cancellation, exhausted for a deadline)
	// and Runner.Run finalizes with it, so a second write would be redundant.
	//
	// This was not redundant when written — it was written on the belief that a
	// cancelled run finalizes as "failed", and the falsification round found that
	// removing it changed no test. A confident comment justifying dead code is
	// worse than the dead code, so both went.
	s.mu.Lock()
	h.res, h.err = res, err
	h.finished = true
	h.finishedAt = time.Now()
	s.mu.Unlock()

	if err != nil {
		s.log.Warn("session ended with an error", "session", sess.ID, "err", err)
	}
}

// Cancel stops a running session.
//
// Returns once the session has been asked to stop, not once it has. Callers that
// need to know it finished should Wait. The distinction matters: a lead mid-fetch
// takes as long as the fetch does, and an MCP caller should not be blocked on it.
func (s *Supervisor) Cancel(id string) error {
	s.mu.Lock()
	h, ok := s.sessions[id]
	if ok && !h.finished {
		h.cancelled = true
	} else {
		ok = false
	}
	s.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}
	h.cancel()
	return nil
}

// Wait blocks until the session finishes, or ctx is done.
//
// An id the supervisor is not running is an error rather than an immediate
// return: "already finished" and "never existed" are different answers, and only
// the store can tell them apart.
func (s *Supervisor) Wait(ctx context.Context, id string) (*Result, error) {
	s.mu.Lock()
	h, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSession, id)
	}

	select {
	case <-h.done:
		s.mu.Lock()
		res, err := h.res, h.err
		s.mu.Unlock()
		return res, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Running lists the session ids currently in flight, sorted.
func (s *Supervisor) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.sessions))
	for id, h := range s.sessions {
		if !h.finished {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Shutdown cancels every running session and waits for them to finish.
//
// Waits rather than returning immediately, because the thing being waited for is
// each session releasing its budget holds. A daemon that exits while a
// reservation is open leaves money neither spent nor available until the next
// boot's sweep reclaims it (§9.4) — recoverable, but only later, and only by
// somebody who runs mole again.
//
// If ctx expires first the sessions are still cancelled and the error says so;
// their holds are then left to that sweep.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	n := s.liveLocked()
	for _, h := range s.sessions {
		if !h.finished {
			h.cancelled = true
		}
	}
	s.mu.Unlock()

	s.stop()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("session: shutdown timed out with %d session(s) still finishing; "+
			"their holds will be reclaimed by the next boot sweep: %w", n, ctx.Err())
	}
}

// liveLocked counts sessions still running. Caller holds s.mu.
func (s *Supervisor) liveLocked() int {
	n := 0
	for _, h := range s.sessions {
		if !h.finished {
			n++
		}
	}
	return n
}

// reapLocked drops the oldest finished handles past maxFinished. Caller holds s.mu.
func (s *Supervisor) reapLocked() {
	var done []string
	for id, h := range s.sessions {
		if h.finished {
			done = append(done, id)
		}
	}
	if len(done) <= maxFinished {
		return
	}
	sort.Slice(done, func(i, j int) bool {
		return s.sessions[done[i]].finishedAt.Before(s.sessions[done[j]].finishedAt)
	})
	for _, id := range done[:len(done)-maxFinished] {
		delete(s.sessions, id)
	}
}
