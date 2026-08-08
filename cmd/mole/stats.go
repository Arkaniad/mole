package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/spf13/cobra"
)

// mole stats
//
// Cross-session aggregation. `trace` shows one session; §17.1's gate is a
// decision made from the whole corpus, and per-session numbers cannot answer it
// — which is why this is an M2 deliverable rather than a nicety.
//
// The command's real job is the denominator. A rate is a fraction, and three of
// the outcomes in §10.4 exist specifically to keep this one honest: fetches
// never attempted, refusals the system itself made, and pages that a parser
// rather than a browser would fix. Printing counts and letting a reader divide
// is how the wrong number gets quoted in a decision.

// statsUsage is the prose cobra cannot generate; the flag list comes from the
// definitions themselves.
const statsUsage = `Cross-session measurement.

The gate reads js_required as a fraction of ELIGIBLE fetches: requests actually
made, minus the ones Mole itself refused. Refusals are the system working, and
leaving them in the denominator understates a capability gap that is real.
Fetches the search provider supplied content for are excluded too — they were
never attempted, and counting them moves the rate whenever you switch provider.
`

func newStatsCmd() *cobra.Command {
	var (
		wantFetch bool
		since     time.Duration
		domains   int
		asJSON    bool
	)

	c := &cobra.Command{
		Use:   "stats",
		Short: "Cross-session measurement",
		Long:  statsUsage,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !wantFetch {
				_ = cmd.Help()
				return errors.New("nothing selected: pass --fetch")
			}
			return cmdStats(cmd.Context(), dbPath(cmd), since, domains, asJSON)
		},
	}

	f := c.Flags()
	f.BoolVar(&wantFetch, "fetch", false, "fetch outcome mix and the §17.1 headless-browser gate")
	f.DurationVar(&since, "since", 720*time.Hour, "look-back window")
	f.IntVar(&domains, "domains", 5, "top domains per cause (0 to omit)")
	f.BoolVar(&asJSON, "json", false, "emit the aggregation as JSON")
	return c
}

func cmdStats(ctx context.Context, path string, since time.Duration, domains int, asJSON bool) error {
	cutoff := time.Now().Add(-since)

	db, err := openDBNoMigrate(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()

	var stats []store.FetchStat
	if err := db.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		stats, err = q.FetchOutcomeStats(ctx, cutoff, domains)
		return err
	}); err != nil {
		return err
	}

	mix := summarize(stats, since)

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(mix)
	}
	printFetchMix(mix, stats, domains)
	return nil
}

// ---------------------------------------------------------------------------
// The arithmetic
// ---------------------------------------------------------------------------

// FetchMix is the aggregation plus the gate verdict derived from it.
type FetchMix struct {
	Window string `json:"window"`

	// Total is every recorded outcome, including ones where no request was made.
	Total int64 `json:"total"`
	// NotAttempted is provider_content: the search provider supplied the text,
	// so no request happened. Counting these as successes was a real bug — it
	// padded the denominator with work never done and dragged every failure
	// rate toward zero in proportion to how much of the corpus Tavily covered.
	NotAttempted int64 `json:"not_attempted"`
	// Attempted is requests actually made.
	Attempted int64 `json:"attempted"`
	// Refused is robots_denied plus guard_denied — Mole declining, not failing.
	Refused int64 `json:"refused"`
	// Eligible is Attempted minus Refused: pages we tried to read and were
	// permitted to. This is the gate's denominator.
	Eligible int64 `json:"eligible"`

	OK             int64 `json:"ok"`
	JSRequired     int64 `json:"js_required"`
	StructuredOnly int64 `json:"structured_only"`

	// JSRequiredPct is js_required over Eligible.
	JSRequiredPct float64 `json:"js_required_pct"`

	// Verdict is the §17.1 branch this number falls in.
	Verdict     string `json:"verdict"`
	VerdictNote string `json:"verdict_note"`
	// Confident is false when the sample is too small for the verdict to mean
	// anything. A 0-of-3 measurement is not evidence that a gap does not exist.
	Confident bool `json:"confident"`
}

// minGateSample is the number of eligible fetches below which the gate's
// verdict is reported but explicitly not trusted.
//
// Not a statistical threshold — a rough floor. The failure this guards against
// is closing an architectural question permanently on a handful of fetches from
// one afternoon's testing, which §17.1 asks to be recorded in the repo and
// therefore treated as settled.
const minGateSample = 100

func summarize(stats []store.FetchStat, window time.Duration) FetchMix {
	m := FetchMix{Window: humanWindow(window)}

	for _, s := range stats {
		o := fetch.Outcome(s.Outcome)
		m.Total += s.Count

		if !o.Attempted() {
			m.NotAttempted += s.Count
			continue
		}
		m.Attempted += s.Count
		if o.SystemWorking() {
			m.Refused += s.Count
		}

		switch o {
		case fetch.OutcomeOK:
			m.OK += s.Count
		case fetch.OutcomeJSRequired:
			m.JSRequired += s.Count
		case fetch.OutcomeStructuredOnly:
			m.StructuredOnly += s.Count
		}
	}

	m.Eligible = m.Attempted - m.Refused
	if m.Eligible > 0 {
		m.JSRequiredPct = 100 * float64(m.JSRequired) / float64(m.Eligible)
	}
	m.Confident = m.Eligible >= minGateSample
	m.Verdict, m.VerdictNote = gateVerdict(m)
	return m
}

// gateVerdict maps the rate onto §17.1's four branches.
func gateVerdict(m FetchMix) (string, string) {
	if m.Eligible == 0 {
		return "no data", "No eligible fetches in this window. Run the eval corpus (§14.2) before reading a verdict here."
	}
	switch {
	case m.JSRequiredPct < 5:
		return "drop headless",
			"Under ~5%. Delete the headless item from M10 and record this number in the repo, " +
				"so the question stays closed rather than being relitigated on intuition."
	case m.JSRequiredPct < 15:
		return "build structured extraction first",
			"Between ~5% and ~15%. Build structured-data extraction and re-measure; expect most of " +
				"this to move to structured_only. Reconsider only on what remains."
	default:
		return "buy the capability, hosted first",
			"Above ~15%. Worth buying — but reach for a hosted render API, which is a metered " +
				"per-call charge the ledger already models. Ship an in-process browser only if a " +
				"concrete requirement rules the render API out."
	}
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

func printFetchMix(m FetchMix, stats []store.FetchStat, topDomains int) {
	fmt.Printf("fetch outcomes — last %s\n\n", m.Window)

	if m.Total == 0 {
		fmt.Println("  no fetches recorded in this window")
		return
	}

	// Attempted fetches, as a rate over the eligible denominator.
	fmt.Printf("  %-18s %6d   (requests made)\n", "attempted", m.Attempted)
	if m.Refused > 0 {
		fmt.Printf("  %-18s %6d   (robots/guard — Mole declining, excluded below)\n", "refused", m.Refused)
	}
	fmt.Printf("  %-18s %6d   (denominator for every rate)\n\n", "eligible", m.Eligible)

	for _, s := range stats {
		o := fetch.Outcome(s.Outcome)
		if !o.Attempted() || o.SystemWorking() {
			continue
		}
		marker := ""
		if o == fetch.OutcomeJSRequired {
			marker = "  ← the §17.1 gate reads this"
		}
		fmt.Printf("  %-18s %6d  %5.1f%%%s\n", s.Outcome, s.Count, pct(s.Count, m.Eligible), marker)
		printDomains(s, topDomains)
	}

	if m.NotAttempted > 0 {
		fmt.Printf("\n  %-18s %6d   (search provider supplied the text; no request made)\n",
			"provider_content", m.NotAttempted)
	}
	if m.Refused > 0 {
		fmt.Println("\n  refused — the system working, not a capability gap:")
		for _, s := range stats {
			if fetch.Outcome(s.Outcome).SystemWorking() {
				fmt.Printf("  %-18s %6d\n", s.Outcome, s.Count)
				printDomains(s, topDomains)
			}
		}
	}

	fmt.Printf("\n§17.1 gate: js_required is %.1f%% of eligible fetches (%d of %d).\n",
		m.JSRequiredPct, m.JSRequired, m.Eligible)
	fmt.Printf("  → %s\n", m.Verdict)
	fmt.Printf("    %s\n", wrapAt(m.VerdictNote, 72, "    "))

	if !m.Confident && m.Eligible > 0 {
		fmt.Printf("\n  NOT ENOUGH DATA: %d eligible fetches, want at least %d before acting.\n",
			m.Eligible, minGateSample)
		fmt.Println("  A low rate over a small sample is an absence of evidence, not evidence")
		fmt.Println("  of absence — and §17.1 asks for this to be recorded as settled.")
	}
	if m.StructuredOnly > 0 {
		fmt.Printf("\n  %d fetch(es) were rescued by structured data (%.1f%%). Those are a parser's\n",
			m.StructuredOnly, pct(m.StructuredOnly, m.Eligible))
		fmt.Println("  work, not a browser's — see §10.4's cheap paths before reading the gate.")
	}
}

func printDomains(s store.FetchStat, top int) {
	if top <= 0 || len(s.Domains) == 0 {
		return
	}
	// A failure rate concentrated in three domains is a denylist entry or a
	// targeted adapter, not an architecture change (§17.1).
	for _, d := range s.Domains {
		fmt.Printf("  %-18s %6s     %s (%d)\n", "", "", d.Domain, d.Count)
	}
}

// humanWindow renders a look-back the way someone would say it. The default is
// 720h, and "720h0m0s" in a report about a month of data is just noise.
func humanWindow(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return d.String()
}

func pct(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(n) / float64(total)
}

// wrapAt breaks text at word boundaries, indenting continuation lines.
func wrapAt(s string, width int, indent string) string {
	var out, line []byte
	for _, word := range splitWords(s) {
		if len(line) > 0 && len(line)+1+len(word) > width {
			out = append(out, line...)
			out = append(out, '\n')
			out = append(out, indent...)
			line = line[:0]
		}
		if len(line) > 0 {
			line = append(line, ' ')
		}
		line = append(line, word...)
	}
	return string(append(out, line...))
}

func splitWords(s string) []string {
	var words []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\n' || s[i] == '\t' {
			if start >= 0 {
				words = append(words, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		words = append(words, s[start:])
	}
	return words
}
