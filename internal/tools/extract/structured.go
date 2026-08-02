package extract

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/fetch"
)

// Structured-data extraction.
//
// JSON-LD is the only source trusted for BODY text. It is a specified format
// with a defined field (`articleBody`), so recovering text from it is parsing
// rather than guessing.
//
// OpenGraph and Twitter cards supply metadata only. `og:description` is a
// summary written for social previews; treating it as article text would hand
// the summarizer a blurb and let claims be "grounded" against marketing copy.
//
// __NEXT_DATA__ is detected but not mined. It is an arbitrary application
// state blob with no agreed location for article text, so extraction would mean
// walking the JSON looking for the longest string — which produces plausible
// garbage often enough to be worse than admitting we cannot read it. It is
// counted as structured data so the page is not misfiled as js_required, and
// left there. See §17.1: if `structured_only` turns out to concentrate in
// __NEXT_DATA__ sites, that is the moment to write a per-framework miner.

var (
	jsonLDScriptRe = regexp.MustCompile(`(?is)<script[^>]+type\s*=\s*["']application/ld\+json["'][^>]*>(.*?)</script>`)
	metaTagRe      = regexp.MustCompile(`(?is)<meta\s+([^>]+?)/?>`)
	attrRe         = regexp.MustCompile(`(?is)([a-z:_-]+)\s*=\s*("([^"]*)"|'([^']*)')`)
	htmlTagRe      = regexp.MustCompile(`(?s)<[^>]*>`)
)

// structuredMeta is what the structured pass recovered.
type structuredMeta struct {
	Body     string
	Title    string
	Byline   string
	Excerpt  string
	SiteName string
	Language string

	Published *time.Time
	Modified  *time.Time

	NotFree bool
}

// parseStructured reads JSON-LD and meta tags from raw HTML.
//
// It works on the raw string rather than a parsed tree because it must run
// before readability, which mutates any tree handed to it.
func parseStructured(raw string) (*structuredMeta, error) {
	meta := &structuredMeta{}
	found := false

	for _, m := range jsonLDScriptRe.FindAllStringSubmatch(raw, 8) {
		if applyJSONLD(meta, m[1]) {
			found = true
		}
	}
	if applyMetaTags(meta, raw) {
		found = true
	}

	if !found {
		return nil, nil
	}
	return meta, nil
}

func applyStructured(doc *Document, m *structuredMeta) {
	if m.Body != "" {
		doc.Text = m.Body
	}
	doc.Title = firstNonEmpty(doc.Title, m.Title)
	doc.Byline = firstNonEmpty(doc.Byline, m.Byline)
	doc.Excerpt = firstNonEmpty(doc.Excerpt, m.Excerpt)
	doc.SiteName = firstNonEmpty(doc.SiteName, m.SiteName)
	doc.Language = firstNonEmpty(doc.Language, m.Language)
	if doc.PublishedAt == nil {
		doc.PublishedAt = m.Published
	}
	if doc.ModifiedAt == nil {
		doc.ModifiedAt = m.Modified
	}
}

// ---------------------------------------------------------------------------
// JSON-LD
// ---------------------------------------------------------------------------

// articleTypes are the schema.org types whose articleBody is real article text.
var articleTypes = map[string]bool{
	"article":          true,
	"newsarticle":      true,
	"blogposting":      true,
	"techarticle":      true,
	"scholarlyarticle": true,
	"report":           true,
	"webpage":          true,
}

func applyJSONLD(meta *structuredMeta, payload string) bool {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return false
	}

	var v any
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		// Malformed JSON-LD is common in the wild and is not an error worth
		// failing extraction over — readability is still going to run.
		return false
	}
	return walkJSONLD(meta, v, 0)
}

// walkJSONLD descends into @graph containers and arrays, which is how most
// CMS platforms actually emit their markup.
func walkJSONLD(meta *structuredMeta, v any, depth int) bool {
	if depth > 6 {
		return false
	}

	switch t := v.(type) {
	case []any:
		found := false
		for _, item := range t {
			if walkJSONLD(meta, item, depth+1) {
				found = true
			}
		}
		return found

	case map[string]any:
		if g, ok := t["@graph"]; ok {
			if walkJSONLD(meta, g, depth+1) {
				return true
			}
		}
		if !isArticleType(t["@type"]) {
			return false
		}

		found := false
		if body := cleanBody(stringField(t, "articleBody")); len(body) >= fetch.MinUsableText {
			if len(body) > len(meta.Body) {
				meta.Body = body
				found = true
			}
		}
		if s := stringField(t, "headline", "name"); s != "" && meta.Title == "" {
			meta.Title = s
			found = true
		}
		if s := stringField(t, "description"); s != "" && meta.Excerpt == "" {
			meta.Excerpt = s
			found = true
		}
		if s := authorName(t["author"]); s != "" && meta.Byline == "" {
			meta.Byline = s
			found = true
		}
		if s := stringField(t, "inLanguage"); s != "" && meta.Language == "" {
			meta.Language = s
			found = true
		}
		if p := parseDate(stringField(t, "datePublished")); p != nil && meta.Published == nil {
			meta.Published = p
			found = true
		}
		if p := parseDate(stringField(t, "dateModified")); p != nil && meta.Modified == nil {
			meta.Modified = p
			found = true
		}
		// The page declaring itself paywalled is stronger evidence than any
		// phrase match on the rendered body.
		if free, ok := t["isAccessibleForFree"]; ok && isFalsey(free) {
			meta.NotFree = true
			found = true
		}
		return found
	}
	return false
}

func isArticleType(v any) bool {
	switch t := v.(type) {
	case string:
		return articleTypes[strings.ToLower(strings.TrimSpace(t))]
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && articleTypes[strings.ToLower(strings.TrimSpace(s))] {
				return true
			}
		}
	}
	return false
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// authorName handles the several shapes schema.org allows: a bare string, a
// Person object, or a list of either.
func authorName(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		return stringField(t, "name")
	case []any:
		var names []string
		for _, item := range t {
			if n := authorName(item); n != "" {
				names = append(names, n)
			}
		}
		return strings.Join(names, ", ")
	}
	return ""
}

func isFalsey(v any) bool {
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "false" || s == "no" || s == "0"
	}
	return false
}

// cleanBody strips any markup an articleBody carries and normalizes whitespace,
// so structured text is directly comparable with readability's output — quote
// verification checks against whichever produced the body.
func cleanBody(s string) string {
	if s == "" {
		return ""
	}
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = unescapeEntities(s)
	return normalizeText(s)
}

// ---------------------------------------------------------------------------
// Meta tags
// ---------------------------------------------------------------------------

// applyMetaTags reads OpenGraph, Twitter, and standard meta tags. Metadata
// only — never body text.
func applyMetaTags(meta *structuredMeta, raw string) bool {
	found := false

	for _, m := range metaTagRe.FindAllStringSubmatch(raw, 200) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(m[1], -1) {
			val := a[3]
			if val == "" {
				val = a[4]
			}
			attrs[strings.ToLower(a[1])] = val
		}

		key := attrs["property"]
		if key == "" {
			key = attrs["name"]
		}
		content := strings.TrimSpace(unescapeEntities(attrs["content"]))
		if key == "" || content == "" {
			continue
		}

		switch strings.ToLower(key) {
		case "og:title", "twitter:title":
			if meta.Title == "" {
				meta.Title, found = content, true
			}
		case "og:description", "twitter:description", "description":
			if meta.Excerpt == "" {
				meta.Excerpt, found = content, true
			}
		case "og:site_name":
			if meta.SiteName == "" {
				meta.SiteName, found = content, true
			}
		case "og:locale":
			if meta.Language == "" {
				meta.Language, found = content, true
			}
		case "article:published_time", "datepublished", "publishdate", "date":
			if meta.Published == nil {
				if p := parseDate(content); p != nil {
					meta.Published, found = p, true
				}
			}
		case "article:modified_time", "datemodified", "lastmod":
			if meta.Modified == nil {
				if p := parseDate(content); p != nil {
					meta.Modified, found = p, true
				}
			}
		case "author", "article:author":
			if meta.Byline == "" {
				meta.Byline, found = content, true
			}
		}
	}
	return found
}

// ---------------------------------------------------------------------------
// Dates
// ---------------------------------------------------------------------------

// dateLayouts covers what these fields actually contain. schema.org specifies
// ISO 8601; the rest are what publishers emit anyway.
var dateLayouts = []string{
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006/01/02",
	time.RFC1123Z,
	time.RFC1123,
	"January 2, 2006",
	"2 January 2006",
}

// parseDate returns nil rather than a guess.
//
// A wrong PublishedAt is worse than a missing one: §11.2 uses it to decide
// whether a contradiction is disagreement or staleness, and a fabricated date
// makes the graph confidently wrong.
func parseDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil && !t.IsZero() {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

// ---------------------------------------------------------------------------

var entityReplacer = strings.NewReplacer(
	"&amp;", "&",
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&#39;", "'",
	"&apos;", "'",
	"&nbsp;", " ",
	"&#x27;", "'",
	"&#x2F;", "/",
)

func unescapeEntities(s string) string { return entityReplacer.Replace(s) }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
