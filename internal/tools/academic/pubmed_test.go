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

// efetchXML is shaped like a real record, including the trap: the article
// embeds a reference list whose entries carry their OWN ArticleId elements. The
// cited paper's PMC id comes first in document order under a descendant search,
// which is what makes a `.//ArticleId` parser attribute the wrong full text.
const efetchXML = `<?xml version="1.0"?>
<PubmedArticleSet>
 <PubmedArticle>
  <MedlineCitation>
   <PMID>32003791</PMID>
   <Article>
    <Journal><JournalIssue><PubDate>
      <Year>2020</Year><Month>May</Month><Day>01</Day>
    </PubDate></JournalIssue></Journal>
    <ArticleTitle>Coolpup.py: versatile pile-up
      analysis of Hi-C data.</ArticleTitle>
    <Abstract>
     <AbstractText Label="MOTIVATION">Hi-C is the method of choice.</AbstractText>
     <AbstractText Label="RESULTS">We describe coolpup.py, a versatile tool.</AbstractText>
    </Abstract>
    <AuthorList>
     <Author><LastName>Flyamer</LastName><ForeName>Ilya</ForeName></Author>
    </AuthorList>
   </Article>
   <CommentsCorrectionsList>
    <CommentsCorrections>
     <PMID>99999999</PMID>
    </CommentsCorrections>
   </CommentsCorrectionsList>
  </MedlineCitation>
  <PubmedData>
   <ArticleIdList>
    <ArticleId IdType="pubmed">32003791</ArticleId>
    <ArticleId IdType="pmc">PMC7214034</ArticleId>
    <ArticleId IdType="doi">10.1093/bioinformatics/btaa073</ArticleId>
   </ArticleIdList>
   <ReferenceList>
    <Reference>
     <ArticleIdList>
      <ArticleId IdType="pmc">PMC0000001</ArticleId>
      <ArticleId IdType="doi">10.9999/cited.paper</ArticleId>
     </ArticleIdList>
    </Reference>
   </ReferenceList>
  </PubmedData>
 </PubmedArticle>
</PubmedArticleSet>`

const esearchJSON = `{"esearchresult":{"count":"734","idlist":["32003791"]}}`

func newPubMed(t *testing.T, h http.HandlerFunc) *academic.PubMed {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	lim := limiter.New(limiter.Unlimited)
	academic.Register(lim)
	lim.SetClock(time.Now, func(context.Context, time.Duration) error { return nil })

	p, err := academic.NewPubMed(academic.Config{
		ContactEmail: "someone@example.org", BaseURL: srv.URL,
	}, srv.Client(), lim)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func routeNCBI(t *testing.T, seen *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path)
		// §10.3: NCBI documents tool and email as required on EVERY request.
		if r.URL.Query().Get("tool") == "" || r.URL.Query().Get("email") == "" {
			t.Errorf("%s sent without tool/email: %s", r.URL.Path, r.URL.RawQuery)
		}
		switch {
		case strings.Contains(r.URL.Path, "esearch"):
			_, _ = w.Write([]byte(esearchJSON))
		case strings.Contains(r.URL.Path, "efetch"):
			_, _ = w.Write([]byte(efetchXML))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}
}

// TestPubMedTakesTheArticlesOwnIdentifiers is the one that matters.
//
// A real record carried 4 identifiers of its own and 80 belonging to papers it
// cites. Taking a descendant match would attach a cited paper's PMC id here, and
// mole would fetch someone else's full text believing it was this paper's — a
// claim with a verbatim quote from the wrong document.
func TestPubMedTakesTheArticlesOwnIdentifiers(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))

	res, err := p.Search(context.Background(), "hi-c pile-up", academic.Options{MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Papers) != 1 {
		t.Fatalf("%d papers, want 1", len(res.Papers))
	}
	got := res.Papers[0]

	if got.PMCID != "PMC7214034" {
		t.Errorf("PMCID = %q, want the article's own — a reference's id was taken", got.PMCID)
	}
	if got.DOI != "10.1093/bioinformatics/btaa073" {
		t.Errorf("DOI = %q, want the article's own", got.DOI)
	}
	if strings.Contains(got.HTMLURL, "PMC0000001") {
		t.Fatalf("full-text URL points at a CITED paper: %q", got.HTMLURL)
	}
}

// TestPubMedSearchUsesTwoRequests. esearch then efetch — no esummary and no
// elink, because efetch carries the identifiers and the abstracts that neither
// of the others returns. A third fewer requests against a shared public service.
func TestPubMedSearchUsesTwoRequests(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))
	if _, err := p.Search(context.Background(), "q", academic.Options{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("%d requests %v, want 2 (esearch, efetch)", len(seen), seen)
	}
}

// TestPubMedKeepsAbstractSectionLabels. A clinical abstract is several labelled
// sections, and the labels are the most useful thing in it: RESULTS is where the
// number is, and a claim mined from CONCLUSIONS is interpretation rather than
// measurement. Concatenating the text alone runs them into one argument.
func TestPubMedKeepsAbstractSectionLabels(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))
	res, err := p.Search(context.Background(), "q", academic.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ab := res.Papers[0].Abstract
	for _, want := range []string{"MOTIVATION:", "RESULTS:", "Hi-C is the method of choice."} {
		if !strings.Contains(ab, want) {
			t.Errorf("abstract missing %q: %q", want, ab)
		}
	}
	if strings.Contains(ab, "\n") {
		t.Errorf("abstract carries newlines; §11.5 verifies quotes verbatim: %q", ab)
	}
}

func TestPubMedCollapsesWrappedTitles(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))
	res, _ := p.Search(context.Background(), "q", academic.Options{})
	want := "Coolpup.py: versatile pile-up analysis of Hi-C data."
	if got := res.Papers[0].Title; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
}

// TestAPMCIdentifierMeansReadableFullText. Unlike arXiv's HTML, this is
// reported rather than guessed, so it is a fact FullTextFormat can rest on.
func TestAPMCIdentifierMeansReadableFullText(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))
	res, _ := p.Search(context.Background(), "q", academic.Options{})
	got := res.Papers[0]

	if got.FullTextFormat() != academic.FormatHTML {
		t.Fatalf("format = %q, want html — a PMC id is readable full text", got.FullTextFormat())
	}
	if want := "https://pmc.ncbi.nlm.nih.gov/articles/PMC7214034/"; got.HTMLURL != want {
		t.Fatalf("HTMLURL = %q, want %q — the www.ncbi.nlm.nih.gov/pmc path 301s here, "+
			"and following a redirect on every fetch is a request NCBI need not serve",
			got.HTMLURL, want)
	}
}

func TestPubMedParsesPartialDates(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))
	res, _ := p.Search(context.Background(), "q", academic.Options{})
	at := res.Papers[0].PublishedAt
	if at == nil {
		t.Fatal("no date")
	}
	if got := at.UTC().Format("2006-01-02"); got != "2020-05-01" {
		t.Fatalf("PublishedAt = %s, want 2020-05-01 (month arrives as a NAME)", got)
	}
}

// TestPubMedResolvesADOIThroughTheAIDField. PubMed has no DOI endpoint; the
// identifier is indexed under [AID].
func TestPubMedResolvesADOIThroughTheAIDField(t *testing.T) {
	var term string
	p := newPubMed(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "esearch") {
			term = r.URL.Query().Get("term")
			_, _ = w.Write([]byte(esearchJSON))
			return
		}
		_, _ = w.Write([]byte(efetchXML))
	})

	if _, err := p.Resolve(context.Background(), "10.1093/bioinformatics/btaa073"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(term, "[AID]") {
		t.Fatalf("DOI lookup used term %q, want the [AID] field", term)
	}
}

// TestPubMedResolveSkipsTheSearchForAPMID. A numeric id needs no lookup, and
// spending a rate-limited request to discover that is the cost of not checking.
func TestPubMedResolveSkipsTheSearchForAPMID(t *testing.T) {
	var seen []string
	p := newPubMed(t, routeNCBI(t, &seen))
	if _, err := p.Resolve(context.Background(), "32003791"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "efetch") {
		t.Fatalf("requests %v, want a single efetch", seen)
	}
}
