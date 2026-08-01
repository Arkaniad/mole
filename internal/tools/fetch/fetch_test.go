package fetch_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/fetch"
	"github.com/lajosdeme/mole/internal/tools/limiter"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testGuard permits exactly what a local httptest server needs and nothing
// more.
//
// Both concessions are deliberately narrow, and the fact that they are needed
// at all is the evidence that the production defaults are restrictive:
// httptest binds 127.0.0.1 (blocked as loopback) on a random high port
// (blocked by the 80/443 allowlist). The port is added per-server rather than
// wholesale, so a test cannot accidentally reach anything else.
func testGuard(t *testing.T, serverURL string) *fetch.Guard {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("server port: %v", err)
	}
	return &fetch.Guard{
		AllowPrivateNetworks: true,
		AllowedPorts:         []int{80, 443, port},
	}
}

// newFetcher builds a fetcher pointed at a test server.
func newFetcher(t *testing.T, serverURL string, cfg fetch.Config) *fetch.HTTPFetcher {
	t.Helper()
	if cfg.PerDomain == (limiter.Limit{}) {
		cfg.PerDomain = limiter.Limit{Rate: 1000, Burst: 1000} // don't slow tests
	}
	return fetch.NewHTTP(cfg, fetch.Options{
		Guard: testGuard(t, serverURL),
		Log:   quietLog(),
	})
}

func TestFetchHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "Mole") {
			t.Errorf("User-Agent = %q, want it to identify Mole", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><p>hello</p></body></html>"))
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{})
	res, err := f.Fetch(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Outcome != fetch.OutcomeOK {
		t.Errorf("outcome = %q, want ok", res.Outcome)
	}
	if !strings.Contains(string(res.Content), "hello") {
		t.Errorf("content = %q", res.Content)
	}
	if res.StatusCode != 200 {
		t.Errorf("status = %d", res.StatusCode)
	}
	if res.Duration <= 0 {
		t.Error("duration not recorded")
	}
}

func TestFetchRespectsRobots(t *testing.T) {
	var pageHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private\n"))
			return
		}
		pageHits.Add(1)
		_, _ = w.Write([]byte("<html>secret</html>"))
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{})

	res, err := f.Fetch(context.Background(), srv.URL+"/private/doc")
	if err == nil {
		t.Fatal("disallowed path was fetched")
	}
	if res.Outcome != fetch.OutcomeRobotsDenied {
		t.Errorf("outcome = %q, want robots_denied", res.Outcome)
	}
	// The refusal must happen before the request, not after.
	if n := pageHits.Load(); n != 0 {
		t.Errorf("server was hit %d times for a disallowed path", n)
	}

	// An allowed path on the same host still works, and reuses the cached
	// robots.txt.
	if _, err := f.Fetch(context.Background(), srv.URL+"/public/doc"); err != nil {
		t.Fatalf("allowed path: %v", err)
	}
	if n := pageHits.Load(); n != 1 {
		t.Errorf("page hits = %d, want 1", n)
	}
}

func TestIgnoreRobotsIsAnExplicitOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
			return
		}
		_, _ = w.Write([]byte("<html>body</html>"))
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{IgnoreRobots: true})
	res, err := f.Fetch(context.Background(), srv.URL+"/anything")
	if err != nil {
		t.Fatalf("fetch with IgnoreRobots: %v", err)
	}
	if res.Outcome != fetch.OutcomeOK {
		t.Errorf("outcome = %q, want ok", res.Outcome)
	}
}

// TestRedirectToPrivateAddressIsBlocked is the standard SSRF bypass: a public
// URL that 302s inward. The initial CheckURL passes, so only the per-hop check
// catches it.
func TestRedirectToPrivateAddressIsBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	// Note: private networks are allowed here (httptest needs it), and the
	// redirect is STILL blocked — link-local is refused unconditionally.
	f := newFetcher(t, srv.URL, fetch.Config{})
	res, err := f.Fetch(context.Background(), srv.URL+"/redirect")
	if err == nil {
		t.Fatal("redirect to the metadata endpoint succeeded")
	}
	if res.Outcome != fetch.OutcomeGuardDenied {
		t.Errorf("outcome = %q, want guard_denied", res.Outcome)
	}
}

func TestRedirectLoopIsBounded(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{MaxRedirects: 3})
	if _, err := f.Fetch(context.Background(), srv.URL+"/loop"); err == nil {
		t.Fatal("infinite redirect loop was followed")
	}
}

func TestDenylistedDomainIsRefusedBeforeDNS(t *testing.T) {
	f := fetch.NewHTTP(fetch.Config{}, fetch.Options{
		Guard: &fetch.Guard{},
		Deny:  fetch.NewDomainDeny([]string{"linkedin.com"}),
		Log:   quietLog(),
	})

	res, err := f.Fetch(context.Background(), "https://www.linkedin.com/in/someone")
	if err == nil {
		t.Fatal("denylisted domain was fetched")
	}
	if res.Outcome != fetch.OutcomeGuardDenied {
		t.Errorf("outcome = %q, want guard_denied", res.Outcome)
	}
	if res.Domain != "linkedin.com" {
		t.Errorf("domain = %q, want linkedin.com", res.Domain)
	}
}

func TestSizeCapTruncatesAndReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(strings.Repeat("x", 100_000)))
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{MaxBytes: 1024})
	res, err := f.Fetch(context.Background(), srv.URL+"/big")
	if err == nil {
		t.Fatal("oversized body was accepted")
	}
	if res.Outcome != fetch.OutcomeTooLarge {
		t.Errorf("outcome = %q, want too_large", res.Outcome)
	}
}

func TestStatusCodesAreClassified(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   fetch.Outcome
	}{
		{404, "", fetch.OutcomeNotFound},
		{403, "", fetch.OutcomeBotBlock},
		{429, "", fetch.OutcomeBotBlock},
		{500, "", fetch.OutcomeServerError},
		{401, "", fetch.OutcomePaywall},
	}

	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					http.NotFound(w, r)
					return
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()

			f := newFetcher(t, srv.URL, fetch.Config{})
			res, err := f.Fetch(context.Background(), srv.URL+"/x")
			if err == nil {
				t.Fatalf("status %d returned no error", c.status)
			}
			if res.Outcome != c.want {
				t.Errorf("outcome = %q, want %q", res.Outcome, c.want)
			}
			if res.StatusCode != c.status {
				t.Errorf("status = %d, want %d", res.StatusCode, c.status)
			}
		})
	}
}

func TestUnsupportedContentTypeIsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n"))
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{})
	res, _ := f.Fetch(context.Background(), srv.URL+"/img")
	if res.Outcome != fetch.OutcomeUnsupported {
		t.Errorf("outcome = %q, want unsupported_type", res.Outcome)
	}
}

// TestRefineUpgradesToBodyClassification: the fetcher reports transport-level
// success, and the body-level verdict is only knowable after extraction runs.
func TestRefineUpgradesToBodyClassification(t *testing.T) {
	res := &fetch.Result{Outcome: fetch.OutcomeOK, Content: []byte(spaBody)}
	res.Refine(8)
	if res.Outcome != fetch.OutcomeJSRequired {
		t.Errorf("outcome = %q, want js_required", res.Outcome)
	}

	// Refine must not overwrite a decided failure.
	denied := &fetch.Result{Outcome: fetch.OutcomeRobotsDenied}
	denied.Refine(0)
	if denied.Outcome != fetch.OutcomeRobotsDenied {
		t.Errorf("Refine clobbered %q", denied.Outcome)
	}
}

func TestCrawlDelayIsHonoured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nCrawl-delay: 1\n"))
			return
		}
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	defer srv.Close()

	f := fetch.NewHTTP(fetch.Config{PerDomain: limiter.Limit{Rate: 1000, Burst: 1000}}, fetch.Options{
		Guard: testGuard(t, srv.URL),
		Log:   quietLog(),
	})

	ctx := context.Background()
	if _, err := f.Fetch(ctx, srv.URL+"/a"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	// The bucket is fast, but the site asked for a second between requests.
	// Without the MinInterval floor a burst-capable bucket would ignore it.
	start := time.Now()
	if _, err := f.Fetch(ctx, srv.URL+"/b"); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Errorf("second fetch waited %v, want >= ~1s from Crawl-delay", elapsed)
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	f := newFetcher(t, srv.URL, fetch.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	res, err := f.Fetch(ctx, srv.URL+"/slow")
	if err == nil {
		t.Fatal("slow request was not cancelled")
	}
	if res.Outcome != fetch.OutcomeTimeout {
		t.Errorf("outcome = %q, want timeout", res.Outcome)
	}
}

func TestNonHTTPSchemesRejected(t *testing.T) {
	f := fetch.NewHTTP(fetch.Config{}, fetch.Options{Guard: &fetch.Guard{}, Log: quietLog()})
	for _, raw := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://example.com/"} {
		res, err := f.Fetch(context.Background(), raw)
		if err == nil {
			t.Errorf("%s was fetched", raw)
			continue
		}
		if res.Outcome != fetch.OutcomeGuardDenied {
			t.Errorf("%s outcome = %q, want guard_denied", raw, res.Outcome)
		}
	}
}
