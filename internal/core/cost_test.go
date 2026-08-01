package core_test

import (
	"testing"

	"github.com/lajosdeme/mole/internal/core"
)

func TestParseUSD(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"2", 2_000_000, false},
		{"2.50", 2_500_000, false},
		{"$2.50", 2_500_000, false},
		{" $3.00 ", 3_000_000, false},
		{"0.000001", 1, false},
		{".5", 500_000, false},
		{"0", 0, false},
		// More precision than micro-dollars is rejected rather than silently
		// rounded — quiet precision loss is exactly what integer money exists
		// to prevent.
		{"1.0000001", 0, true},
		{"-1", 0, true},
		{"", 0, true},
		{"abc", 0, true},
	}

	for _, c := range cases {
		got, err := core.ParseUSD(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseUSD(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUSD(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseUSD(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatUSDRoundTrips(t *testing.T) {
	for _, micros := range []int64{0, 1, 999, 1_000_000, 2_500_000, 123_456_789} {
		s := core.FormatUSD(micros)
		if s == "" {
			t.Fatalf("FormatUSD(%d) empty", micros)
		}
	}
	if got := core.FormatUSD(2_500_000); got != "$2.5000" {
		t.Errorf("FormatUSD(2500000) = %q, want $2.5000", got)
	}
}

func TestBudgetAmountSelectsUnit(t *testing.T) {
	c := core.Cost{
		USDMicros:        7_500,
		InputTokens:      100,
		OutputTokens:     20,
		CacheReadTokens:  300,
		CacheWriteTokens: 50,
	}
	if got := c.BudgetAmount(core.BudgetUSD); got != 7_500 {
		t.Errorf("usd amount = %d, want 7500", got)
	}
	if got := c.BudgetAmount(core.BudgetTokens); got != 470 {
		t.Errorf("token amount = %d, want 470", got)
	}
}

func TestCostAddIsComponentwise(t *testing.T) {
	a := core.Cost{USDMicros: 100, InputTokens: 1, CacheReadTokens: 5}
	b := core.Cost{USDMicros: 250, OutputTokens: 2, CacheWriteTokens: 7}
	sum := a.Add(b)

	want := core.Cost{USDMicros: 350, InputTokens: 1, OutputTokens: 2, CacheReadTokens: 5, CacheWriteTokens: 7}
	if sum != want {
		t.Errorf("sum = %+v, want %+v", sum, want)
	}
}

func TestSessionAvailableSubtractsHeldAndEscrow(t *testing.T) {
	s := &core.Session{Budget: 1000, Spent: 100, Held: 200, Escrow: 150}
	if got := s.Available(); got != 550 {
		t.Errorf("available = %d, want 550", got)
	}

	// Available never goes negative, so callers can compare without guarding.
	s = &core.Session{Budget: 100, Spent: 200}
	if got := s.Available(); got != 0 {
		t.Errorf("available = %d, want 0", got)
	}
}

func TestActorTypesRoundTrip(t *testing.T) {
	in := []core.ActorType{core.ActorWeb, core.ActorAcademic, core.ActorLocalCompute}
	out := core.DecodeActorTypes(core.EncodeActorTypes(in))
	if len(out) != len(in) {
		t.Fatalf("round trip length = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("index %d = %q, want %q", i, out[i], in[i])
		}
	}
	if got := core.DecodeActorTypes(""); got != nil {
		t.Errorf("empty decode = %v, want nil", got)
	}
}
