package actors

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/academic"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
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

	// Fetch and Extract read tier-1 full text. Nil disables escalation
	// entirely, which is a supported configuration: the abstracts still yield
	// claims, and §10.2's ladder is about reading LESS, so refusing to read
	// more is never wrong, only less complete.
	Fetch   fetch.Fetcher
	Extract extract.Extractor

	// Rank orders passages by relevance to a sub-question. See rankChunks for
	// why it is injected rather than imported.
	Rank func(question string, passages []string, n int) []int

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

		// Tier 1. Only for a paper whose full text is known readable, and only
		// when the abstract did not answer the question — see shouldEscalate.
		if p.FullTextFormat() == academic.FormatHTML && shouldEscalate(lead.Query, out.Claims) {
			res.Claims = append(res.Claims, a.readFullText(ctx, lead, p, miner, maxClaims, res)...)
		}
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

// ---------------------------------------------------------------------------
// Tier 1: full text, ranked
// ---------------------------------------------------------------------------

// DefaultEscalationSections is how many passages of full text one paper may
// contribute after its abstract was judged insufficient.
//
// Small on purpose. The whole argument for the ladder is that reading a whole
// paper to answer one sub-question is the expensive default; escalating from
// "one abstract" to "one whole paper" would give the saving straight back.
const DefaultEscalationSections = 3

// shouldEscalate decides whether the abstract answered the sub-question.
//
// Mechanical, and deliberately so. Asking the model "did that answer it?" costs
// nothing extra — it can ride on the mining call — but it is a model grading its
// own sufficiency, and the thing being decided is whether to spend more money.
// A gate that cannot be talked into spending is worth more than a slightly
// better-informed one.
//
// The rule: the sub-question's distinctive terms have to appear in what came
// back. Zero claims escalates outright; claims that never mention what was asked
// about are the case this exists for — an abstract that says a paper is "about
// metabolic health" while the question asks about sample size.
func shouldEscalate(query string, claims []core.Claim) bool {
	terms := contentTerms(query)
	if len(terms) == 0 {
		return false
	}
	if len(claims) == 0 {
		return true
	}

	var haystack strings.Builder
	for _, c := range claims {
		haystack.WriteString(strings.ToLower(c.Text))
		haystack.WriteByte(' ')
		haystack.WriteString(strings.ToLower(c.Quote))
		haystack.WriteByte(' ')
	}
	text := haystack.String()

	covered := 0
	for _, t := range terms {
		if strings.Contains(text, t) {
			covered++
		}
	}
	// Half, not all: an abstract rarely echoes every word of a question, and
	// demanding full coverage would escalate almost always, which is the same as
	// having no ladder.
	return covered*2 < len(terms)
}

// academicStopwords are words too common to distinguish one sub-question from
// another. Short and closed; a longer list is a tuning exercise that cannot be
// evaluated without the corpus.
var academicStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "what": true,
	"does": true, "how": true, "why": true, "are": true, "was": true,
	"were": true, "is": true, "in": true, "of": true, "to": true, "on": true,
	"a": true, "an": true, "do": true, "did": true, "than": true, "that": true,
	"this": true, "from": true, "when": true, "which": true, "study": true,
	"studies": true, "paper": true, "research": true,
}

func contentTerms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.Trim(f, ".,;:!?\"'()[]")
		if len(f) < 4 || academicStopwords[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// readFullText fetches a paper's full text and mines the passages most relevant
// to the lead.
//
// Only ever called for a paper whose full text is KNOWN readable — PMC says so
// from metadata, and arXiv HTML has been probed. A paper whose only copy is a
// PDF is left alone and recorded, which is what turns the PDF question into a
// number rather than an assumption (§10.4).
func (a *AcademicActor) readFullText(
	ctx context.Context,
	lead core.Lead,
	p academic.Paper,
	miner *Miner,
	maxClaims int,
	res *Result,
) []core.Claim {
	if a.Fetch == nil || a.Extract == nil || strings.TrimSpace(p.HTMLURL) == "" {
		return nil
	}

	fetched, err := a.Fetch.Fetch(ctx, p.HTMLURL)
	if err != nil || fetched == nil || !fetched.Outcome.Usable() {
		outcome := fetch.OutcomeNetworkError
		if fetched != nil {
			outcome = fetched.Outcome
		}
		a.logger().DebugContext(ctx, "full text unavailable", "url", p.HTMLURL, "outcome", outcome)
		res.Stats.ChunksFailed++
		return nil
	}
	res.Stats.Fetched++

	pageURL, perr := url.Parse(p.HTMLURL)
	if perr != nil {
		return nil
	}
	doc, err := a.Extract.Extract(ctx, fetched.Content, fetched.ContentType, pageURL)
	if err != nil || doc == nil || strings.TrimSpace(doc.Text) == "" {
		res.Stats.ChunksFailed++
		return nil
	}

	chunks := llm.Split(doc.Text, llm.ChunkOptions{MaxChars: 6000, OverlapChars: 200, MinChars: 400})
	if len(chunks) == 0 {
		return nil
	}

	order := a.rankChunks(lead.Query, chunks)
	if len(order) > DefaultEscalationSections {
		order = order[:DefaultEscalationSections]
	}

	var claims []core.Claim
	for _, idx := range order {
		chunk := chunks[idx]
		out, err := miner.Mine(ctx, MineInput{
			Lead: lead,
			// Cited as the paper, not as the full-text URL: a reader following
			// a citation wants the paper, and PMC's article page is where the
			// quote can be checked either way.
			SourceURL:   citationURL(p),
			Title:       p.Title,
			PublishedAt: p.PublishedAt,
			Text:        chunk.Text,
			Offset:      chunk.Start,
			MaxClaims:   maxClaims,
		})
		if out.HasCall {
			res.Costs = append(res.Costs, out.Call)
		}
		res.Stats.ClaimsProposed += out.Proposed
		res.Stats.ClaimsRejected += out.Rejected
		res.Stats.Chunks++
		if err != nil {
			res.Stats.ChunksFailed++
			continue
		}
		claims = append(claims, out.Claims...)
	}
	return claims
}

// rankChunks orders passages by relevance to the question.
//
// Ranking, not searching. The instinct is to have a model say which part holds
// the answer and then binary-search the rest, but binary search needs the probe
// to say which half to go to next — and not finding an answer in one chunk says
// nothing about which other chunk holds it, so it degrades to a linear scan at
// one model call per probe.
//
// Rank is INJECTED rather than imported. verifier.LexicalRetriever is the right
// scorer and already does this for research.ask, but verifier/ground.go imports
// this package, so importing it back would be a cycle. The caller supplies it;
// unset, passages are read in document order, which for a paper puts the
// abstract and introduction first and is a defensible default rather than a
// silent failure.
func (a *AcademicActor) rankChunks(query string, chunks []llm.Chunk) []int {
	if a.Rank == nil {
		order := make([]int, len(chunks))
		for i := range order {
			order[i] = i
		}
		return order
	}
	passages := make([]string, len(chunks))
	for i, c := range chunks {
		passages[i] = c.Text
	}
	return a.Rank(query, passages, len(passages))
}

// ShouldEscalateForTest exposes the escalation gate to the package's tests.
//
// Exported for tests only, and named so. The gate is the one decision in this
// actor that spends money, so it is worth testing directly rather than through
// a run whose result could satisfy the assertion for other reasons.
func ShouldEscalateForTest(query string, claims []core.Claim) bool {
	return shouldEscalate(query, claims)
}
