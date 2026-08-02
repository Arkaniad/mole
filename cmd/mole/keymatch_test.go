package main

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/llm"
)

// The flag-permutation tests that lived here are gone with parseArgs: pflag
// parses interspersed flags and positionals natively, so there is no longer a
// shim to test. TestFlagsAfterPositionalsAreParsed survives as a CLI-level
// assertion in cli_test.go, because the BEHAVIOUR still matters — it is the bug
// that prompted the port.

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
