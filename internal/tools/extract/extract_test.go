package extract_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func body(n int) string {
	return strings.Repeat("The quick brown fox jumps over the lazy dog. ", n)
}

func article(extra, content string) string {
	return `<!doctype html><html lang="en"><head><title>Page Title</title>` + extra +
		`</head><body>
<nav><a href="/">Home</a><a href="/about">About</a></nav>
<header>Site chrome that is not the article</header>
<article><h1>Real Headline</h1><p>` + content + `</p></article>
<aside>Related links you should not see</aside>
<footer>Copyright boilerplate 2026</footer>
</body></html>`
}

// TestReadabilityStripsChromeKeepsBody is the core job: nav, aside, and footer
// out; article text in.
func TestReadabilityStripsChromeKeepsBody(t *testing.T) {
	e := extract.New()
	doc, err := e.Extract([]byte(article("", body(20))), "text/html", mustURL(t, "https://example.com/a"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if doc.Source != extract.SourceReadability {
		t.Errorf("source = %q, want readability", doc.Source)
	}
	if !doc.Usable() {
		t.Fatalf("document not usable, %d chars", len(doc.Text))
	}
	if !strings.Contains(doc.Text, "quick brown fox") {
		t.Error("article body was dropped")
	}
	for _, junk := range []string{"Home", "About", "Related links", "Copyright boilerplate"} {
		if strings.Contains(doc.Text, junk) {
			t.Errorf("boilerplate %q survived extraction", junk)
		}
	}
}

func TestPlainTextPassesThrough(t *testing.T) {
	e := extract.New()
	raw := body(20)
	doc, err := e.Extract([]byte(raw), "text/plain; charset=utf-8", mustURL(t, "https://example.com/t"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if doc.Source != extract.SourcePlainText {
		t.Errorf("source = %q, want plaintext", doc.Source)
	}
	if !strings.Contains(doc.Text, "quick brown fox") {
		t.Error("plain text body lost")
	}
}

// TestJSONLDRescuesBodyWhenReadabilityFails is the §10.4 cheap path: the page
// renders client-side, but the text is sitting in a script tag.
func TestJSONLDRescuesBodyWhenReadabilityFails(t *testing.T) {
	text := body(30)
	page := `<!doctype html><html><head>
<script type="application/ld+json">
{"@context":"https://schema.org","@type":"NewsArticle",
 "headline":"Structured Headline",
 "datePublished":"2024-03-15T09:00:00Z",
 "author":{"@type":"Person","name":"Jane Doe"},
 "articleBody":"` + text + `"}
</script>
<script src="/bundle.js"></script><script src="/vendor.js"></script>
</head><body><div id="root"></div></body></html>`

	e := extract.New()
	doc, err := e.Extract([]byte(page), "text/html", mustURL(t, "https://example.com/spa"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if doc.Source != extract.SourceStructured {
		t.Fatalf("source = %q, want structured", doc.Source)
	}
	if !doc.Usable() {
		t.Fatalf("structured body not usable, %d chars", len(doc.Text))
	}
	if !strings.Contains(doc.Text, "quick brown fox") {
		t.Error("articleBody was not recovered")
	}
	if doc.Title != "Structured Headline" {
		t.Errorf("title = %q", doc.Title)
	}
	if doc.Byline != "Jane Doe" {
		t.Errorf("byline = %q", doc.Byline)
	}
	if doc.PublishedAt == nil || doc.PublishedAt.Format("2006-01-02") != "2024-03-15" {
		t.Errorf("published = %v", doc.PublishedAt)
	}

	// This page would otherwise be filed as js_required and inflate the one
	// number §17.1's gate reads.
	res := &fetch.Result{Outcome: fetch.OutcomeOK, Content: []byte(page)}
	extract.Refine(res, doc)
	if res.Outcome != fetch.OutcomeStructuredOnly {
		t.Errorf("outcome = %q, want structured_only", res.Outcome)
	}
	if res.Outcome.CapabilityGap() {
		t.Error("structured_only must not count as a capability gap")
	}
}

// TestReadabilityWinsWhenBothAvailable: when the page renders server-side AND
// carries JSON-LD, readability's rendering is the better text.
func TestReadabilityWinsWhenBothAvailable(t *testing.T) {
	page := article(`<script type="application/ld+json">
{"@type":"Article","headline":"LD Headline","articleBody":"`+body(20)+`"}
</script>`, body(20))

	e := extract.New()
	doc, err := e.Extract([]byte(page), "text/html", mustURL(t, "https://example.com/both"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if doc.Source != extract.SourceReadability {
		t.Errorf("source = %q, want readability to win", doc.Source)
	}
	// Metadata still merges from the structured data, which is the more
	// reliable source for it.
	if doc.Title != "LD Headline" {
		t.Errorf("title = %q, want the JSON-LD headline", doc.Title)
	}
}

// TestOpenGraphIsMetadataOnly: og:description is a social-preview blurb.
// Treating it as article text would let claims be "grounded" against marketing
// copy.
func TestOpenGraphIsMetadataOnly(t *testing.T) {
	page := `<!doctype html><html><head>
<meta property="og:title" content="OG Title">
<meta property="og:description" content="A short promotional blurb about the page.">
<meta property="og:site_name" content="Example News">
<meta property="article:published_time" content="2024-05-01T12:00:00Z">
<script src="/a.js"></script><script src="/b.js"></script>
</head><body><div id="app"></div></body></html>`

	e := extract.New()
	doc, err := e.Extract([]byte(page), "text/html", mustURL(t, "https://example.com/og"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if doc.Usable() {
		t.Errorf("og:description was used as body text: %q", doc.Text)
	}
	if doc.Title != "OG Title" || doc.SiteName != "Example News" {
		t.Errorf("metadata not merged: title=%q site=%q", doc.Title, doc.SiteName)
	}
	if doc.PublishedAt == nil || doc.PublishedAt.Format("2006-01-02") != "2024-05-01" {
		t.Errorf("published = %v", doc.PublishedAt)
	}

	// No body anywhere, and it is a script-heavy app shell.
	res := &fetch.Result{Outcome: fetch.OutcomeOK, Content: []byte(page)}
	extract.Refine(res, doc)
	if res.Outcome != fetch.OutcomeStructuredOnly && res.Outcome != fetch.OutcomeJSRequired {
		t.Errorf("outcome = %q, want structured_only or js_required", res.Outcome)
	}
}

func TestPaywallSignalFromStructuredData(t *testing.T) {
	page := `<!doctype html><html><head>
<script type="application/ld+json">
{"@type":"NewsArticle","headline":"Members only","isAccessibleForFree":false}
</script></head><body><p>Teaser paragraph.</p></body></html>`

	e := extract.New()
	doc, err := e.Extract([]byte(page), "text/html", mustURL(t, "https://example.com/pw"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if !doc.PaywallSignal {
		t.Fatal("isAccessibleForFree:false not detected")
	}

	// The page's own metadata beats prose heuristics, which would have called
	// this extract_failed.
	res := &fetch.Result{Outcome: fetch.OutcomeOK, Content: []byte(page)}
	extract.Refine(res, doc)
	if res.Outcome != fetch.OutcomePaywall {
		t.Errorf("outcome = %q, want paywall", res.Outcome)
	}
}

// TestNormalizedTextSupportsVerbatimQuoteMatching is the property §11.5 rests
// on: a quote lifted from the extracted text must be findable in it, so
// non-breaking spaces and ragged whitespace cannot fail a grounded claim.
func TestNormalizedTextSupportsVerbatimQuoteMatching(t *testing.T) {
	page := article("", "MambaByte achieves   1.31\tbits per byte on PG-19.\n\n\n\n"+body(15))

	e := extract.New()
	doc, err := e.Extract([]byte(page), "text/html", mustURL(t, "https://example.com/q"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	if strings.Contains(doc.Text, " ") {
		t.Error("non-breaking space survived normalization")
	}
	if strings.Contains(doc.Text, "  ") {
		t.Error("collapsed whitespace still contains a double space")
	}
	if strings.Contains(doc.Text, "\n\n\n") {
		t.Error("more than one blank line survived")
	}
	if !strings.Contains(doc.Text, "MambaByte achieves 1.31 bits per byte on PG-19.") {
		t.Errorf("normalized quote not findable in:\n%.200q", doc.Text)
	}
}

func TestRefineLeavesDecidedFailuresAlone(t *testing.T) {
	for _, o := range []fetch.Outcome{
		fetch.OutcomeRobotsDenied, fetch.OutcomeGuardDenied,
		fetch.OutcomeNotFound, fetch.OutcomeBotBlock,
	} {
		res := &fetch.Result{Outcome: o}
		extract.Refine(res, &extract.Document{})
		if res.Outcome != o {
			t.Errorf("Refine changed %q to %q", o, res.Outcome)
		}
	}
}

func TestMalformedInputDoesNotPanic(t *testing.T) {
	e := extract.New()
	cases := [][]byte{
		nil,
		[]byte(""),
		[]byte("<html><body><p>unclosed"),
		[]byte(`<script type="application/ld+json">{ not json </script><body>x</body>`),
		[]byte("\x00\xff\xfe binary garbage"),
		[]byte(strings.Repeat("<div>", 500)),
	}
	for i, c := range cases {
		doc, err := e.Extract(c, "text/html", mustURL(t, "https://example.com/x"))
		if err != nil {
			continue // a returned error is an acceptable outcome
		}
		if doc == nil {
			t.Errorf("case %d: nil document with nil error", i)
		}
	}
}

func TestTextIsCapped(t *testing.T) {
	e := &extract.HTML{MaxTextBytes: 1000}
	doc, err := e.Extract([]byte(article("", body(500))), "text/html", mustURL(t, "https://example.com/big"))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(doc.Text) > 1000 {
		t.Errorf("text = %d bytes, want <= 1000", len(doc.Text))
	}
	if !utf8.ValidString(doc.Text) {
		t.Error("truncation produced invalid UTF-8")
	}
}

// TestNoDirectNetworkAccess is a structural guard, not a behavioural one.
//
// readability.FromURL performs its own HTTP. Calling it would bypass the egress
// guard, robots, per-host rate limiting, and outcome classification in a single
// line — every control slice 1 exists to provide. The failure would be silent,
// so the check is mechanical.
func TestNoDirectNetworkAccess(t *testing.T) {
	// Parse rather than grep: this file's own doc comments discuss FromURL by
	// name, and a substring check would flag the explanation of the rule as a
	// violation of it.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no non-test sources found; the guard would pass vacuously")
	}

	bannedImports := map[string]bool{
		"net/http": true,
		"net":      true,
	}
	bannedCalls := map[string]bool{
		"FromURL": true,
	}

	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			base := filepath.Base(name)

			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				if bannedImports[path] {
					t.Errorf("%s imports %q — extraction must never fetch; that path "+
						"bypasses the egress guard, robots, rate limiting, and outcome "+
						"classification in one line", base, path)
				}
			}

			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if bannedCalls[sel.Sel.Name] {
					t.Errorf("%s calls %s at %s — extraction must never fetch",
						base, sel.Sel.Name, fset.Position(sel.Pos()))
				}
				return true
			})
		}
	}
}
