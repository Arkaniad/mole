package academic

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// DefaultPubMedBaseURL is NCBI's E-utilities root.
const DefaultPubMedBaseURL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils"

// PubMed searches PubMed and links to PMC full text. Keyless (§10.2).
//
// Two requests per search, not three. esummary carries clean identifiers and a
// normalized date, and elink would give the PMC link — but efetch has all of it
// alongside the abstracts, which the other two do not return at all. Going
// straight from esearch to efetch is a third fewer requests against a shared
// public service, which is the kind of arithmetic §10.3 is about.
type PubMed struct {
	client  *http.Client
	lim     *limiter.Limiter
	baseURL string
	contact string
	agent   string
}

func NewPubMed(cfg Config, client *http.Client, lim *limiter.Limiter) (*PubMed, error) {
	cfg.Kind = KindPubMed
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultPubMedBaseURL
	}
	return &PubMed{
		client:  client,
		lim:     lim,
		baseURL: strings.TrimRight(base, "/"),
		contact: strings.TrimSpace(cfg.ContactEmail),
		agent:   fmt.Sprintf("mole (+contact: %s)", strings.TrimSpace(cfg.ContactEmail)),
	}, nil
}

func (p *PubMed) Kind() Kind { return KindPubMed }

// identify adds the tool and email parameters NCBI requires on every request.
//
// Not optional and not a courtesy: E-utilities documents both as required, and
// requests without them are the ones that get an address blocked. In the
// User-Agent as well, because that is where a server operator looks first.
func (p *PubMed) identify(q url.Values) url.Values {
	q.Set("tool", "mole")
	q.Set("email", p.contact)
	return q
}

func (p *PubMed) Search(ctx context.Context, query string, opts Options) (*Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("academic: empty query")
	}
	max := opts.MaxResults
	if max <= 0 {
		max = DefaultMaxResults
	}

	ids, err := p.esearch(ctx, query, max)
	if err != nil {
		return nil, err
	}
	out := &Response{Query: query, Provider: KindPubMed}
	if len(ids) == 0 {
		return out, nil
	}
	papers, err := p.efetch(ctx, ids)
	if err != nil {
		return nil, err
	}
	out.Papers = papers
	return out, nil
}

// Resolve looks a paper up by PMID or DOI.
//
// A DOI goes through esearch with the [AID] field, which is how PubMed indexes
// article identifiers — there is no direct DOI endpoint. Two requests rather
// than one, and worth it: a DOI is what the other providers hand around.
func (p *PubMed) Resolve(ctx context.Context, id string) (*Paper, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("academic: empty identifier")
	}

	pmids := []string{id}
	if !isPMID(id) {
		found, err := p.esearch(ctx, id+"[AID]", 1)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("academic: pubmed has no paper for %q", id)
		}
		pmids = found
	}

	papers, err := p.efetch(ctx, pmids)
	if err != nil {
		return nil, err
	}
	if len(papers) == 0 {
		return nil, fmt.Errorf("academic: pubmed has no paper for %q", id)
	}
	return &papers[0], nil
}

func isPMID(s string) bool {
	if len(s) < 4 || len(s) > 9 {
		return false
	}
	_, err := strconv.Atoi(s)
	return err == nil
}

func (p *PubMed) esearch(ctx context.Context, term string, max int) ([]string, error) {
	q := p.identify(url.Values{})
	q.Set("db", "pubmed")
	q.Set("term", term)
	q.Set("retmax", strconv.Itoa(max))
	q.Set("retmode", "json")

	body, err := p.get(ctx, p.baseURL+"/esearch.fcgi?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Result struct {
			IDList []string `json:"idlist"`
		} `json:"esearchresult"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("academic: pubmed: parse esearch: %w", err)
	}
	return parsed.Result.IDList, nil
}

func (p *PubMed) efetch(ctx context.Context, pmids []string) ([]Paper, error) {
	q := p.identify(url.Values{})
	q.Set("db", "pubmed")
	q.Set("id", strings.Join(pmids, ","))
	q.Set("retmode", "xml")

	body, err := p.get(ctx, p.baseURL+"/efetch.fcgi?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var set pubmedArticleSet
	if err := xml.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("academic: pubmed: parse efetch: %w", err)
	}

	out := make([]Paper, 0, len(set.Articles))
	for _, a := range set.Articles {
		if paper, ok := a.toPaper(); ok {
			out = append(out, paper)
		}
	}
	return out, nil
}

func (p *PubMed) get(ctx context.Context, rawURL string) ([]byte, error) {
	if p.lim != nil {
		if err := p.lim.Wait(ctx, LimiterKey(KindPubMed)); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.agent)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("academic: pubmed: %s", scrubURLError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return nil, fmt.Errorf("%w: pubmed returned %d", ErrRateLimited, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("academic: pubmed returned %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// ---------------------------------------------------------------------------
// efetch XML
// ---------------------------------------------------------------------------

type pubmedArticleSet struct {
	XMLName  xml.Name        `xml:"PubmedArticleSet"`
	Articles []pubmedArticle `xml:"PubmedArticle"`
}

// pubmedArticle mirrors only the parts mole reads.
//
// The paths are DIRECT, not descendant-or-self, and that is load-bearing. A
// PubmedArticle embeds its whole reference list, each reference carrying its own
// ArticleId elements — a real record returned four identifiers of its own and
// eighty belonging to papers it cites. A `.//ArticleId` search finds all of them
// in document order, so the "PMC id" picked up would frequently belong to a
// cited paper, and mole would go and read someone else's full text believing it
// was this one's.
type pubmedArticle struct {
	PMID    string         `xml:"MedlineCitation>PMID"`
	Article medlineArticle `xml:"MedlineCitation>Article"`
	IDs     []pubmedID     `xml:"PubmedData>ArticleIdList>ArticleId"`
}

type medlineArticle struct {
	// InnerXML, not chardata. PubMed declares ArticleTitle and AbstractText as
	// MIXED CONTENT, and biomedical records use it constantly: <i> for gene
	// names, <sub> for formulae, MathML for equations. encoding/xml Skips nested
	// elements when unmarshalling into a string, DISCARDING their text — so
	// "Regulation of <i>TP53</i> in CO<sub>2</sub>-rich media" parsed as
	// "Regulation of in CO-rich media". The subject of the sentence disappears
	// and CO2 silently becomes CO.
	//
	// §11.5 cannot catch this. FindQuote verifies the model's quote against the
	// same mangled string, so the quote matches and a corrupted claim is stored
	// as verified evidence. Measured before this changed.
	Title    xmlFragment    `xml:"ArticleTitle"`
	Abstract []abstractText `xml:"Abstract>AbstractText"`
	PubDate  pubmedDate     `xml:"Journal>JournalIssue>PubDate"`
}

// xmlFragment captures an element's mixed content verbatim.
//
// A wrapper type because encoding/xml only accepts ",innerxml" on a field of the
// struct mapped to the element, not as a path suffix.
type xmlFragment struct {
	Inner string `xml:",innerxml"`
}

type abstractText struct {
	Label string `xml:"Label,attr"`
	// InnerXML for the reason above. Stripped and unescaped by flattenXML.
	Text string `xml:",innerxml"`
}

type pubmedDate struct {
	Year  string `xml:"Year"`
	Month string `xml:"Month"`
	Day   string `xml:"Day"`
	// MedlineDate carries ranges like "2020 May-Jun" where a structured date
	// does not exist. Parsed for its year only; inventing a month from a range
	// would put a false precision into §11.2's staleness comparison.
	MedlineDate string `xml:"MedlineDate"`
}

type pubmedID struct {
	Type  string `xml:"IdType,attr"`
	Value string `xml:",chardata"`
}

func (a pubmedArticle) toPaper() (Paper, bool) {
	title := flattenXML(a.Article.Title.Inner)
	if title == "" {
		return Paper{}, false
	}

	p := Paper{
		Title:    title,
		Abstract: joinAbstract(a.Article.Abstract),
		PMID:     strings.TrimSpace(a.PMID),
		Source:   KindPubMed,
	}
	for _, id := range a.IDs {
		switch strings.ToLower(strings.TrimSpace(id.Type)) {
		case "doi":
			p.DOI = strings.TrimSpace(id.Value)
		case "pmc":
			p.PMCID = strings.TrimSpace(id.Value)
		}
	}
	if t, ok := a.Article.PubDate.parse(); ok {
		p.PublishedAt = &t
	}

	p.LandingURL = "https://pubmed.ncbi.nlm.nih.gov/" + p.PMID + "/"
	if p.PMCID != "" {
		// A PMC identifier means the full text is on PMC, and unlike arXiv's
		// HTML this is reported rather than guessed — so HTMLURL is a fact here.
		//
		// An upper bound, though, not a guarantee of access: an author
		// manuscript can be on PMC under embargo. A fetch that comes back
		// restricted is recorded as a fetch outcome (§10.4) rather than
		// silently counted as readable.
		p.HTMLURL = PMCArticleURL(p.PMCID)
	}
	return p, true
}

// PMCArticleURL is where a PMC article is read.
//
// pmc.ncbi.nlm.nih.gov, not the www.ncbi.nlm.nih.gov/pmc path the older
// documentation uses: that one 301s here, and following a redirect on every
// fetch is a request NCBI does not need to serve.
func PMCArticleURL(pmcid string) string {
	// Validated, not merely trimmed. The identifier is XML chardata from a
	// third party and is concatenated into a request path; the only thing
	// otherwise standing between it and an outbound URL is the literal host
	// prefix. "PMC" followed by digits is the whole of the real format.
	if !pmcIDPattern.MatchString(strings.TrimSpace(pmcid)) {
		return ""
	}
	return "https://pmc.ncbi.nlm.nih.gov/articles/" + strings.TrimSpace(pmcid) + "/"
}

var pmcIDPattern = regexp.MustCompile(`^PMC\d+$`)

// joinAbstract flattens PubMed's structured abstract.
//
// Clinical abstracts arrive as several labelled sections — BACKGROUND, METHODS,
// RESULTS, CONCLUSIONS — and concatenating the text alone runs them together
// into a paragraph that reads as one argument. The labels are kept because they
// are the most useful thing in a medical abstract: "RESULTS" is where the number
// is, and a claim mined from CONCLUSIONS is the authors' interpretation rather
// than their measurement.
func joinAbstract(parts []abstractText) string {
	var b strings.Builder
	for _, part := range parts {
		text := flattenXML(part.Text)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		if label := collapse(part.Label); label != "" {
			b.WriteString(label)
			b.WriteString(": ")
		}
		b.WriteString(text)
	}
	return b.String()
}

var pubmedMonths = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// parse turns PubMed's partial dates into a time.
//
// Year is the only part that is always present; month is a name as often as a
// number, and day is frequently absent. Missing parts default to the START of
// the period rather than being refused, because "published in May 2020" is
// genuinely more information than nothing for §11.2 — but nothing is invented
// beyond that, and a record with no year at all reports no date rather than a
// zero time that would sort as 1 January year one.
func (d pubmedDate) parse() (time.Time, bool) {
	year := strings.TrimSpace(d.Year)
	if year == "" && d.MedlineDate != "" {
		if f := strings.Fields(d.MedlineDate); len(f) > 0 {
			year = f[0]
		}
	}
	y, err := strconv.Atoi(strings.TrimSpace(year))
	if err != nil || y < 1500 {
		return time.Time{}, false
	}

	month := time.January
	raw := strings.ToLower(strings.TrimSpace(d.Month))
	if len(raw) >= 3 {
		if m, ok := pubmedMonths[raw[:3]]; ok {
			month = m
		}
	}
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 12 {
		month = time.Month(n)
	}

	day := 1
	if n, err := strconv.Atoi(strings.TrimSpace(d.Day)); err == nil && n >= 1 && n <= 31 {
		day = n
	}
	return time.Date(y, month, day, 0, 0, 0, 0, time.UTC), true
}

// flattenXML turns a mixed-content XML fragment into the text a reader sees.
//
// Tags out, entities in: innerxml hands back the raw fragment, so &amp;lt; is still
// an entity and <i>TP53</i> is still markup. Both have to be resolved, and in
// that order — unescaping first would turn an encoded &amp;lt;i&amp;gt; in the source
// text into a tag that the strip then deletes.
func flattenXML(fragment string) string {
	stripped := xmlTagPattern.ReplaceAllString(fragment, "")
	return collapse(html.UnescapeString(stripped))
}

var xmlTagPattern = regexp.MustCompile(`<[^>]*>`)
