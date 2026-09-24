package testutil_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/testutil"
)

// TestSocketDirBindsUnderALongTMPDIR is the regression.
//
// The helper used to hand back os.MkdirTemp("", "mole") unconditionally, which
// inherits $TMPDIR. That is fine on Linux CI, where $TMPDIR is /tmp, and fine
// on a bare macOS shell, where it is ~50 bytes. It is not fine one level down:
// `nix develop` nests a directory inside the per-user TMPDIR, and the socket
// path then crosses 104 and bind fails with EINVAL — an error that names
// neither the length nor the path.
//
// Binding for real rather than asserting on len(dir): the budget arithmetic is
// the thing under test, and a length assertion would pass against a budget that
// is itself wrong.
func TestSocketDirBindsUnderALongTMPDIR(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 60))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatalf("staging a long TMPDIR: %v", err)
	}
	t.Setenv("TMPDIR", long)

	// The longest suffix any caller in this repo appends, so the test fails
	// before the callers do.
	dir := filepath.Join(testutil.SocketDir(t), "shared")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "mole.sock")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("bind %q (%d bytes): %v", path, len(path), err)
	}
	defer l.Close()
}

// TestSocketDirPrefersTMPDIRWhenItFits guards the other half.
//
// Falling back to /tmp unconditionally would be the easy fix and the wrong one:
// $TMPDIR is where the environment wants temporary files, it is what gets
// cleaned up, and on a sandboxed builder it can be the only writable directory.
// The fallback has to stay the exception.
func TestSocketDirPrefersTMPDIRWhenItFits(t *testing.T) {
	// Staged under /tmp rather than t.TempDir(): t.TempDir() embeds the test
	// name and is itself past the budget on macOS, which made this test skip
	// exactly where the helper is hardest to get right.
	short, err := os.MkdirTemp("/tmp", "short")
	if err != nil {
		t.Fatalf("staging a short TMPDIR: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	t.Setenv("TMPDIR", short)

	if dir := testutil.SocketDir(t); !strings.HasPrefix(dir, short) {
		t.Errorf("SocketDir = %q, want it under $TMPDIR %q — a short TMPDIR was "+
			"abandoned for the fallback", dir, short)
	}
}
