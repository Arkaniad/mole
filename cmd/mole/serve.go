package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/daemon"
	"github.com/lajosdeme/mole/internal/session"
	"github.com/spf13/cobra"
)

// defaultServeMaxSources matches the CLI's --max-sources default. The daemon has
// no per-call flag for it yet; the MCP tool surface carries it in slice 5.
const defaultServeMaxSources = 5

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
			"The daemon holds the database's single writer for its lifetime. Read\n" +
			"commands — sessions, trace, eval, stats — open read-only and are neither\n" +
			"blocked by it nor block it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmdServe(cmd.Context(), serveOpts{
				socket:      socket,
				maxSessions: maxSessions,
				grace:       grace,
				dbPath:      dbPath(cmd),
			})
		},
	}
	c.Flags().StringVar(&socket, "socket", defaultSocket(), "unix socket to listen on")
	c.Flags().IntVar(&maxSessions, "max-sessions", session.DefaultMaxConcurrent,
		"sessions to run at once; past this, new requests are refused rather than queued")
	c.Flags().DurationVar(&grace, "shutdown-grace", daemon.DefaultShutdownGrace,
		"how long to wait on shutdown for sessions to release their budget holds")
	return c
}

type serveOpts struct {
	socket      string
	maxSessions int
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
	actor, err := buildWebActor(cfg, nil, defaultServeMaxSources, false, true)
	if err != nil {
		return err
	}

	db, err := openDBWrite(ctx, o.dbPath)
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
	srv := &daemon.Server{
		Socket:        o.socket,
		Supervisor:    sup,
		Handler:       daemon.HandlerFunc(notYetServing),
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
	fmt.Printf("  database  %s (holding the single writer)\n", o.dbPath)
	fmt.Printf("  sessions  up to %d at once\n", o.maxSessions)
	fmt.Println("  stop with Ctrl-C; running sessions are cancelled and their budget released")

	if err := srv.Serve(ctx, ln); err != nil {
		return err
	}
	fmt.Println("daemon stopped")
	return nil
}

// notYetServing answers a connection before the MCP server exists.
//
// A message rather than silence or a closed socket: everything under it — the
// listener's permissions, peer checks, the supervisor, shutdown that waits for
// budget holds — is real and running, and somebody who connects should be told
// which part is missing rather than left guessing whether the daemon is broken.
func notYetServing(_ context.Context, c net.Conn) {
	_, _ = c.Write([]byte("mole: the daemon is running but speaks no protocol yet " +
		"(MCP arrives in M7 slice 5)\n"))
}
