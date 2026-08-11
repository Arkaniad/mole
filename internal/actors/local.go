package actors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lajosdeme/mole/internal/compute/coderunner"
	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/compute/gate"
	"github.com/lajosdeme/mole/internal/compute/hypothesis"
	"github.com/lajosdeme/mole/internal/compute/sqlguard"
	"github.com/lajosdeme/mole/internal/compute/stats"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/pricing"
)

// LocalComputeActor answers a lead from the user's own data (M8, §12).
//
// The shape is the same as the other two actors — take a lead, do the work,
// return summary plus claims plus costs — and the evidence is different in one
// way that decides the whole implementation: it never leaves the machine.
//
// The run is four steps, and the model is only in the first and the last:
//
//	profile  →  the model picks a template and columns   (§12.3: it cannot author SQL)
//	         →  hypothesis.Render turns that into SQL     (identifiers from the profile)
//	         →  sqlguard + the aggregation gate run it    (§12.2, §12.1)
//	         →  the model mines claims from the envelope  (§11.5 quote check, unchanged)
//
// The middle two steps are deterministic. What the model influences is which
// question gets asked; what it never touches is the data, the statement, or
// whether the answer may cross.
type LocalComputeActor struct {
	// Connectors is the registered set. The model chooses among them by name,
	// so a run with none configured is a no-op rather than an error — the same
	// way a web run with no search provider is a configuration problem and not
	// a crash.
	Connectors ConnectorSource

	LLM     llm.Provider
	Pricing *pricing.Table
	Log     *slog.Logger
	Budget  Budget

	// Gate tunes the aggregation gate. The zero value is §12.1's defaults, and
	// raising KFloor is the only knob a privacy-conscious user needs.
	Gate gate.Options

	// Code runs model-authored analysis in the sandbox (§12.1). Nil disables it,
	// which is the state of any machine without a container runtime — and a
	// supported one: the SQL path needs no sandbox, so a nil Code costs the
	// hypotheses SQL cannot express and nothing else.
	Code coderunner.Runner
}

// ConnectorSource is the registry, narrowed to what the actor needs.
type ConnectorSource interface {
	List() []connector.Connector
}

func (a *LocalComputeActor) Type() core.ActorType { return core.ActorLocalCompute }

func (a *LocalComputeActor) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// maxHypotheses caps how many questions one lead may ask.
const defaultHypotheses = 3

func (a *LocalComputeActor) Run(ctx context.Context, lead core.Lead) (*Result, error) {
	budget := a.Budget
	if sub, ok := SubBudgetFrom(ctx); ok {
		budget = budget.tighten(sub)
	}
	maxHypotheses := budget.MaxSources
	if maxHypotheses <= 0 {
		maxHypotheses = defaultHypotheses
	}
	maxClaims := budget.MaxClaimsPerSource
	if maxClaims <= 0 {
		maxClaims = 8
	}

	res := &Result{}
	sources := a.Connectors.List()
	if len(sources) == 0 {
		res.Summary = "No local data is registered, so this lead was not researched. " +
			"`mole connect add <name> <path>` registers a file or a folder."
		return res, nil
	}

	plans, err := a.plan(ctx, lead, sources, maxHypotheses, res)
	if err != nil {
		return res, err
	}
	if len(plans) == 0 {
		res.Summary = "No hypothesis could be formed over the registered data for this lead."
		return res, nil
	}

	miner := &Miner{LLM: a.LLM, Pricing: a.Pricing, Log: a.Log, SessionID: lead.SessionID}
	var findings []string

	for _, p := range plans {
		if err := ctx.Err(); err != nil {
			res.Truncated = true
			break
		}
		text, cite, note, err := a.evidence(ctx, sources, p)
		if err != nil {
			// A refused or unanswerable hypothesis is a normal outcome, not a
			// failed run: the gate exists to say no. It is recorded so the
			// trace shows what was tried.
			a.logger().InfoContext(ctx, "hypothesis not answered",
				"connector", p.Connector, "table", p.Table, "template", p.Template, "err", err)
			res.Stats.ChunksSkipped++
			continue
		}
		res.Stats.SearchResults++
		res.Stats.Chunks++

		out, err := miner.Mine(ctx, MineInput{
			Lead:      lead,
			SourceURL: cite,
			Title:     evidenceTitle(p),
			Text:      text,
			MaxClaims: maxClaims,
		})
		if out.HasCall {
			res.Costs = append(res.Costs, out.Call)
		}
		res.Stats.ClaimsProposed += out.Proposed
		res.Stats.ClaimsRejected += out.Rejected
		if err != nil {
			res.Stats.ChunksFailed++
			a.logger().WarnContext(ctx, "mining a local result failed",
				"connector", p.Connector, "err", err)
			continue
		}
		res.Claims = append(res.Claims, note.cap(out.Claims)...)
		findings = append(findings, note.finding)
	}

	res.Summary = localSummary(lead, findings, res)
	return res, nil
}

// verdictNote is what a run learned about its own evidence, kept separately from
// the passage so the two paths can share the capping rule.
type verdictNote struct {
	finding     string
	unsupported bool
}

// cap applies §4's statistical-validity check.
func (n verdictNote) cap(claims []core.Claim) []core.Claim {
	if !n.unsupported {
		return claims
	}
	for i := range claims {
		if claims[i].AssertionStrength > unsupportedAssertionCap {
			claims[i].AssertionStrength = unsupportedAssertionCap
		}
	}
	return claims
}

// evidence turns one plan into the passage claims are mined from.
//
// Two routes, and which one is taken is the plan's choice rather than a
// fallback: a template renders SQL through both gates, and a code plan runs in
// the sandbox. Both end at a passage of figures and a citation, so everything
// downstream — mining, the quote check, the claim graph — cannot tell them
// apart. That is the point: the evidence differs and the standard does not.
func (a *LocalComputeActor) evidence(
	ctx context.Context, sources []connector.Connector, p hypothesis.Plan,
) (text, cite string, note verdictNote, err error) {
	c, ok := findConnector(sources, p.Connector)
	if !ok {
		return "", "", note, fmt.Errorf("no connector named %q is registered", p.Connector)
	}
	if p.Code != nil {
		return a.runCode(ctx, c, p)
	}
	return a.runQuery(ctx, c, p)
}

// runQuery is the SQL route: render, both gates, envelope.
func (a *LocalComputeActor) runQuery(
	ctx context.Context, c connector.Connector, p hypothesis.Plan,
) (string, string, verdictNote, error) {
	var note verdictNote

	query, err := hypothesis.Render(c, p)
	if err != nil {
		return "", "", note, err
	}
	// hypothesis.Render only emits statements from its own templates, so this
	// cannot fail — which is exactly why it is called. §12.2 asks for the parse
	// gate on the path to the database, not on the paths thought likely to
	// carry something bad.
	if err := sqlguard.Check(query); err != nil {
		return "", "", note, err
	}

	db, err := c.Open()
	if err != nil {
		return "", "", note, err
	}
	defer db.Close()

	opts := a.Gate
	opts.Log = a.logger()
	opts.FreeTextColumns = freeTextColumns(c)

	env, err := gate.Aggregate(ctx, db, query, opts)
	if err != nil {
		return "", "", note, err
	}

	note.finding = describeFinding(p, env)
	note.unsupported = unsupportedComparison(env)
	// §4's table: a local claim's source is "connector name + query hash", and a
	// claim nobody can trace back to a statement is not evidence.
	cite := fmt.Sprintf("connector:%s#%s", c.Name, env.QueryHash[:16])
	return env.Text(), cite, note, nil
}

// runCode is the sandbox route (§12.1).
//
// The script never touches the aggregation gate, because it is on the other side
// of it: the gate exists to stop rows reaching a model, and inside the sandbox
// there is no model. What constrains this route is the container and the output
// contract — see internal/compute/coderunner.
func (a *LocalComputeActor) runCode(
	ctx context.Context, c connector.Connector, p hypothesis.Plan,
) (string, string, verdictNote, error) {
	var note verdictNote
	if a.Code == nil {
		return "", "", note, errors.New(
			"no container runtime is available, so a code hypothesis cannot run " +
				"(mole doctor reports what is missing)")
	}
	query, _ := hypothesis.Render(c, p)

	out, err := a.Code.Analyze(ctx, coderunner.Request{
		DBPath: c.DBPath,
		Query:  query,
		Script: p.Code.Script,
		Contract: coderunner.Contract{
			Metrics: p.Code.Metrics,
			Tests:   p.Code.Tests,
		},
	})
	if err != nil {
		return "", "", note, err
	}
	if out.Empty() {
		return "", "", note, fmt.Errorf(
			"the analysis produced nothing that may cross (%d declared and %d undeclared "+
				"output(s) refused)", len(out.Dropped), out.Undeclared)
	}

	note.finding = describeCodeFinding(p, out)
	note.unsupported = unsupportedFindings(out)
	// The script is what produced the evidence, so the script is what the
	// citation identifies. A query hash would name a statement that only
	// suggested what to look at.
	cite := fmt.Sprintf("connector:%s#code:%s", c.Name, shortHash(p.Code.Script))
	return out.Text(p.Code.Script), cite, note, nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// unsupportedComparison reports whether an envelope ran a comparison and none of
// them supported a difference.
func unsupportedComparison(env gate.AggregateEnvelope) bool {
	if len(env.TestResults) == 0 {
		return false
	}
	for _, t := range env.TestResults {
		if t.Verdict == stats.Significant {
			return false
		}
	}
	return true
}

func unsupportedFindings(out coderunner.Output) bool {
	if len(out.Findings) == 0 {
		return false
	}
	for _, f := range out.Findings {
		if f.Verdict == stats.Significant {
			return false
		}
	}
	return true
}

func describeCodeFinding(p hypothesis.Plan, out coderunner.Output) string {
	q := p.Question
	if strings.TrimSpace(q) == "" {
		q = "a sandboxed analysis over " + p.Connector
	}
	line := fmt.Sprintf("%s — ran in the sandbox, %d metric(s) and %d statistical result(s)",
		q, len(out.Metrics), len(out.Findings))
	if n := len(out.Dropped) + out.Undeclared; n > 0 {
		line += fmt.Sprintf(", %d output(s) refused", n)
	}
	for _, f := range out.Findings {
		line += fmt.Sprintf("; %s %s", f.Name, f.Verdict)
	}
	return line
}

func findConnector(sources []connector.Connector, name string) (connector.Connector, bool) {
	for _, c := range sources {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return connector.Connector{}, false
}

// freeTextColumns is what the profile already decided, handed to the gate so it
// can union that with what it derives from the result.
func freeTextColumns(c connector.Connector) []string {
	var out []string
	for _, t := range c.Tables {
		for _, col := range t.Columns {
			if col.FreeText {
				out = append(out, col.Name)
			}
		}
	}
	return out
}

// unsupportedAssertionCap is where a claim lands when the envelope it was mined
// from ran a comparison and the comparison did not support one.
//
// A cap and not a rejection, because the claim may be perfectly true — "the
// north region has the most records" is a fact about the counts whether or not
// the means differ significantly. What it must not do is enter the graph
// asserting as much as a claim the data actually supports. §11.3 derives
// confidence from the graph, and AssertionStrength is what the source says
// about itself; an underpowered result should not say much.
const unsupportedAssertionCap = 0.3

// -----------------------------------------------------------------------------
// Planning
// -----------------------------------------------------------------------------

// plan asks the model which questions to put to the data.
//
// One call for every hypothesis rather than one per hypothesis: the schema is
// the expensive part of the prompt and it does not change between them.
func (a *LocalComputeActor) plan(
	ctx context.Context, lead core.Lead, sources []connector.Connector, max int, res *Result,
) ([]hypothesis.Plan, error) {
	fence := fenceToken()
	prompt := planPrompt(fence, lead.Query, sources, max, a.Code != nil)

	resp, err := a.LLM.Complete(ctx, llm.Request{
		Tier:      llm.TierCheap,
		System:    planSystemPrompt,
		Messages:  []llm.Message{llm.User(prompt)},
		MaxTokens: 2048,
	})
	if resp == nil {
		return nil, err
	}
	res.Costs = append(res.Costs,
		toolCallFor(lead.SessionID, lead, resp, "local:plan", a.Pricing, a.Log, err))
	if err != nil {
		return nil, err
	}
	if resp.Refused {
		return nil, fmt.Errorf("actors: model refused to plan (%s)", resp.RefusalCategory)
	}

	plans, err := parsePlans(resp.Text)
	if err != nil {
		return nil, err
	}
	if len(plans) > max {
		plans = plans[:max]
	}
	return plans, nil
}

func parsePlans(raw string) ([]hypothesis.Plan, error) {
	body := extractJSONArray(raw)
	if body == "" {
		return nil, fmt.Errorf("actors: no hypothesis list in the model's reply: %s",
			truncateForLog(raw))
	}
	var plans []hypothesis.Plan
	if err := json.Unmarshal([]byte(body), &plans); err != nil {
		// The reply is quoted, and it needs to be. A weak model fails here in
		// ways the error alone does not identify — a live run against a 3B
		// model returned a doubly-wrapped array, which reads only as "cannot
		// unmarshal array into ...Plan" unless the text is in front of you.
		return nil, fmt.Errorf("actors: parse hypotheses: %w; reply was: %s",
			err, truncateForLog(body))
	}
	return plans, nil
}

// extractJSONArray finds the outermost bracketed array in a reply.
func extractJSONArray(raw string) string {
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return ""
	}
	return raw[start : end+1]
}

// -----------------------------------------------------------------------------
// Summary
// -----------------------------------------------------------------------------

func describeFinding(p hypothesis.Plan, env gate.AggregateEnvelope) string {
	q := p.Question
	if strings.TrimSpace(q) == "" {
		q = fmt.Sprintf("%s over %s.%s", p.Template, p.Connector, p.Table)
	}
	line := fmt.Sprintf("%s — %d row(s) described", q, env.RowCount)
	if env.Suppressed > 0 {
		line += fmt.Sprintf(", %d group(s) too small to report", env.Suppressed)
	}
	// The verdict, in the summary the planner reads. Without it a replan sees
	// "compared revenue by region" and treats an underpowered result as a
	// settled one worth building on.
	for _, t := range env.TestResults {
		line += fmt.Sprintf("; comparison %s", t.Verdict)
	}
	return line
}

func localSummary(lead core.Lead, findings []string, res *Result) string {
	if len(findings) == 0 {
		return fmt.Sprintf(
			"No hypothesis over the registered data could be answered for %q. "+
				"%d were tried.", lead.Query, res.Stats.ChunksSkipped)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Queried local data for %q. %d hypothes%s answered, yielding %d claim(s).\n\n",
		lead.Query, len(findings), plural(len(findings)), len(res.Claims))
	for _, f := range findings {
		b.WriteString("- " + f + "\n")
	}
	b.WriteString("\nNo row from the data left the machine: every figure above came " +
		"through the aggregation gate as an aggregate (§12.1).")
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return "is"
	}
	return "es"
}

// evidenceTitle names the source in the mining prompt.
func evidenceTitle(p hypothesis.Plan) string {
	if p.Code != nil {
		return fmt.Sprintf("%s.%s — sandboxed analysis", p.Connector, p.Table)
	}
	return fmt.Sprintf("%s.%s — %s", p.Connector, p.Table, p.Template)
}
