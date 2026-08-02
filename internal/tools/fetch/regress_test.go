package fetch_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/fetch"
)

// TestEscapeHatchDoesNotReachMetadataByAnotherName: AllowPrivateNetworks means
// "let me reach my intranet". NAT64, 6to4 and CGNAT are alternative spellings
// of the addresses CheckAddr refuses unconditionally, so letting the hatch
// cover them made the promise in its own doc comment false.
func TestEscapeHatchDoesNotReachMetadataByAnotherName(t *testing.T) {
	loose := &fetch.Guard{AllowPrivateNetworks: true}

	for _, tc := range []struct{ addr, why string }{
		{"2002:a9fe:a9fe::", "6to4 wrapping 169.254.169.254"},
		{"64:ff9b::a9fe:a9fe", "NAT64 wrapping 169.254.169.254"},
		{"64:ff9b::a00:1", "NAT64 wrapping 10.0.0.1"},
		{"100.64.0.1", "CGNAT"},
		{"192.0.0.1", "IETF protocol assignments"},
		{"169.254.169.254", "cloud metadata, direct"},
	} {
		if reason := loose.CheckAddr(netip.MustParseAddr(tc.addr)); reason == "" {
			t.Errorf("%s (%s) allowed with AllowPrivateNetworks set", tc.addr, tc.why)
		}
	}
}

// TestEscapeHatchStillReachesTheIntranet: the fix above must not take away the
// case the hatch exists for, or every test using an httptest server breaks.
func TestEscapeHatchStillReachesTheIntranet(t *testing.T) {
	loose := &fetch.Guard{AllowPrivateNetworks: true}
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "192.168.0.7", "172.16.0.1"} {
		if reason := loose.CheckAddr(netip.MustParseAddr(a)); reason != "" {
			t.Errorf("%s refused with AllowPrivateNetworks set: %s", a, reason)
		}
	}
}

// TestOpenGraphAloneIsNotStructuredData is the §17.1 metric guard. Nearly every
// SPA emits og:title, so counting it filed them all as structured_only and held
// js_required at approximately zero — a number the headless-browser decision
// reads and could never see rise.
func TestOpenGraphAloneIsNotStructuredData(t *testing.T) {
	spa := `<!doctype html><html><head>
<meta property="og:title" content="Dashboard">
<meta property="og:description" content="A short blurb.">
</head><body><div id="root"></div>
<script src="/a.js"></script><script src="/b.js"></script></body></html>`

	if fetch.HasStructuredData(spa) {
		t.Error("og: tags alone counted as structured data")
	}
	if got := fetch.ClassifyBody(spa, 0); got != fetch.OutcomeJSRequired {
		t.Errorf("SPA classified as %s, want js_required", got)
	}
}

func TestRealStructuredDataStillCounts(t *testing.T) {
	for name, page := range map[string]string{
		"json-ld":       `<html><head><script type="application/ld+json">{"@type":"Article"}</script></head><body><div id="app"></div><script></script><script></script></body></html>`,
		"__NEXT_DATA__": `<html><body><div id="__next"></div><script id="__NEXT_DATA__" type="application/json">{}</script><script></script></body></html>`,
	} {
		if !fetch.HasStructuredData(page) {
			t.Errorf("%s: not recognized as structured data", name)
		}
		if got := fetch.ClassifyBody(page, 0); got != fetch.OutcomeStructuredOnly {
			t.Errorf("%s: classified as %s, want structured_only", name, got)
		}
	}
}

// TestNonSuccessStatusIsNotOK: a 3xx that was never followed carries no
// document, and reporting ok entered it into the denominator as a success.
func TestNonSuccessStatusIsNotOK(t *testing.T) {
	for _, status := range []int{100, 204, 301, 302, 304, 307} {
		got := fetch.ClassifyStatus(status, "")
		switch status {
		case 204:
			// 2xx: genuinely a successful response, however empty.
			if got != fetch.OutcomeOK {
				t.Errorf("status %d -> %s, want ok", status, got)
			}
		default:
			if got == fetch.OutcomeOK {
				t.Errorf("status %d reported as ok", status)
			}
		}
	}
}

// TestProviderContentIsNotAFetchAttempt keeps the §10.4 denominator honest.
func TestProviderContentIsNotAFetchAttempt(t *testing.T) {
	if fetch.OutcomeProviderContent.Attempted() {
		t.Error("provider_content counted as an attempted fetch")
	}
	if !fetch.OutcomeOK.Attempted() || !fetch.OutcomeBotBlock.Attempted() {
		t.Error("a real fetch was not counted as attempted")
	}
	if !fetch.OutcomeProviderContent.Usable() {
		t.Error("provider_content is not usable, but it is exactly the usable case")
	}
	if fetch.OutcomeProviderContent.CapabilityGap() {
		t.Error("provider_content counted as evidence for a headless browser")
	}
}

// TestCrawlDelayIsCapped: Crawl-delay is a number a stranger writes in a text
// file. Honouring an hour parks a worker until the lead's wall clock kills it,
// having read nothing.
func TestCrawlDelayIsCapped(t *testing.T) {
	rules := fetch.ParseRobots(strings.NewReader("User-agent: *\nCrawl-delay: 3600\nDisallow: /private\n"))
	if got := rules.CrawlDelay("molebot"); got != fetch.MaxCrawlDelay {
		t.Errorf("crawl delay = %v, want it capped at %v", got, fetch.MaxCrawlDelay)
	}

	rules = fetch.ParseRobots(strings.NewReader("User-agent: *\nCrawl-delay: 2\nDisallow: /private\n"))
	if got := rules.CrawlDelay("molebot"); got != 2*time.Second {
		t.Errorf("reasonable crawl delay = %v, want 2s", got)
	}
}

// TestTransientRobotsFailureIsNotCached: denying the request that failed is
// correct. Denying the whole origin for the cache TTL because one context was
// cancelled is not — it converts a blip into an hour-long outage for that host.
func TestTransientRobotsFailureIsNotCached(t *testing.T) {
	var calls int
	fail := true
	c := fetch.NewRobotsCache(time.Hour, func(ctx context.Context, url string) (int, []byte, error) {
		calls++
		if fail {
			return 0, nil, context.Canceled
		}
		return 200, []byte("User-agent: *\nAllow: /\n"), nil
	})

	if c.Get(context.Background(), "https://example.com").Allowed("molebot", "/a") {
		t.Error("a failed robots fetch was treated as consent")
	}

	fail = false
	if !c.Get(context.Background(), "https://example.com").Allowed("molebot", "/a") {
		t.Error("the origin stayed denied after robots.txt became reachable")
	}
	if calls != 2 {
		t.Errorf("robots fetched %d times, want 2 (the failure must not be cached)", calls)
	}
}

// TestSuccessfulRobotsIsStillCached: the fix above must not turn the cache off.
func TestSuccessfulRobotsIsStillCached(t *testing.T) {
	var calls int
	c := fetch.NewRobotsCache(time.Hour, func(ctx context.Context, url string) (int, []byte, error) {
		calls++
		return 200, []byte("User-agent: *\nDisallow: /private\n"), nil
	})

	for i := 0; i < 5; i++ {
		c.Get(context.Background(), "https://example.com")
	}
	if calls != 1 {
		t.Errorf("robots fetched %d times, want 1", calls)
	}
}

func TestDomainOfMatchesResultDomain(t *testing.T) {
	cases := map[string]string{
		"https://www.example.com/a/b": "example.com",
		"https://example.com":         "example.com",
		"http://a.b.c.example.co.uk/": "co.uk", // documented limitation, not a PSL
		"https://127.0.0.1:8080/x":    "127.0.0.1",
		"not a url":                   "",
	}
	for in, want := range cases {
		if got := fetch.DomainOf(in); got != want {
			t.Errorf("DomainOf(%q) = %q, want %q", in, got, want)
		}
	}
}
