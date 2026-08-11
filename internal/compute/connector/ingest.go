package connector

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ingest registers a source: a single file, a directory of files, or an
// existing SQLite database.
//
// A directory becomes one table per readable file, because "point mole at my
// data folder" is the shape people actually have — a year of monthly exports,
// not one tidy file. Discovery is one level deep and deliberately so: recursing
// would pull in whatever else lives under the path, and a research tool that
// hoovers up subdirectories nobody meant to expose is a privacy bug in the
// milestone whose entire subject is the privacy boundary.
//
// scratchDB is where imported files land. It is ignored for an existing
// database, which is attached in place.
func Ingest(ctx context.Context, name, source, scratchDB string) (Connector, error) {
	if sanitize(name) != name || !safeIdent(name) {
		return Connector{}, fmt.Errorf(
			"connector: %q is not a usable name; use lower-case letters, digits and underscores", name)
	}

	abs, err := filepath.Abs(source)
	if err != nil {
		return Connector{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Connector{}, fmt.Errorf("connector: %w", err)
	}

	c := Connector{Name: name, Source: abs, ImportedAt: time.Now().UTC()}

	if !info.IsDir() && isSQLiteFile(abs) {
		c.Kind, c.DBPath = KindSQLite, abs
		if c.Tables, c.Skipped, err = profileDatabase(ctx, c); err != nil {
			return Connector{}, err
		}
		return c, nil
	}

	files, err := discover(abs, info.IsDir())
	if err != nil {
		return Connector{}, err
	}
	c.Kind, c.DBPath = KindImport, scratchDB
	if c.Tables, c.Skipped, err = importFiles(ctx, c, files); err != nil {
		return Connector{}, err
	}
	return c, nil
}

func isSQLiteFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".db", ".sqlite", ".sqlite3":
		return true
	}
	return false
}

// readableExt is what importFiles knows how to parse. Parquet is the obvious
// omission and is left out on purpose: SQLite cannot read it, so it would mean
// a decoder plus a type mapping, and slice 0 is not the place to find out how
// much of one is needed.
func readableExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv", ".tsv", ".jsonl", ".ndjson":
		return true
	}
	return false
}

func discover(root string, isDir bool) ([]string, error) {
	if !isDir {
		if !readableExt(root) {
			return nil, fmt.Errorf("connector: %s is not a readable format "+
				"(want .csv, .tsv, .jsonl, .ndjson, or a .db/.sqlite database)", filepath.Base(root))
		}
		return []string{root}, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("connector: %w", err)
	}
	var files []string
	var skipped int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		if readableExt(p) {
			files = append(files, p)
			continue
		}
		skipped++
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %s holds no .csv/.tsv/.jsonl files (%d other entries ignored)",
			ErrNoData, root, skipped)
	}
	sort.Strings(files)
	return files, nil
}

// -----------------------------------------------------------------------------
// Import
// -----------------------------------------------------------------------------

// importFiles builds the scratch database. This is the only place a writable
// connection to connector data is ever opened, and it is closed before Ingest
// returns.
func importFiles(ctx context.Context, c Connector, files []string) ([]Table, []string, error) {
	if dir := filepath.Dir(c.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("connector: scratch dir: %w", err)
		}
	}
	// Rebuild from scratch. A re-import that merged into a previous one would
	// report row counts for files that are no longer in the folder.
	// Built at a temporary path and moved into place only once every file has
	// been read.
	//
	// It used to delete the destination first and write in place, so a failed
	// re-import left no scratch database while `connectors.json` still pointed at
	// one — `mole connect add sales --replace empty.csv` after a good import took
	// the connector from working to unopenable, and the error blamed an active
	// writer.
	building := c.DBPath + ".building"
	for _, stale := range []string{building, building + "-wal", building + "-shm"} {
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("connector: clear %s: %w", stale, err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+building+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, nil, fmt.Errorf("connector: create scratch database: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	taken := map[string]bool{}
	var tables []Table
	var skipped []string
	for i, f := range files {
		base := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		tbl := uniqueName(taken, sanitize(base), fmt.Sprintf("table_%d", i+1))

		t, dropped, err := importOne(ctx, db, tbl, f)
		if err != nil {
			return nil, nil, fmt.Errorf("connector: %s: %w", filepath.Base(f), err)
		}
		skipped = append(skipped, dropped...)
		tables = append(tables, t)
	}
	if len(tables) == 0 {
		return nil, nil, ErrNoData
	}
	for i := range tables {
		if err := profileTable(ctx, db, &tables[i]); err != nil {
			return nil, nil, err
		}
	}

	// Every file read and every table profiled: only now does the old database
	// stop existing.
	if err := db.Close(); err != nil {
		return nil, nil, fmt.Errorf("connector: close scratch database: %w", err)
	}
	for _, old := range []string{c.DBPath, c.DBPath + "-wal", c.DBPath + "-shm"} {
		if err := os.Remove(old); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("connector: replace scratch database: %w", err)
		}
	}
	if err := os.Rename(building, c.DBPath); err != nil {
		return nil, nil, fmt.Errorf("connector: install scratch database: %w", err)
	}
	// The WAL is checkpointed by Close, so only the main file needs moving; any
	// sidecars left behind belong to the discarded build.
	for _, side := range []string{building + "-wal", building + "-shm"} {
		_ = os.Remove(side)
	}
	return tables, skipped, nil
}

// importOne reads a file twice: once to infer column types, once to insert.
//
// Two passes rather than one because the alternative is buffering the file in
// memory to decide its types, and the whole appeal of a local connector is that
// it works on the export someone actually has rather than a small enough one.
// Reading a local file twice costs nothing worth optimising.
func importOne(ctx context.Context, db *sql.DB, table, path string) (Table, []string, error) {
	header, types, err := inferFile(ctx, path)
	if err != nil {
		return Table{}, nil, err
	}
	if len(header) == 0 {
		return Table{}, nil, errors.New("no columns")
	}

	cols := make([]Column, len(header))
	taken := map[string]bool{}
	for i, h := range header {
		cols[i] = Column{
			Name: uniqueName(taken, sanitize(h), fmt.Sprintf("col_%d", i+1)),
			// Capped like every other scalar the profile keeps. A header is
			// untrusted text of unbounded length that reaches a model prompt, so
			// one crafted cell could otherwise inflate every planning call.
			Label: truncateScalar(strings.TrimSpace(h)),
			Type:  types[i],
		}
		// A header that sanitizes to exactly its own text carries no
		// information the identifier does not.
		if cols[i].Label == cols[i].Name {
			cols[i].Label = ""
		}
	}

	if err := createTable(ctx, db, table, cols); err != nil {
		return Table{}, nil, err
	}
	rows, ragged, err := insertRows(ctx, db, table, cols, path)
	if err != nil {
		return Table{}, nil, err
	}
	var skipped []string
	if ragged > 0 {
		skipped = append(skipped, fmt.Sprintf(
			"%s: %d row(s) had more fields than the header; the extras were dropped",
			filepath.Base(path), ragged))
	}
	return Table{Name: table, Origin: filepath.Base(path), Rows: rows, Columns: cols}, skipped, nil
}

func createTable(ctx context.Context, db *sql.DB, table string, cols []Column) error {
	qt, err := quoteIdent(table)
	if err != nil {
		return err
	}
	var defs []string
	for _, c := range cols {
		qc, err := quoteIdent(c.Name)
		if err != nil {
			return err
		}
		defs = append(defs, qc+" "+sqlType(c.Type))
	}
	_, err = db.ExecContext(ctx, "CREATE TABLE "+qt+" ("+strings.Join(defs, ", ")+")")
	return err
}

// sqlType maps an inferred type onto a SQLite declared type.
//
// Timestamps are TEXT holding RFC3339 in UTC. SQLite has no date type, and
// normalising at import is what makes MIN/MAX and ORDER BY on a time column
// mean what a reader expects — lexical order over mixed offsets does not.
func sqlType(t ColumnType) string {
	switch t {
	case TypeInteger, TypeBool:
		return "INTEGER"
	case TypeReal:
		return "REAL"
	default:
		return "TEXT"
	}
}

// insertRows returns the number of rows written and the number that carried
// more fields than the header.
func insertRows(ctx context.Context, db *sql.DB, table string, cols []Column, path string) (int64, int64, error) {
	qt, err := quoteIdent(table)
	if err != nil {
		return 0, 0, err
	}
	names := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		if names[i], err = quoteIdent(c.Name); err != nil {
			return 0, 0, err
		}
		marks[i] = "?"
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO "+qt+" ("+strings.Join(names, ", ")+") VALUES ("+strings.Join(marks, ", ")+")")
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()

	var n, ragged int64
	err = scanFile(ctx, path, func(_ []string, rec []string) error {
		if len(rec) > len(cols) {
			// Extra fields past the header are dropped, which is the only thing
			// that can be done with them — but silently dropping data is how a
			// misaligned export reads as a clean import.
			ragged++
		}
		args := make([]any, len(cols))
		for i := range cols {
			var raw string
			if i < len(rec) {
				raw = rec[i]
			}
			args[i] = coerce(raw, cols[i].Type)
		}
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return err
		}
		n++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return n, ragged, nil
}

// coerce converts a raw field to the column's storage type, falling back to NULL
// rather than to a zero. A blank cell in a revenue column is missing data, and
// storing it as 0 would move every mean the aggregation gate later reports.
func coerce(raw string, t ColumnType) any {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	switch t {
	case TypeInteger:
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	case TypeReal:
		if v, err := strconv.ParseFloat(s, 64); err == nil &&
			!math.IsInf(v, 0) && !math.IsNaN(v) {
			return v
		}
	case TypeBool:
		if v, ok := parseBool(s); ok {
			if v {
				return int64(1)
			}
			return int64(0)
		}
	case TypeTime:
		if ts, ok := parseTime(s); ok {
			return ts.UTC().Format(time.RFC3339)
		}
	}
	// Inference said this parses and it does not. Keeping the text is the
	// honest outcome: the value was in the file.
	return s
}

// -----------------------------------------------------------------------------
// Type inference
// -----------------------------------------------------------------------------

// candidate is the set of types a column could still be.
type candidate struct {
	integer, real, boolean, timestamp bool
	seen                              bool // any non-empty value at all
}

func newCandidate() candidate {
	return candidate{integer: true, real: true, boolean: true, timestamp: true}
}

func (c *candidate) observe(raw string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return // empty is NULL, and NULL is compatible with every type
	}
	c.seen = true
	if c.integer {
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			c.integer = false
		}
	}
	if c.real {
		// ParseFloat accepts "inf", "+Inf" and "NaN". A CSV column of
		// 1.5, inf, 2.5 inferred as real, stored +Inf, and reported max="+Inf"
		// to the planner — after which every moment computed over it is Inf or
		// NaN. A cell spelling a non-finite value is text, not a number.
		if f, err := strconv.ParseFloat(s, 64); err != nil ||
			math.IsInf(f, 0) || math.IsNaN(f) {
			c.real = false
		}
	}
	if c.boolean {
		if _, ok := parseBool(s); !ok {
			c.boolean = false
		}
	}
	if c.timestamp {
		if _, ok := parseTime(s); !ok {
			c.timestamp = false
		}
	}
}

// resolve picks the narrowest surviving type.
//
// Order matters. Integer beats boolean so a column of 0s and 1s stays a number
// you can sum — parseBool deliberately does not accept "0"/"1" either, since
// treating a count column as a flag is the more damaging mistake. Timestamp
// beats integer so a column of bare years is not turned into dates: parseTime
// requires a full date, so "2024" never reaches it.
func (c candidate) resolve() ColumnType {
	switch {
	case !c.seen:
		return TypeText // all NULL; text is the type that constrains nothing
	case c.integer:
		return TypeInteger
	case c.real:
		return TypeReal
	case c.boolean:
		return TypeBool
	case c.timestamp:
		return TypeTime
	default:
		return TypeText
	}
}

func parseBool(s string) (bool, bool) {
	switch strings.ToLower(s) {
	case "true", "t", "yes", "y":
		return true, true
	case "false", "f", "no", "n":
		return false, true
	}
	return false, false
}

var timeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02",
}

func parseTime(s string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// inferFile is pass one: header plus a type per column.
func inferFile(ctx context.Context, path string) ([]string, []ColumnType, error) {
	var header []string
	var cands []candidate

	err := scanFile(ctx, path, func(hdr []string, rec []string) error {
		if header == nil {
			header = hdr
			cands = make([]candidate, len(hdr))
			for i := range cands {
				cands[i] = newCandidate()
			}
		}
		for i := range cands {
			if i < len(rec) {
				cands[i].observe(rec[i])
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		// A file with a header and no data rows still has a schema, and a
		// schema with no rows is a legitimate thing to register.
		if header, err = headerOnly(path); err != nil {
			return nil, nil, err
		}
		cands = make([]candidate, len(header))
	}
	types := make([]ColumnType, len(cands))
	for i, c := range cands {
		types[i] = c.resolve()
	}
	return header, types, nil
}

// -----------------------------------------------------------------------------
// Scanning
// -----------------------------------------------------------------------------

// scanFile calls fn once per data row, with the header and the row's fields
// positioned to match it. Both passes use it so inference and insertion can
// never disagree about what a row is.
func scanFile(ctx context.Context, path string, fn func(header, rec []string) error) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".ndjson":
		return scanJSONL(ctx, path, fn)
	default:
		return scanDelimited(ctx, path, fn)
	}
}

func scanDelimited(ctx context.Context, path string, fn func(header, rec []string) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReaderSize(f, 64<<10))
	r.Comma = delimiterFor(path)
	// Ragged rows are normal in exports. FieldsPerRecord=-1 accepts them and
	// the callers pad or truncate to the header, which beats refusing the file.
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	r.LazyQuotes = true

	header, err := r.Read()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	header = append([]string(nil), header...)
	header = stripBOM(header)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read row: %w", err)
		}
		if err := fn(header, rec); err != nil {
			return err
		}
	}
}

func delimiterFor(path string) rune {
	if strings.EqualFold(filepath.Ext(path), ".tsv") {
		return '\t'
	}
	return ','
}

func stripBOM(header []string) []string {
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	return header
}

// scanJSONL flattens objects to the union of their keys, in first-seen order.
//
// Lines are objects, not rows, and different lines may carry different keys —
// so the header has to be discovered before any row can be positioned against
// it. The file is read once here per pass, and the key order is stable because
// both passes see the file in the same order.
func scanJSONL(ctx context.Context, path string, fn func(header, rec []string) error) error {
	keys, err := jsonlKeys(ctx, path)
	if err != nil || len(keys) == 0 {
		return err
	}
	index := make(map[string]int, len(keys))
	for i, k := range keys {
		index[k] = i
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	first := true
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(sc.Text())
		if first {
			// The BOM strip was wired into the CSV path only, so a BOM-prefixed
			// .jsonl failed with "invalid character 'ï'" while the same bytes in
			// a .csv imported fine.
			line, first = strings.TrimPrefix(line, "\ufeff"), false
		}
		if line == "" {
			continue
		}
		// UseNumber, so an integer arrives as its own literal text.
		//
		// json.Unmarshal into map[string]any decodes every number as float64,
		// which silently corrupted ids: 1234567890123456789 and
		// 1234567890123456788 both stored as 1234567890123456800, and the profile
		// then reported two distinct values over three rows. Snowflake and order
		// ids are exactly the shape a JSONL export carries.
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil {
			return fmt.Errorf("parse line: %w", err)
		}
		rec := make([]string, len(keys))
		for k, v := range obj {
			if i, ok := index[k]; ok {
				rec[i] = jsonScalar(v)
			}
		}
		if err := fn(keys, rec); err != nil {
			return err
		}
	}
	return sc.Err()
}

func jsonlKeys(ctx context.Context, path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var keys []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	first := true
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSpace(sc.Text())
		if first {
			line, first = strings.TrimPrefix(line, "\ufeff"), false
		}
		if line == "" {
			continue
		}
		// Ordered decode: encoding/json unmarshals objects into a map, which
		// has no order, so the key sequence is read from the token stream.
		dec := json.NewDecoder(strings.NewReader(line))
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("parse line: %w", err)
		}
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return nil, errors.New("every line must be a JSON object")
		}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			name, _ := k.(string)
			if !seen[name] {
				seen[name] = true
				keys = append(keys, name)
			}
			var discard any
			if err := dec.Decode(&discard); err != nil {
				return nil, err
			}
		}
	}
	return keys, sc.Err()
}

// jsonScalar renders a JSON value as the text the inference pass will see.
// Nested objects and arrays are kept as their JSON, which reliably infers as
// text — a column of nested documents is not something to aggregate over.
func jsonScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		// The literal as written, so nothing is lost between the file and the
		// column.
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

// headerOnly reads the column names from a file with no data rows.
func headerOnly(path string) ([]string, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".ndjson":
		return jsonlKeys(context.Background(), path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	r.Comma = delimiterFor(path)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err == io.EOF {
		return nil, errors.New("file is empty")
	}
	if err != nil {
		return nil, err
	}
	return stripBOM(header), nil
}
