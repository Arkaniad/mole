package actors

import (
	"strings"
	"testing"
)

// The exact responses a live qwen2.5:3b produced. Each cost every claim in the
// call, including the ones that had arrived whole.
const (
	truncatedCompact = `{"claims":[{"text":"MambaByte provides a viable token-free alternative to subword transformers.","quote":"MambaByte offers a viable token-free alternative","confidence":0.9},{"text":"MambaByte provides a viable token-free alternative to subwor`

	truncatedPretty = `{
 "claims": [
 {
   "text": "MambaByte's core innovation is the selective state space model.",
   "quote": "the adoption of the selective state space model",
   "confidence": 0.8
 },
 {
   "text": "MambaByte's core innovation is the adoption of the`

	wrappedInArray = `[
  {"claims":[
    {"text":"The proposed architecture, MambaByte, is designed for byte-level modelling.",
     "quote":"MambaByte is designed for byte-level modelling","confidence":0.7},
    {"text":"The proposed architecture, MambaByte, is designed f`
)

// TestTruncatedResponsesKeepTheirCompleteClaims. A 3B model asked for eight
// claims routinely hits its output ceiling mid-JSON, and the strict parser
// returned nothing for the whole response — three of four mining calls on a live
// run, each discarding claims that had arrived intact.
//
// Salvaging is safe because of §11.5: a recovered claim still has to carry a
// quote that appears verbatim in the chunk, so anything half-parsed dies at the
// actor boundary. The only thing this changes is whether COMPLETE claims are
// thrown away alongside the incomplete one.
func TestTruncatedResponsesKeepTheirCompleteClaims(t *testing.T) {
	for name, raw := range map[string]string{
		"compact":    truncatedCompact,
		"pretty":     truncatedPretty,
		"in a array": wrappedInArray,
	} {
		claims, err := parseMined(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(claims) != 1 {
			t.Errorf("%s: recovered %d claims, want the 1 that completed", name, len(claims))
			continue
		}
		if claims[0].Text == "" || claims[0].Quote == "" {
			t.Errorf("%s: recovered a claim missing text or quote: %+v", name, claims[0])
		}
		// The truncated tail must not become a claim.
		for _, c := range claims {
			if strings.HasSuffix(c.Text, "subwor") || strings.HasSuffix(c.Text, "of the") {
				t.Errorf("%s: the truncated object was accepted: %q", name, c.Text)
			}
		}
	}
}

// TestCompleteResponsesTakeTheStrictPath, so salvage is a fallback and not the
// primary parser — it accepts shapes the strict parser would reject.
func TestCompleteResponsesTakeTheStrictPath(t *testing.T) {
	raw := `{"claims":[
		{"text":"First claim.","quote":"a quote for the first","confidence":0.9},
		{"text":"Second claim.","quote":"a quote for the second","confidence":0.5}
	]}`
	claims, err := parseMined(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("%d claims, want 2", len(claims))
	}
	if claims[0].Confidence != 0.9 {
		t.Errorf("confidence lost: %+v", claims[0])
	}
}

// TestEmptyClaimsListIsNotAnError: the prompt explicitly asks for {"claims":[]}
// when a document says nothing relevant, and treating that as a parse failure
// would turn a correct answer into a degraded lead.
func TestEmptyClaimsListIsNotAnError(t *testing.T) {
	claims, err := parseMined(`{"claims":[]}`)
	if err != nil {
		t.Errorf("an empty claims list was rejected: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("%d claims from an empty list", len(claims))
	}
}

// TestGenuineGarbageStillFails, or the salvage path would mask a broken prompt.
func TestGenuineGarbageStillFails(t *testing.T) {
	for _, raw := range []string{
		"",
		"I cannot help with that request.",
		"{{{{",
		`{"unrelated":"object"}`,
		// A claim with no quote is deliberately NOT here: that is valid JSON, the
		// strict path returns it, and FindQuote rejects it at the actor boundary
		// (§11.5). Parsing is not where that check belongs.
	} {
		if claims, err := parseMined(raw); err == nil && len(claims) > 0 {
			t.Errorf("garbage accepted (%d claims): %.60q", len(claims), raw)
		}
	}
}

// TestSalvageIgnoresTheWrapperObject. The outer {"claims": ...} is itself a
// balanced object; treating it as a claim would yield one empty entry and stop.
func TestSalvageIgnoresTheWrapperObject(t *testing.T) {
	raw := `{"claims":[{"text":"A real claim.","quote":"a real quote here","confidence":0.6}],`
	claims := salvageClaims(raw)
	if len(claims) != 1 {
		t.Fatalf("%d claims, want 1", len(claims))
	}
	if claims[0].Text != "A real claim." {
		t.Errorf("recovered %+v", claims[0])
	}
}
