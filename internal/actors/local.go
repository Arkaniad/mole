package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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
		env, cite, err := a.ask(ctx, sources, p)
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

		text := env.Text()
		out, err := miner.Mine(ctx, MineInput{
			Lead:      lead,
			SourceURL: cite,
			Title:     fmt.Sprintf("%s.%s — %s", p.Connector, p.Table, p.Template),
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
		res.Claims = append(res.Claims, capUnsupported(out.Claims, env)...)
		findings = append(findings, describeFinding(p, env))
	}

	res.Summary = localSummary(lead, findings, res)
	return res, nil
}

// ask renders one plan, runs it through both gates, and returns the envelope.
//
// The citation is built here rather than by the caller because it has to name
// the query that produced the evidence: §4's table gives a local claim's source
// as "connector name + query hash", and a claim nobody can trace back to a
// statement is not evidence.
func (a *LocalComputeActor) ask(
	ctx context.Context, sources []connector.Connector, p hypothesis.Plan,
) (gate.AggregateEnvelope, string, error) {
	c, ok := findConnector(sources, p.Connector)
	if !ok {
		return gate.AggregateEnvelope{}, "", fmt.Errorf(
			"no connector named %q is registered", p.Connector)
	}

	query, err := hypothesis.Render(c, p)
	if err != nil {
		return gate.AggregateEnvelope{}, "", err
	}
	// hypothesis.Render only emits statements from its own templates, so this
	// cannot fail — which is exactly why it is called. §12.2 asks for the parse
	// gate on the path to the database, not on the paths thought likely to
	// carry something bad.
	if err := sqlguard.Check(query); err != nil {
		return gate.AggregateEnvelope{}, "", err
	}

	db, err := c.Open()
	if err != nil {
		return gate.AggregateEnvelope{}, "", err
	}
	defer db.Close()

	opts := a.Gate
	opts.Log = a.logger()
	opts.FreeTextColumns = freeTextColumns(c)

	env, err := gate.Aggregate(ctx, db, query, opts)
	if err != nil {
		return gate.AggregateEnvelope{}, "", err
	}
	return env, fmt.Sprintf("connector:%s#%s", c.Name, env.QueryHash[:16]), nil
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

// capUnsupported applies §4's statistical-validity check to what was mined.
//
// The rule is deliberately mechanical. Deciding whether a sentence ASSERTS the
// difference the test failed to find would take another model call and would be
// wrong sometimes in both directions; capping every claim mined from an
// unsupported comparison is blunt, cheap, and cannot be argued with.
//
// A query with no comparison in it — a distribution, an overview — is left
// alone. There is no test to fail, and capping those would punish the claims
// that are simply counts.
func capUnsupported(claims []core.Claim, env gate.AggregateEnvelope) []core.Claim {
	if len(env.TestResults) == 0 {
		return claims
	}
	for _, t := range env.TestResults {
		if t.Verdict == stats.Significant {
			return claims
		}
	}
	for i := range claims {
		if claims[i].AssertionStrength > unsupportedAssertionCap {
			claims[i].AssertionStrength = unsupportedAssertionCap
		}
	}
	return claims
}

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
	prompt := planPrompt(fence, lead.Query, sources, max)

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
