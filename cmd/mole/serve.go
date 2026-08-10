package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/daemon"
	"github.com/lajosdeme/mole/internal/executor"
	"github.com/lajosdeme/mole/internal/mcpserver"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/spf13/cobra"
)

// defaultServeMaxSources matches the CLI's --max-sources default. The daemon has
// no per-call flag for it yet; the MCP tool surface carries it in slice 5.
const (
	defaultServeMaxSources = 5
	defaultServeMaxDepth   = 2
	defaultServeMaxLeads   = 12
	defaultServeWorkers    = executor.DefaultWorkers
	defaultServeTimeout    = 20 * time.Minute
)

// DefaultSocketName is the socket's basename under the runtime directory.
const DefaultSocketName = "mole.sock"

// defaultSocket is $XDG_RUNTIME_DIR/mole.sock, falling back to the state dir.
//
// XDG_RUNTIME_DIR is already 0700 and user-owned, and it is cleared on logout —
// exactly the lifetime a socket should have. Where it is unset (a bare container,
// some non-systemd setups) the state directory is used instead, and Listen makes
// that private itself rather than assuming.
func defaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, DefaultSocketName)
	}
	// Beside the database, which is already under a directory this user owns.
	return filepath.Join(filepath.Dir(defaultDBPath()), DefaultSocketName)
}

func newServeCmd() *cobra.Command {
	var (
		socket      string
		maxSessions int
		workers     int
		grace       time.Duration
	)
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the local daemon (§5)",
		Long: "Holds session, claim and budget state for callers that outlive a single\n" +
			"command — coding agents over MCP, principally. Research runs past an hour;\n" +
			"a bare stdio subprocess dies when the editor session closes, so the work\n" +
			"lives here and a disposable shim forwards to it.\n\n" +
			"Listens on a unix socket, mode 0600, in a private directory, and refuses\n" +
			"connections from any other user (§3.5). Nothing binds TCP.\n\n" +
			"The daemon keeps the database open for its lifetime but takes the write\n" +
			"lock only for the length of each transaction, so other mole commands can\n" +
			"run alongside it — reads never contend at all, and `ask` writes in\n" +
			"transactions short enough that the wait is not perceptible. Only\n" +
			"`mole migrate` should not be run against a live daemon.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmdServe(cmd.Context(), serveOpts{
				socket:      socket,
				maxSessions: maxSessions,
				workers:     workers,
				grace:       grace,
				dbPath:      dbPath(cmd),
			})
		},
	}
	c.Flags().StringVar(&socket, "socket", defaultSocket(), "unix socket to listen on")
	c.Flags().IntVar(&maxSessions, "max-sessions", session.DefaultMaxConcurrent,
		"sessions to run at once; past this, new requests are refused rather than queued")
	c.Flags().IntVar(&workers, "workers", defaultServeWorkers,
		"leads each session runs at once; multiplies with --max-sessions")
	c.Flags().DurationVar(&grace, "shutdown-grace", daemon.DefaultShutdownGrace,
		"how long to wait on shutdown for sessions to release their budget holds")
	return c
}

type serveOpts struct {
	socket      string
	maxSessions int
	workers     int
	grace       time.Duration
	dbPath      string
}

func cmdServe(ctx context.Context, o serveOpts) error {
	cfg, err := config.Load()
	if err != nil && !errors.Is(err, config.ErrNotConfigured) {
		return err
	}

	// Built before the socket exists. A missing search key should fail in under a
	// second, not after advertising a daemon that cannot run anything.
	// needSearch is true here and not conditional: the daemon has no --actors
	// flag (see the README's known gaps), so every session it runs is a web
	// session and a missing key should fail before the socket exists rather
	// than on the first request.
	actor, err := buildWebActor(cfg, nil, defaultServeMaxSources, false, true, true)
	if err != nil {
		return err
	}

	db, err := openDBMigrate(ctx, o.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	runner := &session.Runner{
		Store:             db,
		Actor:             actor,
		VerifierModel:     cfg.LLM.VerifierModel,
		VerifierBatchSize: cfg.LLM.VerifierBatchSize,
		Owner:             "daemon",
		Log:               actor.Log,
		Notice: func(msg string) {
			actor.Log.Warn(msg)
		},
	}

	// Once, at boot: reclaim what a previous process left behind (§9.4). Per
	// session would sweep leases belonging to sessions running alongside.
	if rc := runner.Recover(ctx); rc.Any() {
		actor.Log.Info("recovered state from a previous run",
			"leads", rc.Leads, "reservations", rc.Reservations, "sessions", rc.Sessions)
	}

	sup := session.NewSupervisor(runner, o.maxSessions, actor.Log)

	// One MCP server for the daemon, connected to each accepted connection
	// separately. Its tools close over the supervisor and the store, so every
	// connection shares the same state — which is the point: a session started
	// through one shim is visible through the next.
	mcpSrv := mcpserver.New(mcpserver.Deps{
		Supervisor:       sup,
		Store:            db,
		MaxSessionUSD:    cfg.MaxSessionUSD,
		MaxSessionTokens: cfg.MaxSessionTokens,
		LLM:              actor.LLM,
		Pricing:          actor.Pricing,
		MaxSources:       defaultServeMaxSources,
		MaxDepth:         defaultServeMaxDepth,
		MaxLeads:         defaultServeMaxLeads,
		Timeout:          defaultServeTimeout,
		Log:              actor.Log,
		Version:          version,
		Workers:          o.workers,
	})

	srv := &daemon.Server{
		Socket:     o.socket,
		Supervisor: sup,
		Handler: daemon.HandlerFunc(func(ctx context.Context, c net.Conn) {
			// Run, not Connect: each connection has its own transport and its own
			// session, and Run blocks until that session ends — which is exactly the
			// lifetime the daemon already gives a handler goroutine.
			if err := mcpSrv.Run(ctx, &mcp.IOTransport{Reader: c, Writer: c}); err != nil &&
				!errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				actor.Log.Warn("mcp session ended with an error", "err", err)
			}
		}),
		Log:           actor.Log,
		ShutdownGrace: o.grace,
	}

	ln, err := srv.Listen()
	if err != nil {
		return err
	}

	// SIGINT and SIGTERM both mean stop: one is a person, the other is systemd.
	// Cancelling the context is what starts the shutdown that waits for budget
	// holds to be released.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("mole daemon listening on %s\n", o.socket)
	fmt.Printf("  database  %s\n", o.dbPath)
	// Both numbers and their product, because the product is what leaves this
	// machine. Printed from EffectiveWorkers rather than the configured value:
	// the banner used to print the raw default and overstate the real ceiling by
	// a third, since the pool is also bounded by the replan cadence.
	perSession := executor.EffectiveWorkers(o.workers)
	fmt.Printf("  sessions  up to %d at once, %d lead(s) each (up to %d concurrent leads)\n",
		o.maxSessions, perSession, o.maxSessions*perSession)
	if cfg.MaxSessionUSD > 0 {
		fmt.Printf("  ceiling   %s per session\n", core.FormatUSD(cfg.MaxSessionUSD))
	} else {
		fmt.Println("  ceiling   none — set one with: mole config set daemon.max-session-usd")
	}
	fmt.Println("  stop with Ctrl-C; running sessions are cancelled and their budget released")

	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}
	fmt.Println("daemon stopped")
	return nil
}
