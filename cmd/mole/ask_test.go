package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/mcpserver"
)

func TestAskRefusesWithoutBothArguments(t *testing.T) {
	for _, args := range [][]string{
		{"ask"},
		{"ask", "s_123"},
		{"ask", "s_123", "why", "extra"},
	} {
		out, err := exec(t, args...)
		if err == nil {
			t.Fatalf("mole %s was accepted; want a usage error\n%s", strings.Join(args, " "), out)
		}
		// The message has to show the quoting, or a user types
		// `mole ask s_123 why is it slow` and gets an arity complaint instead.
		if !strings.Contains(err.Error(), `mole ask <session-id> "<question>"`) {
			t.Fatalf("usage text does not show the form: %v", err)
		}
	}
}

// TestAskDoesNotMigrate is the safety property behind running `mole ask` while a
// daemon is live (see openDBNoMigrate). Ordinary writes from two processes are
// fine; a schema change under a running daemon is not. So `ask` must refuse a
// database it would have to migrate rather than quietly bringing it forward.
//
// Falsified by pointing cmdAsk at openDBMigrate: the command then succeeds in
// creating the schema and this test fails on the missing error.
func TestAskDoesNotMigrate(t *testing.T) {
	t.Setenv("MOLE_CONFIG_DIR", t.TempDir())
	// A path with no database at all is the strongest form of "would have to
	// migrate": openDBMigrate creates and migrates it, openDBNoMigrate refuses.
	missing := filepath.Join(t.TempDir(), "absent.db")

	err := cmdAsk(context.Background(), &strings.Builder{}, askOpts{
		sessionID: "s_123",
		question:  "anything",
		dbPath:    missing,
	})
	if err == nil {
		t.Fatal("ask against a nonexistent database succeeded; it migrated one into place")
	}
	if !strings.Contains(err.Error(), "mole migrate") {
		t.Fatalf("error does not point at the fix: %v", err)
	}
}

func TestPrintAskShowsCostAndSources(t *testing.T) {
	var b strings.Builder
	printAsk(&b, mcpserver.AskOut{
		Answer: "MambaByte outperforms MegaByte. [1]",
		Citations: []mcpserver.Citation{{
			N:      1,
			Source: "https://arxiv.org/html/2402.19155v1",
			Quotes: []string{"MambaByte-353M outperforms MegaByte-758M+262M"},
		}},
		Claims:       []mcpserver.Claim{{ID: "c1"}, {ID: "c2"}},
		Spent:        4_100,
		AskSessionID: "s_ask",
	}, "")
	got := b.String()

	for _, want := range []string{
		"MambaByte outperforms MegaByte. [1]",
		"[1] https://arxiv.org/html/2402.19155v1",
		"MambaByte-353M outperforms MegaByte-758M+262M",
		"2 claim(s)",
		"charged to s_ask",
		"$0.0041",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q:\n%s", want, got)
		}
	}
}

// TestPrintAskSaysWhenTheCostIsNotMeasured keeps a local model from printing
// "$0.0000" as if that were a measurement. §8 treats an unenforceable number as
// worse than no number.
func TestPrintAskSaysWhenTheCostIsNotMeasured(t *testing.T) {
	var b strings.Builder
	printAsk(&b, mcpserver.AskOut{Answer: "x", Spent: 0}, "gemma3:12b")
	got := b.String()
	if !strings.Contains(got, "cost is not measured") || !strings.Contains(got, "gemma3:12b") {
		t.Fatalf("unpriced model not disclosed:\n%s", got)
	}
}

// TestPrintAskSurfacesDegradation covers the no-provider path, where the answer
// is a claim listing rather than prose. Silently printing the fallback as though
// it were a synthesized answer is the failure this prevents.
func TestPrintAskSurfacesDegradation(t *testing.T) {
	var b strings.Builder
	printAsk(&b, mcpserver.AskOut{
		Answer:   "- some claim",
		Degraded: "no model provider configured",
	}, "")
	if !strings.Contains(b.String(), "not synthesized: no model provider configured") {
		t.Fatalf("degradation not surfaced:\n%s", b.String())
	}
}
