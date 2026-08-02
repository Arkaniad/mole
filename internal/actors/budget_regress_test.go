package actors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/tools/search"
)

// longArticle is big enough to split into several chunks, so per-chunk and
// per-source limits can be told apart.
func longArticle(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "Finding %d: the measured value was %d.%02d units under the stated conditions. ",
			i, i, i%100)
		fmt.Fprintf(&b, "This sentence exists to give paragraph %d enough substance to survive chunking.\n\n", i)
	}
	return b.String()
}

// mineEverySentence answers with as many claims as the prompt allows, quoting
// the chunk verbatim so every one of them verifies. A well-behaved greedy model
// is exactly what a per-source cap has to hold back.
func mineEverySentence(prompt string) string {
	doc := documentBlock(prompt)
	var claims []map[string]any
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 40 {
			continue
		}
		claims = append(claims, map[string]any{
			"text":       "A measurement was reported: " + line[:30],
			"quote":      line,
			"confidence": 0.8,
		})
		if len(claims) >= 50 {
			break
		}
	}
	out, _ := json.Marshal(map[string]any{"claims": claims})
	return string(out)
}

// TestClaimCapIsPerSourceNotPerChunk. MaxClaimsPerSource exists so one verbose
// document cannot dominate the graph. Enforced per chunk instead, a ten-chunk
// page contributed ten times its intended share — and long pages are precisely
// the ones that get chunked.
func TestClaimCapIsPerSourceNotPerChunk(t *testing.T) {
	h := newHarness(t, []search.Result{
		{URL: "https://verbose.example/a", Content: longArticle(300), Rank: 1},
	}, nil, mineEverySentence)
	h.actor.Budget = actors.Budget{MaxSources: 1, MaxClaimsPerSource: 4, MaxInputTokens: 1_000_000}

	res, err := h.actor.Run(context.Background(), h.lead)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Stats.Chunks < 2 {
		t.Fatalf("document produced %d chunk(s); the test needs several to be meaningful", res.Stats.Chunks)
	}
	if len(res.Claims) > 4 {
		t.Errorf("one source contributed %d claims across %d chunks, want at most 4",
			len(res.Claims), res.Stats.Chunks)
	}
	if len(res.Claims) == 0 {
		t.Error("the cap swallowed every claim")
	}
}

// TestSubBudgetIsRecheckedBetweenChunks. Plan sizes a document against an
// ESTIMATE. Checking the ceiling only once per source let a document whose real
// token count ran over spend roughly a whole extra document before anything
// noticed.
func TestSubBudgetIsRecheckedBetweenChunks(t *testing.T) {
	h := newHarness(t, []search.Result{
		{URL: "https://verbose.example/a", Content: longArticle(400), Rank: 1},
	}, nil, mineEverySentence)
	// The fake reports 1200 input tokens per call regardless of the estimate,
	// which is the real-world case: the estimate is deliberately rough.
	h.actor.Budget = actors.Budget{MaxSources: 1, MaxClaimsPerSource: 50, MaxInputTokens: 3000}

	res, err := h.actor.Run(context.Background(), h.lead)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var mineCalls int
	for _, c := range res.Costs {
		if c.Type == core.CallLLM && strings.HasPrefix(c.Input, "mine:") {
			mineCalls++
		}
	}
	// 3000 tokens at 1200 reported per call covers two calls and no more.
	if mineCalls > 3 {
		t.Errorf("%d mining calls against a 3000-token ceiling at 1200/call — the ceiling is not being rechecked", mineCalls)
	}
	if !res.Truncated {
		t.Error("the run overran its sub-budget without reporting Truncated")
	}
}

// TestProviderContentIsNotRecordedAsAFetch. Every per-cause rate in §10.4 is a
// fraction of attempted fetches. Filing a skipped fetch as ok padded that
// denominator with requests that were never made, which drags every failure
// rate — including the js_required number §17.1 turns on — toward zero.
func TestProviderContentIsNotRecordedAsAFetch(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, []search.Result{
		{URL: "https://provider.example/a", Content: articleBody, Rank: 1},
		{URL: "https://provider.example/b", Content: articleBody, Rank: 2},
		{URL: "https://spa.example/c", Rank: 3},
	}, map[string]*fetch.Result{
		"https://spa.example/c": {URL: "https://spa.example/c", Domain: "spa.example",
			Outcome: fetch.OutcomeOK, StatusCode: 200, ContentType: "text/html",
			Content: []byte(`<html><head><meta property="og:title" content="App"></head>` +
				`<body><div id="root"></div><script src="/a.js"></script><script src="/b.js"></script></body></html>`)},
	}, nil)
	h.actor.Budget = actors.Budget{MaxSources: 5, MaxInputTokens: 1_000_000}

	if _, err := h.actor.Run(ctx, h.lead); err != nil {
		t.Fatalf("run: %v", err)
	}

	byOutcome := map[string]int64{}
	if err := h.db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		stats, err := q.FetchOutcomeStats(ctx, time.Now().Add(-time.Hour), 20)
		for _, s := range stats {
			byOutcome[s.Outcome] = s.Count
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if byOutcome[string(fetch.OutcomeOK)] != 0 {
		t.Errorf("skipped fetches were recorded as ok: %v", byOutcome)
	}
	if byOutcome[string(fetch.OutcomeProviderContent)] != 2 {
		t.Errorf("provider_content count = %d, want 2; got %v",
			byOutcome[string(fetch.OutcomeProviderContent)], byOutcome)
	}
	// With the denominator honest, the one real fetch is the whole sample and
	// the SPA shows up as the capability gap it is.
	if byOutcome[string(fetch.OutcomeJSRequired)] != 1 {
		t.Errorf("js_required count = %d, want 1; got %v",
			byOutcome[string(fetch.OutcomeJSRequired)], byOutcome)
	}
}

// TestClaimsCiteTheURLTheBytesCameFrom. A claim cited to a URL that redirects
// elsewhere sends a reader to the redirect rather than to the evidence.
func TestClaimsCiteTheURLTheBytesCameFrom(t *testing.T) {
	const (
		searchURL = "https://old.example/a"
		finalURL  = "https://new.example/a"
	)

	h := newHarness(t, []search.Result{
		{URL: searchURL, Rank: 1},
	}, map[string]*fetch.Result{
		searchURL: {URL: searchURL, FinalURL: finalURL, Domain: "new.example",
			Outcome: fetch.OutcomeOK, StatusCode: 200, ContentType: "text/html",
			Content: []byte(articleHTML())},
	}, nil)

	res, err := h.actor.Run(context.Background(), h.lead)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Claims) == 0 {
		t.Fatal("no claims produced")
	}
	for _, c := range res.Claims {
		if c.Source != finalURL {
			t.Errorf("claim cites %q, want the post-redirect %q", c.Source, finalURL)
		}
	}
}
