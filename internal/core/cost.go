package core

import (
	"fmt"
	"strconv"
	"strings"
)

// MicrosPerUSD is the fixed-point scale for money. All monetary values in Mole
// are int64 micro-dollars.
const MicrosPerUSD = 1_000_000

// Cost is what one tool call spent, always recorded in both units.
//
// The token breakdown is not decoration. Cache reads bill at ~0.1x input and
// cache writes at 1.25x (5m TTL) or 2x (1h TTL); this design caches heavily, so
// a single flat token count would misprice sessions badly in both directions.
type Cost struct {
	USDMicros int64

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// TotalTokens counts every token that moved, including cache reads and writes.
// Cache-read tokens are cheaper, not free, and they are real tokens — a token
// budget that ignored them would not bound anything.
func (c Cost) TotalTokens() int64 {
	return c.InputTokens + c.OutputTokens + c.CacheReadTokens + c.CacheWriteTokens
}

// BudgetAmount converts a cost into the session's accounting unit. This is the
// only place the two units meet, which is what makes switching a session's unit
// a display-and-gating choice rather than a data migration.
func (c Cost) BudgetAmount(unit BudgetUnit) int64 {
	switch unit {
	case BudgetUSD:
		return c.USDMicros
	case BudgetTokens:
		return c.TotalTokens()
	default:
		return 0
	}
}

func (c Cost) Add(o Cost) Cost {
	return Cost{
		USDMicros:        c.USDMicros + o.USDMicros,
		InputTokens:      c.InputTokens + o.InputTokens,
		OutputTokens:     c.OutputTokens + o.OutputTokens,
		CacheReadTokens:  c.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: c.CacheWriteTokens + o.CacheWriteTokens,
	}
}

func (c Cost) IsZero() bool { return c == Cost{} }

func (c Cost) Validate() error {
	if c.USDMicros < 0 {
		return fmt.Errorf("cost: negative usd")
	}
	if c.InputTokens < 0 || c.OutputTokens < 0 || c.CacheReadTokens < 0 || c.CacheWriteTokens < 0 {
		return fmt.Errorf("cost: negative token count")
	}
	return nil
}

func (c Cost) String() string {
	return fmt.Sprintf("%s (%d tok: %din/%dout/%dcr/%dcw)",
		FormatUSD(c.USDMicros), c.TotalTokens(),
		c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens)
}

// SumCosts adds a slice of costs. Used when settling a reservation against the
// several tool calls one actor run produced.
func SumCosts(cs []Cost) Cost {
	var total Cost
	for _, c := range cs {
		total = total.Add(c)
	}
	return total
}

// ---------------------------------------------------------------------------
// Formatting and parsing
// ---------------------------------------------------------------------------

// FormatUSD renders micro-dollars for humans. Sub-cent amounts keep enough
// precision to be useful when a single fetch costs $0.0004.
func FormatUSD(micros int64) string {
	neg := micros < 0
	if neg {
		micros = -micros
	}
	s := fmt.Sprintf("$%d.%04d", micros/MicrosPerUSD, (micros%MicrosPerUSD)/100)
	if neg {
		return "-" + s
	}
	return s
}

// FormatAmount renders a budget amount in the session's unit.
func FormatAmount(amount int64, unit BudgetUnit) string {
	switch unit {
	case BudgetUSD:
		return FormatUSD(amount)
	case BudgetTokens:
		return strconv.FormatInt(amount, 10) + " tok"
	default:
		return strconv.FormatInt(amount, 10)
	}
}

// ParseUSD converts a dollar string ("2", "2.50", "$2.50") to micro-dollars.
// Rejects more precision than micro-dollars rather than silently rounding —
// a budget that quietly loses precision is exactly the bug this type exists to
// prevent.
func ParseUSD(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "$"))
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		return 0, fmt.Errorf("negative amount %q", s)
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	micros := w * MicrosPerUSD

	if hasFrac {
		if len(frac) > 6 {
			return 0, fmt.Errorf("amount %q has more precision than micro-dollars", s)
		}
		padded := frac + strings.Repeat("0", 6-len(frac))
		f, err := strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
		micros += f
	}
	return micros, nil
}
