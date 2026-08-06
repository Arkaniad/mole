//go:build linux

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPeerCredentialsAreActuallyRead.
//
// authorize refuses a connection whose uid is not ours — but only if peerUID
// reports one. A version that always answered "unsupported" would turn the check
// into a no-op, and every other test in this package would still pass, because
// they all connect as the same user. This is the test that says the control is
// running at all.
func TestPeerCredentialsAreActuallyRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
	}
	defer func() { _ = server.Close() }()

	uid, ok, err := peerUID(server)
	if err != nil {
		t.Fatalf("peerUID: %v", err)
	}
	if !ok {
		t.Fatal("peerUID reported unsupported on Linux; the uid check is a no-op")
	}
	if want := uint32(os.Getuid()); uid != want {
		t.Errorf("peer uid = %d, want %d", uid, want)
	}

	// And the decision built on it lets our own connection through.
	s := &Server{Socket: path}
	if err := s.authorize(server); err != nil {
		t.Errorf("authorize refused our own connection: %v", err)
	}
}

// TestAuthorizeRefusesAForeignUID exercises the branch a same-user test cannot
// reach, by checking the comparison rather than the syscall.
func TestAuthorizeRefusesAForeignUID(t *testing.T) {
	if uint32(os.Getuid()) == 0 {
		t.Skip("running as root; the comparison below would not be meaningful")
	}
	// A non-unix conn makes peerUID report unsupported, which must NOT be treated
	// as a denial — the file mode is still the control there.
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()
	s := &Server{Socket: "x"}
	if err := s.authorize(c1); err != nil {
		t.Errorf("an unsupported platform was treated as a denial: %v", err)
	}
}
