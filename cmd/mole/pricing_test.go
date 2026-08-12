package main

import (
	"testing"

	"github.com/lajosdeme/mole/internal/config"
	"github.com/lajosdeme/mole/internal/pricing"
)

// A self-hosted model costs electricity, not tokens.
//
// Left unpriced it warned on every call — "model not in pricing table; USD cost
// recorded as zero" — several hundred times per local run, sharing a channel with
// the warning that matters: a HOSTED model nobody registered, where zero cost means
// the --usd ceiling cannot bind.

func TestALoopbackModelIsPricedAtZeroRatherThanUnpriced(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.BaseURL = "http://localhost:11434/v1"
	cfg.LLM.Model = "qwen3:4b"
	cfg.LLM.CheapModel = "qwen2.5:3b"

	table := pricingFor(cfg)
	for _, m := range []string{"qwen3:4b", "qwen2.5:3b"} {
		r, ok := table.Lookup(m)
		if !ok {
			t.Errorf("%s is unpriced; every call will warn", m)
			continue
		}
		if r.Input != 0 || r.Output != 0 {
			t.Errorf("%s priced at %+v, want zero", m, r)
		}
	}
}

// TestAHostedModelIsStillUnpriced. The warning has to survive for the case it was
// written for: a --usd ceiling that cannot bind.
func TestAHostedModelIsStillUnpriced(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.BaseURL = "https://api.deepseek.com/v1"
	cfg.LLM.Model = "some-unregistered-model"

	if _, ok := pricingFor(cfg).Lookup("some-unregistered-model"); ok {
		t.Error("a hosted model was silently priced at zero; the ceiling would not bind")
	}
}

// TestAModelOnAnotherMachineIsNotAssumedFree. Someone else's GPU may well be
// metered, and mole does not get to decide their costs are zero.
func TestAModelOnAnotherMachineIsNotAssumedFree(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.BaseURL = "http://192.168.1.50:11434/v1"
	cfg.LLM.Model = "qwen3:4b"

	if _, ok := pricingFor(cfg).Lookup("qwen3:4b"); ok {
		t.Error("a model on another host was priced at zero")
	}
}

// TestTheRegisteredModelsStillPrice, so the local case cannot quietly zero them.
func TestTheRegisteredModelsStillPrice(t *testing.T) {
	cfg := &config.Config{}
	cfg.LLM.BaseURL = "http://localhost:11434/v1"
	cfg.LLM.Model = "qwen3:4b"

	r, ok := pricingFor(cfg).Lookup("deepseek-v4-flash")
	if !ok || r.Input != 140 {
		t.Errorf("deepseek rates = %+v ok=%v; a local endpoint changed a hosted model's price",
			r, ok)
	}
	_ = pricing.NanoPerMicro
}
