package fetch_test

import (
	"testing"

	"github.com/lajosdeme/mole/internal/tools/fetch"
)

const spaBody = `<!doctype html><html><head>
<script src="/static/bundle.js"></script>
<script src="/static/vendor.js"></script>
</head><body><div id="root"></div></body></html>`

func TestClassifyBodyUsableTextIsOK(t *testing.T) {
	if got := fetch.ClassifyBody(spaBody, 5000); got != fetch.OutcomeOK {
		t.Errorf("outcome = %q, want ok — extraction succeeded", got)
	}
}

func TestClassifyBodyDetectsSPA(t *testing.T) {
	if got := fetch.ClassifyBody(spaBody, 12); got != fetch.OutcomeJSRequired {
		t.Errorf("outcome = %q, want js_required", got)
	}

	// An app-root with no scripts is not a client-rendered page.
	noScripts := `<html><body><div id="root"></div></body></html>`
	if got := fetch.ClassifyBody(noScripts, 12); got == fetch.OutcomeJSRequired {
		t.Error("classified as js_required without script tags")
	}
}

// TestBotBlockOutranksSPASignature is the precedence that protects §17.1's
// gate. A challenge page is also script-heavy with an empty body; counting it
// as js_required would inflate the single number the headless-browser decision
// turns on.
func TestBotBlockOutranksSPASignature(t *testing.T) {
	challenge := `<!doctype html><html><head>
<script src="/cdn-cgi/challenge.js"></script><script>window.x=1</script>
</head><body><div id="root">Just a moment...</div>
<p>Checking your browser before accessing the site.</p></body></html>`

	if got := fetch.ClassifyBody(challenge, 40); got != fetch.OutcomeBotBlock {
		t.Errorf("outcome = %q, want bot_block", got)
	}
	if fetch.OutcomeBotBlock.CapabilityGap() {
		t.Error("bot_block must not count as a capability gap")
	}
}

func TestConsentWallAndPaywallDetected(t *testing.T) {
	consent := `<html><head><script src="https://cdn.cookielaw.org/otSDK.js"></script>
<script>window.OneTrust={}</script></head><body><div id="root"></div></body></html>`
	if got := fetch.ClassifyBody(consent, 30); got != fetch.OutcomeConsentWall {
		t.Errorf("consent outcome = %q, want consent_wall", got)
	}

	paywall := `<html><body><script type="application/ld+json">
{"@type":"NewsArticle","isAccessibleForFree":false}</script>
<div>Subscribe to continue reading</div></body></html>`
	if got := fetch.ClassifyBody(paywall, 30); got != fetch.OutcomePaywall {
		t.Errorf("paywall outcome = %q, want paywall", got)
	}
}

// TestStructuredOnlyIsSeparateFromJSRequired: these need a day of parser work,
// not a browser, so lumping them together would overstate the case for one.
func TestStructuredOnlyIsSeparateFromJSRequired(t *testing.T) {
	nextData := `<!doctype html><html><head>
<script src="/a.js"></script><script src="/b.js"></script>
<script id="__NEXT_DATA__" type="application/json">{"props":{"article":"..."}}</script>
</head><body><div id="__next"></div></body></html>`

	got := fetch.ClassifyBody(nextData, 10)
	if got != fetch.OutcomeStructuredOnly {
		t.Errorf("outcome = %q, want structured_only", got)
	}
	if got.CapabilityGap() {
		t.Error("structured_only must not count as a capability gap")
	}
	if !got.Usable() {
		t.Error("structured_only should be usable once a parser exists")
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   fetch.Outcome
	}{
		{404, "", fetch.OutcomeNotFound},
		{410, "", fetch.OutcomeNotFound},
		{401, "", fetch.OutcomePaywall},
		{402, "", fetch.OutcomePaywall},
		{429, "", fetch.OutcomeBotBlock},
		{403, "", fetch.OutcomeBotBlock},
		// A 403 is ambiguous; the body decides. Getting this wrong would file
		// paywalls under the bucket a browser is supposed to fix.
		{403, "Subscribe to continue", fetch.OutcomePaywall},
		{500, "", fetch.OutcomeServerError},
		{503, "", fetch.OutcomeServerError},
		{504, "", fetch.OutcomeTimeout},
	}
	for _, c := range cases {
		if got := fetch.ClassifyStatus(c.status, c.body); got != c.want {
			t.Errorf("ClassifyStatus(%d, %q) = %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

// TestOnlyJSRequiredIsACapabilityGap pins the classification that §17.1's
// decision procedure reads. If this table ever changes, the gate changes with
// it, and that should be a deliberate edit rather than a side effect.
func TestOnlyJSRequiredIsACapabilityGap(t *testing.T) {
	all := []fetch.Outcome{
		fetch.OutcomeOK, fetch.OutcomeJSRequired, fetch.OutcomeStructuredOnly,
		fetch.OutcomeConsentWall, fetch.OutcomeBotBlock, fetch.OutcomePaywall,
		fetch.OutcomeNotFound, fetch.OutcomeTimeout, fetch.OutcomeRobotsDenied,
		fetch.OutcomeGuardDenied, fetch.OutcomeExtractFailed, fetch.OutcomeServerError,
		fetch.OutcomeTooLarge, fetch.OutcomeUnsupported, fetch.OutcomeNetworkError,
	}
	for _, o := range all {
		want := o == fetch.OutcomeJSRequired
		if got := o.CapabilityGap(); got != want {
			t.Errorf("%q.CapabilityGap() = %v, want %v", o, got, want)
		}
	}

	// The two outcomes that mean Mole refused on purpose.
	if !fetch.OutcomeRobotsDenied.SystemWorking() || !fetch.OutcomeGuardDenied.SystemWorking() {
		t.Error("robots_denied and guard_denied must report SystemWorking")
	}
	if fetch.OutcomeNotFound.SystemWorking() {
		t.Error("not_found is a failure, not the system working")
	}
}
