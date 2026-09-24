// Package testutil holds helpers shared by tests in more than one package.
package testutil

import (
	"os"
	"testing"
)

const (
	// MaxSocketPath is the shorter of the two sun_path limits: 104 on macOS,
	// 108 on Linux. Budgeting against the smaller one keeps a single behaviour
	// on both, and the four spare bytes on Linux buy nothing worth branching
	// for.
	//
	// The limit is on the WHOLE path, not on any one component, which is why a
	// directory helper has to care about it at all.
	MaxSocketPath = 104

	// callerBudget is the room SocketDir leaves for what the caller appends to
	// the directory it returns. The longest in this repo is "/shared/mole.sock"
	// at 17 bytes; 32 leaves margin so the next caller does not have to come
	// back and revise this.
	callerBudget = 32

	// nameBudget is what os.MkdirTemp spends on top of the base directory:
	// a separator, the "mole" prefix, and the random suffix it appends. The
	// suffix is a formatted uint32, so 10 digits is its ceiling.
	nameBudget = len("/mole") + 10
)

// SocketDir is a temp dir short enough to bind a unix socket inside.
//
// t.TempDir() cannot be used here: it names the directory after the test, and
// one descriptive Go test name can spend 60 of the 104 bytes on its own.
//
// $TMPDIR is not reliably short either, which is the part that keeps being
// rediscovered. macOS hands out a ~50-byte per-user TMPDIR, `nix develop` and
// nix-shell nest another ~15-30 inside it, and CI runners nest more still. Past
// roughly 75 bytes, os.MkdirTemp("", "mole") returns a base that cannot hold a
// socket, and bind fails with EINVAL — "invalid argument", which names neither
// the length nor the path and sends you reading socket code instead. So when
// $TMPDIR is too long to work in, fall back to /tmp, which is short by
// construction.
//
// Worth a helper rather than shorter test names, because the failure is
// invisible exactly where the tests usually run: $TMPDIR is /tmp on Linux CI,
// so CI stays green and only a macOS checkout — or a nix build — sees it. Left
// to naming discipline, the next descriptive test name brings it back.
//
// The directory is 0700, matching t.TempDir(). That matters most for the /tmp
// fallback, since /tmp is world-writable: os.MkdirTemp creates the directory
// itself with 0700 and a random name, so nothing else can be sitting at the
// path. Callers that assert on modes chmod it themselves, since a test that
// depends on the umask reports the environment rather than the code.
func SocketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp(socketTempBase(), "mole")
	if err != nil {
		t.Fatalf("testutil: temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// Belt and braces: if even the fallback could not find room, say so here
	// rather than let the caller's bind fail with EINVAL three frames away.
	if len(dir)+callerBudget > MaxSocketPath {
		t.Fatalf("testutil: socket dir %q is %d bytes, leaving under %d for the "+
			"socket name against a %d-byte kernel limit",
			dir, len(dir), callerBudget, MaxSocketPath)
	}
	return dir
}

// socketTempBase is "" — meaning os.MkdirTemp uses $TMPDIR — unless $TMPDIR is
// too long to bind a socket under, in which case it is /tmp.
//
// Preferring $TMPDIR matters: it is where the environment wants temporary files,
// it is what gets cleaned up, and on a sandboxed builder it may be the only
// writable directory. /tmp is the exception, not the default.
func socketTempBase() string {
	if len(os.TempDir())+nameBudget+callerBudget <= MaxSocketPath {
		return ""
	}
	return "/tmp"
}
