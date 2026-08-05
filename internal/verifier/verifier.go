package verifier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/store"
)

// Verifier builds the claim graph for a session (§11).
//
// One pass does: read the claims nobody has scored, retrieve candidate pairs
// against the session's whole claim set, settle what can be settled without a
// model, adjudicate the rest in batches, and write the edges.
//
// Deriving confidence from the resulting graph (§11.3) is a separate stage and is
// not here yet; a pass marks claims verified so the work queue drains, and the
// confidence it writes stays 0 until that stage lands. That is the honest value —
// there is nothing to derive from until the graph exists.
type Verifier struct {
	Store  store.Store
	Ledger *budget.Ledger
	LLM    llm.Provider

	// Retriever selects candidate pairs. Nil takes LexicalRetriever.
	Retriever Retriever

	// MaxCandidatesPerClaim bounds fan-out per claim. Zero takes the default.
	MaxCandidatesPerClaim int

	// BatchSize is how many pairs go into one adjudication call. Zero takes the
	// default.
	BatchSize int

	// StalenessGap is how far apart two publication dates must be for a
	// contradiction to read as staleness (§11.2). Zero takes the default.
	StalenessGap time.Duration

	// MaxShareOfBudget caps cumulative verification spend as a fraction of the
	// session's whole budget. Zero takes DefaultMaxShareOfBudget.
	//
	// A ceiling is not optional here. Pair count grows with the square of the
	// claim count before the per-claim cap bites, so a session that gathers a lot
	// of claims can spend more verifying them than it spent finding them. On the
	// real 13-claim run, one call per pair would have cost about a third of the
	// whole session.
	MaxShareOfBudget float64

	Log *slog.Logger

	// spent accumulates this Verifier's charges, so the share cap binds across
	// passes rather than per pass.
	spent int64
}

// DefaultMaxShareOfBudget is the cumulative verification allowance.
//
// 0.25 is a guess and flagged as one, in the same category as §9.1's fan-out
// taper: nothing here can yet say what fraction of a session SHOULD go to
// verification, because that depends on how often the graph changes a conclusion —
// which is what §14.2's corpus measures. Set generously on purpose; a ceiling that
// binds before the graph is useful would make the Verifier look worthless for
// reasons of arithmetic.
const DefaultMaxShareOfBudget = 0.25

// Result reports what one pass did.
type Result struct {
	ClaimsVerified int
	PairsRetrieved int
	// PairsDecidedFree were settled by a mechanical rule, with no model call.
	PairsDecidedFree int
	PairsJudged      int
	// PairsUnjudged is pairs a batch did not return a verdict for, plus pairs
	// skipped because the allowance ran out.
	PairsUnjudged int
	EdgesWritten  int
	Calls         int
	Spent         int64

	// Degraded says what the pass could not finish (§9.5). Empty when complete.
	Degraded string
}

// ErrNoProvider means the Verifier was built without an LLM.
var ErrNoProvider = errors.New("verifier: no model provider configured")

func (v *Verifier) logger() *slog.Logger {
	if v.Log != nil {
		return v.Log
	}
	return slog.Default()
}

func (v *Verifier) retriever() Retriever {
	if v.Retriever != nil {
		return v.Retriever
	}
	return LexicalRetriever{}
}

// Run performs one verification pass over a session.
//
// Never returns an error for running out of allowance or for a model that answered
// badly — both are Degraded. An error means the store or the ledger failed, which
// is the caller's problem rather than a research outcome.
func (v *Verifier) Run(ctx context.Context, sessionID string) (*Result, error) {
	res := &Result{}
	if v.LLM == nil {
		return res, ErrNoProvider
	}

	var (
		targets []*core.Claim
		pool    []*core.Claim
		edges   []*core.ClaimEdge
	)
	if err := v.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		if targets, err = q.ListUnverifiedClaims(ctx, sessionID, 0); err != nil {
			return err
		}
		if pool, err = q.ListClaims(ctx, sessionID, 0); err != nil {
			return err
		}
		edges, err = q.ListEdges(ctx, sessionID, 0)
		return err
	}); err != nil {
		return res, err
	}
	if len(targets) == 0 {
		return res, nil
	}

	// Pairs that already carry an edge. Only reachable when a previous pass was
	// interrupted: in the normal course a pair is generated once, when the later of
	// its two claims is verified.
	already := make(map[string]bool, len(edges))
	for _, e := range edges {
		already[newPairKey(e.FromID, e.ToID)] = true
	}

	pairs, free, err := CandidatePairs(ctx, v.retriever(), targets, pool,
		v.MaxCandidatesPerClaim, func(k string) bool { return already[k] })
	if err != nil {
		return res, err
	}
	res.PairsRetrieved = len(pairs) + len(free)
	res.PairsDecidedFree = len(free)

	verdicts := free
	consumed := 0
	for _, batch := range Batches(pairs, v.BatchSize) {
		judged, unjudged, stop := v.adjudicate(ctx, sessionID, batch, res)
		verdicts = append(verdicts, judged...)
		res.PairsUnjudged += len(unjudged)
		consumed += len(batch)

		if stop != nil {
			// Out of allowance, out of budget, or a fatal provider error. Keep what
			// was judged: an incomplete graph is worth more than none, and the
			// claims still need marking verified or the queue never drains.
			res.Degraded = stop.Error()
			break
		}
	}
	// Pairs the loop never reached. Counted from what it consumed rather than from
	// judged+unjudged, which double-counts the batch that triggered the stop.
	if remaining := len(pairs) - consumed; remaining > 0 {
		res.PairsUnjudged += remaining
	}
	res.PairsJudged = len(verdicts) - len(free)

	if err := v.persist(ctx, sessionID, targets, verdicts, res); err != nil {
		return res, err
	}
	return res, nil
}

// adjudicate reserves, calls, settles, and parses one batch.
//
// Reserve BEFORE the call, per §8.2. The order matters for the same reason it did
// in the planner: an after-the-fact reserve that gets refused because a ceiling
// just fired leaves the cost spent and unrecorded.
func (v *Verifier) adjudicate(ctx context.Context, sessionID string, batch []Pair, res *Result) ([]Judged, []Pair, error) {
	sess, err := v.session(ctx, sessionID)
	if err != nil {
		return nil, batch, err
	}

	allowance := v.remainingAllowance(sess)
	est := estimateBatch(sess.BudgetUnit, len(batch))
	if est > allowance {
		return nil, batch, fmt.Errorf("verification allowance exhausted (%d of %.0f%% of budget spent)",
			v.spent, v.share()*100)
	}
	if avail := sess.Available(); est > avail {
		est = avail
	}
	if est <= 0 {
		return nil, batch, fmt.Errorf("%w: nothing left to verify with", budget.ErrInsufficientBudget)
	}

	reservation, rerr := v.Ledger.Reserve(ctx, sessionID, est)
	if rerr != nil {
		return nil, batch, rerr
	}

	prompt, _ := adjudicateUserPrompt(batch)
	resp, callErr := v.LLM.Complete(ctx, llm.Request{
		// Cheap tier: this is a bounded comparison of two sentences, run many
		// times, which is exactly what §10.1's split exists for.
		Tier:      llm.TierCheap,
		System:    adjudicateSystemPrompt,
		Messages:  []llm.Message{llm.User(prompt)},
		MaxTokens: maxTokensForBatch(len(batch)),
	})

	// Settle unconditionally. The tokens were spent whether or not the response
	// parsed, and a reservation left held is budget neither spent nor available.
	var calls []core.ToolCall
	if resp != nil && !resp.Usage.IsZero() {
		calls = append(calls, core.ToolCall{
			SessionID: sessionID,
			Role:      core.RoleVerifier,
			Type:      core.CallLLM,
			Model:     resp.Model,
			Cost: core.Cost{
				InputTokens:  resp.Usage.InputTokens,
				OutputTokens: resp.Usage.OutputTokens,
			},
		})
	}
	settled, serr := v.Ledger.Settle(ctx, reservation, calls)
	if serr != nil {
		v.logger().WarnContext(ctx, "verifier: settle failed", "err", serr)
	} else {
		charged := settled.Cost.BudgetAmount(sess.BudgetUnit)
		v.spent += charged
		res.Spent += charged
		res.Calls++
	}

	if callErr != nil {
		// §9.5. A bad credential or an exhausted quota will fail every remaining
		// batch identically, and each attempt still takes a reservation and a
		// round trip — so stop rather than walking the whole list into the same
		// wall. Anything else is this batch's problem alone.
		if isFatal(callErr) {
			return nil, batch, callErr
		}
		v.logger().WarnContext(ctx, "verifier: adjudication call failed; skipping batch", "err", callErr)
		return nil, batch, nil
	}
	if resp.Refused {
		v.logger().WarnContext(ctx, "verifier: model refused to compare claims",
			"category", resp.RefusalCategory)
		return nil, batch, nil
	}

	judged, unjudged, perr := parseVerdicts(resp.Text, batch)
	if perr != nil {
		v.logger().WarnContext(ctx, "verifier: batch returned no usable verdicts", "err", perr)
	}
	return judged, unjudged, nil
}

// persist writes the edges and marks every target claim verified.
//
// Both in one transaction. A pass that wrote edges but failed to mark the claims
// would regenerate the same pairs next time and pay for them again; one that
// marked them without the edges would lose the graph permanently.
func (v *Verifier) persist(ctx context.Context, sessionID string, targets []*core.Claim, verdicts []Judged, res *Result) error {
	edges := Edges(sessionID, verdicts, v.StalenessGap)

	scores := make([]store.ClaimScore, 0, len(targets))
	for _, c := range targets {
		// Confidence stays 0: §11.3 derives it from the graph, and that stage has
		// not landed. verified_at is what drains the work queue.
		scores = append(scores, store.ClaimScore{ClaimID: c.ID})
	}

	if err := v.Store.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if len(edges) > 0 {
			if err := tx.InsertEdges(ctx, edges); err != nil {
				return err
			}
		}
		return tx.ScoreClaims(ctx, scores)
	}); err != nil {
		return err
	}

	res.EdgesWritten = len(edges)
	res.ClaimsVerified = len(targets)
	return nil
}

func (v *Verifier) session(ctx context.Context, sessionID string) (*core.Session, error) {
	var s *core.Session
	err := v.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, sessionID)
		return err
	})
	return s, err
}

func (v *Verifier) share() float64 {
	if v.MaxShareOfBudget > 0 {
		return v.MaxShareOfBudget
	}
	return DefaultMaxShareOfBudget
}

// remainingAllowance is what is left of the verification share.
func (v *Verifier) remainingAllowance(sess *core.Session) int64 {
	ceiling := int64(float64(sess.Budget) * v.share())
	if left := ceiling - v.spent; left > 0 {
		return left
	}
	return 0
}

// isFatal reports whether an error will fail every remaining batch the same way
// (§9.5). A bad credential and an exhausted quota do not improve on retry.
func isFatal(err error) bool {
	return errors.Is(err, llm.ErrUnauthorized) || errors.Is(err, llm.ErrQuotaExceeded)
}

// estimateBatch guesses what one adjudication call costs.
//
// Deliberately generous. The estimate is a reservation, not a charge — settling
// replaces it with the real usage — and under-reserving is the failure that
// matters: §8.2's whole point is that the hold is taken before the call, so a hold
// smaller than the call trips the overshoot detector.
func estimateBatch(unit core.BudgetUnit, pairs int) int64 {
	// ~350 tokens of instructions, ~120 in and ~60 out per pair, doubled for
	// headroom.
	tokens := int64(2 * (350 + pairs*180))
	if unit == core.BudgetTokens {
		return tokens
	}
	// USD mode: price it as cheap-tier work. A coarse rate rather than a pricing
	// lookup, because this is a reservation and the settle carries the truth.
	const microsPerKTokenCheap = 2
	return maxInt64(1, tokens*microsPerKTokenCheap/1000)
}

// maxTokensForBatch leaves room for a verdict per pair plus a reasoning model's
// preamble.
//
// The failure this avoids is measured: a 3B model given too small an output
// allowance spends it all on reasoning and returns empty content with
// finish_reason "length" (llm.ErrEmptyOutput). Unused output tokens are not
// billed, so there is no reason to be tight.
func maxTokensForBatch(pairs int) int {
	return 1500 + pairs*120
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// newPairKey is Pair.Key for two bare IDs, used to index existing edges.
func newPairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}
