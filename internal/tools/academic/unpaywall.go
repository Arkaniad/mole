package academic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// DefaultUnpaywallBaseURL is Unpaywall's v2 API.
const DefaultUnpaywallBaseURL = "https://api.unpaywall.org/v2"

// Unpaywall turns a DOI into a legal open-access copy (§10.2).
//
// A Resolver and not a Provider: it has no topical search, and it returns no
// abstract. What it knows is where a paper can lawfully be read, which is the
// one thing neither arXiv nor PubMed can answer for a publisher's article.
type Unpaywall struct {
	client  *http.Client
	lim     *limiter.Limiter
	baseURL string
	contact string
	agent   string
}

func NewUnpaywall(cfg Config, client *http.Client, lim *limiter.Limiter) (*Unpaywall, error) {
	cfg.Kind = KindUnpaywall
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = DefaultUnpaywallBaseURL
	}
	return &Unpaywall{
		client:  client,
		lim:     lim,
		baseURL: strings.TrimRight(base, "/"),
		contact: strings.TrimSpace(cfg.ContactEmail),
		agent:   fmt.Sprintf("mole (+contact: %s)", strings.TrimSpace(cfg.ContactEmail)),
	}, nil
}

func (u *Unpaywall) Kind() Kind { return KindUnpaywall }

var doiPattern = regexp.MustCompile(`(?i)10\.\d{4,9}/\S+`)

// Resolve finds where a DOI can legally be read.
func (u *Unpaywall) Resolve(ctx context.Context, id string) (*Paper, error) {
	doi := strings.TrimSpace(id)
	// Accept a bare DOI or a doi.org URL, since both are what the other
	// providers hand around.
	if m := doiPattern.FindString(doi); m != "" {
		doi = strings.TrimRight(m, ".,;)")
	} else {
		return nil, fmt.Errorf("academic: %q is not a DOI", id)
	}

	if u.lim != nil {
		if err := u.lim.Wait(ctx, LimiterKey(KindUnpaywall)); err != nil {
			return nil, err
		}
	}

	q := url.Values{}
	// Required by Unpaywall on every call (§10.3). Not a courtesy parameter:
	// requests without it are rejected, and it is how the operator reaches
	// whoever is generating traffic.
	q.Set("email", u.contact)

	// PathEscape, because the DOI is THIRD-PARTY DATA — it arrives from an
	// arXiv <arxiv:doi> element or a PubMed ArticleId, and doiPattern's \S+
	// tail accepts ?, # and @.
	//
	// Concatenated raw, a DOI of "10.1234/x?email=attacker@evil.example&" put
	// the attacker's address in the email parameter and left the real one
	// parsed under the junk key "?email" — so the identification §10.3 makes
	// mandatory was silently replaced with an address of the provider's
	// choosing, pointing Unpaywall's abuse contact at a third party. Verified
	// before this line existed.
	endpoint := u.baseURL + "/" + url.PathEscape(doi) + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", u.agent)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("academic: unpaywall: %s", scrubURLError(err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Unpaywall genuinely does not know this DOI. Distinct from "known and
		// closed", which is a 200 with is_oa false — conflating them would make
		// an unindexed paper look like a paywalled one.
		return nil, fmt.Errorf("academic: unpaywall has no record of %q", doi)
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: unpaywall returned %d", ErrRateLimited, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("academic: unpaywall returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("academic: unpaywall: read: %w", err)
	}
	var rec unpaywallRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("academic: unpaywall: parse: %w", err)
	}
	p := rec.toPaper(doi)
	return &p, nil
}

type unpaywallRecord struct {
	DOI           string              `json:"doi"`
	Title         string              `json:"title"`
	IsOA          bool                `json:"is_oa"`
	OAStatus      string              `json:"oa_status"`
	PublishedDate string              `json:"published_date"`
	Year          int                 `json:"year"`
	Best          *unpaywallLocation  `json:"best_oa_location"`
	Locations     []unpaywallLocation `json:"oa_locations"`
	Authors       []struct {
		Given  string `json:"given"`
		Family string `json:"family"`
	} `json:"z_authors"`
}

type unpaywallLocation struct {
	HostType   string `json:"host_type"`
	Version    string `json:"version"`
	License    string `json:"license"`
	URL        string `json:"url"`
	PDFURL     string `json:"url_for_pdf"`
	LandingURL string `json:"url_for_landing_page"`
}

func (r unpaywallRecord) toPaper(doi string) Paper {
	p := Paper{
		// Titles arrive with publisher markup and hard wrapping — one real
		// record began "<i>Coolpup.py:</i>\n                    versatile…".
		// Left as-is that reaches a prompt as pseudo-HTML and a citation as a
		// broken line.
		Title:      collapse(stripTags(r.Title)),
		DOI:        firstNonEmpty(strings.TrimSpace(r.DOI), doi),
		OpenAccess: r.IsOA,
		Source:     KindUnpaywall,
	}
	for _, a := range r.Authors {
		if n := collapse(a.Given + " " + a.Family); n != "" {
			p.Authors = append(p.Authors, n)
		}
	}
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(r.PublishedDate)); err == nil {
		p.PublishedAt = &t
	} else if r.Year > 1500 {
		t := time.Date(r.Year, time.January, 1, 0, 0, 0, 0, time.UTC)
		p.PublishedAt = &t
	}

	locs := r.Locations
	if r.Best != nil {
		// Considered alongside the rest, not instead of them — see pickLocations.
		locs = append([]unpaywallLocation{*r.Best}, locs...)
	}
	p.HTMLURL, p.PDFURL, p.LandingURL, p.PMCID = pickLocations(locs)
	return p
}

// pickLocations chooses what mole can actually READ, which is not what
// Unpaywall calls best.
//
// best_oa_location optimizes for the published version and the most permissive
// licence — both good things, and neither the question here. On a real record it
// selected a publisher PDF while a PMC copy of the same paper sat two entries
// down in oa_locations. Taking best blindly would have reported that paper as
// pdf_only, and pdf_only is the bucket the whole PDF-extractor decision turns
// on: the measurement meant to test "do we need a parser?" would have been
// biased toward yes by the resolver feeding it.
//
// So every location is scanned. A landing page counts as readable full text only
// on hosts where that is KNOWN — PMC today — because a publisher landing page is
// as often an abstract stub behind a paywall, and calling it full text would bias
// the same measurement the other way.
func pickLocations(locs []unpaywallLocation) (htmlURL, pdfURL, landingURL, pmcID string) {
	for _, l := range locs {
		for _, candidate := range []string{l.LandingURL, l.URL} {
			if id := pmcIDFromURL(candidate); id != "" && pmcID == "" {
				pmcID = id
				htmlURL = PMCArticleURL(id)
			}
		}
		if pdfURL == "" {
			if u := strings.TrimSpace(l.PDFURL); u != "" {
				pdfURL = u
			} else if u := strings.TrimSpace(l.URL); strings.HasSuffix(strings.ToLower(u), ".pdf") {
				pdfURL = u
			}
		}
		if landingURL == "" {
			landingURL = strings.TrimSpace(l.LandingURL)
		}
	}
	return htmlURL, pdfURL, landingURL, pmcID
}

// pmcIDFromURL recognises a PMC article in any of the forms Unpaywall reports.
//
// Both hosts and both spellings appear in real records: the current
// pmc.ncbi.nlm.nih.gov/articles/PMC7214034, the legacy
// www.ncbi.nlm.nih.gov/pmc/articles/7214034 with the prefix dropped, and with or
// without a trailing slash.
var pmcPathPattern = regexp.MustCompile(`(?i)^/(?:pmc/)?articles/(?:PMC)?(\d+)`)

// pmcHosts are the hosts whose article paths are PMC full text.
var pmcHosts = map[string]bool{
	"pmc.ncbi.nlm.nih.gov": true,
	"www.ncbi.nlm.nih.gov": true,
	"ncbi.nlm.nih.gov":     true,
}

// pmcIDFromURL recognises a PMC article, matching on HOST rather than substring.
//
// An unanchored pattern matched anywhere in the string, so a hostile OA-location
// of "https://attacker.example/?r=ncbi.nlm.nih.gov/articles/7214034" was read as
// PMC7214034 — and mole would then fetch and mine a DIFFERENT, real paper's full
// text and attribute it, with verbatim quotes, to the DOI that was asked about.
// The same misattribution class as PubMed's reference-list trap, one layer up.
func pmcIDFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !pmcHosts[strings.ToLower(u.Hostname())] {
		return ""
	}
	if m := pmcPathPattern.FindStringSubmatch(u.Path); m != nil {
		return "PMC" + m[1]
	}
	return ""
}

var tagPattern = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string { return tagPattern.ReplaceAllString(s, "") }

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
