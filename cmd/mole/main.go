// Command mole is the daemon and CLI.
//
// M0 ships the foundations only: schema, ledger, cassettes, tracing. There is
// no research loop yet, so the commands here are the ones that operate on
// persisted state — migrate, doctor, sessions, trace — plus a dev seeder so the
// trace view can be exercised by hand before M1 produces real sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/lajosdeme/mole/internal/budget"
	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/obs"
	"github.com/lajosdeme/mole/internal/pricing"
	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/store/sqlite"
)

// version is overridden at build time via -ldflags.
var version = "dev"

const usage = `mole — deep research agent

Usage:
  mole <command> [flags]

Commands:
  migrate      Apply pending database migrations
  config       Get, set, and list settings (see: mole config)
  doctor       Check configuration and environment
  sessions     List recent sessions
  trace        Show the cost and span breakdown for one session
  dev          Development helpers (see: mole dev -h)
  version      Print version

Global flags:
  --db PATH    Database path (default $MOLE_DB, else the XDG data dir)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "mole: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("mole " + version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	case "migrate":
		return cmdMigrate(ctx, rest)
	case "config":
		return cmdConfig(ctx, rest)
	case "doctor":
		return cmdDoctor(ctx, rest)
	case "sessions":
		return cmdSessions(ctx, rest)
	case "trace":
		return cmdTrace(ctx, rest)
	case "dev":
		return cmdDev(ctx, rest)
	default:
		return fmt.Errorf("unknown command %q (try: mole help)", cmd)
	}
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

// defaultDBPath follows the XDG data spec so the database lands somewhere a
// backup tool will find it, not in a dotfile at $HOME.
func defaultDBPath() string {
	if p := os.Getenv("MOLE_DB"); p != "" {
		return p
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "mole.db"
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "mole", "mole.db")
}

func addDBFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultDBPath(), "database path")
}

// openDBWrite opens and migrates. Only commands that actually write should use
// it — migrating takes the write lock, and the daemon may be holding it.
func openDBWrite(ctx context.Context, path string) (*sqlite.DB, error) {
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// openDBRead opens without migrating.
//
// This is the distinction that lets the CLI coexist with a running daemon.
// SQLite in WAL mode allows any number of concurrent readers alongside one
// writer, so `mole trace` never blocks and is never blocked — but only if it
// does not try to write. Migrating on every open (the obvious shortcut) would
// have every read command contend for the writer against the daemon.
//
// Instead of migrating, it checks the schema version and says what to run.
func openDBRead(ctx context.Context, path string) (*sqlite.DB, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("no database at %s (run: mole migrate)", path)
	}

	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		return nil, err
	}

	have, err := db.SchemaVersion(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	want, err := sqlite.ExpectedSchemaVersion()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if have < want {
		_ = db.Close()
		return nil, fmt.Errorf("database schema is at version %d, this binary expects %d (run: mole migrate)", have, want)
	}
	if have > want {
		// A newer daemon wrote this database. Reading it with older code could
		// misinterpret columns, so refuse rather than print something wrong.
		_ = db.Close()
		return nil, fmt.Errorf("database schema is at version %d, newer than this binary's %d (upgrade mole)", have, want)
	}
	return db, nil
}

func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

// ---------------------------------------------------------------------------
// migrate
// ---------------------------------------------------------------------------

func cmdMigrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dbPath := addDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDBWrite(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	fmt.Printf("migrations applied — %s\n", db.Path())
	return nil
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// cmdDoctor is where the "check this at startup, not in the README"
// requirements live. M0 covers what M0 owns: the database, the schema, and the
// pricing table. Provider keys, contact email, socket permissions, and sandbox
// availability get added as their milestones land.
func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dbPath := addDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ok := true
	report := func(good bool, label, detail string) {
		mark := "✓"
		if !good {
			mark = "!"
			ok = false
		}
		fmt.Printf("%s %-18s %s\n", mark, label, detail)
	}

	db, err := openDBRead(ctx, *dbPath)
	if err != nil {
		report(false, "state db", err.Error())
		return errors.New("doctor found problems")
	}
	defer db.Close()
	report(true, "state db", db.Path()+" (sqlite, WAL, single writer)")

	var sessionCount int
	err = db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		list, err := q.ListSessions(ctx, 1000)
		sessionCount = len(list)
		return err
	})
	if err != nil {
		report(false, "schema", err.Error())
	} else {
		report(true, "schema", fmt.Sprintf("readable, %d session(s)", sessionCount))
	}

	table := pricing.NewTable()
	report(true, "pricing", fmt.Sprintf("%d models registered", len(table.Models())))

	// Reconcile every session's materialized counters against the ledger.
	// A divergence here is the single most important thing this command can
	// find: it means the budget ceiling is no longer trustworthy.
	led := budget.New(db, budget.DefaultConfig())
	drifted := 0
	err = db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		list, err := q.ListSessions(ctx, 1000)
		if err != nil {
			return err
		}
		for _, s := range list {
			v, err := led.Verify(ctx, s.ID)
			if err != nil {
				return err
			}
			if !v.Consistent() {
				drifted++
				fmt.Printf("    session %s: spent recorded=%d ledger=%d, held recorded=%d rows=%d\n",
					s.ID, v.SpentRecorded, v.SpentFromLedger, v.HeldRecorded, v.HeldFromRows)
			}
		}
		return nil
	})
	if err != nil {
		report(false, "ledger", err.Error())
	} else {
		report(drifted == 0, "ledger", fmt.Sprintf("%d session(s) reconciled, %d drifted", sessionCount, drifted))
	}

	reportConfig(report)

	if !ok {
		fmt.Println("\nsome checks are informational until their milestone lands")
	}
	return nil
}

// reportConfig checks credentials and settings. These are the "verify at
// startup, not in a README" requirements: a missing search key is a session
// that fails on its first lead, and a missing contact address is a ban from an
// academic provider that requires identification.
func reportConfig(report func(bool, string, string)) {
	cfg, err := config.Load()
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		report(false, "config", err.Error())
		return
	}

	if permsOK, detail := config.CheckPermissions(); !permsOK {
		// The config file holds API keys and nothing else protects them yet.
		report(false, "config perms", detail)
	} else {
		report(true, "config perms", detail)
	}

	switch {
	case cfg.Search.Provider == "":
		report(false, "search provider", "not selected (run: mole config set search.provider brave|tavily)")
	case cfg.Search.ActiveKey() == "":
		report(false, "search provider",
			fmt.Sprintf("%s selected but no key (run: mole config set search.%s-key ...)",
				cfg.Search.Provider, cfg.Search.Provider))
	default:
		note := ""
		if cfg.Search.Provider == "tavily" {
			// Worth surfacing: it changes how many fetches a session makes.
			note = " — returns page content, skips fetches"
		}
		report(true, "search provider",
			fmt.Sprintf("%s, key %s%s", cfg.Search.Provider, config.Mask(cfg.Search.ActiveKey()), note))
	}

	if cfg.LLM.APIKey == "" {
		report(false, "llm provider", "no key (run: mole config set llm.api-key ...)")
	} else {
		model := cfg.LLM.Model
		if model == "" {
			model = "(no default model set)"
		}
		report(true, "llm provider", fmt.Sprintf("%s, key %s", model, config.Mask(cfg.LLM.APIKey)))
	}

	if cfg.ContactEmail == "" {
		report(false, "contact email", "not set — required by Unpaywall and NCBI before academic providers (M6)")
	} else {
		report(true, "contact email", cfg.ContactEmail)
	}
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

func cmdSessions(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	dbPath := addDBFlag(fs)
	limit := fs.Int("limit", 20, "maximum sessions to list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDBRead(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var list []*core.Session
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		list, err = q.ListSessions(ctx, *limit)
		return err
	}); err != nil {
		return err
	}

	if len(list) == 0 {
		fmt.Println("no sessions")
		return nil
	}

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tSTATUS\tMODE\tSPENT\tBUDGET\tAVAILABLE\tCREATED")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.Status, s.Mode,
			core.FormatAmount(s.Spent, s.BudgetUnit),
			core.FormatAmount(s.Budget, s.BudgetUnit),
			core.FormatAmount(s.Available(), s.BudgetUnit),
			s.CreatedAt.Format(time.RFC3339))
	}
	return w.Flush()
}

// ---------------------------------------------------------------------------
// trace
// ---------------------------------------------------------------------------

func cmdTrace(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	dbPath := addDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: mole trace <session-id>")
	}
	sessionID := fs.Arg(0)

	db, err := openDBRead(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var (
		sess   *core.Session
		byRole map[core.Role]core.Cost
		spans  []*core.Span
	)
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		if sess, err = q.GetSession(ctx, sessionID); err != nil {
			return err
		}
		if byRole, err = q.SumCostsByRole(ctx, sessionID); err != nil {
			return err
		}
		spans, err = q.ListSpans(ctx, sessionID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no session %s", sessionID)
		}
		return err
	}

	unit := sess.BudgetUnit
	fmt.Printf("%s  %s / %s  ·  status=%s  mode=%s\n",
		sess.ID,
		core.FormatAmount(sess.Spent, unit),
		core.FormatAmount(sess.Budget, unit),
		sess.Status, sess.Mode)
	if sess.Escrow > 0 {
		fmt.Printf("escrow held: %s\n", core.FormatAmount(sess.Escrow, unit))
	}
	fmt.Println()

	// Role breakdown — the reason ToolCall.Role exists.
	roles := []core.Role{core.RolePlanner, core.RoleExecutor, core.RoleVerifier, core.RoleOutput}
	total := sess.Spent
	w := newTabWriter()
	fmt.Fprintln(w, "ROLE\tSPEND\tSHARE\tTOKENS")
	for _, r := range roles {
		c, ok := byRole[r]
		if !ok {
			continue
		}
		amount := c.BudgetAmount(unit)
		share := 0.0
		if total > 0 {
			share = float64(amount) / float64(total) * 100
		}
		fmt.Fprintf(w, "%s\t%s\t%.0f%%\t%d\n", r, core.FormatAmount(amount, unit), share, c.TotalTokens())
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Cache accounting, which a flat token count would hide entirely.
	var all core.Cost
	for _, c := range byRole {
		all = all.Add(c)
	}
	fmt.Printf("\ntokens: %d in · %d out · %d cache-read · %d cache-write\n",
		all.InputTokens, all.OutputTokens, all.CacheReadTokens, all.CacheWriteTokens)

	// Ledger reconciliation, shown inline so a drift is impossible to miss.
	led := budget.New(db, budget.DefaultConfig())
	v, err := led.Verify(ctx, sessionID)
	if err != nil {
		return err
	}
	if v.Consistent() {
		fmt.Printf("ledger: consistent (%d calls)\n", v.CallsFromRows)
	} else {
		fmt.Printf("ledger: DRIFT — spent recorded=%d ledger=%d, held recorded=%d rows=%d, calls recorded=%d rows=%d\n",
			v.SpentRecorded, v.SpentFromLedger, v.HeldRecorded, v.HeldFromRows, v.CallsRecorded, v.CallsFromRows)
	}

	if len(spans) > 0 {
		fmt.Printf("\nspans (%d):\n", len(spans))
		sort.Slice(spans, func(i, j int) bool { return spans[i].StartedAt.Before(spans[j].StartedAt) })
		sw := newTabWriter()
		fmt.Fprintln(sw, "  NAME\tDURATION\tSTATUS")
		for _, s := range spans {
			dur := "open"
			if s.EndedAt != nil {
				dur = s.EndedAt.Sub(s.StartedAt).Round(time.Millisecond).String()
			}
			status := s.Status
			if status == "" {
				status = "-"
			}
			fmt.Fprintf(sw, "  %s\t%s\t%s\n", s.Name, dur, status)
		}
		if err := sw.Flush(); err != nil {
			return err
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// dev
// ---------------------------------------------------------------------------

func cmdDev(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Println("Usage: mole dev seed [--db PATH]")
		return nil
	}
	switch args[0] {
	case "seed":
		return cmdDevSeed(ctx, args[1:])
	default:
		return fmt.Errorf("unknown dev subcommand %q", args[0])
	}
}

// cmdDevSeed writes one synthetic session through the real ledger — reserve,
// settle, escrow release — so `mole trace` and `mole doctor` have something to
// operate on before M1 exists. It uses no network and no LLM.
func cmdDevSeed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("dev seed", flag.ContinueOnError)
	dbPath := addDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := openDBWrite(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	log := obs.NewLogger(obs.LogOptions{Level: "info", Format: "text"})
	tracer := obs.NewTracer(db, log)
	table := pricing.NewTable()
	led := budget.New(db, budget.DefaultConfig())

	sess, err := led.CreateSession(ctx, budget.SessionSpec{
		Prompt:       "seed: what is the current consensus on byte-level LLMs?",
		Mode:         core.ModeReport,
		ActorTypes:   []core.ActorType{core.ActorWeb},
		BudgetUnit:   core.BudgetUSD,
		Budget:       3 * core.MicrosPerUSD,
		MaxToolCalls: 200,
	})
	if err != nil {
		return err
	}

	type step struct {
		role  core.Role
		model string
		usage pricing.Usage
	}
	steps := []step{
		{core.RolePlanner, "claude-opus-5", pricing.Usage{InputTokens: 3200, OutputTokens: 700}},
		{core.RoleExecutor, "claude-opus-5", pricing.Usage{InputTokens: 14000, OutputTokens: 1800, CacheReadTokens: 9000}},
		{core.RoleExecutor, "claude-opus-5", pricing.Usage{InputTokens: 11000, OutputTokens: 1500, CacheReadTokens: 9000}},
		{core.RoleVerifier, "claude-opus-5", pricing.Usage{InputTokens: 5200, OutputTokens: 900, CacheReadTokens: 4000}},
	}

	for i, st := range steps {
		ctx, span := tracer.Start(ctx, sess.ID, string(st.role))

		cost, err := table.Cost(st.model, st.usage)
		if err != nil {
			span.End(ctx, "error")
			return err
		}

		amount := cost.BudgetAmount(sess.BudgetUnit)
		res, err := led.Reserve(ctx, sess.ID, amount)
		if err != nil {
			span.End(ctx, "error")
			return fmt.Errorf("step %d: %w", i, err)
		}

		if _, err := led.Settle(ctx, res, []core.ToolCall{{
			Role:  st.role,
			Type:  core.CallLLM,
			Model: st.model,
			Cost:  cost,
		}}); err != nil {
			span.End(ctx, "error")
			return err
		}
		span.End(ctx, "ok")
	}

	// Output is paid from escrow — release it first, exactly as the real loop
	// will.
	released, err := led.ReleaseEscrow(ctx, sess.ID)
	if err != nil {
		return err
	}
	ctx, outSpan := tracer.Start(ctx, sess.ID, "output")
	outCost, err := table.Cost("claude-opus-5", pricing.Usage{InputTokens: 18000, OutputTokens: 4200, CacheReadTokens: 12000})
	if err != nil {
		outSpan.End(ctx, "error")
		return err
	}
	outRes, err := led.Reserve(ctx, sess.ID, outCost.BudgetAmount(sess.BudgetUnit))
	if err != nil {
		outSpan.End(ctx, "error")
		return err
	}
	if _, err := led.Settle(ctx, outRes, []core.ToolCall{{
		Role: core.RoleOutput, Type: core.CallLLM, Model: "claude-opus-5", Cost: outCost,
	}}); err != nil {
		outSpan.End(ctx, "error")
		return err
	}
	outSpan.End(ctx, "ok")

	if err := led.Finish(ctx, sess.ID, core.StatusDone); err != nil {
		return err
	}

	fmt.Printf("seeded session %s (escrow released: %s)\n",
		sess.ID, core.FormatAmount(released, sess.BudgetUnit))
	fmt.Printf("inspect: mole trace %s\n", sess.ID)
	return nil
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

const configUsage = `mole config — settings and credentials

Usage:
  mole config list              Show all settings (secrets masked)
  mole config get <key>         Print one value
  mole config set <key> <value> Assign a value
  mole config path              Print the config file location

Keys:
`

func cmdConfig(ctx context.Context, args []string) error {
	if len(args) == 0 {
		printConfigUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return cmdConfigList()
	case "get":
		if len(args) != 2 {
			return errors.New("usage: mole config get <key>")
		}
		return cmdConfigGet(args[1])
	case "set":
		if len(args) != 3 {
			return errors.New("usage: mole config set <key> <value>")
		}
		return cmdConfigSet(args[1], args[2])
	case "path":
		fmt.Println(config.Path())
		return nil
	case "help", "-h", "--help":
		printConfigUsage()
		return nil
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

func printConfigUsage() {
	fmt.Print(configUsage)
	w := newTabWriter()
	for _, f := range config.Fields() {
		secret := ""
		if f.Secret {
			secret = "  (secret)"
		}
		fmt.Fprintf(w, "  %s\t%s%s\n", f.Name, f.Help, secret)
	}
	_ = w.Flush()
}

// loadConfigForEdit tolerates a missing file: `mole config set` on a fresh
// machine has to work, and that is the only way the file ever gets created.
func loadConfigForEdit() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		return nil, err
	}
	return cfg, nil
}

func cmdConfigList() error {
	cfg, err := loadConfigForEdit()
	if err != nil {
		return err
	}

	fmt.Printf("%s\n\n", config.Path())
	w := newTabWriter()
	fmt.Fprintln(w, "KEY\tVALUE")
	for _, f := range config.Fields() {
		fmt.Fprintf(w, "%s\t%s\n", f.Name, config.Display(f, cfg))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if ok, detail := config.CheckPermissions(); !ok {
		fmt.Printf("\n! %s\n", detail)
	}
	return nil
}

func cmdConfigGet(key string) error {
	cfg, err := loadConfigForEdit()
	if err != nil {
		return err
	}
	v, err := cfg.Get(key)
	if err != nil {
		return err
	}
	// Print secrets in full here: `get` on a single named key is a deliberate
	// act, unlike `list`, which is browsing.
	fmt.Println(v)
	return nil
}

func cmdConfigSet(key, value string) error {
	cfg, err := loadConfigForEdit()
	if err != nil {
		return err
	}
	if err := cfg.Set(key, value); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	f, _ := lookupConfigField(key)
	fmt.Printf("set %s = %s\n", key, config.Display(f, cfg))
	return nil
}

func lookupConfigField(name string) (config.Field, bool) {
	for _, f := range config.Fields() {
		if f.Name == name {
			return f, true
		}
	}
	return config.Field{}, false
}
