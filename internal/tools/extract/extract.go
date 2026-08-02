// Package extract turns fetched bytes into clean text plus metadata.
//
// Two strategies, tried in order:
//
//  1. Readability — strip nav, ads, footers, and comments; keep the article.
//  2. Structured data — JSON-LD `articleBody`, when readability came back
//     empty. §10.4 calls this the cheap path worth trying before any headless
//     browser, because it converts `structured_only` outcomes into usable text
//     for a small amount of parser work.
//
// Metadata is merged from both regardless of which produced the body, since
// JSON-LD's `datePublished` is more reliable than anything inferred from the
// rendered page — and Claim.PublishedAt feeds the supersedes edges that tell
// staleness apart from genuine disagreement (§11.2).
//
// This package never fetches. readability.FromURL exists and would bypass the
// egress guard, robots, rate limiting, and outcome classification all at once;
// TestNoDirectNetworkAccess asserts it is never referenced.
package extract

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	readability "codeberg.org/readeck/go-readability/v2"

	"github.com/lajosdeme/mole/internal/tools/fetch"
)

// Source records which strategy produced the body text.
type Source string

const (
	// SourceReadability is the normal path.
	SourceReadability Source = "readability"
	// SourceStructured means the body came out of JSON-LD after readability
	// found nothing.
	SourceStructured Source = "structured"
	// SourcePlainText means the response was already text.
	SourcePlainText Source = "plaintext"
	// SourceNone means nothing usable was recovered.
	SourceNone Source = "none"
)

// Document is the extracted result.
type Document struct {
	Text  string
	Title string

	Byline   string
	Excerpt  string
	SiteName string
	Language string

	PublishedAt *time.Time
	ModifiedAt  *time.Time

	Source Source

	// HasStructuredData is true when the page carried JSON-LD, __NEXT_DATA__,
	// or OpenGraph, whether or not it was used for the body. It separates "an
	// SPA we cannot read" from "a page whose text is right there in a script
	// tag" — a distinction §17.1's gate depends on.
	HasStructuredData bool

	// PaywallSignal is set when the page's own structured data declares it is
	// not free to read. More reliable than matching prose.
	PaywallSignal bool
}

// Usable reports whether enough text came back to work with.
func (d *Document) Usable() bool { return len(d.Text) >= fetch.MinUsableText }

// Extractor converts bytes to a Document.
type Extractor interface {
	Extract(content []byte, contentType string, pageURL *url.URL) (*Document, error)
}

// HTML is the default extractor.
type HTML struct {
	// MaxTextBytes caps the extracted text. A pathological page should not be
	// able to hand the chunker something unbounded.
	MaxTextBytes int
}

// New returns an extractor with defaults applied.
func New() *HTML { return &HTML{MaxTextBytes: 4 << 20} }

// Extract parses content and returns the best text available.
func (e *HTML) Extract(content []byte, contentType string, pageURL *url.URL) (*Document, error) {
	maxText := e.MaxTextBytes
	if maxText <= 0 {
		maxText = 4 << 20
	}

	if isPlainText(contentType) {
		text := normalizeText(string(content))
		return &Document{
			Text:   truncate(text, maxText),
			Source: sourceFor(text),
		}, nil
	}

	raw := string(content)
	doc := &Document{}

	// Structured data is read from the raw bytes, independently of readability.
	//
	// It has to run first and separately: readability prunes and rewrites the
	// tree it works on, so anything read from its output would be looking at a
	// document it had already dismantled. Reading the raw string also means a
	// page whose markup is too broken for the parser can still yield its
	// JSON-LD — which is precisely the rescue this package is here for.
	if meta, err := parseStructured(raw); err == nil && meta != nil {
		doc.HasStructuredData = true
		doc.PaywallSignal = meta.NotFree
		applyStructured(doc, meta)
	}
	if !doc.HasStructuredData {
		doc.HasStructuredData = fetch.HasStructuredData(raw)
	}

	// FromReader parses internally and runs charset detection, which matters
	// for the non-UTF-8 pages a research agent will meet. A parse failure is
	// not fatal when structured data already produced a body — that rescue is
	// exactly what this package is for.
	article, rerr := readability.FromReader(bytes.NewReader(content), pageURL)
	if rerr != nil && doc.Text == "" {
		return nil, fmt.Errorf("extract: parse html: %w", rerr)
	}
	if rerr == nil {
		var buf strings.Builder
		if err := article.RenderText(&buf); err == nil {
			if text := normalizeText(buf.String()); len(text) >= fetch.MinUsableText {
				doc.Text = text
				doc.Source = SourceReadability
			}
		}
		mergeReadability(doc, article)
	}

	// Fall back to the structured body only when readability produced nothing
	// usable. When both exist, readability's is the better rendering.
	if doc.Source != SourceReadability && doc.Text != "" {
		doc.Source = SourceStructured
	}
	if doc.Source == "" {
		doc.Source = sourceFor(doc.Text)
	}

	doc.Text = truncate(doc.Text, maxText)
	return doc, nil
}

func sourceFor(text string) Source {
	if len(text) >= fetch.MinUsableText {
		return SourcePlainText
	}
	return SourceNone
}

// mergeReadability fills fields readability found, without overwriting the
// structured values — JSON-LD is the more reliable source when both exist.
func mergeReadability(doc *Document, a readability.Article) {
	if doc.Title == "" {
		doc.Title = strings.TrimSpace(a.Title())
	}
	if doc.Byline == "" {
		doc.Byline = strings.TrimSpace(a.Byline())
	}
	if doc.Excerpt == "" {
		doc.Excerpt = strings.TrimSpace(a.Excerpt())
	}
	if doc.SiteName == "" {
		doc.SiteName = strings.TrimSpace(a.SiteName())
	}
	if doc.Language == "" {
		doc.Language = strings.TrimSpace(a.Language())
	}
	if doc.PublishedAt == nil {
		if t, err := a.PublishedTime(); err == nil && !t.IsZero() {
			doc.PublishedAt = &t
		}
	}
	if doc.ModifiedAt == nil {
		if t, err := a.ModifiedTime(); err == nil && !t.IsZero() {
			doc.ModifiedAt = &t
		}
	}
}

// ---------------------------------------------------------------------------
// Outcome mapping
// ---------------------------------------------------------------------------

// Refine sets the fetch result's outcome from what extraction actually
// produced.
//
// The fetcher can only report transport-level success; whether the bytes
// contained readable text is knowable only after this package has run. Keeping
// the two steps separate is what lets `structured_only` be counted as its own
// cause rather than folded into `js_required`.
func Refine(res *fetch.Result, doc *Document) {
	if res == nil || res.Outcome != fetch.OutcomeOK {
		return
	}
	switch {
	case doc == nil:
		res.Outcome = fetch.OutcomeExtractFailed
	case doc.Source == SourceStructured && doc.Usable():
		res.Outcome = fetch.OutcomeStructuredOnly
	case doc.Usable():
		res.Outcome = fetch.OutcomeOK
	case doc.PaywallSignal:
		// The page's own metadata says it is not free. Trust that over the
		// prose heuristics, which would otherwise call this extract_failed.
		res.Outcome = fetch.OutcomePaywall
	default:
		res.Outcome = fetch.ClassifyBody(string(res.Content), len(doc.Text))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func isPlainText(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/plain")
}

// normalizeText collapses the whitespace readability leaves behind.
//
// Chunk boundaries and quote verification both operate on this text, so the
// normalization has to be deterministic: a claim's quote is checked verbatim
// against it, and inconsistent spacing would fail claims that are actually
// grounded.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// Non-breaking and other exotic spaces become plain spaces, so a quote
	// copied from the page matches the text we stored.
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)

	var out strings.Builder
	out.Grow(len(s))

	var blankRun int
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		// Collapse runs of spaces inside the line.
		for strings.Contains(line, "  ") {
			line = strings.ReplaceAll(line, "  ", " ")
		}
		line = strings.TrimSpace(line)

		if line == "" {
			blankRun++
			// At most one blank line between paragraphs.
			if blankRun > 1 {
				continue
			}
		} else {
			blankRun = 0
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimSpace(out.String())
}

// truncate cuts at a rune boundary so the text stays valid UTF-8.
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !isRuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
