// Package connector registers local data sources and hands out read-only
// handles to them (M8, §12).
//
// §12.2 says defense in depth, all four required, and names least privilege
// first: "connector registration requires a read-only role and warns loudly if
// the DSN's user has write grants". A local file has no roles and no grants, so
// the equivalent here is the handle itself — every query runs through a
// connection opened `mode=ro` with `query_only(1)`, and there is no other
// connection. That makes the least-privilege defense a property of the driver
// rather than of the caller's discipline, which is the point of having it
// separate from `sqlguard`: if the parse gate is bypassed entirely, a generated
// DROP TABLE still cannot execute.
//
// Registration is the only write. It ingests files into a scratch database,
// profiles the columns, and then never opens a writable connection again.
package connector

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Kind is how a connector reaches its data.
type Kind string

const (
	// KindSQLite is a database the user already has. It is attached in place
	// and never copied — copying someone's database to profile it is both
	// wasteful and a second copy of their data to look after.
	KindSQLite Kind = "sqlite"
	// KindImport is one or more delimited/JSON files read into a scratch
	// database that mole owns.
	KindImport Kind = "import"
)

var (
	ErrNoSuchConnector = errors.New("connector: not registered")
	ErrDuplicateName   = errors.New("connector: already registered")
	ErrNoData          = errors.New("connector: no readable data files")
)

// Connector is one registered source.
type Connector struct {
	Name string `json:"name"`
	// Source is what the user named: a file, or a directory of files.
	Source string `json:"source"`
	Kind   Kind   `json:"kind"`
	// DBPath is what gets opened. For KindSQLite it is Source itself.
	DBPath     string    `json:"db_path"`
	Tables     []Table   `json:"tables"`
	ImportedAt time.Time `json:"imported_at"`

	// Skipped names everything registration could not use: a table or column
	// whose name is not a usable identifier, a ragged row's extra fields.
	//
	// Reported rather than dropped. Uppercase identifiers used to be rejected,
	// so an ordinary CamelCase database registered with no error and a profile
	// of one table with one column — the model then planned over a schema that
	// was not the user's data, and nothing anywhere said so.
	Skipped []string `json:"skipped,omitempty"`
}

// Table is one relation and its profile.
type Table struct {
	Name string `json:"name"`
	// Origin is the file it came from, empty for KindSQLite.
	Origin  string   `json:"origin,omitempty"`
	Rows    int64    `json:"rows"`
	Columns []Column `json:"columns"`
}

// Column carries everything the planner is allowed to see about a column.
//
// §12.3 constrains local leads to "hypothesis templates over the connector's
// known schema", so this struct IS what the model gets to reason over. It holds
// shape — type, null rate, cardinality, range — and no values from the data
// except Min/Max, which are bounded scalars on ordered columns and are withheld
// for anything flagged FreeText.
type Column struct {
	// Name is the sanitized identifier used in SQL. Label is the header as it
	// appeared in the file, kept because "total_rev_usd" tells a model less
	// than "Total Revenue (USD)".
	Name  string     `json:"name"`
	Label string     `json:"label,omitempty"`
	Type  ColumnType `json:"type"`

	Nulls    int64   `json:"nulls"`
	Distinct int64   `json:"distinct"`
	AvgLen   float64 `json:"avg_len"`
	Min      string  `json:"min,omitempty"`
	Max      string  `json:"max,omitempty"`

	// FreeText marks a column whose distinct values are effectively the rows.
	// §12.1 excludes these from TopK by default — "a 'top values' list over a
	// notes field is just the rows" — so the flag has to be decided here, at
	// ingest, where the values are still in front of us. The gate cannot
	// recompute it later: by then it only has aggregates.
	FreeText bool `json:"free_text"`
}

// ColumnType is the inferred storage type.
type ColumnType string

const (
	TypeInteger ColumnType = "integer"
	TypeReal    ColumnType = "real"
	TypeBool    ColumnType = "boolean"
	TypeTime    ColumnType = "timestamp"
	TypeText    ColumnType = "text"
)

// Numeric reports whether the column supports the moment and quantile
// statistics an AggregateEnvelope carries.
func (t ColumnType) Numeric() bool { return t == TypeInteger || t == TypeReal }

// Table finds a table by name.
func (c Connector) Table(name string) (Table, bool) {
	for _, t := range c.Tables {
		if t.Name == name {
			return t, true
		}
	}
	return Table{}, false
}

// Column finds a column by its SQL identifier.
func (t Table) Column(name string) (Column, bool) {
	for _, col := range t.Columns {
		if col.Name == name {
			return col, true
		}
	}
	return Column{}, false
}

// -----------------------------------------------------------------------------
// The read-only handle
// -----------------------------------------------------------------------------

// Open returns a read-only handle to the connector's data.
//
// `mode=ro` is the defense. It refuses at the VFS layer, before SQL sees the
// statement, and nothing a query can say reaches it.
//
// `query_only(1)` is set as well but is measurably weaker: `PRAGMA query_only
// = 0` executes successfully on this handle, so the SQL-layer flag can be
// switched off by the same statements it exists to constrain. It stops an
// accident, not an attempt. connector_test.go asserts a write still fails
// after that PRAGMA, which is what pins the distinction down.
//
// Two consequences worth carrying forward. `sqlguard` must reject PRAGMA
// outright — it is not a harmless read-only verb. And an engine whose
// equivalent of `mode=ro` is only a session setting would need a different
// control here, because a settable flag is not least privilege.
func (c Connector) Open() (*sql.DB, error) {
	if c.DBPath == "" {
		return nil, fmt.Errorf("connector %q: no database path", c.Name)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(c.DBPath))
	if err != nil {
		return nil, fmt.Errorf("connector %q: open: %w", c.Name, err)
	}
	// Small. Analysis is one query at a time behind the aggregation gate, and
	// a pool here would only add file handles on someone else's database.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		// A WAL database with a live writer needs write access to its -shm
		// file, so read-only open fails. Saying so beats "unable to open".
		return nil, fmt.Errorf("connector %q: open %s read-only: %w "+
			"(a database with an active writer cannot be attached read-only; "+
			"export a copy and register that instead)", c.Name, c.DBPath, err)
	}
	return db, nil
}

func readOnlyDSN(path string) string {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	// Nothing this handle runs may reach the filesystem or another database.
	// sqlguard rejects ATTACH and load_extension() by parse, and modernc ships
	// no loadable-extension support, but trusting a parser alone is what §12.2
	// exists to avoid.
	q.Add("_pragma", "trusted_schema(0)")
	return "file:" + path + "?" + q.Encode()
}

// -----------------------------------------------------------------------------
// Registry
// -----------------------------------------------------------------------------

// Registry is the set of registered connectors, persisted as JSON.
//
// It holds paths into the user's filesystem and a profile of their data — no
// credentials, since a local file has none — and is still written 0600. A list
// of exactly which files someone pointed a research tool at is not something to
// leave world-readable.
type Registry struct {
	path string
	list []Connector
}

// LoadRegistry reads the registry, returning an empty one if it does not exist.
func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connector: read registry: %w", err)
	}
	if err := json.Unmarshal(raw, &r.list); err != nil {
		return nil, fmt.Errorf("connector: parse %s: %w", path, err)
	}
	return r, nil
}

// Save writes the registry with owner-only permissions.
func (r *Registry) Save() error {
	if dir := filepath.Dir(r.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("connector: registry dir: %w", err)
		}
	}
	raw, err := json.MarshalIndent(r.list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(r.path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("connector: write registry: %w", err)
	}
	return nil
}

// Add registers a connector. Names are unique: a duplicate would make
// "connector:<name>#<hash>" — the claim source for local evidence — ambiguous
// about which data it came from.
func (r *Registry) Add(c Connector) error {
	if _, ok := r.Get(c.Name); ok {
		return fmt.Errorf("%w: %s", ErrDuplicateName, c.Name)
	}
	r.list = append(r.list, c)
	sort.Slice(r.list, func(i, j int) bool { return r.list[i].Name < r.list[j].Name })
	return nil
}

// Get returns a connector by name.
func (r *Registry) Get(name string) (Connector, bool) {
	for _, c := range r.list {
		if c.Name == name {
			return c, true
		}
	}
	return Connector{}, false
}

// Remove drops a connector and reports whether it was there. The scratch
// database is the caller's to delete: removing a registration should not
// silently delete a file, and for KindSQLite that file is the user's own.
func (r *Registry) Remove(name string) (Connector, bool) {
	for i, c := range r.list {
		if c.Name == name {
			r.list = append(r.list[:i], r.list[i+1:]...)
			return c, true
		}
	}
	return Connector{}, false
}

// List returns every registered connector.
func (r *Registry) List() []Connector {
	out := make([]Connector, len(r.list))
	copy(out, r.list)
	return out
}

// -----------------------------------------------------------------------------
// Identifiers
// -----------------------------------------------------------------------------

// safeIdent reports whether s can be interpolated into SQL as an identifier.
//
// Every identifier that reaches a query is produced by sanitize() from a file
// name or a CSV header, so this should always hold; it is checked at the point
// of use anyway because a header is attacker-controlled input in exactly the
// way §12.3 means when it says web-derived content "cannot author SQL".
func safeIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' {
		return false
	}
	for _, r := range s {
		switch {
		// Uppercase is accepted, and excluding it was a serious bug rather than
		// a strict-but-safe choice. Registering an ordinary CamelCase database —
		//
		//	CREATE TABLE Orders (OrderID INTEGER, Amount REAL, region TEXT);
		//	CREATE TABLE parts  (partId INTEGER, qty INTEGER);
		//
		// returned no error and produced a profile of ONE table with ONE column,
		// parts.qty. Everything else was skipped: tables silently, columns with
		// no record at all, and the `skipped` list only surfaces when zero tables
		// survive. The model then planned over a schema that was not the user's
		// data.
		//
		// Case is not a safety property. Every identifier is double-quoted at the
		// point of use, and SQLite identifiers are case-insensitive but
		// case-preserving, so quoting the name the schema reports is correct.
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// QuoteIdent double-quotes an identifier, refusing anything that is not one.
//
// Exported because internal/compute/hypothesis renders the same identifiers into
// the same statements and kept its own copy of this rule, which drifted twice —
// once over a leading underscore, once over uppercase. Both times a name that
// passed registration could not be queried, and both times the refusal blamed
// the identifier rather than the disagreement. One implementation, for the same
// reason IsFreeText has one.
func QuoteIdent(s string) (string, error) { return quoteIdent(s) }

// quoteIdent double-quotes a checked identifier.
func quoteIdent(s string) (string, error) {
	if !safeIdent(s) {
		return "", fmt.Errorf("connector: refusing unsafe identifier %q", s)
	}
	return `"` + s + `"`, nil
}

// sanitize turns arbitrary text into a safe lower-case identifier.
func sanitize(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "c_" + out
	}
	if len(out) > 64 {
		out = strings.Trim(out[:64], "_")
	}
	return out
}

// uniqueName appends a numeric suffix until the name is unused. Two files
// called `2024/sales.csv` and `2025/sales.csv` both sanitize to `sales`, and
// silently keeping one of them would report on half the data.
func uniqueName(taken map[string]bool, want, fallback string) string {
	if want == "" {
		want = fallback
	}
	if !taken[want] {
		taken[want] = true
		return want
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s_%d", want, i)
		if !taken[candidate] {
			taken[candidate] = true
			return candidate
		}
	}
}
