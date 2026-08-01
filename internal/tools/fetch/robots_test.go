package fetch_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/fetch"
)

const ua = "Mole/0.1 (+https://github.com/lajosdeme/mole)"

func TestRobotsBasicAllowDisallow(t *testing.T) {
	r := fetch.ParseRobots(strings.NewReader(`
User-agent: *
Disallow: /private/
Disallow: /tmp
Allow: /private/public.html
`))

	cases := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/index.html", true},
		{"/private/", false},
		{"/private/secret.html", false},
		// Longest match wins, so the Allow beats the shorter Disallow.
		{"/private/public.html", true},
		{"/tmp", false},
		{"/tmpfile", false}, // unanchored patterns are prefix matches
	}
	for _, c := range cases {
		if got := r.Allowed(ua, c.path); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestRobotsWildcardsAndAnchors(t *testing.T) {
	r := fetch.ParseRobots(strings.NewReader(`
User-agent: *
Disallow: /*.pdf$
Disallow: /search
Allow: /search/help
Disallow: /a/*/private
`))

	cases := []struct {
		path string
		want bool
	}{
		{"/doc.pdf", false},
		{"/deep/path/doc.pdf", false},
		// '$' anchors: a query string means it no longer ends in .pdf.
		{"/doc.pdf?download=1", true},
		{"/search", false},
		{"/search?q=x", false},
		{"/search/help", true},
		{"/a/b/private", false},
		{"/a/b/c/private", false},
		{"/a/private", true}, // the middle segment is required
	}
	for _, c := range cases {
		if got := r.Allowed(ua, c.path); got != c.want {
			t.Errorf("Allowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestRobotsGroupSelection: a directive aimed at our agent must win over the
// wildcard group, and only the winning group's rules apply.
func TestRobotsGroupSelection(t *testing.T) {
	r := fetch.ParseRobots(strings.NewReader(`
User-agent: *
Disallow: /

User-agent: Mole
Allow: /
Disallow: /admin

User-agent: Googlebot
Disallow: /nothing-to-do-with-us
`))

	if !r.Allowed(ua, "/anything") {
		t.Error("Mole-specific group did not take precedence over *")
	}
	if r.Allowed(ua, "/admin") {
		t.Error("/admin should be disallowed for Mole")
	}
	// An unrelated crawler falls back to the wildcard group.
	if r.Allowed("SomeOtherBot/1.0", "/anything") {
		t.Error("unrelated agent should fall back to the * group's Disallow: /")
	}
}

func TestRobotsSharedGroupAndCrawlDelay(t *testing.T) {
	r := fetch.ParseRobots(strings.NewReader(`
User-agent: alpha
User-agent: mole
Crawl-delay: 2.5
Disallow: /x
`))

	if got := r.CrawlDelay(ua); got != 2500*time.Millisecond {
		t.Errorf("CrawlDelay = %v, want 2.5s", got)
	}
	if r.Allowed(ua, "/x") {
		t.Error("shared group's Disallow did not apply to the second agent")
	}
}

func TestRobotsEmptyDisallowMeansAllowAll(t *testing.T) {
	r := fetch.ParseRobots(strings.NewReader("User-agent: *\nDisallow:\n"))
	if !r.Allowed(ua, "/anything") {
		t.Error("empty Disallow should permit everything")
	}
}

func TestRobotsCommentsAndJunkAreIgnored(t *testing.T) {
	r := fetch.ParseRobots(strings.NewReader(`
# a comment
Sitemap: https://example.com/sitemap.xml
this line has no colon
User-agent: *   # trailing comment
Disallow: /nope   # another
`))
	if r.Allowed(ua, "/nope") {
		t.Error("Disallow with a trailing comment was not applied")
	}
	if !r.Allowed(ua, "/fine") {
		t.Error("unrelated path blocked")
	}
}

// TestRobotsFetchFailurePolicy pins the conservative reading: a missing
// robots.txt is permission, an unreachable one is not.
func TestRobotsFetchFailurePolicy(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		err     error
		allowed bool
	}{
		{"404 means no restrictions", 404, nil, true},
		{"401 means no restrictions", 401, nil, true},
		{"500 means disallow all", 500, nil, false},
		{"503 means disallow all", 503, nil, false},
		{"network error means disallow all", 0, errors.New("boom"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cache := fetch.NewRobotsCache(time.Hour,
				func(ctx context.Context, u string) (int, []byte, error) {
					return c.status, nil, c.err
				})
			rules := cache.Get(context.Background(), "https://example.com")
			if got := rules.Allowed(ua, "/some/path"); got != c.allowed {
				t.Errorf("Allowed = %v, want %v", got, c.allowed)
			}
		})
	}
}

// TestRobotsCacheFetchesOncePerOrigin: a worker pool starting on a new host
// must not stampede robots.txt.
func TestRobotsCacheFetchesOncePerOrigin(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	cache := fetch.NewRobotsCache(time.Hour,
		func(ctx context.Context, u string) (int, []byte, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return 200, []byte("User-agent: *\nDisallow: /x\n"), nil
		})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := cache.Get(context.Background(), "https://example.com")
			if r.Allowed(ua, "/x") {
				t.Error("cached rules not applied")
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("fetched robots.txt %d times, want 1", calls)
	}
}

func TestDomainDenyMatchesSubdomainsOnly(t *testing.T) {
	d := fetch.NewDomainDeny([]string{"linkedin.com", "x.com"})

	denied := []string{"linkedin.com", "www.linkedin.com", "de.linkedin.com", "x.com", "api.x.com"}
	for _, h := range denied {
		if !d.Denied(h) {
			t.Errorf("%s should be denied", h)
		}
	}

	// A domain that merely ends with the same letters is a different site.
	allowed := []string{"notlinkedin.com", "linkedin.com.evil.net", "example.com", "xx.com"}
	for _, h := range allowed {
		if d.Denied(h) {
			t.Errorf("%s should NOT be denied", h)
		}
	}
}
