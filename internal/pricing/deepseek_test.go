package pricing_test

import (
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/pricing"
)

// DeepSeek's rates, pinned against the published figures.
//
// A pricing table is data, and wrong data here is invisible: every ledger row
// reconciles against every other, the scorecard passes, and only the provider's
// invoice disagrees. So the assertions are in dollars-per-million — the unit the
// price list is published in — rather than in the nano-dollars the table stores.

// perMillion is what one million tokens of each kind costs, in micro-dollars.
func perMillion(t *testing.T, model string, kind string) int64 {
	t.Helper()
	var u pricing.Usage
	switch kind {
	case "input":
		u.InputTokens = 1_000_000
	case "output":
		u.OutputTokens = 1_000_000
	case "cache_read":
		u.CacheReadTokens = 1_000_000
	case "cache_write":
		u.CacheWriteTokens = 1_000_000
	}
	c, err := pricing.NewTable().Cost(model, u)
	if err != nil {
		t.Fatalf("%s: %v", model, err)
	}
	return c.USDMicros
}

// TestDeepSeekRatesMatchThePublishedTable.
func TestDeepSeekRatesMatchThePublishedTable(t *testing.T) {
	for _, tc := range []struct {
		model, kind string
		micros      int64
		published   string
	}{
		{"deepseek-v4-flash", "input", 140_000, "$0.14/MTok"},
		{"deepseek-v4-flash", "output", 280_000, "$0.28/MTok"},
		// $0.0028/MTok is 2.8 nano per token; the rate is an integer and rounds up,
		// because under-billing is the one direction this table may not fail in.
		{"deepseek-v4-flash", "cache_read", 3_000, "$0.0028/MTok, rounded up"},

		{"deepseek-v4-pro", "input", 435_000, "$0.435/MTok"},
		{"deepseek-v4-pro", "output", 870_000, "$0.87/MTok"},
		{"deepseek-v4-pro", "cache_read", 4_000, "$0.003625/MTok, rounded up"},
	} {
		if got := perMillion(t, tc.model, tc.kind); got != tc.micros {
			t.Errorf("%s %s = %d micros/MTok, want %d (%s)",
				tc.model, tc.kind, got, tc.micros, tc.published)
		}
	}
}

// TestACacheReadIsNeverUnderBilled. The published figure is not representable as
// an integer number of nano-dollars, so the only question is which way it rounds.
func TestACacheReadIsNeverUnderBilled(t *testing.T) {
	for model, published := range map[string]int64{
		// The exact published cost of 1M cache-hit tokens, in micro-dollars,
		// truncated toward zero: 2800 and 3625 nano → 2.8 and 3.625 micros.
		"deepseek-v4-flash": 2_800,
		"deepseek-v4-pro":   3_625,
	} {
		got := perMillion(t, model, "cache_read")
		if got*1000 < published {
			t.Errorf("%s cache read = %d nano/MTok, under the published %d — the table "+
				"under-bills", model, got*1000, published)
		}
	}
}

// TestTheAliasIsPricedForTheLedgerAndForThePreflight.
//
// Two lookups with two different names. `checkUSDIsEnforceable` reads the
// CONFIGURED model and refuses --usd when it is unpriced; the ledger prices what
// the API RETURNED. deepseek-chat is configured, deepseek-v4-flash comes back, and
// both need rates or one of the two halves silently does nothing.
func TestTheAliasIsPricedForTheLedgerAndForThePreflight(t *testing.T) {
	table := pricing.NewTable()
	for _, model := range []string{
		"deepseek-chat", "deepseek-v4-flash", "deepseek-reasoner", "deepseek-v4-pro",
	} {
		if _, ok := table.Lookup(model); !ok {
			t.Errorf("%q has no rates; --usd refuses the run or the ledger charges zero", model)
		}
	}

	// The configured alias and the model it resolves to must agree, or the
	// pre-flight estimate and the settled cost describe different runs.
	if a, b := perMillion(t, "deepseek-chat", "input"),
		perMillion(t, "deepseek-v4-flash", "input"); a != b {
		t.Errorf("deepseek-chat = %d, deepseek-v4-flash = %d; the estimate and the "+
			"ledger disagree about the same call", a, b)
	}
}

// TestAnAliasEstimateIsNeverBelowWhatTheCallCanCost.
//
// An alias is DeepSeek's to repoint without telling anyone. Both currently resolve
// to v4-flash, and deepseek-reasoner is registered at PRO rates so a repoint
// upward cannot make a reservation too small — the failure that lets work start
// which cannot be paid for.
func TestAnAliasEstimateIsNeverBelowWhatTheCallCanCost(t *testing.T) {
	alias := perMillion(t, "deepseek-reasoner", "output")
	for _, resolved := range []string{"deepseek-v4-flash", "deepseek-v4-pro"} {
		if got := perMillion(t, resolved, "output"); alias < got {
			t.Errorf("deepseek-reasoner estimates %d/MTok, below %s at %d — a "+
				"reservation would be too small", alias, resolved, got)
		}
	}
}

// TestARealCallPricesToSomethingNonZero.
//
// The shape of the first live DeepSeek call: 11 input tokens, 1 output. A table
// that priced this at zero would pass every other test in this file and still make
// --usd meaningless.
func TestARealCallPricesToSomethingNonZero(t *testing.T) {
	c, err := pricing.NewTable().Cost("deepseek-v4-flash", pricing.Usage{
		InputTokens: 11, OutputTokens: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 11×140 + 1×280 = 1820 nano = 1.82 micros, rounded half-up to 2.
	if c.USDMicros != 2 {
		t.Errorf("cost = %d micros, want 2", c.USDMicros)
	}
	if core.FormatAmount(c.USDMicros, core.BudgetUSD) == "" {
		t.Error("the amount does not format")
	}
}
