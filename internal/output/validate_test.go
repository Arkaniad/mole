package output_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/output"
)

// liveDegenerateBody is verbatim what a 3B model returned as an entire report over
// nine findings: a fragment of the prompt's own instructions, then a bare marker group.
const liveDegenerateBody = "source does NOT support this on re-read\n\n[1][4]"

// liveForgedCitation is verbatim from an earlier live run — a bracket holding claim
// text, which reads as a citation and points at nothing.
const liveForgedCitation = "[subword tokenizers often need a massive vocabulary to cover " +
	"diverse scripts and morphologies.] The evidence is mixed [1]."

// TestADegenerateSynthesisIsRejected. Both bodies here are real: a live run produced
// each one, and both were printed as the answer. The evidence listing was already
// available for nothing and says strictly more than either.
func TestADegenerateSynthesisIsRejected(t *testing.T) {
	var claims []core.Claim
	for i := 0; i < 6; i++ {
		claims = append(claims, claim(
			fmt.Sprintf("Byte-level modelling changes throughput in setting %d, measured on PG-19.", i),
			fmt.Sprintf("https://s%d.example/p", i),
			fmt.Sprintf("a quote long enough to be real evidence %d", i)))
	}
	st, sid := newStore(t, claims)

	cases := map[string]struct {
		body string
		want string
	}{
		"instruction fragment plus a bare marker": {liveDegenerateBody, "less than the shortest"},
		"claim text inside brackets":              {liveForgedCitation, "not a citation"},
		"a citation with no source":               {"The answer is clear [99].", "only 6 source(s) exist"},
		"no citation at all": {
			"Byte-level modelling appears to change throughput across every setting examined here.",
			"cites nothing",
		},
		"empty": {"   ", "returned nothing"},
	}

	for name, tc := range cases {
		f := &fakeLLM{reply: func(string) string { return tc.body }}
		rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.Degraded == "" {
			t.Errorf("%s: accepted, and printed as the answer:\n%s", name, rep.Body)
			continue
		}
		if !strings.Contains(rep.Degraded, tc.want) {
			t.Errorf("%s: reason %q does not name the problem (want %q)", name, rep.Degraded, tc.want)
		}
		// The fallback has to be there, not merely the rejection.
		if !strings.Contains(rep.Body, "unsynthesized") {
			t.Errorf("%s: rejected without falling back to the evidence:\n%s", name, rep.Body)
		}
	}
}

// TestAGoodSynthesisIsAccepted, or the check is just a way to never print a report.
func TestAGoodSynthesisIsAccepted(t *testing.T) {
	claims := []core.Claim{
		claim("Byte-level modelling removes subword tokenization entirely.",
			"https://a.example/1", "a quote long enough to be real evidence one"),
		claim("Throughput drops on long documents without patching.",
			"https://b.example/1", "a quote long enough to be real evidence two"),
	}
	st, sid := newStore(t, claims)

	good := []string{
		"Byte-level modelling removes subword tokenization [1], though throughput drops on " +
			"long documents unless patching is used [2].",
		// Grouped and comma-separated markers are both things models write.
		"The two sources agree that tokenization is removed [1][2].",
		"The two sources agree that tokenization is removed [1, 2].",
	}
	for _, body := range good {
		f := &fakeLLM{reply: func(string) string { return body }}
		rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Degraded != "" {
			t.Errorf("a usable answer was rejected (%s):\n%s", rep.Degraded, body)
		}
		if rep.Body != body {
			t.Errorf("body was altered:\n got %q\nwant %q", rep.Body, body)
		}
	}
}

// TestTheRulesNoLongerHandTheModelACopyableVerdict.
//
// The live failure was not that the model wrote prose badly — it was that the prompt
// contained a finished clause asserting a re-read verdict, and the model lifted it as
// the whole answer. A claim nobody checked was reported as failing its source.
func TestTheRulesNoLongerHandTheModelACopyableVerdict(t *testing.T) {
	no := false
	c := claim("A claim whose source disagrees with it.", "https://a.example/1",
		"a quote long enough to be real evidence one")
	c.Grounded = &no
	st, sid := newStore(t, []core.Claim{c})

	f := &fakeLLM{reply: func(string) string { return "The evidence is weak [1] on this point." }}
	if _, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid); err != nil {
		t.Fatal(err)
	}

	// No sentence in the prompt asserts a verdict a model could paste as an answer.
	for _, banned := range []string{
		"source does NOT support this on re-reading",
		"superseded by a later source",
	} {
		if strings.Contains(f.prompt, banned) {
			t.Errorf("the prompt still contains a liftable verdict clause: %q", banned)
		}
	}
	// The flag still has to reach the model, as a fragment.
	if !strings.Contains(material(f.prompt), "failed source re-read") {
		t.Errorf("the grounding flag no longer reaches the model:\n%s", material(f.prompt))
	}
	// And the model is told not to reproduce the notes.
	if !strings.Contains(f.prompt, "Do not restate these instructions") {
		t.Errorf("the prompt does not forbid echoing itself:\n%s", f.prompt)
	}
}
