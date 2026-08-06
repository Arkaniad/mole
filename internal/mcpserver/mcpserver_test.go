package mcpserver_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/mcpserver"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
	"github.com/lajosdeme/mole/internal/tools/search"
)

// --- stubs -----------------------------------------------------------------

type idlePlanner struct{}

func (idlePlanner) Name() string               { return "stub" }
func (idlePlanner) ModelFor(t llm.Tier) string { return "stub-model" }
func (idlePlanner) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{
		Text:  `{"questions":[],"rationale":"none"}`,
		Model: "stub-model",
		Usage: llm.Usage{InputTokens: 1, OutputTokens: 1},
	}, nil
}

type emptySearch struct{}

func (emptySearch) Kind() search.Kind { return search.KindTavily }
func (emptySearch) Search(context.Context, string, search.Options) (*search.Response, error) {
	return &search.Response{}, nil
}

// --- harness ----------------------------------------------------------------

type rig struct {
	client *mcp.ClientSession
	db     store.Store
	sup    *session.Supervisor
}

// connect wires a real MCP client to the server over an in-memory transport.
//
// A real client rather than calling the handlers directly, because the contract
// this slice ships is a wire format: schemas inferred from Go structs, arguments
// unmarshalled by the SDK, results marshalled back. Calling the Go functions
// would test none of that and would stay green through a field rename that
// breaks every caller.
func connect(t *testing.T, maxUSD int64) *rig {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	runner := &session.Runner{
		Store: db,
		Actor: &actors.WebActor{LLM: idlePlanner{}, Search: emptySearch{}},
		Owner: "test",
	}
	sup := session.NewSupervisor(runner, 4, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sup.Shutdown(ctx)
	})

	srv := mcpserver.New(mcpserver.Deps{
		Supervisor:    sup,
		Store:         db,
		MaxSessionUSD: maxUSD,
		MaxSources:    3,
		MaxDepth:      1,
		MaxLeads:      4,
		Timeout:       time.Minute,
	})

	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return &rig{client: cs, db: db, sup: sup}
}

// call invokes a tool and decodes its structured result.
func (r *rig) call(t *testing.T, name string, args any, out any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	res, err := r.client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: transport error: %v", name, err)
	}
	if res.IsError {
		return res
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("%s: re-marshal: %v", name, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: decode into %T: %v\n%s", name, out, err, raw)
		}
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// --- tests ------------------------------------------------------------------

// TestTheAdvertisedToolsAreTheOnesWeAgreedToShip.
//
// §5.1 lists nine tools; three of them need M8 or M9. Advertising a tool that
// errors is worse than not advertising it, because an agent plans around its
// existence — it will tell a user it can analyze their database and then fail.
func TestTheAdvertisedToolsAreTheOnesWeAgreedToShip(t *testing.T) {
	r := connect(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := r.client.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)

	want := []string{
		"research.cancel", "research.report", "research.result",
		"research.sessions.list", "research.status",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("advertised tools:\n  got  %v\n  want %v", got, want)
	}
	// The three that need another milestone must be absent, by name.
	for _, absent := range []string{"research.dataset", "research.analyze_local", "research.connectors.list"} {
		for _, g := range got {
			if g == absent {
				t.Errorf("%s is advertised but its milestone has not landed", absent)
			}
		}
	}
	// Every tool needs a description an agent can route on.
	for _, tool := range res.Tools {
		if len(tool.Description) < 40 {
			t.Errorf("%s has a %d-character description; an agent cannot tell when to call it",
				tool.Name, len(tool.Description))
		}
	}
}

// TestReportStartsASessionAndReturnsImmediately is §5.3's async contract.
func TestReportStartsASessionAndReturnsImmediately(t *testing.T) {
	r := connect(t, 0)

	var out mcpserver.ReportOut
	res := r.call(t, "research.report", map[string]any{
		"prompt": "what is a byte-level language model",
		"budget": map[string]any{"unit": "usd", "amount": "0.50"},
	}, &out)
	if res.IsError {
		t.Fatalf("report failed: %s", errText(res))
	}
	if !strings.HasPrefix(out.SessionID, "s_") {
		t.Errorf("session_id = %q, want an s_ id", out.SessionID)
	}
	if out.Budget != 500_000 {
		t.Errorf("budget = %d micro-dollars, want 500000 ($0.50)", out.Budget)
	}
	if out.Note == "" {
		t.Error("no note telling the caller to poll; the async contract is invisible otherwise")
	}

	// The session is real and findable by the other tools.
	var st mcpserver.StatusOut
	r.call(t, "research.status", map[string]any{"session_id": out.SessionID}, &st)
	if st.SessionID != out.SessionID {
		t.Errorf("status returned a different session: %q", st.SessionID)
	}
	if st.Budget != out.Budget {
		t.Errorf("status budget %d != report budget %d", st.Budget, out.Budget)
	}
}

// TestTheBudgetCeilingIsTheDaemonsNotTheCallers.
//
// On the command line the budget is a number a person typed. Over MCP it is a
// number an agent chose, and nothing else bounds it. This is the one limit the
// caller cannot raise.
func TestTheBudgetCeilingIsTheDaemonsNotTheCallers(t *testing.T) {
	r := connect(t, 1_000_000) // $1.00

	res := r.call(t, "research.report", map[string]any{
		"prompt": "expensive question",
		"budget": map[string]any{"unit": "usd", "amount": "50.00"},
	}, nil)
	if !res.IsError {
		t.Fatal("a $50 request was accepted under a $1 ceiling")
	}
	msg := errText(res)
	for _, want := range []string{"$1.00", "daemon.max-session-usd"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so the caller cannot act on it: %s", want, msg)
		}
	}

	// Nothing was created. A refused request must not leave a session behind.
	var list mcpserver.ListOut
	r.call(t, "research.sessions.list", map[string]any{}, &list)
	if len(list.Sessions) != 0 {
		t.Errorf("a refused request created %d session(s)", len(list.Sessions))
	}

	// And a request under the ceiling still works.
	var ok mcpserver.ReportOut
	if res := r.call(t, "research.report", map[string]any{
		"prompt": "affordable question",
		"budget": map[string]any{"unit": "usd", "amount": "0.25"},
	}, &ok); res.IsError {
		t.Fatalf("a request under the ceiling was refused: %s", errText(res))
	}
}

// TestABudgetWithoutAUnitIsRefused. §8 makes the unit semantically load-bearing:
// a caller that means tokens and gets dollars overspends by six orders of
// magnitude, so there is no bare-number form here any more than on the CLI.
//
// Two layers refuse these, and the split is worth knowing. The SDK infers the
// input schema from the Go struct, and neither Budget field carries omitempty —
// so a MISSING unit or amount is rejected by schema validation before any of our
// code runs. The remaining cases (unknown unit, zero, negative, unparseable)
// reach parseBudget, and mutating either of its checks fails this test.
func TestABudgetWithoutAUnitIsRefused(t *testing.T) {
	r := connect(t, 0)
	for name, budget := range map[string]map[string]any{
		"no unit":      {"amount": "1.00"},
		"no amount":    {"unit": "usd"},
		"unknown unit": {"unit": "euros", "amount": "1.00"},
		"zero":         {"unit": "usd", "amount": "0"},
		"negative":     {"unit": "usd", "amount": "-5"},
		"not a number": {"unit": "tokens", "amount": "lots"},
	} {
		res := r.call(t, "research.report", map[string]any{
			"prompt": "a question", "budget": budget,
		}, nil)
		if !res.IsError {
			t.Errorf("%s: accepted %v", name, budget)
		}
	}
}

// TestResultTruncationIsVisible.
//
// The result goes straight into an agent's context and nothing upstream bounds
// how many claims a session gathers. Silently returning a prefix is how a caller
// concludes a session found less than it did — the same defect as a search that
// quietly drops results.
func TestResultTruncationIsVisible(t *testing.T) {
	r := connect(t, 0)

	var rep mcpserver.ReportOut
	r.call(t, "research.report", map[string]any{
		"prompt": "a question",
		"budget": map[string]any{"unit": "usd", "amount": "0.50"},
	}, &rep)

	// More claims than the cap, written directly: the point is the boundary, not
	// how the claims got there.
	total := mcpserver.MaxClaimsReturned + 25
	writeClaims(t, r.db, rep.SessionID, total)

	var out mcpserver.ResultOut
	r.call(t, "research.result", map[string]any{"session_id": rep.SessionID}, &out)

	if len(out.Claims) != mcpserver.MaxClaimsReturned {
		t.Errorf("returned %d claims, want the cap of %d", len(out.Claims), mcpserver.MaxClaimsReturned)
	}
	if out.TotalClaims != total {
		t.Errorf("total_claims = %d, want %d — the caller cannot tell what it is missing",
			out.TotalClaims, total)
	}
	if !out.Truncated {
		t.Error("truncated is false on a truncated result")
	}
	if !strings.Contains(out.Note, "of") {
		t.Errorf("the note does not say how much was withheld: %q", out.Note)
	}
}

// TestUnknownSessionsAreRefusedByEveryTool. A daemon is long-lived and ids come
// from an agent's context, which may be stale or invented.
func TestUnknownSessionsAreRefusedByEveryTool(t *testing.T) {
	r := connect(t, 0)
	for _, tool := range []string{"research.status", "research.result", "research.cancel"} {
		res := r.call(t, tool, map[string]any{"session_id": "s_NOPE"}, nil)
		if !res.IsError {
			t.Errorf("%s accepted an unknown session id", tool)
		}
		if !strings.Contains(errText(res), "s_NOPE") {
			t.Errorf("%s does not name the id it rejected: %s", tool, errText(res))
		}
	}
	// An empty id is a caller bug, not a lookup miss, and should say so.
	res := r.call(t, "research.status", map[string]any{"session_id": ""}, nil)
	if !res.IsError {
		t.Error("an empty session_id was accepted")
	}
}

// TestCancellingAFinishedSessionIsNotAnError.
//
// "No such session" and "that one already finished" are different answers, and
// an agent polling a session that completed between its status call and its
// cancel call should get the second.
func TestCancellingAFinishedSessionIsNotAnError(t *testing.T) {
	r := connect(t, 0)

	var rep mcpserver.ReportOut
	r.call(t, "research.report", map[string]any{
		"prompt": "a question",
		"budget": map[string]any{"unit": "usd", "amount": "0.50"},
	}, &rep)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = r.sup.Wait(ctx, rep.SessionID)
	for i := 0; i < 100 && len(r.sup.Running()) > 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}

	var out mcpserver.CancelOut
	res := r.call(t, "research.cancel", map[string]any{"session_id": rep.SessionID}, &out)
	if res.IsError {
		t.Fatalf("cancelling a finished session errored: %s", errText(res))
	}
	if out.Cancelled {
		t.Error("reported cancelling a session that had already finished")
	}
	if !strings.Contains(out.Note, "not running") {
		t.Errorf("the note does not explain why nothing happened: %q", out.Note)
	}
}

func TestSessionsListFindsWhatWasStarted(t *testing.T) {
	r := connect(t, 0)
	var rep mcpserver.ReportOut
	r.call(t, "research.report", map[string]any{
		"prompt": "findable question",
		"budget": map[string]any{"unit": "usd", "amount": "0.50"},
	}, &rep)

	var list mcpserver.ListOut
	r.call(t, "research.sessions.list", map[string]any{}, &list)
	for _, s := range list.Sessions {
		if s.SessionID == rep.SessionID {
			if s.Prompt != "findable question" {
				t.Errorf("prompt = %q", s.Prompt)
			}
			return
		}
	}
	t.Errorf("the session just started is not in the list: %+v", list.Sessions)
}

// writeClaims inserts n claims directly, with a lead of its own.
//
// It creates the lead rather than looking one up. The first version searched for
// the lead the executor makes when the planner proposes nothing — which it
// creates asynchronously, so the test passed or failed depending on whether the
// goroutine had got there yet. It failed roughly one run in two, and `go test`
// without -v reported "ok" on the run I first looked at.
func writeClaims(t *testing.T, db store.Store, sessionID string, n int) {
	t.Helper()
	ctx := context.Background()

	lead := core.Lead{
		ID: core.NewLeadID(), SessionID: sessionID,
		ActorType: core.ActorWeb, Query: "fixture", Status: core.LeadQueued,
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertLead(ctx, &lead)
	}); err != nil {
		t.Fatalf("insert lead: %v", err)
	}

	claims := make([]core.Claim, 0, n)
	for i := 0; i < n; i++ {
		claims = append(claims, core.Claim{
			ID:          core.NewClaimID(),
			SessionID:   sessionID,
			LeadID:      lead.ID,
			Text:        strings.Repeat("claim ", 3) + string(rune('a'+i%26)),
			Source:      "https://example.com/x",
			Quote:       "a quote",
			RetrievedAt: time.Now().UTC(),
		})
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertClaims(ctx, claims)
	}); err != nil {
		t.Fatalf("insert claims: %v", err)
	}
}
