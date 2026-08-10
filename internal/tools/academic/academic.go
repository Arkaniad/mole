// Package academic is the provider boundary for scholarly sources (§10.2).
//
// arXiv and PubMed/PMC are the keyless defaults; Unpaywall turns a DOI into a
// legal open-access copy, which §10.2 calls the single highest-leverage call in
// the package. Semantic Scholar and OpenAlex are deliberately absent — they
// serve citation-graph queries, which is a different capability from finding
// papers about a question. So is ScienceDirect/Elsevier, for a harder reason:
// full text there needs an institutional entitlement the API does not grant.
//
// The API surface is the easy part. §10.3 is the milestone: every provider is
// rate-limited and identifies itself, because a worker pool without that gets
// the user banned in week one. Both are enforced here rather than left to each
// implementation, so a new provider cannot forget.
package academic

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// Kind names a provider.
type Kind string

const (
	KindArXiv     Kind = "arxiv"
	KindPubMed    Kind = "pubmed"
	KindUnpaywall Kind = "unpaywall"
)

func Kinds() []Kind { return []Kind{KindArXiv, KindPubMed, KindUnpaywall} }

func (k Kind) Valid() bool {
	switch k {
	case KindArXiv, KindPubMed, KindUnpaywall:
		return true
	}
	return false
}

// ErrNoContact is returned when a provider is built without a contact address.
//
// A sentinel rather than a string because §10.3 makes this a startup check
// rather than a README line, and a caller has to be able to tell "you have not
// configured this yet" from "the provider is down".
var ErrNoContact = errors.New("academic: no contact email configured")

// ErrRateLimited is a 429 or a provider's documented throttle response. Named to
// match search.ErrRateLimited so the executor's §9.5 policy classifies both the
// same way — transient, retry, never abort.
var ErrRateLimited = errors.New("academic: rate limited")

// LimitFor is §10.3's table, in code.
//
// The numbers are the providers' own published constraints, not guesses, and
// they are conservative where the provider is vague. Getting these wrong is not
// a performance question: these are shared public services run by libraries and
// government agencies, and the failure mode is a block on the user's address.
//
// The keys are prefixed rather than bare, because this limiter is shared with
// the web fetcher, which keys by hostname — an unprefixed "arxiv" would be a
// different bucket from the "arxiv.org" the fetcher uses, and a prefixed one
// cannot silently collide with either.
func LimitFor(k Kind) limiter.Limit {
	switch k {
	case KindArXiv:
		// arXiv's Terms of Use ask for no more than one request every three
		// seconds and a single connection at a time. Burst 1 is the "single
		// connection" part: a burst of 2 is two simultaneous requests, which is
		// the thing they ask callers not to do.
		return limiter.Limit{Rate: 1.0 / 3.0, Burst: 1, MinInterval: 3 * time.Second}

	case KindPubMed:
		// NCBI E-utilities: 3 requests/second without an API key, 10 with one.
		// The keyless figure is used because mole ships keyless — a key is an
		// optimization, and encoding the higher number here would mean the
		// default configuration exceeds the limit it was written for.
		return limiter.Limit{Rate: 3, Burst: 3}

	case KindUnpaywall:
		// Unpaywall asks for an email on every call and publishes a daily
		// allowance rather than a per-second one. Held to a modest rate because
		// there is no documented burst tolerance to rely on.
		return limiter.Limit{Rate: 5, Burst: 5}
	}
	return limiter.Unlimited
}

// LimiterKey is the bucket a provider's calls are charged to.
func LimiterKey(k Kind) string { return "academic:" + string(k) }

// Register installs every provider's limit on a limiter.
//
// Called once where the limiter is built, so the constraints are in force before
// any provider exists to violate them. Registering per provider construction
// would mean a provider built twice resets its own bucket — which is exactly the
// Crawl-delay bug M5's review found in the fetcher.
func Register(l *limiter.Limiter) {
	if l == nil {
		return
	}
	for _, k := range Kinds() {
		l.Set(LimiterKey(k), LimitFor(k))
	}
}

// Options bound one search.
type Options struct {
	// MaxResults caps papers returned. Zero takes the provider's default.
	MaxResults int
}

// Paper is one result, normalized across providers.
//
// The full-text locations are recorded from METADATA, not by fetching. Every
// provider knows which formats exist for a paper without downloading any of
// them, and that is what makes the escalation ladder affordable: the decision
// to read more than the abstract is taken on free information.
type Paper struct {
	Title    string
	Abstract string
	Authors  []string

	// Identifiers. A paper often has several; whichever the provider knows is
	// recorded, because Unpaywall keys on DOI and PMC keys on PMCID.
	DOI     string
	ArXivID string
	PMID    string
	PMCID   string

	// PublishedAt is the paper's date, which academic sources report exactly.
	//
	// Worth more here than anywhere else in mole: §11.2's staleness detection
	// needs publication dates on both sides of a contradiction to tell "these
	// disagree" from "this one is older", and web sources supply them rarely
	// and unreliably. This is the first place they arrive as fact.
	PublishedAt *time.Time

	// Where the text can be read, discovered from metadata.
	//
	// HTMLURL is the escalation target: arXiv's LaTeXML HTML and PMC's XML are
	// full text the existing extractor already handles. PDFURL is recorded but
	// not yet readable — mole has no PDF extraction, so a paper with only a PDF
	// is counted rather than parsed, which is what turns "do we need a PDF
	// library?" into a number (§10.4).
	HTMLURL    string
	PDFURL     string
	LandingURL string
	// OpenAccess is the provider's own verdict, not an inference from the URLs
	// above: a landing page exists for closed papers too.
	OpenAccess bool

	Source Kind
}

// FullTextFormat reports the best text mole can currently READ for a paper.
//
// Deliberately about mole's capability rather than the paper's availability,
// because the two are different questions and conflating them is how a PDF
// library gets built for a corpus that turns out to be mostly closed access.
type FullTextFormat string

const (
	// FormatHTML is full text mole can read today, through the existing fetcher
	// and extractor.
	FormatHTML FullTextFormat = "html"
	// FormatPDFOnly is open access mole cannot read yet. This is the only
	// bucket a PDF extractor would serve.
	FormatPDFOnly FullTextFormat = "pdf_only"
	// FormatClosed has no open-access copy at all. No amount of parsing helps;
	// the abstract is the whole of what can be used.
	FormatClosed FullTextFormat = "closed"
)

func (p Paper) FullTextFormat() FullTextFormat {
	switch {
	case strings.TrimSpace(p.HTMLURL) != "":
		return FormatHTML
	case strings.TrimSpace(p.PDFURL) != "":
		return FormatPDFOnly
	default:
		return FormatClosed
	}
}

// Response is what a search returned.
type Response struct {
	Query    string
	Provider Kind
	Papers   []Paper
	// Cost is reported so the ledger settles against the reservation that
	// covered this lead. These APIs are free, so it is normally zero — which
	// means a USD budget does not bind on them and §8.5's unit-independent
	// ceilings are what actually bound an academic session.
	Cost core.Cost
}

// Provider is one scholarly source.
type Provider interface {
	Search(ctx context.Context, query string, opts Options) (*Response, error)
	// Resolve looks a paper up by DOI. Unpaywall's whole purpose, and useful
	// from the others for turning a citation into a readable location.
	Resolve(ctx context.Context, doi string) (*Paper, error)
	Kind() Kind
}

// Config is what every provider needs.
type Config struct {
	Kind Kind
	// ContactEmail identifies the caller. NOT optional — see Validate.
	ContactEmail string
	// BaseURL overrides the provider's endpoint, for tests.
	BaseURL string
}

// Validate refuses a provider that cannot identify itself.
//
// §10.3: "Contact email is a required config value before any academic provider
// is enabled. Ship it as a startup check, not a README line." Unpaywall requires
// an email parameter and NCBI requires tool and email; sending requests without
// them is against both providers' documented terms, and the consequence lands on
// the user's address rather than on mole.
//
// Enforced at construction rather than at call time so a misconfigured install
// fails before it has made a single request.
func (c Config) Validate() error {
	if !c.Kind.Valid() {
		return fmt.Errorf("academic: unknown provider %q (want one of %v)", c.Kind, Kinds())
	}
	return CheckContact(c.ContactEmail)
}

// CheckContact reports whether an address is usable for provider identification.
//
// Exported so `mole doctor` reports readiness from the same rule that gates the
// providers. A second, hand-written check in the CLI would be free to drift, and
// the whole point of a startup check is that it agrees with the thing it gates.
func CheckContact(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return ErrNoContact
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || !strings.Contains(addr.Address, "@") {
		return fmt.Errorf("%w: %q is not a usable address", ErrNoContact, email)
	}
	return nil
}
