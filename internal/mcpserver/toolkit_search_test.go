package mcpserver_test

import (
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/core"
	. "github.com/lajosdeme/mole/internal/mcpserver"
)

// mole.search had no behavioural test at all: every rig wired a provider that
// answered with nothing, so the clamp, the truncation and the refusals were
// exercised by no test in the phase that shipped them.

// TestSearchClampsWhatItAsksTheProviderFor.
//
// max_results reaches a paid provider, and a caller asking for a thousand gets a
// thousand results' worth of billing and context window.
func TestSearchClampsWhatItAsksTheProviderFor(t *testing.T) {
	prov := &loudSearch{}
	r := connectToolkitSearch(t, prov)
	sess, _ := openWithDoc(t, r)

	var out struct {
		Results []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
		Note string `json:"note"`
	}
	r.call(t, "mole.search", map[string]any{
		"session_id": sess, "query": "fasting", "max_results": 1000}, &out)

	if prov.asked.MaxResults > MaxSearchResults {
		t.Errorf("asked the provider for %d results, limit %d",
			prov.asked.MaxResults, MaxSearchResults)
	}
	if len(out.Results) > MaxSearchResults {
		t.Errorf("returned %d results, limit %d", len(out.Results), MaxSearchResults)
	}
	if !strings.Contains(out.Note, "untrusted") {
		t.Errorf("the note does not say snippets are untrusted: %q", out.Note)
	}
}

// TestProviderSuppliedTextIsBounded.
//
// Titles and snippets are page text relayed by a provider, and they arrive
// outside the fence. Being short is the only protection they have; the title was
// unbounded while the snippet beside it was cut at 300.
func TestProviderSuppliedTextIsBounded(t *testing.T) {
	r := connectToolkitSearch(t, &loudSearch{})
	sess, _ := openWithDoc(t, r)

	var out struct {
		Results []struct {
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	r.call(t, "mole.search", map[string]any{"session_id": sess, "query": "fasting"}, &out)
	if len(out.Results) == 0 {
		t.Fatal("no results; the fixture is wrong")
	}
	for i, res := range out.Results {
		if n := len([]rune(res.Title)); n > MaxTitleChars {
			t.Errorf("result %d: title is %d runes, limit %d", i, n, MaxTitleChars)
		}
		if n := len([]rune(res.Snippet)); n > 300 {
			t.Errorf("result %d: snippet is %d runes, limit 300", i, n)
		}
	}
}

// TestASearchIsChargedAtWhatTheProviderSaidItCost.
//
// The provider's cost is the only real money in toolkit mode, and it used to be
// discarded — a session could run up a provider bill and report spending nothing.
func TestASearchIsChargedAtWhatTheProviderSaidItCost(t *testing.T) {
	r := connectToolkitSearch(t, &loudSearch{cost: core.Cost{USDMicros: 8_000}})
	sess, _ := openWithDoc(t, r)
	r.call(t, "mole.search", map[string]any{"session_id": sess, "query": "fasting"}, nil)

	var closed struct {
		Spent string `json:"spent"`
	}
	r.call(t, "mole.session_close", map[string]any{"session_id": sess}, &closed)
	// Asserted against zero in the session's own unit, whatever that is: an
	// earlier version compared against "$0.00" while the default session was
	// denominated in tokens, so it passed with the cost thrown away.
	if closed.Spent == "" || closed.Spent == core.FormatUSD(0) ||
		strings.HasPrefix(closed.Spent, "0 ") {
		t.Errorf("spent = %q after a search the provider priced at $0.008", closed.Spent)
	}
}

// TestSearchNeedsASession, so a search cannot be made that nothing records.
func TestSearchNeedsASession(t *testing.T) {
	r := connectToolkitSearch(t, &loudSearch{})
	res := r.call(t, "mole.search", map[string]any{
		"session_id": "", "query": "fasting"}, nil)
	if !res.IsError {
		t.Fatal("a search ran with no session to charge or record it")
	}
	// The message matters: the reservation would refuse an unknown session too,
	// but with "budget: ... no such session", which does not tell an agent that
	// it forgot to open one.
	if !strings.Contains(errText(res), "session_open") {
		t.Errorf("the refusal does not say where a session comes from: %s", errText(res))
	}
}

// TestAnEmptyQueryIsRefusedBeforeTheProviderIsPaid.
func TestAnEmptyQueryIsRefusedBeforeTheProviderIsPaid(t *testing.T) {
	prov := &loudSearch{}
	r := connectToolkitSearch(t, prov)
	sess, _ := openWithDoc(t, r)

	res := r.call(t, "mole.search", map[string]any{
		"session_id": sess, "query": "   "}, nil)
	if !res.IsError {
		t.Fatal("an empty query was sent to the provider")
	}
	if prov.asked.MaxResults != 0 {
		t.Error("the provider was called anyway")
	}
}
