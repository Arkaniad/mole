package core

import "time"

// Document is source text mole fetched and kept, so a quote can be checked
// against it later (toolkit mode).
//
// mole discards source text everywhere else. This exists for one reason: when an
// agent's model mines a claim and asks mole to record it, the quote has to be
// verified against text MOLE fetched. Verifying against text the agent supplied
// would prove nothing, since a model that invents a quote can invent the passage.
type Document struct {
	ID        string
	SessionID string

	URL   string
	Title string
	// Text is exactly what the caller was handed. It must not be re-normalised
	// after storage: a stored offset that no longer locates its quote is
	// provenance that lies.
	Text string
	// Truncated reports that the extractor cut the page short, so a quote from
	// beyond the cut will fail verification for a reason that is not the caller's
	// fault.
	Truncated bool

	// PublishedAt is when the source says it was published, when it says at all.
	// Zero means unstated, which is the common case and is not the same fact as
	// "published at the epoch" — the staleness rule needs a date on both sides
	// before it will rewrite anything.
	PublishedAt time.Time

	FetchedAt time.Time
	ExpiresAt time.Time
}

// DefaultDocumentTTL bounds how long source text is kept.
//
// Seven days. Long enough that a research session spread over a working week can
// still verify its own claims, short enough that a machine does not silently
// accumulate a corpus of other people's pages. Sessions are usually deleted long
// before this; the TTL is for the ones nobody deletes.
const DefaultDocumentTTL = 7 * 24 * time.Hour

// Expired reports whether a document is past its TTL at the given instant.
func (d Document) Expired(now time.Time) bool { return !now.Before(d.ExpiresAt) }
