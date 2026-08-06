// Package mcpserver exposes the daemon's research surface over MCP (§5.1).
//
// Separate from internal/daemon so that package stays transport-only: the socket
// and its permissions are one concern, what a caller can ask for is another, and
// this one is testable over an in-memory transport with no socket at all.
//
// The tools are asynchronous by design (§5.3). research.report returns a session
// id and nothing else; the work continues in the daemon long after the calling
// agent's context is gone, and the caller polls research.status the way it would
// wait on a CI job.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/lajosdeme/mole/internal/store"
)

// MaxClaimsReturned caps how many claims research.result hands back.
//
// The result goes straight into a coding agent's context window, and nothing
// upstream bounds how many claims a session gathers. A cap the caller can see —
// reported as Truncated with the real Total — beats both silently sending five
// thousand and silently sending two hundred of them.
const MaxClaimsReturned = 200

// MaxEdgesReturned caps the edges alongside them, for the same reason.
const MaxEdgesReturned = 500

// DefaultSessionsListed is the page size for research.sessions.list.
const DefaultSessionsListed = 20

// Deps is what the tools operate on.
type Deps struct {
	// Supervisor starts and cancels sessions.
	Supervisor *session.Supervisor
	// Store answers every read. Deliberately not the supervisor: the store is
	// authoritative and survives a restart, while the supervisor only remembers
	// the sessions it is running plus a bounded tail of finished ones. A status
	// query that worked before a daemon restart and failed after would be the
	// worst possible behaviour for a caller told to poll.
	Store store.Store

	// MaxSessionUSD caps a single session's budget in micro-dollars. Zero means
	// no ceiling. See config.MaxSessionUSD for why this exists.
	MaxSessionUSD int64

	// Defaults applied when a call does not specify.
	MaxSources int
	MaxDepth   int
	MaxLeads   int
	Timeout    time.Duration

	Log *slog.Logger
}

// New builds the MCP server with the tool surface attached.
//
// One server for the whole daemon, connected to each accepted connection
// separately. The tools close over shared state, so everything they touch is
// concurrent by construction.
func New(d Deps) *mcp.Server {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "mole",
		Title:   "mole — deep research",
		Version: version(),
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "research.report",
		Description: "Start a research session and return its id immediately. The work " +
			"continues in the daemon; poll research.status and then research.result. " +
			"Budget is required and is spent on real API calls.",
	}, d.report)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "research.status",
		Description: "How a session is progressing: status, spend against its ceiling, " +
			"and what it has found so far.",
	}, d.status)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "research.result",
		Description: "The finished answer for a session: the report, plus the claims and " +
			"graph edges behind it. Each claim carries its source, the verbatim quote " +
			"supporting it, and a derived confidence — weigh them rather than taking the " +
			"prose alone.",
	}, d.result)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "research.cancel",
		Description: "Stop a running session. Budget already spent is not refunded, but " +
			"nothing further is spent: the remaining research, the grounding pass and the " +
			"report synthesis are all skipped.",
	}, d.cancel)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "research.sessions.list",
		Description: "Recent sessions with their status and spend. Use it to find a " +
			"session id again after your own context has been compacted.",
	}, d.listSessions)

	return srv
}

// ---------------------------------------------------------------------------
// research.report
// ---------------------------------------------------------------------------

// Budget is the amount a session may spend. Both fields are required: §8 makes
// the unit semantically load-bearing, and a bare number is ambiguous between
// dollars and tokens.
type Budget struct {
	Unit   string `json:"unit" jsonschema:"the budget unit: usd or tokens"`
	Amount string `json:"amount" jsonschema:"how much to spend: dollars like 2.50 for usd, a whole number of tokens for tokens"`
}

type ReportIn struct {
	Prompt string `json:"prompt" jsonschema:"the research question"`
	Budget Budget `json:"budget" jsonschema:"what this session may spend"`

	MaxSources int `json:"max_sources,omitempty" jsonschema:"sources to read per lead; omit for the daemon default"`
	MaxDepth   int `json:"max_depth,omitempty" jsonschema:"rounds of follow-up questions the planner may add; omit for the daemon default"`
}

type ReportOut struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Unit      string `json:"budget_unit"`
	Budget    int64  `json:"budget"`
	Note      string `json:"note,omitempty"`
}

func (d Deps) report(ctx context.Context, _ *mcp.CallToolRequest, in ReportIn) (*mcp.CallToolResult, ReportOut, error) {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, ReportOut{}, errors.New("prompt is empty")
	}

	unit, amount, err := parseBudget(in.Budget)
	if err != nil {
		return nil, ReportOut{}, err
	}
	// The daemon's ceiling, not the caller's. Checked before the session row is
	// written so a refused request costs nothing and leaves nothing behind.
	if unit == core.BudgetUSD && d.MaxSessionUSD > 0 && amount > d.MaxSessionUSD {
		return nil, ReportOut{}, fmt.Errorf(
			"budget %s is over this daemon's per-session ceiling of %s "+
				"(raise it with: mole config set daemon.max-session-usd)",
			core.FormatUSD(amount), core.FormatUSD(d.MaxSessionUSD))
	}

	spec := session.Spec{
		Question:   prompt,
		Mode:       core.ModeReport,
		BudgetUnit: unit,
		Budget:     amount,
		MaxSources: orDefault(in.MaxSources, d.MaxSources),
		MaxDepth:   orDefault(in.MaxDepth, d.MaxDepth),
		MaxLeads:   d.MaxLeads,
		Timeout:    d.Timeout,
	}

	// ctx is the MCP CALL's context and dies when this response is sent. Start
	// uses it only to create the session row; the run hangs off the supervisor's
	// own context, which is the whole reason the supervisor exists.
	sess, err := d.Supervisor.Start(ctx, spec)
	if err != nil {
		return nil, ReportOut{}, err
	}
	d.Log.Info("session started over MCP", "session", sess.ID, "budget", amount, "unit", unit)

	return nil, ReportOut{
		SessionID: sess.ID,
		Status:    string(sess.Status),
		Unit:      string(unit),
		Budget:    sess.Budget,
		Note: "Research is running. Poll research.status; when it reports done, " +
			"call research.result.",
	}, nil
}

// ---------------------------------------------------------------------------
// research.status
// ---------------------------------------------------------------------------

type SessionRef struct {
	SessionID string `json:"session_id" jsonschema:"the id returned by research.report"`
}

type StatusOut struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Prompt    string `json:"prompt"`

	Unit      string `json:"budget_unit"`
	Budget    int64  `json:"budget"`
	Spent     int64  `json:"spent"`
	Remaining int64  `json:"remaining"`

	Claims   int  `json:"claims"`
	Verified int  `json:"claims_verified"`
	Edges    int  `json:"edges"`
	Running  bool `json:"running"`
}

func (d Deps) status(ctx context.Context, _ *mcp.CallToolRequest, in SessionRef) (*mcp.CallToolResult, StatusOut, error) {
	sess, err := d.loadSession(ctx, in.SessionID)
	if err != nil {
		return nil, StatusOut{}, err
	}

	var claims []*core.Claim
	var edges []*core.ClaimEdge
	if err := d.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var rerr error
		if claims, rerr = q.ListClaims(ctx, sess.ID, 10_000); rerr != nil {
			return rerr
		}
		edges, rerr = q.ListEdges(ctx, sess.ID, 0)
		return rerr
	}); err != nil {
		return nil, StatusOut{}, err
	}

	verified := 0
	for _, c := range claims {
		if c != nil && c.VerifiedAt != nil {
			verified++
		}
	}

	return nil, StatusOut{
		SessionID: sess.ID,
		Status:    string(sess.Status),
		Prompt:    sess.Prompt,
		Unit:      string(sess.BudgetUnit),
		Budget:    sess.Budget,
		Spent:     sess.Spent,
		Remaining: sess.Available(),
		Claims:    len(claims),
		Verified:  verified,
		Edges:     len(edges),
		Running:   d.isRunning(sess.ID),
	}, nil
}

// ---------------------------------------------------------------------------
// research.result
// ---------------------------------------------------------------------------

// Claim is one finding, in the shape §5.3 says an agent caller wants: sourced,
// dated, quoted, and scored, rather than a paragraph.
type Claim struct {
	ID          string  `json:"id"`
	Text        string  `json:"text"`
	Source      string  `json:"source"`
	Quote       string  `json:"quote,omitempty"`
	PublishedAt string  `json:"published_at,omitempty"`
	Confidence  float64 `json:"confidence"`
	Grounded    *bool   `json:"grounded,omitempty"`
}

// Edge is one relation in the claim graph (§11.2).
type Edge struct {
	From      string  `json:"from"`
	To        string  `json:"to"`
	Kind      string  `json:"kind"`
	Weight    float64 `json:"weight,omitempty"`
	Rationale string  `json:"rationale,omitempty"`
}

type ResultOut struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	ReportMD  string `json:"report_md"`

	Claims []Claim `json:"claims"`
	Edges  []Edge  `json:"edges"`

	// TotalClaims and TotalEdges are what the session holds; the arrays above may
	// be shorter. Reported so a truncated result is visibly truncated — silently
	// returning a prefix is how a caller concludes a session found less than it
	// did.
	TotalClaims int  `json:"total_claims"`
	TotalEdges  int  `json:"total_edges"`
	Truncated   bool `json:"truncated"`

	Note string `json:"note,omitempty"`
}

func (d Deps) result(ctx context.Context, _ *mcp.CallToolRequest, in SessionRef) (*mcp.CallToolResult, ResultOut, error) {
	sess, err := d.loadSession(ctx, in.SessionID)
	if err != nil {
		return nil, ResultOut{}, err
	}

	var (
		claims []*core.Claim
		edges  []*core.ClaimEdge
		report string
	)
	if err := d.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var rerr error
		if claims, rerr = q.ListClaims(ctx, sess.ID, 10_000); rerr != nil {
			return rerr
		}
		edges, rerr = q.ListEdges(ctx, sess.ID, 0)
		return rerr
	}); err != nil {
		return nil, ResultOut{}, err
	}

	out := ResultOut{
		SessionID:   sess.ID,
		Status:      string(sess.Status),
		ReportMD:    report,
		TotalClaims: len(claims),
		TotalEdges:  len(edges),
	}
	if sess.Status == core.StatusRunning {
		out.Note = "This session is still running; the claims below are what it has " +
			"gathered so far and the report is not written yet."
	}

	for i, c := range claims {
		if c == nil {
			continue
		}
		if i >= MaxClaimsReturned {
			out.Truncated = true
			break
		}
		cl := Claim{
			ID:         c.ID,
			Text:       c.Text,
			Source:     c.Source,
			Quote:      c.Quote,
			Confidence: c.Confidence,
			Grounded:   c.Grounded,
		}
		if c.PublishedAt != nil {
			cl.PublishedAt = c.PublishedAt.UTC().Format(time.RFC3339)
		}
		out.Claims = append(out.Claims, cl)
	}
	for i, e := range edges {
		if e == nil {
			continue
		}
		if i >= MaxEdgesReturned {
			out.Truncated = true
			break
		}
		out.Edges = append(out.Edges, Edge{
			From: e.FromID, To: e.ToID, Kind: string(e.Kind),
			Weight: e.Weight, Rationale: e.Rationale,
		})
	}
	if out.Truncated {
		out.Note = strings.TrimSpace(out.Note + fmt.Sprintf(
			" Showing %d of %d claims and %d of %d edges.",
			len(out.Claims), out.TotalClaims, len(out.Edges), out.TotalEdges))
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// research.cancel
// ---------------------------------------------------------------------------

type CancelOut struct {
	SessionID string `json:"session_id"`
	Cancelled bool   `json:"cancelled"`
	Note      string `json:"note,omitempty"`
}

func (d Deps) cancel(ctx context.Context, _ *mcp.CallToolRequest, in SessionRef) (*mcp.CallToolResult, CancelOut, error) {
	// Confirm it exists before asking the supervisor, so "no such session" and
	// "that session already finished" are different answers. Only the store can
	// tell them apart.
	sess, err := d.loadSession(ctx, in.SessionID)
	if err != nil {
		return nil, CancelOut{}, err
	}

	if err := d.Supervisor.Cancel(sess.ID); err != nil {
		if errors.Is(err, session.ErrNoSuchSession) {
			return nil, CancelOut{
				SessionID: sess.ID,
				Cancelled: false,
				Note: fmt.Sprintf("this session is not running (status %s); nothing to cancel",
					sess.Status),
			}, nil
		}
		return nil, CancelOut{}, err
	}
	d.Log.Info("session cancelled over MCP", "session", sess.ID)

	return nil, CancelOut{
		SessionID: sess.ID,
		Cancelled: true,
		Note: "Stopping. Budget already spent is not refunded; nothing further is " +
			"spent. Poll research.status to see it settle.",
	}, nil
}

// ---------------------------------------------------------------------------
// research.sessions.list
// ---------------------------------------------------------------------------

type ListIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many recent sessions to return; omit for 20"`
}

type SessionSummary struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Prompt    string `json:"prompt"`
	Unit      string `json:"budget_unit"`
	Budget    int64  `json:"budget"`
	Spent     int64  `json:"spent"`
	Running   bool   `json:"running"`
}

type ListOut struct {
	Sessions []SessionSummary `json:"sessions"`
}

func (d Deps) listSessions(ctx context.Context, _ *mcp.CallToolRequest, in ListIn) (*mcp.CallToolResult, ListOut, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = DefaultSessionsListed
	}

	var sessions []*core.Session
	if err := d.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var rerr error
		sessions, rerr = q.ListSessions(ctx, limit)
		return rerr
	}); err != nil {
		return nil, ListOut{}, err
	}

	out := ListOut{Sessions: make([]SessionSummary, 0, len(sessions))}
	for _, s := range sessions {
		if s == nil {
			continue
		}
		out.Sessions = append(out.Sessions, SessionSummary{
			SessionID: s.ID,
			Status:    string(s.Status),
			Prompt:    s.Prompt,
			Unit:      string(s.BudgetUnit),
			Budget:    s.Budget,
			Spent:     s.Spent,
			Running:   d.isRunning(s.ID),
		})
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// Shared
// ---------------------------------------------------------------------------

func (d Deps) loadSession(ctx context.Context, id string) (*core.Session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("session_id is empty")
	}
	var sess *core.Session
	err := d.Store.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var rerr error
		sess, rerr = q.GetSession(ctx, id)
		return rerr
	})
	if err != nil {
		return nil, fmt.Errorf("no session %s: %w", id, err)
	}
	return sess, nil
}

func (d Deps) isRunning(id string) bool {
	if d.Supervisor == nil {
		return false
	}
	for _, r := range d.Supervisor.Running() {
		if r == id {
			return true
		}
	}
	return false
}

// parseBudget turns the wire form into a unit and an amount.
//
// The unit is required. §8 makes it semantically load-bearing and there is no
// bare-number form on the command line either, for the same reason: a caller
// that means tokens and gets dollars overspends by six orders of magnitude.
func parseBudget(b Budget) (core.BudgetUnit, int64, error) {
	unit := core.BudgetUnit(strings.ToLower(strings.TrimSpace(b.Unit)))
	amount := strings.TrimSpace(b.Amount)
	if amount == "" {
		return "", 0, errors.New("budget.amount is required")
	}

	switch unit {
	case core.BudgetUSD:
		v, err := core.ParseUSD(amount)
		if err != nil {
			return "", 0, fmt.Errorf("budget.amount %q is not a dollar amount: %w", amount, err)
		}
		if v <= 0 {
			return "", 0, errors.New("budget.amount must be positive")
		}
		return unit, v, nil
	case core.BudgetTokens:
		var v int64
		if _, err := fmt.Sscanf(amount, "%d", &v); err != nil {
			return "", 0, fmt.Errorf("budget.amount %q is not a whole number of tokens", amount)
		}
		if v <= 0 {
			return "", 0, errors.New("budget.amount must be positive")
		}
		return unit, v, nil
	case "":
		return "", 0, errors.New("budget.unit is required: usd or tokens")
	default:
		return "", 0, fmt.Errorf("budget.unit %q is not usd or tokens", b.Unit)
	}
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func version() string { return "0.1.0" }
