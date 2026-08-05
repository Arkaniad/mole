package output

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/verifier"
)

// Duplicate collapse and disagreement disclosure (§11.2, §11.3).
//
// §11.2's rule: a duplicate_of cluster renders as ONE claim with N sources. Two
// things follow, and both were visibly wrong in the last live run.
//
// The report stops restating itself. That run produced
//
//	[1] MambaByte compares favorably to various subword baselines
//	[1] Compared to existing subword models, MambaByte is competitive
//
// as two separate bullets from one source — the same finding twice, which reads as
// two independent facts and is neither.
//
// And corroboration becomes visible. Three publishers agreeing is the strongest
// signal §11.3 has, and rendering them as three bullets hides it behind repetition
// rather than showing it as one claim with three citations.

// Finding is one assertion put in front of the synthesis model.
//
// A collapsed cluster, so Sources may hold several. Contradicts names the findings
// that dispute it — surfaced rather than resolved, because a reader who cannot see a
// disagreement cannot judge it, and §11.3's lowered confidence is a number the prose
// never shows.
type Finding struct {
	Claim *core.Claim
	// Sources are every distinct source asserting this, representative first.
	Sources []string
	// Publishers is how many DISTINCT publishers those sources represent (§11.3's
	// corroboration signal, which is not the same as the URL count).
	Publishers int
	// breadth is how many claims in the session came from this finding's sources.
	// A pre-Verifier tie-break only — see the ordering note on Findings.
	breadth int
	// Confidence is the cluster's derived figure.
	Confidence float64
	// Contradicts holds the indices, into the Findings slice, of findings that
	// dispute this one.
	Contradicts []int
	// Superseded is set when a later-published finding supersedes this one.
	Superseded bool

	// cluster is every claim collapsed into this finding, so a citation can carry
	// the quote its own source supplied.
	cluster []*core.Claim
	// order is the earliest position its claims held in the input, which arrives
	// from the store in created_at order. Used as the final tie-break.
	order int
	// disputes holds the representative claim IDs of findings that contradict this
	// one, recorded before the sort and resolved to indices after it.
	disputes []string
}

// Findings collapses claims into what the report should say.
//
// Ordered by DERIVED confidence (§11.3), never by the extractor's self-report. This
// ordering replaced a function that sorted by the latter under the same field name, so
// an uncalibrated number — one §11.3 describes as "mostly encoding fluency" — decided
// which claims led the answer and which were dropped at the cap. A fluent claim from
// one anonymous page outranked a claim three independent publishers agreed on.
//
// Before the Verifier scores a session every derived confidence is 0 and the sort has
// nothing to work with. Assertion strength is the wrong tie-break — preferring it is
// the exact behaviour that was removed — so ties fall back to publisher breadth, which
// is mechanical and claims nothing.
//
// Truncated to max AFTER collapsing. Collapsing first is what lets the cap count
// distinct assertions rather than repetitions; a cap applied to raw claims spends its
// whole budget on one page saying one thing eight ways.
func Findings(claims []*core.Claim, edges []*core.ClaimEdge, max int) []Finding {
	if len(claims) == 0 {
		return nil
	}

	// Input position per claim. This is the final tie-break, and it has to be:
	// claim IDs carry a millisecond timestamp and a random suffix, so a batch
	// inserted in one transaction shares the prefix and orders RANDOMLY. Sorting on
	// ID made the report's citation numbering differ between runs of one session —
	// unstable output, and a cassette recorded on one ordering misses on another.
	// ListClaims returns created_at order, which is both stable and meaningful.
	inputPos := make(map[string]int, len(claims))
	for i, c := range claims {
		if c != nil {
			inputPos[c.ID] = i
		}
	}

	// Claims per source across the whole session. The pre-Verifier tie-break: with
	// no graph every derived confidence is 0, and a source that also supports other
	// claims is more likely load-bearing than a page mentioned once. Mechanical, and
	// it claims nothing about quality — which is the point, since the alternative
	// that was removed was assertion strength.
	perSource := map[string]int{}
	for _, c := range claims {
		if c != nil {
			perSource[c.Source]++
		}
	}

	clusters := verifier.Clusters(claims, edges)
	out := make([]Finding, 0, len(clusters))
	// clusterOf resolves an edge endpoint to the finding it landed in.
	clusterOf := map[string]int{}

	for _, cl := range clusters {
		rep := cl.Representative()
		if rep == nil {
			continue
		}
		f := Finding{Claim: rep, Confidence: rep.Confidence, cluster: cl.Claims, order: len(claims)}
		for _, c := range cl.Claims {
			if p, ok := inputPos[c.ID]; ok && p < f.order {
				f.order = p
			}
		}

		seen := map[string]bool{}
		pubs := map[string]bool{}
		// Representative's source first, so the citation a reader checks first is
		// the strongest-classed one.
		for _, c := range append([]*core.Claim{rep}, cl.Claims...) {
			if c.Source == "" || seen[c.Source] {
				continue
			}
			seen[c.Source] = true
			f.Sources = append(f.Sources, c.Source)
			if p := verifier.PublisherOf(c.Source); p != "" {
				pubs[p] = true
			}
		}
		f.Publishers = len(pubs)
		for _, src := range f.Sources {
			f.breadth += perSource[src]
		}

		for _, c := range cl.Claims {
			clusterOf[c.ID] = len(out)
		}
		out = append(out, f)
	}

	// Disagreements, recorded by the opposing finding's REPRESENTATIVE CLAIM ID
	// rather than by its index.
	//
	// Indices do not survive the sort below. An earlier version assigned them here,
	// sorted `out` in place, and then tried to translate them using the sorted slice
	// as the pre-sort reference — which is incoherent, and passed only while the sort
	// happened not to reorder anything. When it did reorder, a disagreement pointed
	// at whatever finding now occupied the old position: worse than not disclosing
	// it, because it attributes a dispute to an unrelated claim.
	disputes := make([][]string, len(out))
	for _, e := range edges {
		if e == nil {
			continue
		}
		from, okFrom := clusterOf[e.FromID]
		to, okTo := clusterOf[e.ToID]
		if !okFrom || !okTo || from == to {
			continue
		}
		switch e.Kind {
		case core.EdgeContradicts:
			disputes[from] = appendUniqueStr(disputes[from], out[to].Claim.ID)
			disputes[to] = appendUniqueStr(disputes[to], out[from].Claim.ID)
		case core.EdgeSupersedes:
			// Directional: only the target is stale.
			out[to].Superseded = true
		}
	}
	for i := range out {
		out[i].disputes = disputes[i]
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		if out[i].Publishers != out[j].Publishers {
			return out[i].Publishers > out[j].Publishers
		}
		if out[i].breadth != out[j].breadth {
			return out[i].breadth > out[j].breadth
		}
		return out[i].order < out[j].order
	})

	// Now that the order is final, resolve the recorded IDs to indices.
	position := make(map[string]int, len(out))
	for i, f := range out {
		position[f.Claim.ID] = i
	}
	for i := range out {
		var idx []int
		for _, id := range out[i].disputes {
			if p, ok := position[id]; ok {
				idx = appendUnique(idx, p)
			}
		}
		sort.Ints(idx)
		out[i].Contradicts = idx
	}

	if max > 0 && len(out) > max {
		out = out[:max]
		// Drop references past the cap: a citation to a finding that is not in the
		// report points nowhere.
		for i := range out {
			out[i].Contradicts = keepBelow(out[i].Contradicts, max)
		}
	}
	return out
}

func appendUnique(xs []int, v int) []int {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func appendUniqueStr(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func keepBelow(xs []int, max int) []int {
	var out []int
	for _, x := range xs {
		if x < max {
			out = append(out, x)
		}
	}
	return out
}

// Disagreements returns the pairs a report must disclose, most confident first.
//
// Each pair appears once. §11.3 already lowers both sides' confidence, but that is a
// number the prose never shows, and a reader looking at two cited sentences has no
// way to know the pipeline found them incompatible.
func Disagreements(findings []Finding) [][2]int {
	var out [][2]int
	seen := map[[2]int]bool{}
	for i, f := range findings {
		for _, j := range f.Contradicts {
			if j < 0 || j >= len(findings) {
				continue
			}
			key := [2]int{min(i, j), max(i, j)}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// corroborationNote describes a finding's support for the trailing source list.
func corroborationNote(f Finding) string {
	var parts []string
	switch {
	case f.Publishers > 1:
		parts = append(parts, fmt.Sprintf("%d independent publishers", f.Publishers))
	case len(f.Sources) > 1:
		// Several URLs, one publisher. Worth saying: it looks like corroboration
		// and is not, which is §1275's inflation risk in a form a reader can see.
		parts = append(parts, fmt.Sprintf("%d pages from one publisher", len(f.Sources)))
	}
	if f.Superseded {
		parts = append(parts, "outdated")
	}
	if len(f.Contradicts) > 0 {
		parts = append(parts, "disputed")
	}
	if f.Claim != nil && f.Claim.Grounded != nil && !*f.Claim.Grounded {
		// The one flag a reader must not miss: the claim's own source, re-read,
		// does not support it (§11.5).
		//
		// Worded as a fragment, not a sentence. The earlier phrasing was a complete
		// clause — "source does NOT support this on re-reading" — and a 3B model
		// lifted it straight into the report body as the whole answer, asserting a
		// re-read verdict on claims that were never checked.
		parts = append(parts, "failed source re-read")
	}
	return strings.Join(parts, "; ")
}

// claims returns every claim in the finding's cluster.
//
// The representative is what the report quotes, but a citation's verified spans
// come from whichever claim cited that source — so a reader checking source [3]
// sees the quote that source actually supplied, not the representative's.
func (f Finding) claims() []*core.Claim {
	if f.cluster != nil {
		return f.cluster
	}
	if f.Claim == nil {
		return nil
	}
	return []*core.Claim{f.Claim}
}
