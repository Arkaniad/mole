package fetch

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// robots.txt handling.
//
// Rev 1 dropped the site-specific scrapers on legal grounds but kept a fetcher
// that could reach the same sites. The posture only actually changes with
// mechanism, so this is on by default and the override logs loudly.

// MaxCrawlDelay bounds what a site's Crawl-delay can cost us.
const MaxCrawlDelay = 30 * time.Second

// Rules is a parsed robots.txt.
type Rules struct {
	groups    []group
	fetchedAt time.Time
	allowAll  bool
	denyAll   bool
}

type group struct {
	agents     []string
	rules      []rule
	crawlDelay time.Duration
}

type rule struct {
	pattern string
	allow   bool
}

// AllowAllRules is used when a site has no robots.txt (404), which the standard
// treats as unrestricted.
func AllowAllRules() *Rules { return &Rules{allowAll: true} }

// DenyAllRules is used when robots.txt could not be read due to a server error.
// The standard treats a 5xx as a full disallow; erring the other way would mean
// a flaky server silently converts into permission we were never given.
func DenyAllRules() *Rules { return &Rules{denyAll: true} }

// ParseRobots reads a robots.txt body.
func ParseRobots(r io.Reader) *Rules {
	rules := &Rules{fetchedAt: time.Now()}

	var (
		cur       *group
		lastWasUA bool
	)

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		field, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		field = strings.ToLower(strings.TrimSpace(field))
		value = strings.TrimSpace(value)

		switch field {
		case "user-agent":
			if value == "" {
				continue
			}
			// Consecutive User-agent lines share one group; a User-agent line
			// after a rule line starts a new one.
			if cur == nil || !lastWasUA {
				rules.groups = append(rules.groups, group{})
				cur = &rules.groups[len(rules.groups)-1]
			}
			cur.agents = append(cur.agents, strings.ToLower(value))
			lastWasUA = true

		case "disallow", "allow":
			if cur == nil {
				// Rules before any User-agent line are not addressed to anyone.
				continue
			}
			lastWasUA = false
			// "Disallow:" with an empty value means allow everything.
			if field == "disallow" && value == "" {
				continue
			}
			cur.rules = append(cur.rules, rule{pattern: value, allow: field == "allow"})

		case "crawl-delay":
			if cur == nil {
				continue
			}
			lastWasUA = false
			if f, err := strconv.ParseFloat(value, 64); err == nil && f > 0 {
				d := time.Duration(f * float64(time.Second))
				// Cap it. Crawl-delay is a number a stranger puts in a text
				// file, and values of hours exist in the wild; honouring one
				// literally parks a worker until the lead's wall-clock ceiling
				// kills it, having read nothing. Past the cap the honest
				// reading is "do not crawl this", and the fetch failing on
				// deadline says that more usefully than a silent stall.
				if d > MaxCrawlDelay {
					d = MaxCrawlDelay
				}
				cur.crawlDelay = d
			}
		}
	}
	return rules
}

// groupFor picks the most specific matching group.
//
// The standard matches a group when its User-agent token is a case-insensitive
// substring of the crawler's name, and the longest matching token wins. "*"
// is the fallback and only applies when nothing else matched.
func (r *Rules) groupFor(userAgent string) *group {
	ua := strings.ToLower(userAgent)

	var (
		best    *group
		bestLen = -1
		wild    *group
	)
	for i := range r.groups {
		g := &r.groups[i]
		for _, a := range g.agents {
			if a == "*" {
				if wild == nil {
					wild = g
				}
				continue
			}
			if strings.Contains(ua, a) && len(a) > bestLen {
				best, bestLen = g, len(a)
			}
		}
	}
	if best != nil {
		return best
	}
	return wild
}

// Allowed reports whether userAgent may fetch path.
//
// Precedence follows the standard: the longest matching pattern wins, and on a
// tie Allow beats Disallow. Getting this backwards would silently over-block
// (harmless but useless) or under-block (the thing we are trying not to do).
func (r *Rules) Allowed(userAgent, path string) bool {
	if r == nil || r.allowAll {
		return true
	}
	if r.denyAll {
		return false
	}
	g := r.groupFor(userAgent)
	if g == nil {
		return true
	}
	if path == "" {
		path = "/"
	}

	var (
		bestLen   = -1
		bestAllow = true
	)
	for _, rl := range g.rules {
		if !pathMatch(rl.pattern, path) {
			continue
		}
		l := len(rl.pattern)
		switch {
		case l > bestLen:
			bestLen, bestAllow = l, rl.allow
		case l == bestLen && rl.allow:
			bestAllow = true
		}
	}
	if bestLen < 0 {
		return true
	}
	return bestAllow
}

// CrawlDelay reports the group's Crawl-delay, or 0.
func (r *Rules) CrawlDelay(userAgent string) time.Duration {
	if r == nil || r.allowAll || r.denyAll {
		return 0
	}
	if g := r.groupFor(userAgent); g != nil {
		return g.crawlDelay
	}
	return 0
}

// pathMatch implements robots path matching: '*' matches any sequence, a
// trailing '$' anchors the end, and an unanchored pattern is a prefix match.
func pathMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	if pattern == "" {
		return !anchored || path == ""
	}

	parts := strings.Split(pattern, "*")

	// No wildcards: prefix match, or exact when anchored.
	if len(parts) == 1 {
		if anchored {
			return path == pattern
		}
		return strings.HasPrefix(path, pattern)
	}

	// First segment must be a prefix.
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	pos := len(parts[0])

	// Middle segments match greedily in order.
	for _, seg := range parts[1 : len(parts)-1] {
		if seg == "" {
			continue
		}
		i := strings.Index(path[pos:], seg)
		if i < 0 {
			return false
		}
		pos += i + len(seg)
	}

	last := parts[len(parts)-1]
	if last == "" {
		// Pattern ended in '*'. With '$' the rest must be empty.
		return !anchored || pos == len(path)
	}
	if anchored {
		return strings.HasSuffix(path, last) && len(path)-len(last) >= pos
	}
	return strings.Contains(path[pos:], last)
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// RobotsCache fetches and caches robots.txt per origin.
type RobotsCache struct {
	mu      sync.Mutex
	entries map[string]*robotsEntry
	ttl     time.Duration

	// Fetch retrieves a robots.txt body. Injected so the cache can be tested
	// without a network and so the real one reuses the guarded transport.
	Fetch func(ctx context.Context, robotsURL string) (status int, body []byte, err error)
}

type robotsEntry struct {
	rules   *Rules
	expires time.Time
	// once guards a single in-flight fetch per origin; a worker pool would
	// otherwise stampede robots.txt on the first request to a new host.
	once *sync.Once
}

func NewRobotsCache(ttl time.Duration, fetch func(context.Context, string) (int, []byte, error)) *RobotsCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &RobotsCache{entries: map[string]*robotsEntry{}, ttl: ttl, Fetch: fetch}
}

// Get returns the rules for an origin ("https://example.com").
//
// Concurrent callers for the same origin share one fetch. The entry must be
// created once and then mutated in place — replacing it on each cache miss
// hands every caller its own sync.Once, which is a stampede wearing the costume
// of a singleflight.
func (c *RobotsCache) Get(ctx context.Context, origin string) *Rules {
	now := time.Now()

	c.mu.Lock()
	e, ok := c.entries[origin]
	switch {
	case ok && e.rules != nil && now.Before(e.expires):
		// Fresh hit.
		rules := e.rules
		c.mu.Unlock()
		return rules
	case !ok, e.rules != nil:
		// Absent, or present but stale: start a new fetch.
		e = &robotsEntry{once: &sync.Once{}}
		c.entries[origin] = e
	default:
		// A fetch is already in flight; join it.
	}
	once, entry := e.once, e
	c.mu.Unlock()

	var transient bool
	once.Do(func() {
		rules, ok := c.load(ctx, origin)
		transient = !ok
		c.mu.Lock()
		entry.rules = rules
		if ok {
			entry.expires = time.Now().Add(c.ttl)
		}
		// A transient failure leaves expires at the zero time, so the entry is
		// already stale and the next caller refetches. Caching it for the full
		// TTL meant one cancelled context locked a whole origin out for an hour
		// — the deny is correct for the request that failed, not for the next
		// sixty minutes of them.
		c.mu.Unlock()
	})

	c.mu.Lock()
	rules := entry.rules
	if transient {
		// Drop the entry so a caller arriving after this one does not join a
		// resolved sync.Once holding a failure.
		if cur, ok := c.entries[origin]; ok && cur == entry {
			delete(c.entries, origin)
		}
	}
	c.mu.Unlock()
	if rules == nil {
		return AllowAllRules()
	}
	return rules
}

// load fetches and parses robots.txt. The bool reports whether the answer is
// durable enough to cache: a network failure or a cancelled context says
// nothing about the site's policy, only about this attempt.
func (c *RobotsCache) load(ctx context.Context, origin string) (*Rules, bool) {
	if c.Fetch == nil {
		return AllowAllRules(), true
	}
	status, body, err := c.Fetch(ctx, origin+"/robots.txt")
	switch {
	case err != nil:
		// Unreachable robots.txt is treated as a server error, not as consent.
		return DenyAllRules(), false
	case status == http.StatusOK:
		return ParseRobots(strings.NewReader(string(body))), true
	case status >= 400 && status < 500:
		// No robots.txt means no restrictions.
		return AllowAllRules(), true
	default:
		// 5xx is a full disallow per the standard, but it is also the classic
		// transient failure. Deny this request; do not hold the origin.
		return DenyAllRules(), false
	}
}

// ---------------------------------------------------------------------------
// Domain denylist
// ---------------------------------------------------------------------------

// DefaultDenyDomains ships with the binary. These are the sites whose terms
// clearly disallow automated access — the ones rev 1 removed first-party
// scrapers for. Removing the scraper without denying the domain would have left
// the exposure exactly where it was, just reached by a different code path.
func DefaultDenyDomains() []string {
	return []string{
		"linkedin.com",
		"facebook.com",
		"instagram.com",
		"x.com",
		"twitter.com",
	}
}

// DomainDeny matches hosts against a suffix list.
type DomainDeny struct {
	suffixes []string
}

func NewDomainDeny(domains []string) *DomainDeny {
	s := make([]string, 0, len(domains))
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "."))
		if d != "" {
			s = append(s, d)
		}
	}
	return &DomainDeny{suffixes: s}
}

// Denied reports whether host is on the list, matching the domain and any
// subdomain of it but not a domain that merely ends with the same letters
// ("notlinkedin.com" is not "linkedin.com").
func (d *DomainDeny) Denied(host string) bool {
	if d == nil {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range d.suffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	return false
}
