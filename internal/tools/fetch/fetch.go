// Package fetch retrieves web content under egress, politeness, and rate
// controls, and classifies every attempt.
//
// The guards are not bolted on around a fetcher — they are the fetcher. §3.3
// and §3.4 require them present from the first commit precisely because
// retrofitting them means auditing every call site that has since appeared.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// Fetcher retrieves a URL.
//
// It returns a non-nil *Result even on failure, because the outcome of a failed
// fetch is data the project needs (§10.4) — a fetcher that returned only an
// error would throw away the input to the headless-browser decision.
type Fetcher interface {
	Fetch(ctx context.Context, rawURL string) (*Result, error)
}

// Config tunes an HTTPFetcher.
type Config struct {
	// UserAgent identifies Mole. Required: an anonymous crawler is impolite and
	// gives site operators no way to contact anyone or block selectively.
	UserAgent string

	MaxBytes     int64
	Timeout      time.Duration
	MaxRedirects int

	// IgnoreRobots disables robots.txt. Off by default; every use is logged at
	// WARN so it cannot happen quietly.
	IgnoreRobots bool

	// RobotsTTL is how long a robots.txt is cached.
	RobotsTTL time.Duration

	// PerDomain is the default rate limit applied to each host.
	PerDomain limiter.Limit
}

func (c Config) withDefaults() Config {
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 8 << 20 // 8 MiB
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxRedirects <= 0 {
		c.MaxRedirects = 5
	}
	if c.RobotsTTL <= 0 {
		c.RobotsTTL = time.Hour
	}
	if c.PerDomain.Rate <= 0 && c.PerDomain.MinInterval <= 0 {
		// One request per second per host, burst 2. Deliberately conservative:
		// the cost of being slightly slow is latency, the cost of being fast is
		// a ban.
		c.PerDomain = limiter.Limit{Rate: 1, Burst: 2}
	}
	return c
}

// DefaultUserAgent identifies the crawler and points at the project.
const DefaultUserAgent = "Mole/0.1 (+https://github.com/lajosdeme/mole)"

// HTTPFetcher is the only web-fetch mechanism. There is no per-site adapter,
// and whether a headless browser is ever added is gated on §17.1.
type HTTPFetcher struct {
	cfg    Config
	guard  *Guard
	deny   *DomainDeny
	robots *RobotsCache
	lim    *limiter.Limiter
	client *http.Client
	log    *slog.Logger
}

// Options are the collaborators an HTTPFetcher needs.
type Options struct {
	Guard *Guard
	Deny  *DomainDeny
	Lim   *limiter.Limiter
	Log   *slog.Logger

	// Transport lets the record/replay cassette layer wrap the guarded
	// transport, so tests are deterministic and cost nothing.
	Transport func(base http.RoundTripper) http.RoundTripper
}

// NewHTTP builds a fetcher. A nil Guard gets the default (deny private
// networks), which is the safe direction for a missing argument.
func NewHTTP(cfg Config, opts Options) *HTTPFetcher {
	cfg = cfg.withDefaults()

	g := opts.Guard
	if g == nil {
		g = &Guard{}
	}
	deny := opts.Deny
	if deny == nil {
		deny = NewDomainDeny(DefaultDenyDomains())
	}
	lim := opts.Lim
	if lim == nil {
		lim = limiter.New(cfg.PerDomain)
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	f := &HTTPFetcher{cfg: cfg, guard: g, deny: deny, lim: lim, log: log}

	var rt http.RoundTripper = &http.Transport{
		DialContext:           g.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		MaxIdleConnsPerHost:   2,
		ForceAttemptHTTP2:     true,
	}
	if opts.Transport != nil {
		rt = opts.Transport(rt)
	}

	f.client = &http.Client{
		Transport: rt,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= cfg.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", cfg.MaxRedirects)
			}
			// Re-check every hop. A public URL that 302s to a private address
			// is the standard bypass, and the dialer alone would allow the
			// request to be *made* before the address check fails.
			if err := g.CheckURL(req.URL); err != nil {
				return err
			}
			if deny.Denied(req.URL.Hostname()) {
				return &GuardError{URL: req.URL.String(), Host: req.URL.Hostname(), Reason: "domain on denylist"}
			}
			return nil
		},
	}

	f.robots = NewRobotsCache(cfg.RobotsTTL, f.fetchRobots)
	if cfg.IgnoreRobots {
		log.Warn("robots.txt checking is DISABLED for this process")
	}
	return f
}

// Limiter exposes the rate limiter so callers can tighten a specific host.
func (f *HTTPFetcher) Limiter() *limiter.Limiter { return f.lim }

// Fetch retrieves rawURL.
func (f *HTTPFetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	start := time.Now()

	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return &Result{URL: rawURL, Outcome: OutcomeGuardDenied, Err: "unparseable url"},
			fmt.Errorf("fetch: parse %q: %w", rawURL, err)
	}
	host := u.Hostname()
	res := &Result{URL: rawURL, Domain: registrableish(host)}

	fail := func(o Outcome, err error) (*Result, error) {
		res.Outcome = o
		res.Duration = time.Since(start)
		if err != nil {
			res.Err = err.Error()
		}
		return res, err
	}

	// 1. Domain denylist, before any DNS traffic.
	if f.deny.Denied(host) {
		return fail(OutcomeGuardDenied, &GuardError{URL: rawURL, Host: host, Reason: "domain on denylist"})
	}

	// 2. Egress guard on scheme, port, literal IP.
	if err := f.guard.CheckURL(u); err != nil {
		return fail(OutcomeGuardDenied, err)
	}

	// 3. robots.txt.
	var crawlDelay time.Duration
	if !f.cfg.IgnoreRobots {
		rules := f.robots.Get(ctx, u.Scheme+"://"+u.Host)
		if !rules.Allowed(f.cfg.UserAgent, u.EscapedPath()) {
			return fail(OutcomeRobotsDenied, fmt.Errorf("fetch: robots.txt disallows %s", u.EscapedPath()))
		}
		crawlDelay = rules.CrawlDelay(f.cfg.UserAgent)
	}

	// 4. Rate limit per host, honouring Crawl-delay when the site asks for one
	// slower than our default.
	key := strings.ToLower(host)
	if crawlDelay > 0 {
		cur := f.lim.LimitFor(key)
		if crawlDelay > cur.MinInterval {
			f.lim.Set(key, cur.WithDelay(crawlDelay))
		}
	}
	if err := f.lim.Wait(ctx, key); err != nil {
		return fail(OutcomeTimeout, err)
	}

	// 5. Request.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fail(OutcomeNetworkError, err)
	}
	req.Header.Set("User-Agent", f.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en")

	resp, err := f.client.Do(req)
	if err != nil {
		return fail(classifyTransportError(err), err)
	}
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode
	res.ContentType = resp.Header.Get("Content-Type")
	if resp.Request != nil && resp.Request.URL != nil {
		res.FinalURL = resp.Request.URL.String()
	}

	// 6. Read with a hard size cap. Reading MaxBytes+1 is how we distinguish
	// "exactly at the limit" from "truncated".
	body, truncated, err := readCapped(resp.Body, f.cfg.MaxBytes)
	res.Bytes = int64(len(body))
	if err != nil {
		return fail(classifyTransportError(err), err)
	}
	if truncated {
		return fail(OutcomeTooLarge, fmt.Errorf("fetch: body exceeds %d bytes", f.cfg.MaxBytes))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		o := ClassifyStatus(resp.StatusCode, string(body))
		return fail(o, fmt.Errorf("fetch: %s returned %d", u.Host, resp.StatusCode))
	}

	if !supportedContentType(res.ContentType) {
		return fail(OutcomeUnsupported, fmt.Errorf("fetch: unsupported content type %q", res.ContentType))
	}

	res.Content = body
	res.Outcome = OutcomeOK
	res.Duration = time.Since(start)
	return res, nil
}

// Refine upgrades a transport-level OK to a body-level classification once the
// extractor has run. Splitting it this way keeps the fetcher out of the
// business of deciding what counts as usable text.
func (r *Result) Refine(extractedLen int) {
	if r.Outcome != OutcomeOK {
		return
	}
	r.Outcome = ClassifyBody(string(r.Content), extractedLen)
}

// fetchRobots retrieves robots.txt through the same guarded client, skipping
// the robots check itself (which would be circular) and the rate limiter (a
// single small request per host per TTL).
func (f *HTTPFetcher) fetchRobots(ctx context.Context, robotsURL string) (int, []byte, error) {
	u, err := url.Parse(robotsURL)
	if err != nil {
		return 0, nil, err
	}
	if err := f.guard.CheckURL(u); err != nil {
		return 0, nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", f.cfg.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, _, err := readCapped(resp.Body, 512<<10)
	return resp.StatusCode, body, err
}

// ---------------------------------------------------------------------------

func readCapped(r io.Reader, max int64) (body []byte, truncated bool, err error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return b, false, err
	}
	if int64(len(b)) > max {
		return b[:max], true, nil
	}
	return b, false, nil
}

func supportedContentType(ct string) bool {
	if ct == "" {
		// Servers omit it; assume HTML rather than discarding the response.
		return true
	}
	mediaType, _, _ := strings.Cut(ct, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/html", "application/xhtml+xml", "text/plain", "application/xml", "text/xml":
		return true
	}
	return false
}

func classifyTransportError(err error) Outcome {
	var ge *GuardError
	if errors.As(err, &ge) {
		return OutcomeGuardDenied
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return OutcomeTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return OutcomeTimeout
	}
	// A redirect chain that trips the guard surfaces as a *url.Error wrapping
	// the GuardError; errors.As above catches it, so anything left is genuine
	// network trouble.
	return OutcomeNetworkError
}

// registrableish reduces a host to something stable enough to group by in the
// per-cause domain ranking (§10.4).
//
// It is NOT a public-suffix implementation: "bbc.co.uk" reduces to "co.uk".
// That is acceptable for ranking which domains cause which failures, and a
// real PSL is a dependency this does not yet justify. Revisit if the M2 report
// turns out to be misleading because of it.
func registrableish(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
