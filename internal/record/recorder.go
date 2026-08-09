package record

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Wiring.
//
// §14.1 requires every outbound call — search, fetch, LLM, academic — to pass
// through this package, so orchestrator and eval tests are deterministic, run
// without network access, and cost nothing.
//
// A Recorder is that seam made concrete: one cassette per named run, one client
// handed to every provider, and a Close that persists it. Providers already
// accept an *http.Client or a transport hook, so nothing below this line needs
// to know cassettes exist.
//
// Mode is deliberately NOT a persisted config field. It belongs to an
// invocation, not to an installation: a user who set "record" once and forgot
// would silently accumulate every API response on disk, including bodies from
// pages they only meant to read once. An environment variable is ephemeral,
// visible in the command that set it, and natural in CI.

// EnvMode and EnvDir configure recording.
const (
	EnvMode = "MOLE_RECORD"
	EnvDir  = "MOLE_CASSETTE_DIR"
)

// Recorder is a cassette plus the client that reads and writes it.
type Recorder struct {
	Mode     Mode
	Cassette *Cassette
	Path     string

	client *http.Client
	base   http.RoundTripper
}

// ParseMode validates a mode string. An empty value is ModeOff.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeRecord:
		return ModeRecord, nil
	case ModeReplay:
		return ModeReplay, nil
	case ModeAuto:
		return ModeAuto, nil
	default:
		return "", fmt.Errorf("record: unknown mode %q (want off, record, replay, or auto)", s)
	}
}

// ModeFromEnv reports the configured mode without opening anything.
//
// Separate from FromEnv because some checks have to happen BEFORE a cassette is
// touched. Refusing a worker pool is one: in replay, FromEnv fails on a missing
// cassette first, so a guard placed after it never runs and the caller is told
// the recording is absent rather than that the request was never allowed.
func ModeFromEnv() (Mode, error) {
	return ParseMode(os.Getenv(EnvMode))
}

// FromEnv builds a Recorder for a named run from MOLE_RECORD and
// MOLE_CASSETTE_DIR. With recording off it returns a Recorder that passes
// everything through, so callers need no conditional.
func FromEnv(name string) (*Recorder, error) {
	mode, err := ModeFromEnv()
	if err != nil {
		return nil, err
	}
	if mode == ModeOff {
		return &Recorder{Mode: ModeOff}, nil
	}

	dir := os.Getenv(EnvDir)
	if dir == "" {
		// No default. A cassette directory chosen for the user would either
		// scatter recordings through the working tree or bury them somewhere
		// they are never reviewed before being committed — and a cassette is a
		// file that gets committed.
		return nil, fmt.Errorf("record: %s=%s requires %s to be set", EnvMode, mode, EnvDir)
	}
	return Open(mode, dir, name)
}

// Open loads (or creates) the cassette for a named run.
func Open(mode Mode, dir, name string) (*Recorder, error) {
	if mode == ModeOff {
		return &Recorder{Mode: ModeOff}, nil
	}
	path := filepath.Join(dir, Slug(name)+".json")

	cas, err := LoadCassette(path)
	if err != nil {
		return nil, err
	}
	if mode == ModeReplay && cas.Len() == 0 {
		// Failing here beats failing on the first request: the message can name
		// the file that is missing, which "cassette miss for <sha>" cannot.
		return nil, fmt.Errorf("record: no cassette at %s to replay (record it first with %s=record)", path, EnvMode)
	}
	return &Recorder{Mode: mode, Cassette: cas, Path: path}, nil
}

// Client returns the HTTP client every provider should use.
func (r *Recorder) Client() *http.Client {
	if r == nil || r.Mode == ModeOff {
		return nil // providers treat nil as "use your own default"
	}
	if r.client == nil {
		r.client = NewTransport(r.Mode, r.Cassette, r.base).Client()
	}
	return r.client
}

// Wrap adapts a transport that a caller has already built.
//
// The fetcher is the one provider that cannot simply be handed a client: its
// transport carries the egress guard's DialContext, and replacing it would
// bypass SSRF protection. Wrapping instead keeps the guard underneath, so a
// recording still goes through it and a replay never reaches a socket at all.
func (r *Recorder) Wrap(base http.RoundTripper) http.RoundTripper {
	if r == nil || r.Mode == ModeOff {
		return base
	}
	return NewTransport(r.Mode, r.Cassette, base)
}

// Enabled reports whether anything is being recorded or replayed.
func (r *Recorder) Enabled() bool { return r != nil && r.Mode != ModeOff }

// Close persists the cassette if anything changed.
func (r *Recorder) Close() error {
	if r == nil || r.Cassette == nil {
		return nil
	}
	return r.Cassette.Save()
}

// Slug turns a run name into a stable, readable filename.
//
// Readability is the point: a cassette is reviewed in a pull request, and
// "what-is-the-consensus-on-byte-level-llms.json" is reviewable in a way that a
// hash is not. Same question in, same file out, so a re-record replaces the
// recording it supersedes rather than accumulating beside it.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "unnamed"
	}
	// Bound it: some filesystems cap a component at 255 bytes, and a research
	// question can be a paragraph.
	const max = 80
	if len(s) > max {
		s = strings.Trim(s[:max], "-")
	}
	return s
}
