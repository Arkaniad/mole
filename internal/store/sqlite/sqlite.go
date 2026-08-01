// Package sqlite implements store.Store on SQLite via a pure-Go driver.
//
// Two design points are deliberate and load-bearing:
//
//   - The driver is modernc.org/sqlite (no cgo), because "one static binary,
//     no separate service" is a stated property of the product. A cgo driver
//     would trade that away for marginal speed.
//   - There are two connection pools. Writes go through a pool capped at ONE
//     connection, which serializes them at the pool and removes the
//     `SQLITE_BUSY` / "database is locked" failure mode entirely once the
//     executor runs multiple workers. Reads use a normal pool and run
//     concurrently under WAL.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/lajosdeme/mole/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Options configures the store. The zero value is usable.
type Options struct {
	// ReadConns caps the read pool. Defaults to 8.
	ReadConns int
	// BusyTimeout is how long a statement waits on a lock before erroring.
	// Defaults to 5s. With a single-writer pool this should never be hit from
	// inside one process; it matters when a second process (the CLI) attaches.
	BusyTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.ReadConns <= 0 {
		o.ReadConns = 8
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5 * time.Second
	}
	return o
}

// DB is the SQLite-backed store.
type DB struct {
	write *sql.DB // MaxOpenConns(1) — the single writer
	read  *sql.DB
	path  string
}

// Open prepares the database at path, creating it if needed. Use ":memory:"
// for tests; that mode collapses to a single shared connection because an
// in-memory database is per-connection.
func Open(path string, opts Options) (*DB, error) {
	opts = opts.withDefaults()

	memory := path == ":memory:" || strings.Contains(path, "mode=memory")
	if !memory {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := ensureDir(dir); err != nil {
				return nil, err
			}
		}
	}

	writeDSN := dsn(path, opts, memory, true)
	w, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open write pool: %w", err)
	}
	// The single-writer invariant. Do not raise this.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	if err := w.Ping(); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}

	db := &DB{write: w, path: path}

	if memory {
		// An in-memory database lives on its connection, so reads must share
		// the writer. Concurrency is irrelevant for the test/ephemeral case.
		db.read = w
	} else {
		r, err := sql.Open("sqlite", dsn(path, opts, memory, false))
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("sqlite: open read pool: %w", err)
		}
		r.SetMaxOpenConns(opts.ReadConns)
		r.SetMaxIdleConns(opts.ReadConns)
		if err := r.Ping(); err != nil {
			_ = w.Close()
			_ = r.Close()
			return nil, fmt.Errorf("sqlite: ping read pool: %w", err)
		}
		db.read = r
	}

	return db, nil
}

func dsn(path string, opts Options, memory, writer bool) string {
	q := url.Values{}
	add := func(p string) { q.Add("_pragma", p) }

	if !memory {
		add("journal_mode(WAL)")
	}
	add("busy_timeout(" + strconv.FormatInt(opts.BusyTimeout.Milliseconds(), 10) + ")")
	add("foreign_keys(1)")
	if writer {
		// NORMAL is the right durability point under WAL: a crash can lose the
		// last transactions but cannot corrupt the database, and the ledger is
		// recoverable by recomputing from tool_calls.
		add("synchronous(NORMAL)")
	} else {
		add("query_only(1)")
	}

	return "file:" + path + "?" + q.Encode()
}

func (d *DB) Close() error {
	var firstErr error
	if d.read != nil && d.read != d.write {
		if err := d.read.Close(); err != nil {
			firstErr = err
		}
	}
	if err := d.write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Path reports the on-disk location, for diagnostics.
func (d *DB) Path() string { return d.path }

// WithTx runs fn in a write transaction.
func (d *DB) WithTx(ctx context.Context, fn func(context.Context, store.Tx) error) error {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	if err := fn(ctx, &queries{q: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

// Read runs fn against the read pool.
func (d *DB) Read(ctx context.Context, fn func(context.Context, store.Queries) error) error {
	return fn(ctx, &queries{q: d.read})
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

// Migrate applies pending migrations. Each runs in its own transaction and
// records its version, so a partially-applied set is resumable rather than
// requiring a manual repair.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.write.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("sqlite: create schema_migrations: %w", err)
	}

	applied, err := d.appliedVersions(ctx)
	if err != nil {
		return err
	}

	migs, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			return fmt.Errorf("sqlite: migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// SchemaVersion reports the highest applied migration, or 0 on a fresh
// database. It reads through the read pool, so a read-only command can check
// the schema without taking a write lock — which matters because the daemon may
// hold the writer while the CLI runs.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.read.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		// A fresh database has no schema_migrations table at all.
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("sqlite: read schema version: %w", err)
	}
	return int(v.Int64), nil
}

// ExpectedSchemaVersion is the highest migration this binary ships.
func ExpectedSchemaVersion() (int, error) {
	migs, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	if len(migs) == 0 {
		return 0, nil
	}
	return migs[len(migs)-1].version, nil
}

func (d *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := d.write.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := d.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC().UnixMicro()); err != nil {
		return err
	}
	return tx.Commit()
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		verStr, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("sqlite: migration %q must be named <version>_<name>.sql", e.Name())
		}
		v, err := strconv.Atoi(verStr)
		if err != nil {
			return nil, fmt.Errorf("sqlite: migration %q has non-numeric version: %w", e.Name(), err)
		}
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}
