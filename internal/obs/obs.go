// Package obs provides structured logging and lead-level tracing.
//
// Spans are persisted to the same database as everything else rather than
// exported to an OTel collector. That is a deliberate M0 choice: `mole trace
// <session>` has to work offline, on a user's laptop, with no extra service
// running. An OTel bridge can be added later without changing call sites.
package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// LogOptions configures the process logger.
type LogOptions struct {
	Level  string // debug | info | warn | error
	Format string // text | json
	Out    io.Writer
}

// NewLogger builds a slog.Logger. JSON is the right default for the daemon
// (logs get grepped and shipped); text is friendlier for the CLI.
func NewLogger(o LogOptions) *slog.Logger {
	out := o.Out
	if out == nil {
		out = os.Stderr
	}

	var level slog.Level
	switch strings.ToLower(o.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(o.Format, "text") {
		return slog.New(slog.NewTextHandler(out, opts))
	}
	return slog.New(slog.NewJSONHandler(out, opts))
}

// ---------------------------------------------------------------------------
// Tracing
// ---------------------------------------------------------------------------

type ctxKey struct{}

// Tracer records spans.
type Tracer struct {
	st  store.Store
	log *slog.Logger
}

func NewTracer(st store.Store, log *slog.Logger) *Tracer {
	if log == nil {
		log = slog.Default()
	}
	return &Tracer{st: st, log: log}
}

// Span is an in-flight trace record.
type Span struct {
	tracer *Tracer
	rec    *core.Span
	ended  bool
}

// SpanOption customizes a span at creation.
type SpanOption func(*core.Span)

func WithLead(leadID string) SpanOption {
	return func(s *core.Span) { s.LeadID = &leadID }
}

func WithAttrs(attrs map[string]string) SpanOption {
	return func(s *core.Span) {
		if s.Attrs == nil {
			s.Attrs = map[string]string{}
		}
		for k, v := range attrs {
			s.Attrs[k] = v
		}
	}
}

// Start opens a span and returns a context carrying it, so a nested Start
// automatically attaches as a child.
//
// A failure to persist a span is logged, never returned: observability must not
// be able to fail the work it is observing.
func (t *Tracer) Start(ctx context.Context, sessionID, name string, opts ...SpanOption) (context.Context, *Span) {
	rec := &core.Span{
		ID:        core.NewSpanID(),
		SessionID: sessionID,
		Name:      name,
		StartedAt: time.Now().UTC(),
	}
	if parent, ok := ctx.Value(ctxKey{}).(*Span); ok && parent != nil {
		id := parent.rec.ID
		rec.ParentID = &id
		if rec.LeadID == nil {
			rec.LeadID = parent.rec.LeadID
		}
	}
	for _, o := range opts {
		o(rec)
	}

	if t.st != nil {
		err := t.st.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
			return tx.StartSpan(ctx, rec)
		})
		if err != nil {
			t.log.WarnContext(ctx, "span start failed", "span", rec.ID, "name", name, "err", err)
		}
	}

	sp := &Span{tracer: t, rec: rec}
	return context.WithValue(ctx, ctxKey{}, sp), sp
}

// End closes the span. Calling End twice is a no-op.
func (s *Span) End(ctx context.Context, status string) {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	now := time.Now().UTC()
	s.rec.EndedAt = &now
	s.rec.Status = status

	if s.tracer.st == nil {
		return
	}
	// Use a detached context: a span must still be closed when the work it
	// wraps was cancelled, which is exactly when the trace matters most.
	detached := context.WithoutCancel(ctx)
	err := s.tracer.st.WithTx(detached, func(ctx context.Context, tx store.Tx) error {
		return tx.EndSpan(ctx, s.rec.ID, now, status)
	})
	if err != nil {
		s.tracer.log.WarnContext(detached, "span end failed", "span", s.rec.ID, "err", err)
	}
}

// ID exposes the span identifier, for correlating log lines.
func (s *Span) ID() string {
	if s == nil {
		return ""
	}
	return s.rec.ID
}

// Duration reports elapsed time; for an open span, time so far.
func (s *Span) Duration() time.Duration {
	if s == nil {
		return 0
	}
	if s.rec.EndedAt != nil {
		return s.rec.EndedAt.Sub(s.rec.StartedAt)
	}
	return time.Since(s.rec.StartedAt)
}

// FromContext returns the active span, if any.
func FromContext(ctx context.Context) *Span {
	sp, _ := ctx.Value(ctxKey{}).(*Span)
	return sp
}
