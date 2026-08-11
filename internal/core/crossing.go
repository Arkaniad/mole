package core

import "time"

// Crossing is one record of data leaving the user's machine (§12.1).
//
// §12.1: "Every crossing is logged, so a user can audit exactly what left their
// machine." A log line satisfies the letter of that and not the use: logs are
// rotated, are off by default at Info in some setups, and cannot be queried per
// session. A user asking "what did mole send about my sales data last Tuesday"
// needs a table.
//
// The rule that shapes every field: this record carries NO value from the data.
// It carries the statement, its hash, how much was described, how much was
// withheld, and the outcome. An audit trail that is another copy of the thing the
// user was worried about is worse than none — and the statement itself is safe to
// keep because §12.3 forbids the model from authoring one: a query can only be a
// hypothesis template filled with identifiers the profile already published.
type Crossing struct {
	ID        string
	SessionID string
	LeadID    string

	// Connector is the registered name, and Query/QueryHash identify the
	// statement. A local claim cites "connector:<name>#<hash>", so this is what
	// makes a claim traceable back to the question that produced it.
	Connector string
	Query     string
	QueryHash string

	Outcome CrossingOutcome
	// Detail is mole's own reason string — a refusal's text, never a value from
	// the result.
	Detail string

	RowsDescribed   int64
	Columns         int
	ColumnsWithheld int
	Buckets         int
	Suppressed      int
	BeyondTopK      int
	Tests           int
	Truncated       bool
	CreatedAt       time.Time
}

// CrossingOutcome is what happened to the envelope.
type CrossingOutcome string

const (
	// CrossingCrossed: an envelope was returned to a model.
	CrossingCrossed CrossingOutcome = "crossed"
	// CrossingRefused: the gate or the parse guard said no. Nothing crossed, and
	// this is the normal case rather than an error — the gate exists to refuse.
	CrossingRefused CrossingOutcome = "refused"
	// CrossingWithheld: the envelope was built and then held back because the
	// exfil check found a value in it (gate.ErrLeak).
	//
	// Its own outcome, not folded into refused, because they mean opposite things
	// about this code: a refusal is the design working, and a withholding is a
	// rule upstream having broken in a way the backstop caught. §14.3's metric is
	// the count of these, and it must be zero.
	CrossingWithheld CrossingOutcome = "withheld"
)
