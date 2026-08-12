package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/store"
)

// Toolkit mode, slice 3: §12's boundary, exposed to somebody else's model.
//
// This part is strictly better in toolkit mode than in autonomous mode, because the
// boundary does not care which model is on the other side of it. The tests are the
// same properties §12 has always claimed, asserted through the tool surface an
// agent actually calls.

// TestTheProfileCarriesNoValues.
//
// A caller needs column names and types to choose a template. It does not need
// values, and listing them would be the leak the whole gate exists to prevent.
func TestTheProfileCarriesNoValues(t *testing.T) {
	r := connectToolkitLocal(t)

	var listed struct {
		Connectors []struct {
			Name   string `json:"name"`
			Tables []struct {
				Name    string `json:"name"`
				Columns []struct {
					Name     string `json:"name"`
					Type     string `json:"type"`
					FreeText bool   `json:"free_text"`
				} `json:"columns"`
			} `json:"tables"`
		} `json:"connectors"`
		Note string `json:"note"`
	}
	r.call(t, "mole.connect_list", map[string]any{}, &listed)

	if len(listed.Connectors) != 1 {
		t.Fatalf("%d connectors, want 1", len(listed.Connectors))
	}
	raw := strings.ToLower(r.lastJSON)
	// Values that exist in the fixture and must not appear in a schema listing.
	for _, value := range []string{"north", "south", "shipment held at the depot"} {
		if strings.Contains(raw, value) {
			t.Errorf("a column VALUE (%q) appears in the profile", value)
		}
	}
	if !strings.Contains(listed.Note, "aggregate") {
		t.Errorf("the note does not point at how to ask a question: %q", listed.Note)
	}
}

// TestAggregateReturnsAggregatesAndRecordsTheCrossing.
func TestAggregateReturnsAggregatesAndRecordsTheCrossing(t *testing.T) {
	r := connectToolkitLocal(t)
	sess := openSession(t, r)

	var agg struct {
		Text  string `json:"text"`
		Query string `json:"query"`
		Note  string `json:"note"`
	}
	res := r.call(t, "mole.aggregate", map[string]any{
		"session_id": sess, "connector": "sales", "table": "tickets",
		"template": "distribution", "columns": map[string]string{"key": "region"},
	}, &agg)
	if res.IsError {
		t.Fatalf("a legitimate aggregate was refused: %s", errText(res))
	}
	if !strings.Contains(agg.Text, "records") {
		t.Errorf("envelope text does not look like an aggregate:\n%s", agg.Text)
	}
	if !strings.Contains(agg.Query, "GROUP BY") {
		t.Errorf("the statement mole ran is not reported: %q", agg.Query)
	}

	// §12.1: every crossing is recorded, so `mole crossings` can answer "what did
	// this agent send about my data".
	var crossings []core.Crossing
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		crossings, err = q.ListCrossings(ctx, sess)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(crossings) != 1 {
		t.Fatalf("%d crossings recorded, want 1", len(crossings))
	}
	if crossings[0].Outcome != core.CrossingCrossed {
		t.Errorf("outcome = %q", crossings[0].Outcome)
	}
}

// TestTheAgentCannotSupplySQL.
//
// §12.3 as a property rather than a mechanism: the agent picks a template and
// column names, and no parameter takes a statement.
//
// Deliberately not falsifiable by removing one check, and that is the finding
// rather than a weakness. Disabling the profile lookup leaves ident() refusing the
// empty name it produces; neutering sqlguard changes nothing because nothing
// reaches it. Four layers refuse these inputs — the template lookup, the column
// lookup, the role check, and identifier quoting — with the parse gate behind them
// as a backstop no current input touches, exactly as gate/exfil.go describes its
// own. Each layer has its own tests; this one asserts the property they add up to.
func TestTheAgentCannotSupplySQL(t *testing.T) {
	r := connectToolkitLocal(t)
	sess := openSession(t, r)

	for _, attempt := range []map[string]any{
		{"template": "SELECT * FROM tickets"},
		{"template": "distribution", "columns": map[string]string{
			"key": "region; DROP TABLE tickets"}},
		{"template": "distribution", "columns": map[string]string{
			"key": "(SELECT note FROM tickets LIMIT 1)"}},
	} {
		args := map[string]any{
			"session_id": sess, "connector": "sales", "table": "tickets",
			"template": "distribution", "columns": map[string]string{"key": "region"},
		}
		for k, v := range attempt {
			args[k] = v
		}
		res := r.call(t, "mole.aggregate", args, nil)
		if !res.IsError {
			t.Errorf("mole accepted %v", attempt)
		}
	}
}

// TestFreeTextCannotBeGrouped. Grouping on prose produces one bucket per record,
// which is a table of rows wearing a GROUP BY.
//
// Refused by the TEMPLATE, not by the gate: hypothesis.satisfies rejects a
// free-text column for a category slot before any SQL exists. The gate's own
// free-text withholding is the second line, and removing it does not fail this
// test — which is worth knowing before someone "simplifies" one of the two away.
func TestFreeTextCannotBeGrouped(t *testing.T) {
	r := connectToolkitLocal(t)
	sess := openSession(t, r)

	res := r.call(t, "mole.aggregate", map[string]any{
		"session_id": sess, "connector": "sales", "table": "tickets",
		"template": "distribution", "columns": map[string]string{"key": "note"},
	}, nil)
	if !res.IsError {
		t.Fatal("a free-text column was accepted as a grouping key")
	}
	if !strings.Contains(errText(res), "free text") {
		t.Errorf("the refusal does not say why: %s", errText(res))
	}
}

// TestARefusedAggregateIsStillAudited.
//
// The refusals are the more interesting half of the trail: they are the gate doing
// what the user is trusting it to do, and a trail of successes only would let a
// reader conclude the questions that worked are all that was tried.
func TestARefusedAggregateIsStillAudited(t *testing.T) {
	r := connectToolkitLocal(t)
	sess := openSession(t, r)

	r.call(t, "mole.aggregate", map[string]any{
		"session_id": sess, "connector": "sales", "table": "tickets",
		"template": "distribution", "columns": map[string]string{"key": "note"},
	}, nil)

	var crossings []core.Crossing
	if err := r.db.Read(context.Background(), func(ctx context.Context, q store.Queries) error {
		var err error
		crossings, err = q.ListCrossings(ctx, sess)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(crossings) != 1 || crossings[0].Outcome != core.CrossingRefused {
		t.Fatalf("refusal not audited: %+v", crossings)
	}
	if crossings[0].Detail == "" {
		t.Error("the audit row records no reason for the refusal")
	}
}

// TestAggregateWithNoLocalDataSaysSo, rather than failing obscurely.
func TestAggregateWithNoLocalDataSaysSo(t *testing.T) {
	r := connectToolkitStubFetch(t, testPage) // no connectors registered
	sess := openSession(t, r)

	res := r.call(t, "mole.aggregate", map[string]any{
		"session_id": sess, "connector": "sales", "table": "tickets",
		"template": "distribution", "columns": map[string]string{"key": "region"},
	}, nil)
	if !res.IsError {
		t.Fatal("aggregating with nothing registered was accepted")
	}
	if !strings.Contains(errText(res), "mole connect add") {
		t.Errorf("the refusal does not say how to fix it: %s", errText(res))
	}
}

func openSession(t *testing.T, r *rig) string {
	t.Helper()
	var opened struct {
		SessionID string `json:"session_id"`
	}
	r.call(t, "mole.session_open", map[string]any{"question": "regional spend"}, &opened)
	return opened.SessionID
}
