// Package cache stops a session paying twice for the same artifact (§9.3).
//
// Rev 1 did `if cache.SeenRecently(lead) { continue }`, which had two problems.
// The planner asked for something and got nothing back, so the next replan
// spawned an equivalent lead and the loop livelocked. And keying at the LEAD
// level missed the common case: two differently-worded queries surfacing the
// same page still paid for two fetches.
//
// Both fixes follow from keying on the ARTIFACT — a normalized URL, a DOI, a
// query hash — and from returning the stored result rather than skipping. A hit
// is an answer, not an absence.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// trackingParams are dropped from a URL before it is used as a key.
//
// Two links to the same article that differ only by campaign tag are the same
// artifact, and a search provider will happily return both. Without this the
// cache misses precisely where it would help most: aggregators and newsletters,
// which is where duplicate links come from.
var trackingParams = map[string]bool{
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true, "utm_id": true,
	"gclid": true, "fbclid": true, "msclkid": true, "dclid": true,
	"mc_cid": true, "mc_eid": true, "igshid": true, "twclid": true,
	"ref": true, "referrer": true, "source": true,
	"_hsenc": true, "_hsmi": true, "hsCtaTracking": true,
	"spm": true, "scid": true, "yclid": true,
}

// NormalizeURL reduces a URL to a stable identity for the same document.
//
// Deliberately conservative about what it drops. Over-normalizing is worse than
// under-normalizing here: a cache that treats two different pages as one
// returns evidence from a page the claim does not cite, which is a correctness
// failure, whereas a missed hit only costs a fetch.
//
// That is why the path is left alone apart from a trailing slash, and why query
// parameters other than known tracking tags are kept and sorted rather than
// stripped — `?id=42` and `?id=43` are different documents, and nothing here
// can tell which parameters are load-bearing for a given site.
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		// Unparseable: fall back to the literal string, lowercased. Still a
		// usable key, just a less forgiving one.
		return strings.ToLower(strings.TrimSpace(raw))
	}

	u.Scheme = strings.ToLower(u.Scheme)
	// http and https serve the same document often enough, and a redirect
	// between them is invisible by the time a URL reaches here.
	if u.Scheme == "http" {
		u.Scheme = "https"
	}

	host := strings.ToLower(u.Hostname())
	host = strings.TrimSuffix(host, ".")
	// "www." is a prefix, not a document.
	host = strings.TrimPrefix(host, "www.")
	if port := u.Port(); port != "" && !defaultPort(u.Scheme, port) {
		host = host + ":" + port
	}
	u.Host = host

	// A fragment addresses a position within a document, not a document.
	u.Fragment = ""
	u.RawFragment = ""
	// Credentials are not identity, and keeping them would put them in a key.
	u.User = nil

	q := u.Query()
	for k := range q {
		if trackingParams[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	// Encode sorts keys, so parameter order stops mattering.
	u.RawQuery = q.Encode()

	if u.Path == "" {
		u.Path = "/"
	} else if len(u.Path) > 1 {
		u.Path = strings.TrimSuffix(u.Path, "/")
	}

	return u.String()
}

func defaultPort(scheme, port string) bool {
	return (scheme == "https" && port == "443") || (scheme == "http" && port == "80")
}

// QueryKey is the key for a research query.
//
// Normalized to fold the trivial differences a planner produces across
// replans — casing, punctuation, word order — without pretending two genuinely
// different questions are one. Word order is folded because "MambaByte PG-19
// results" and "PG-19 results MambaByte" are the same search; wording is not,
// because "what does X achieve" and "why does X fail" are not.
func QueryKey(query string) string {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-'
	})
	if len(words) == 0 {
		return ""
	}
	sort.Strings(words)
	sum := sha256.Sum256([]byte(strings.Join(words, " ")))
	return "q:" + hex.EncodeToString(sum[:12])
}

// URLKey is the key for a fetched document.
func URLKey(raw string) string {
	n := NormalizeURL(raw)
	if n == "" {
		return ""
	}
	return "u:" + n
}

// DOIKey is the key for an academic identifier.
//
// Present now because §9.3 names it and because AcademicActor (M6) will resolve
// the same DOI from several routes — a publisher page, an Unpaywall copy, an
// arXiv mirror — which is the case URL keying cannot fold.
func DOIKey(doi string) string {
	d := strings.ToLower(strings.TrimSpace(doi))
	d = strings.TrimPrefix(d, "https://doi.org/")
	d = strings.TrimPrefix(d, "http://doi.org/")
	d = strings.TrimPrefix(d, "doi:")
	if d == "" {
		return ""
	}
	return "d:" + d
}
