// Package pricing converts model token usage into money.
//
// Rates are stored as nano-dollars per token (int64), not dollars per million
// tokens (float64). At $5/MTok an input token is exactly 5000 nano-dollars and
// a cache read is exactly 500 — both integers, so the cost of a call is exact
// rather than a float that drifts once summed across a long session.
//
// The table is data, not code: a self-hosted model registered with all-zero
// rates costs $0 and still records real token counts, which is what makes
// token-mode budgeting meaningful for users who aren't paying per token.
package pricing

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lajosdeme/mole/internal/core"
)

// NanoPerMicro is the scale between the rate unit and the money unit.
const NanoPerMicro = 1000

// Rates is the per-token price of one model, in nano-dollars.
type Rates struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite   int64 // 5-minute TTL
	CacheWrite1h int64
}

// Usage is the raw token count returned by a provider.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CacheWriteTTL1h  bool
}

// perMTok builds Rates from the published dollars-per-million-tokens figures.
// Cache multipliers are fixed by the API contract: reads bill at 0.1x input,
// writes at 1.25x (5m TTL) and 2x (1h TTL).
func perMTok(inputUSD, outputUSD float64) Rates {
	in := int64(inputUSD * 1000)
	return Rates{
		Input:        in,
		Output:       int64(outputUSD * 1000),
		CacheRead:    in / 10,
		CacheWrite:   in * 125 / 100,
		CacheWrite1h: in * 2,
	}
}

// defaultTable holds first-party Anthropic API rates. Partner platforms
// (Bedrock, Vertex) price separately and should be registered explicitly.
//
// Note: Sonnet 5 carries an introductory $2/$10 rate through 2026-08-31. The
// table uses the standard $3/$15 so estimates never under-report; register an
// override if you want the promotional rate reflected.
func defaultTable() map[string]Rates {
	return map[string]Rates{
		"claude-fable-5":  perMTok(10, 50),
		"claude-mythos-5": perMTok(10, 50),

		"claude-opus-5":   perMTok(5, 25),
		"claude-opus-4-8": perMTok(5, 25),
		"claude-opus-4-7": perMTok(5, 25),
		"claude-opus-4-6": perMTok(5, 25),

		"claude-sonnet-5":   perMTok(3, 15),
		"claude-sonnet-4-6": perMTok(3, 15),

		"claude-haiku-4-5": perMTok(1, 5),
	}
}

// Table maps model IDs to rates. Safe for concurrent use.
type Table struct {
	mu    sync.RWMutex
	rates map[string]Rates
	// strict makes an unknown model an error rather than a silent $0. Default
	// true: silently pricing an unknown model at zero would make the budget
	// ceiling unenforceable for exactly the model you forgot to register.
	strict bool
}

func NewTable() *Table {
	return &Table{rates: defaultTable(), strict: true}
}

// NewEmptyTable returns a table with no models registered — the starting point
// for a purely self-hosted deployment.
func NewEmptyTable() *Table {
	return &Table{rates: map[string]Rates{}, strict: true}
}

// SetStrict controls whether unknown models error (true) or price at zero.
func (t *Table) SetStrict(strict bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.strict = strict
}

// Register adds or replaces a model's rates.
func (t *Table) Register(model string, r Rates) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rates[model] = r
}

// RegisterFree registers a model that costs nothing — a local or self-hosted
// model whose tokens should still be counted and budgeted.
func (t *Table) RegisterFree(model string) { t.Register(model, Rates{}) }

// Lookup finds rates for a model, falling back to its undated base ID.
//
// Anthropic publishes an alias and a dated snapshot that resolve to the same
// weights and the same price — "claude-haiku-4-5" and "claude-haiku-4-5-20251001"
// are one model, and the table registers the alias. Without the fallback, a config
// naming the snapshot prices at nothing, which is worse than it sounds: --usd
// refuses to run at all rather than bound a session it cannot cost. That is what
// happened the first time this project pointed at a hosted provider.
//
// Exact match first, so a snapshot that ever does diverge in price can be
// registered explicitly and win.
func (t *Table) Lookup(model string) (Rates, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r, ok := t.rates[model]; ok {
		return r, true
	}
	if base, ok := stripDateSuffix(model); ok {
		r, ok := t.rates[base]
		return r, ok
	}
	return Rates{}, false
}

// stripDateSuffix removes a trailing -YYYYMMDD, reporting whether it found one.
//
// Deliberately strict about the shape: eight digits that parse as a plausible
// date. A looser rule would fold "gpt-4-32768" onto "gpt-4" and price a model
// against a different one's rates, and mispricing silently is the failure this
// whole package exists to prevent.
func stripDateSuffix(model string) (string, bool) {
	i := strings.LastIndexByte(model, '-')
	if i <= 0 || len(model)-i-1 != 8 {
		return "", false
	}
	digits := model[i+1:]
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	if _, err := time.Parse("20060102", digits); err != nil {
		return "", false
	}
	return model[:i], true
}

// Models lists registered model IDs, sorted.
func (t *Table) Models() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.rates))
	for m := range t.rates {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ErrUnknownModel is returned by Cost when a strict table has no rates for the
// model.
type ErrUnknownModel struct{ Model string }

func (e *ErrUnknownModel) Error() string {
	return fmt.Sprintf("pricing: no rates registered for model %q", e.Model)
}

// Cost prices a usage record. The returned Cost carries both the money and the
// token breakdown, so the same row serves USD-mode and token-mode sessions.
func (t *Table) Cost(model string, u Usage) (core.Cost, error) {
	c := core.Cost{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}

	r, ok := t.Lookup(normalize(model))
	if !ok {
		t.mu.RLock()
		strict := t.strict
		t.mu.RUnlock()
		if strict {
			return c, &ErrUnknownModel{Model: model}
		}
		return c, nil
	}

	write := r.CacheWrite
	if u.CacheWriteTTL1h {
		write = r.CacheWrite1h
	}

	nano := u.InputTokens*r.Input +
		u.OutputTokens*r.Output +
		u.CacheReadTokens*r.CacheRead +
		u.CacheWriteTokens*write

	c.USDMicros = divRoundHalfUp(nano, NanoPerMicro)
	return c, nil
}

// divRoundHalfUp avoids systematically under-billing: plain integer division
// truncates, which over thousands of calls quietly understates spend and lets a
// session drift past its ceiling.
func divRoundHalfUp(n, d int64) int64 {
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

// normalize strips the context-window suffix some deployments append
// (e.g. "claude-opus-5[1m]") so a tagged model still prices correctly.
func normalize(model string) string {
	if i := strings.IndexByte(model, '['); i > 0 {
		return model[:i]
	}
	return model
}
