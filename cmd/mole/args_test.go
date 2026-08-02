package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/llm"
)

func testFlags() (*flag.FlagSet, *string, *bool, *int) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	usd := fs.String("usd", "", "")
	jsonOut := fs.Bool("json", false, "")
	n := fs.Int("max-sources", 5, "")
	return fs, usd, jsonOut, n
}

// TestFlagsAfterPositionalsAreParsed. Go's flag package stops at the first
// non-flag argument, so `mole research "a question" --usd 0.50` left --usd
// unparsed and folded it into the question — the command then refused for
// having no budget, quoting a flag the user had just passed. Every usage string
// here shows the positional first because that is how people write it, so the
// parser has to accept it.
func TestFlagsAfterPositionalsAreParsed(t *testing.T) {
	fs, usd, jsonOut, n := testFlags()
	if err := parseArgs(fs, []string{"a question", "--usd", "0.50", "--json"}); err != nil {
		t.Fatal(err)
	}
	if *usd != "0.50" {
		t.Errorf("usd = %q, want 0.50", *usd)
	}
	if !*jsonOut {
		t.Error("--json after a positional was not parsed")
	}
	if got := strings.Join(fs.Args(), " "); got != "a question" {
		t.Errorf("positionals = %q, want \"a question\" — a flag leaked into the question", got)
	}
	if *n != 5 {
		t.Errorf("max-sources = %d, want the default 5", *n)
	}
}

// TestBooleanFlagDoesNotSwallowAPositional: a bool takes no value, so consuming
// the next argument would eat the question.
func TestBooleanFlagDoesNotSwallowAPositional(t *testing.T) {
	fs, usd, jsonOut, _ := testFlags()
	if err := parseArgs(fs, []string{"--json", "the question", "--usd", "1.00"}); err != nil {
		t.Fatal(err)
	}
	if !*jsonOut || *usd != "1.00" {
		t.Errorf("json=%v usd=%q", *jsonOut, *usd)
	}
	if got := strings.Join(fs.Args(), " "); got != "the question" {
		t.Errorf("positionals = %q, want \"the question\"", got)
	}
}

func TestInterleavedAndEqualsForms(t *testing.T) {
	for _, args := range [][]string{
		{"--usd", "0.50", "a question"},
		{"a question", "--usd", "0.50"},
		{"--usd=0.50", "a question"},
		{"a question", "--usd=0.50"},
		{"-usd", "0.50", "a question"},
		{"a", "--usd", "0.50", "question"},
	} {
		fs, usd, _, _ := testFlags()
		if err := parseArgs(fs, args); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if *usd != "0.50" {
			t.Errorf("%v: usd = %q, want 0.50", args, *usd)
		}
	}
}

// TestDoubleDashEndsFlagParsing so a question that starts with a dash survives.
func TestDoubleDashEndsFlagParsing(t *testing.T) {
	fs, usd, _, _ := testFlags()
	if err := parseArgs(fs, []string{"--usd", "0.50", "--", "--not-a-flag"}); err != nil {
		t.Fatal(err)
	}
	if *usd != "0.50" {
		t.Errorf("usd = %q", *usd)
	}
	if got := strings.Join(fs.Args(), " "); got != "--not-a-flag" {
		t.Errorf("positionals = %q, want the literal argument after --", got)
	}
}

func TestUnknownFlagStillErrors(t *testing.T) {
	fs, _, _, _ := testFlags()
	if err := parseArgs(fs, []string{"a question", "--nope", "x"}); err == nil {
		t.Error("an unknown flag was accepted")
	}
}

// ---------------------------------------------------------------------------
// Key / provider mismatch
// ---------------------------------------------------------------------------

// TestMismatchedKeyIsRefusedRatherThanGreen. llm.provider defaults to anthropic
// when unset, so a Groq key set on its own produced a doctor line reading
// "✓ llm provider claude-opus-5 via config" followed by an auth failure at the
// first model call. A check that runs and passes wrongly is worse than no
// check.
func TestMismatchedKeyIsRefusedRatherThanGreen(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.APIKey = "gsk_abc123"

	err := checkKeyMatchesProvider(cfg, llm.KindAnthropic)
	if err == nil {
		t.Fatal("a Groq key defaulting to anthropic was accepted")
	}
	// The message has to be actionable: name the vendor and the exact settings.
	for _, want := range []string{"Groq", "llm.provider openai-compatible", "api.groq.com", "--tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%s", want, err)
		}
	}
}

// TestExplicitProviderIsTheUsersDecision: only the silent default is blocked.
// A proxy that accepts an OpenAI-shaped key at an Anthropic endpoint is
// unusual, not impossible, and the user said so explicitly.
func TestExplicitProviderIsTheUsersDecision(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.APIKey = "gsk_abc123"
	cfg.LLM.Provider = "anthropic"

	if err := checkKeyMatchesProvider(cfg, llm.KindAnthropic); err != nil {
		t.Errorf("an explicitly configured provider was overridden: %v", err)
	}
}

func TestMatchingAndUnknownKeysPass(t *testing.T) {
	cases := []struct {
		key  string
		kind llm.Kind
	}{
		{"sk-ant-api03-xxx", llm.KindAnthropic},
		{"gsk_abc", llm.KindOpenAICompatible},
		{"sk-proj-abc", llm.KindOpenAICompatible},
		{"my-self-hosted-token", llm.KindAnthropic}, // unrecognized: not our business
		{"", llm.KindAnthropic},                     // no key: the SDK chain resolves it
	}
	for _, tc := range cases {
		cfg := &config.Config{}
		cfg.LLM.APIKey = tc.key
		if err := checkKeyMatchesProvider(cfg, tc.kind); err != nil {
			t.Errorf("key %q with kind %s was refused: %v", tc.key, tc.kind, err)
		}
	}
}

// TestVendorPrefixOrdering: "sk-ant-" must win over the broader "sk-".
func TestVendorPrefixOrdering(t *testing.T) {
	v, ok := llm.VendorFromKey("sk-ant-api03-xxx")
	if !ok || v.Kind != llm.KindAnthropic {
		t.Errorf("sk-ant- resolved to %+v, want Anthropic", v)
	}
	v, ok = llm.VendorFromKey("sk-abc")
	if !ok || v.Kind != llm.KindOpenAICompatible {
		t.Errorf("sk- resolved to %+v, want OpenAI-compatible", v)
	}
	if _, ok := llm.VendorFromKey("totally-opaque"); ok {
		t.Error("an unrecognized key was assigned a vendor")
	}
}
