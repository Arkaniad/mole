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
	// Asserting only "not the guard's message" is not enough: deleting the
	// --workers flag entirely makes cobra return "unknown flag", which also does
	// not contain that string, and this test passed. Measured. Naming the reason
	// it SHOULD fail is what makes it about the guard.
	if !strings.Contains(err.Error(), "search provider") {
		t.Fatalf("failed for an unexpected reason: %v", err)
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
	if !strings.Contains(err.Error(), "search provider") {
		t.Fatalf("failed for an unexpected reason: %v", err)
	}
}

// TestZeroAndNegativeWorkersAreNotSerial closes the hole the guard had: it
// tested the raw flag, and --workers 0 means "use the default", so 0 and any
// negative passed a check whose entire job is to enforce serial execution and
// then ran a pool.
func TestZeroAndNegativeWorkersAreNotSerial(t *testing.T) {
	for _, w := range []string{"0", "-3"} {
		t.Run("workers="+w, func(t *testing.T) {
			t.Setenv("MOLE_RECORD", "record")
			t.Setenv("MOLE_CASSETTE_DIR", t.TempDir())

			out, err := exec(t, "research", "a question", "--usd", "0.50", "--workers", w)
			if err == nil {
				t.Fatalf("--workers %s was accepted while recording\n%s", w, out)
			}
			if !strings.Contains(err.Error(), "--workers 1") {
				t.Fatalf("--workers %s bypassed the cassette guard: %v", w, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// M6: actor selection
// ---------------------------------------------------------------------------

// TestAcademicRequiresAContactEmail. §10.3 makes this a startup check, and a
// run that quietly researched half of what was asked for is worse than one that
// says why — so it is an error rather than a silent downgrade to web-only.
func TestAcademicRequiresAContactEmail(t *testing.T) {
	out, err := exec(t, "research", "a question", "--tokens", "1000", "--actors", "academic")
	if err == nil {
		t.Fatalf("--actors academic was accepted with no contact email\n%s", out)
	}
	if !strings.Contains(err.Error(), "contact email") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestWebOnlyDoesNotRequireAContactEmail. The address is required of the people
// who use academic providers, not of everyone — building providers nobody asked
// for would make every run depend on it.
func TestWebOnlyDoesNotRequireAContactEmail(t *testing.T) {
	_, err := exec(t, "research", "a question", "--tokens", "1000", "--actors", "web")
	if err == nil {
		t.Fatal("expected a failure on the missing search provider")
	}
	if strings.Contains(err.Error(), "contact email") {
		t.Fatalf("web-only asked for a contact email: %v", err)
	}
}

func TestUnknownActorIsRefused(t *testing.T) {
	_, err := exec(t, "research", "a question", "--tokens", "1000", "--actors", "web,quantum")
	if err == nil {
		t.Fatal("an unknown actor was accepted")
	}
	if !strings.Contains(err.Error(), "unknown actor") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
