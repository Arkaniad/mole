package fetch

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Guard is the egress control on outbound fetches.
//
// The Fetcher is the daemon's network position, and it fetches URLs chosen by a
// search engine and by an LLM. Without this, "summarize https://…" is a request
// to read the cloud metadata endpoint, an internal admin panel, or a service
// bound to loopback.
//
// Two independent layers, because either alone has a hole:
//
//   - CheckURL rejects on scheme, port, and literal-IP host. It runs per
//     redirect hop, since a public URL that 302s to 169.254.169.254 is the
//     standard bypass.
//   - DialContext resolves the hostname, checks every returned address, and
//     dials the address it checked. Checking and then handing the *name* to the
//     dialer leaves a DNS-rebinding window where the second lookup returns a
//     different address.
type Guard struct {
	// AllowedSchemes defaults to http and https.
	AllowedSchemes []string
	// AllowedPorts defaults to 80 and 443.
	AllowedPorts []int
	// ExtraDeny blocks additional ranges — an operator's internal supernets.
	ExtraDeny []netip.Prefix

	// AllowPrivateNetworks permits loopback and RFC1918 destinations.
	//
	// DANGEROUS. It exists so tests can reach an httptest server on 127.0.0.1
	// and so an operator can deliberately point Mole at an intranet. Never
	// enable it for a session that fetches attacker-influenced URLs.
	//
	// It does NOT unblock link-local, multicast, or the unspecified address —
	// see CheckAddr. Cloud metadata stays unreachable either way.
	AllowPrivateNetworks bool

	// Resolver defaults to net.DefaultResolver.
	Resolver *net.Resolver
	// DialTimeout defaults to 10s.
	DialTimeout time.Duration
}

// GuardError explains a refusal. Callers map it to FetchGuardDenied, which is
// deliberately not counted as evidence that a headless browser is needed
// (§17.1) — a blocked request is the system working.
type GuardError struct {
	URL    string
	Host   string
	Addr   string
	Reason string
}

func (e *GuardError) Error() string {
	var b strings.Builder
	b.WriteString("fetch: blocked by egress guard: ")
	b.WriteString(e.Reason)
	if e.Host != "" {
		b.WriteString(" (host " + e.Host)
		if e.Addr != "" {
			b.WriteString(" -> " + e.Addr)
		}
		b.WriteString(")")
	}
	return b.String()
}

// deniedPrefixes are ranges that must never be reachable. netip's helpers cover
// most of these, but not all — NAT64 and 6to4 in particular can smuggle a
// private IPv4 destination inside an IPv6 address that no built-in flags.
var deniedPrefixes = []struct {
	prefix netip.Prefix
	reason string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "this-network"},
	{netip.MustParsePrefix("100.64.0.0/10"), "CGNAT"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "TEST-NET-1"},
	{netip.MustParsePrefix("192.88.99.0/24"), "6to4 relay anycast"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmark range"},
	{netip.MustParsePrefix("198.51.100.0/24"), "TEST-NET-2"},
	{netip.MustParsePrefix("203.0.113.0/24"), "TEST-NET-3"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation range"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 (can address private IPv4)"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "local-use NAT64"},
	{netip.MustParsePrefix("2002::/16"), "6to4 (can address private IPv4)"},
	{netip.MustParsePrefix("100::/64"), "discard-only"},
	{netip.MustParsePrefix("2001::/23"), "IETF protocol assignments"},
}

func (g *Guard) schemes() []string {
	if len(g.AllowedSchemes) > 0 {
		return g.AllowedSchemes
	}
	return []string{"http", "https"}
}

func (g *Guard) ports() []int {
	if len(g.AllowedPorts) > 0 {
		return g.AllowedPorts
	}
	return []int{80, 443}
}

// CheckAddr reports why an address is refused, or "" if it is allowed.
func (g *Guard) CheckAddr(addr netip.Addr) string {
	if !addr.IsValid() {
		return "invalid address"
	}
	// Unmap first: ::ffff:127.0.0.1 is loopback, and every IPv4 test below must
	// see the IPv4 form or it silently passes.
	addr = addr.Unmap()

	// Unconditional refusals. These are never a legitimate fetch target, so
	// they hold even when AllowPrivateNetworks is set.
	if addr.IsUnspecified() {
		return "unspecified address"
	}
	if addr.IsMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsLinkLocalMulticast() {
		return "multicast address"
	}
	if addr.IsLinkLocalUnicast() {
		// 169.254.0.0/16 carries the cloud metadata endpoint
		// (169.254.169.254), which hands out instance credentials. An operator
		// enabling AllowPrivateNetworks means "let me reach my intranet", and
		// intranets are RFC1918 — nobody means "let the LLM read my IAM role".
		// Keeping this outside the escape hatch removes the single worst SSRF
		// target without costing any real use case.
		return "link-local address"
	}

	if !g.AllowPrivateNetworks {
		if addr.IsLoopback() {
			return "loopback address"
		}
		if addr.IsPrivate() {
			return "private address"
		}
		for _, d := range deniedPrefixes {
			if d.prefix.Contains(addr) {
				return d.reason
			}
		}
	}

	// Operator-supplied ranges apply even when private networks are allowed:
	// they are an explicit "not this", not part of the default posture.
	for _, p := range g.ExtraDeny {
		if p.Contains(addr) {
			return "operator-denied range " + p.String()
		}
	}
	return ""
}

// CheckURL validates scheme, port, and a literal-IP host. It does not resolve;
// DialContext does that, so that the check and the connection cannot disagree.
func (g *Guard) CheckURL(u *url.URL) error {
	if u == nil {
		return &GuardError{Reason: "nil url"}
	}

	scheme := strings.ToLower(u.Scheme)
	if !contains(g.schemes(), scheme) {
		return &GuardError{URL: u.String(), Reason: "scheme " + strconv.Quote(scheme) + " not allowed"}
	}

	host := u.Hostname()
	if host == "" {
		return &GuardError{URL: u.String(), Reason: "empty host"}
	}

	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return &GuardError{URL: u.String(), Host: host, Reason: "invalid port " + strconv.Quote(port)}
	}
	if !containsInt(g.ports(), n) {
		return &GuardError{URL: u.String(), Host: host, Reason: "port " + port + " not allowed"}
	}

	// A literal IP can be rejected now, before any DNS traffic.
	if addr, err := netip.ParseAddr(host); err == nil {
		if reason := g.CheckAddr(addr); reason != "" {
			return &GuardError{URL: u.String(), Host: host, Addr: addr.String(), Reason: reason}
		}
	}
	return nil
}

// DialContext resolves, checks every address, and connects to one it checked.
//
// If ANY resolved address is denied the whole dial is refused, rather than
// picking an allowed one. A host that round-robins between a public and a
// private address would otherwise be reachable on retry.
func (g *Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &GuardError{Host: address, Reason: "malformed address"}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || !containsInt(g.ports(), port) {
		return nil, &GuardError{Host: host, Reason: "port " + portStr + " not allowed"}
	}

	res := g.Resolver
	if res == nil {
		res = net.DefaultResolver
	}

	addrs, err := res.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("fetch: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, &GuardError{Host: host, Reason: "no addresses resolved"}
	}

	checked := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if reason := g.CheckAddr(a); reason != "" {
			return nil, &GuardError{Host: host, Addr: a.String(), Reason: reason}
		}
		checked = append(checked, a)
	}

	timeout := g.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := &net.Dialer{Timeout: timeout}

	// Dial the checked addresses by IP. Passing the hostname back to the dialer
	// would resolve a second time and reopen the rebinding window.
	var lastErr error
	for _, a := range checked {
		target := net.JoinHostPort(a.String(), portStr)
		conn, err := d.DialContext(ctx, network, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("fetch: dial %s: %w", host, lastErr)
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func containsInt(hay []int, needle int) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
