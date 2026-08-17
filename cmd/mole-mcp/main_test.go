package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/testutil"
)

// TestStdinEOFDoesNotDiscardTheReplyInFlight.
//
// The subtle half of a byte pump. An editor closing its end of stdin means "no
// more requests", not "throw away the response you are mid-way through sending".
// A pump that returns as soon as stdin ends truncates the last reply — and the
// symptom is an MCP client that hangs waiting for a result the daemon already
// sent, which looks like a daemon bug from every angle except this one.
func TestStdinEOFDoesNotDiscardTheReplyInFlight(t *testing.T) {
	// A real unix socket, not net.Pipe: the behaviour under test is half-close,
	// and net.Pipe does not have one. Testing it over a pipe measures the
	// fallback path and nothing that ships.
	client, server := socketPair(t)

	// The "daemon": waits for the request, waits a beat, then answers slowly.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = server.Close() }()
		buf := make([]byte, 64)
		if _, err := server.Read(buf); err != nil {
			return
		}
		// The reply begins only after stdin has certainly ended.
		time.Sleep(100 * time.Millisecond)
		for _, part := range []string{`{"jsonrpc":"2.0",`, `"id":1,`, `"result":{"ok":true}}`, "\n"} {
			if _, err := server.Write([]byte(part)); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var out bytes.Buffer
	stdin := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")

	done := make(chan error, 1)
	go func() { done <- pump(client, stdin, &out) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pump: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump never returned")
	}
	wg.Wait()

	got := out.String()
	if !strings.Contains(got, `"result":{"ok":true}`) {
		t.Errorf("the reply was truncated by stdin EOF; stdout has %q", got)
	}
}

// TestTheDaemonSeesEOFWhenStdinEnds.
//
// The other side of the same coin: the daemon has to learn the client is done,
// or its MCP session sits open until the socket is torn down. Half-close is what
// tells it, and it is easy to leave out because everything still appears to work
// until connections accumulate.
func TestTheDaemonSeesEOFWhenStdinEnds(t *testing.T) {
	dir := testutil.SocketDir(t)
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	sawEOF := make(chan bool, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		// Read until EOF. Without a half-close this blocks until the test ends.
		_, err = io.Copy(io.Discard, c)
		sawEOF <- err == nil
	}()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	var out bytes.Buffer
	go func() { _ = pump(conn, strings.NewReader("hello\n"), &out) }()

	select {
	case ok := <-sawEOF:
		if !ok {
			t.Error("the daemon side ended on an error rather than a clean EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon never saw EOF; stdin ending did not half-close the socket")
	}
}

// TestBytesAreForwardedUnchanged. The shim must not reframe, buffer into lines,
// or otherwise touch the protocol — anything it "helpfully" normalizes is a
// difference between what the client sent and what the daemon parses.
func TestBytesAreForwardedUnchanged(t *testing.T) {
	client, server := socketPair(t)

	// Awkward on purpose: no trailing newline, embedded newlines, and a
	// multi-byte rune that a naive line reader could split.
	payload := "{\"a\":1}\n{\"b\":\"héllo\"}\n{\"c\":[1,2,3]}"

	got := make(chan string, 1)
	go func() {
		defer func() { _ = server.Close() }()
		b, _ := io.ReadAll(server)
		got <- string(b)
	}()

	var out bytes.Buffer
	go func() { _ = pump(client, strings.NewReader(payload), &out) }()

	select {
	case g := <-got:
		if g != payload {
			t.Errorf("the shim altered the stream:\n  sent %q\n  got  %q", payload, g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing arrived at the daemon side")
	}
}

// TestAMissingDaemonSaysWhatToDo.
//
// This runs inside an editor, where the entire failure a user sees is "the MCP
// server would not start". "connect: no such file or directory" does not tell
// them the daemon is not running.
func TestAMissingDaemonSaysWhatToDo(t *testing.T) {
	missing := filepath.Join(testutil.SocketDir(t), "absent.sock")
	err := run(missing, 200*time.Millisecond)
	if err == nil {
		t.Fatal("connecting to a nonexistent socket succeeded")
	}
	msg := err.Error()
	for _, want := range []string{"mole serve", missing, "--socket"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error omits %q, so the user cannot act on it:\n%s", want, msg)
		}
	}

	// A socket file with nothing behind it is a different situation and gets a
	// different answer: the daemon died without cleaning up.
	stale := filepath.Join(testutil.SocketDir(t), "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = ln.Close()
	if _, serr := os.Stat(stale); serr != nil {
		t.Skipf("could not leave a stale socket behind: %v", serr)
	}

	err = run(stale, 200*time.Millisecond)
	if err == nil {
		t.Fatal("connecting to a stale socket succeeded")
	}
	if !strings.Contains(err.Error(), "without cleaning up") {
		t.Errorf("a stale socket got the same advice as a missing one:\n%s", err)
	}
}

// socketPair returns two ends of a connected unix socket.
//
// The transport the shim actually runs on. net.Pipe is an in-memory
// io.ReadWriteCloser with no half-close, so a test written over one exercises a
// path production never takes.
func socketPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	path := filepath.Join(testutil.SocketDir(t), "p.sock")
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
	client, err = net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}
