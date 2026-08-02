package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sqlite: create %s: %w", dir, err)
	}
	return nil
}

// execer is satisfied by both *sql.DB and *sql.Tx, so one implementation
// serves the read pool and the write transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type queries struct{ q execer }

var (
	_ store.Queries = (*queries)(nil)
	_ store.Tx      = (*queries)(nil)
)

// ---------------------------------------------------------------------------
// Time helpers — timestamps are unix microseconds UTC.
// ---------------------------------------------------------------------------

func toMicros(t time.Time) int64 { return t.UTC().UnixMicro() }

func fromMicros(v int64) time.Time { return time.UnixMicro(v).UTC() }

func nullMicros(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toMicros(*t), Valid: true}
}

func micrasPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMicros(v.Int64)
	return &t
}

func nullStr(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func strPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

const sessionCols = `id, prompt, mode, actor_types, budget_unit, budget, spent, held, escrow,
	max_tool_calls, max_leads, max_wallclock_ns, tool_call_count, lead_count,
	status, created_at, updated_at`

func (t *queries) InsertSession(ctx context.Context, s *core.Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now

	_, err := t.q.ExecContext(ctx, `
		INSERT INTO sessions (`+sessionCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.ID, s.Prompt, string(s.Mode), core.EncodeActorTypes(s.ActorTypes),
		string(s.BudgetUnit), s.Budget, s.Spent, s.Held, s.Escrow,
		s.MaxToolCalls, s.MaxLeads, int64(s.MaxWallClock), s.ToolCallCount, s.LeadCount,
		string(s.Status), toMicros(s.CreatedAt), toMicros(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: insert session: %w", err)
	}
	return nil
}

func scanSession(sc interface{ Scan(...any) error }) (*core.Session, error) {
	var (
		s           core.Session
		mode        string
		actorTypes  string
		budgetUnit  string
		status      string
		wallclockNS int64
		created     int64
		updated     int64
	)
	err := sc.Scan(&s.ID, &s.Prompt, &mode, &actorTypes, &budgetUnit,
		&s.Budget, &s.Spent, &s.Held, &s.Escrow,
		&s.MaxToolCalls, &s.MaxLeads, &wallclockNS, &s.ToolCallCount, &s.LeadCount,
		&status, &created, &updated)
	if err != nil {
		return nil, err
	}
	s.Mode = core.Mode(mode)
	s.ActorTypes = core.DecodeActorTypes(actorTypes)
	s.BudgetUnit = core.BudgetUnit(budgetUnit)
	s.Status = core.SessionStatus(status)
	s.MaxWallClock = time.Duration(wallclockNS)
	s.CreatedAt = fromMicros(created)
	s.UpdatedAt = fromMicros(updated)
	return &s, nil
}

func (t *queries) GetSession(ctx context.Context, id string) (*core.Session, error) {
	row := t.q.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id)
	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get session: %w", err)
	}
	return s, nil
}

func (t *queries) ListSessions(ctx context.Context, limit int) ([]*core.Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := t.q.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer rows.Close()

	var out []*core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (t *queries) SetSessionStatus(ctx context.Context, id string, status core.SessionStatus) error {
	if !status.Valid() {
		return fmt.Errorf("sqlite: invalid status %q", status)
	}
	res, err := t.q.ExecContext(ctx,
		`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), toMicros(time.Now()), id)
	if err != nil {
		return fmt.Errorf("sqlite: set status: %w", err)
	}
	return requireOneRow(res, "session")
}

// ApplyBudgetDelta adjusts the session counters atomically.
//
// The WHERE clause carries the invariants rather than trusting the caller:
// spent, held, and escrow may never go negative. A settle that would drive held
// below zero — a double-settle, or a settle against a released reservation —
// matches no rows and surfaces as ErrConflict instead of silently corrupting
// the ledger.
func (t *queries) ApplyBudgetDelta(ctx context.Context, sessionID string, d store.BudgetDelta) error {
	res, err := t.q.ExecContext(ctx, `
		UPDATE sessions SET
			spent           = spent           + ?,
			held            = held            + ?,
			escrow          = escrow          + ?,
			tool_call_count = tool_call_count + ?,
			lead_count      = lead_count      + ?,
			updated_at      = ?
		WHERE id = ?
		  AND spent           + ? >= 0
		  AND held            + ? >= 0
		  AND escrow          + ? >= 0
		  AND tool_call_count + ? >= 0
		  AND lead_count      + ? >= 0`,
		d.Spent, d.Held, d.Escrow, d.ToolCallCount, d.LeadCount, toMicros(time.Now()), sessionID,
		d.Spent, d.Held, d.Escrow, d.ToolCallCount, d.LeadCount)
	if err != nil {
		return fmt.Errorf("sqlite: apply budget delta: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: budget delta %+v would violate a non-negative invariant on session %s",
			store.ErrConflict, d, sessionID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reservations
// ---------------------------------------------------------------------------

const reservationCols = `id, session_id, lead_id, amount, status, created_at, expires_at, resolved_at`

func (t *queries) InsertReservation(ctx context.Context, r *core.Reservation) error {
	if r.Amount <= 0 {
		return fmt.Errorf("sqlite: reservation amount must be positive, got %d", r.Amount)
	}
	_, err := t.q.ExecContext(ctx, `
		INSERT INTO reservations (`+reservationCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.SessionID, nullStr(r.LeadID), r.Amount, string(r.Status),
		toMicros(r.CreatedAt), toMicros(r.ExpiresAt), nullMicros(r.ResolvedAt))
	if err != nil {
		return fmt.Errorf("sqlite: insert reservation: %w", err)
	}
	return nil
}

func (t *queries) GetReservation(ctx context.Context, id string) (*core.Reservation, error) {
	var (
		r        core.Reservation
		leadID   sql.NullString
		status   string
		created  int64
		expires  int64
		resolved sql.NullInt64
	)
	err := t.q.QueryRowContext(ctx,
		`SELECT `+reservationCols+` FROM reservations WHERE id = ?`, id).
		Scan(&r.ID, &r.SessionID, &leadID, &r.Amount, &status, &created, &expires, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get reservation: %w", err)
	}
	r.LeadID = strPtr(leadID)
	r.Status = core.ReservationStatus(status)
	r.CreatedAt = fromMicros(created)
	r.ExpiresAt = fromMicros(expires)
	r.ResolvedAt = micrasPtr(resolved)
	return &r, nil
}

// ResolveReservation moves a held reservation to settled or released. The
// status guard in the WHERE clause makes the transition idempotent-safe: a
// second settle affects no rows and reports a conflict rather than
// double-releasing the hold.
func (t *queries) ResolveReservation(ctx context.Context, id string, status core.ReservationStatus, at time.Time) error {
	if status != core.ReservationSettled && status != core.ReservationReleased {
		return fmt.Errorf("sqlite: cannot resolve reservation to %q", status)
	}
	res, err := t.q.ExecContext(ctx,
		`UPDATE reservations SET status = ?, resolved_at = ? WHERE id = ? AND status = 'held'`,
		string(status), toMicros(at), id)
	if err != nil {
		return fmt.Errorf("sqlite: resolve reservation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: reservation %s is not held", store.ErrConflict, id)
	}
	return nil
}

func (t *queries) SumHeldReservations(ctx context.Context, sessionID string) (int64, error) {
	var total sql.NullInt64
	err := t.q.QueryRowContext(ctx,
		`SELECT SUM(amount) FROM reservations WHERE session_id = ? AND status = 'held'`, sessionID).
		Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("sqlite: sum held: %w", err)
	}
	return total.Int64, nil
}

// ExpireStaleReservations releases holds whose lease has passed. This is the
// reservation half of crash recovery: a worker that died mid-run leaves budget
// held forever otherwise.
func (t *queries) ExpireStaleReservations(ctx context.Context, now time.Time) (int, error) {
	rows, err := t.q.QueryContext(ctx,
		`SELECT id, session_id, amount FROM reservations
		 WHERE status = 'held' AND expires_at < ?`, toMicros(now))
	if err != nil {
		return 0, fmt.Errorf("sqlite: scan stale reservations: %w", err)
	}

	type stale struct {
		id, sessionID string
		amount        int64
	}
	var list []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.sessionID, &s.amount); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, s := range list {
		if err := t.ResolveReservation(ctx, s.id, core.ReservationReleased, now); err != nil {
			return 0, err
		}
		if err := t.ApplyBudgetDelta(ctx, s.sessionID, store.BudgetDelta{Held: -s.amount}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}

// ---------------------------------------------------------------------------
// Tool calls (the ledger)
// ---------------------------------------------------------------------------

const toolCallCols = `id, session_id, lead_id, role, type, model, input,
	usd_micros, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	err, duration_ms, created_at`

func (t *queries) InsertToolCall(ctx context.Context, tc *core.ToolCall) error {
	if err := tc.Validate(); err != nil {
		return err
	}
	if tc.ID == "" {
		tc.ID = core.NewToolCallID()
	}
	if tc.CreatedAt.IsZero() {
		tc.CreatedAt = time.Now().UTC()
	}
	_, err := t.q.ExecContext(ctx, `
		INSERT INTO tool_calls (`+toolCallCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		tc.ID, tc.SessionID, nullStr(tc.LeadID), string(tc.Role), string(tc.Type), tc.Model, tc.Input,
		tc.Cost.USDMicros, tc.Cost.InputTokens, tc.Cost.OutputTokens,
		tc.Cost.CacheReadTokens, tc.Cost.CacheWriteTokens,
		tc.Err, tc.DurationMS, toMicros(tc.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: insert tool call: %w", err)
	}
	return nil
}

const costSumExpr = `COALESCE(SUM(usd_micros),0), COALESCE(SUM(input_tokens),0),
	COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0),
	COALESCE(SUM(cache_write_tokens),0)`

func (t *queries) SumCosts(ctx context.Context, sessionID string) (core.Cost, error) {
	var c core.Cost
	err := t.q.QueryRowContext(ctx,
		`SELECT `+costSumExpr+` FROM tool_calls WHERE session_id = ?`, sessionID).
		Scan(&c.USDMicros, &c.InputTokens, &c.OutputTokens, &c.CacheReadTokens, &c.CacheWriteTokens)
	if err != nil {
		return c, fmt.Errorf("sqlite: sum costs: %w", err)
	}
	return c, nil
}

// SumCostsByRole answers "how much of this session went to verification vs.
// execution" as one GROUP BY. This is the entire reason ToolCall.Role exists.
func (t *queries) SumCostsByRole(ctx context.Context, sessionID string) (map[core.Role]core.Cost, error) {
	rows, err := t.q.QueryContext(ctx,
		`SELECT role, `+costSumExpr+` FROM tool_calls WHERE session_id = ? GROUP BY role`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: sum costs by role: %w", err)
	}
	defer rows.Close()

	out := map[core.Role]core.Cost{}
	for rows.Next() {
		var (
			role string
			c    core.Cost
		)
		if err := rows.Scan(&role, &c.USDMicros, &c.InputTokens, &c.OutputTokens,
			&c.CacheReadTokens, &c.CacheWriteTokens); err != nil {
			return nil, err
		}
		out[core.Role(role)] = c
	}
	return out, rows.Err()
}

func (t *queries) CountToolCalls(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := t.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tool_calls WHERE session_id = ?`, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count tool calls: %w", err)
	}
	return n, nil
}

func (t *queries) ListToolCalls(ctx context.Context, sessionID string, limit int) ([]*core.ToolCall, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := t.q.QueryContext(ctx,
		`SELECT `+toolCallCols+` FROM tool_calls WHERE session_id = ? ORDER BY created_at LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list tool calls: %w", err)
	}
	defer rows.Close()

	var out []*core.ToolCall
	for rows.Next() {
		var (
			tc      core.ToolCall
			leadID  sql.NullString
			role    string
			typ     string
			created int64
		)
		if err := rows.Scan(&tc.ID, &tc.SessionID, &leadID, &role, &typ, &tc.Model, &tc.Input,
			&tc.Cost.USDMicros, &tc.Cost.InputTokens, &tc.Cost.OutputTokens,
			&tc.Cost.CacheReadTokens, &tc.Cost.CacheWriteTokens,
			&tc.Err, &tc.DurationMS, &created); err != nil {
			return nil, err
		}
		tc.LeadID = strPtr(leadID)
		tc.Role = core.Role(role)
		tc.Type = core.CallType(typ)
		tc.CreatedAt = fromMicros(created)
		out = append(out, &tc)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Fetch outcomes (§10.4)
// ---------------------------------------------------------------------------

func (t *queries) RecordFetchOutcome(ctx context.Context, o *store.FetchOutcome) error {
	if o.Outcome == "" {
		return fmt.Errorf("sqlite: fetch outcome is empty")
	}
	if o.ID == "" {
		o.ID = core.NewSpanID()
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now().UTC()
	}
	_, err := t.q.ExecContext(ctx, `
		INSERT INTO fetch_outcomes
			(id, session_id, lead_id, url, domain, outcome, status_code, bytes, duration_ms, err, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, nullStr(o.SessionID), nullStr(o.LeadID), o.URL, o.Domain, o.Outcome,
		o.StatusCode, o.Bytes, o.Duration.Milliseconds(), o.Err, toMicros(o.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: record fetch outcome: %w", err)
	}
	return nil
}

// FetchOutcomeStats produces the outcome mix plus the ranked domains per cause.
//
// Both halves matter to §17.1: the rate says whether a capability gap is worth
// closing, and the domain list often says it is really three sites rather than
// an architecture problem.
func (t *queries) FetchOutcomeStats(ctx context.Context, since time.Time, topDomains int) ([]store.FetchStat, error) {
	rows, err := t.q.QueryContext(ctx, `
		SELECT outcome, COUNT(*) FROM fetch_outcomes
		WHERE created_at >= ?
		GROUP BY outcome ORDER BY COUNT(*) DESC`, toMicros(since))
	if err != nil {
		return nil, fmt.Errorf("sqlite: fetch outcome stats: %w", err)
	}

	var stats []store.FetchStat
	for rows.Next() {
		var s store.FetchStat
		if err := rows.Scan(&s.Outcome, &s.Count); err != nil {
			rows.Close()
			return nil, err
		}
		stats = append(stats, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if topDomains <= 0 {
		return stats, nil
	}

	for i := range stats {
		dRows, err := t.q.QueryContext(ctx, `
			SELECT domain, COUNT(*) FROM fetch_outcomes
			WHERE outcome = ? AND created_at >= ? AND domain != ''
			GROUP BY domain ORDER BY COUNT(*) DESC LIMIT ?`,
			stats[i].Outcome, toMicros(since), topDomains)
		if err != nil {
			return nil, fmt.Errorf("sqlite: fetch outcome domains: %w", err)
		}
		for dRows.Next() {
			var dc store.DomainCount
			if err := dRows.Scan(&dc.Domain, &dc.Count); err != nil {
				dRows.Close()
				return nil, err
			}
			stats[i].Domains = append(stats[i].Domains, dc)
		}
		dRows.Close()
		if err := dRows.Err(); err != nil {
			return nil, err
		}
	}
	return stats, nil
}

// ---------------------------------------------------------------------------
// Spans
// ---------------------------------------------------------------------------

func (t *queries) StartSpan(ctx context.Context, s *core.Span) error {
	if s.ID == "" {
		s.ID = core.NewSpanID()
	}
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now().UTC()
	}
	attrs := "{}"
	if len(s.Attrs) > 0 {
		b, err := json.Marshal(s.Attrs)
		if err != nil {
			return fmt.Errorf("sqlite: marshal span attrs: %w", err)
		}
		attrs = string(b)
	}
	_, err := t.q.ExecContext(ctx, `
		INSERT INTO spans (id, session_id, lead_id, parent_id, name, started_at, ended_at, status, attrs)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		s.ID, s.SessionID, nullStr(s.LeadID), nullStr(s.ParentID), s.Name,
		toMicros(s.StartedAt), nullMicros(s.EndedAt), s.Status, attrs)
	if err != nil {
		return fmt.Errorf("sqlite: insert span: %w", err)
	}
	return nil
}

func (t *queries) EndSpan(ctx context.Context, id string, endedAt time.Time, status string) error {
	res, err := t.q.ExecContext(ctx,
		`UPDATE spans SET ended_at = ?, status = ? WHERE id = ? AND ended_at IS NULL`,
		toMicros(endedAt), status, id)
	if err != nil {
		return fmt.Errorf("sqlite: end span: %w", err)
	}
	return requireOneRow(res, "span")
}

func (t *queries) ListSpans(ctx context.Context, sessionID string) ([]*core.Span, error) {
	rows, err := t.q.QueryContext(ctx, `
		SELECT id, session_id, lead_id, parent_id, name, started_at, ended_at, status, attrs
		FROM spans WHERE session_id = ? ORDER BY started_at`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list spans: %w", err)
	}
	defer rows.Close()

	var out []*core.Span
	for rows.Next() {
		var (
			s        core.Span
			leadID   sql.NullString
			parentID sql.NullString
			started  int64
			ended    sql.NullInt64
			attrs    string
		)
		if err := rows.Scan(&s.ID, &s.SessionID, &leadID, &parentID, &s.Name,
			&started, &ended, &s.Status, &attrs); err != nil {
			return nil, err
		}
		s.LeadID = strPtr(leadID)
		s.ParentID = strPtr(parentID)
		s.StartedAt = fromMicros(started)
		s.EndedAt = micrasPtr(ended)
		if attrs != "" && attrs != "{}" {
			if err := json.Unmarshal([]byte(attrs), &s.Attrs); err != nil {
				return nil, fmt.Errorf("sqlite: unmarshal span attrs: %w", err)
			}
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------

func requireOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, store.ErrNotFound)
	}
	return nil
}

// isUniqueViolation reports whether err is a UNIQUE/PRIMARY KEY conflict.
// The driver does not export a typed error for this, so the check is on the
// message — narrow enough to be safe, and it is only used to convert a
// duplicate insert into ErrConflict.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------------------------------------------------------------------------
// Leads
// ---------------------------------------------------------------------------

const leadCols = `id, session_id, actor_type, query, parent_id, depth, priority,
	status, lease_owner, lease_expires, created_at, updated_at`

func (t *queries) InsertLead(ctx context.Context, l *core.Lead) error {
	if l.ID == "" {
		l.ID = core.NewLeadID()
	}
	now := time.Now().UTC()
	if l.CreatedAt.IsZero() {
		l.CreatedAt = now
	}
	l.UpdatedAt = now
	if l.Status == "" {
		l.Status = core.LeadQueued
	}

	_, err := t.q.ExecContext(ctx, `
		INSERT INTO leads (`+leadCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.ID, l.SessionID, string(l.ActorType), l.Query, nullStr(l.ParentID),
		l.Depth, l.Priority, string(l.Status),
		nullStr(l.LeaseOwner), nullMicros(l.LeaseExpires),
		toMicros(l.CreatedAt), toMicros(l.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: insert lead: %w", err)
	}
	return nil
}

func scanLead(sc interface{ Scan(...any) error }) (*core.Lead, error) {
	var (
		l         core.Lead
		actorType string
		parentID  sql.NullString
		status    string
		owner     sql.NullString
		expires   sql.NullInt64
		created   int64
		updated   int64
	)
	err := sc.Scan(&l.ID, &l.SessionID, &actorType, &l.Query, &parentID,
		&l.Depth, &l.Priority, &status, &owner, &expires, &created, &updated)
	if err != nil {
		return nil, err
	}
	l.ActorType = core.ActorType(actorType)
	l.ParentID = strPtr(parentID)
	l.Status = core.LeadStatus(status)
	l.LeaseOwner = strPtr(owner)
	l.LeaseExpires = micrasPtr(expires)
	l.CreatedAt = fromMicros(created)
	l.UpdatedAt = fromMicros(updated)
	return &l, nil
}

func (t *queries) GetLead(ctx context.Context, id string) (*core.Lead, error) {
	row := t.q.QueryRowContext(ctx, `SELECT `+leadCols+` FROM leads WHERE id = ?`, id)
	l, err := scanLead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get lead: %w", err)
	}
	return l, nil
}

func (t *queries) ListLeads(ctx context.Context, sessionID string, limit int) ([]*core.Lead, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := t.q.QueryContext(ctx,
		`SELECT `+leadCols+` FROM leads WHERE session_id = ? ORDER BY created_at LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list leads: %w", err)
	}
	defer rows.Close()

	var out []*core.Lead
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (t *queries) SetLeadStatus(ctx context.Context, id string, status core.LeadStatus) error {
	res, err := t.q.ExecContext(ctx,
		`UPDATE leads SET status = ?, updated_at = ? WHERE id = ?`,
		string(status), toMicros(time.Now()), id)
	if err != nil {
		return fmt.Errorf("sqlite: set lead status: %w", err)
	}
	return requireOneRow(res, "lead")
}

// ---------------------------------------------------------------------------
// Claims
// ---------------------------------------------------------------------------

const claimCols = `id, session_id, lead_id, text, source, tool_call_id, quote,
	quote_offset, published_at, retrieved_at, root_claim_id, verify_depth,
	confidence, grounded, created_at`

// InsertClaims writes a batch.
//
// All or nothing: a partial batch would leave the claim graph citing a lead
// that went on to report failure, and §11's clustering would then be reasoning
// over evidence that was never fully recorded.
func (t *queries) InsertClaims(ctx context.Context, claims []core.Claim) error {
	now := time.Now().UTC()
	for i := range claims {
		c := &claims[i]
		if c.ID == "" {
			c.ID = core.NewClaimID()
		}
		// A claim with no verification ancestry is its own root. Setting this
		// here rather than at the call site is what keeps the lineage cap in
		// §11.4 from silently starting over on every new claim.
		if c.RootClaimID == "" {
			c.RootClaimID = c.ID
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = now
		}
		if c.RetrievedAt.IsZero() {
			c.RetrievedAt = now
		}

		var grounded sql.NullInt64
		if c.Grounded != nil {
			grounded = sql.NullInt64{Int64: b2i(*c.Grounded), Valid: true}
		}

		_, err := t.q.ExecContext(ctx, `
			INSERT INTO claims (`+claimCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			c.ID, c.SessionID, c.LeadID, c.Text, c.Source, c.ToolCallID, c.Quote,
			c.QuoteOffset, nullMicros(c.PublishedAt), toMicros(c.RetrievedAt),
			c.RootClaimID, c.VerifyDepth, c.Confidence, grounded, toMicros(c.CreatedAt))
		if err != nil {
			return fmt.Errorf("sqlite: insert claim %d/%d: %w", i+1, len(claims), err)
		}
	}
	return nil
}

func (t *queries) ListClaims(ctx context.Context, sessionID string, limit int) ([]*core.Claim, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := t.q.QueryContext(ctx,
		`SELECT `+claimCols+` FROM claims WHERE session_id = ? ORDER BY created_at LIMIT ?`,
		sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list claims: %w", err)
	}
	defer rows.Close()

	var out []*core.Claim
	for rows.Next() {
		var (
			c         core.Claim
			published sql.NullInt64
			retrieved int64
			grounded  sql.NullInt64
			created   int64
		)
		if err := rows.Scan(&c.ID, &c.SessionID, &c.LeadID, &c.Text, &c.Source,
			&c.ToolCallID, &c.Quote, &c.QuoteOffset, &published, &retrieved,
			&c.RootClaimID, &c.VerifyDepth, &c.Confidence, &grounded, &created); err != nil {
			return nil, err
		}
		c.PublishedAt = micrasPtr(published)
		c.RetrievedAt = fromMicros(retrieved)
		c.CreatedAt = fromMicros(created)
		if grounded.Valid {
			v := grounded.Int64 != 0
			c.Grounded = &v
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

func (t *queries) CountClaims(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := t.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE session_id = ?`, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count claims: %w", err)
	}
	return n, nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
