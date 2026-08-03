package cache_test

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/cache"
)

// TestSameDocumentFoldsToOneKey. §9.3's concrete win: two differently-worded
// queries converging on one page should pay for one fetch. That only happens if
// the links a search provider returns for the same article normalize together,
// and aggregators and newsletters are exactly where duplicate links come from.
func TestSameDocumentFoldsToOneKey(t *testing.T) {
	canonical := cache.URLKey("https://arxiv.org/abs/2401.13660")

	same := []string{
		"https://arxiv.org/abs/2401.13660",
		"https://ARXIV.ORG/abs/2401.13660",           // host case
		"https://www.arxiv.org/abs/2401.13660",       // www
		"https://arxiv.org/abs/2401.13660/",          // trailing slash
		"https://arxiv.org/abs/2401.13660#section-3", // fragment
		"https://arxiv.org:443/abs/2401.13660",       // default port
		"https://arxiv.org/abs/2401.13660?utm_source=newsletter&utm_medium=email",
		"https://arxiv.org/abs/2401.13660?fbclid=abc123",
		"https://user:pass@arxiv.org/abs/2401.13660", // credentials
	}
	for _, u := range same {
		if got := cache.URLKey(u); got != canonical {
			t.Errorf("%s\n  key %q\n want %q", u, got, canonical)
		}
	}
}

// TestDifferentDocumentsStayDistinct. Over-normalizing is worse than
// under-normalizing: a cache that treats two pages as one returns evidence from
// a page the claim does not cite, which is a correctness failure. A missed hit
// only costs a fetch.
func TestDifferentDocumentsStayDistinct(t *testing.T) {
	base := cache.URLKey("https://example.com/article?id=42")

	different := []string{
		"https://example.com/article?id=43",      // a load-bearing parameter
		"https://example.com/article",            // no parameter at all
		"https://example.com/other?id=42",        // different path
		"https://other.example/article?id=42",    // different host
		"https://example.com:8080/article?id=42", // non-default port
		"https://blog.example.com/article?id=42", // subdomain is not www
	}
	for _, u := range different {
		if cache.URLKey(u) == base {
			t.Errorf("%s folded into the same key as the base URL", u)
		}
	}
}

// TestParameterOrderDoesNotMatter, or a provider that emits parameters in map
// order produces a different key each time.
func TestParameterOrderDoesNotMatter(t *testing.T) {
	a := cache.URLKey("https://example.com/x?b=2&a=1&c=3")
	b := cache.URLKey("https://example.com/x?c=3&a=1&b=2")
	if a != b {
		t.Errorf("parameter order changed the key:\n  %q\n  %q", a, b)
	}
}

// TestUnparseableURLsStillKey rather than collapsing to an empty key that would
// make every malformed link a cache hit for every other one.
func TestUnparseableURLsStillKey(t *testing.T) {
	a := cache.URLKey("not a url at all")
	b := cache.URLKey("also not a url")
	if a == "" || b == "" {
		t.Error("a malformed URL produced an empty key")
	}
	if a == b {
		t.Error("two different malformed URLs share a key")
	}
}

// TestQueryKeyFoldsFormattingOnly. Casing, punctuation and whitespace are noise
// a planner produces across replans; word order is not.
func TestQueryKeyFoldsFormattingOnly(t *testing.T) {
	same := []string{
		"MambaByte PG-19 results",
		"mambabyte pg-19 results",
		"  MambaByte,  PG-19   results!  ",
		"MambaByte... PG-19 -- results?",
	}
	first := cache.QueryKey(same[0])
	for _, q := range same[1:] {
		if cache.QueryKey(q) != first {
			t.Errorf("%q did not fold into the same key", q)
		}
	}

	if cache.QueryKey("what does MambaByte achieve") == cache.QueryKey("why does MambaByte fail") {
		t.Error("two genuinely different questions share a key")
	}
}

// TestQueryKeyPreservesTheDirectionOfARelation. An earlier version sorted the
// words, so these collided — and the second question was then completed as
// skipped_cache and never researched, while the digest was credited with the
// first one's claims.
func TestQueryKeyPreservesTheDirectionOfARelation(t *testing.T) {
	for _, p := range [][2]string{
		{"did Acme acquire Beta", "did Beta acquire Acme"},
		{"is drug A safer than drug B", "is drug B safer than drug A"},
		{"does smoking cause cancer", "does cancer cause smoking"},
		{"is X faster than Y", "is Y faster than X"},
		{"does A cause B or does B cause A", "does B cause A or does A cause B"},
	} {
		if cache.QueryKey(p[0]) == cache.QueryKey(p[1]) {
			t.Errorf("%q and %q share a key — word order carries the relation", p[0], p[1])
		}
	}
}

// TestNonLatinQueriesStillKey. The earlier character class was [a-z0-9-], so any
// query in another script produced an empty key and silently disabled lead-level
// caching for it.
func TestNonLatinQueriesStillKey(t *testing.T) {
	for _, q := range []string{
		"バイトレベル言語モデルの現状",
		"каковы результаты MambaByte",
		"字节级语言模型",
	} {
		if cache.QueryKey(q) == "" {
			t.Errorf("QueryKey(%q) is empty — caching is silently disabled for this script", q)
		}
	}
	if cache.QueryKey("字节级语言模型") == cache.QueryKey("バイトレベル言語モデルの現状") {
		t.Error("two different non-Latin queries share a key")
	}
}

// TestPlaintextAndTLSAreDifferentDocuments. Content fetched over http can be
// rewritten by anyone on the path; storing it under the https key means a later
// hit on the https URL serves those bytes with no fetch, quotes verify against
// the attacker's text, and claims cite a URL that was never retrieved.
func TestPlaintextAndTLSAreDifferentDocuments(t *testing.T) {
	if cache.URLKey("http://example.com/doc") == cache.URLKey("https://example.com/doc") {
		t.Error("plaintext content is stored under the TLS key")
	}
}

// TestContentSelectingParamsAreNotTreatedAsTracking. "ref" and "source" look
// like tracking tags and often are, but they also select content — GitHub uses
// ?ref=<branch>. Folding them merges two different documents.
func TestContentSelectingParamsAreNotTreatedAsTracking(t *testing.T) {
	for _, p := range [][2]string{
		{"https://gh.example/f.md?ref=main", "https://gh.example/f.md?ref=attacker-branch"},
		{"https://v.example/view?source=a", "https://v.example/view?source=b"},
	} {
		if cache.URLKey(p[0]) == cache.URLKey(p[1]) {
			t.Errorf("%s and %s folded together", p[0], p[1])
		}
	}
	// Genuine tracking tags must still fold.
	if cache.URLKey("https://a.example/x?utm_source=news") != cache.URLKey("https://a.example/x") {
		t.Error("a real tracking tag was not stripped")
	}
}

func TestEmptyQueryHasNoKey(t *testing.T) {
	for _, q := range []string{"", "   ", "!!!", "?? ..."} {
		if got := cache.QueryKey(q); got != "" {
			t.Errorf("QueryKey(%q) = %q, want empty", q, got)
		}
	}
}

// TestDOIKeyNormalizesPrefixes. M6 will resolve the same DOI from a publisher
// page, an Unpaywall copy and an arXiv mirror — the case URL keying cannot fold.
func TestDOIKeyNormalizesPrefixes(t *testing.T) {
	canonical := cache.DOIKey("10.48550/arXiv.2401.13660")
	for _, d := range []string{
		"10.48550/arxiv.2401.13660",
		"doi:10.48550/arXiv.2401.13660",
		"https://doi.org/10.48550/arXiv.2401.13660",
		"http://doi.org/10.48550/arXiv.2401.13660",
		"  10.48550/arXiv.2401.13660  ",
	} {
		if got := cache.DOIKey(d); got != canonical {
			t.Errorf("%s\n  key %q\n want %q", d, got, canonical)
		}
	}
	if cache.DOIKey("") != "" {
		t.Error("an empty DOI produced a key")
	}
}

// TestKeyspacesDoNotCollide. The literal u:/q:/d: prefixes mean the three
// differ in their first byte by construction, so this can only fail if a
// prefix is dropped — which is exactly the regression worth catching, since the
// three key types share one map.
func TestKeyspacesDoNotCollide(t *testing.T) {
	keys := map[string]string{
		"url":   cache.URLKey("https://example.com/x"),
		"query": cache.QueryKey("example com x"),
		"doi":   cache.DOIKey("10.1234/example"),
	}
	seen := map[string]string{}
	for kind, k := range keys {
		if k == "" {
			t.Errorf("%s produced an empty key", kind)
			continue
		}
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s produced the same key %q", kind, other, k)
		}
		seen[k] = kind
	}

	// The property that actually matters: a key must carry its namespace, so a
	// URL that happens to hash like a query cannot be served for it.
	for kind, k := range keys {
		if !strings.Contains(k[:2], ":") {
			t.Errorf("%s key %q has no namespace prefix", kind, k)
		}
	}
}
