package actors

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/cache"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/tools/search"
)

// WebActor runs a web research lead:
//
//	search → (fetch → extract)? → chunk → mine claims → verify quotes → reduce
//
// The parenthesized step is conditional: a search provider that returned usable
// page content skips it entirely (§10.4). Everything raw dies when Run returns.
type WebActor struct {
	Search  search.Provider
	Fetch   fetch.Fetcher
	Extract extract.Extractor
	LLM     llm.Provider
	Pricing *pricing.Table
	Store   store.Store
	Log     *slog.Logger
	Budget  Budget

	// SessionID scopes the claims and fetch outcomes this actor records.
	SessionID string

	// Cache, when set, holds documents already fetched in this session. The
	// nested check §9.3 describes: two distinct queries converging on one page
	// pay for one fetch. Nil disables it.
	Cache *cache.Cache
}

func (a *WebActor) Type() core.ActorType { return core.ActorWeb }

func (a *WebActor) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Run executes one lead.
func (a *WebActor) Run(ctx context.Context, lead core.Lead) (*Result, error) {
	budget := a.Budget
	// The executor's reservation is the only ceiling that knows what this lead
	// may actually cost (§9.2). Without it the actor's configured budget bore
	// no relation to the money held for it, and a lead could spend several
	// times the whole session budget.
	if sub, ok := SubBudgetFrom(ctx); ok {
		budget = budget.tighten(sub)
	}
	budget = budget.withDefaults()
	res := &Result{}

	// 1. Search.
	sr, err := a.Search.Search(ctx, lead.Query, search.Options{
		MaxResults:     budget.MaxSources * 2, // headroom for guard/robots refusals
		IncludeContent: true,
	})
	if err != nil {
		return res, fmt.Errorf("actors/web: search: %w", err)
	}
	res.Costs = append(res.Costs, core.ToolCall{
		SessionID: a.SessionID,
		LeadID:    &lead.ID,
		Role:      core.RoleExecutor,
		Type:      core.CallSearch,
		Input:     lead.Query,
		Cost:      sr.Cost,
	})
	res.Stats.SearchResults = len(sr.Results)

	// 2-5. Read sources until the budget or the source cap is reached.
	var (
		inputTokensUsed int64
		sourcesRead     int
		perSource       []sourceSummary
	)

	for _, hit := range sr.Results {
		// Count sources READ, not sources that yielded something. Counting
		// summaries meant a run where nothing extracted cleanly walked the
		// entire result list — spending a fetch, an extract, and a model call
		// per hit against a cap that never advanced.
		if sourcesRead >= budget.MaxSources {
			break
		}
		if remaining := budget.MaxInputTokens - inputTokensUsed; remaining <= 0 {
			res.Truncated = true
			break
		}

		src, ok := a.readSource(ctx, lead, hit, budget, res)
		if !ok {
			continue
		}
		sourcesRead++
		doc := src.doc

		// Chunk under what is left of the sub-budget.
		plan := llm.Plan(doc.Text, budget.MaxInputTokens-inputTokensUsed, budget.ChunkOptions())
		if plan.Truncated {
			res.Truncated = true
		}
		res.Stats.Chunks += len(plan.Chunks)
		res.Stats.ChunksSkipped += plan.Skipped

		summary := sourceSummary{Title: doc.Title, URL: src.url}

		for _, chunk := range plan.Chunks {
			// Plan sized the whole document against an ESTIMATE. Re-check
			// between chunks so a document whose real token count runs over
			// stops here rather than at the next source — the sub-budget is a
			// ceiling, and checking it once per source overshot it by roughly
			// the cost of a full document.
			if inputTokensUsed >= budget.MaxInputTokens {
				res.Truncated = true
				res.Stats.ChunksSkipped++
				continue
			}
			// The cap is per SOURCE, so one verbose page cannot dominate the
			// graph (§ Budget.MaxClaimsPerSource). Applying it per chunk let a
			// ten-chunk document contribute ten times the intended share.
			remainingClaims := budget.MaxClaimsPerSource - len(summary.Claims)
			if remainingClaims <= 0 {
				break
			}

			claims, usage, err := a.mineChunk(ctx, lead, src, doc, chunk, remainingClaims, budget, res)
			inputTokensUsed += usage.InputTokens + usage.CacheReadTokens
			if err != nil {
				// One bad chunk does not fail the lead; the others may still
				// carry the answer. §9.5 classifies this as degraded — but only
				// if it is counted, so the caller can tell "this source was
				// thin" from "nothing worked".
				res.Stats.ChunksFailed++
				a.logger().WarnContext(ctx, "chunk mining failed",
					"url", src.url, "chunk", chunk.Index, "err", err)
				continue
			}
			res.Claims = append(res.Claims, claims...)
			summary.Claims = append(summary.Claims, claims...)
		}

		if len(summary.Claims) > 0 || doc.Excerpt != "" {
			summary.Excerpt = doc.Excerpt
			perSource = append(perSource, summary)
		}
	}

	// 6. Reduce to one summary.
	if len(perSource) > 0 {
		summary, err := a.reduce(ctx, lead, perSource, res)
		if err != nil {
			a.logger().WarnContext(ctx, "reduce failed", "lead", lead.ID, "err", err)
		} else {
			res.Summary = summary
		}
	}
	if res.Summary == "" {
		// Say what actually happened. Claiming no sources were found while
		// printing claims from those sources directly underneath is worse than
		// no summary: it tells a reader the run was empty when it was
		// degraded, and those call for different responses.
		switch {
		case len(res.Claims) > 0:
			res.Summary = fmt.Sprintf(
				"Summarization failed; %d verified claim(s) from %d source(s) are listed below, unsynthesized.",
				len(res.Claims), len(perSource))
		case res.Stats.ChunksFailed > 0:
			res.Summary = fmt.Sprintf(
				"No claims extracted: all %d model call(s) over %d source(s) failed.",
				res.Stats.ChunksFailed, res.Stats.Fetched+res.Stats.SkippedFetch)
		default:
			res.Summary = fmt.Sprintf("No usable sources found for %q.", lead.Query)
		}
	}

	// 7. Persist claims. All or nothing (§ store.InsertClaims).
	if len(res.Claims) > 0 && a.Store != nil {
		if err := a.Store.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.InsertClaims(ctx, res.Claims)
		}); err != nil {
			return res, fmt.Errorf("actors/web: persist claims: %w", err)
		}
	}

	return res, nil
}

type sourceSummary struct {
	Title   string
	URL     string
	Excerpt string
	Claims  []core.Claim
}

// source is a read source: its text, and the URL that text actually came from.
type source struct {
	doc    *extract.Document
	url    string
	domain string
}

// readSource turns one search hit into extracted text.
//
// The fetch is skipped when the search provider already returned usable
// content. That is the efficiency §10.4 identifies before any headless-browser
// question: no HTTP request, no robots round-trip, no rate-limit pressure, and
// no js_required outcome to explain.
func (a *WebActor) readSource(ctx context.Context, lead core.Lead, hit search.Result, budget Budget, res *Result) (source, bool) {
	pageURL, err := url.Parse(hit.URL)
	if err != nil {
		return source{}, false
	}

	// Check the URL cache after search, before fetch (§9.3). A page another
	// lead already read is free, and converging queries are the common case
	// once a planner is decomposing one question several ways.
	key := cache.URLKey(hit.URL)
	if e, ok := a.Cache.Get(key); ok && len(e.Text) >= fetch.MinUsableText {
		res.Stats.CacheHits++
		return source{
			doc: &extract.Document{
				Text:        e.Text,
				Title:       e.Title,
				PublishedAt: e.PublishedAt,
				Source:      extract.SourcePlainText,
			},
			url:    hit.URL,
			domain: fetch.DomainOf(hit.URL),
		}, true
	}

	if hit.HasUsableContent() && !budget.AlwaysFetch {
		res.Stats.SkippedFetch++
		a.recordOutcome(ctx, lead, hit.URL, fetch.OutcomeProviderContent, 0, 0, "")
		a.Cache.Put(&cache.Entry{
			Key: key, Text: hit.Content, Title: hit.Title, PublishedAt: hit.PublishedAt,
		})
		return source{
			doc: &extract.Document{
				Text:        hit.Content,
				Title:       hit.Title,
				Excerpt:     hit.Snippet,
				PublishedAt: hit.PublishedAt,
				Source:      extract.SourcePlainText,
			},
			url:    hit.URL,
			domain: fetch.DomainOf(hit.URL),
		}, true
	}

	fr, ferr := a.Fetch.Fetch(ctx, hit.URL)
	res.Stats.Fetched++
	res.Costs = append(res.Costs, core.ToolCall{
		SessionID:  a.SessionID,
		LeadID:     &lead.ID,
		Role:       core.RoleExecutor,
		Type:       core.CallFetch,
		Input:      hit.URL,
		DurationMS: fr.Duration.Milliseconds(),
		Err:        fr.Err,
	})

	// The fetcher already reduced the host; recomputing it here from the raw
	// URL produced a second, subtly different answer for the same row.
	domain := fr.Domain
	if domain == "" {
		domain = fetch.DomainOf(hit.URL)
	}
	// Attribute to where the bytes came from. A claim cited to a URL that 301s
	// elsewhere sends a reader to the redirect, not to the evidence.
	finalURL := fr.FinalURL
	if finalURL == "" {
		finalURL = hit.URL
	}

	if ferr != nil || !fr.Outcome.Usable() {
		a.recordOutcomeFor(ctx, lead, hit.URL, domain, fr.Outcome, fr.StatusCode, fr.Bytes, fr.Err)
		return source{}, false
	}

	doc, eerr := a.Extract.Extract(ctx, fr.Content, fr.ContentType, pageURL)
	if eerr != nil {
		a.recordOutcomeFor(ctx, lead, hit.URL, domain, fetch.OutcomeExtractFailed, fr.StatusCode, fr.Bytes, eerr.Error())
		return source{}, false
	}

	// The transport said OK; whether the bytes held readable text is only
	// knowable now. This is what keeps structured_only a distinct cause.
	extract.Refine(fr, doc)
	a.recordOutcomeFor(ctx, lead, hit.URL, domain, fr.Outcome, fr.StatusCode, fr.Bytes, fr.Err)

	if !doc.Usable() {
		return source{}, false
	}
	if doc.PublishedAt == nil {
		doc.PublishedAt = hit.PublishedAt
	}
	// Key on the URL we were given AND the one we landed on: a later lead may
	// surface either, and a redirect chain that costs one fetch should not cost
	// a second because the search provider phrased the link differently.
	a.Cache.Put(&cache.Entry{
		Key: key, Text: doc.Text, Title: doc.Title, PublishedAt: doc.PublishedAt,
	})
	if fk := cache.URLKey(finalURL); fk != key {
		a.Cache.Put(&cache.Entry{
			Key: fk, Text: doc.Text, Title: doc.Title, PublishedAt: doc.PublishedAt,
		})
	}
	return source{doc: doc, url: finalURL, domain: domain}, true
}

// mineChunk extracts claims from one chunk and verifies every quote.
func (a *WebActor) mineChunk(
	ctx context.Context,
	lead core.Lead,
	src source,
	doc *extract.Document,
	chunk llm.Chunk,
	maxClaims int,
	budget Budget,
	res *Result,
) ([]core.Claim, llm.Usage, error) {
	prompt := mineUserPrompt(fenceToken(), lead.Query, maxClaims, doc.Title, src.url, chunk.Text)

	resp, err := a.LLM.Complete(ctx, llm.Request{
		Tier:      llm.TierCheap,
		System:    mineSystemPrompt,
		Messages:  []llm.Message{llm.User(prompt)},
		MaxTokens: 4096,
	})
	if resp == nil {
		return nil, llm.Usage{}, err
	}

	// Record the call even on failure — the tokens were spent.
	res.Costs = append(res.Costs, a.toolCall(lead, resp, core.RoleExecutor, "mine:"+src.url, err))
	if err != nil {
		return nil, resp.Usage, err
	}
	if resp.Refused {
		return nil, resp.Usage, fmt.Errorf("actors/web: model refused (%s)", resp.RefusalCategory)
	}

	mined, err := parseMined(resp.Text)
	if err != nil {
		return nil, resp.Usage, err
	}

	now := time.Now().UTC()
	var claims []core.Claim

	for _, m := range mined {
		res.Stats.ClaimsProposed++

		if strings.TrimSpace(m.Text) == "" {
			res.Stats.ClaimsRejected++
			continue
		}

		// The check. A quote that is not in the chunk was not copied from it.
		match, ok := FindQuote(chunk.Text, m.Quote)
		if !ok {
			res.Stats.ClaimsRejected++
			a.logger().DebugContext(ctx, "claim rejected: quote not found in source",
				"url", src.url, "quote", truncateForLog(m.Quote))
			continue
		}

		claims = append(claims, core.Claim{
			SessionID: a.SessionID,
			LeadID:    lead.ID,
			Text:      strings.TrimSpace(m.Text),
			Source:    src.url,
			Quote:     TruncateQuote(match.Text),
			// Offsets index the whole document, not the chunk — the chunk does
			// not outlive this function, and a later re-verification needs to
			// find the span in the source.
			QuoteOffset: int64(chunk.Start + match.Offset),
			PublishedAt: doc.PublishedAt,
			RetrievedAt: now,
			Confidence:  clamp01(m.Confidence),
		})

		if len(claims) >= maxClaims {
			break
		}
	}

	return claims, resp.Usage, nil
}

// reduce writes the one summary the planner will see.
func (a *WebActor) reduce(ctx context.Context, lead core.Lead, sources []sourceSummary, res *Result) (string, error) {
	var b strings.Builder
	for _, s := range sources {
		b.WriteString("\nsource: " + sanitizeTag(s.URL) + "\n")
		if s.Title != "" {
			b.WriteString("title: " + sanitizeTag(s.Title) + "\n")
		}
		for _, c := range s.Claims {
			b.WriteString("- " + sanitizeTag(c.Text) + "\n")
		}
		if len(s.Claims) == 0 && s.Excerpt != "" {
			b.WriteString("- " + sanitizeTag(s.Excerpt) + "\n")
		}
	}

	resp, err := a.LLM.Complete(ctx, llm.Request{
		Tier:      llm.TierStrong,
		System:    reduceSystemPrompt,
		Messages:  []llm.Message{llm.User(reduceUserPrompt(fenceToken(), lead.Query, b.String()))},
		MaxTokens: 2048,
	})
	if resp == nil {
		return "", err
	}
	res.Costs = append(res.Costs, a.toolCall(lead, resp, core.RoleExecutor, "reduce", err))
	if err != nil {
		return "", err
	}
	if resp.Refused {
		return "", fmt.Errorf("actors/web: model refused summary (%s)", resp.RefusalCategory)
	}
	return strings.TrimSpace(resp.Text), nil
}

// toolCall prices a model response into a ledger row.
func (a *WebActor) toolCall(lead core.Lead, resp *llm.Response, role core.Role, input string, callErr error) core.ToolCall {
	tc := core.ToolCall{
		SessionID:  a.SessionID,
		LeadID:     &lead.ID,
		Role:       role,
		Type:       core.CallLLM,
		Model:      resp.Model,
		Input:      input,
		DurationMS: resp.Elapsed.Milliseconds(),
	}
	if callErr != nil {
		tc.Err = callErr.Error()
	}

	table := a.Pricing
	if table == nil {
		table = pricing.NewTable()
	}
	cost, err := table.Cost(resp.Model, pricing.Usage{
		InputTokens:      resp.Usage.InputTokens,
		OutputTokens:     resp.Usage.OutputTokens,
		CacheReadTokens:  resp.Usage.CacheReadTokens,
		CacheWriteTokens: resp.Usage.CacheWriteTokens,
	})
	if err != nil {
		// An unpriced model still spent tokens. Recording zero dollars but real
		// tokens is the honest answer: token-mode budgets stay correct, and
		// USD-mode surfaces the gap through doctor rather than here.
		a.logger().Warn("model not in pricing table; USD cost recorded as zero",
			"model", resp.Model, "tokens", resp.Usage.Total())
	}
	tc.Cost = cost
	return tc
}

// recordOutcome writes the §10.4 row for one fetch attempt.
//
// Failures are recorded as diligently as successes: the rate per cause is
// meaningless without a denominator, and §17.1's gate reads both.
func (a *WebActor) recordOutcome(ctx context.Context, lead core.Lead, rawURL string, outcome fetch.Outcome, status int, bytes int64, errText string) {
	a.recordOutcomeFor(ctx, lead, rawURL, fetch.DomainOf(rawURL), outcome, status, bytes, errText)
}

// recordOutcomeFor is recordOutcome for a caller that already knows the domain.
func (a *WebActor) recordOutcomeFor(ctx context.Context, lead core.Lead, rawURL, domain string, outcome fetch.Outcome, status int, bytes int64, errText string) {
	if a.Store == nil {
		return
	}
	sessionID := a.SessionID
	leadID := lead.ID

	err := a.Store.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.RecordFetchOutcome(ctx, &store.FetchOutcome{
			SessionID:  &sessionID,
			LeadID:     &leadID,
			URL:        rawURL,
			Domain:     domain,
			Outcome:    string(outcome),
			StatusCode: status,
			Bytes:      bytes,
			Err:        errText,
		})
	})
	if err != nil {
		// Losing an outcome row degrades the M2 dataset; it must not fail the
		// lead that was otherwise fine.
		a.logger().WarnContext(ctx, "recording fetch outcome failed", "url", rawURL, "err", err)
	}
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

func truncateForLog(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "…"
}
