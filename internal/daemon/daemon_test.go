package daemon_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/daemon"
)

func echoHandler() daemon.Handler {
	return daemon.HandlerFunc(func(ctx context.Context, c net.Conn) {
		_, _ = io.Copy(c, c)
	})
}

func newServer(t *testing.T, h daemon.Handler) *daemon.Server {
	t.Helper()
	return &daemon.Server{
		Socket:        filepath.Join(t.TempDir(), "sub", "mole.sock"),
		Handler:       h,
		ShutdownGrace: 5 * time.Second,
	}
}

// serve starts the server and returns a stop function.
func serve(t *testing.T, s *daemon.Server) func() {
	t.Helper()
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
	}
}

// TestTheSocketIsNotReachableByOtherUsers is §3.5's control, checked as one.
//
// Two independent properties, because each alone has a hole. net.Listen applies
// the process umask, so the socket can be created 0755 and be connectable in the
// window before Chmod runs — a private parent directory closes that window. And
// the socket's own mode still matters, because a directory can be loosened later
// by an install script or by someone moving the socket somewhere shared.
func TestTheSocketIsNotReachableByOtherUsers(t *testing.T) {
	s := newServer(t, echoHandler())
	stop := serve(t, s)
	defer stop()

	si, err := os.Stat(s.Socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := si.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode is %#o, want 0600", perm)
	}

	di, err := os.Stat(filepath.Dir(s.Socket))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket directory mode is %#o; group or other can reach the socket", perm)
	}
}

// TestAStaleSocketIsReplacedAndALiveOneIsNot.
//
// A daemon that crashed leaves the file behind, and refusing to start until
// somebody deletes it by hand is a bad first experience. But unlinking without
// checking would silently steal the socket from a RUNNING daemon: the old one
// keeps its listener on an unlinked inode, every client reaches the new one, and
// two processes then hold write locks on the same database.
func TestAStaleSocketIsReplacedAndALiveOneIsNot(t *testing.T) {
	// A private subdirectory: Listen refuses a directory group or other can
	// reach, and t.TempDir() is 0755 under the usual umask.
	dir := filepath.Join(t.TempDir(), "priv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mole.sock")

	// Stale: a socket file with nothing accepting on it.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close() // Go's unix listener unlinks on Close, so recreate the file.
	if f, err := os.Create(path); err == nil {
		_ = f.Close()
	}
	// A plain file is not a socket, and must be refused rather than removed —
	// deleting an unrelated file because it sat at the socket path is worse than
	// failing to start.
	plain := &daemon.Server{Socket: path, Handler: echoHandler()}
	if _, err := plain.Listen(); err == nil {
		t.Error("Listen accepted a path occupied by a regular file")
	} else if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("unhelpful error for a non-socket path: %v", err)
	}
	_ = os.Remove(path)

	// Now a genuinely stale socket: created, then its process "died".
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := stale.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket file did not survive: %v", err)
	}

	s := &daemon.Server{Socket: path, Handler: echoHandler(), ShutdownGrace: 5 * time.Second}
	stop := serve(t, s)

	// And a second daemon on a LIVE socket must refuse.
	second := &daemon.Server{Socket: path, Handler: echoHandler()}
	if _, err := second.Listen(); err == nil {
		t.Error("a second daemon took over a live socket")
	} else if !strings.Contains(err.Error(), "already served") {
		t.Errorf("unclear error for a live socket: %v", err)
	}
	stop()

	// The first daemon removed its socket on the way out.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket outlived a clean shutdown: %v", err)
	}
}

// TestConnectionsAreServedAndShutdownIsClean.
func TestConnectionsAreServedAndShutdownIsClean(t *testing.T) {
	s := newServer(t, echoHandler())
	stop := serve(t, s)

	c, err := net.Dial("unix", s.Socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 5)
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "ping\n" {
		t.Errorf("got %q", buf)
	}
	_ = c.Close()
	stop()
}

// TestShutdownClosesAnIdleConnection.
//
// A handler blocked reading from a client that never speaks again must not hold
// the daemon open. Shutdown closes live connections first precisely so their
// handlers unblock — and only then waits, because what it is really waiting for
// is each session releasing its budget holds.
func TestShutdownClosesAnIdleConnection(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	blocked := daemon.HandlerFunc(func(ctx context.Context, c net.Conn) {
		defer wg.Done()
		// Blocks until the connection is closed under it.
		_, _ = io.Copy(io.Discard, c)
	})

	s := newServer(t, blocked)
	stop := serve(t, s)

	c, err := net.Dial("unix", s.Socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Let the handler get into its read.
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown hung on an idle connection")
	}

	handlerDone := make(chan struct{})
	go func() { wg.Wait(); close(handlerDone) }()
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Error("the handler was never unblocked")
	}
}

func TestListenRefusesAnEmptyPath(t *testing.T) {
	s := &daemon.Server{Handler: echoHandler()}
	if _, err := s.Listen(); err == nil {
		t.Error("Listen accepted an empty socket path")
	}
}

// TestListenRejectsAnOverlongPathClearly.
//
// sun_path is a fixed 108 bytes in the kernel's address struct, and going over it
// fails as "bind: invalid argument" — an error that says nothing about length and
// sent me looking at permissions first. The path comes from a flag or from
// XDG_RUNTIME_DIR, so this is a configuration mistake a user can fix, once told.
func TestListenRejectsAnOverlongPathClearly(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120)+".sock")
	s := &daemon.Server{Socket: long, Handler: echoHandler()}

	_, err := s.Listen()
	if err == nil {
		t.Fatal("Listen accepted a path longer than sun_path")
	}
	msg := err.Error()
	for _, want := range []string{"socket path", "--socket"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q, so it does not say how to fix it: %v", want, err)
		}
	}
	if strings.Contains(msg, "invalid argument") {
		t.Errorf("the raw kernel error leaked through instead of an explanation: %v", err)
	}
}

// TestListenRefusesADirectoryOtherUsersCanReach.
//
// os.MkdirAll does NOT make an existing directory private — it returns nil
// without touching mode or owner. So Listen's claim that a private parent closes
// the umask window between net.Listen and Chmod held only for a directory it
// created itself, and the common case is one that already exists:
// `--socket /tmp/mole.sock` on a 1777 /tmp produced a 0755 socket,
// world-connectable until Chmod ran.
//
// The original test only ever used a directory Listen created, which is why the
// gap shipped.
func TestListenRefusesADirectoryOtherUsersCanReach(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o770, 0o707} {
		dir := filepath.Join(t.TempDir(), "shared")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		// MkdirAll honours the umask, so set the mode explicitly.
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}

		s := &daemon.Server{Socket: filepath.Join(dir, "m.sock"), Handler: echoHandler()}
		ln, err := s.Listen()
		if err == nil {
			_ = ln.Close()
			t.Errorf("mode %#o: Listen accepted a directory other users can reach", mode)
			continue
		}
		if !strings.Contains(err.Error(), "socket directory") {
			t.Errorf("mode %#o: unclear error: %v", mode, err)
		}
	}

	// And a private one is still accepted, or the check is just a refusal.
	dir := filepath.Join(t.TempDir(), "priv")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &daemon.Server{Socket: filepath.Join(dir, "m.sock"), Handler: echoHandler()}
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("a 0700 directory was refused: %v", err)
	}
	_ = ln.Close()
}
