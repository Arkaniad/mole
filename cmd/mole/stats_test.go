package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/store"
	"github.com/lajosdeme/mole/internal/tools/fetch"
)

func statRows(counts map[fetch.Outcome]int64) []store.FetchStat {
	var out []store.FetchStat
	for o, n := range counts {
		out = append(out, store.FetchStat{Outcome: string(o), Count: n})
	}
	return out
}

// TestProviderContentIsExcludedFromEveryRate. This is the arithmetic the whole
// command exists for. A corpus where Tavily supplied most of the text would
// otherwise report a js_required rate diluted in exact proportion to the
// provider's coverage — a number that moves when you switch search provider,
// which is not a property of the web.
func TestProviderContentIsExcludedFromEveryRate(t *testing.T) {
	// 10 js_required out of 100 real attempts is 10%. Add 900 provider hits.
	m := summarize(statRows(map[fetch.Outcome]int64{
		fetch.OutcomeOK:              90,
		fetch.OutcomeJSRequired:      10,
		fetch.OutcomeProviderContent: 900,
	}), time.Hour)

	if m.Attempted != 100 {
		t.Errorf("attempted = %d, want 100", m.Attempted)
	}
	if m.NotAttempted != 900 {
		t.Errorf("not attempted = %d, want 900", m.NotAttempted)
	}
	if m.JSRequiredPct < 9.9 || m.JSRequiredPct > 10.1 {
		t.Errorf("js_required = %.1f%%, want 10%% — provider content diluted the rate", m.JSRequiredPct)
	}
	if m.Verdict != "build structured extraction first" {
		t.Errorf("verdict = %q, want the 5-15%% branch", m.Verdict)
	}
}

// TestRefusalsAreExcludedFromTheDenominator. robots_denied and guard_denied are
// Mole declining, and a page we will never fetch cannot be evidence about
// whether we could read it if we did. Leaving them in understates a real gap.
func TestRefusalsAreExcludedFromTheDenominator(t *testing.T) {
	m := summarize(statRows(map[fetch.Outcome]int64{
		fetch.OutcomeOK:           40,
		fetch.OutcomeJSRequired:   10,
		fetch.OutcomeRobotsDenied: 45,
		fetch.OutcomeGuardDenied:  5,
	}), time.Hour)

	if m.Attempted != 100 {
		t.Errorf("attempted = %d, want 100", m.Attempted)
	}
	if m.Refused != 50 {
		t.Errorf("refused = %d, want 50", m.Refused)
	}
	if m.Eligible != 50 {
		t.Errorf("eligible = %d, want 50", m.Eligible)
	}
	// 10 of 50, not 10 of 100.
	if m.JSRequiredPct < 19.9 || m.JSRequiredPct > 20.1 {
		t.Errorf("js_required = %.1f%%, want 20%% of eligible", m.JSRequiredPct)
	}
	if m.Verdict != "buy the capability, hosted first" {
		t.Errorf("verdict = %q, want the >15%% branch", m.Verdict)
	}
}

// TestGateBranches walks §17.1's four cases, because the whole point of the
// command is that the branch is computed rather than eyeballed.
func TestGateBranches(t *testing.T) {
	cases := []struct {
		name       string
		js, ok     int64
		wantPrefix string
	}{
		{"under 5%", 4, 196, "drop headless"},
		{"just over 5%", 6, 94, "build structured"},
		{"just under 15%", 14, 86, "build structured"},
		{"over 15%", 20, 80, "buy the capability"},
	}
	for _, tc := range cases {
		m := summarize(statRows(map[fetch.Outcome]int64{
			fetch.OutcomeJSRequired: tc.js,
			fetch.OutcomeOK:         tc.ok,
		}), time.Hour)
		if !strings.HasPrefix(m.Verdict, tc.wantPrefix) {
			t.Errorf("%s (%.1f%%): verdict %q, want prefix %q",
				tc.name, m.JSRequiredPct, m.Verdict, tc.wantPrefix)
		}
	}
}

// TestSmallSampleIsNotConfident. §17.1 asks for the drop-headless verdict to be
// recorded in the repo so the question stays closed. Closing an architectural
// question on three fetches is how that instruction becomes a mistake that is
// hard to reopen.
func TestSmallSampleIsNotConfident(t *testing.T) {
	m := summarize(statRows(map[fetch.Outcome]int64{fetch.OutcomeOK: 3}), time.Hour)
	if m.Verdict != "drop headless" {
		t.Errorf("verdict = %q; 0%% should still map to the under-5%% branch", m.Verdict)
	}
	if m.Confident {
		t.Error("a 3-fetch sample was reported as confident")
	}

	big := summarize(statRows(map[fetch.Outcome]int64{fetch.OutcomeOK: 500}), time.Hour)
	if !big.Confident {
		t.Error("a 500-fetch sample was not reported as confident")
	}
}

// TestNoDataDoesNotProduceAVerdict: zero eligible fetches is not "0%", it is
// "we have not measured". Reporting the former would let an empty database
// close the headless question.
func TestNoDataDoesNotProduceAVerdict(t *testing.T) {
	m := summarize(nil, time.Hour)
	if m.Verdict != "no data" {
		t.Errorf("verdict on an empty corpus = %q, want \"no data\"", m.Verdict)
	}
	if m.Confident {
		t.Error("an empty corpus was reported as confident")
	}

	// Provider content alone is also no data: nothing was ever fetched.
	m = summarize(statRows(map[fetch.Outcome]int64{fetch.OutcomeProviderContent: 50}), time.Hour)
	if m.Verdict != "no data" {
		t.Errorf("verdict with only provider content = %q, want \"no data\"", m.Verdict)
	}
}

// TestStructuredOnlyIsNotCountedAsAGap: it is a day of parser work, and folding
// it in is the specific mistake §17.1 warns about.
func TestStructuredOnlyIsNotCountedAsAGap(t *testing.T) {
	m := summarize(statRows(map[fetch.Outcome]int64{
		fetch.OutcomeOK:             50,
		fetch.OutcomeStructuredOnly: 46,
		fetch.OutcomeJSRequired:     4,
	}), time.Hour)

	if m.JSRequiredPct >= 5 {
		t.Errorf("js_required = %.1f%%; structured_only was folded into the gap", m.JSRequiredPct)
	}
	if m.Verdict != "drop headless" {
		t.Errorf("verdict = %q, want drop headless", m.Verdict)
	}
	if m.StructuredOnly != 46 {
		t.Errorf("structured_only = %d, want 46 surfaced separately", m.StructuredOnly)
	}
}

// TestReportSaysWhichDenominatorItUsed. Someone reading this output is about to
// close or fund an architecture item; an unlabelled percentage is how the wrong
// one gets quoted.
func TestReportSaysWhichDenominatorItUsed(t *testing.T) {
	stats := statRows(map[fetch.Outcome]int64{
		fetch.OutcomeOK:              120,
		fetch.OutcomeJSRequired:      8,
		fetch.OutcomeRobotsDenied:    12,
		fetch.OutcomeProviderContent: 60,
	})
	m := summarize(stats, 720*time.Hour)

	got := captureStdout(t, func() { printFetchMix(m, stats, 0) })

	for _, want := range []string{
		"eligible",
		"denominator for every rate",
		"of eligible fetches",
		"provider_content",
		"no request made",
		"the system working",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not explain %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "(8 of 128)") {
		t.Errorf("the gate line does not show its arithmetic:\n%s", got)
	}
}

// TestEmptyCorpusPrintsNothingMisleading.
func TestEmptyCorpusPrintsNothingMisleading(t *testing.T) {
	got := captureStdout(t, func() { printFetchMix(summarize(nil, time.Hour), nil, 5) })
	if !strings.Contains(got, "no fetches recorded") {
		t.Errorf("empty corpus output:\n%s", got)
	}
	if strings.Contains(got, "0.0%") {
		t.Error("an empty corpus printed a rate")
	}
}

func TestWrapAtBreaksOnWords(t *testing.T) {
	got := wrapAt("one two three four five six seven eight", 12, "> ")
	for _, line := range strings.Split(got, "\n") {
		if len(strings.TrimPrefix(line, "> ")) > 12 {
			t.Errorf("line exceeds width: %q", line)
		}
	}
	if strings.Contains(got, "  ") {
		t.Errorf("words were mangled: %q", got)
	}
}
