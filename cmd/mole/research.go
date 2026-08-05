package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/cache"
	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/executor"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/output"
	"github.com/lajosdeme/mole/internal/planner"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/queue"
	"github.com/lajosdeme/mole/internal/record"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/tools/search"
	"github.com/lajosdeme/mole/internal/verifier"
	"github.com/spf13/cobra"
)

// mole research
//
// The planner loop (§9.2) driven from a terminal: decompose the question, work a
// queue of leads, replan against a rolling digest as evidence arrives, and
// synthesize a cited report from escrow held back at session start.
//
// It runs in-process and holds the database's single writer for its lifetime.
// That is the same position the daemon will take in M7; until then there is
// nothing to attach to, so Ctrl-C settles what was spent and stops rather than
// detaching.

// researchUsage is the prose cobra cannot generate. The flag list is
// deliberately absent: it is derived from the definitions below, so it cannot
// drift out of step with them the way the hand-written one did.
const researchUsage = `Run one research question end to end.

Budget is required and the two units are mutually exclusive — pass --usd N or
--tokens N, or set a default with ` + "`mole config set default.usd`" + `. There is
deliberately no bare --budget: a number alone is ambiguous between the two, and
§8 makes the unit load-bearing. Only USD mode can price a search call, and only
token mode can bound a model the pricing table does not know.

The question is decomposed into sub-questions, each researched as its own lead,
and the planner replans against a rolling digest as evidence arrives. The report
is paid from escrow held back at session start, so a run that spends everything
it is allowed can still afford to write up what it found.

--mode accepts only "report"; dataset and chain arrive with their milestones.
`

// researchOpts is what the flags resolve to.
type researchOpts struct {
	usd         string
	tokens      int64
	mode        string
	maxSources  int
	timeout     time.Duration
	asJSON      bool
	quiet       bool
	alwaysFetch bool
	maxDepth    int
	dbPath      string

	// silent suppresses ALL output, including the report.
	//
	// Distinct from quiet, which suppresses progress and still prints the answer — that
	// is what a person running one question wants. The corpus runner produces its own
	// output and a hundred inlined reports would bury it. No flag: nothing on the
	// command line should ask mole to research a question and say nothing.
	silent bool

	// onSession, when set, is called with the session id as soon as it exists.
	//
	// A callback rather than a return value because the corpus runner needs the id even
	// when the run goes on to fail — a session that errored still has a ledger to
	// reconcile and claims to score, and those are exactly the runs worth looking at.
	// Nil for the CLI.
	onSession func(string)
}

func newResearchCmd() *cobra.Command {
	var o researchOpts

	c := &cobra.Command{
		Use:   "research <question>",
		Short: "Run one research question end to end",
		Long:  researchUsage,
		Args:  minArgs(1, `mole research "<question>" (--usd N | --tokens N)`),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.dbPath = dbPath(cmd)
			return cmdResearch(cmd.Context(), strings.Join(args, " "), o)
		},
	}

	f := c.Flags()
	f.StringVar(&o.usd, "usd", "", "budget in dollars, e.g. --usd 3.00")
	f.Int64Var(&o.tokens, "tokens", 0, "budget in tokens")
	f.StringVar(&o.mode, "mode", string(core.ModeReport), "session mode")
	f.IntVar(&o.maxSources, "max-sources", 5, "sources to read per lead")
	f.DurationVar(&o.timeout, "timeout", 5*time.Minute, "wall-clock ceiling for the whole session")
	f.BoolVar(&o.asJSON, "json", false, "emit the result as JSON")
	f.BoolVar(&o.quiet, "quiet", false, "suppress progress; print only the result")
	f.IntVar(&o.maxDepth, "max-depth", planner.DefaultMaxDepth,
		"rounds of follow-up leads the planner may add (0 for none)")
	f.BoolVar(&o.alwaysFetch, "always-fetch", false,
		"fetch every page even when the search provider supplied its text (slower; required for citation accuracy and the §17.1 gate)")
	return c
}

func cmdResearch(ctx context.Context, rawQuestion string, o researchOpts) error {
	question := strings.TrimSpace(rawQuestion)
	if question == "" {
		return errors.New("no question given")
	}

	cfg, err := config.Load()
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		return err
	}

	unit, amount, err := resolveBudget(o.usd, o.tokens, cfg)
	if err != nil {
		return err
	}
	sessionMode := core.Mode(o.mode)
	if !sessionMode.Valid() {
		return fmt.Errorf("unknown mode %q", o.mode)
	}
	if sessionMode != core.ModeReport {
		return fmt.Errorf("mode %q is not implemented yet (M3 for report+, M9 for dataset)", o.mode)
	}

	// One cassette per question (§14.1). Off unless MOLE_RECORD says otherwise,
	// so this costs nothing in normal use.
	rec, err := record.FromEnv(question)
	if err != nil {
		return err
	}
	defer func() {
		if err := rec.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save cassette: %v\n", err)
		}
	}()

	// Build the actor before touching the database. A missing search key should
	// fail in under a second, not after creating a session that can never run.
	actor, err := buildWebActor(cfg, rec, o.maxSources, o.alwaysFetch, o.quiet)
	if err != nil {
		return err
	}
	if unit == core.BudgetUSD {
		if err := checkUSDIsEnforceable(actor.LLM); err != nil {
			return err
		}
	}
	if rec.Enabled() && !o.quiet && !o.asJSON {
		fmt.Printf("cassette %s (%s)\n", rec.Path, rec.Mode)
	}

	db, err := openDBWrite(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// The context gets headroom over the wall-clock CEILING so the two do not
	// fire together. When they do, the loop is killed mid-lead instead of
	// stopping at its own check, and the run ends in a cascade of "context
	// deadline exceeded" from whatever was in flight — a persist, a settle, a
	// count — rather than a clean "stopped: max_wallclock".
	ctx, cancel := context.WithTimeout(ctx, o.timeout+30*time.Second)
	defer cancel()

	led := budget.New(db, budget.DefaultConfig())
	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt:     question,
		Mode:       sessionMode,
		ActorTypes: []core.ActorType{core.ActorWeb},
		BudgetUnit: unit,
		Budget:     amount,
		// Unit-independent ceilings (§8.5). They bind even when the money
		// estimate is wrong, which is the case they exist for.
		// Sized from what the planner can actually produce: an initial fan-out
		// plus one round of follow-ups per depth level, with headroom. A fixed
		// 1 was left over from M1, when the CLI ran exactly one lead — the
		// ceiling worked, which is how the stale value surfaced.
		MaxToolCalls: int64(maxLeadsFor(o)) * int64(o.maxSources) * 4,
		MaxLeads:     int64(maxLeadsFor(o)),
		MaxWallClock: o.timeout,
	})
	if o.onSession != nil && sess != nil {
		o.onSession(sess.ID)
	}
	if err != nil {
		return err
	}
	actor.SessionID = sess.ID
	actor.Store = db

	out := &researchOutput{Question: question, SessionID: sess.ID, Unit: string(unit), Budget: amount}

	if !o.quiet && !o.asJSON {
		fmt.Printf("session  %s   mode=%s  budget=%s (escrow %s held, §8.3)\n\n",
			sess.ID, sessionMode, fmtAmount(unit, sess.Budget), fmtAmount(unit, sess.Escrow))
		if rec.Enabled() {
			fmt.Printf("cassette %s (%s)\n\n", rec.Path, rec.Mode)
		}
		fmt.Println(" executing ─────────────────────────────────────────")
	}

	// Recover what a previous crash left behind (§9.4). Both halves: leases,
	// and the reservations those leads were holding — a stale hold is budget
	// neither spent nor available, and Ledger.SweepExpired had no caller at all,
	// so a crash mid-lead understated a session's Available() permanently.
	//
	// Unscoped on purpose: boot recovery does not know which sessions were in
	// flight, and this is the one caller for which that is correct.
	q := queue.New(db, 0)
	if n, err := q.Sweep(ctx, ""); err != nil {
		fmt.Fprintf(os.Stderr, "warning: lease sweep failed: %v\n", err)
	} else if n > 0 && !o.quiet {
		fmt.Printf(" recovered %d lead(s) stranded by a previous run\n", n)
	}
	if n, err := led.SweepExpired(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: reservation sweep failed: %v\n", err)
	} else if n > 0 && !o.quiet {
		fmt.Printf(" released %d stale reservation(s) from a previous run\n", n)
	}
	if n, err := led.SweepAbandonedSessions(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: session sweep failed: %v\n", err)
	} else if n > 0 && !o.quiet {
		fmt.Printf(" closed %d session(s) abandoned by a previous run\n", n)
	}

	// One cache shared between the loop and the actor, so a lead-level hit and
	// a URL-level hit are the same cache and one lead's fetches serve another's.
	sessionCache := cache.New()
	actor.Cache = sessionCache

	// The Verifier shares the actor's provider, store and fetcher. §11.1 wants it
	// over the session's whole claim set, which is why it reads the store rather
	// than being handed each lead's batch — and the fetcher is the actor's own, so
	// §11.5's re-read goes through the same robots handling, rate limiter and SSRF
	// guard as the fetch that produced the claim.
	vf := &verifier.Verifier{
		Store:  db,
		Ledger: led,
		LLM:    actor.LLM,
		Log:    actor.Log,
		// Empty leaves the cheap model in place; set it when the cheap model cannot
		// tell a contradiction from two unrelated statements.
		Model: cfg.LLM.VerifierModel,
		// Zero takes the default. Lower it when a slow model cannot finish eight pair
		// judgements inside one call: batching is what makes verification affordable,
		// but the batch is also the unit that has to fit in the client timeout.
		BatchSize: cfg.LLM.VerifierBatchSize,
		Grounder:  &verifier.Grounder{Fetch: actor.Fetch, Extract: actor.Extract},
	}

	exec := &executor.Executor{
		Store:      db,
		Ledger:     led,
		Queue:      q,
		Cache:      sessionCache,
		Pricing:    actor.Pricing,
		CheapModel: actor.LLM.ModelFor(llm.TierCheap),
		Planner:    &planner.Planner{LLM: actor.LLM, MaxDepth: o.maxDepth},
		Verifier:   vf,
		Actors:     map[core.ActorType]actors.Actor{core.ActorWeb: actor},
		Log:        actor.Log,
		Owner:      "cli",
	}
	if !o.quiet && !o.asJSON {
		exec.Progress = progressPrinter()
	}

	runRes, runErr := exec.Run(ctx, sess.ID)
	if runRes != nil {
		out.ClaimsVerified = runRes.ClaimsVerified
		out.Contradictions = runRes.Contradictions
		out.LeadsRun = runRes.LeadsRun
		out.LeadsFailed = runRes.LeadsFailed
		out.LeadsCached = runRes.LeadsCached
		out.Replans = runRes.Replans
		out.CacheHits = runRes.CacheStats.Hits
		out.Claims = runRes.Claims
		out.StoppedBecause = runRes.StoppedBecause
		if runRes.Digest != nil {
			out.OpenQuestions = len(runRes.Digest.Open())
		}
		if !o.quiet && !o.asJSON {
			printProgress(runRes)
		}
	}
	if runErr != nil {
		out.Error = runErr.Error()
	}

	// The report is paid from escrow (§8.3). Releasing it here is what makes
	// the money set aside at session creation spendable — a run that produced
	// good claims and could not afford to write them up would have wasted the
	// whole budget, not just the last call.
	report := generateReport(ctx, db, led, actor, vf, sess, out, o)

	status := core.StatusDone
	if runRes != nil {
		status = runRes.Status
	}
	if runErr != nil && status == core.StatusDone {
		status = core.StatusFailed
	}
	if ferr := led.Finish(context.WithoutCancel(ctx), sess.ID, status); ferr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not finalize session: %v\n", ferr)
	}
	out.Status = string(status)

	// The executor already read this with a live context, which is the whole
	// reason that read exists — re-reading here duplicated it.
	if runRes != nil {
		out.Spent = runRes.Spent
	}

	if o.silent {
		return nil
	}
	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		printReport(out, report, unit, sess.Budget, o.quiet)
	}

	if runErr != nil {
		return runErr
	}
	if status == core.StatusFailed {
		return fmt.Errorf("session ended %s: %s", status, out.StoppedBecause)
	}
	return nil
}

// generateReport synthesizes the answer, charged against released escrow.
//
// Never fatal. A failed synthesis costs the prose, not the evidence: the
// generator falls back to listing verified claims with their citations, which
// is what the escrow already paid to collect.
func generateReport(
	ctx context.Context,
	db store.Store,
	led *budget.Ledger,
	actor *actors.WebActor,
	vf *verifier.Verifier,
	sess *core.Session,
	out *researchOutput,
	o researchOpts,
) *output.Report {
	ctx = context.WithoutCancel(ctx)

	released, err := led.ReleaseEscrow(ctx, sess.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not release escrow: %v\n", err)
	}

	// §11.5.2's grounding pass, between the escrow release and the report.
	//
	// Here rather than inside the loop for two reasons. Its candidates are the
	// claims the REPORT will lean on, which is only knowable once research has
	// stopped and the graph is complete. And §11.5 puts its spend on the escrow —
	// which is exactly the money just released, so it takes a bounded share and
	// leaves the rest for the answer. A grounding pass that spent the escrow would
	// produce a well-checked set of claims and no report to put them in.
	groundReport := runGrounding(ctx, vf, sess.ID, released, out, o)

	// Reserve BEFORE generating. The order used to be release → generate →
	// reserve, which inverts §8.2 and had a concrete failure: after any research
	// overshoot the reserve was refused, and the report tokens — already spent —
	// were never written to the ledger. §8.1 says every tool call writes a cost
	// row, and Verify() still reconciled because the row never existed.
	//
	// Bounded by what is actually available, not by what escrow nominally
	// released: an overshoot may already have eaten into it.
	amount := released
	if groundReport != nil {
		amount -= groundReport.Spent
	}
	if sess, err := loadSession(ctx, db, sess.ID); err == nil {
		if avail := sess.Available(); amount > avail {
			amount = avail
		}
	}

	gen := &output.Generator{LLM: actor.LLM}
	var reservation *core.Reservation
	if amount > 0 {
		reservation, err = led.ReserveOutput(ctx, sess.ID, amount)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not reserve for the report: %v\n", err)
		}
	}
	if reservation == nil {
		// Nothing to spend. Emit the evidence without prose rather than making
		// a call whose cost cannot be recorded.
		gen.LLM = nil
	}

	report, err := gen.Generate(ctx, db, sess.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: report generation failed: %v\n", err)
		if reservation != nil {
			if rerr := led.Release(ctx, reservation); rerr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not release the report hold: %v\n", rerr)
			}
		}
		return nil
	}

	if reservation != nil {
		// Settle unconditionally, including a zero cost: an unresolved hold is
		// budget neither spent nor available.
		var calls []core.ToolCall
		if !report.Cost.IsZero() {
			calls = append(calls, core.ToolCall{
				SessionID: sess.ID,
				Role:      core.RoleOutput,
				Type:      core.CallLLM,
				Model:     report.Model,
				Input:     "report",
				Cost:      priceReport(actor, report),
			})
		}
		if _, serr := led.Settle(ctx, reservation, calls); serr != nil {
			fmt.Fprintf(os.Stderr, "warning: settling the report failed: %v\n", serr)
		}
	}

	out.Report = report.Markdown()
	out.Degraded = report.Degraded
	return report
}

// priceReport turns the generator's token usage into a ledger cost.
func priceReport(actor *actors.WebActor, r *output.Report) core.Cost {
	table := actor.Pricing
	if table == nil {
		table = pricing.NewTable()
	}
	cost, err := table.Cost(r.Model, pricing.Usage{
		InputTokens:      r.Cost.InputTokens,
		OutputTokens:     r.Cost.OutputTokens,
		CacheReadTokens:  r.Cost.CacheReadTokens,
		CacheWriteTokens: r.Cost.CacheWriteTokens,
	})
	if err != nil {
		// An unpriced model still spent tokens. Recording zero dollars but real
		// tokens keeps token-mode budgets correct and lets doctor surface the
		// gap, rather than silently inflating or dropping the charge.
		cost = core.Cost{
			InputTokens:      r.Cost.InputTokens,
			OutputTokens:     r.Cost.OutputTokens,
			CacheReadTokens:  r.Cost.CacheReadTokens,
			CacheWriteTokens: r.Cost.CacheWriteTokens,
		}
	}
	return cost
}

// maxLeadsFor bounds the lead tree from the planner's own fan-out limits, so
// the ceiling and the plan cannot disagree.
// plannerDepth translates the flag into the planner's encoding, where zero
// means "unset" and DepthNone means "no follow-ups".
func plannerDepth(flag int) int {
	if flag <= 0 {
		return planner.DepthNone
	}
	return flag
}

func maxLeadsFor(o researchOpts) int {
	depth := o.maxDepth
	if depth < 0 {
		depth = 0
	}
	return planner.DefaultMaxInitialLeads + depth*planner.DefaultMaxNewLeadsPerReplan + 2
}

func loadSession(ctx context.Context, db store.Store, id string) (*core.Session, error) {
	var s *core.Session
	err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		s, err = q.GetSession(ctx, id)
		return err
	})
	return s, err
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

func buildWebActor(cfg *config.Config, rec *record.Recorder, maxSources int, alwaysFetch, quiet bool) (*actors.WebActor, error) {
	if cfg.Search.Provider == "" {
		return nil, errors.New("no search provider selected (run: mole config set search.provider brave|tavily)")
	}
	if cfg.Search.ActiveKey() == "" {
		return nil, fmt.Errorf("no API key for %s (run: mole config set search.%s-key ...)",
			cfg.Search.Provider, cfg.Search.Provider)
	}
	provider, err := search.New(search.Config{
		Provider:           search.Kind(cfg.Search.Provider),
		APIKey:             cfg.Search.ActiveKey(),
		CostPerQueryMicros: cfg.Search.CostPerQueryMicros,
	}, rec.Client())
	if err != nil {
		return nil, err
	}

	model, _, err := buildLLMWithClient(cfg, rec.Client())
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, errors.New("no LLM provider found — set one with `mole config set llm.api-key ...`, " +
			"run `ant auth login`, or start a local model (ollama serve)")
	}

	level := slog.LevelWarn
	if quiet {
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	return &actors.WebActor{
		Search: provider,
		// Wrap rather than replace: the fetcher's transport carries the egress
		// guard's DialContext, and handing it a plain client would bypass SSRF
		// protection to get determinism. Wrapped, a recording still passes the
		// guard and a replay never opens a socket.
		Fetch: fetch.NewHTTP(fetch.Config{UserAgent: userAgent()}, fetch.Options{
			Log:       log,
			Transport: rec.Wrap,
		}),
		Extract: extract.New(),
		LLM:     model,
		Pricing: pricing.NewTable(),
		Log:     log,
		Budget: actors.Budget{
			MaxSources:         maxSources,
			MaxClaimsPerSource: 8,
			MaxChunkTokens:     cfg.LLM.MaxInputTokens,
			AlwaysFetch:        alwaysFetch,
		},
	}, nil
}

// checkUSDIsEnforceable refuses a dollar budget the ledger cannot enforce.
//
// An unpriced model records zero dollars per call, so --usd names a ceiling
// nothing counts against: the run spends whatever it spends and reports a few
// cents of search cost. Budget is this project's first-class primitive (§8),
// and a ceiling that silently does not bind is worse than no ceiling, because
// the number printed at the end looks like it held.
//
// doctor already warns about this. Warning is not enough at the point where
// money is about to be spent under a limit that does not exist.
func checkUSDIsEnforceable(p llm.Provider) error {
	missing := unpricedModelList(p)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"--usd cannot bound this run: no price is registered for %s, so every model call "+
			"would be ledgered at $0.00 and the ceiling would never bind.\n"+
			"Use --tokens N instead, which counts what the provider reports.",
		strings.Join(missing, ", "))
}

func userAgent() string {
	// An anonymous crawler is impolite and gives an operator no way to contact
	// anyone or block selectively (§10.2).
	return "mole/" + version + " (+https://github.com/lajosdeme/mole)"
}

// resolveBudget turns the flags into a unit and an amount.
//
// Mutually exclusive by construction. §8 makes the unit semantically
// load-bearing — USD mode cannot bound an unpriced model, token mode cannot
// price a search call — so guessing one would produce a ceiling that silently
// does not bind.
func resolveBudget(usd string, tokens int64, cfg *config.Config) (core.BudgetUnit, int64, error) {
	switch {
	case usd != "" && tokens > 0:
		return "", 0, errors.New("--usd and --tokens are mutually exclusive")

	case usd != "":
		micros, err := core.ParseUSD(usd)
		if err != nil {
			return "", 0, fmt.Errorf("--usd: %w", err)
		}
		if micros <= 0 {
			return "", 0, errors.New("--usd must be positive")
		}
		return core.BudgetUSD, micros, nil

	case tokens > 0:
		return core.BudgetTokens, tokens, nil
	}

	// Fall back to a configured default, so a bare `mole research` still cannot
	// run away (§8.5).
	switch core.BudgetUnit(cfg.DefaultBudgetUnit) {
	case core.BudgetUSD:
		micros, err := core.ParseUSD(cfg.DefaultBudgetUSD)
		if err != nil || micros <= 0 {
			return "", 0, errors.New("default_budget_usd is not a valid amount (run: mole config set default.usd 3.00)")
		}
		return core.BudgetUSD, micros, nil
	case core.BudgetTokens:
		if cfg.DefaultBudgetToks <= 0 {
			return "", 0, errors.New("default_budget_tokens is not set (run: mole config set default.tokens 50000)")
		}
		return core.BudgetTokens, cfg.DefaultBudgetToks, nil
	}

	return "", 0, errors.New("no budget given: pass --usd N or --tokens N, or set a default " +
		"(mole config set default.unit usd; mole config set default.usd 3.00)")
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

type researchOutput struct {
	Question  string `json:"question"`
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Unit      string `json:"budget_unit"`
	Budget    int64  `json:"budget"`
	Spent     int64  `json:"spent"`

	LeadsRun      int `json:"leads_run"`
	LeadsFailed   int `json:"leads_failed"`
	LeadsCached   int `json:"leads_cached"`
	CacheHits     int `json:"cache_hits"`
	Replans       int `json:"replans"`
	OpenQuestions int `json:"open_questions"`

	// Verification and grounding (§11).
	ClaimsVerified    int `json:"claims_verified"`
	Contradictions    int `json:"contradictions"`
	GroundChecked     int `json:"ground_checked"`
	GroundConfirmed   int `json:"ground_confirmed"`
	GroundUnsupported int `json:"ground_unsupported"`

	Report string       `json:"report"`
	Claims []core.Claim `json:"claims"`

	StoppedBecause string `json:"stopped_because,omitempty"`
	Degraded       string `json:"degraded,omitempty"`
	Error          string `json:"error,omitempty"`
}

// progressPrinter reports each phase as it happens, with elapsed time.
//
// Live feedback, not decoration. Planning is one model call that produces no
// output until it returns, and on a local model that is minutes of silence after
// a one-line header — which reads as a hang. Three real runs were killed by hand
// before reaching the first lead.
func progressPrinter() func(executor.Event) {
	start := time.Now()
	return func(ev executor.Event) {
		el := time.Since(start).Round(time.Second)
		switch ev.Phase {
		case "planning":
			fmt.Printf(" [%6s] planning: %s…\n", el, ev.Detail)
		case "executing":
			fmt.Printf(" [%6s] plan ready: %s\n", el, ev.Detail)
		case "replanning":
			fmt.Printf(" [%6s] replanning: %s…\n", el, ev.Detail)
		case "lead":
			fmt.Printf(" [%6s]   → %s\n", el, ev.Detail)
		case "lead-done":
			fmt.Printf(" [%6s]     %s\n", el, ev.Detail)
		case "cached":
			fmt.Printf(" [%6s]   ⤿ cached: %s\n", el, ev.Detail)
		case "verifying":
			fmt.Printf(" [%6s] verifying: %s…\n", el, ev.Detail)
		case "verified":
			fmt.Printf(" [%6s]     graph: %s\n", el, ev.Detail)
		case "follow-up":
			fmt.Printf(" [%6s]   ⚡ %s\n", el, ev.Detail)
		}
	}
}

// printProgress shows what the loop did, in the shape §18.3 sketches.
func printProgress(res *executor.Result) {
	mark := "✓"
	switch {
	case res.LeadsRun == 0:
		mark = "⚠"
	case res.LeadsFailed*2 > res.LeadsRun:
		// More than half the leads failed. The run produced something, but not
		// the something it was asked for (§9.5 degraded).
		mark = "~"
	}
	fmt.Printf(" %s %d lead(s) run, %d failed  ·  %d replan(s)  ·  %d claim(s)\n",
		mark, res.LeadsRun, res.LeadsFailed, res.Replans, len(res.Claims))
	if res.LeadsCached > 0 || res.CacheStats.Hits > 0 {
		fmt.Printf("   cache: %d lead(s) reused, %d source(s) served without a fetch\n",
			res.LeadsCached, res.CacheStats.Hits)
	}
	if res.VerifyPasses > 0 {
		fmt.Printf("   graph: %d claim(s) verified, %d edge(s), %d contradiction(s)",
			res.ClaimsVerified, res.EdgesWritten, res.Contradictions)
		if res.FollowUpsQueued > 0 {
			fmt.Printf(", %d follow-up lead(s)", res.FollowUpsQueued)
		}
		fmt.Println()
		if res.VerifyDegraded != "" {
			// Said out loud. An unverified claim keeps confidence 0, and a reader
			// who does not know verification was cut short reads that as "nothing
			// here is trustworthy" rather than "nothing here was checked".
			fmt.Printf("   verification incomplete: %s\n", res.VerifyDegraded)
		}
	} else if len(res.Claims) > 0 {
		fmt.Println("   graph: not built — claims carry verified quotes but no derived confidence")
	}
	if res.StoppedBecause != "" {
		fmt.Printf("   stopped: %s\n", res.StoppedBecause)
	}
}

func printReport(out *researchOutput, report *output.Report, unit core.BudgetUnit, budgetAmt int64, quiet bool) {
	if !quiet {
		fmt.Println()
	}
	if report != nil {
		fmt.Println(report.Markdown())
	} else if out.Report != "" {
		fmt.Println(out.Report)
	}

	fmt.Printf(" spent %s / %s  ·  %d lead(s), %d failed  ·  %d replan(s)  ·  %d claim(s)\n",
		fmtAmount(unit, out.Spent), fmtAmount(unit, budgetAmt),
		out.LeadsRun, out.LeadsFailed, out.Replans, len(out.Claims))

	if out.OpenQuestions > 0 {
		// The planner stopped with questions still open. Saying so is the
		// difference between "this is the answer" and "this is what fit the
		// budget", which a reader has to be able to tell apart.
		fmt.Printf(" %d sub-question(s) still open when the run stopped\n", out.OpenQuestions)
	}
	if out.StoppedBecause != "" {
		fmt.Printf(" stopped: %s\n", out.StoppedBecause)
	}
	if out.Error != "" {
		fmt.Printf(" error: %s\n", out.Error)
	}
	fmt.Printf("\n mole eval %s        for the scorecard\n", out.SessionID)
	fmt.Printf(" mole trace %s       for the per-call cost breakdown\n", out.SessionID)
}

func fmtAmount(unit core.BudgetUnit, amount int64) string {
	if unit == core.BudgetTokens {
		return fmt.Sprintf("%d tok", amount)
	}
	return core.FormatUSD(amount)
}

// runGrounding performs §11.5.2's budgeted re-fetch pass and reports it.
//
// Bounded by a share of the released escrow, so the report stays affordable. Never
// fails the run: a fetch that failed, a page that changed, or a judge that would not
// answer are outcomes worth recording, not reasons to withhold a report.
func runGrounding(
	ctx context.Context,
	vf *verifier.Verifier,
	sessionID string,
	releasedEscrow int64,
	out *researchOutput,
	o researchOpts,
) *verifier.GroundReport {
	if vf == nil || vf.Grounder == nil || releasedEscrow <= 0 {
		return nil
	}
	allowance := int64(float64(releasedEscrow) * verifier.DefaultGroundShareOfEscrow)
	if allowance <= 0 {
		return nil
	}

	rep, err := vf.Ground(ctx, sessionID, allowance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: grounding pass failed: %v\n", err)
		return nil
	}
	if rep == nil {
		return nil
	}
	out.GroundChecked = rep.Checked
	out.GroundConfirmed = rep.Confirmed
	out.GroundUnsupported = rep.Unsupported

	if !o.quiet && !o.asJSON {
		reportGrounding(rep, o)
	}
	return rep
}

// reportGrounding prints what the pass did, including when it did nothing.
//
// Split out so the "did nothing" branches are testable without a store or a network —
// they are the ones that were silently wrong twice.
func reportGrounding(rep *verifier.GroundReport, o researchOpts) {
	if rep.Checked == 0 {
		switch {
		case rep.NotFetchable > 0:
			fmt.Printf("   grounding: not run — %d claim(s) cite pages the search provider "+
				"supplied, which cannot be re-read without comparing two different "+
				"extractions\n", rep.NotFetchable)
		case rep.Degraded != "":
			fmt.Printf("   grounding: not run — %s\n", rep.Degraded)
		default:
			fmt.Println("   grounding: no claim was eligible for a re-read")
		}
		return
	}

	fmt.Printf("   grounding: %d claim(s) re-read — %d confirmed, %d unsupported",
		rep.Checked, rep.Confirmed, rep.Unsupported)
	if rep.NotFetchable > 0 {
		fmt.Printf(", %d not re-readable", rep.NotFetchable)
	}
	if n := rep.Vanished + rep.Unreachable + rep.Undecided; n > 0 {
		// Said separately: none of these is a verdict about a claim. The page changed,
		// the host was down, or the judge would not answer. Folding them into
		// "unsupported" would penalize a claim for someone else's edit.
		fmt.Printf(", %d inconclusive", n)
	}
	fmt.Println()
	for _, r := range rep.Results {
		if r.Outcome == verifier.GroundUnsupported {
			// The one outcome a reader must see: a claim whose own source does not
			// support it is invisible in a confidence number.
			fmt.Printf("     ⚠ %.70s\n", r.Note)
		}
	}
}
