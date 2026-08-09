package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// M5 slice 5.
//
// A cassette is keyed on the request body, and with a worker pool the request
// bodies stop being reproducible: the synthesis prompt is built from claims in
// order, and lead completion order is whatever the network decided that run. So
// a cassette recorded under concurrency cannot be replayed — not at a different
// worker count, and not even against itself.
//
// This refusal is what lets concurrency and deterministic replay coexist:
// everything that touches a cassette runs serial, every real research run does
// not.

func TestRecordingRefusesAWorkerPool(t *testing.T) {
	for _, mode := range []string{"record", "replay"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MOLE_RECORD", mode)
			t.Setenv("MOLE_CASSETTE_DIR", t.TempDir())

			out, err := exec(t, "research", "a question", "--usd", "0.50", "--workers", "4")
			if err == nil {
				t.Fatalf("MOLE_RECORD=%s with --workers 4 was accepted\n%s", mode, out)
			}
			// The message has to name the fix, not just the problem: a user who
			// hits this is mid-way through recording a corpus.
			if !strings.Contains(err.Error(), "--workers 1") {
				t.Fatalf("error does not say what to do: %v", err)
			}
		})
	}
}

// TestRecordingAllowsOneWorker. The refusal must be about the pool, not about
// recording — pinning it to 1 has to get past this check and fail later, on the
// missing search provider, which is what the empty config dir guarantees.
func TestRecordingAllowsOneWorker(t *testing.T) {
	t.Setenv("MOLE_RECORD", "record")
	t.Setenv("MOLE_CASSETTE_DIR", t.TempDir())

	_, err := exec(t, "research", "a question", "--usd", "0.50", "--workers", "1")
	if err == nil {
		t.Fatal("expected the run to fail on configuration, not to succeed")
	}
	if strings.Contains(err.Error(), "--workers 1") {
		t.Fatalf("--workers 1 was refused by the cassette guard: %v", err)
	}
}

// TestAPoolIsAllowedWithoutACassette is the other half: the guard must not fire
// on an ordinary run, which is every run a person actually makes.
func TestAPoolIsAllowedWithoutACassette(t *testing.T) {
	t.Setenv("MOLE_CASSETTE_DIR", filepath.Join(t.TempDir(), "unused"))

	_, err := exec(t, "research", "a question", "--usd", "0.50", "--workers", "8")
	if err == nil {
		t.Fatal("expected the run to fail on configuration, not to succeed")
	}
	if strings.Contains(err.Error(), "--workers 1") {
		t.Fatalf("the cassette guard fired with MOLE_RECORD unset: %v", err)
	}
}
