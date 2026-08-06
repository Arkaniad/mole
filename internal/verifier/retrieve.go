// Package verifier builds and scores the claim graph (§11).
//
// Three stages, in order: retrieve candidate pairs worth comparing, infer the
// edge between each pair, then derive confidence from the resulting graph. The
// split is what keeps the expensive stage small — every candidate pair costs a
// model call, so retrieval decides the bill.
//
// §11.1's rule shapes all of it: the Verifier operates over the session's whole
// claim set, not the batch one lead produced. A contradiction between two claims
// found by different leads is the case rev 1 structurally could not see, and it is
// the case that matters most.
package verifier

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/lajosdeme/mole/internal/core"
)

// Retriever finds the claims worth comparing against a target.
//
// A cheap filter in front of an expensive judgement. Recall is what matters here
// and precision barely does: a spurious candidate costs one adjudication and comes
// back labelled `unrelated`, while a missed one is an edge the graph never has and
// a contradiction nobody sees.
//
// An interface because §11.2 leaves the embedding question open, and the sketch
// files it as a cost-versus-dependency decision (per-claim embedding cost against
// the weight of another provider). LexicalRetriever answers it with "not yet":
// free, deterministic, replayable, and it needs no network. If eval data shows
// embeddings retrieve pairs this misses, they drop in here.
type Retriever interface {
	// Candidates returns up to max claims from pool that may relate to target,
	// most promising first. Never returns target itself.
	Candidates(ctx context.Context, target *core.Claim, pool []*core.Claim, max int) ([]*core.Claim, error)
}

// LexicalRetriever scores claim pairs by IDF-weighted cosine similarity over
// their words.
//
// IDF is computed over the session's own claims, which is what makes this work
// without tuning. Every claim in a session about MambaByte contains "MambaByte",
// so that term carries almost no information about which claims relate to each
// other; "subword", "tokenization" and "1.31" carry most of it. A retriever
// weighting all shared words equally ranks by topic and returns the whole session.
//
// Cosine rather than raw overlap because claim length varies: a long claim shares
// more words with everything by accident, and would otherwise dominate every
// candidate list.
type LexicalRetriever struct{}

// Candidates implements Retriever.
func (LexicalRetriever) Candidates(_ context.Context, target *core.Claim, pool []*core.Claim, max int) ([]*core.Claim, error) {
	if target == nil || len(pool) == 0 {
		return nil, nil
	}
	if max <= 0 {
		max = DefaultMaxCandidates
	}

	// IDF over the pool INCLUDING the target, so a term unique to the target is
	// still rare rather than absent.
	idf := idfOver(pool, target)

	targetVec := weigh(tokenize(target.Text), idf)
	if len(targetVec) == 0 {
		// Nothing but stopwords. Returning the whole pool would spend a model call
		// per claim to learn that a claim with no content words relates to nothing.
		return nil, nil
	}

	type scored struct {
		claim *core.Claim
		score float64
	}
	var ranked []scored
	for _, c := range pool {
		if c == nil || c.ID == target.ID {
			continue
		}
		s := cosine(targetVec, weigh(tokenize(c.Text), idf))
		if s <= 0 {
			// No shared content word at all. Not a judgement call — there is
			// nothing for a model to compare.
			continue
		}
		ranked = append(ranked, scored{c, s})
	}

	// Score descending, then ID, so a tie does not reorder between runs — a
	// candidate list that shuffles makes a cassette replay miss.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].claim.ID < ranked[j].claim.ID
	})
	if len(ranked) > max {
		ranked = ranked[:max]
	}

	out := make([]*core.Claim, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.claim)
	}
	return out, nil
}

// DefaultMaxCandidates bounds how many pairs one claim can put up for
// adjudication.
//
// The cap, not a similarity threshold, is what bounds cost. A threshold would need
// a number nobody can justify, and picking it wrong silently drops real edges;
// the cap costs at most this many calls per claim and is honest about being
// arbitrary.
const DefaultMaxCandidates = 8

// ---------------------------------------------------------------------------
// Lexical scoring
// ---------------------------------------------------------------------------

// idfOver computes inverse document frequency per term across the claim pool.
//
// Smoothed as log((N+1)/(df+0.5)) rather than log(N/df). The unsmoothed form
// returns exactly 0 for a term appearing in every claim, which zeroes the score of
// a two-claim pool that shares everything — the smallest pool the Verifier ever
// sees, and the one where it must still say "these two are duplicates".
func idfOver(pool []*core.Claim, extra *core.Claim) map[string]float64 {
	df := map[string]int{}
	n := 0

	count := func(c *core.Claim) {
		if c == nil {
			return
		}
		n++
		for t := range tokenSet(c.Text) {
			df[t]++
		}
	}
	seen := map[string]bool{}
	for _, c := range pool {
		if c != nil {
			seen[c.ID] = true
		}
		count(c)
	}
	if extra != nil && !seen[extra.ID] {
		count(extra)
	}

	out := make(map[string]float64, len(df))
	for t, d := range df {
		out[t] = math.Log(float64(n+1) / (float64(d) + 0.5))
	}
	return out
}

// term is one weighted term of a claim vector.
type term struct {
	tok string
	w   float64
}

// weigh turns a token set into an IDF-weighted vector, sorted by term.
//
// A sorted SLICE rather than a map, because the vector gets summed and floating
// point addition is not associative. Ranging a map gives Go's deliberately
// randomized order, so the same two claims scored twice produced sums differing
// in the last bits — measured at five distinct values over 2000 identical calls.
//
// That is not a rounding curiosity. Candidates compares scores with ==, so a
// one-ULP difference skips the ID tiebreak that exists to keep ties stable, and
// because the ranked list is then cut to the top N, a flip at the boundary
// changes which candidates are SELECTED rather than merely their order. A
// different candidate set is a different pair set, a different batch count, and
// a different graph — from identical inputs.
func weigh(tokens []string, idf map[string]float64) []term {
	// Presence, not frequency: a claim is one sentence, so a repeated word is
	// grammar rather than emphasis.
	seen := make(map[string]float64, len(tokens))
	for _, t := range tokens {
		w, ok := idf[t]
		if !ok || w <= 0 {
			continue
		}
		seen[t] = w
	}
	out := make([]term, 0, len(seen))
	for t, w := range seen {
		out = append(out, term{tok: t, w: w})
	}
	// The map above is iterated in random order; this is what makes the result
	// deterministic regardless.
	sort.Slice(out, func(i, j int) bool { return out[i].tok < out[j].tok })
	return out
}

// cosine scores two weighted vectors.
//
// A merge of two sorted slices rather than a map lookup per term: deterministic
// by construction, and cheaper — no hashing, and it touches each term once.
func cosine(a, b []term) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot float64
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i].tok < b[j].tok:
			i++
		case a[i].tok > b[j].tok:
			j++
		default:
			dot += a[i].w * b[j].w
			i++
			j++
		}
	}
	if dot == 0 {
		return 0
	}
	return dot / (norm(a) * norm(b))
}

func norm(v []term) float64 {
	var sum float64
	for _, t := range v {
		sum += t.w * t.w
	}
	return math.Sqrt(sum)
}

func tokenSet(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range tokenize(s) {
		out[t] = struct{}{}
	}
	return out
}

// tokenize splits claim text into comparable terms.
//
// Words and numbers are kept separately: a shared number is among the strongest
// signals two claims are about the same fact ("1.31 bits per byte", "2.6×
// speedup"), and stemming or splitting one destroys it. Decimal points inside a
// number survive; everything else is a separator.
func tokenize(s string) []string {
	var (
		out []string
		cur strings.Builder
		// numeric tracks whether the token being built started with a digit, so a
		// '.' can be kept inside 1.31 and dropped at the end of a sentence.
		numeric bool
	)
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		t := strings.Trim(cur.String(), ".")
		cur.Reset()
		if t == "" {
			return
		}
		if !numeric {
			t = fold(t)
			if t == "" || stopwords[t] {
				return
			}
			// Single letters carry no signal and match everything.
			if len(t) < 2 {
				return
			}
		}
		out = append(out, t)
	}

	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsDigit(r):
			if cur.Len() == 0 {
				numeric = true
			}
			cur.WriteRune(r)
		case unicode.IsLetter(r):
			if cur.Len() == 0 {
				numeric = false
			}
			// A letter inside a number ends it: "2.6×" is 2.6, and "v2" is v2.
			if numeric {
				flush()
				numeric = false
			}
			cur.WriteRune(r)
		case r == '.' && numeric && cur.Len() > 0:
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// fold strips the inflections that would otherwise hide a match.
//
// "compares" and "compared", "model" and "models", "outperforms" and
// "outperform" — claim text about the same fact rarely agrees on tense or number,
// and an unstemmed retriever misses the pair. Deliberately crude and deliberately
// aggressive: over-folding costs one adjudication that returns `unrelated`, while
// under-folding loses the edge permanently.
//
// Not a real stemmer. A real one is a dependency and a table, and nothing here has
// shown it would retrieve a pair this misses.
func fold(t string) string {
	// Order matters: check longer suffixes first, or "ies" is handled as "s".
	switch {
	case strings.HasSuffix(t, "ies") && len(t) > 4:
		return t[:len(t)-3] + "y"
	case strings.HasSuffix(t, "sses") && len(t) > 5:
		return t[:len(t)-2]
	case strings.HasSuffix(t, "ing") && len(t) > 5:
		t = t[:len(t)-3]
	case strings.HasSuffix(t, "ed") && len(t) > 4:
		t = t[:len(t)-2]
	case strings.HasSuffix(t, "es") && len(t) > 4:
		t = t[:len(t)-2]
	case strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") && len(t) > 3:
		t = t[:len(t)-1]
	}
	// A doubled final consonant left by -ing/-ed: "scanning" -> "scann" -> "scan".
	if n := len(t); n > 3 && t[n-1] == t[n-2] && !isVowel(t[n-1]) {
		t = t[:n-1]
	}

	// The silent 'e' has to go, or the two forms of a word ending in one never
	// converge — which is most of the vocabulary claims are written in:
	//
	//	sequences -> sequenc   but  sequence -> sequence
	//	tokenizes -> tokeniz   but  tokenize -> tokenize
	//	scaling   -> scal      but  scale    -> scale
	//
	// All three pairs were live misses until this ran: stripping the suffix
	// removes the 'e' from the inflected form and leaves it on the base one.
	if n := len(t); n > 3 && t[n-1] == 'e' {
		t = t[:n-1]
	}
	return t
}

func isVowel(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}

// stopwords are the words that discriminate nothing.
//
// IDF already suppresses the ubiquitous ones, but not to zero, and they are
// numerous enough that their combined weight moves a cosine score. Removing them
// is cheaper than tuning around them.
//
// The second group is the one worth explaining: verbs describing the act of
// asserting or enabling rather than any subject matter. They are not frequent
// enough for IDF to suppress, so they read as rare and therefore meaningful —
// which is backwards. A claim about resuming a CUDA kernel was matched to one
// about UTF-8 tokenization purely because both said "enabling", and a model call
// would have been spent learning they are unrelated.
//
// Entries are the FOLDED forms, since tokenize folds before it checks this.
var stopwords = map[string]bool{
	// Assertion and enablement verbs.
	"achiev": true, "allow": true, "becom": true, "consist": true,
	"demonstrat": true, "enabl": true, "exist": true, "giv": true,
	"includ": true, "indicat": true, "involv": true, "mak": true,
	"occur": true, "provid": true, "report": true, "result": true,
	"show": true, "suggest": true, "tak": true, "us": true,

	// Function words.
	"a": true, "about": true, "all": true, "also": true, "an": true, "and": true,
	"any": true, "ar": true, "are": true, "as": true, "at": true, "be": true,
	"been": true, "both": true, "but": true, "by": true, "can": true, "do": true,
	"doe": true, "for": true, "from": true, "ha": true, "had": true, "has": true,
	"have": true, "how": true, "in": true, "into": true, "is": true, "it": true,
	"its": true, "may": true, "more": true, "most": true, "much": true,
	"no": true, "not": true, "of": true, "on": true, "one": true, "only": true,
	"or": true, "other": true, "out": true, "over": true, "same": true,
	"some": true, "such": true, "than": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true, "these": true,
	"they": true, "thi": true, "this": true, "those": true, "to": true,
	"up": true, "use": true, "very": true, "wa": true, "was": true, "we": true,
	"were": true, "what": true, "when": true, "where": true, "which": true,
	"while": true, "who": true, "will": true, "with": true, "would": true,
}
