package verifier

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"log/slog"
)

// Relation is what an adjudicator may say about a pair of claims.
//
// Deliberately not core.EdgeKind. Two differences, both load-bearing:
//
//   - `unrelated` is a valid and common verdict that stores no edge. Making it
//     representable is what lets a pass distinguish "judged, no relation" from
//     "not judged", without an edge table full of rows recording absence.
//   - `supersedes` is absent. §11.2 derives it from PublishedAt, and a model
//     cannot see publication dates — asking it to rank two claims by recency
//     invites invention.
type Relation string

const (
	RelSupports    Relation = "supports"
	RelContradicts Relation = "contradicts"
	RelDuplicate   Relation = "duplicate_of"
	RelRefines     Relation = "refines"
	RelUnrelated   Relation = "unrelated"
)

func (r Relation) Valid() bool {
	switch r {
	case RelSupports, RelContradicts, RelDuplicate, RelRefines, RelUnrelated:
		return true
	}
	return false
}

// Pair is two claims put up for adjudication, in canonical order.
//
// A.ID < B.ID always. Not cosmetic: a pair reached from both ends is one pair,
// and the whole point of canonicalizing is that it gets judged and stored once.
type Pair struct {
	A, B *core.Claim
}

// Key identifies the pair independently of which end it was reached from.
func (p Pair) Key() string { return p.A.ID + "|" + p.B.ID }

func newPair(x, y *core.Claim) Pair {
	if x.ID > y.ID {
		x, y = y, x
	}
	return Pair{A: x, B: y}
}

// Judged is a pair with a verdict.
type Judged struct {
	Pair
	Relation  Relation
	Weight    float64
	Rationale string
	// DecidedBy names what produced the verdict — a model, or the mechanical rule
	// that made a call without one. Written to claim_edges.created_by, so a trace
	// can say why an edge exists.
	DecidedBy string
}

// DefaultStalenessGap is how far apart two publication dates must be before a
// contradiction reads as staleness rather than disagreement (§11.2).
//
// A guess, and flagged as one. Two papers a week apart that disagree are
// disagreeing; two a decade apart usually are not, and the boundary between them
// depends on how fast the field moves — §11.3 calls this "the question's
// volatility", and nothing here can measure it yet. §14.2's corpus is what would
// calibrate it.
const DefaultStalenessGap = 365 * 24 * time.Hour

// CandidatePairs builds the work list for one verification pass.
//
// Returns the pairs that need a model call, plus the ones settled without one.
// targets are the claims not yet verified; pool is the session's whole claim set,
// because §11.1's rule is that the Verifier sees the store rather than the batch
// one lead produced.
//
// judged reports whether a pair already carries an edge; nil means none do. Pairs
// are generated once per claim in the normal course — a pair (older, newer) is
// produced when newer is verified — so this only matters when a pass is re-run
// over a session that was interrupted.
func CandidatePairs(
	ctx context.Context,
	r Retriever,
	targets, pool []*core.Claim,
	maxPerClaim int,
	judged func(pairKey string) bool,
) (needJudging []Pair, decided []Judged, err error) {
	if r == nil || len(targets) == 0 || len(pool) == 0 {
		return nil, nil, nil
	}

	seen := map[string]bool{}
	for _, target := range targets {
		if target == nil {
			continue
		}
		candidates, cerr := r.Candidates(ctx, target, pool, maxPerClaim)
		if cerr != nil {
			return nil, nil, cerr
		}
		for _, c := range candidates {
			p := newPair(target, c)
			key := p.Key()
			// Deduped against this pass AND against the store. Two new claims in
			// one batch each retrieve the other, so without this every such pair
			// is judged twice — and on the real 13-claim run, canonicalizing cut
			// 96 ordered pairs to 54.
			if seen[key] {
				continue
			}
			seen[key] = true
			if judged != nil && judged(key) {
				continue
			}
			if j, ok := decideMechanically(p); ok {
				decided = append(decided, j)
				continue
			}
			needJudging = append(needJudging, p)
		}
	}

	// Stable order, so batches are reproducible and a cassette replays. Retrieval
	// is already deterministic; this keeps the pass deterministic too.
	sortPairs(needJudging)
	sort.SliceStable(decided, func(i, j int) bool { return decided[i].Key() < decided[j].Key() })
	return needJudging, decided, nil
}

func sortPairs(ps []Pair) {
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Key() < ps[j].Key() })
}

// decideMechanically settles the pairs a model would only confirm.
//
// Both rules read RAW text, never the retrieval tokenizer's output, and that
// distinction is the most dangerous thing in this package.
//
// The tokenizer treats "not" as a stopword — correct for retrieval, where recall
// is everything — so these two produce identical token sets and a cosine of 1.0:
//
//	MambaByte outperforms subword Transformers.
//	MambaByte does not outperform subword Transformers.
//
// A shortcut keyed on similarity would file the strongest possible contradiction
// as a duplicate. Which inverts the rule people expect: the model call is MOST
// load-bearing for the highest-scoring pairs, because negation hides in high
// lexical overlap. There is no similarity threshold at which auto-classifying is
// safe, and none is used.
func decideMechanically(p Pair) (Judged, bool) {
	// Byte-identical assertions, modulo case, spacing and enclosing punctuation.
	// Negation changes the bytes, so this cannot swallow one.
	if sameAssertion(p.A.Text, p.B.Text) {
		return Judged{
			Pair: p, Relation: RelDuplicate, Weight: 1.0,
			Rationale: "identical assertion text",
			DecidedBy: "mechanical:identical-text",
		}, true
	}

	// The same span of the same document, mined twice. Chunks overlap, so one
	// sentence reaches the extractor in two chunks and comes back as two claims.
	if p.A.Source != "" && p.A.Source == p.B.Source &&
		p.A.Quote != "" && p.A.Quote == p.B.Quote &&
		p.A.QuoteOffset == p.B.QuoteOffset {
		return Judged{
			Pair: p, Relation: RelDuplicate, Weight: 1.0,
			Rationale: "same quote span of the same source",
			DecidedBy: "mechanical:same-span",
		}, true
	}

	return Judged{}, false
}

// sameAssertion compares two claim texts for equality of assertion.
//
// Lowercase, collapse internal whitespace, strip enclosing punctuation. Nothing
// else — no stopword removal, no stemming. Every one of those would risk folding
// away a word that reverses the meaning, and the extractor's own artifacts are all
// this needs to survive: a trailing period, a leading markdown bullet (a real run
// produced `* MambaByte Model is a token-free…`), a doubled space.
func sameAssertion(a, b string) bool {
	na, nb := normalizeAssertion(a), normalizeAssertion(b)
	return na != "" && na == nb
}

func normalizeAssertion(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	return strings.Join(strings.Fields(s), " ")
}

// Edges turns verdicts into graph edges.
//
// Two things happen here rather than at the model boundary. `unrelated` produces
// nothing — it is a real answer, and storing it would fill the edge table with
// rows recording absence. And a contradiction between claims published far enough
// apart becomes `supersedes` (§11.2): a 2019 finding contradicted by a 2025 one is
// usually stale rather than disputed.
//
// The supersedes edge REPLACES the contradiction rather than joining it. Keeping
// both would have §11.3 penalize the newer claim's confidence for a disagreement
// it wins, and the direction of the supersedes edge already records the conflict.
func Edges(sessionID string, verdicts []Judged, stalenessGap time.Duration) []core.ClaimEdge {
	if stalenessGap <= 0 {
		stalenessGap = DefaultStalenessGap
	}

	var out []core.ClaimEdge
	for _, v := range verdicts {
		if v.Relation == RelUnrelated || !v.Relation.Valid() {
			continue
		}

		kind := core.EdgeKind(v.Relation)
		from, to := v.A, v.B
		rationale := v.Rationale

		if v.Relation == RelContradicts {
			if newer, older, ok := stalePair(v.Pair, stalenessGap); ok {
				kind = core.EdgeSupersedes
				from, to = newer, older
				gap := newer.PublishedAt.Sub(*older.PublishedAt)
				rationale = "contradiction across " +
					strconv.Itoa(int(gap.Hours()/24)) + " days of publication: " + rationale
			}
		}

		weight := v.Weight
		if weight <= 0 {
			weight = 1.0
		}
		out = append(out, core.ClaimEdge{
			SessionID: sessionID,
			FromID:    from.ID,
			ToID:      to.ID,
			Kind:      kind,
			Weight:    weight,
			CreatedBy: v.DecidedBy,
			Rationale: rationale,
		})
	}
	return out
}

// stalePair reports whether a contradicting pair is far enough apart in
// publication time to read as staleness, and which way the arrow points.
func stalePair(p Pair, gap time.Duration) (newer, older *core.Claim, ok bool) {
	if p.A.PublishedAt == nil || p.B.PublishedAt == nil {
		// No dates, no staleness claim. Most web claims have none, so this is the
		// common case and it correctly leaves the contradiction standing.
		return nil, nil, false
	}
	a, b := *p.A.PublishedAt, *p.B.PublishedAt
	switch {
	case a.Sub(b) >= gap:
		return p.A, p.B, true
	case b.Sub(a) >= gap:
		return p.B, p.A, true
	}
	return nil, nil, false
}

// Batches splits pairs into model calls.
//
// Batching is the difference between verification costing a third of the session
// and costing a twentieth. Measured on a real 13-claim run: 96 ordered pairs
// dedupe to 54, and one call per pair spends roughly 39k tokens against a run that
// spent 114k on the research itself. Batched at 8 it is seven calls and about 6k.
//
// The instruction block is what dominates a single-pair call — around 350 tokens
// of rules against 60 of claim text — so amortizing it over several pairs is where
// nearly all of the saving comes from.
func Batches(pairs []Pair, size int) [][]Pair {
	if size <= 0 {
		size = DefaultBatchSize
	}
	var out [][]Pair
	for i := 0; i < len(pairs); i += size {
		end := min(i+size, len(pairs))
		out = append(out, pairs[i:end])
	}
	return out
}

// DefaultBatchSize is how many pairs go into one adjudication call.
//
// A tradeoff rather than an optimum. Larger batches amortize the instructions
// further, but a small model's attention degrades across many parallel judgements
// and its output ceiling arrives sooner — the same 3B model that truncated mid-JSON
// during claim mining is the one that will be asked to emit eight verdicts. Partial
// responses are salvaged per verdict for exactly that reason, so an over-long batch
// degrades rather than failing.
const DefaultBatchSize = 8

// JudgePairs adjudicates pairs with no ledger and no store.
//
// The evaluation path. Verifier.Run reserves budget, settles it, writes edges and marks
// claims verified — all correct for research and all wrong for asking "would a different
// model have judged these better". Charging an eval re-run to the session would inflate its
// spend and corrupt cost-per-claim for the very session being examined, and writing edges
// would destroy the judgements being compared against.
//
// So this shares the prompt, the batching and the parsing with the real path, and shares
// nothing else. The cost is the evaluator's, not the session's, and it is not recorded —
// which is a deliberate exception to §8.1's every-call-writes-a-row rule, taken because the
// alternative is worse.
//
// Returns what it judged and what it could not, in that order.
func JudgePairs(ctx context.Context, p llm.Provider, model string, pairs []Pair, batchSize int, log *slog.Logger) ([]Judged, []Pair) {
	if log == nil {
		log = slog.Default()
	}
	var judged []Judged
	var unjudged []Pair

	for _, batch := range Batches(pairs, batchSize) {
		prompt, _ := adjudicateUserPrompt(batch)
		resp, err := p.Complete(ctx, llm.Request{
			Tier:      llm.TierCheap,
			Model:     model,
			System:    adjudicateSystemPrompt,
			Messages:  []llm.Message{llm.User(prompt)},
			MaxTokens: maxTokensForBatch(len(batch)),
		})
		if err != nil || resp == nil || resp.Refused {
			log.WarnContext(ctx, "judge: batch failed", "pairs", len(batch), "err", err)
			unjudged = append(unjudged, batch...)
			if isFatal(err) {
				// Every remaining batch fails the same way; stop rather than walking the
				// whole list into the same wall.
				break
			}
			continue
		}

		got, missed, perr := parseVerdicts(resp.Text, batch)
		if perr != nil {
			log.WarnContext(ctx, "judge: no usable verdicts", "err", perr)
		}
		judged = append(judged, got...)
		unjudged = append(unjudged, missed...)
	}
	return judged, unjudged
}
