package core_test

import (
	"math"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/core"
)

// TestRemainingFractionTakesTheTightestLimit is why this is not just
// (Budget-Spent)/Budget.
//
// §9.1's replan asks the planner whether more work is worth the remaining
// budget. A session two minutes from max_wallclock with almost all its dollars
// unspent has minutes left, not dollars, and telling it otherwise invites it to
// open threads that cannot finish — spending the allowance that would have
// written up what it already found.
func TestRemainingFractionTakesTheTightestLimit(t *testing.T) {
	created := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	base := func() *core.Session {
		return &core.Session{Budget: 1000, CreatedAt: created}
	}

	cases := []struct {
		name string
		mut  func(*core.Session)
		now  time.Time
		want float64
	}{
		{"untouched", func(*core.Session) {}, created, 1.0},
		{"half the money spent", func(s *core.Session) { s.Spent = 500 }, created, 0.5},
		{"escrow is not spendable on research", func(s *core.Session) { s.Escrow = 250 }, created, 0.75},
		{"a reservation outstanding", func(s *core.Session) { s.Held = 100 }, created, 0.9},
		{
			"wall clock binds tighter than money",
			func(s *core.Session) { s.MaxWallClock = 10 * time.Minute },
			created.Add(9 * time.Minute),
			0.1,
		},
		{
			"lead ceiling binds tighter than money",
			func(s *core.Session) { s.MaxLeads, s.LeadCount = 4, 3 },
			created, 0.25,
		},
		{
			"tool-call ceiling binds tighter than money",
			func(s *core.Session) { s.MaxToolCalls, s.ToolCallCount = 200, 180 },
			created, 0.1,
		},
		{
			"money still binds when it is the tightest",
			func(s *core.Session) {
				s.Spent = 950
				s.MaxLeads, s.LeadCount = 100, 1
				s.MaxWallClock = time.Hour
			},
			created, 0.05,
		},
		{"never negative past a ceiling", func(s *core.Session) { s.Spent = 5000 }, created, 0},
		{
			"never negative past the wall clock",
			func(s *core.Session) { s.MaxWallClock = time.Minute },
			created.Add(time.Hour), 0,
		},
		// Unmetered sessions have no denominator to divide by; reporting 0 there
		// would read as "stop immediately".
		{"no limits at all", func(s *core.Session) { s.Budget = 0 }, created, 1.0},
	}

	for _, tc := range cases {
		s := base()
		tc.mut(s)
		got := s.RemainingFraction(tc.now)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: RemainingFraction = %v, want %v", tc.name, got, tc.want)
		}
	}
}
