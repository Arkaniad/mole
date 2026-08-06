package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/output"
	"github.com/lajosdeme/mole/internal/pricing"
)

// AskAllowance bounds what one research.ask call may spend, in micro-dollars.
//
// §13's promise is that an ask is cheap: one model call against claims already
// paid for. The allowance is what makes that true rather than aspirational —
// without it an ask draws on whatever the session has left, and a session that
// stopped early with most of its budget intact could spend it all answering one
// follow-up.
const AskAllowance = 50_000 // $0.05

type AskIn struct {
	SessionID string `json:"session_id" jsonschema:"the session whose research should be queried"`
	Question  string `json:"question" jsonschema:"the follow-up question to answer from that session's claims"`
}

type AskOut struct {
	SessionID string `json:"session_id"`
	Question  string `json:"question"`
	Answer    string `json:"answer"`

	// Claims are the ones the answer is built from, so a caller can weigh the
	// evidence rather than the prose (§5.3).
	Claims []Claim `json:"claims"`
	// Citations maps each [n] in the answer to its source.
	Citations []Citation `json:"citations"`

	// AskSessionID is the small session this answer was charged to. Present so
	// the cost is traceable: it will not appear against the session that was
	// queried, whose accounts are closed.
	AskSessionID string `json:"ask_session_id,omitempty"`
	Spent        int64  `json:"spent"`
	// Degraded says why the answer is not synthesized prose, when it is not.
	Degraded string `json:"degraded,omitempty"`
}

// Citation is one numbered source behind an answer.
//
// Quotes plural: several claims can cite one source, and §13 asks for the
// verified span per citation so a reader can check the answer without
// re-fetching the page.
type Citation struct {
	N           int      `json:"n"`
	Source      string   `json:"source"`
	Quotes      []string `json:"quotes,omitempty"`
	PublishedAt string   `json:"published_at,omitempty"`
}

// ask answers a new question from a finished session's graph (§13).
//
// The ask runs as its OWN session, with mode "ask" and a small budget of its
// own, reading the source session's claims. That is not indirection for its own
// sake — it is forced, and the reason is worth stating.
//
// §8 refuses a reservation against a terminal session, and rightly: a finished
// session's ledger is settled and reconciled, and adding cost rows to it later
// rewrites a closed account. So an ask cannot be charged to the research it
// queries. Giving it a session of its own keeps every §8 invariant intact —
// every model call belongs to a live session, every cost row has a reservation,
// the source session's accounts stay closed — and has the side benefit that an
// ask shows up in sessions.list with its own cost, which is what a person
// wondering where the money went needs to see.
//
// This is also what core.ModeAsk was reserved for. §13 describes it as "listed
// in rev 1's Session.Mode but never defined"; this defines it.
func (d Deps) ask(ctx context.Context, _ *mcp.CallToolRequest, in AskIn) (*mcp.CallToolResult, AskOut, error) {
	question := strings.TrimSpace(in.Question)
	if question == "" {
		return nil, AskOut{}, errors.New("question is empty")
	}
	src, err := d.loadSession(ctx, in.SessionID)
	if err != nil {
		return nil, AskOut{}, err
	}
	if src.Status == core.StatusRunning {
		return nil, AskOut{}, fmt.Errorf(
			"session %s is still running; ask it once it has finished, or its claim "+
				"graph will change under the answer", src.ID)
	}

	allowance := int64(AskAllowance)
	if d.MaxSessionUSD > 0 && allowance > d.MaxSessionUSD {
		allowance = d.MaxSessionUSD
	}

	led := budget.New(d.Store, budget.DefaultConfig())
	gen := &output.Generator{LLM: d.LLM}

	var (
		askSess     *core.Session
		reservation *core.Reservation
	)
	if d.LLM != nil {
		askSess, err = led.CreateSession(ctx, budget.SessionSpec{
			Prompt:     question,
			Mode:       core.ModeAsk,
			ActorTypes: []core.ActorType{core.ActorWeb},
			BudgetUnit: core.BudgetUSD,
			Budget:     allowance,
			// One call, no leads, no fetches. The ceilings say so rather than
			// relying on the loop never being started.
			MaxToolCalls: 1,
			MaxLeads:     0,
			MaxWallClock: askTimeout,
		})
		if err != nil {
			return nil, AskOut{}, fmt.Errorf("could not open an ask session: %w", err)
		}
		// The escrow a session reserves at creation is for a report this session
		// will never write; releasing it makes the whole allowance spendable on the
		// one call the ask actually makes.
		if _, rerr := led.ReleaseEscrow(ctx, askSess.ID); rerr != nil {
			d.Log.Warn("could not release ask escrow", "session", askSess.ID, "err", rerr)
		}
		if cur, lerr := d.loadSession(ctx, askSess.ID); lerr == nil {
			if avail := cur.Available(); avail > 0 {
				reservation, err = led.ReserveOutput(ctx, askSess.ID, avail)
				if err != nil {
					d.Log.Warn("could not reserve for an ask", "session", askSess.ID, "err", err)
				}
			}
		}
	}
	if reservation == nil {
		// No provider, or nothing reservable. Answer then lists the relevant claims
		// with citations rather than making a call whose cost cannot be recorded.
		gen.LLM = nil
	}

	// Reads the SOURCE session's claims; spends the ask session's budget.
	rep, err := gen.Answer(ctx, d.Store, src.ID, question)
	if err != nil {
		d.closeAsk(ctx, led, askSess, reservation, nil, core.StatusFailed)
		return nil, AskOut{}, err
	}

	spent := d.closeAsk(ctx, led, askSess, reservation, rep, core.StatusDone)

	out := AskOut{
		SessionID: src.ID,
		Question:  question,
		Answer:    rep.Body,
		Spent:     spent,
		Degraded:  rep.Degraded,
	}
	if askSess != nil {
		out.AskSessionID = askSess.ID
	}
	for _, c := range rep.Citations {
		cit := Citation{N: c.N, Source: c.Source, Quotes: c.Quotes}
		if c.PublishedAt != nil {
			cit.PublishedAt = c.PublishedAt.UTC().Format(time.RFC3339)
		}
		out.Citations = append(out.Citations, cit)
	}
	for _, f := range rep.Findings {
		if f.Claim == nil {
			continue
		}
		out.Claims = append(out.Claims, Claim{
			ID:         f.Claim.ID,
			Text:       f.Claim.Text,
			Source:     f.Claim.Source,
			Quote:      f.Claim.Quote,
			Confidence: f.Confidence,
			Grounded:   f.Claim.Grounded,
		})
	}
	return nil, out, nil
}

// askTimeout bounds the ask session's wall clock. One model call.
const askTimeout = 5 * time.Minute

// closeAsk settles the reservation and finalizes the ask session.
//
// Called on every path including the failing one: an unresolved hold is budget
// neither spent nor available, and an ask session left running would be swept as
// abandoned half an hour later rather than closed now.
func (d Deps) closeAsk(
	ctx context.Context,
	led *budget.Ledger,
	askSess *core.Session,
	reservation *core.Reservation,
	rep *output.Report,
	status core.SessionStatus,
) int64 {
	if askSess == nil {
		return 0
	}
	ctx = context.WithoutCancel(ctx)

	var spent int64
	if reservation != nil {
		var calls []core.ToolCall
		if rep != nil && !rep.Cost.IsZero() {
			calls = append(calls, core.ToolCall{
				SessionID: askSess.ID,
				Role:      core.RoleOutput,
				Type:      core.CallLLM,
				Model:     rep.Model,
				Input:     "ask",
				Cost:      d.priceAsk(rep),
			})
		}
		settled, err := led.Settle(ctx, reservation, calls)
		if err != nil {
			d.Log.Warn("settling an ask failed", "session", askSess.ID, "err", err)
			if rerr := led.Release(ctx, reservation); rerr != nil {
				d.Log.Warn("could not release an ask hold", "session", askSess.ID, "err", rerr)
			}
		} else {
			spent = settled.Cost.BudgetAmount(core.BudgetUSD)
		}
	}
	if err := led.Finish(ctx, askSess.ID, status); err != nil {
		d.Log.Warn("could not finalize an ask session", "session", askSess.ID, "err", err)
	}
	return spent
}

func (d Deps) priceAsk(rep *output.Report) core.Cost {
	table := d.Pricing
	if table == nil {
		table = pricing.NewTable()
	}
	cost, err := table.Cost(rep.Model, pricing.Usage{
		InputTokens:  rep.Cost.InputTokens,
		OutputTokens: rep.Cost.OutputTokens,
	})
	if err != nil {
		// Unknown model: record the tokens, price them at zero. A row with real
		// counts and no money still reconciles; a missing row does not.
		return rep.Cost
	}
	return cost
}

// llmProvider is the subset of the provider an ask needs. Declared so Deps can
// hold one without the whole actor.
type llmProvider = llm.Provider
