package main

import (
	"bytes"
	"strings"
	"testing"
)

// exec runs the real command tree against captured output, so these assert what
// a user gets rather than what an internal function returns.
func exec(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// TestFlagsAfterPositionalsAreParsed is the bug that prompted this port. Go's
// flag package stops at the first non-flag argument, so
//
//	mole research "a question" --usd 0.50
//
// left --usd unparsed, folded it into the question, and then refused the
// command for having no budget while quoting the flag back. pflag handles this
// natively, but the behaviour is what matters, so it stays asserted.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	for _, args := range [][]string{
		{"research", "a question", "--usd", "0.50"},
		{"research", "--usd", "0.50", "a question"},
		{"research", "a question", "--usd=0.50"},
		{"research", "a", "--usd", "0.50", "question"},
	} {
		_, err := exec(t, args...)
		if err == nil {
			continue // reached execution: the budget parsed
		}
		// Whatever it failed on, it must not be a missing budget.
		if strings.Contains(err.Error(), "no budget given") {
			t.Errorf("%v: --usd was not parsed", args)
		}
	}
}

// TestBudgetFlagsStillMutuallyExclusive: the port must not lose the rule.
func TestBudgetFlagsStillMutuallyExclusive(t *testing.T) {
	_, err := exec(t, "research", "a question", "--usd", "1.00", "--tokens", "500")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want the mutual-exclusion refusal", err)
	}
}

// TestUnknownCommandSuggests rather than only reporting the miss.
func TestUnknownCommandSuggests(t *testing.T) {
	out, err := exec(t, "reserch")
	if err == nil {
		t.Fatal("an unknown command succeeded")
	}
	combined := out + err.Error()
	if !strings.Contains(combined, "research") {
		t.Errorf("no suggestion offered:\n%s", combined)
	}
}

// TestArgErrorsNameTheCommand. Cobra's stock text is "accepts 1 arg(s),
// received 0", which restates the arity and not the thing to type.
func TestArgErrorsNameTheCommand(t *testing.T) {
	cases := map[string]string{
		"trace":  "mole trace <session-id>",
		"config": "mole config get <key>",
	}
	if _, err := exec(t, "trace"); err == nil || !strings.Contains(err.Error(), cases["trace"]) {
		t.Errorf("trace: err = %v, want %q", err, cases["trace"])
	}
	if _, err := exec(t, "config", "get"); err == nil || !strings.Contains(err.Error(), cases["config"]) {
		t.Errorf("config get: err = %v, want %q", err, cases["config"])
	}
	if _, err := exec(t, "research"); err == nil || !strings.Contains(err.Error(), "mole research") {
		t.Errorf("research: err = %v, want the usage line", err)
	}
}

// TestHelpListsEveryCommand so a new one cannot be added invisibly.
func TestHelpListsEveryCommand(t *testing.T) {
	out, err := exec(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"research", "migrate", "config", "doctor", "sessions", "stats", "trace", "dev", "version",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not list %q", want)
		}
	}
}

// TestFlagHelpIsGeneratedNotWritten is the point of the port. A hand-maintained
// list drifts: the old one documented a flag ordering the parser rejected.
func TestFlagHelpIsGeneratedNotWritten(t *testing.T) {
	out, err := exec(t, "research", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--usd", "--tokens", "--mode", "--max-sources", "--timeout", "--json", "--quiet", "--db"} {
		if !strings.Contains(out, want) {
			t.Errorf("generated help is missing %q", want)
		}
	}
	// Each flag should appear once. A hand-written list left beside the
	// generated one is the drift problem doubled, not solved.
	if n := strings.Count(out, "--max-sources"); n != 1 {
		t.Errorf("--max-sources appears %d times; a hand-written list is still present", n)
	}
}

// TestDBFlagHelpHidesTheResolvedPath: help output gets pasted into issues, and
// one user's home directory is noise to everyone else.
func TestDBFlagHelpHidesTheResolvedPath(t *testing.T) {
	out, err := exec(t, "sessions", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "/.local/share/mole") || strings.Contains(out, "/home/") {
		t.Errorf("help leaks a resolved home path:\n%s", out)
	}
	if !strings.Contains(out, "MOLE_DB") {
		t.Errorf("help does not say where the default comes from:\n%s", out)
	}
}

// TestVersionBothForms — `mole version` was documented and in use before the
// port, and cobra only provides the flag.
func TestVersionBothForms(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		out, err := exec(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !strings.Contains(out, "mole ") {
			t.Errorf("%v printed %q", args, out)
		}
	}
}

// TestModeIsStillGated: only report works until M3.
func TestModeIsStillGated(t *testing.T) {
	_, err := exec(t, "research", "a question", "--usd", "1.00", "--mode", "dataset")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("err = %v, want the unimplemented-mode refusal", err)
	}
}
