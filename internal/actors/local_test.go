package actors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/actors"
	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/core"
)

// M8 slice 4.
//
// The actor is the first thing in mole that can reach a connector with a query,
// so these tests are about the seam rather than about the pieces: the gates
// have their own suites, and what has never been exercised is a model's choice
// travelling all the way to a claim.
//
// Two properties carry the milestone. A claim mined from local data cites the
// query that produced it and quotes the numbers it came from, exactly as a web
// claim cites a URL and quotes the page (§11.5). And a model that tries to
// author SQL gets nowhere (§12.3) — asserted by checking that no query ran, not
// merely that no claim came back.

const localCSV = `region,units,revenue,closed_at,rep_note
north,10,1050,2024-01-05,"Customer asked for the invoice to be split across two cost centres"
north,7,880.25,2024-01-22,"Customer asked for the invoice to be split across two cost centres"
north,3,410,2024-02-02,"Shipment held at the depot until the new quarter opened"
north,9,990,2024-02-14,"Shipment held at the depot until the new quarter opened"
north,4,300,2024-03-01,"Customer asked for the invoice to be split across two cost centres"
south,4,220,2024-01-11,"Straightforward renewal with no changes requested at all"
south,6,540,2024-02-19,"Straightforward renewal with no changes requested at all"
south,2,110,2024-03-04,"Shipment held at the depot until the new quarter opened"
south,8,700,2024-03-11,"Straightforward renewal with no changes requested at all"
south,5,480,2024-03-19,"Shipment held at the depot until the new quarter opened"
`

type registry []connector.Connector

func (r registry) List() []connector.Connector { return r }

func localRegistry(t *testing.T) registry {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "tickets.csv")
	if err := os.WriteFile(src, []byte(localCSV), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "sales", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	return registry{c}
}

// scriptedModel answers the planning call with plansJSON, and every mining call
// by quoting a line of the passage it was given — which is what a model reading
// the envelope honestly would do, and what makes §11.5's check pass.
func scriptedModel(plansJSON string, quoteLine func(passage string) (text, quote string)) *fakeLLM {
	return &fakeLLM{mineFunc: func(prompt string) string {
		if strings.Contains(prompt, "Research question:") {
			return plansJSON
		}
		text, quote := quoteLine(prompt)
		raw, _ := json.Marshal(map[string]any{"claims": []map[string]any{
			{"text": text, "quote": quote, "confidence": 0.8},
		}})
		return string(raw)
	}}
}

// quoteAGroupLine lifts a bucket line out of the passage, verbatim.
func quoteAGroupLine(passage string) (string, string) {
	for _, line := range strings.Split(passage, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, " records") && !strings.HasPrefix(line, "-") {
			return "The data shows " + line, line
		}
	}
	return "nothing", "nothing"
}

func localLead() core.Lead {
	return core.Lead{ID: "lead-1", SessionID: "sess-1", Query: "how do the regions compare"}
}

func runLocal(t *testing.T, fl *fakeLLM, reg actors.ConnectorSource) *actors.Result {
	t.Helper()
	a := &actors.LocalComputeActor{Connectors: reg, LLM: fl}
	res, err := a.Run(context.Background(), localLead())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestALocalClaimCitesItsQueryAndQuotesItsNumbers.
func TestALocalClaimCitesItsQueryAndQuotesItsNumbers(t *testing.T) {
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	res := runLocal(t, fl, reg)

	if len(res.Claims) == 0 {
		t.Fatalf("no claims; summary was:\n%s", res.Summary)
	}
	for _, c := range res.Claims {
		// §4's table: a local claim's source is "connector name + query hash".
		if !strings.HasPrefix(c.Source, "connector:sales#") {
			t.Errorf("claim cites %q, want connector:sales#<hash>", c.Source)
		}
		if len(strings.TrimPrefix(c.Source, "connector:sales#")) < 16 {
			t.Errorf("the citation carries no usable query hash: %q", c.Source)
		}
		if strings.TrimSpace(c.Quote) == "" {
			t.Error("a claim crossed with no quote, so §11.5 verified nothing")
		}
		if c.SessionID != "sess-1" || c.LeadID != "lead-1" {
			t.Errorf("claim is not attributed to its lead: %+v", c)
		}
	}

	// Two model calls: one to plan, one to mine. Both are money and both must
	// be in the ledger.
	if len(res.Costs) != 2 {
		t.Fatalf("recorded %d tool call(s), want 2 (plan and mine)", len(res.Costs))
	}
	var planned bool
	for _, c := range res.Costs {
		if c.Input == "local:plan" {
			planned = true
		}
		if c.Type != core.CallLLM {
			t.Errorf("cost row is %q, want an LLM call: %+v", c.Type, c)
		}
	}
	if !planned {
		t.Error("the planning call was not recorded, so a run that plans and finds " +
			"nothing would look free")
	}
}

// TestTheQuoteCheckStillDropsFabrication. The evidence is numbers rather than
// prose, and a model inventing a number is the failure this milestone would be
// most likely to produce and least likely to notice.
func TestTheQuoteCheckStillDropsFabrication(t *testing.T) {
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"group_comparison",
	   "columns":{"key":"region","measure":"revenue"},"question":"revenue by region"}
	]`, func(string) (string, string) {
		return "Revenue in the north fell 40% year on year",
			"north — 812 records, mean 91"
	})

	res := runLocal(t, fl, reg)

	if len(res.Claims) != 0 {
		t.Fatalf("a claim quoting figures that are not in the envelope was kept: %+v",
			res.Claims)
	}
	if res.Stats.ClaimsRejected == 0 {
		t.Error("the rejection was not counted, so a model that fabricates every " +
			"claim would look like one that found nothing")
	}
}

// TestAModelThatWritesSQLGetsNowhere is §12.3 at the actor.
//
// Asserted on the query never running rather than on the absence of claims: a
// plan that produced no claims for some other reason would pass the weaker
// check while the statement had already executed.
func TestAModelThatWritesSQLGetsNowhere(t *testing.T) {
	reg := localRegistry(t)

	for _, tc := range []struct {
		why   string
		plans string
	}{
		{"SQL as the template name", `[
		  {"connector":"sales","table":"tickets",
		   "template":"SELECT rep_note FROM tickets","columns":{"key":"region"}}]`},
		{"SQL in a column slot", `[
		  {"connector":"sales","table":"tickets","template":"distribution",
		   "columns":{"key":"region\" ); DROP TABLE tickets; --"}}]`},
		{"prose as a grouping key", `[
		  {"connector":"sales","table":"tickets","template":"distribution",
		   "columns":{"key":"rep_note"}}]`},
		{"a connector that does not exist", `[
		  {"connector":"payroll","table":"tickets","template":"distribution",
		   "columns":{"key":"region"}}]`},
	} {
		t.Run(tc.why, func(t *testing.T) {
			fl := scriptedModel(tc.plans, quoteAGroupLine)
			res := runLocal(t, fl, reg)

			if len(res.Claims) != 0 {
				t.Fatalf("claims came back: %+v", res.Claims)
			}
			// One call — the plan. A second would mean a query ran and its
			// result reached the miner.
			if len(res.Costs) != 1 {
				t.Fatalf("%d model call(s); a mining call means the statement ran",
					len(res.Costs))
			}
			if res.Stats.ChunksSkipped == 0 {
				t.Error("the refused hypothesis was not counted, so a run that " +
					"refused everything looks like one that was never asked")
			}
		})
	}
}

// TestFreeTextNeverReachesTheModel. The mining prompt is the last place row
// content could escape: it is the one thing in this actor that carries text to
// a hosted API.
func TestFreeTextNeverReachesTheModel(t *testing.T) {
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"group_comparison",
	   "columns":{"key":"region","measure":"units"},"question":"units by region"}
	]`, quoteAGroupLine)

	runLocal(t, fl, reg)

	for _, req := range fl.calls {
		for _, m := range req.Messages {
			if strings.Contains(m.Text, "cost centres") || strings.Contains(m.Text, "the depot") {
				t.Fatalf("row content was sent to the model:\n%s", m.Text)
			}
		}
	}
}

// TestNoRegisteredDataIsNotAnError, and costs nothing. The executor treats a
// failing actor as fatal, so a session that asked for local research on a
// machine with none must degrade rather than end the run.
func TestNoRegisteredDataIsNotAnError(t *testing.T) {
	fl := scriptedModel(`[]`, quoteAGroupLine)
	res := runLocal(t, fl, registry{})

	if len(res.Costs) != 0 {
		t.Errorf("a model was called with nothing to query: %+v", res.Costs)
	}
	if !strings.Contains(res.Summary, "mole connect add") {
		t.Errorf("the summary does not say how to fix it:\n%s", res.Summary)
	}
}

// TestTheHypothesisCountIsBounded. Budget.MaxSources is how many questions one
// lead may put to the data; a model returning more must not be able to spend
// past it.
func TestTheHypothesisCountIsBounded(t *testing.T) {
	reg := localRegistry(t)
	one := `{"connector":"sales","table":"tickets","template":"distribution",
	         "columns":{"key":"region"}}`
	fl := scriptedModel("["+strings.Repeat(one+",", 5)+one+"]", quoteAGroupLine)

	a := &actors.LocalComputeActor{
		Connectors: reg, LLM: fl,
		Budget: actors.Budget{MaxSources: 2},
	}
	res, err := a.Run(context.Background(), localLead())
	if err != nil {
		t.Fatal(err)
	}
	// One plan call plus at most two mining calls.
	if len(res.Costs) > 3 {
		t.Fatalf("%d model call(s) for a ceiling of 2 hypotheses", len(res.Costs))
	}
}

// TestTheSummaryReportsWhatCrossed. The planner sees only this, so a summary
// that omitted the privacy property would make it invisible to the report.
func TestTheSummaryReportsWhatCrossed(t *testing.T) {
	reg := localRegistry(t)
	fl := scriptedModel(`[
	  {"connector":"sales","table":"tickets","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	res := runLocal(t, fl, reg)
	for _, want := range []string{"How do records split by region?", "aggregation gate"} {
		if !strings.Contains(res.Summary, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, res.Summary)
		}
	}
}

// -----------------------------------------------------------------------------
// §4's statistical-validity check, at the actor
// -----------------------------------------------------------------------------

// samplesRegistry builds two groups of n records with a fixed separation, so a
// comparison is either underpowered or significant purely by choosing n.
func samplesRegistry(t *testing.T, n int) registry {
	t.Helper()
	var b strings.Builder
	b.WriteString("region,spend\n")
	for _, g := range []struct {
		name string
		mean float64
	}{{"north", 100}, {"south", 40}} {
		for i := 0; i < n; i++ {
			v := g.mean + 5
			if i%2 == 1 {
				v = g.mean - 5
			}
			fmt.Fprintf(&b, "%s,%.0f\n", g.name, v)
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "samples.csv")
	if err := os.WriteFile(src, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "sales", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	return registry{c}
}

// quoteTheTestLine makes the model cite the statistical summary, which is what
// a model reasoning honestly about a difference would quote.
func quoteTheTestLine(passage string) (string, string) {
	for _, line := range strings.Split(passage, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Welch") || strings.Contains(line, "UNDERPOWERED") {
			return "Spending differs between the regions", line
		}
	}
	return quoteAGroupLine(passage)
}

const comparePlan = `[
  {"connector":"sales","table":"samples","template":"group_comparison",
   "columns":{"key":"region","measure":"spend"},"question":"How does spend differ by region?"}
]`

// TestAnUnsupportedComparisonCapsWhatTheClaimAsserts is §4's check.
//
// The claim may well be true — it is asserting a difference the numbers show —
// but twelve records cannot support it, and §11.3 derives confidence from the
// graph on the strength each source claims for itself. An underpowered result
// must not enter the graph asserting as much as a measured one.
func TestAnUnsupportedComparisonCapsWhatTheClaimAsserts(t *testing.T) {
	for _, tc := range []struct {
		why     string
		n       int
		capped  bool
		verdict string
	}{
		{"six records a side", 6, true, "underpowered"},
		{"forty records a side", 40, false, "significant"},
	} {
		t.Run(tc.why, func(t *testing.T) {
			fl := scriptedModel(comparePlan, quoteTheTestLine)
			res := runLocal(t, fl, samplesRegistry(t, tc.n))

			if len(res.Claims) == 0 {
				t.Fatalf("no claims; summary was:\n%s", res.Summary)
			}
			for _, c := range res.Claims {
				capped := c.AssertionStrength <= 0.3
				if capped != tc.capped {
					t.Errorf("AssertionStrength = %v (capped=%v), want capped=%v for a %s result",
						c.AssertionStrength, capped, tc.capped, tc.verdict)
				}
			}
			if !strings.Contains(res.Summary, tc.verdict) {
				t.Errorf("the planner's summary does not report the verdict %q:\n%s",
					tc.verdict, res.Summary)
			}
		})
	}
}

// TestAQueryWithNoComparisonIsNotCapped. A distribution has no test to fail,
// and capping those would punish claims that are simply counts.
func TestAQueryWithNoComparisonIsNotCapped(t *testing.T) {
	fl := scriptedModel(`[
	  {"connector":"sales","table":"samples","template":"distribution",
	   "columns":{"key":"region"},"question":"How do records split by region?"}
	]`, quoteAGroupLine)

	res := runLocal(t, fl, samplesRegistry(t, 6))
	if len(res.Claims) == 0 {
		t.Fatalf("no claims:\n%s", res.Summary)
	}
	for _, c := range res.Claims {
		if c.AssertionStrength <= 0.3 {
			t.Errorf("a claim from a query with no comparison was capped: %v",
				c.AssertionStrength)
		}
	}
}
