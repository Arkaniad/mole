// Package core holds the domain types shared by every other package.
//
// Two conventions matter here and are load-bearing everywhere else:
//
//   - Money is integer micro-dollars (int64), never float64. Budget adherence is
//     a tested metric (max overshoot must be ~0); float accumulation over
//     thousands of reserve/settle operations drifts, so the architecture sketch's
//     `float64` USD fields are represented as `USDMicros` throughout.
//   - A session's budget, spend, holds, and escrow are all expressed in the
//     session's own BudgetUnit, so they are directly comparable int64s.
package core

import (
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

type BudgetUnit string

const (
	BudgetUSD    BudgetUnit = "usd"
	BudgetTokens BudgetUnit = "tokens"
)

func (u BudgetUnit) Valid() bool { return u == BudgetUSD || u == BudgetTokens }

type Mode string

const (
	ModeReport  Mode = "report"
	ModeDataset Mode = "dataset"
	ModeChain   Mode = "chain"
	ModeAsk     Mode = "ask"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeReport, ModeDataset, ModeChain, ModeAsk:
		return true
	}
	return false
}

type ActorType string

const (
	ActorWeb          ActorType = "web"
	ActorAcademic     ActorType = "academic"
	ActorLocalCompute ActorType = "local_compute"
)

func (a ActorType) Valid() bool {
	switch a {
	case ActorWeb, ActorAcademic, ActorLocalCompute:
		return true
	}
	return false
}

type SessionStatus string

const (
	StatusRunning   SessionStatus = "running"
	StatusDone      SessionStatus = "done"
	StatusExhausted SessionStatus = "budget_exhausted"
	StatusCancelled SessionStatus = "cancelled"
	StatusFailed    SessionStatus = "failed"
)

func (s SessionStatus) Valid() bool {
	switch s {
	case StatusRunning, StatusDone, StatusExhausted, StatusCancelled, StatusFailed:
		return true
	}
	return false
}

// Terminal reports whether the session can no longer spend.
func (s SessionStatus) Terminal() bool { return s != StatusRunning }

type LeadStatus string

const (
	LeadQueued       LeadStatus = "queued"
	LeadLeased       LeadStatus = "leased"
	LeadDone         LeadStatus = "done"
	LeadFailed       LeadStatus = "failed"
	LeadSkippedCache LeadStatus = "skipped_cached"
)

// Role records which stage of the loop spent a tool call. It is what makes
// "how much went to verification vs. execution" a GROUP BY rather than an
// archaeology project.
type Role string

const (
	RolePlanner  Role = "planner"
	RoleExecutor Role = "executor"
	RoleVerifier Role = "verifier"
	RoleOutput   Role = "output"
)

func (r Role) Valid() bool {
	switch r {
	case RolePlanner, RoleExecutor, RoleVerifier, RoleOutput:
		return true
	}
	return false
}

type CallType string

const (
	CallSearch    CallType = "search"
	CallFetch     CallType = "fetch"
	CallLLM       CallType = "llm"
	CallLocalQry  CallType = "local_query"
	CallAcademic  CallType = "academic_query"
	CallSandbox   CallType = "sandbox"
	CallEmbedding CallType = "embedding"
)

type ReservationStatus string

const (
	ReservationHeld     ReservationStatus = "held"
	ReservationSettled  ReservationStatus = "settled"
	ReservationReleased ReservationStatus = "released"
)

type EdgeKind string

const (
	EdgeSupports    EdgeKind = "supports"
	EdgeContradicts EdgeKind = "contradicts"
	EdgeDuplicateOf EdgeKind = "duplicate_of"
	EdgeSupersedes  EdgeKind = "supersedes"
	EdgeRefines     EdgeKind = "refines"
)

// ---------------------------------------------------------------------------
// Session
// ---------------------------------------------------------------------------

// Session is one research task.
//
// Spent, Held, and Escrow are NOT independent state. Spent is a materialized
// sum of the tool_calls ledger, written in the same transaction as each cost
// row, so it can always be recomputed after a crash (see budget.Ledger.Verify).
type Session struct {
	ID         string
	Prompt     string
	Mode       Mode
	ActorTypes []ActorType

	BudgetUnit BudgetUnit
	Budget     int64 // micro-dollars, or tokens, per BudgetUnit
	Spent      int64 // materialized sum of the ledger
	Held       int64 // sum of outstanding reservations
	Escrow     int64 // held back for output + final verify

	// Unit-independent ceilings. Without these, token mode has a hole: search
	// and fetch calls cost no tokens, so an unbounded fetch loop would be free.
	MaxToolCalls  int64
	MaxLeads      int64
	MaxWallClock  time.Duration
	ToolCallCount int64
	LeadCount     int64

	Status    SessionStatus
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Available is the spendable balance: everything not already spent, held by an
// outstanding reservation, or escrowed for output.
func (s *Session) Available() int64 {
	a := s.Budget - s.Spent - s.Held - s.Escrow
	if a < 0 {
		return 0
	}
	return a
}

// HitCeiling reports whether any unit-independent limit has been reached.
func (s *Session) HitCeiling(now time.Time) (bool, string) {
	if s.MaxToolCalls > 0 && s.ToolCallCount >= s.MaxToolCalls {
		return true, "max_tool_calls"
	}
	if s.MaxLeads > 0 && s.LeadCount >= s.MaxLeads {
		return true, "max_leads"
	}
	if s.MaxWallClock > 0 && now.Sub(s.CreatedAt) >= s.MaxWallClock {
		return true, "max_wallclock"
	}
	return false, ""
}

// RemainingFraction is how much of the session's allowance is left, in [0,1].
//
// HitCeiling's continuous twin, and it takes the minimum over the same limits
// for the same reason HitCeiling checks all of them: a session two minutes from
// max_wallclock with 90% of its dollars unspent has 10% left, not 90%. Reporting
// spend alone would promise room that cannot be used.
//
// Exists because §9.1's replan prompt asks the planner to judge whether the open
// sub-questions are "worth more budget" while the digest reported nothing about
// budget — an instruction the planner had no way to answer.
//
// Available() rather than Budget-Spent, so escrow is excluded: tokens held back
// for the report are not spendable on more research, and counting them would
// overstate what is left at exactly the moment the answer matters.
func (s *Session) RemainingFraction(now time.Time) float64 {
	frac := 1.0
	if s.Budget > 0 {
		frac = min(frac, float64(s.Available())/float64(s.Budget))
	}
	if s.MaxToolCalls > 0 {
		frac = min(frac, float64(s.MaxToolCalls-s.ToolCallCount)/float64(s.MaxToolCalls))
	}
	if s.MaxLeads > 0 {
		frac = min(frac, float64(s.MaxLeads-s.LeadCount)/float64(s.MaxLeads))
	}
	if s.MaxWallClock > 0 {
		frac = min(frac, float64(s.MaxWallClock-now.Sub(s.CreatedAt))/float64(s.MaxWallClock))
	}
	return max(0, frac)
}

func (s *Session) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("session: empty id")
	}
	if !s.Mode.Valid() {
		return fmt.Errorf("session: invalid mode %q", s.Mode)
	}
	if !s.BudgetUnit.Valid() {
		return fmt.Errorf("session: invalid budget unit %q", s.BudgetUnit)
	}
	if !s.Status.Valid() {
		return fmt.Errorf("session: invalid status %q", s.Status)
	}
	if s.Budget < 0 {
		return fmt.Errorf("session: negative budget")
	}
	if len(s.ActorTypes) == 0 {
		return fmt.Errorf("session: no actor types")
	}
	for _, a := range s.ActorTypes {
		if !a.Valid() {
			return fmt.Errorf("session: invalid actor type %q", a)
		}
	}
	return nil
}

// EncodeActorTypes / DecodeActorTypes keep the list in one TEXT column rather
// than a join table; the set is tiny and never queried by membership.
func EncodeActorTypes(as []ActorType) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = string(a)
	}
	return strings.Join(parts, ",")
}

func DecodeActorTypes(s string) []ActorType {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]ActorType, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, ActorType(p))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Lead
// ---------------------------------------------------------------------------

// Lead is a unit of work. Leases (not a bare status column) are what make the
// daemon crash-recoverable: a worker takes a lease and heartbeats it, and a
// boot-time sweep requeues leads whose lease expired.
type Lead struct {
	ID        string
	SessionID string
	ActorType ActorType
	Query     string
	ParentID  *string
	Depth     int
	Priority  int
	Status    LeadStatus

	LeaseOwner   *string
	LeaseExpires *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ---------------------------------------------------------------------------
// Claim + edges
// ---------------------------------------------------------------------------

// Claim is an atomic extracted fact.
//
// Quote is required at extraction time and checked verbatim against the
// extracted text before the claim is accepted. That check is cheap,
// deterministic, and rejects fabricated citations at the actor boundary rather
// than letting the verifier discover them later.
type Claim struct {
	ID        string
	SessionID string
	LeadID    string
	Text      string

	Source      string // URL, DOI, or "connector:<name>#<query_hash>"
	ToolCallID  string
	Quote       string
	QuoteOffset int64

	PublishedAt *time.Time
	RetrievedAt time.Time

	// Verification lineage. A per-row recheck counter cannot work: a follow-up
	// lead produces a NEW claim, which would start the counter over. Depth is
	// inherited and incremented so the cap actually binds.
	RootClaimID string
	VerifyDepth int

	Confidence float64
	Grounded   *bool

	CreatedAt time.Time
}

type ClaimEdge struct {
	ID        string
	SessionID string
	FromID    string
	ToID      string
	Kind      EdgeKind
	Weight    float64
	CreatedBy string
	Rationale string
	CreatedAt time.Time
}

// ---------------------------------------------------------------------------
// ToolCall
// ---------------------------------------------------------------------------

// ToolCall is one row of the append-only cost ledger. The ledger is the source
// of truth for spend; Session.Spent is a materialized sum of it.
type ToolCall struct {
	ID         string
	SessionID  string
	LeadID     *string
	Role       Role
	Type       CallType
	Model      string
	Input      string // redacted or hashed for local queries
	Cost       Cost
	Err        string
	DurationMS int64
	CreatedAt  time.Time
}

func (t *ToolCall) Validate() error {
	if t.SessionID == "" {
		return fmt.Errorf("toolcall: empty session id")
	}
	if !t.Role.Valid() {
		return fmt.Errorf("toolcall: invalid role %q", t.Role)
	}
	if t.Type == "" {
		return fmt.Errorf("toolcall: empty type")
	}
	return t.Cost.Validate()
}

// ---------------------------------------------------------------------------
// Reservation
// ---------------------------------------------------------------------------

// Reservation is a hold placed on budget before dispatch. Charging after the
// fact lets a pool of N workers overshoot the ceiling by N lead-costs; holding
// first bounds the overshoot to estimate error on a single lead.
type Reservation struct {
	ID         string
	SessionID  string
	LeadID     *string
	Amount     int64
	Status     ReservationStatus
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ResolvedAt *time.Time
}

// ---------------------------------------------------------------------------
// Span
// ---------------------------------------------------------------------------

// Span is one trace record. One span per lead is the M0 requirement; the type
// is general enough for nested spans inside an actor run.
type Span struct {
	ID        string
	SessionID string
	LeadID    *string
	ParentID  *string
	Name      string
	StartedAt time.Time
	EndedAt   *time.Time
	Status    string
	Attrs     map[string]string
}
