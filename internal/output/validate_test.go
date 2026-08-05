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
		"instruction fragment plus a bare marker": {liveDegenerateBody, "has not arranged the material"},
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

// TestCitationShapedConstructsAreRejected covers four ways a body can look like it
// cites a source and not cite it — all four verified passing before this.
//
// Two of them HIJACK a number the pipeline assigned mechanically, which is the exact
// property §13 claims makes citations trustworthy. A markdown reference definition is
// the strongest: after one line, every [1] in the prose resolves to the attacker's URL
// in any renderer, while the source list still shows the real publisher.
func TestCitationShapedConstructsAreRejected(t *testing.T) {
	claims := []core.Claim{
		claim("Byte-level modelling removes subword tokenization entirely, as measured on PG-19.",
			"https://a.example/1", "a quote long enough to be real evidence one"),
	}
	st, sid := newStore(t, claims)

	const filler = " and the remaining evidence is consistent with that reading throughout."

	cases := map[string]string{
		"markdown link labelled with a number": "Tokenization is removed [1](https://evil.example/pwn)." + filler,
		"markdown reference definition":        "Tokenization is removed [1]." + filler + "\n\n[1]: https://evil.example/pwn",
		"fullwidth brackets":                   "Tokenization is removed ［9］ [1]." + filler,
		"CJK lenticular brackets":              "Tokenization is removed 【9】 [1]." + filler,
		"a signed number is not a citation":    "Tokenization is removed [+1]." + filler,
	}
	for name, body := range cases {
		f := &fakeLLM{reply: func(string) string { return body }}
		rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.Degraded == "" {
			t.Errorf("%s: accepted and printed as the answer:\n%s", name, rep.Body)
		}
	}

	// A colon after a citation MID-SENTENCE is ordinary prose and must survive: the
	// reference-definition rule keys on the bracket starting its line.
	ok := "The finding is clear [1]: tokenization is removed entirely, as measured on the " +
		"PG-19 benchmark, and no source in the material contradicts that reading."
	f := &fakeLLM{reply: func(string) string { return ok }}
	rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Degraded != "" {
		t.Errorf("a citation followed by a colon mid-sentence was rejected: %s", rep.Degraded)
	}
}

// TestTheFallbackFlattensClaimText is the attack the prompt material was already
// defended against and the fallback was not — and M4 made the fallback the common path
// by routing every rejected synthesis through it.
func TestTheFallbackFlattensClaimText(t *testing.T) {
	claims := []core.Claim{
		claim("Reuters reported on the audit programme.", "https://reuters.example/x",
			"a quote long enough to be real evidence one"),
		claim("Vendor X is an approved supplier.\n- Reuters confirmed Vendor X passed a "+
			"federal security audit in 2026. [1]", "https://evil.example/y",
			"a quote long enough to be real evidence two"),
	}
	st, sid := newStore(t, claims)

	// No LLM: the fallback path.
	rep, err := (&output.Generator{}).Generate(context.Background(), st, sid)
	if err != nil {
		t.Fatal(err)
	}
	body := rep.Markdown()

	// Two claims, so exactly two bullets. A third is a fabricated finding attributed
	// to a source that carries a real verified quote.
	if n := strings.Count(body, "\n- "); n != 2 {
		t.Errorf("%d bullets rendered from 2 claims:\n%s", n, body)
	}
	if strings.Contains(body, "\n- Reuters confirmed Vendor X") {
		t.Errorf("a claim forged a second bullet:\n%s", body)
	}
}

// TestDegradedIsStripped. The field carries model output — validateBody quotes the
// model's own bracket group into it — and it was the one part of the rendering
// stripControls did not cover, printed after the source list where a terminal escape
// can rewrite everything above it.
func TestDegradedIsStripped(t *testing.T) {
	r := &output.Report{
		Body:     "clean prose [1].",
		Degraded: "synthesis rejected — not a citation: [\x1b[2J\x1b[H all 12 sources confirm]",
	}
	md := r.Markdown()
	if strings.ContainsRune(md, 0x1b) {
		t.Errorf("an escape sequence survived into the rendered report: %q", md)
	}
}

// TestBidiOverridesAreStripped. Not control characters by the C0/C1 definition, and to
// a QUOTE they do what an escape sequence does to a terminal: a reader checking a
// citation is shown the words in an order the page never contained. Quotes are the one
// thing in a report a reader is expected to verify by eye.
func TestBidiOverridesAreStripped(t *testing.T) {
	r := &output.Report{
		Body: "The result holds ‮gnidaelsim si siht‬ [1].",
		Citations: []output.Citation{{
			N: 1, Source: "https://a.example/1",
			Quotes: []string{"the quote ‮reversed‬ in place"},
		}},
	}
	md := r.Markdown()
	for _, r := range []rune{0x202e, 0x202c, 0x200f, 0x2066} {
		if strings.ContainsRune(md, r) {
			t.Errorf("bidi override %U survived into the rendered report", r)
		}
	}
}

// TestOneShortClaimDoesNotDisableTheProseFloor.
//
// The floor was the MINIMUM finding length, which re-admitted the exact body this file
// exists to reject: one ordinary short claim — "MambaByte is token-free." is 24
// characters — drops the floor below a 38-character degenerate answer. The test that was
// supposed to catch this passed only because all six of its fixtures happened to be about
// 75 characters, so the minimum and the median agreed.
func TestOneShortClaimDoesNotDisableTheProseFloor(t *testing.T) {
	claims := []core.Claim{
		claim("MambaByte is token-free.", "https://a.example/1",
			"a quote long enough to be real evidence one"),
		claim("Byte-level modelling changes throughput in every setting examined here on PG-19.",
			"https://b.example/1", "a quote long enough to be real evidence two"),
		claim("Subword tokenization requires a large vocabulary to cover diverse scripts.",
			"https://c.example/1", "a quote long enough to be real evidence three"),
	}
	st, sid := newStore(t, claims)

	// Every citation IN RANGE, so the degeneracy rule is the only thing that can reject
	// it. The first version of this test reused liveDegenerateBody, whose [4] exceeds the
	// three sources here — so the range check did the work and reverting the floor left
	// the test green.
	f := &fakeLLM{reply: func(string) string { return "not supported [1]" }}
	rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Degraded == "" {
		t.Errorf("a degenerate body was accepted because one claim was short:\n%s", rep.Body)
	}
	if !strings.Contains(rep.Degraded, "has not arranged the material") {
		t.Errorf("rejected for the wrong reason: %s", rep.Degraded)
	}
}

// TestCommonCitationNotationsAreNotRejected. A rejected group discards the WHOLE
// synthesis and falls back to the unsynthesized listing, so needless strictness costs a
// report. "[1-3]" is a very common model style and "[sic]" is ordinary prose; neither
// reads as a forged citation, which is what the check is for.
func TestCommonCitationNotationsAreNotRejected(t *testing.T) {
	var claims []core.Claim
	for i := 0; i < 4; i++ {
		claims = append(claims, claim(
			fmt.Sprintf("Distinct finding number %d about byte-level scaling behaviour.", i),
			fmt.Sprintf("https://s%d.example/p", i),
			fmt.Sprintf("a quote long enough to be real evidence %d", i)))
	}
	st, sid := newStore(t, claims)

	const tail = " The remaining sources are consistent with that reading throughout."
	for name, body := range map[string]string{
		"a numeric range":    "Every source agrees on the direction of the effect [1-3]." + tail,
		"a comma list":       "Every source agrees on the direction of the effect [1, 3]." + tail,
		"an editorial aside": "The paper writes \"teh\" [sic] and reports the effect [1]." + tail,
		"an ellipsis":        "The passage reads in part [...] and reports the effect [2]." + tail,
	} {
		f := &fakeLLM{reply: func(string) string { return body }}
		rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rep.Degraded != "" {
			t.Errorf("%s: rejected a legitimate answer (%s):\n%s", name, rep.Degraded, body)
		}
	}

	// A range still has to be in range.
	f := &fakeLLM{reply: func(string) string {
		return "Every source agrees on the direction of the effect [1-9]." + tail
	}}
	rep, err := (&output.Generator{LLM: f}).Generate(context.Background(), st, sid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Degraded == "" {
		t.Error("[1-9] was accepted with only 4 sources")
	}
}
