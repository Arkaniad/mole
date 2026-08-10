package academic

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// DefaultArXivBaseURL is arXiv's Atom query endpoint.
//
// https, not the http the documentation still shows: plain http returned an
// empty body from here, and a search provider that silently yields nothing is
// worse than one that fails.
const DefaultArXivBaseURL = "https://export.arxiv.org/api/query"

// DefaultMaxResults bounds a search when the caller does not.
const DefaultMaxResults = 10

// ArXiv searches arXiv's Atom API. Keyless (§10.2).
type ArXiv struct {
	client  *http.Client
	lim     *limiter.Limiter
	baseURL string
	contact string
	agent   string
}

// NewArXiv builds the provider, refusing without a contact address (§10.3).
//
// No generic New yet: a constructor that switches over Kind would need an arm
// per provider, and the arms for PubMed and Unpaywall would be errors saying
// "not built". One factory arrives when there is something to dispatch.
func NewArXiv(cfg Config, client *http.Client, lim *limiter.Limiter) (*ArXiv, error) {
	cfg.Kind = KindArXiv
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultArXivBaseURL
	}
	return &ArXiv{
		client:  client,
		lim:     lim,
		baseURL: base,
		contact: strings.TrimSpace(cfg.ContactEmail),
		// arXiv asks callers to identify themselves. The contact address goes in
		// the User-Agent because the Atom API takes no email parameter — unlike
		// NCBI and Unpaywall, which do.
		agent: fmt.Sprintf("mole (+contact: %s)", strings.TrimSpace(cfg.ContactEmail)),
	}, nil
}

func (a *ArXiv) Kind() Kind { return KindArXiv }

// Search returns papers matching a query.
//
// Tier 0 of the escalation ladder, and the reason it is worth having: the
// abstract comes back IN THIS RESPONSE. A web lead pays a search plus a fetch
// plus extraction to reach less text than this, and the publication date here is
// exact rather than guessed from a page.
func (a *ArXiv) Search(ctx context.Context, query string, opts Options) (*Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("academic: empty query")
	}
	max := opts.MaxResults
	if max <= 0 {
		max = DefaultMaxResults
	}

	q := url.Values{}
	q.Set("search_query", "all:"+query)
	q.Set("start", "0")
	q.Set("max_results", fmt.Sprint(max))

	feed, err := a.get(ctx, a.baseURL+"?"+q.Encode())
	if err != nil {
		return nil, err
	}

	out := &Response{Query: query, Provider: KindArXiv}
	for _, e := range feed.Entries {
		if p, ok := entryToPaper(e); ok {
			out.Papers = append(out.Papers, p)
		}
	}
	return out, nil
}

// arXivIDPattern matches the identifier inside an arXiv abs URL or a DOI.
var arXivIDPattern = regexp.MustCompile(`(?i)(?:arxiv[.:/]|abs/)(\d{4}\.\d{4,5}(?:v\d+)?)`)

// Resolve looks a paper up by identifier.
//
// arXiv resolves arXiv IDs, not arbitrary DOIs — it has no index of other
// publishers' identifiers, and pretending otherwise would return "not found"
// for papers that exist somewhere else. A 10.48550/arXiv.NNNN DOI is accepted
// because arXiv assigns those itself and the id is recoverable from it;
// anything else is refused as not this provider's to answer.
func (a *ArXiv) Resolve(ctx context.Context, id string) (*Paper, error) {
	m := arXivIDPattern.FindStringSubmatch(strings.TrimSpace(id))
	bare := strings.TrimSpace(id)
	switch {
	case m != nil:
		bare = m[1]
	case !regexp.MustCompile(`^\d{4}\.\d{4,5}(v\d+)?$`).MatchString(bare):
		return nil, fmt.Errorf("academic: %q is not an arXiv identifier", id)
	}

	q := url.Values{}
	q.Set("id_list", bare)
	feed, err := a.get(ctx, a.baseURL+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	for _, e := range feed.Entries {
		if p, ok := entryToPaper(e); ok {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("academic: arXiv has no paper %q", bare)
}

// get performs one rate-limited, identified request.
//
// The limiter wait is INSIDE this method rather than at the call sites so no
// future path can reach arXiv without it — §10.3's constraint is a property of
// talking to the provider, not of any particular caller.
func (a *ArXiv) get(ctx context.Context, rawURL string) (*atomFeed, error) {
	if a.lim != nil {
		if err := a.lim.Wait(ctx, LimiterKey(KindArXiv)); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", a.agent)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("academic: arxiv: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		// arXiv answers a caller going too fast with 503 rather than 429. Both
		// map to the same sentinel so §9.5 classifies them as transient and the
		// executor retries with backoff instead of failing the lead.
		return nil, fmt.Errorf("%w: arxiv returned %d", ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("academic: arxiv returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("academic: arxiv: read: %w", err)
	}
	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("academic: arxiv: parse feed: %w", err)
	}
	return &feed, nil
}

// ---------------------------------------------------------------------------
// Atom
// ---------------------------------------------------------------------------

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	Title     string     `xml:"title"`
	Summary   string     `xml:"summary"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Authors   []atomName `xml:"author"`
	Links     []atomLink `xml:"link"`
	// DOI is arXiv's own extension namespace, and present only when the paper
	// has been published somewhere with a journal DOI. That is the DOI worth
	// having: Unpaywall resolves it to a publisher's open-access copy, whereas
	// arXiv's self-assigned 10.48550 DOI resolves back to arXiv.
	DOI string `xml:"http://arxiv.org/schemas/atom doi"`
}

type atomName struct {
	Name string `xml:"name"`
}

type atomLink struct {
	Href  string `xml:"href,attr"`
	Rel   string `xml:"rel,attr"`
	Type  string `xml:"type,attr"`
	Title string `xml:"title,attr"`
}

func entryToPaper(e atomEntry) (Paper, bool) {
	title := collapse(e.Title)
	abstract := collapse(e.Summary)
	if title == "" {
		return Paper{}, false
	}

	p := Paper{
		Title:    title,
		Abstract: abstract,
		DOI:      strings.TrimSpace(e.DOI),
		ArXivID:  arXivIDFromURL(e.ID),
		Source:   KindArXiv,
		// Everything on arXiv is free to read. Not an inference from the links:
		// it is what arXiv is.
		OpenAccess: true,
	}
	for _, au := range e.Authors {
		if n := collapse(au.Name); n != "" {
			p.Authors = append(p.Authors, n)
		}
	}
	for _, l := range e.Links {
		switch {
		case strings.EqualFold(l.Title, "pdf"), l.Type == "application/pdf":
			p.PDFURL = l.Href
		case l.Rel == "alternate", l.Type == "text/html":
			p.LandingURL = l.Href
		}
	}

	// PublishedAt is `published`, the v1 date, not `updated`. A claim mined from
	// this paper was first made then, and §11.2's staleness rule compares when
	// things were CLAIMED — taking the latest revision would make a paper
	// corrected for a typo look newer than one that superseded it.
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Published)); err == nil {
		p.PublishedAt = &t
	}

	// HTMLURL is deliberately left unset.
	//
	// arXiv has served LaTeXML HTML for papers since late 2023, but the Atom API
	// does not report whether a given paper has it — a real response carries the
	// PDF link and the abs page and nothing else. Deriving the URL and calling it
	// available would put a guess where FullTextFormat expects a fact, and the
	// PDF-coverage measurement reads that field. Availability is a HEAD, taken
	// where it is worth paying for: at escalation, when the text is about to be
	// fetched anyway. See ArXivHTMLURL.
	return p, true
}

// ArXivHTMLURL is where a paper's HTML full text would live if it has any.
//
// A candidate, not a fact — see entryToPaper. The caller confirms it with a
// request; this only spells the URL so two places cannot spell it differently.
func ArXivHTMLURL(arXivID string) string {
	arXivID = strings.TrimSpace(arXivID)
	if arXivID == "" {
		return ""
	}
	return "https://arxiv.org/html/" + arXivID
}

func arXivIDFromURL(raw string) string {
	if m := arXivIDPattern.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	return ""
}

// collapse folds the whitespace arXiv wraps titles and abstracts in.
//
// Both arrive hard-wrapped with newlines and leading indentation. Left as-is,
// a title becomes a multi-line string in the digest and an abstract carries
// line breaks into the middle of the sentences a quote is verified against —
// and §11.5 verifies quotes verbatim, so a stray newline is a failed match.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }
