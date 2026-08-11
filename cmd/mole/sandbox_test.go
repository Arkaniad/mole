package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M8 slice 6, at the command level.
//
// The property is that a missing container runtime never fails a setup. mole is
// one static binary and the sandbox serves CodeRunner alone; §12.2 says the
// sandbox "is not the control" for the SQL path, so local analysis works
// without it. A `doctor` that exited non-zero for an absent runtime would tell
// every CI job and install script otherwise.

// doctorRun runs the real doctor against an isolated database and captures what
// it printed. It writes through fmt.Printf rather than the command's writer, so
// stdout is redirected instead of being handed a buffer.
func doctorRun(t *testing.T, home, path string) (string, error) {
	t.Helper()
	t.Setenv("MOLE_CONFIG_DIR", t.TempDir())
	db := filepath.Join(home, "mole.db")
	t.Setenv("MOLE_DB", db)
	if path != "" {
		t.Setenv("PATH", path)
	}

	if _, err := os.Stat(db); os.IsNotExist(err) {
		if err := cmdMigrate(context.Background(), db); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	doctorErr := cmdDoctor(context.Background(), db)

	_ = w.Close()
	os.Stdout = stdout
	return <-done, doctorErr
}

// TestAMissingSandboxDoesNotFailDoctor.
//
// Asserted by comparing the outcome with and against the same configuration:
// the only difference is whether a runtime is reachable, so any change in the
// exit status would be the sandbox check causing it.
func TestAMissingSandboxDoesNotFailDoctor(t *testing.T) {
	home := t.TempDir()

	_, errWith := doctorRun(t, home, "")
	withoutPath, errWithout := doctorRun(t, home, t.TempDir())

	if !strings.Contains(withoutPath, "sandbox") {
		t.Fatalf("doctor says nothing about the sandbox:\n%s", withoutPath)
	}
	// Same configuration, same verdict. Whether the numbers are zero or not
	// depends on the empty config directory; what must not change is the answer.
	if (errWith == nil) != (errWithout == nil) {
		t.Errorf("removing the container runtime changed doctor's verdict\n"+
			"  with a runtime:    %v\n  without:           %v", errWith, errWithout)
	}
	if errWith != nil && errWithout != nil && errWith.Error() != errWithout.Error() {
		t.Errorf("the problem count changed with the runtime removed\n"+
			"  with:    %v\n  without: %v", errWith, errWithout)
	}
}

// TestDoctorSaysWhatStillWorksWithoutASandbox. "sandbox: not found" tells
// somebody something is missing without telling them whether it matters, and
// the answer is that it usually does not.
func TestDoctorSaysWhatStillWorksWithoutASandbox(t *testing.T) {
	out, _ := doctorRun(t, t.TempDir(), t.TempDir())

	for _, want := range []string{
		"no container runtime found",
		"podman",
		"docker",
		"local code analysis unavailable",
		"SQL analysis is unaffected",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is missing %q:\n%s", want, out)
		}
	}
}

// TestDoctorPrintsTheFlagsItWouldUse, when a runtime is available.
//
// A line claiming "netns disabled, seccomp default" while the runner forgot
// --network=none would be a check reporting a property nothing enforces, so the
// summary is rendered from the same flags CodeRunner will pass.
func TestDoctorPrintsTheFlagsItWouldUse(t *testing.T) {
	out, _ := doctorRun(t, t.TempDir(), "")
	if !strings.Contains(out, "no container runtime found") {
		for _, want := range []string{"no network", "read-only rootfs", "capabilities dropped", "uid 65534"} {
			if !strings.Contains(out, want) {
				t.Errorf("a usable runtime was found but %q is not in the report:\n%s", want, out)
			}
		}
		return
	}
	t.Skip("no container runtime on this machine")
}
