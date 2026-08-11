package actors_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/core"
)

// A quote that drops the qualification it was supposed to carry.
//
// Found on the first live DeepSeek run of M8, and it defeated the design as
// written. Every comment in internal/compute/stats claiming a correction is safe
// "because it is in the sentence a claim must quote" assumed a quote covers the
// whole sentence. A verbatim PREFIX is also a verbatim substring, so the model
// quoted up to "(p = <0.001)", stopped, and the Holm correction, the effect size
// and the holdout stability vanished from the claim, the citation and the report.

// truncatingModel plans one comparison, then quotes the verdict line only as far
// as the p-value — exactly what DeepSeek did unprompted.
func truncatingModel(cut string) *fakeLLM {
	return &fakeLLM{mineFunc: func(prompt string) string {
		if strings.Contains(prompt, "Research question:") {
			return comparePlan
		}
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			i := strings.Index(line, cut)
			if i < 0 {
				continue
			}
			prefix := line[:i+len(cut)]
			return `{"claims":[{"text":"Spend differs between the regions.",` +
				`"quote":` + jsonString(prefix) + `,"confidence":0.9}]}`
		}
		return `{"claims":[]}`
	}}
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestATruncatedVerdictQuoteIsWidenedToTheWholeSentence.
func TestATruncatedVerdictQuoteIsWidenedToTheWholeSentence(t *testing.T) {
	db, sessionID := crossingStore(t)
	a := &actors.LocalComputeActor{
		Connectors: samplesRegistry(t, 40),
		LLM:        truncatingModel("statistically significant (p = <0.001)"),
		Store:      db,
	}
	res, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "do the regions differ",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) == 0 {
		t.Fatal("no claim survived; the fixture cannot observe the widening")
	}

	c := res.Claims[0]
	if !strings.Contains(c.Quote, "effect size") {
		t.Errorf("the quote still stops at the p-value, so the effect size is lost:\n%q",
			c.Quote)
	}
	if !strings.Contains(c.Quote, "Welch t") {
		t.Errorf("the quote does not carry the test statistic:\n%q", c.Quote)
	}
	// And it is still verbatim: the widened text has to appear in the passage, or
	// the fix has traded §11.5 for a nicer-looking citation.
	if strings.Count(c.Quote, "statistically significant") != 1 {
		t.Errorf("the widened quote is malformed:\n%q", c.Quote)
	}
	if c.QuoteOffset < 0 {
		t.Errorf("offset = %d", c.QuoteOffset)
	}
}

// TestWideningKeepsTheQuoteVerbatim, checked against the passage the miner saw.
func TestWideningKeepsTheQuoteVerbatim(t *testing.T) {
	db, sessionID := crossingStore(t)

	var passage string
	inner := truncatingModel("statistically significant (p = <0.001)")
	a := &actors.LocalComputeActor{
		Connectors: samplesRegistry(t, 40), Store: db,
		LLM: &fakeLLM{mineFunc: func(prompt string) string {
			if !strings.Contains(prompt, "Research question:") {
				passage = prompt
			}
			return inner.mineFunc(prompt)
		}},
	}
	res, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "do the regions differ",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) == 0 {
		t.Fatal("no claim survived")
	}
	q := res.Claims[0].Quote
	if !strings.Contains(passage, q) {
		t.Errorf("the widened quote is not in the passage:\n%q", q)
	}
	// The OFFSET cannot be checked from out here: it is relative to the evidence
	// passage the actor built, and this test only sees the prompt that wraps it.
	// TestTheWidenedOffsetPointsAtTheQuote checks it in-package, where the passage
	// is in hand.
}

// TestABucketQuoteIsNotWidened.
//
// Only verdict lines are widened. A bucket line is meant to be checked figure by
// figure, and widening every quote would make a short one look thorough for no gain.
func TestABucketQuoteIsNotWidened(t *testing.T) {
	db, sessionID := crossingStore(t)
	a := &actors.LocalComputeActor{
		Connectors: localRegistry(t), Store: db,
		LLM: scriptedModel(`[
		  {"connector":"sales","table":"tickets","template":"distribution",
		   "columns":{"key":"region"},"question":"How do records split by region?"}
		]`, func(passage string) (string, string) {
			for _, line := range strings.Split(passage, "\n") {
				line = strings.TrimSpace(line)
				if strings.Contains(line, " records") && !strings.HasPrefix(line, "-") {
					// A deliberate prefix of a NON-verdict line.
					if i := strings.Index(line, " records"); i > 0 {
						return "The data shows " + line, line[:i+len(" records")]
					}
				}
			}
			return "nothing", "nothing"
		}),
	}
	res, err := a.Run(context.Background(), core.Lead{
		ID: "lead-1", SessionID: sessionID, Query: "how do the regions compare",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Claims) == 0 {
		t.Skip("the fixture produced no claim; nothing to observe")
	}
	if strings.Contains(res.Claims[0].Quote, "statistically") {
		t.Errorf("a bucket quote was widened into a verdict line: %q", res.Claims[0].Quote)
	}
}
