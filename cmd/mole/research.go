package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/record"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/extract"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/tools/search"
)

// mole research
//
// This is M1's deliverable made visible: one lead, run end to end, inside a
// real reservation. There is no planner until M3, so the question becomes a
// single web lead verbatim rather than a plan — and the output says so, because
// a command that printed "planning" over a hardcoded single lead would be
// describing a system that does not exist yet.
//
// It runs in-process and holds the database's single writer for its lifetime.
// That is the same position the daemon will take in M7; until then there is
// nothing to attach to, so Ctrl-C settles what was spent and stops rather than
// detaching.

const researchUsage = `mole research — run one research question end to end

Usage:
  mole research "<question>" (--usd N | --tokens N) [flags]

Budget (exactly one, or a configured default):
  --usd N          Spend at most N dollars, e.g. --usd 3.00
  --tokens N       Spend at most N tokens

Flags:
  --mode MODE      report (default). dataset/chain/ask arrive with their milestones
  --max-sources N  Sources to read for this lead (default 5)
  --timeout D      Wall-clock ceiling for the whole session (default 5m)
  --json           Emit the result as JSON instead of a report
  --quiet          Suppress progress; print only the result
  --db PATH        Database path

Budget flags are mutually exclusive on purpose. A bare number is ambiguous
between dollars and tokens, and §8 makes the unit load-bearing: only USD mode
can price a search call, and only token mode works with an unpriced model.
`

func cmdResearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("research", flag.ContinueOnError)
	fs.Usage = func() { fmt.Print(researchUsage) }
	dbPath := addDBFlag(fs)
	usd := fs.String("usd", "", "budget in dollars")
	tokens := fs.Int64("tokens", 0, "budget in tokens")
	mode := fs.String("mode", string(core.ModeReport), "session mode")
	maxSources := fs.Int("max-sources", 5, "sources to read")
	timeout := fs.Duration("timeout", 5*time.Minute, "wall-clock ceiling")
	asJSON := fs.Bool("json", false, "emit JSON")
	quiet := fs.Bool("quiet", false, "suppress progress")
	if err := parseArgs(fs, args); err != nil {
		return err
	}

	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		fmt.Print(researchUsage)
		return errors.New("no question given")
	}

	cfg, err := config.Load()
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		return err
	}

	unit, amount, err := resolveBudget(*usd, *tokens, cfg)
	if err != nil {
		return err
	}
	sessionMode := core.Mode(*mode)
	if !sessionMode.Valid() {
		return fmt.Errorf("unknown mode %q", *mode)
	}
	if sessionMode != core.ModeReport {
		return fmt.Errorf("mode %q is not implemented yet (M3 for report+, M9 for dataset)", *mode)
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
	actor, err := buildWebActor(cfg, rec, *maxSources, *quiet)
	if err != nil {
		return err
	}
	if unit == core.BudgetUSD {
		if err := checkUSDIsEnforceable(actor.LLM); err != nil {
			return err
		}
	}
	if rec.Enabled() && !*quiet && !*asJSON {
		fmt.Printf("cassette %s (%s)\n", rec.Path, rec.Mode)
	}

	db, err := openDBWrite(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, *timeout)
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
		MaxToolCalls: int64(*maxSources)*4 + 8,
		MaxLeads:     1,
		MaxWallClock: *timeout,
	})
	if err != nil {
		return err
	}
	actor.SessionID = sess.ID
	actor.Store = db

	out := &researchOutput{Question: question, SessionID: sess.ID, Unit: string(unit), Budget: amount}

	if !*quiet && !*asJSON {
		fmt.Printf("session  %s   mode=%s  budget=%s (escrow %s held, §8.3)\n\n",
			sess.ID, sessionMode, fmtAmount(unit, sess.Budget), fmtAmount(unit, sess.Escrow))
		fmt.Println(" executing ─────────────────────────────────────────")
	}

	settled, runErr := runOneLead(ctx, db, led, actor, sess, question, out, *quiet || *asJSON)

	// Settle first, report second. The session's final status has to reflect
	// what the ledger says, and the ledger is only correct once the reservation
	// is resolved — including when the run failed.
	status := core.StatusDone
	switch {
	case errors.Is(runErr, context.DeadlineExceeded):
		status = core.StatusExhausted
	case errors.Is(runErr, context.Canceled):
		status = core.StatusCancelled
	case runErr != nil:
		status = core.StatusFailed
	}
	if ferr := led.Finish(context.WithoutCancel(ctx), sess.ID, status); ferr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not finalize session: %v\n", ferr)
	}
	out.Status = string(status)
	if settled != nil {
		out.Spent = settled.Charged
		out.Cost = settled.Cost
	}
	if runErr != nil {
		out.Error = runErr.Error()
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		printReport(out, unit, sess.Budget, *quiet)
	}

	// A run that hit a ceiling is not a crash, but it is not a success either;
	// a caller scripting this needs to be able to tell.
	if runErr != nil {
		return runErr
	}
	if status != core.StatusDone {
		return fmt.Errorf("session ended %s", status)
	}
	return nil
}

// runOneLead reserves, runs, and settles. This is the executor's job in M5; the
// shape is deliberately the same so turning on workers is configuration rather
// than a rewrite.
func runOneLead(
	ctx context.Context,
	db store.Store,
	led *budget.Ledger,
	actor actors.Actor,
	sess *core.Session,
	question string,
	out *researchOutput,
	quiet bool,
) (*budget.SettleResult, error) {
	lead := core.Lead{
		ID:        core.NewLeadID(),
		SessionID: sess.ID,
		ActorType: core.ActorWeb,
		Query:     question,
		Status:    core.LeadQueued,
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return tx.InsertLead(ctx, &lead)
	}); err != nil {
		return nil, err
	}

	estimate := budget.NewEstimator(sess.BudgetUnit).For(core.ActorWeb, 0)
	if avail := sess.Available(); estimate > avail {
		// Reserve what is actually left rather than failing: a small budget
		// should buy a small run, not an error.
		estimate = avail
	}
	if estimate <= 0 {
		return nil, fmt.Errorf("budget %s leaves nothing to spend after escrow",
			fmtAmount(sess.BudgetUnit, sess.Budget))
	}

	res, err := led.ReserveFor(ctx, sess.ID, lead.ID, estimate)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	result, runErr := actor.Run(ctx, lead)

	// Settle whatever was spent, even on failure. The tokens were billed either
	// way, and a ledger of successes cannot enforce a ceiling.
	var calls []core.ToolCall
	if result != nil {
		calls = result.Costs
	}
	settled, serr := led.Settle(context.WithoutCancel(ctx), res, calls)
	if serr != nil {
		return nil, errors.Join(runErr, fmt.Errorf("settle: %w", serr))
	}

	if result != nil {
		out.Summary = result.Summary
		out.Claims = result.Claims
		out.Stats = result.Stats
		out.Truncated = result.Truncated
		if !quiet {
			printLeadLine(question, result, settled, sess.BudgetUnit, time.Since(started))
		}
	}
	if settled.Flagged {
		fmt.Fprintf(os.Stderr, "warning: lead cost %s against a %s reservation (§8.4)\n",
			fmtAmount(sess.BudgetUnit, settled.Charged), fmtAmount(sess.BudgetUnit, settled.Reserved))
	}
	return &settled, runErr
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

func buildWebActor(cfg *config.Config, rec *record.Recorder, maxSources int, quiet bool) (*actors.WebActor, error) {
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

	model, reason, err := buildLLMWithClient(cfg, rec.Client())
	if err != nil {
		return nil, err
	}
	if model == nil {
		return nil, errors.New("no LLM provider found — set one with `mole config set llm.api-key ...`, " +
			"run `ant auth login`, or start a local model (ollama serve)")
	}
	_ = reason

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
	if warn := unpricedModels(p); warn == "" {
		return nil
	}
	table := pricing.NewTable()
	var missing []string
	for _, m := range []string{p.ModelFor(llm.TierStrong), p.ModelFor(llm.TierCheap)} {
		if m == "" {
			continue
		}
		if _, ok := table.Lookup(m); !ok && !contains(missing, m) {
			missing = append(missing, m)
		}
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
	Question  string          `json:"question"`
	SessionID string          `json:"session_id"`
	Status    string          `json:"status"`
	Unit      string          `json:"budget_unit"`
	Budget    int64           `json:"budget"`
	Spent     int64           `json:"spent"`
	Cost      core.Cost       `json:"cost"`
	Summary   string          `json:"summary"`
	Claims    []core.Claim    `json:"claims"`
	Stats     actors.RunStats `json:"stats"`
	Truncated bool            `json:"truncated"`
	Error     string          `json:"error,omitempty"`
}

func printLeadLine(question string, res *actors.Result, s budget.SettleResult, unit core.BudgetUnit, took time.Duration) {
	// A tick here means the lead worked. A run where most model calls failed
	// produced a handful of claims by accident and must not look the same as
	// one that succeeded (§9.5 degraded).
	mark := "✓"
	switch {
	case len(res.Claims) == 0:
		mark = "⚠"
	case res.Stats.ChunksFailed > res.Stats.Chunks-res.Stats.ChunksFailed:
		mark = "~"
	}
	q := question
	if len(q) > 44 {
		q = q[:43] + "…"
	}
	fmt.Printf(" %s web       %-45q %2d claims  %8s  %s\n",
		mark, q, len(res.Claims), fmtAmount(unit, s.Charged), took.Round(time.Millisecond))
}

func printReport(out *researchOutput, unit core.BudgetUnit, budgetAmt int64, quiet bool) {
	if !quiet {
		fmt.Println()
	}

	if out.Summary != "" {
		fmt.Println(out.Summary)
		fmt.Println()
	}

	if len(out.Claims) > 0 {
		// Sources are numbered and listed once, so a claim cites [n] rather
		// than repeating a URL that is often longer than the claim.
		sources, index := numberSources(out.Claims)

		fmt.Println("Claims")
		for _, c := range out.Claims {
			fmt.Printf("  [%d] %s\n", index[c.Source], c.Text)
			fmt.Printf("      > %s\n", ellipsize(c.Quote, 100))
		}
		fmt.Println()
		fmt.Println("Sources")
		for i, src := range sources {
			fmt.Printf("  [%d] %s\n", i+1, src)
		}
		fmt.Println()
	}

	// The counters that matter for M1: what the quote check rejected, and
	// whether the sub-budget cut the document short.
	fmt.Printf(" spent %s / %s  ·  claims %d  ·  sources %d read (%d from provider)  ·  chunks %d\n",
		fmtAmount(unit, out.Spent), fmtAmount(unit, budgetAmt),
		len(out.Claims), out.Stats.Fetched+out.Stats.SkippedFetch, out.Stats.SkippedFetch, out.Stats.Chunks)

	// Failed model calls are the difference between "the sources were thin"
	// and "the run barely worked". Buried in WARN lines they scroll past; the
	// summary is where a reader decides whether to trust the result.
	if out.Stats.ChunksFailed > 0 {
		fmt.Printf(" DEGRADED: %d of %d model call(s) failed — see the warnings above (§9.5)\n",
			out.Stats.ChunksFailed, out.Stats.Chunks)
	}
	if out.Stats.ClaimsRejected > 0 {
		fmt.Printf(" %d of %d proposed claims rejected: quote not found in the source (§11.5)\n",
			out.Stats.ClaimsRejected, out.Stats.ClaimsProposed)
	}
	if out.Truncated {
		fmt.Println(" truncated: the sub-budget did not cover every chunk (§4.1)")
	}
	if out.Error != "" {
		fmt.Printf(" ended %s: %s\n", out.Status, out.Error)
	}
	fmt.Printf("\n mole trace %s   for the per-call cost breakdown\n", out.SessionID)
}

// numberSources assigns each distinct source a stable number in first-seen
// order, which is the order a reader meets them.
func numberSources(claims []core.Claim) ([]string, map[string]int) {
	index := map[string]int{}
	var ordered []string
	for _, c := range claims {
		if _, seen := index[c.Source]; seen {
			continue
		}
		ordered = append(ordered, c.Source)
		index[c.Source] = len(ordered)
	}
	return ordered, index
}

func fmtAmount(unit core.BudgetUnit, amount int64) string {
	if unit == core.BudgetTokens {
		return fmt.Sprintf("%d tok", amount)
	}
	return core.FormatUSD(amount)
}

func ellipsize(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return cut + "…"
}
