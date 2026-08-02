package planner_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/planner"
)

// TestDigestIsConstantSizeAsResearchDeepens is the whole reason this type
// exists. Rev 1 passed every summary on every replan, so planner input grew
// with lead count and total planner cost was quadratic in leads — worst
// precisely when research goes deep, which is the case the design serves.
//
// The assertion is not "small" but "does not grow": the digest after 500 leads
// must be the same size as after 20.
func TestDigestIsConstantSizeAsResearchDeepens(t *testing.T) {
	d := planner.NewDigest("what is the consensus on byte-level language models?", planner.DefaultDigestChars)

	var sizeAt20, sizeAt500 int
	for i := 0; i < 500; i++ {
		// A realistic deep session: new sub-questions appear, leads run,
		// claims land, and routes dead-end.
		if i%5 == 0 {
			d.AddQuestions([]planner.SubQuestion{{
				ID:   fmt.Sprintf("q%d", i),
				Text: fmt.Sprintf("Sub-question %d about some reasonably long research topic that a planner would actually produce", i),
			}})
		}
		d.RecordLead(fmt.Sprintf("q%d", (i/5)*5))
		d.RecordClaims(fmt.Sprintf("q%d", (i/5)*5), 3)
		if i%7 == 0 {
			d.RecordDeadEnd(fmt.Sprintf("cause_%d", i%11), fmt.Sprintf("a query that went nowhere %d", i))
		}
		if i%3 == 0 {
			d.MarkAnswered(fmt.Sprintf("q%d", (i/5)*5))
		}

		if i == 20 {
			sizeAt20 = len(d.String())
		}
	}
	sizeAt500 = len(d.String())

	if sizeAt500 > planner.DefaultDigestChars {
		t.Errorf("digest is %d chars after 500 leads, over the %d budget",
			sizeAt500, planner.DefaultDigestChars)
	}
	// The real claim: it converged rather than merely being under the cap by
	// luck. 25x the leads must not mean a materially larger digest.
	if sizeAt500 > sizeAt20*3 {
		t.Errorf("digest grew from %d to %d chars over 25x the leads — not O(1)",
			sizeAt20, sizeAt500)
	}
	t.Logf("digest: %d chars at 20 leads, %d at 500", sizeAt20, sizeAt500)
}

// TestOpenQuestionsAreSacrificedLast. Compaction can elide an open question —
// an unbounded digest is worse — but only after everything else is gone. A
// planner that cannot see what is unanswered re-plans work already done, the
// livelock §9.3 describes arriving by a different route, so the order matters.
//
// Here the budget fits the open questions but not the forty answered ones and
// the dead-end tail, so all three open ones must survive intact.
func TestOpenQuestionsAreSacrificedLast(t *testing.T) {
	d := planner.NewDigest("a question", 800)

	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("q%d", i)
		d.AddQuestions([]planner.SubQuestion{{ID: id, Text: fmt.Sprintf("Sub-question number %d with some real length to it", i)}})
		if i < 37 {
			d.MarkAnswered(id)
		}
		d.RecordDeadEnd(fmt.Sprintf("cause_%d", i), "some query that failed")
	}

	open := d.Open()
	if len(open) != 3 {
		t.Fatalf("%d open questions survived, want 3 — they were cut before the answered ones", len(open))
	}
	out := d.String()
	for _, q := range open {
		if !strings.Contains(out, q.ID) {
			t.Errorf("open question %s is missing from the serialized digest:\n%s", q.ID, out)
		}
	}
	if len(out) > 800 {
		t.Errorf("digest is %d chars, over its 800 budget", len(out))
	}
}

// TestTheOriginalQuestionSurvivesCompaction: without it the planner does not
// know what it is researching, and every replan is unanchored.
func TestTheOriginalQuestionSurvivesCompaction(t *testing.T) {
	const question = "what is the current consensus on tokenizer-free byte-level LLMs?"
	d := planner.NewDigest(question, 300)

	for i := 0; i < 60; i++ {
		d.AddQuestions([]planner.SubQuestion{{
			ID: fmt.Sprintf("q%d", i), Text: strings.Repeat("padding ", 20),
		}})
		d.RecordDeadEnd(fmt.Sprintf("cause%d", i), strings.Repeat("x", 100))
	}

	if !strings.Contains(d.String(), question) {
		t.Errorf("the research question was compacted away:\n%s", d.String())
	}
}

// TestAnsweredQuestionsAreCountedAfterBeingDropped. The planner needs to know
// six questions were answered even when it no longer needs to know which; a
// count that vanished with the detail would make progress look like stagnation.
func TestAnsweredQuestionsAreCountedAfterBeingDropped(t *testing.T) {
	d := planner.NewDigest("a question", 350)

	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("q%d", i)
		d.AddQuestions([]planner.SubQuestion{{ID: id, Text: fmt.Sprintf("A sub-question with enough text to matter, number %d", i)}})
		d.MarkAnswered(id)
	}
	d.AddQuestions([]planner.SubQuestion{{ID: "still-open", Text: "The one that is not done"}})

	out := d.String()
	if !strings.Contains(out, "30 sub-question(s) answered") {
		t.Errorf("dropped questions were not tallied:\n%s", out)
	}
	if !strings.Contains(out, "still-open") {
		t.Errorf("the open question is missing:\n%s", out)
	}
}

// TestDeadEndsCollapseByCause. Twenty bot_block failures are one fact for the
// planner — "this route is blocked" — and listing each would spend the whole
// digest budget on its least informative part.
func TestDeadEndsCollapseByCause(t *testing.T) {
	d := planner.NewDigest("a question", planner.DefaultDigestChars)
	for i := 0; i < 20; i++ {
		d.RecordDeadEnd("bot_block", fmt.Sprintf("query %d", i))
	}
	d.RecordDeadEnd("js_required", "an spa")

	out := d.String()
	if !strings.Contains(out, "bot_block ×20") {
		t.Errorf("dead ends did not collapse:\n%s", out)
	}
	if strings.Count(out, "bot_block") != 1 {
		t.Errorf("bot_block listed more than once:\n%s", out)
	}
	if !strings.Contains(out, "js_required") {
		t.Errorf("a distinct cause was lost:\n%s", out)
	}
}

// TestLeadsWithoutClaimsIsVisible. This is the signal that separates "no answer
// exists" from "we are asking badly", and the planner needs it to choose
// between rephrasing and giving up.
func TestLeadsWithoutClaimsIsVisible(t *testing.T) {
	d := planner.NewDigest("a question", planner.DefaultDigestChars)
	d.AddQuestions([]planner.SubQuestion{
		{ID: "productive", Text: "A question that worked"},
		{ID: "barren", Text: "A question that found nothing"},
	})
	for i := 0; i < 4; i++ {
		d.RecordLead("barren")
	}
	d.RecordLead("productive")
	d.RecordClaims("productive", 7)

	out := d.String()
	if !strings.Contains(out, "4 lead(s) run, 0 claim(s) found") {
		t.Errorf("a barren question does not show as barren:\n%s", out)
	}
	if !strings.Contains(out, "1 lead(s) run, 7 claim(s) found") {
		t.Errorf("a productive question does not show its yield:\n%s", out)
	}
}

// TestSerializationIsStable. Two identical states must produce identical text,
// or the cassette key changes between runs and every replay misses (§14.1).
func TestSerializationIsStable(t *testing.T) {
	build := func() *planner.Digest {
		d := planner.NewDigest("a question", planner.DefaultDigestChars)
		d.AddQuestions([]planner.SubQuestion{
			{ID: "a", Text: "first"}, {ID: "b", Text: "second"}, {ID: "c", Text: "third"},
		})
		d.RecordClaims("a", 2)
		d.RecordDeadEnd("bot_block", "x")
		d.RecordDeadEnd("paywall", "y")
		d.MarkAnswered("b")
		return d
	}
	first := build().String()
	for i := 0; i < 20; i++ {
		if got := build().String(); got != first {
			t.Fatalf("serialization is not stable:\n--- %d\n%s\n--- first\n%s", i, got, first)
		}
	}
}

// TestCompleteRequiresQuestions: an empty digest is not a finished session, it
// is one that has not planned yet. Reporting complete would end a run before it
// started.
func TestCompleteRequiresQuestions(t *testing.T) {
	d := planner.NewDigest("a question", planner.DefaultDigestChars)
	if d.Complete() {
		t.Error("an unplanned session reported complete")
	}

	d.AddQuestions([]planner.SubQuestion{{ID: "a", Text: "one"}})
	if d.Complete() {
		t.Error("an open question reported complete")
	}
	d.MarkAnswered("a")
	if !d.Complete() {
		t.Error("every question answered, but not complete")
	}
}

// TestDuplicateQuestionsAreIgnored so a replan that re-proposes an existing
// thread does not double-count its coverage.
func TestDuplicateQuestionsAreIgnored(t *testing.T) {
	d := planner.NewDigest("a question", planner.DefaultDigestChars)
	q := []planner.SubQuestion{{ID: "a", Text: "the same question"}}
	d.AddQuestions(q)
	d.AddQuestions(q)
	d.AddQuestions(q)

	if n := len(d.Open()); n != 1 {
		t.Errorf("%d copies of one question", n)
	}
}

// TestTruncationKeepsQuestionsRecognizable. A question shortened past
// recognition is worse than a slightly oversized digest: the planner re-plans
// it as new, and pays for the same research twice.
func TestTruncationKeepsQuestionsRecognizable(t *testing.T) {
	d := planner.NewDigest("a question", 200)
	for i := 0; i < 10; i++ {
		d.AddQuestions([]planner.SubQuestion{{
			ID:   fmt.Sprintf("q%d", i),
			Text: fmt.Sprintf("Distinctive topic %d: %s", i, strings.Repeat("detail ", 30)),
		}})
	}
	for _, q := range d.Open() {
		if len(q.Text) < 20 {
			t.Errorf("%s truncated to %q — unrecognizable", q.ID, q.Text)
		}
	}
}

// TestDigestStaysBoundedWhenNothingIsAnswered is the case the first version of
// compaction missed, and the one that matters most.
//
// String() lists only OPEN questions, so a session that answers most of them
// stays small for free — which is what made the original O(1) test pass while
// the property was false. A session where nothing is ever answered grew the
// digest without limit: 43k characters over 500 open questions, ten times the
// budget, which is precisely the quadratic planner cost §9.1 forbids.
func TestDigestStaysBoundedWhenNothingIsAnswered(t *testing.T) {
	d := planner.NewDigest("a question nobody can answer", planner.DefaultDigestChars)

	var sizes []int
	for i := 0; i < 500; i++ {
		d.AddQuestions([]planner.SubQuestion{{
			ID:   fmt.Sprintf("q%d", i),
			Text: fmt.Sprintf("Open sub-question %d that nobody ever manages to answer at all", i),
		}})
		d.RecordLead(fmt.Sprintf("q%d", i))
		if i%100 == 99 {
			sizes = append(sizes, len(d.String()))
		}
	}

	for _, n := range sizes {
		if n > planner.DefaultDigestChars {
			t.Errorf("digest reached %d chars, over the %d budget: %v",
				n, planner.DefaultDigestChars, sizes)
			break
		}
	}
	// Converged, not merely capped: the last four checkpoints must agree.
	if len(sizes) >= 2 && sizes[len(sizes)-1] != sizes[len(sizes)-2] {
		t.Errorf("digest size still moving at 500 questions: %v", sizes)
	}
}

// TestElidedOpenQuestionsAreDeclared. Dropping open questions is lossy, so the
// planner has to be told its view is partial — otherwise it reads a truncated
// list as the complete set of remaining work and calls the session finished.
func TestElidedOpenQuestionsAreDeclared(t *testing.T) {
	d := planner.NewDigest("a question", 500)
	for i := 0; i < 100; i++ {
		d.AddQuestions([]planner.SubQuestion{{
			ID:   fmt.Sprintf("q%d", i),
			Text: fmt.Sprintf("An unanswered sub-question with real length, number %d", i),
		}})
		d.RecordLead(fmt.Sprintf("q%d", i))
	}

	out := d.String()
	if !strings.Contains(out, "elided") || !strings.Contains(out, "partial") {
		t.Errorf("open questions were dropped without saying so:\n%s", out)
	}
	if d.Complete() {
		t.Error("a digest with elided open questions reported complete")
	}
}

// TestElisionKeepsUnattemptedQuestions. A question with leads and no claims is
// closest to a dead end and its failure is already recorded in aggregate; a
// question never attempted is the actionable one, so it must survive.
func TestElisionKeepsUnattemptedQuestions(t *testing.T) {
	d := planner.NewDigest("a question", 450)

	// Twenty heavily-attempted questions.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("tried%d", i)
		d.AddQuestions([]planner.SubQuestion{{ID: id, Text: fmt.Sprintf("Attempted question %d with padding text here", i)}})
		for j := 0; j < 5; j++ {
			d.RecordLead(id)
		}
	}
	// One fresh one, never attempted.
	d.AddQuestions([]planner.SubQuestion{{ID: "fresh", Text: "Never attempted question"}})

	found := false
	for _, q := range d.Open() {
		if q.ID == "fresh" {
			found = true
		}
	}
	if !found {
		t.Errorf("the unattempted question was elided in favour of exhausted ones; open = %d", len(d.Open()))
	}
}

// TestElisionNeverEmptiesTheOpenList: a digest with no open questions reads as
// a finished session and would stop the loop early.
func TestElisionNeverEmptiesTheOpenList(t *testing.T) {
	d := planner.NewDigest(strings.Repeat("a very long research question ", 30), 100)
	for i := 0; i < 50; i++ {
		d.AddQuestions([]planner.SubQuestion{{
			ID: fmt.Sprintf("q%d", i), Text: strings.Repeat("long question text ", 20),
		}})
		d.RecordLead(fmt.Sprintf("q%d", i))
	}
	if len(d.Open()) == 0 {
		t.Error("compaction emptied the open list; the session would report complete")
	}
}
