package cache_test

import (
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
		"http://arxiv.org/abs/2401.13660",            // scheme
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

// TestQueryKeyFoldsWordOrderButNotWording. "MambaByte PG-19 results" and "PG-19
// results MambaByte" are the same search; "what does X achieve" and "why does X
// fail" are not.
func TestQueryKeyFoldsWordOrderButNotWording(t *testing.T) {
	same := []string{
		"MambaByte PG-19 results",
		"PG-19 results MambaByte",
		"mambabyte pg-19 results",
		"  MambaByte,  PG-19   results!  ",
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

// TestKeyspacesDoNotCollide: a URL, a query and a DOI must never share a key.
func TestKeyspacesDoNotCollide(t *testing.T) {
	keys := map[string]string{
		"url":   cache.URLKey("https://example.com/x"),
		"query": cache.QueryKey("example com x"),
		"doi":   cache.DOIKey("10.1234/example"),
	}
	seen := map[string]string{}
	for kind, k := range keys {
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s produced the same key %q", kind, other, k)
		}
		seen[k] = kind
	}
}
