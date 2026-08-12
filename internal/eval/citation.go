package eval

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
)

// Citation accuracy (§14.3).
//
// The actor already checks a quote against the chunk it was extracted from
// (§11.5), which kills fabrication. This checks something that check cannot:
// that the quote is in the source the claim CITES. Those differ when a claim
// is attributed to the wrong URL — a real failure mode in a run that reads
// several sources, and one that produces a perfectly plausible citation
// pointing at a page that never said it.
//
// Concretely, it catches:
//   - a quote attributed to the wrong source
//   - a quote that no longer appears in the source at all
//   - a stored offset that does not locate the quote
//
// It does not catch fabrication (already dead at the actor boundary) or whether
// the quote SUPPORTS the claim, which needs a judge and stays blocked.
//
// Re-reading a source costs a fetch, which is why this is opt-in. Under
// cassette replay it costs nothing and is deterministic, which is the whole
// reason §14.1 was built before the actors.

// SourceReader re-reads a cited source as text.
type SourceReader interface {
	Text(ctx context.Context, rawURL string) (string, error)
}

// Verdict is one claim's citation outcome.
type Verdict string

const (
	// CitationVerified: the quote is present in the cited source.
	CitationVerified Verdict = "verified"
	// CitationMismatch: the source was read and the quote is not in it. This
	// is the finding the metric exists for.
	CitationMismatch Verdict = "mismatch"
	// CitationOffsetDrift: the quote is present, but not where the claim says.
	// Not a fabrication — the citation is sound — but a later re-verification
	// that trusts the offset would look at the wrong span.
	CitationOffsetDrift Verdict = "offset-drift"
	// CitationUnreachable: the source could not be re-read. Says nothing about
	// the claim, so it must not count against accuracy.
	CitationUnreachable Verdict = "unreachable"
	// CitationSkipped: the source was never fetched in the first place — the
	// search provider supplied its text — so re-fetching would compare against
	// a different extraction and manufacture mismatches.
	CitationSkipped Verdict = "skipped"
)

// CitationProblem is one claim that did not verify.
type CitationProblem struct {
	Verdict Verdict `json:"verdict"`
	Source  string  `json:"source"`
	Claim   string  `json:"claim"`
	Detail  string  `json:"detail"`
}

// CitationReport aggregates the pass.
type CitationReport struct {
	Verified    int `json:"verified"`
	Mismatch    int `json:"mismatch"`
	OffsetDrift int `json:"offset_drift"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`

	Problems []CitationProblem `json:"problems,omitempty"`
}

// Checked is the number of claims the metric could actually judge. Unreachable
// and skipped sources are excluded: counting them would let a run with no
// network report perfect accuracy.
func (r CitationReport) Checked() int { return r.Verified + r.Mismatch + r.OffsetDrift }

// VerifyCitations re-reads each cited source and looks for the quote.
//
// skipSources names URLs whose text came from the search provider rather than a
// fetch. Re-fetching those would run a different extractor over the same page
// and disagree on whitespace and boilerplate, producing mismatches that say
// nothing about the claim.
func VerifyCitations(ctx context.Context, claims []*core.Claim, skipSources map[string]bool, r SourceReader) CitationReport {
	var rep CitationReport

	// One read per source, not per claim: a page cited by eight claims should
	// cost one fetch.
	texts := map[string]string{}
	failed := map[string]error{}

	for _, c := range claims {
		if ctx.Err() != nil {
			break
		}
		if skipSources[c.Source] {
			rep.Skipped++
			continue
		}

		text, ok := texts[c.Source]
		if !ok {
			if err, seen := failed[c.Source]; seen {
				rep.Unreachable++
				rep.addProblem(CitationUnreachable, c, err.Error())
				continue
			}
			var err error
			text, err = r.Text(ctx, c.Source)
			if err != nil {
				failed[c.Source] = err
				rep.Unreachable++
				rep.addProblem(CitationUnreachable, c, err.Error())
				continue
			}
			texts[c.Source] = text
		}

		// The same matcher the actor used. A different one would disagree on
		// whitespace and report mismatches that are not.
		match, found := actors.FindQuote(text, c.Quote)
		switch {
		case !found:
			rep.Mismatch++
			rep.addProblem(CitationMismatch, c,
				fmt.Sprintf("quote absent from %d bytes of source text", len(text)))
		case int64(match.Offset) != c.QuoteOffset:
			rep.OffsetDrift++
			rep.addProblem(CitationOffsetDrift, c,
				fmt.Sprintf("quote found at %d, claim records %d", match.Offset, c.QuoteOffset))
		default:
			rep.Verified++
		}
	}
	return rep
}

func (r *CitationReport) addProblem(v Verdict, c *core.Claim, detail string) {
	// Bounded: a systematically broken run would otherwise print thousands of
	// identical lines and bury the count.
	if len(r.Problems) >= 20 {
		return
	}
	r.Problems = append(r.Problems, CitationProblem{
		Verdict: v, Source: c.Source, Claim: truncate(c.Text, 70), Detail: detail,
	})
}

// citationAccuracy turns the report into a scorecard line.
// CitationAccuracyFor and CitationOffsetDriftFor are exported for the tests that
// pin the two apart: the distinction between "the source never said it" and "the
// offset moved" is the whole content of this pair, and it was wrong once.
func CitationAccuracyFor(rep CitationReport) Metric { return citationAccuracy(rep) }

// CitationOffsetDriftFor is the precision half. See CitationAccuracyFor.
func CitationOffsetDriftFor(rep CitationReport) Metric { return citationOffsetDrift(rep) }

func citationAccuracy(rep CitationReport) Metric {
	m := Metric{Name: "citation accuracy", Status: Measured, Unit: "%"}

	if rep.Checked() == 0 {
		m.Status = NotApplicable
		m.Detail = fmt.Sprintf("nothing verifiable: %d unreachable, %d supplied by the search provider",
			rep.Unreachable, rep.Skipped)
		return m
	}

	// Drift counts as ACCURATE, and the arithmetic used to say otherwise while the
	// verdict's own doc comment said "the citation is sound". A live academic run
	// scored 0.0% with every one of its eight quotes present in the source it
	// cited — they were simply at different offsets, because the claim was mined
	// from an abstract the API supplied and the re-read fetches the page. Reporting
	// that as zero conflates "this source never said it", which is the fabrication
	// this metric exists to catch, with "the byte offset moved", which is a
	// provenance-precision problem and has its own line below.
	found := rep.Verified + rep.OffsetDrift
	m.Value = 100 * float64(found) / float64(rep.Checked())
	m.Detail = fmt.Sprintf("%d of %d quotes found in the source they cite",
		found, rep.Checked())
	if rep.OffsetDrift > 0 {
		m.Detail += fmt.Sprintf(" · %d at a different offset (see citation offset drift)",
			rep.OffsetDrift)
	}
	if rep.Unreachable > 0 || rep.Skipped > 0 {
		m.Detail += fmt.Sprintf(" · %d unreachable, %d provider-supplied (excluded)",
			rep.Unreachable, rep.Skipped)
	}

	// A quote that is not in the source it cites is a citation pointing at a
	// page that never said it. That is the failure this whole pipeline is built
	// to prevent, so it fails the build rather than lowering a score.
	if rep.Mismatch > 0 {
		m.Regression = true
	}
	return m
}

// citationOffsetDrift is the precision half, reported separately so raising one
// number cannot hide the other.
//
// Not a regression. Drift is structural on some paths rather than a fault: an
// academic claim is mined from the abstract a provider returned and re-read from
// the publisher's page, so the same sentence sits at a different byte offset by
// construction. It still matters — a later re-verification that trusts the offset
// reads the wrong span — so it is measured rather than folded away.
func citationOffsetDrift(rep CitationReport) Metric {
	m := Metric{Name: "citation offset drift", Status: Measured, Unit: "%"}
	if rep.Checked() == 0 {
		m.Status = NotApplicable
		m.Detail = "nothing verifiable"
		return m
	}
	m.Value = 100 * float64(rep.OffsetDrift) / float64(rep.Checked())
	m.Detail = fmt.Sprintf("%d of %d quotes sit at a different offset than the claim "+
		"records; the citation is sound and the stored span is not",
		rep.OffsetDrift, rep.Checked())
	return m
}

// ---------------------------------------------------------------------------
// Reading a source back
// ---------------------------------------------------------------------------

// PipelineReader re-reads a source through the same fetch and extract path the
// actor used.
//
// Using the same components is the point: a different extractor would produce
// different whitespace and boilerplate, and every difference becomes a mismatch
// that is an artefact of the harness rather than a fault in the claim.
type PipelineReader struct {
	Fetch   fetch.Fetcher
	Extract extract.Extractor
}

func NewPipelineReader(f fetch.Fetcher, e extract.Extractor) *PipelineReader {
	return &PipelineReader{Fetch: f, Extract: e}
}

func (p *PipelineReader) Text(ctx context.Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("unparseable source: %w", err)
	}

	res, err := p.Fetch.Fetch(ctx, rawURL)
	if err != nil {
		return "", err
	}
	if !res.Outcome.Usable() {
		return "", fmt.Errorf("re-fetch returned %s", res.Outcome)
	}

	doc, err := p.Extract.Extract(ctx, res.Content, res.ContentType, u)
	if err != nil {
		return "", err
	}
	return doc.Text, nil
}

// StoredReader answers from the source text mole kept, and falls back to a
// re-fetch when it has none.
//
// Toolkit mode stores the document a quote was verified against — it has to, since
// verifying against text the caller supplied proves nothing — and the citation
// metric was re-fetching anyway. That made the measurement worse in exactly the
// case the store exists for: a page that has changed or 404'd since reports
// "unreachable" or an offset drift, while mole is holding the bytes the claim was
// checked against on disk.
//
// The fallback matters. An autonomous session stores no documents, and a toolkit
// session's text expires after seven days, so this has to degrade to the network
// rather than to "unverifiable".
type StoredReader struct {
	Store     store.Store
	SessionID string
	// Fallback reads a source the store does not have. Nil means such a source is
	// simply unreadable, which is the honest answer when there is no network path
	// configured.
	Fallback SourceReader

	once  sync.Once
	byURL map[string]string
	err   error
}

func NewStoredReader(st store.Store, sessionID string, fallback SourceReader) *StoredReader {
	return &StoredReader{Store: st, SessionID: sessionID, Fallback: fallback}
}

func (r *StoredReader) Text(ctx context.Context, rawURL string) (string, error) {
	r.once.Do(func() {
		r.byURL = map[string]string{}
		r.err = r.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
			docs, err := q.DocumentsForSession(ctx, r.SessionID, time.Now().UTC())
			if err != nil {
				return err
			}
			for _, d := range docs {
				// First write wins: a URL fetched twice in one session has two
				// rows, and the earlier one is the text the earlier claims were
				// checked against.
				if _, seen := r.byURL[d.URL]; !seen {
					r.byURL[d.URL] = d.Text
				}
			}
			return nil
		})
	})
	if r.err != nil {
		return "", r.err
	}
	if text, ok := r.byURL[rawURL]; ok {
		return text, nil
	}
	if r.Fallback == nil {
		return "", fmt.Errorf("no stored copy of %s and no reader configured", rawURL)
	}
	return r.Fallback.Text(ctx, rawURL)
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// sortedProblems gives the report a stable order, so a diff between two runs
// shows what changed rather than how a map iterated.
func sortedProblems(ps []CitationProblem) []CitationProblem {
	out := append([]CitationProblem(nil), ps...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Verdict != out[j].Verdict {
			return out[i].Verdict < out[j].Verdict
		}
		return out[i].Source < out[j].Source
	})
	return out
}
