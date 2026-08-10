package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M8 slice 0, at the command level.
//
// cli_test.go's exec() gives every call a fresh database directory, which is
// right for the commands it drives and wrong for these: `connect add` then
// `connect list` is one session's worth of state, and a helper that isolated
// them from each other would assert that registration does nothing.

func connectExec(t *testing.T, home string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("MOLE_CONFIG_DIR", t.TempDir())
	t.Setenv("MOLE_DB", filepath.Join(home, "mole.db"))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	// Two statements, not `return out.String(), root.Execute()`: return values
	// are evaluated left to right, so the one-liner captures the buffer before
	// anything has written to it and every output assertion sees "".
	err := root.Execute()
	return out.String(), err
}

func exportsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"jan.csv": "region,units,revenue,rep_note\n" +
			"north,10,1050,\"asked to split the invoice across two cost centres\"\n" +
			"south,4,220.5,\"straightforward renewal with no changes requested\"\n",
		"feb.csv": "region,units,revenue,rep_note\n" +
			"east,3,91.5,\"wanted the delivery held until the new quarter opened\"\n",
		"README.md": "not data",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestConnectAddThenListAndSchema drives the sequence a user actually types.
func TestConnectAddThenListAndSchema(t *testing.T) {
	home := t.TempDir()
	src := exportsDir(t)

	out, err := connectExec(t, home, "connect", "add", "exports", src)
	if err != nil {
		t.Fatalf("connect add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "2 table(s), 3 row(s)") {
		t.Fatalf("add did not report what it imported:\n%s", out)
	}

	out, err = connectExec(t, home, "connect", "list")
	if err != nil {
		t.Fatalf("connect list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "exports") || !strings.Contains(out, "import") {
		t.Fatalf("list does not show the registration made by the previous command:\n%s", out)
	}

	out, err = connectExec(t, home, "connect", "schema", "exports")
	if err != nil {
		t.Fatalf("connect schema: %v\n%s", err, out)
	}
	for _, want := range []string{"exports.jan", "exports.feb", "revenue", "real"} {
		if !strings.Contains(out, want) {
			t.Errorf("schema is missing %q:\n%s", want, out)
		}
	}
	// §12.1's exclusion has to be visible. A profile that silently omitted the
	// range of a prose column would read as an incomplete import rather than
	// as the privacy boundary doing its job.
	if !strings.Contains(out, "free text") {
		t.Errorf("schema does not mark the excluded column:\n%s", out)
	}
	// And the values themselves must not be in the output. This is the command
	// a user runs to inspect a source, so it is the most likely place for a row
	// to escape into a terminal, a screenshot, or a bug report.
	if strings.Contains(out, "cost centres") || strings.Contains(out, "renewal") {
		t.Errorf("schema printed the contents of a free-text column:\n%s", out)
	}
}

// TestConnectAddRefusesToOverwriteSilently. Re-importing is a destructive
// rebuild of the scratch database, and a name that already means something is
// not the place to do it without being asked.
func TestConnectAddRefusesToOverwriteSilently(t *testing.T) {
	home := t.TempDir()
	src := exportsDir(t)

	if _, err := connectExec(t, home, "connect", "add", "exports", src); err != nil {
		t.Fatal(err)
	}
	out, err := connectExec(t, home, "connect", "add", "exports", src)
	if err == nil {
		t.Fatalf("the second add succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--replace") {
		t.Errorf("the refusal does not say how to proceed: %v", err)
	}
	if _, err := connectExec(t, home, "connect", "add", "exports", src, "--replace"); err != nil {
		t.Fatalf("--replace was refused: %v", err)
	}
}

// TestConnectRemoveLeavesTheCopyUnlessAsked. Removing a registration must not
// delete a file by itself — for an attached database that file is the user's
// own, and for an imported one it is still a copy of their data.
func TestConnectRemoveLeavesTheCopyUnlessAsked(t *testing.T) {
	home := t.TempDir()
	scratch := filepath.Join(home, "connectors", "exports.db")

	if _, err := connectExec(t, home, "connect", "add", "exports", exportsDir(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("no scratch database at %s: %v", scratch, err)
	}

	out, err := connectExec(t, home, "connect", "remove", "exports")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("remove deleted the imported copy without being asked: %v", err)
	}
	if !strings.Contains(out, "--purge") {
		t.Errorf("remove does not say the copy is still there:\n%s", out)
	}

	if _, err := connectExec(t, home, "connect", "add", "exports", exportsDir(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := connectExec(t, home, "connect", "remove", "exports", "--purge"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("--purge left the copy at %s (err=%v)", scratch, err)
	}
}

// TestConnectIsScopedByDB. --db separates one install from another, and a
// registry that ignored it would leak sources between them — including in
// tests, which is how a suite ends up reading a developer's real data.
func TestConnectIsScopedByDB(t *testing.T) {
	src := exportsDir(t)
	first, second := t.TempDir(), t.TempDir()

	if _, err := connectExec(t, first, "connect", "add", "exports", src); err != nil {
		t.Fatal(err)
	}
	out, err := connectExec(t, second, "connect", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "exports") {
		t.Fatalf("a source registered under one --db is visible under another:\n%s", out)
	}
	if !strings.Contains(out, "no sources registered") {
		t.Errorf("an empty registry does not say how to add one:\n%s", out)
	}
}
