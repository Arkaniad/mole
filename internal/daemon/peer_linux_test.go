//go:build linux

package daemon

import (
	"errors"
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

// TestAForeignUIDIsRefused covers the §3.5 control itself.
//
// The test that used to sit here was named for this and did something else: it
// handed authorize a net.Pipe, which makes peerUID report "unsupported", and
// asserted the connection was ALLOWED. The uid comparison — the entire point —
// had no coverage, and could not have, because every connection a test makes
// carries the test's own uid. Hence authorizeUID, which takes the decision's
// inputs directly.
func TestAForeignUIDIsRefused(t *testing.T) {
	self := uint32(os.Getuid())

	if err := authorizeUID(self+1, true, nil, self); err == nil {
		t.Error("a connection from another uid was allowed")
	}
	if err := authorizeUID(0, true, nil, self); err == nil && self != 0 {
		t.Error("a root connection was allowed; root's config and credentials are not ours")
	}
	if err := authorizeUID(self, true, nil, self); err != nil {
		t.Errorf("our own uid was refused: %v", err)
	}
	// A check that could not run is a refusal, not a pass.
	if err := authorizeUID(0, false, errors.New("boom"), self); err == nil {
		t.Error("a failed credential read was treated as authorization")
	}
	// An unsupported platform falls back to the file mode rather than denying
	// every connection — a denial dressed as security.
	if err := authorizeUID(0, false, nil, self); err != nil {
		t.Errorf("an unsupported platform was treated as a denial: %v", err)
	}
}
