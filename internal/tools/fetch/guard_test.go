package fetch_test

import (
	"context"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/tools/fetch"
)

// TestCheckAddrBlocksInternalRanges is the core SSRF table. Every row here is a
// destination an attacker-supplied or LLM-chosen URL must not reach.
func TestCheckAddrBlocksInternalRanges(t *testing.T) {
	g := &fetch.Guard{}

	blocked := []struct{ addr, why string }{
		{"127.0.0.1", "loopback"},
		{"127.1.2.3", "loopback (whole /8)"},
		{"::1", "IPv6 loopback"},
		{"169.254.169.254", "cloud metadata endpoint"},
		{"169.254.0.1", "link-local"},
		{"fe80::1", "IPv6 link-local"},
		{"10.0.0.1", "RFC1918"},
		{"172.16.0.1", "RFC1918"},
		{"172.31.255.255", "RFC1918 upper bound"},
		{"192.168.1.1", "RFC1918"},
		{"100.64.0.1", "CGNAT"},
		{"0.0.0.0", "unspecified"},
		{"::", "IPv6 unspecified"},
		{"fc00::1", "IPv6 ULA"},
		{"fd12:3456::1", "IPv6 ULA"},
		{"224.0.0.1", "multicast"},
		{"ff02::1", "IPv6 multicast"},
		{"240.0.0.1", "reserved"},
		{"192.0.2.1", "TEST-NET-1"},
		{"198.51.100.1", "TEST-NET-2"},
		{"203.0.113.1", "TEST-NET-3"},
		{"198.18.0.1", "benchmark range"},

		// The ones a naive implementation misses.
		{"::ffff:127.0.0.1", "IPv4-mapped loopback"},
		{"::ffff:169.254.169.254", "IPv4-mapped metadata endpoint"},
		{"::ffff:10.0.0.1", "IPv4-mapped RFC1918"},
		{"64:ff9b::a00:1", "NAT64 wrapping 10.0.0.1"},
		{"2002:a00:1::", "6to4 wrapping 10.0.0.1"},
		{"2001:db8::1", "documentation range"},
	}

	for _, c := range blocked {
		addr, err := netip.ParseAddr(c.addr)
		if err != nil {
			t.Fatalf("bad test address %q: %v", c.addr, err)
		}
		if reason := g.CheckAddr(addr); reason == "" {
			t.Errorf("%s (%s) was ALLOWED — SSRF hole", c.addr, c.why)
		}
	}

	allowed := []string{
		"1.1.1.1",
		"8.8.8.8",
		"93.184.216.34",
		"2606:2800:220:1:248:1893:25c8:1946",
	}
	for _, a := range allowed {
		addr := netip.MustParseAddr(a)
		if reason := g.CheckAddr(addr); reason != "" {
			t.Errorf("public address %s was blocked: %s", a, reason)
		}
	}
}

func TestAllowPrivateNetworksIsOptIn(t *testing.T) {
	strict := &fetch.Guard{}
	loose := &fetch.Guard{AllowPrivateNetworks: true}

	addr := netip.MustParseAddr("127.0.0.1")
	if strict.CheckAddr(addr) == "" {
		t.Error("loopback allowed by default guard")
	}
	if reason := loose.CheckAddr(addr); reason != "" {
		t.Errorf("loopback blocked with AllowPrivateNetworks: %s", reason)
	}

	// These stay blocked regardless — never a legitimate fetch target, even on
	// an intranet. 169.254.169.254 in particular hands out cloud credentials.
	for _, a := range []string{"224.0.0.1", "0.0.0.0", "169.254.169.254", "fe80::1"} {
		if loose.CheckAddr(netip.MustParseAddr(a)) == "" {
			t.Errorf("%s allowed even with AllowPrivateNetworks", a)
		}
	}
}

func TestExtraDenyAppliesEvenWhenPrivateAllowed(t *testing.T) {
	// An operator's internal supernet is an explicit "not this", so it must
	// survive the escape hatch that permits private networks generally.
	g := &fetch.Guard{
		AllowPrivateNetworks: true,
		ExtraDeny:            []netip.Prefix{netip.MustParsePrefix("10.99.0.0/16")},
	}
	if g.CheckAddr(netip.MustParseAddr("10.99.0.5")) == "" {
		t.Error("operator-denied range was allowed")
	}
	if reason := g.CheckAddr(netip.MustParseAddr("10.1.0.5")); reason != "" {
		t.Errorf("unrelated private address blocked: %s", reason)
	}
}

func TestCheckURLSchemeAndPort(t *testing.T) {
	g := &fetch.Guard{}

	bad := []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/x",
		"dict://example.com:2628/",
		"http://example.com:22/",
		"http://example.com:6379/",
		"https://example.com:8080/",
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"http://",
	}
	for _, raw := range bad {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if err := g.CheckURL(u); err == nil {
			t.Errorf("%s was allowed", raw)
		}
	}

	good := []string{
		"http://example.com/",
		"https://example.com/path?q=1",
		"https://example.com:443/",
		"http://example.com:80/",
	}
	for _, raw := range good {
		u, _ := url.Parse(raw)
		if err := g.CheckURL(u); err != nil {
			t.Errorf("%s was blocked: %v", raw, err)
		}
	}
}

// TestDialRefusesHostnameResolvingToLoopback covers the case CheckURL cannot:
// a perfectly ordinary hostname whose DNS answer points inside.
func TestDialRefusesHostnameResolvingToLoopback(t *testing.T) {
	g := &fetch.Guard{}

	_, err := g.DialContext(context.Background(), "tcp", "localhost:80")
	if err == nil {
		t.Fatal("dial to localhost succeeded")
	}
	var ge *fetch.GuardError
	if !asGuardError(err, &ge) {
		t.Fatalf("error = %T %v, want *fetch.GuardError", err, err)
	}
	if !strings.Contains(ge.Reason, "loopback") {
		t.Errorf("reason = %q, want loopback", ge.Reason)
	}
}

func TestDialRejectsDisallowedPort(t *testing.T) {
	g := &fetch.Guard{AllowPrivateNetworks: true}
	if _, err := g.DialContext(context.Background(), "tcp", "127.0.0.1:22"); err == nil {
		t.Fatal("dial to port 22 succeeded")
	}
}

func asGuardError(err error, target **fetch.GuardError) bool {
	for err != nil {
		if ge, ok := err.(*fetch.GuardError); ok {
			*target = ge
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
