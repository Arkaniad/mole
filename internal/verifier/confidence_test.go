package verifier

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
)

func claimAt(id, source string) *core.Claim {
	return &core.Claim{ID: id, Text: "an assertion", Source: source}
}

func dupEdge(a, b string) *core.ClaimEdge {
	return &core.ClaimEdge{FromID: a, ToID: b, Kind: core.EdgeDuplicateOf, Weight: 1}
}

func scoreOf(t *testing.T, claims []*core.Claim, edges []*core.ClaimEdge, id string) float64 {
	t.Helper()
	scores, _ := DeriveConfidence(claims, edges)
	for _, s := range scores {
		if s.ClaimID == id {
			return s.Confidence
		}
	}
	t.Fatalf("no score for %s", id)
	return 0
}

// TestPublisherIdentityIsRegistrableDomain is §11.3's "distinct domains/publishers,
// not distinct URLs", and the reason the public suffix list is worth a direct
// dependency.
//
// The naive last-two-labels shortcut — which fetch.DomainOf still uses for failure
// ranking, where it is untidy rather than wrong — gets this wrong in both
// directions. It merges every .co.uk publisher into one entity called "co.uk", and
// it splits nothing that should be split.
func TestPublisherIdentityIsRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"https://www.bbc.co.uk/news/x":     "bbc.co.uk",
		"https://theguardian.co.uk/a":      "theguardian.co.uk",
		"https://arxiv.org/abs/2401.13660": "arxiv.org",
		"https://blog.arxiv.org/post":      "arxiv.org",
		"https://alice.github.io/notes":    "alice.github.io",
		"https://bob.github.io/notes":      "bob.github.io",
		"https://sub.deep.example.com/p":   "example.com",
		"doi:10.1234/xyz":                  "doi.org",
		"https://EXAMPLE.com/A":            "example.com",
		"https://example.com./trailing":    "example.com",
	}
	for src, want := range cases {
		if got := PublisherOf(src); got != want {
			t.Errorf("PublisherOf(%q) = %q, want %q", src, got, want)
		}
	}

	// The pair that proves it: two UK publishers must not collapse into one.
	if PublisherOf("https://bbc.co.uk/a") == PublisherOf("https://theguardian.co.uk/a") {
		t.Error("two UK publishers were merged; corroboration would be undercounted")
	}
	// And two users of one hosting suffix must not merge either.
	if PublisherOf("https://alice.github.io/a") == PublisherOf("https://bob.github.io/a") {
		t.Error("two github.io users were merged")
	}
}

// TestSourceClassMatchesTheDomainNotTheURL. A substring match on ".gov" is how a
// hostile URL gets classified as a government source, and the URL is attacker-chosen
// in the sense that matters: mole followed a search result to get there.
func TestSourceClassMatchesTheDomainNotTheURL(t *testing.T) {
	hostile := []string{
		"https://nih.gov.attacker.example/paper",
		"https://arxiv.org.evil.example/abs/1",
		"https://nature.com.phish.example/article",
		"https://example.com/redirect?to=https://nih.gov/real",
		"https://example.com/.gov/paper",
	}
	for _, src := range hostile {
		if c := ClassOf(src); c != ClassUnknown {
			t.Errorf("ClassOf(%q) = %q, want unknown", src, c)
		}
	}

	legit := map[string]SourceClass{
		"https://www.nih.gov/paper":         ClassPrimary,
		"https://arxiv.org/abs/2401.13660":  ClassPrimary, // preprint: not refereed
		"https://www.nature.com/articles/x": ClassPeerReviewed,
		"doi:10.1234/xyz":                   ClassPeerReviewed,
		"https://en.wikipedia.org/wiki/X":   ClassSecondary,
		"https://someones-blog.example/p":   ClassUnknown,
		"https://www.ietf.org/rfc/rfc9110":  ClassPrimary,
	}
	for src, want := range legit {
		if got := ClassOf(src); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", src, got, want)
		}
	}
}

// TestPreprintsAreNotPeerReviewed on its own, because filing arXiv as refereed
// would be the single most misleading entry the class table could hold — and it is
// the most tempting, since arXiv is where most of the good material is.
func TestPreprintsAreNotPeerReviewed(t *testing.T) {
	for _, src := range []string{
		"https://arxiv.org/abs/2401.13660",
		"https://biorxiv.org/content/1",
		"https://medrxiv.org/content/1",
		"https://ssrn.com/abstract=1",
	} {
		if ClassOf(src) == ClassPeerReviewed {
			t.Errorf("%q classified as peer-reviewed", src)
		}
	}
	// And they must still outrank an unknown blog.
	if ClassOf("https://arxiv.org/abs/1").Weight() <= ClassOf("https://blog.example/p").Weight() {
		t.Error("a preprint carries no more weight than an anonymous blog")
	}
}

// TestManyURLsOnOneSiteIsOnePublisher is §11.3's corroboration-inflation risk,
// stated as a test. Five pages of one site agreeing is one site agreeing.
func TestManyURLsOnOneSiteIsOnePublisher(t *testing.T) {
	// Five claims, all duplicates of each other, all from one publisher.
	var oneSite []*core.Claim
	var edges []*core.ClaimEdge
	for i := 0; i < 5; i++ {
		oneSite = append(oneSite, claimAt(fmt.Sprintf("c_%02d", i),
			fmt.Sprintf("https://blog.example/post%d", i)))
		if i > 0 {
			edges = append(edges, dupEdge("c_00", fmt.Sprintf("c_%02d", i)))
		}
	}
	_, breakdown := DeriveConfidence(oneSite, edges)
	if len(breakdown) != 1 {
		t.Fatalf("%d clusters, want 1", len(breakdown))
	}
	if breakdown[0].Publishers != 1 {
		t.Errorf("%d publishers for five pages of one site", breakdown[0].Publishers)
	}
	inflated := breakdown[0].Confidence

	// The same five claims across five publishers must score higher.
	var fiveSites []*core.Claim
	var edges2 []*core.ClaimEdge
	for i := 0; i < 5; i++ {
		fiveSites = append(fiveSites, claimAt(fmt.Sprintf("c_%02d", i),
			fmt.Sprintf("https://site%d.example/post", i)))
		if i > 0 {
			edges2 = append(edges2, dupEdge("c_00", fmt.Sprintf("c_%02d", i)))
		}
	}
	_, breakdown2 := DeriveConfidence(fiveSites, edges2)
	if breakdown2[0].Publishers != 5 {
		t.Errorf("%d publishers for five distinct sites", breakdown2[0].Publishers)
	}
	if breakdown2[0].Confidence <= inflated {
		t.Errorf("five publishers (%.3f) scored no higher than one publisher across "+
			"five URLs (%.3f)", breakdown2[0].Confidence, inflated)
	}
}

// TestCorroborationSaturates. Diminishing returns are what stop a content farm
// republishing one claim across many domains from reaching certainty.
func TestCorroborationSaturates(t *testing.T) {
	var prev, prevGain float64
	for k := 1; k <= 6; k++ {
		got := corroboration(k)
		if got <= prev {
			t.Errorf("corroboration(%d) = %.4f, not above corroboration(%d) = %.4f",
				k, got, k-1, prev)
		}
		if got >= 1 {
			t.Errorf("corroboration(%d) = %.4f reached certainty", k, got)
		}
		if k > 1 {
			gain := got - prev
			if prevGain > 0 && gain >= prevGain {
				t.Errorf("gain from %d publishers (%.4f) did not shrink against the "+
					"previous step (%.4f)", k, gain, prevGain)
			}
			prevGain = gain
		}
		prev = got
	}
	// One source alone is a lead, not a finding.
	if corroboration(1) >= 0.5 {
		t.Errorf("a single publisher reaches %.3f", corroboration(1))
	}
}

// TestContradictionCostsMoreThanCorroborationGains. The ordering that matters: a
// claim three publishers agree on but one credibly disputes must not read as more
// settled than an unopposed claim from one publisher.
func TestContradictionCostsMoreThanCorroborationGains(t *testing.T) {
	// Three publishers agree, one disputes.
	disputed := []*core.Claim{
		claimAt("c_01", "https://a.example/p"),
		claimAt("c_02", "https://b.example/p"),
		claimAt("c_03", "https://c.example/p"),
		claimAt("c_99", "https://d.example/p"), // the objector
	}
	edges := []*core.ClaimEdge{
		dupEdge("c_01", "c_02"),
		dupEdge("c_01", "c_03"),
		{FromID: "c_01", ToID: "c_99", Kind: core.EdgeContradicts, Weight: 1},
	}
	got := scoreOf(t, disputed, edges, "c_01")

	// One publisher, unopposed.
	quiet := []*core.Claim{claimAt("c_01", "https://a.example/p")}
	base := scoreOf(t, quiet, nil, "c_01")

	if got >= base {
		t.Errorf("a disputed claim with three publishers (%.3f) scored at least as "+
			"high as an unopposed claim from one (%.3f)", got, base)
	}
}

// TestOneObjectorIsNotFive. The symmetric argument to corroboration: five
// contradicting claims from one site is one site disagreeing.
func TestOneObjectorIsNotFive(t *testing.T) {
	claims := []*core.Claim{claimAt("c_01", "https://a.example/p")}
	var edges []*core.ClaimEdge
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("c_9%d", i)
		claims = append(claims, claimAt(id, "https://objector.example/post"))
		edges = append(edges, &core.ClaimEdge{
			FromID: "c_01", ToID: id, Kind: core.EdgeContradicts, Weight: 1,
		})
	}
	// All five objections are duplicates of each other, so they are one objection.
	for i := 1; i < 5; i++ {
		edges = append(edges, dupEdge("c_90", fmt.Sprintf("c_9%d", i)))
	}

	_, breakdown := DeriveConfidence(claims, edges)
	for _, s := range breakdown {
		if s.Contradictors > 1 {
			t.Errorf("%d contradictors for one objecting publisher stating it five times",
				s.Contradictors)
		}
	}
}

// TestSupersededIsPenalizedAndSupersedingIsNot. Being overtaken is a mark against a
// claim; overtaking something is not (§11.2).
func TestSupersededIsPenalizedAndSupersedingIsNot(t *testing.T) {
	claims := []*core.Claim{
		claimAt("c_old", "https://a.example/2019"),
		claimAt("c_new", "https://b.example/2025"),
	}
	edges := []*core.ClaimEdge{
		{FromID: "c_new", ToID: "c_old", Kind: core.EdgeSupersedes, Weight: 1},
	}

	oldScore := scoreOf(t, claims, edges, "c_old")
	newScore := scoreOf(t, claims, edges, "c_new")
	unrelated := scoreOf(t, []*core.Claim{claimAt("c_new", "https://b.example/2025")}, nil, "c_new")

	if oldScore >= newScore {
		t.Errorf("superseded claim (%.3f) scored at least as high as the one that "+
			"superseded it (%.3f)", oldScore, newScore)
	}
	if newScore != unrelated {
		t.Errorf("superseding something changed the newer claim's score: %.3f vs %.3f",
			newScore, unrelated)
	}
}

// TestFailedGroundingIsNearFatal. A claim already passed the extraction-time
// verbatim check (§11.5), so failing a re-read means the evidence is not there —
// which should outweigh any amount of corroboration.
func TestFailedGroundingIsNearFatal(t *testing.T) {
	no, yes := false, true

	// Four publishers, peer-reviewed, but grounding failed.
	var strong []*core.Claim
	var edges []*core.ClaimEdge
	for i, host := range []string{"nature.com", "science.org", "cell.com", "pnas.org"} {
		c := claimAt(fmt.Sprintf("c_%02d", i), "https://www."+host+"/x")
		if i == 0 {
			c.Grounded = &no
		}
		strong = append(strong, c)
		if i > 0 {
			edges = append(edges, dupEdge("c_00", fmt.Sprintf("c_%02d", i)))
		}
	}
	failed := scoreOf(t, strong, edges, "c_00")

	// One anonymous blog, grounding never checked.
	weak := scoreOf(t, []*core.Claim{claimAt("c_00", "https://blog.example/p")}, nil, "c_00")

	if failed >= weak {
		t.Errorf("four peer-reviewed publishers with FAILED grounding (%.3f) scored at "+
			"least as high as one unchecked blog (%.3f)", failed, weak)
	}

	// And a confirmed check ranks above an unchecked one, since it was paid for.
	checked := claimAt("c_00", "https://blog.example/p")
	checked.Grounded = &yes
	if scoreOf(t, []*core.Claim{checked}, nil, "c_00") <= weak {
		t.Error("a confirmed grounding check earned nothing")
	}
}

// TestOneFailedCheckCondemnsTheCluster. Members of a duplicate cluster are the same
// assertion, so if one member's quote does not survive a re-read, the assertion is
// the thing in doubt — not that one row.
func TestOneFailedCheckCondemnsTheCluster(t *testing.T) {
	no, yes := false, true
	a := claimAt("c_01", "https://a.example/p")
	a.Grounded = &yes
	b := claimAt("c_02", "https://b.example/p")
	b.Grounded = &no

	_, breakdown := DeriveConfidence([]*core.Claim{a, b}, []*core.ClaimEdge{dupEdge("c_01", "c_02")})
	if len(breakdown) != 1 {
		t.Fatalf("%d clusters, want 1", len(breakdown))
	}
	if breakdown[0].Grounded == nil || *breakdown[0].Grounded {
		t.Errorf("cluster grounding = %v; one failed check did not condemn it", breakdown[0].Grounded)
	}
}

// TestDuplicateClusteringIsTransitiveAndOrderIndependent. A duplicates B, B
// duplicates C, so all three are one assertion — and the grouping must not depend
// on which order the edges came back in.
func TestDuplicateClusteringIsTransitiveAndOrderIndependent(t *testing.T) {
	claims := []*core.Claim{
		claimAt("c_01", "https://a.example/p"),
		claimAt("c_02", "https://b.example/p"),
		claimAt("c_03", "https://c.example/p"),
		claimAt("c_04", "https://d.example/p"), // unrelated
	}
	forward := []*core.ClaimEdge{dupEdge("c_01", "c_02"), dupEdge("c_02", "c_03")}
	backward := []*core.ClaimEdge{dupEdge("c_02", "c_03"), dupEdge("c_01", "c_02")}

	for name, edges := range map[string][]*core.ClaimEdge{"forward": forward, "backward": backward} {
		got := Clusters(claims, edges)
		if len(got) != 2 {
			t.Fatalf("%s: %d clusters, want 2", name, len(got))
		}
		if n := len(got[0].Claims); n != 3 {
			t.Errorf("%s: first cluster holds %d claims, want 3 (transitive)", name, n)
		}
		if got[0].Claims[0].ID != "c_01" || got[1].Claims[0].ID != "c_04" {
			t.Errorf("%s: clusters are %v / %v", name, got[0].IDs(), got[1].IDs())
		}
	}
}

// TestSupportsDoesNotMergeClaims. Merging on `supports` would delete exactly the
// corroboration §11.3 counts: two claims that support each other are two pieces of
// evidence, and one cluster of two has the publisher count of two, not of one.
func TestSupportsDoesNotMergeClaims(t *testing.T) {
	claims := []*core.Claim{
		claimAt("c_01", "https://a.example/p"),
		claimAt("c_02", "https://b.example/p"),
	}
	edges := []*core.ClaimEdge{
		{FromID: "c_01", ToID: "c_02", Kind: core.EdgeSupports, Weight: 1},
	}
	if got := Clusters(claims, edges); len(got) != 2 {
		t.Errorf("%d clusters from a supports edge, want 2", len(got))
	}
}

// TestEveryClaimIsScoredExactlyOnce. Confidence is written by ScoreClaims, and a
// claim missed here keeps whatever number it had — including the self-reported one
// slice 0 removed, on a database migrated from an older version.
func TestEveryClaimIsScoredExactlyOnce(t *testing.T) {
	claims := []*core.Claim{
		claimAt("c_01", "https://a.example/p"),
		claimAt("c_02", "https://b.example/p"),
		claimAt("c_03", "https://c.example/p"),
	}
	edges := []*core.ClaimEdge{dupEdge("c_01", "c_02")}

	scores, _ := DeriveConfidence(claims, edges)
	if len(scores) != len(claims) {
		t.Fatalf("%d scores for %d claims", len(scores), len(claims))
	}
	seen := map[string]int{}
	for _, s := range scores {
		seen[s.ClaimID]++
	}
	for _, c := range claims {
		if seen[c.ID] != 1 {
			t.Errorf("claim %s scored %d times", c.ID, seen[c.ID])
		}
	}
	// Cluster members share the cluster's number: they are the same assertion, so
	// the report's choice of wording must not change its stated confidence.
	if scores[0].Confidence != scores[1].Confidence {
		t.Errorf("cluster members scored differently: %.4f vs %.4f",
			scores[0].Confidence, scores[1].Confidence)
	}
}

// TestConfidenceStaysInRange across everything the graph can throw at it.
//
// The clamp is reachable: many peer-reviewed publishers plus a confirmed grounding
// check multiplies to 0.998 x 1.0 x 1.05, which is above 1. The first version of
// this test overrode the source to an unknown domain whenever it added publishers,
// so the highest weight and the highest corroboration never met and removing the
// clamp passed.
func TestConfidenceStaysInRange(t *testing.T) {
	no, yes := false, true
	peerReviewed := []string{"nature.com", "science.org", "cell.com", "plos.org",
		"pnas.org", "bmj.com", "nejm.org", "ieee.org", "acm.org", "jstor.org",
		"thelancet.com", "mdpi.com"}

	for _, g := range []*bool{nil, &yes, &no} {
		for _, publishers := range []int{0, 1, 3, 12} {
			var claims []*core.Claim
			var edges []*core.ClaimEdge
			for i := 0; i < publishers; i++ {
				id := fmt.Sprintf("c_%02d", i)
				// Strongest class AND maximum corroboration together, which is the
				// only combination that can exceed 1.
				c := claimAt(id, "https://www."+peerReviewed[i%len(peerReviewed)]+"/x")
				c.Grounded = g
				claims = append(claims, c)
				if i > 0 {
					edges = append(edges, dupEdge("c_00", id))
				}
			}
			if publishers == 0 {
				claims = append(claims, &core.Claim{ID: "c_00", Text: "x", Source: "not a url", Grounded: g})
			}
			scores, breakdown := DeriveConfidence(claims, edges)
			for _, s := range scores {
				if s.Confidence < 0 || s.Confidence > 1 {
					t.Errorf("publishers=%d grounded=%v: confidence %.4f outside 0-1",
						publishers, g, s.Confidence)
				}
			}
			// And confirm the case that stresses the clamp was actually built.
			if publishers == 12 && g == &yes {
				if len(breakdown) != 1 || breakdown[0].Class != ClassPeerReviewed ||
					breakdown[0].Publishers != 12 {
					t.Fatalf("the clamp-stressing case was not constructed: %+v", breakdown)
				}
			}
		}
	}
}

// TestDerivationIsExplained. §11.3 requires the number be explainable in a trace. A
// bare 0.62 is the kind of figure people either trust blindly or dismiss.
func TestDerivationIsExplained(t *testing.T) {
	no := false
	a := claimAt("c_01", "https://www.nature.com/x")
	a.Grounded = &no
	claims := []*core.Claim{a, claimAt("c_02", "https://b.example/p")}
	edges := []*core.ClaimEdge{
		{FromID: "c_01", ToID: "c_02", Kind: core.EdgeContradicts, Weight: 1},
	}
	_, breakdown := DeriveConfidence(claims, edges)

	var found string
	for _, s := range breakdown {
		if s.Class == ClassPeerReviewed {
			found = s.Explain
		}
	}
	if found == "" {
		t.Fatal("no peer-reviewed cluster found")
	}
	for _, want := range []string{"publisher", "peer-reviewed", "contradicted by 1", "grounding FAILED"} {
		if !strings.Contains(found, want) {
			t.Errorf("explanation omits %q: %s", want, found)
		}
	}
}
