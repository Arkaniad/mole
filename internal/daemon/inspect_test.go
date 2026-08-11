package daemon_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/daemon"
)

// `mole doctor`'s socket check, which was informational for two milestones after
// the one that needed it landed.
//
// The check calls the same checkPrivateDir the daemon enforces, so these tests are
// as much about the two staying together as about the reporting.

func TestAnExposedSocketDirectoryIsAProblem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// t.TempDir is 0700 and os.MkdirAll applies the umask, so both are forced.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	st := daemon.InspectSocket(filepath.Join(dir, "mole.sock"))
	if st.OK() {
		t.Fatal("a world-reachable socket directory was reported as fine")
	}
	if !strings.Contains(st.Summary(), "group or other") {
		t.Errorf("summary does not say what is wrong: %q", st.Summary())
	}
}

// privateDir is t.TempDir() made 0700.
//
// Not redundant: t.TempDir() inherits the umask, and it is 0755 on the machine
// this was written on — which is also why the pre-existing daemon test for an
// exposed directory passes without doing anything. A test that depends on the
// umask is a test that reports the environment rather than the code.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAPrivateDirectoryWithNoSocketYetIsFine(t *testing.T) {
	st := daemon.InspectSocket(filepath.Join(privateDir(t), "mole.sock"))
	if !st.OK() {
		t.Fatalf("a fresh install was reported as broken: %v", st.Problems)
	}
	if st.Present || st.Live {
		t.Error("a socket that does not exist was reported as present")
	}
	if !strings.Contains(st.Summary(), "not created yet") {
		t.Errorf("summary = %q", st.Summary())
	}
}

// TestALiveSocketIsReportedAsLive, and a stale file is not.
func TestALiveSocketIsReportedAsLive(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "mole.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	st := daemon.InspectSocket(path)
	if !st.OK() {
		t.Fatalf("problems on a correct socket: %v", st.Problems)
	}
	if !st.Live {
		t.Error("a listening daemon was not detected")
	}
	if !strings.Contains(st.Summary(), "daemon listening") {
		t.Errorf("summary = %q", st.Summary())
	}

	// Close the listener but leave the file: the state a killed daemon leaves.
	_ = ln.Close()
	if _, err := os.Stat(path); err == nil {
		st = daemon.InspectSocket(path)
		if st.Live {
			t.Error("a stale socket file was reported as a running daemon")
		}
	}
}

// TestAWorldConnectableSocketIsAProblem, the control the directory does not cover:
// a socket somewhere private whose own mode was loosened.
func TestAWorldConnectableSocketIsAProblem(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "mole.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	st := daemon.InspectSocket(path)
	if st.OK() {
		t.Fatal("a world-connectable socket was reported as fine")
	}
	if !strings.Contains(strings.Join(st.Problems, " "), "group or other can connect") {
		t.Errorf("problems = %v", st.Problems)
	}
}

// TestAPathThatIsNotASocketIsAProblem. `mole serve` refuses this; doctor should
// say so before the user runs it and gets a raw error.
func TestAPathThatIsNotASocketIsAProblem(t *testing.T) {
	path := filepath.Join(privateDir(t), "mole.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := daemon.InspectSocket(path)
	if st.OK() {
		t.Fatal("a regular file at the socket path was reported as fine")
	}
	if !strings.Contains(st.Summary(), "not a socket") {
		t.Errorf("summary = %q", st.Summary())
	}
}

// TestATooLongPathIsNamedAsSuch. sun_path is 108 bytes and bind fails with
// "invalid argument", which says nothing about length.
func TestATooLongPathIsNamedAsSuch(t *testing.T) {
	st := daemon.InspectSocket("/tmp/" + strings.Repeat("x", 200) + "/mole.sock")
	if st.OK() {
		t.Fatal("a path the kernel cannot bind was reported as fine")
	}
	if !strings.Contains(st.Summary(), "kernel allows") {
		t.Errorf("summary = %q", st.Summary())
	}
}
