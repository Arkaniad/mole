package actors

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/academic"
)

// AcademicActor researches a lead against scholarly sources (§10.2).
//
// The shape differs from WebActor in one way that matters: tier 0 of the
// escalation ladder costs NO fetches at all. arXiv and PubMed return the
// abstract in the search response, so a paper's most information-dense passage
// arrives for the price of the search — where a web lead pays a search, a fetch
// and an extraction to reach less.
//
// It also supplies something no web source reliably does: exact publication
// dates. §11.2's staleness rule compares when claims were made and has been
// reading zero for want of dates on both sides of a contradiction.
type AcademicActor struct {
	// Providers are searched in order. arXiv and PubMed cover different
	// literatures and overlap little, so both run rather than one being chosen.
	Providers []academic.Provider

	LLM     llm.Provider
	Pricing *pricing.Table
	Store   store.Store
	Log     *slog.Logger
	Budget  Budget

	SessionID string
}

func (a *AcademicActor) Type() core.ActorType { return core.ActorAcademic }

func (a *AcademicActor) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func (a *AcademicActor) Run(ctx context.Context, lead core.Lead) (*Result, error) {
	budget := a.Budget
	if sub, ok := SubBudgetFrom(ctx); ok {
		budget = budget.tighten(sub)
	}
	maxSources := budget.MaxSources
	if maxSources <= 0 {
		maxSources = 5
	}
	maxClaims := budget.MaxClaimsPerSource
	if maxClaims <= 0 {
		maxClaims = 8
	}

	res := &Result{}
	if len(a.Providers) == 0 {
		return res, fmt.Errorf("actors/academic: no providers configured")
	}

	papers, searchErr := a.gather(ctx, lead, maxSources)
	if len(papers) == 0 {
		if searchErr != nil {
			return res, searchErr
		}
		res.Summary = fmt.Sprintf("No papers found for %q.", lead.Query)
		return res, nil
	}

	miner := &Miner{LLM: a.LLM, Pricing: a.Pricing, Log: a.Log, SessionID: a.SessionID}
	var used int64

	for _, p := range papers {
		abstract := strings.TrimSpace(p.Abstract)
		if abstract == "" {
			// A paper with no abstract has nothing to mine at tier 0. Counted so
			// the planner sees the lead did work, rather than looking like a
			// question nobody asked.
			res.Stats.ChunksSkipped++
			continue
		}
		// The sub-budget is a ceiling on input tokens, and an abstract is small
		// enough that the estimate can be crude: stopping before the ceiling
		// costs a paper, exceeding it costs the reservation.
		if budget.MaxInputTokens > 0 && used+llm.EstimateTokens(len(abstract)) > budget.MaxInputTokens {
			res.Truncated = true
			break
		}
		used += llm.EstimateTokens(len(abstract))

		out, err := miner.Mine(ctx, MineInput{
			Lead:      lead,
			SourceURL: citationURL(p),
			Title:     p.Title,
			// Exact, from the provider — not guessed from a page.
			PublishedAt: p.PublishedAt,
			Text:        abstract,
			MaxClaims:   maxClaims,
		})
		if out.HasCall {
			res.Costs = append(res.Costs, out.Call)
		}
		res.Stats.ClaimsProposed += out.Proposed
		res.Stats.ClaimsRejected += out.Rejected
		if err != nil {
			a.logger().WarnContext(ctx, "mining an abstract failed",
				"source", citationURL(p), "err", err)
			continue
		}
		res.Stats.Chunks++
		res.Claims = append(res.Claims, out.Claims...)
	}

	res.Summary = summarize(lead.Query, papers, len(res.Claims))

	// Persisted on an uncancellable context, for the reason WebActor's step 7
	// is: by here the model calls are made and the ledger will charge for them
	// whether or not this write lands, and the report is generated from the
	// store rather than from this Result.
	if len(res.Claims) > 0 && a.Store != nil {
		if err := a.Store.WithTx(context.WithoutCancel(ctx), func(ctx context.Context, tx store.Tx) error {
			return tx.InsertClaims(ctx, res.Claims)
		}); err != nil {
			return res, fmt.Errorf("actors/academic: persist claims: %w", err)
		}
	}
	return res, nil
}

// gather searches every provider and merges the results.
//
// A search failure on one provider is not fatal: arXiv and PubMed index
// different literatures, and half an answer beats none. The error is returned
// only when nothing at all came back.
func (a *AcademicActor) gather(ctx context.Context, lead core.Lead, max int) ([]academic.Paper, error) {
	var (
		papers  []academic.Paper
		lastErr error
	)
	seen := map[string]bool{}

	for _, prov := range a.Providers {
		res, err := prov.Search(ctx, lead.Query, academic.Options{MaxResults: max})
		if err != nil {
			lastErr = err
			a.logger().WarnContext(ctx, "academic search failed",
				"provider", prov.Kind(), "err", err)
			continue
		}
		for _, p := range res.Papers {
			key := dedupeKey(p)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			papers = append(papers, p)
		}
	}

	// Newest first. When two papers disagree, §11.2 wants the recent one in
	// hand; and a truncated read should keep current work rather than whatever
	// the provider happened to rank first.
	sort.SliceStable(papers, func(i, j int) bool {
		switch {
		case papers[i].PublishedAt == nil:
			return false
		case papers[j].PublishedAt == nil:
			return true
		default:
			return papers[i].PublishedAt.After(*papers[j].PublishedAt)
		}
	})
	if len(papers) > max {
		papers = papers[:max]
	}
	return papers, lastErr
}

// dedupeKey identifies a paper across providers.
//
// DOI first, because the same paper is routinely on both arXiv and PubMed and
// mining it twice would pay twice and inflate corroboration — §11.3 counts
// independent publishers, and one paper indexed twice is not two publishers.
func dedupeKey(p academic.Paper) string {
	switch {
	case strings.TrimSpace(p.DOI) != "":
		return "doi:" + strings.ToLower(strings.TrimSpace(p.DOI))
	case strings.TrimSpace(p.ArXivID) != "":
		return "arxiv:" + strings.TrimSpace(p.ArXivID)
	case strings.TrimSpace(p.PMID) != "":
		return "pmid:" + strings.TrimSpace(p.PMID)
	default:
		return strings.ToLower(collapseSpace(p.Title))
	}
}

// citationURL is what a claim from this paper cites.
//
// A reader has to be able to open it, so it is the landing page or the DOI —
// never the API endpoint the text arrived through.
func citationURL(p academic.Paper) string {
	switch {
	case strings.TrimSpace(p.LandingURL) != "":
		return p.LandingURL
	case strings.TrimSpace(p.DOI) != "":
		return "https://doi.org/" + strings.TrimSpace(p.DOI)
	case strings.TrimSpace(p.HTMLURL) != "":
		return p.HTMLURL
	default:
		return p.PDFURL
	}
}

// summarize writes the planner's view of this lead, mechanically.
//
// No model call, unlike WebActor's reduce. The digest needs coverage — what was
// looked at and how much came back — and paying a strong-tier call to phrase
// that is the kind of spend the escalation ladder exists to avoid. It also keeps
// §9.1's guarantee that no page-derived text reaches the planner: titles here
// are the providers' own metadata, not mined content.
func summarize(query string, papers []academic.Paper, claims int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Read %d paper(s) for %q, yielding %d claim(s).", len(papers), query, claims)

	shown := papers
	if len(shown) > 5 {
		shown = shown[:5]
	}
	for _, p := range shown {
		year := ""
		if p.PublishedAt != nil {
			year = fmt.Sprintf(" (%d)", p.PublishedAt.Year())
		}
		fmt.Fprintf(&b, "\n  - %s%s", collapseSpace(p.Title), year)
	}
	return b.String()
}
