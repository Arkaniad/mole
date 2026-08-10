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

// unpaywallJSON reproduces a real record, including the thing that matters:
// best_oa_location is a publisher PDF while a readable PMC copy sits further
// down oa_locations. The title carries publisher markup and hard wrapping,
// exactly as returned.
const unpaywallJSON = `{
 "doi": "10.1093/bioinformatics/btaa073",
 "title": "<i>Coolpup.py:</i>\n                    versatile pile-up analysis of Hi-C data",
 "is_oa": true,
 "oa_status": "hybrid",
 "published_date": "2020-01-27",
 "year": 2020,
 "z_authors": [{"given": "Ilya", "family": "Flyamer"}],
 "best_oa_location": {
   "host_type": "publisher", "version": "publishedVersion", "license": "cc-by",
   "url": "https://academic.oup.com/bioinformatics/article-pdf/36/10/2980/x.pdf",
   "url_for_pdf": "https://academic.oup.com/bioinformatics/article-pdf/36/10/2980/x.pdf",
   "url_for_landing_page": "https://doi.org/10.1093/bioinformatics/btaa073"
 },
 "oa_locations": [
  {"host_type": "repository", "version": "submittedVersion",
   "url": "https://www.pure.ed.ac.uk/ws/files/134475161/btaa073.pdf",
   "url_for_pdf": "https://www.pure.ed.ac.uk/ws/files/134475161/btaa073.pdf",
   "url_for_landing_page": "https://hdl.handle.net/20.500.11820/7c2c516b"},
  {"host_type": "repository", "version": "submittedVersion",
   "url": "https://www.ncbi.nlm.nih.gov/pmc/articles/7214034",
   "url_for_pdf": null,
   "url_for_landing_page": "https://www.ncbi.nlm.nih.gov/pmc/articles/7214034"}
 ]
}`

func newUnpaywall(t *testing.T, h http.HandlerFunc) *academic.Unpaywall {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	lim := limiter.New(limiter.Unlimited)
	academic.Register(lim)
	lim.SetClock(time.Now, func(context.Context, time.Duration) error { return nil })

	u, err := academic.NewUnpaywall(academic.Config{
		ContactEmail: "someone@example.org", BaseURL: srv.URL,
	}, srv.Client(), lim)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestUnpaywallPrefersWhatMoleCanRead is the finding this slice turned on.
//
// best_oa_location optimizes for published version and licence, not for what a
// text extractor can read. On the real record it selected a publisher PDF while
// a PMC copy of the same paper sat two entries down. Taking best blindly reports
// the paper as pdf_only — and pdf_only is the bucket the PDF-extractor decision
// turns on, so the measurement meant to test "do we need a parser?" would have
// been biased toward yes by the resolver feeding it.
func TestUnpaywallPrefersWhatMoleCanRead(t *testing.T) {
	u := newUnpaywall(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(unpaywallJSON))
	})

	p, err := u.Resolve(context.Background(), "10.1093/bioinformatics/btaa073")
	if err != nil {
		t.Fatal(err)
	}
	if p.FullTextFormat() != academic.FormatHTML {
		t.Fatalf("format = %q, want html — a readable PMC copy was listed and skipped "+
			"in favour of best_oa_location's PDF", p.FullTextFormat())
	}
	if want := "https://pmc.ncbi.nlm.nih.gov/articles/PMC7214034/"; p.HTMLURL != want {
		t.Fatalf("HTMLURL = %q, want %q", p.HTMLURL, want)
	}
	if p.PMCID != "PMC7214034" {
		t.Errorf("PMCID = %q — Unpaywall reports the legacy form without the prefix", p.PMCID)
	}
	// The PDF is still recorded: it is the published version, and an escalation
	// that cannot read HTML should still know where the PDF was.
	if !strings.HasSuffix(p.PDFURL, ".pdf") {
		t.Errorf("PDF location was lost: %q", p.PDFURL)
	}
}

// TestUnpaywallStripsPublisherMarkupFromTitles. One real record began
// "<i>Coolpup.py:</i>\n                    versatile…". Left alone that reaches a
// prompt as pseudo-HTML and a citation as a broken line.
func TestUnpaywallStripsPublisherMarkupFromTitles(t *testing.T) {
	u := newUnpaywall(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(unpaywallJSON))
	})
	p, err := u.Resolve(context.Background(), "10.1093/bioinformatics/btaa073")
	if err != nil {
		t.Fatal(err)
	}
	want := "Coolpup.py: versatile pile-up analysis of Hi-C data"
	if p.Title != want {
		t.Fatalf("title = %q, want %q", p.Title, want)
	}
}

// TestUnpaywallSendsTheRequiredEmail. §10.3: Unpaywall requires it on every
// call and rejects requests without it.
func TestUnpaywallSendsTheRequiredEmail(t *testing.T) {
	var got string
	u := newUnpaywall(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("email")
		_, _ = w.Write([]byte(unpaywallJSON))
	})
	if _, err := u.Resolve(context.Background(), "10.1093/bioinformatics/btaa073"); err != nil {
		t.Fatal(err)
	}
	if got != "someone@example.org" {
		t.Fatalf("email parameter = %q", got)
	}
}

// TestUnpaywallDistinguishesUnknownFromClosed. A 404 means Unpaywall has never
// heard of the DOI; a 200 with is_oa false means it knows the paper and there is
// no free copy. Reporting both as "not found" would make an unindexed paper look
// paywalled, and the coverage measurement counts those separately.
func TestUnpaywallDistinguishesUnknownFromClosed(t *testing.T) {
	unknown := newUnpaywall(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if _, err := unknown.Resolve(context.Background(), "10.9999/nope"); err == nil {
		t.Fatal("a 404 was accepted")
	} else if !strings.Contains(err.Error(), "no record") {
		t.Errorf("404 not reported as unknown: %v", err)
	}

	closed := newUnpaywall(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"doi":"10.1234/closed.paper","title":"Closed","is_oa":false,"oa_locations":[]}`))
	})
	p, err := closed.Resolve(context.Background(), "10.1234/closed.paper")
	if err != nil {
		t.Fatalf("a known closed paper was reported as an error: %v", err)
	}
	if p.FullTextFormat() != academic.FormatClosed {
		t.Errorf("format = %q, want closed", p.FullTextFormat())
	}
}

func TestUnpaywallRefusesANonDOI(t *testing.T) {
	u := newUnpaywall(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was made for something that is not a DOI")
	})
	if _, err := u.Resolve(context.Background(), "2401.13660"); err == nil {
		t.Fatal("an arXiv id was accepted as a DOI")
	}
}

func TestUnpaywallAcceptsADOIURL(t *testing.T) {
	var path string
	u := newUnpaywall(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(unpaywallJSON))
	})
	if _, err := u.Resolve(context.Background(), "https://doi.org/10.1093/bioinformatics/btaa073"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "10.1093/bioinformatics/btaa073") {
		t.Fatalf("request path = %q, want the bare DOI", path)
	}
}

func TestUnpaywallReportsTheExactPublicationDate(t *testing.T) {
	u := newUnpaywall(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(unpaywallJSON))
	})
	p, _ := u.Resolve(context.Background(), "10.1093/bioinformatics/btaa073")
	if p.PublishedAt == nil {
		t.Fatal("no date")
	}
	if got := p.PublishedAt.UTC().Format("2006-01-02"); got != "2020-01-27" {
		t.Fatalf("PublishedAt = %s, want 2020-01-27", got)
	}
}
