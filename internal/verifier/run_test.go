package verifier_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
	"github.com/lajosdeme/mole/internal/verifier"
)

// scriptedLLM answers with whatever reply() returns for each prompt, and records
// what it was asked.
type scriptedLLM struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	reply   func(call int, prompt string) (string, error)
}

func (s *scriptedLLM) Name() string               { return "fake" }
func (s *scriptedLLM) ModelFor(t llm.Tier) string { return "fake-model" }
func (s *scriptedLLM) Complete(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	var prompt string
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Text
	}
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()

	text, err := s.reply(i, prompt)
	resp := &llm.Response{
		Model: "fake-model",
		Text:  text,
		Usage: llm.Usage{InputTokens: 400, OutputTokens: 80},
	}
	return resp, err
}

func (s *scriptedLLM) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// judgeEveryPair answers every numbered pair in the prompt with one relation.
func judgeEveryPair(rel verifier.Relation) func(int, string) (string, error) {
	return func(_ int, prompt string) (string, error) {
		var b strings.Builder
		b.WriteString(`{"verdicts":[`)
		// `"b":` appears once per data pair. Counting `"pair":` would also match
		// the shape example in the instructions, and did — every batch got one
		// extra out-of-range verdict, silently dropped by validation.
		n := strings.Count(prompt, `"b":`)
		for i := 1; i <= n; i++ {
			if i > 1 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"pair":%d,"relation":%q,"confidence":0.8,"why":"because"}`, i, rel)
		}
		b.WriteString("]}")
		return b.String(), nil
	}
}

type rig struct {
	db   *sqlite.DB
	led  *budget.Ledger
	sess *core.Session
	llm  *scriptedLLM
	v    *verifier.Verifier
}

func newRig(t *testing.T, budgetTokens int64, claims []core.Claim, reply func(int, string) (string, error)) *rig {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt: "q", Mode: core.ModeReport,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: core.BudgetTokens, Budget: budgetTokens,
		MaxLeads: 50, MaxToolCalls: 500,
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	for i := range claims {
		claims[i].SessionID = sess.ID
		if claims[i].LeadID == "" {
			claims[i].LeadID = "l_1"
		}
		if claims[i].Quote == "" {
			claims[i].Quote = fmt.Sprintf("a quote long enough to be real evidence %d", i)
		}
	}
	if len(claims) > 0 {
		if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.InsertClaims(ctx, claims)
		}); err != nil {
			t.Fatalf("insert claims: %v", err)
		}
	}

	fl := &scriptedLLM{reply: reply}
	return &rig{
		db: db, led: led, sess: sess, llm: fl,
		v: &verifier.Verifier{
			Store: db, Ledger: led, LLM: fl,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
	}
}

func (r *rig) edges(t *testing.T) []*core.ClaimEdge {
	t.Helper()
	var out []*core.ClaimEdge
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		out, err = q.ListEdges(ctx, r.sess.ID, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *rig) unverified(t *testing.T) int {
	t.Helper()
	var out []*core.Claim
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		out, err = q.ListUnverifiedClaims(ctx, r.sess.ID, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return len(out)
}

func (r *rig) reload(t *testing.T) *core.Session {
	t.Helper()
	var s *core.Session
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, r.sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// distinctClaims makes n claims that all relate to each other, so retrieval fills
// its cap and there is real work to batch.
func distinctClaims(n int) []core.Claim {
	out := make([]core.Claim, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, core.Claim{
			Text:   fmt.Sprintf("Subword tokenization affects byte-level scaling in variant %d.", i),
			Source: fmt.Sprintf("https://s%d.example/p", i),
		})
	}
	return out
}

// TestBatchingIsWhatMakesVerificationAffordable is the whole reason this slice was
// built before the prompt.
//
// Measured on the real 13-claim run: 96 ordered candidate pairs, 54 after
// canonicalizing. One call per pair costs roughly a third of what the session spent
// on the research itself. Batched at 8 it is seven calls.
func TestBatchingIsWhatMakesVerificationAffordable(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(13), judgeEveryPair(verifier.RelUnrelated))
	r.v.BatchSize = 8

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.PairsRetrieved == 0 {
		t.Fatal("no pairs retrieved, so batching was never exercised")
	}
	wantCalls := (res.PairsRetrieved - res.PairsDecidedFree + 7) / 8
	if res.Calls != wantCalls {
		t.Errorf("%d calls for %d pairs at batch size 8, want %d",
			res.Calls, res.PairsRetrieved, wantCalls)
	}
	// The point: far fewer calls than pairs.
	if res.Calls >= res.PairsRetrieved {
		t.Errorf("%d calls for %d pairs — batching bought nothing", res.Calls, res.PairsRetrieved)
	}
	if res.PairsJudged != res.PairsRetrieved-res.PairsDecidedFree {
		t.Errorf("%d judged of %d needing judgement", res.PairsJudged, res.PairsRetrieved-res.PairsDecidedFree)
	}
}

// TestEveryPairReachesExactlyOneCall. A batching bug that skips the tail is
// invisible: the pass reports success having never compared the last pairs.
func TestEveryPairReachesExactlyOneCall(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(11), judgeEveryPair(verifier.RelSupports))
	r.v.BatchSize = 3

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Count the pair slots actually sent across all prompts.
	sent := 0
	for _, p := range r.llm.prompts {
		sent += strings.Count(p, `"b":`)
	}
	needed := res.PairsRetrieved - res.PairsDecidedFree
	if sent != needed {
		t.Errorf("%d pair slots sent across %d calls, want %d", sent, r.llm.count(), needed)
	}
	if res.PairsUnjudged != 0 {
		t.Errorf("%d pairs left unjudged by a cooperating model", res.PairsUnjudged)
	}
}

// TestVerificationCannotOutspendItsShare. Pair count grows with the square of the
// claim count before the per-claim cap bites, so without a ceiling a claim-heavy
// session spends more verifying than it spent researching.
func TestVerificationCannotOutspendItsShare(t *testing.T) {
	// A small budget and many claims, so the share cap binds before the work runs
	// out.
	r := newRig(t, 40_000, distinctClaims(20), judgeEveryPair(verifier.RelUnrelated))
	r.v.BatchSize = 2
	r.v.MaxShareOfBudget = 0.1 // 4000 tokens

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Degraded == "" {
		t.Fatalf("the pass reported no degradation, so the ceiling never bound "+
			"(spent %d, %d calls)", res.Spent, res.Calls)
	}
	ceiling := int64(float64(r.sess.Budget) * 0.1)
	if res.Spent > ceiling {
		t.Errorf("spent %d against a ceiling of %d", res.Spent, ceiling)
	}
	if res.PairsUnjudged == 0 {
		t.Error("degraded but reported every pair judged")
	}
	// And the claims are still marked verified, or the work queue never drains and
	// the next pass pays again for the same pairs.
	if n := r.unverified(t); n != 0 {
		t.Errorf("%d claims still unverified after a degraded pass", n)
	}
}

// TestNothingIsLeftHeld. Every reservation resolved, on the failure paths too. A
// settle skipped because a call errored leaves budget neither spent nor available.
func TestNothingIsLeftHeld(t *testing.T) {
	cases := map[string]func(int, string) (string, error){
		"all judged":   judgeEveryPair(verifier.RelSupports),
		"prose":        func(int, string) (string, error) { return "I decline.", nil },
		"empty":        func(int, string) (string, error) { return "", nil },
		"transient":    func(int, string) (string, error) { return "", llm.ErrOverloaded },
		"truncated":    func(int, string) (string, error) { return `{"verdicts":[{"pair":1,"relati`, nil },
		"bad pair nos": func(int, string) (string, error) { return `{"verdicts":[{"pair":99,"relation":"supports"}]}`, nil },
	}

	for name, reply := range cases {
		r := newRig(t, 2_000_000, distinctClaims(6), reply)
		r.v.BatchSize = 3
		if _, err := r.v.Run(context.Background(), r.sess.ID); err != nil {
			t.Fatalf("%s: run: %v", name, err)
		}

		after := r.reload(t)
		if after.Held != 0 {
			t.Errorf("%s: %d still held", name, after.Held)
		}
		v, err := r.led.Verify(context.Background(), r.sess.ID)
		if err != nil {
			t.Fatalf("%s: verify: %v", name, err)
		}
		if !v.Consistent() {
			t.Errorf("%s: ledger drift: spent recorded=%d ledger=%d",
				name, v.SpentRecorded, v.SpentFromLedger)
		}
	}
}

// TestAFatalProviderErrorStopsRatherThanRepeating. A bad credential fails every
// remaining batch identically, and each attempt still takes a reservation and a
// round trip (§9.5).
func TestAFatalProviderErrorStopsRatherThanRepeating(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(12), func(int, string) (string, error) {
		return "", llm.ErrUnauthorized
	})
	r.v.BatchSize = 2

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.llm.count() != 1 {
		t.Errorf("%d calls against an unauthorized provider, want 1", r.llm.count())
	}
	if res.Degraded == "" {
		t.Error("a fatal provider error was not reported as degradation")
	}
	if !strings.Contains(res.Degraded, "unauthorized") {
		t.Errorf("degradation reason does not name the cause: %q", res.Degraded)
	}
}

// TestATransientErrorSkipsOneBatchOnly. The opposite of the above: an overloaded
// provider may well serve the next batch, and abandoning the whole graph over one
// 529 loses work for nothing.
func TestATransientErrorSkipsOneBatchOnly(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(10), func(call int, prompt string) (string, error) {
		if call == 0 {
			return "", llm.ErrOverloaded
		}
		return judgeEveryPair(verifier.RelSupports)(call, prompt)
	})
	r.v.BatchSize = 3

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.llm.count() < 2 {
		t.Errorf("%d calls; one transient failure abandoned the rest", r.llm.count())
	}
	if res.PairsJudged == 0 {
		t.Error("no pairs judged after a recoverable first failure")
	}
	if res.PairsUnjudged == 0 {
		t.Error("the failed batch was not counted as unjudged")
	}
}

// TestEdgesAndVerifiedFlagLandTogether. A pass that wrote edges but failed to mark
// claims would regenerate the same pairs and pay again; one that marked without
// writing would lose the graph permanently.
func TestEdgesAndVerifiedFlagLandTogether(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(5), judgeEveryPair(verifier.RelDuplicate))
	r.v.BatchSize = 4

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.EdgesWritten == 0 {
		t.Fatal("no edges written")
	}
	if n := r.unverified(t); n != 0 {
		t.Errorf("%d claims unverified after a complete pass", n)
	}
	if got := len(r.edges(t)); got != res.EdgesWritten {
		t.Errorf("%d edges in the store, %d reported", got, res.EdgesWritten)
	}

	// A second pass has nothing to do and must not call the model again.
	before := r.llm.count()
	second, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.llm.count() != before {
		t.Errorf("a second pass made %d more calls with nothing unverified",
			r.llm.count()-before)
	}
	if second.ClaimsVerified != 0 {
		t.Errorf("second pass reported verifying %d claims", second.ClaimsVerified)
	}
}

// TestUnrelatedVerdictsWriteNoEdges. The common answer, and storing it would grow
// the edge table quadratically with rows recording absence.
func TestUnrelatedVerdictsWriteNoEdges(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(6), judgeEveryPair(verifier.RelUnrelated))
	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PairsJudged == 0 {
		t.Fatal("nothing was judged")
	}
	if res.EdgesWritten != 0 || len(r.edges(t)) != 0 {
		t.Errorf("%d edges written for all-unrelated verdicts", res.EdgesWritten)
	}
	// But the claims are verified: we asked and got an answer.
	if n := r.unverified(t); n != 0 {
		t.Errorf("%d claims unverified", n)
	}
}

// TestIdenticalClaimsCostNothing. The mechanical shortcut, end to end: duplicate
// text is settled without a model call at all.
func TestIdenticalClaimsCostNothing(t *testing.T) {
	claims := []core.Claim{
		{Text: "MambaByte removes subword tokenization.", Source: "https://a.example/1"},
		{Text: "mambabyte removes subword tokenization", Source: "https://b.example/1"},
	}
	r := newRig(t, 2_000_000, claims, func(int, string) (string, error) {
		return "", errors.New("the model must not be called")
	})

	res, err := r.v.Run(context.Background(), r.sess.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if r.llm.count() != 0 {
		t.Errorf("%d model calls for a pair decided mechanically", r.llm.count())
	}
	if res.PairsDecidedFree != 1 {
		t.Errorf("%d pairs decided free, want 1", res.PairsDecidedFree)
	}
	if res.EdgesWritten != 1 {
		t.Errorf("%d edges, want 1 duplicate_of", res.EdgesWritten)
	}
	edges := r.edges(t)
	if len(edges) == 1 && edges[0].CreatedBy != "mechanical:identical-text" {
		t.Errorf("created_by = %q, want the mechanical rule", edges[0].CreatedBy)
	}
}

// TestVerifierSpendIsAttributedToTheVerifier, so `mole trace` can answer what
// verification cost — the number that says whether the graph is worth its price.
func TestVerifierSpendIsAttributedToTheVerifier(t *testing.T) {
	r := newRig(t, 2_000_000, distinctClaims(6), judgeEveryPair(verifier.RelSupports))
	if _, err := r.v.Run(context.Background(), r.sess.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	var byRole map[core.Role]core.Cost
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		byRole, err = q.SumCostsByRole(ctx, r.sess.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if c, ok := byRole[core.RoleVerifier]; !ok || c.TotalTokens() == 0 {
		t.Errorf("no verifier spend recorded; byRole = %v", byRole)
	}
	for role := range byRole {
		if role != core.RoleVerifier {
			t.Errorf("verification charged %v as well", role)
		}
	}
}
