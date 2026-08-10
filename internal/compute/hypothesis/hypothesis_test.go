package hypothesis_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/compute/connector"
	"github.com/lajosdeme/mole/internal/compute/gate"
	"github.com/lajosdeme/mole/internal/compute/hypothesis"
	"github.com/lajosdeme/mole/internal/compute/sqlguard"
)

// M8 slice 4.
//
// Two things are being tested and the first matters more. A template that
// renders SQL the gates then refuse is a template that never works, and nothing
// upstream would say so — the model would choose it, the query would be built,
// the gate would say no, and the run would report "no hypothesis could be
// answered". So every template is run end to end against a real database.
//
// The second is §12.3's actual claim: a model can influence template and column
// choice and cannot author SQL. That is tested by trying to author some.

// tickets.csv is shaped so every template has something to bind to: a category,
// a measure, a time axis, and a column that is none of those.
const ticketsCSV = `region,units,revenue,closed_at,rep_note
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

func testConnector(t *testing.T) connector.Connector {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "tickets.csv")
	if err := os.WriteFile(src, []byte(ticketsCSV), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "local", src,
		filepath.Join(dir, "scratch.db"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// plansForEveryTemplate binds each template to columns that satisfy it.
func plansForEveryTemplate() []hypothesis.Plan {
	return []hypothesis.Plan{
		{Connector: "local", Table: "tickets", Template: hypothesis.KindDistribution,
			Columns: map[string]string{"key": "region"}},
		{Connector: "local", Table: "tickets", Template: hypothesis.KindOverview,
			Columns: map[string]string{"measure": "revenue"}},
		{Connector: "local", Table: "tickets", Template: hypothesis.KindGroupCompare,
			Columns: map[string]string{"key": "region", "measure": "revenue"}},
		{Connector: "local", Table: "tickets", Template: hypothesis.KindTrend,
			Columns: map[string]string{"time": "closed_at", "measure": "units"}},
		{Connector: "local", Table: "tickets", Template: hypothesis.KindTrend,
			Columns: map[string]string{"time": "closed_at"}},
	}
}

// TestEveryTemplateSurvivesBothGates is the property that matters.
//
// A template is only useful if sqlguard permits it, the gate classifies it as
// an aggregate, and the result is something a model can read. Checking the SQL
// string against an expectation would test that the template is what it is;
// running it tests that it works.
func TestEveryTemplateSurvivesBothGates(t *testing.T) {
	c := testConnector(t)
	db, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	covered := map[hypothesis.Kind]bool{}
	for _, p := range plansForEveryTemplate() {
		t.Run(string(p.Template)+"/"+strings.Join(slotNames(p), "+"), func(t *testing.T) {
			query, err := hypothesis.Render(c, p)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if err := sqlguard.Check(query); err != nil {
				t.Fatalf("a rendered template was refused by the parse gate: %v\n%s", err, query)
			}
			// The floor is lowered because the fixture is ten rows; the rule
			// being tested is that the template PRODUCES an aggregate, not that
			// ten rows clear a production floor.
			env, err := gate.Aggregate(context.Background(), db, query, gate.Options{KFloor: 2})
			if err != nil {
				t.Fatalf("a rendered template was refused by the aggregation gate: %v\n%s",
					err, query)
			}
			if env.RowCount == 0 {
				t.Fatalf("the envelope describes nothing:\n%s", query)
			}
			text := env.Text()
			if !strings.Contains(text, "Rows in the result") {
				t.Fatalf("the rendering is not readable:\n%s", text)
			}
			// Every claim mined from this has to quote it, so a rendering with
			// no figures in it would make every claim unciteable.
			if !strings.ContainsAny(text, "0123456789") {
				t.Fatalf("the rendering carries no figures:\n%s", text)
			}
			covered[p.Template] = true
		})
	}

	for _, tpl := range hypothesis.Templates {
		if !covered[tpl.Kind] {
			t.Errorf("template %s was never run; it could be refused by a gate and "+
				"nothing here would say so", tpl.Kind)
		}
	}
}

func slotNames(p hypothesis.Plan) []string {
	var out []string
	for _, k := range []string{"key", "measure", "time"} {
		if p.Columns[k] != "" {
			out = append(out, k)
		}
	}
	return out
}

// TestAModelCannotAuthorSQL is §12.3.
//
// The names a model returns are lookup keys, never text spliced into a
// statement, so a name carrying a quote or a whole second statement fails to
// match a column and the plan is refused. There is no escaping to get wrong.
func TestAModelCannotAuthorSQL(t *testing.T) {
	c := testConnector(t)

	for _, tc := range []struct {
		why  string
		plan hypothesis.Plan
		want string
	}{
		{"a statement in the column slot", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindDistribution,
			Columns: map[string]string{"key": `region" ); DROP TABLE tickets; --`},
		}, "no column"},
		{"a subquery in the column slot", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindOverview,
			Columns: map[string]string{"measure": `(SELECT readfile('/etc/passwd'))`},
		}, "no column"},
		{"a statement in the table slot", hypothesis.Plan{
			Table: `tickets"; DROP TABLE tickets; --`, Template: hypothesis.KindDistribution,
			Columns: map[string]string{"key": "region"},
		}, "no table"},
		{"a statement as the template name", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.Kind(`SELECT * FROM tickets`),
			Columns: map[string]string{"key": "region"},
		}, "no template"},
		{"a column from another table", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindDistribution,
			Columns: map[string]string{"key": "sqlite_master"},
		}, "no column"},
		{"nothing in a required slot", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindGroupCompare,
			Columns: map[string]string{"key": "region"},
		}, "needs a column"},
	} {
		t.Run(tc.why, func(t *testing.T) {
			query, err := hypothesis.Render(c, tc.plan)
			if err == nil {
				t.Fatalf("PERMITTED, rendered: %s", query)
			}
			if !errors.Is(err, hypothesis.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for the wrong reason\n  got:  %v\n  want: %q", err, tc.want)
			}
		})
	}
}

// TestASlotOnlyTakesAColumnThatFitsIt. The role check is what stops a template
// rendering a statement the gate would then refuse — grouping on prose, or
// averaging a region name.
func TestASlotOnlyTakesAColumnThatFitsIt(t *testing.T) {
	c := testConnector(t)

	for _, tc := range []struct {
		why  string
		plan hypothesis.Plan
		want string
	}{
		{"prose as a grouping key", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindDistribution,
			Columns: map[string]string{"key": "rep_note"},
		}, "free text"},
		{"a label as a measure", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindOverview,
			Columns: map[string]string{"measure": "region"},
		}, "must be numeric"},
		{"a number as a time axis", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindTrend,
			Columns: map[string]string{"time": "units"},
		}, "must be a timestamp"},
		{"a continuous number as a grouping key", hypothesis.Plan{
			Table: "tickets", Template: hypothesis.KindDistribution,
			Columns: map[string]string{"key": "revenue"},
		}, "do not group"},
	} {
		t.Run(tc.why, func(t *testing.T) {
			query, err := hypothesis.Render(c, tc.plan)
			if err == nil {
				t.Fatalf("PERMITTED, rendered: %s", query)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refused for the wrong reason\n  got:  %v\n  want: %q", err, tc.want)
			}
		})
	}
}

// TestARefusalNamesWhatWasAvailable. Every refusal here is a model that made a
// wrong choice, and the next attempt is only better if the error says what the
// choices were.
func TestARefusalNamesWhatWasAvailable(t *testing.T) {
	c := testConnector(t)
	_, err := hypothesis.Render(c, hypothesis.Plan{
		Table: "tickets", Template: hypothesis.KindDistribution,
		Columns: map[string]string{"key": "regionn"},
	})
	if err == nil {
		t.Fatal("a misspelled column was accepted")
	}
	for _, want := range []string{"region", "units", "closed_at"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list %q as an option: %v", want, err)
		}
	}
}

// TestGroupingKeysAreBoundedByCardinality. Not a safety rule — the floor is
// that — but a usefulness one: grouping by a near-unique column produces
// buckets of one that the gate then suppresses, and an empty envelope tells
// nobody why.
func TestGroupingKeysAreBoundedByCardinality(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("ref,amount\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "r%03d,%d\n", i, i)
	}
	src := filepath.Join(dir, "wide.csv")
	if err := os.WriteFile(src, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := connector.Ingest(context.Background(), "local", src, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = hypothesis.Render(c, hypothesis.Plan{
		Table: "wide", Template: hypothesis.KindDistribution,
		Columns: map[string]string{"key": "ref"},
	})
	if err == nil {
		t.Fatal("a 300-value column was accepted as a grouping key")
	}
	if !strings.Contains(err.Error(), "distinct") {
		t.Errorf("the refusal does not say the cardinality is the problem: %v", err)
	}
}
