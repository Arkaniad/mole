package mcpserver_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/mcpserver"
	"github.com/lajosdeme/mole/internal/store"
)

// answerer returns a citing answer and counts the calls it was asked to make.
type answerer struct {
	calls atomic.Int32
	// lastPrompt lets a test see what the model was actually shown.
	lastPrompt atomic.Value
}

func (*answerer) Name() string               { return "answerer" }
func (*answerer) ModelFor(t llm.Tier) string { return "stub-model" }
func (a *answerer) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	a.calls.Add(1)
	if len(req.Messages) > 0 {
		a.lastPrompt.Store(req.Messages[0].Text)
	}
	return &llm.Response{
		Text:  "MambaByte reaches 0.930 bits per byte on PG-19. [1]",
		Model: "stub-model",
		Usage: llm.Usage{InputTokens: 400, OutputTokens: 20},
	}, nil
}

func (a *answerer) prompt() string {
	if v, ok := a.lastPrompt.Load().(string); ok {
		return v
	}
	return ""
}

// finishedSession makes a session with claims and a terminal status, which is
// what §13's ask operates on.
func finishedSession(t *testing.T, r *rig, claims []core.Claim) string {
	t.Helper()
	return finishedSessionAt(t, r, claims, "0.50")
}

// finishedSessionAt is finishedSession with the budget chosen, so a rig running
// under a low per-session ceiling can still create one. Passing 0.50 to a rig
// capped at 0.01 gets the report refused and an empty session id, which then
// fails a foreign key three lines later and says nothing about why.
func finishedSessionAt(t *testing.T, r *rig, claims []core.Claim, amount string) string {
	t.Helper()
	var rep mcpserver.ReportOut
	if res := r.call(t, "research.report", map[string]any{
		"prompt": "what does MambaByte achieve on PG-19",
		"budget": map[string]any{"unit": "usd", "amount": amount},
	}, &rep); res.IsError {
		t.Fatalf("fixture report refused: %s", errText(res))
	}
	if rep.SessionID == "" {
		t.Fatal("fixture report returned no session id")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = r.sup.Wait(ctx, rep.SessionID)
	for i := 0; i < 200 && len(r.sup.Running()) > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}

	lead := core.Lead{
		ID: core.NewLeadID(), SessionID: rep.SessionID,
		ActorType: core.ActorWeb, Query: "fixture", Status: core.LeadQueued,
	}
	if err := r.db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertLead(ctx, &lead)
	}); err != nil {
		t.Fatalf("insert lead: %v", err)
	}
	for i := range claims {
		claims[i].ID = core.NewClaimID()
		claims[i].SessionID = rep.SessionID
		claims[i].LeadID = lead.ID
		claims[i].RetrievedAt = time.Now().UTC()
	}
	if err := r.db.WithTx(context.Background(), func(ctx context.Context, tx store.Tx) error {
		return tx.InsertClaims(ctx, claims)
	}); err != nil {
		t.Fatalf("insert claims: %v", err)
	}
	return rep.SessionID
}

func pgClaims() []core.Claim {
	return []core.Claim{
		{Text: "MambaByte-353M achieves 0.930 bits per byte on the PG-19 benchmark dataset.",
			Source: "https://arxiv.org/a", Quote: "0.930 bits per byte on PG-19"},
		{Text: "MambaByte maintains a fixed-size memory state independent of context length.",
			Source: "https://arxiv.org/b", Quote: "fixed-size memory state"},
		{Text: "Equal-Info Windows lets models learn over neural-compressed text.",
			Source: "https://example.com/c", Quote: "Equal-Info Windows"},
	}
}

// TestAskAnswersFromAFinishedSessionAndCharges It.
//
// §13's whole promise: a follow-up against research already paid for, answered in
// one model call. If it did not cost anything the ledger would be wrong, and if
// it re-researched it would not be an ask.
func TestAskAnswersFromAFinishedSessionAndChargesIt(t *testing.T) {
	a := &answerer{}
	r := connectWith(t, 0, a)
	id := finishedSession(t, r, pgClaims())

	srcSpendBefore := sessionSpend(t, r.db, id)

	var out mcpserver.AskOut
	res := r.call(t, "research.ask", map[string]any{
		"session_id": id, "question": "what bits per byte does it reach on PG-19",
	}, &out)
	if res.IsError {
		t.Fatalf("ask failed: %s", errText(res))
	}

	if a.calls.Load() != 1 {
		t.Errorf("%d model calls; an ask is one call", a.calls.Load())
	}
	if !strings.Contains(out.Answer, "0.930") {
		t.Errorf("answer does not carry the finding: %q", out.Answer)
	}
	if len(out.Citations) == 0 {
		t.Error("no citations; an answer an agent cannot check is prose")
	}
	if len(out.Claims) == 0 {
		t.Error("no claims returned; §5.3 says a caller weighs evidence, not prose")
	}

	// It cost something, and the ledger recorded it — against the ASK session.
	if out.Spent <= 0 {
		t.Errorf("spent = %d; a model call that costs nothing is not being recorded", out.Spent)
	}
	if out.AskSessionID == "" {
		t.Fatal("no ask_session_id; the cost is untraceable")
	}
	if out.AskSessionID == id {
		t.Fatal("the ask was charged to the session it queried, whose accounts are closed")
	}

	// The queried session is untouched. Its ledger was settled when it finished
	// and must stay that way.
	if after := sessionSpend(t, r.db, id); after != srcSpendBefore {
		t.Errorf("the queried session's spend moved: %d -> %d", srcSpendBefore, after)
	}

	askSess := loadSess(t, r.db, out.AskSessionID)
	if askSess.Mode != core.ModeAsk {
		t.Errorf("ask session mode = %q, want %q", askSess.Mode, core.ModeAsk)
	}
	if !askSess.Status.Terminal() {
		t.Errorf("the ask session is still %s; it would be swept as abandoned", askSess.Status)
	}
	if askSess.Spent != out.Spent {
		t.Errorf("reported spend %d != ledger %d", out.Spent, askSess.Spent)
	}
	if askSess.Spent > askSess.Budget {
		t.Errorf("an ask spent %d against a %d allowance", askSess.Spent, askSess.Budget)
	}

	// Nothing left held, on either session.
	led := budget.New(r.db, budget.DefaultConfig())
	for _, sid := range []string{id, out.AskSessionID} {
		v, err := led.Verify(context.Background(), sid)
		if err != nil {
			t.Fatal(err)
		}
		if !v.Consistent() || v.HeldRecorded != 0 {
			t.Errorf("%s after an ask: held=%d consistent=%v", sid, v.HeldRecorded, v.Consistent())
		}
	}
}

// TestAnAskIsBoundedByItsOwnAllowance.
//
// The ask runs as its own session precisely so it cannot touch the queried
// session's settled ledger — which means the thing that bounds it is its own
// allowance, and that has to actually bind. §13's promise is that an ask is
// cheap; without this it is only cheap by habit.
func TestAnAskIsBoundedByItsOwnAllowance(t *testing.T) {
	a := &answerer{}
	r := connectWith(t, 0, a)
	id := finishedSession(t, r, pgClaims())

	var out mcpserver.AskOut
	if res := r.call(t, "research.ask", map[string]any{
		"session_id": id, "question": "what bits per byte on PG-19",
	}, &out); res.IsError {
		t.Fatalf("ask: %s", errText(res))
	}
	askSess := loadSess(t, r.db, out.AskSessionID)
	if askSess.Budget != mcpserver.AskAllowance {
		t.Errorf("ask budget = %d, want the allowance %d", askSess.Budget, mcpserver.AskAllowance)
	}

	// And the daemon's per-session ceiling still wins when it is lower.
	r2 := connectWith(t, 10_000, a) // $0.01, below the $0.05 allowance
	id2 := finishedSessionAt(t, r2, pgClaims(), "0.005")
	var out2 mcpserver.AskOut
	if res := r2.call(t, "research.ask", map[string]any{
		"session_id": id2, "question": "what bits per byte on PG-19",
	}, &out2); res.IsError {
		t.Fatalf("ask under a low ceiling: %s", errText(res))
	}
	if b := loadSess(t, r2.db, out2.AskSessionID).Budget; b != 10_000 {
		t.Errorf("ask budget = %d under a 10000 ceiling; the ceiling did not apply", b)
	}
}

// TestAskRefusesARunningSession. The claim graph would change under the answer,
// so the citations could point at claims that were not there when it was written.
func TestAskRefusesARunningSession(t *testing.T) {
	a := &answerer{}
	// Held, not hoped for. This used to skip when the session had already
	// finished, which made it pass alone, skip sometimes, and fail under load —
	// the wrong assertion firing rather than a regression.
	r, release := connectHeld(t, 0, a)
	defer release()

	var rep mcpserver.ReportOut
	r.call(t, "research.report", map[string]any{
		"prompt": "a question",
		"budget": map[string]any{"unit": "usd", "amount": "0.50"},
	}, &rep)

	if len(r.sup.Running()) == 0 {
		t.Fatal("the held session is not running; the planner was not blocked")
	}
	res := r.call(t, "research.ask", map[string]any{
		"session_id": rep.SessionID, "question": "anything",
	}, nil)
	if !res.IsError {
		t.Error("ask accepted a running session")
	}
	if !strings.Contains(errText(res), "still running") {
		t.Errorf("the refusal does not say why: %s", errText(res))
	}
}

// TestAskWithNothingRelevantSaysSoWithoutSpending.
//
// Retrieval found no claim sharing a content word with the question. Paying a
// model to report that is worse than reporting it directly, and an answer
// synthesized from irrelevant claims is worse still.
func TestAskWithNothingRelevantSaysSoWithoutSpending(t *testing.T) {
	a := &answerer{}
	r := connectWith(t, 0, a)
	id := finishedSession(t, r, pgClaims())

	var out mcpserver.AskOut
	if res := r.call(t, "research.ask", map[string]any{
		"session_id": id,
		"question":   "sourdough hydration percentages for rye",
	}, &out); res.IsError {
		t.Fatalf("ask errored: %s", errText(res))
	}

	if a.calls.Load() != 0 {
		t.Errorf("%d model call(s) for a question nothing bears on", a.calls.Load())
	}
	if out.Degraded == "" {
		t.Error("no degraded note explaining why there is no answer")
	}
	if out.Spent != 0 {
		t.Errorf("spent %d on a question nothing bears on", out.Spent)
	}
}

// TestTheAskPromptCarriesTheOriginalQuestion.
//
// The claims were gathered to answer the session's question, not this one. A
// model shown only the new question reads partial coverage as a complete answer —
// the material looks authoritative either way, and nothing in it says what it was
// collected for.
func TestTheAskPromptCarriesTheOriginalQuestion(t *testing.T) {
	a := &answerer{}
	r := connectWith(t, 0, a)
	id := finishedSession(t, r, pgClaims())

	var out mcpserver.AskOut
	r.call(t, "research.ask", map[string]any{
		"session_id": id, "question": "what bits per byte on PG-19",
	}, &out)

	p := a.prompt()
	if p == "" {
		t.Fatal("no prompt captured")
	}
	if !strings.Contains(p, "what does MambaByte achieve on PG-19") {
		t.Error("the prompt does not carry the session's original question, so the " +
			"model cannot tell partial coverage from a complete answer")
	}
	if !strings.Contains(p, "0.930 bits per byte") {
		t.Errorf("the relevant claim did not reach the prompt:\n%.400s", p)
	}
	// And the irrelevant one did not crowd it out.
	if strings.Contains(p, "Equal-Info Windows") {
		t.Error("an unrelated claim reached the prompt; retrieval is not filtering")
	}
}

func sessionSpend(t *testing.T, db store.Store, id string) int64 {
	t.Helper()
	return loadSess(t, db, id).Spent
}

func loadSess(t *testing.T, db store.Store, id string) *core.Session {
	t.Helper()
	var s *core.Session
	if err := db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("load session: %v", err)
	}
	return s
}
