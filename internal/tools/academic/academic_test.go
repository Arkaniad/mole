package academic_test

import (
	"errors"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/tools/academic"
	"github.com/lajosdeme/mole/internal/tools/limiter"
)

// M6 slice 0.
//
// §10.3 makes contact identification a startup check, not a README line, and
// makes rate limiting mandatory rather than best-effort. Both are properties of
// the package rather than of any one provider, so both are tested here — a
// provider added later inherits them and cannot opt out.

func TestAProviderWithoutAContactAddressIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		email string
		want  bool // refused
	}{
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"not an address", "nobody", true},
		{"no domain", "nobody@", true},
		{"usable", "someone@example.org", false},
		{"with a display name", "Mole <someone@example.org>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := academic.Config{Kind: academic.KindUnpaywall, ContactEmail: tc.email}
			err := cfg.Validate()
			if refused := err != nil; refused != tc.want {
				t.Fatalf("Validate() = %v, refused=%v want %v", err, refused, tc.want)
			}
			if tc.want && !errors.Is(err, academic.ErrNoContact) {
				t.Fatalf("refusal is not ErrNoContact, so a caller cannot tell "+
					"'not configured yet' from 'the provider is down': %v", err)
			}
		})
	}
}

// TestAnUnknownProviderIsRefusedBeforeTheAddressIsChecked. Both are
// configuration errors; naming the wrong one sends the user to fix the wrong
// thing.
func TestAnUnknownProviderIsRefused(t *testing.T) {
	cfg := academic.Config{Kind: "elsevier", ContactEmail: "someone@example.org"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown provider was accepted")
	}
	if errors.Is(err, academic.ErrNoContact) {
		t.Fatalf("an unknown provider was reported as a missing contact: %v", err)
	}
}

// TestEveryProviderIsRateLimited is the §10.3 property, asserted over the
// registry rather than per provider — a provider added without a limit would
// otherwise ship unbounded and nothing would notice.
func TestEveryProviderIsRateLimited(t *testing.T) {
	for _, k := range academic.Kinds() {
		lim := academic.LimitFor(k)
		if lim == limiter.Unlimited {
			t.Errorf("%s has no rate limit; §10.3 requires one for every provider", k)
		}
		if lim.Rate <= 0 && lim.MinInterval <= 0 {
			t.Errorf("%s has neither a rate nor a minimum interval: %+v", k, lim)
		}
	}
}

// TestArXivIsHeldToOneConnection. arXiv's terms ask for one request every three
// seconds and a single connection at a time. Burst is the "single connection"
// half: a burst of two IS two simultaneous requests, which is the thing they ask
// callers not to do — and with M5's worker pool it is what would happen by
// default.
func TestArXivIsHeldToOneConnection(t *testing.T) {
	lim := academic.LimitFor(academic.KindArXiv)
	if lim.Burst != 1 {
		t.Errorf("arXiv burst = %d, want 1", lim.Burst)
	}
	if lim.MinInterval < 3*time.Second {
		t.Errorf("arXiv minimum interval = %v, want at least 3s", lim.MinInterval)
	}
}

// TestPubMedUsesTheKeylessRate. NCBI allows 3 requests/second without an API key
// and 10 with one. mole ships keyless, so encoding 10 would mean the DEFAULT
// configuration exceeds the limit it was written for.
func TestPubMedUsesTheKeylessRate(t *testing.T) {
	if got := academic.LimitFor(academic.KindPubMed).Rate; got > 3 {
		t.Errorf("PubMed rate = %v/s, want at most the keyless 3/s", got)
	}
}

// TestRegisterInstallsEveryLimit, and does so on the shared limiter without
// colliding with the fetcher's hostname keys. "arxiv" and "arxiv.org" being
// different buckets is the sort of thing that silently doubles a rate.
func TestRegisterInstallsEveryLimit(t *testing.T) {
	l := limiter.New(limiter.Unlimited)
	academic.Register(l)

	for _, k := range academic.Kinds() {
		key := academic.LimiterKey(k)
		if got := l.LimitFor(key); got != academic.LimitFor(k) {
			t.Errorf("%s: limiter holds %+v, want %+v", k, got, academic.LimitFor(k))
		}
		// A hostname the fetcher would use must not be the same bucket.
		if key == string(k) || key == "arxiv.org" {
			t.Errorf("%s uses key %q, which can collide with a fetcher hostname bucket", k, key)
		}
	}
	if l.LimitFor("arxiv.org") != limiter.Unlimited {
		t.Error("registering academic limits also bound a hostname bucket")
	}
}

// TestFullTextFormatSeparatesUnreadableFromUnavailable.
//
// The distinction decides whether a PDF extractor is worth building: it serves
// pdf_only and nothing else. Folding "we cannot read it" together with "there is
// no legal copy" is how a parser gets built for a corpus that turns out to be
// mostly closed access.
func TestFullTextFormatSeparatesUnreadableFromUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    academic.Paper
		want academic.FullTextFormat
	}{
		{"html wins over pdf", academic.Paper{
			HTMLURL: "https://arxiv.org/html/2402.19155v1",
			PDFURL:  "https://arxiv.org/pdf/2402.19155v1",
		}, academic.FormatHTML},
		{"pdf only", academic.Paper{PDFURL: "https://example.org/p.pdf"}, academic.FormatPDFOnly},
		{"landing page is not full text", academic.Paper{
			LandingURL: "https://publisher.example/abs/1", OpenAccess: true,
		}, academic.FormatClosed},
		{"nothing", academic.Paper{}, academic.FormatClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.FullTextFormat(); got != tc.want {
				t.Fatalf("FullTextFormat() = %q, want %q", got, tc.want)
			}
		})
	}
}
