package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// SearXNG.
//
//	GET {instance}/search?q=…&format=json
//
// A self-hosted metasearch front-end: it forwards the query to other engines
// and merges their results. There is no account and no API key, which is the
// point — the search half of a session costs nothing, so the ledger only ever
// sees fetches and model calls. The trade is that you run the instance, and
// results are only as good as the engines it can still reach.
//
// Like Brave and unlike Tavily it returns snippets and never page content, so
// every result still costs a fetch.
//
// The JSON API is opt-in on the instance side: settings.yml must list json
// under search.formats or every query comes back as HTML. That is the first
// thing to check when a fresh instance refuses everything — Search says so by
// name rather than handing back the markup.
const (
	// searxngDefaultRate is deliberately conservative. The instance itself has
	// no ceiling worth speaking of, but the engines behind it do, and SearXNG's
	// own bot-detection limiter answers a burst with 429 rather than a queue.
	searxngDefaultRate = 1.0
)

type searxngProvider struct {
	base
}

// newSearxng builds the provider. There is no default endpoint — the instance
// is the user's own — so New rejects an empty BaseURL before reaching here.
func newSearxng(cfg Config, client *http.Client) Provider {
	return &searxngProvider{
		base: newBase(cfg, client, "", DefaultSearxngCostMicros,
			limiter.Limit{Rate: searxngDefaultRate, Burst: 1}),
	}
}

// searxngResponse mirrors the documented JSON shape. The many other top-level
// fields (answers, infoboxes, suggestions, unresponsive_engines) are ignored.
type searxngResponse struct {
	Query   string `json:"query"`
	Results []struct {
		Title         string  `json:"title"`
		URL           string  `json:"url"`
		Content       string  `json:"content"`
		Score         float64 `json:"score"`
		PublishedDate string  `json:"publishedDate"`
	} `json:"results"`
}

func (p *searxngProvider) Search(ctx context.Context, query string, opts Options) (*Response, error) {
	opts = opts.withDefaults()
	start := time.Now()

	if err := p.wait(ctx); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("pageno", "1")
	// Pin the category rather than inherit whatever the instance defaults to,
	// so a shared instance tuned for images or maps still answers research
	// queries with web pages.
	q.Set("categories", "general")

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("search: searxng request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// SearXNG itself has no accounts, so the key is optional and usually empty.
	// An instance reachable from outside the host it runs on is normally put
	// behind an authenticating proxy, and the token is that proxy's — sent the
	// same way Tavily's is, in a header rather than the URL, so the cassette
	// layer's header redaction reaches it.
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: searxng: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("search: searxng read: %w", err)
	}
	if err := classifyStatus(KindSearxng, resp.StatusCode, string(body)); err != nil {
		return nil, err
	}

	var parsed searxngResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		// An instance without json in search.formats answers 200 with the
		// ordinary results page. Decoding fails on the first byte, and handing
		// back a page of markup names the symptom rather than the cause.
		if strings.HasPrefix(strings.TrimSpace(string(body)), "<") {
			return nil, fmt.Errorf("search: searxng returned HTML, not JSON — " +
				"add json to search.formats in the instance settings.yml and restart it")
		}
		return nil, fmt.Errorf("search: searxng decode: %w", err)
	}

	// SearXNG has no result-count parameter: a page holds whatever the engines
	// returned, merged. Both domain options are applied here too — the query is
	// passed through to every engine verbatim, so a -site: operator would reach
	// engines that do not implement it as literal text to search for.
	include := newDomainFilter(opts.IncludeDomains)
	exclude := newDomainFilter(opts.ExcludeDomains)

	out := &Response{
		Query:    query,
		Provider: KindSearxng,
		Cost:     p.queryCost(),
	}
	for _, r := range parsed.Results {
		if r.URL == "" {
			continue
		}
		if !include.allows(r.URL) || exclude.matches(r.URL) {
			continue
		}
		out.Results = append(out.Results, Result{
			URL:         r.URL,
			Title:       r.Title,
			Snippet:     r.Content,
			Score:       r.Score,
			PublishedAt: parseSearxngDate(r.PublishedDate),
			Rank:        len(out.Results) + 1,
		})
		if len(out.Results) >= opts.MaxResults {
			break
		}
	}
	out.Elapsed = time.Since(start)
	return out, nil
}

// parseSearxngDate reads whatever the contributing engine supplied. SearXNG
// serializes a Python datetime, and which layout that produces depends on the
// engine — so several are tried and an unrecognized one stays nil rather than
// becoming a guess §11.2's supersedes edges would trust.
func parseSearxngDate(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 02 Jan 2006 15:04:05 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}
