package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// Brave Search API.
//
//	GET https://api.search.brave.com/res/v1/web/search
//	X-Subscription-Token: <key>
//
// Brave returns links and descriptions, never page content — so every Brave
// result costs a subsequent fetch. That is the trade against Tavily: cheaper
// per query, more work per result.

const (
	braveBaseURL = "https://api.search.brave.com/res/v1"
	// braveFreeTierRate is one query per second, the documented free-tier
	// ceiling. Exceeding it returns 429 rather than queuing, so the limiter
	// defaults to the safe value instead of the fast one.
	braveFreeTierRate = 1.0
)

type braveProvider struct {
	base
}

func newBrave(cfg Config, client *http.Client) Provider {
	return &braveProvider{
		base: newBase(cfg, client, braveBaseURL, DefaultBraveCostMicros,
			limiter.Limit{Rate: braveFreeTierRate, Burst: 1}),
	}
}

// braveResponse mirrors the documented shape. Unknown fields are ignored, so a
// provider-side addition does not break parsing.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			PageAge     string `json:"page_age"`
			Age         string `json:"age"`
		} `json:"results"`
	} `json:"web"`
}

func (p *braveProvider) Search(ctx context.Context, query string, opts Options) (*Response, error) {
	opts = opts.withDefaults()
	start := time.Now()

	if err := p.wait(ctx); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(opts.MaxResults))
	q.Set("result_filter", "web")
	// Brave's own extra-snippets flag gives longer descriptions. It is not page
	// content — it does not remove the need to fetch — but it improves the
	// planner's ability to judge a result before spending a fetch on it.
	q.Set("extra_snippets", "true")

	// Brave has no exclude-domains parameter; the documented way is a query
	// operator. Include-domains has no equivalent at all, so it is filtered
	// client-side below rather than silently ignored.
	for _, d := range opts.ExcludeDomains {
		if d = strings.TrimSpace(d); d != "" {
			q.Set("q", q.Get("q")+" -site:"+d)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/web/search?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("search: brave request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search: brave: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("search: brave read: %w", err)
	}
	if err := classifyStatus(KindBrave, resp.StatusCode, string(body)); err != nil {
		return nil, err
	}

	var parsed braveResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("search: brave decode: %w", err)
	}

	include := newDomainFilter(opts.IncludeDomains)
	out := &Response{
		Query:    query,
		Provider: KindBrave,
		Cost:     p.queryCost(),
	}
	for _, r := range parsed.Web.Results {
		if r.URL == "" {
			continue
		}
		if !include.allows(r.URL) {
			continue
		}
		out.Results = append(out.Results, Result{
			URL:         r.URL,
			Title:       r.Title,
			Snippet:     r.Description,
			PublishedAt: parseBraveAge(r.PageAge, r.Age),
			Rank:        len(out.Results) + 1,
		})
	}
	out.Elapsed = time.Since(start)
	return out, nil
}

// parseBraveAge reads whichever date field Brave populated.
//
// page_age is an ISO timestamp; age is a human string like "3 days ago", which
// is deliberately NOT parsed. A guessed date is worse than no date here:
// §11.2's supersedes edges use PublishedAt to separate staleness from genuine
// disagreement, and a fabricated timestamp would produce confident nonsense.
func parseBraveAge(pageAge, age string) *time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(pageAge)); err == nil {
			return &t
		}
	}
	return nil
}
