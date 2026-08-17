// Package testutil holds helpers shared by tests in more than one package.
package testutil

import (
	"os"
	"testing"
)

// SocketDir is t.TempDir() with a name short enough to bind a socket inside.
//
// A unix socket path is capped at 104 bytes on macOS, and t.TempDir() spends
// most of that before the caller gets a say: it names the directory after the
// test. TestAWorldConnectableSocketIsAProblem under a macOS $TMPDIR reaches 110,
// and bind fails with EINVAL — "invalid argument", which says nothing about
// length and sends you looking at the socket code instead.
//
// Worth a helper rather than shorter test names, because the failure is
// invisible exactly where the tests usually run: $TMPDIR is /tmp on Linux, so
// CI stays green and only a macOS checkout sees it. Left to naming discipline,
// the next descriptive test name brings it back.
//
// The mode matches t.TempDir()'s 0700. Callers that care assert or chmod
// anyway, since a test that depends on the umask reports the environment rather
// than the code.
func SocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mole")
	if err != nil {
		t.Fatalf("testutil: temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
