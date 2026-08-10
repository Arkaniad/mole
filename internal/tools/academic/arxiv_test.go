package academic_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/academic"
	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// A real arXiv response, trimmed. Kept verbatim rather than hand-simplified:
// the whitespace in <title> and <summary> is exactly how arXiv sends it, and
// collapsing it is a behaviour worth testing against the real shape.
const arxivFeed = `<?xml version='1.0' encoding='UTF-8'?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <entry>
    <id>http://arxiv.org/abs/2401.13660v3</id>
    <title>MambaByte: Token-free Selective
  State Space Model</title>
    <updated>2024-08-09T20:18:57Z</updated>
    <link href="https://arxiv.org/abs/2401.13660v3" rel="alternate" type="text/html"/>
    <link href="https://arxiv.org/pdf/2401.13660v3" rel="related" type="application/pdf" title="pdf"/>
    <summary>Token-free language models learn directly from raw bytes.
  This results in a $2.6\times$ inference speedup.</summary>
    <published>2024-01-24T18:53:53Z</published>
    <arxiv:doi>10.1000/journal.5678</arxiv:doi>
    <author><name>Junxiong Wang</name></author>
    <author><name>Alexander M. Rush</name></author>
  </entry>
</feed>`

func newArXiv(t *testing.T, h http.HandlerFunc) (*academic.ArXiv, *limiter.Limiter) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	lim := limiter.New(limiter.Unlimited)
	academic.Register(lim)
	// The real interval is three seconds; a test that honoured it would take
	// minutes. The LIMIT is asserted separately in academic_test.go — what is
	// tested here is that the provider consults the limiter at all.
	lim.SetClock(time.Now, func(context.Context, time.Duration) error { return nil })

	p, err := academic.NewArXiv(academic.Config{
		ContactEmail: "someone@example.org", BaseURL: srv.URL,
	}, srv.Client(), lim)
	if err != nil {
		t.Fatal(err)
	}
	return p, lim
}

func TestArXivSearchParsesARealResponse(t *testing.T) {
	var gotQuery, gotAgent string
	p, _ := newArXiv(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("search_query")
		gotAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(arxivFeed))
	})

	res, err := p.Search(context.Background(), "mambabyte", academic.Options{MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "all:mambabyte" {
		t.Errorf("search_query = %q", gotQuery)
	}
	// §10.3: arXiv asks callers to identify themselves, and the Atom API takes
	// no email parameter — so it has to be the User-Agent or nowhere.
	if !strings.Contains(gotAgent, "someone@example.org") {
		t.Errorf("User-Agent does not carry the contact address: %q", gotAgent)
	}
	if len(res.Papers) != 1 {
		t.Fatalf("%d papers, want 1", len(res.Papers))
	}

	got := res.Papers[0]
	if want := "MambaByte: Token-free Selective State Space Model"; got.Title != want {
		t.Errorf("title = %q, want %q — arXiv hard-wraps titles and the newline was kept", got.Title, want)
	}
	if strings.Contains(got.Abstract, "\n") {
		t.Errorf("abstract still carries newlines; §11.5 verifies quotes verbatim "+
			"so a stray break is a failed match: %q", got.Abstract)
	}
	if got.ArXivID != "2401.13660v3" {
		t.Errorf("arXiv id = %q", got.ArXivID)
	}
	if got.DOI != "10.1000/journal.5678" {
		t.Errorf("DOI = %q — the journal DOI is the one Unpaywall can resolve", got.DOI)
	}
	if got.PDFURL == "" || got.LandingURL == "" {
		t.Errorf("links not mapped: pdf=%q landing=%q", got.PDFURL, got.LandingURL)
	}
	if len(got.Authors) != 2 {
		t.Errorf("%d authors, want 2", len(got.Authors))
	}
	if !got.OpenAccess {
		t.Error("arXiv papers are open access by definition")
	}
}

// TestArXivReportsThePublishedDateNotTheRevision. §11.2 compares when a claim
// was MADE; taking `updated` would make a paper corrected for a typo look newer
// than one that superseded it. This is also the first place mole gets exact
// publication dates at all — web sources supply them rarely and unreliably.
func TestArXivReportsThePublishedDateNotTheRevision(t *testing.T) {
	p, _ := newArXiv(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(arxivFeed))
	})
	res, err := p.Search(context.Background(), "mambabyte", academic.Options{})
	if err != nil {
		t.Fatal(err)
	}
	at := res.Papers[0].PublishedAt
	if at == nil {
		t.Fatal("no publication date")
	}
	if got := at.UTC().Format("2006-01-02"); got != "2024-01-24" {
		t.Fatalf("PublishedAt = %s, want the 2024-01-24 <published> date, not the "+
			"2024-08-09 <updated> revision", got)
	}
}

// TestArXivLeavesHTMLUnset is a property of the API, not a limitation of the
// parser: a real arXiv response carries the PDF link and the abs page and
// nothing else. Deriving an HTML URL and calling it available would put a guess
// where FullTextFormat expects a fact, and the PDF-coverage measurement reads
// that field.
func TestArXivLeavesHTMLUnset(t *testing.T) {
	p, _ := newArXiv(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(arxivFeed))
	})
	res, err := p.Search(context.Background(), "mambabyte", academic.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Papers[0].HTMLURL; got != "" {
		t.Fatalf("HTMLURL = %q, but arXiv's API never reports HTML availability", got)
	}
	if got := academic.ArXivHTMLURL("2401.13660v3"); got != "https://arxiv.org/html/2401.13660v3" {
		t.Fatalf("candidate URL = %q", got)
	}
}

func TestArXivWaitsOnItsLimiter(t *testing.T) {
	var waited int
	p, lim := newArXiv(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(arxivFeed))
	})
	lim.SetClock(time.Now, func(context.Context, time.Duration) error {
		waited++
		return nil
	})
	// Burst 1: the second call must wait, which is arXiv's "single connection".
	for i := 0; i < 2; i++ {
		if _, err := p.Search(context.Background(), "q", academic.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if waited == 0 {
		t.Fatal("no wait was taken; the provider is not going through the limiter " +
			"and §10.3's constraint is not in force")
	}
}

// TestArXivTreats503AsTransient. arXiv answers a caller going too fast with 503
// rather than 429. Classified as a failure it would abort the lead; classified
// as transient the executor retries with backoff, which is what §9.5 asks for.
func TestArXivTreats503AsTransient(t *testing.T) {
	p, _ := newArXiv(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := p.Search(context.Background(), "q", academic.Options{})
	if err == nil {
		t.Fatal("a 503 was accepted")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("503 is not reported as rate limiting: %v", err)
	}
}

func TestArXivResolveRefusesAForeignDOI(t *testing.T) {
	p, _ := newArXiv(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("id_list"); got != "2401.13660" {
			t.Errorf("id_list = %q, want the bare arXiv id", got)
		}
		_, _ = w.Write([]byte(arxivFeed))
	})

	if _, err := p.Resolve(context.Background(), "10.48550/arXiv.2401.13660"); err != nil {
		t.Fatalf("arXiv's own DOI form was refused: %v", err)
	}
	// A publisher DOI is not arXiv's to answer. Reporting "not found" would say
	// the paper does not exist, when it exists somewhere arXiv cannot see.
	if _, err := p.Resolve(context.Background(), "10.1038/s41586-020-2649-2"); err == nil {
		t.Fatal("a foreign DOI was accepted")
	}
}
