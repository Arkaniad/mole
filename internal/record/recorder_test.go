package record_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajosdeme/mole/internal/record"
)

// TestReplayMakesNoNetworkCalls is §14.1's whole promise: eval tests run in CI
// without network access and cost nothing. A cassette layer that quietly falls
// through to the origin on a miss would give a green CI run that silently spent
// money — the exact failure ModeAuto is documented as unsafe for.
func TestReplayMakesNoNetworkCalls(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"results":["a","b"]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()

	// Record.
	rec, err := record.Open(record.ModeRecord, dir, "a question about things")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rec.Client().Get(srv.URL + "/search?q=x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("recording made %d requests, want 1", got)
	}

	// Replay. The origin is still up, so a fall-through would succeed silently
	// — which is precisely what this asserts against.
	play, err := record.Open(record.ModeReplay, dir, "a question about things")
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := play.Client().Get(srv.URL + "/search?q=x")
	if err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	body := readAll(t, resp2)
	if !strings.Contains(body, `"results"`) {
		t.Errorf("replayed body = %q", body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("replay reached the network: %d total requests, want 1", got)
	}
}

// TestReplayMissIsLoudRatherThanSilent. A miss in replay must fail, not fetch.
func TestReplayMissIsLoudRatherThanSilent(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	dir := t.TempDir()
	rec, _ := record.Open(record.ModeRecord, dir, "q")
	resp, err := rec.Client().Get(srv.URL + "/recorded")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	rec.Close()

	play, err := record.Open(record.ModeReplay, dir, "q")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := play.Client().Get(srv.URL + "/never-recorded"); err == nil {
		t.Error("an unrecorded request succeeded in replay mode")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the miss reached the network: %d requests, want 1", got)
	}
}

// TestReplayWithoutACassetteNamesTheFile. Failing at Open beats failing on the
// first request, because the message can say which file is missing.
func TestReplayWithoutACassetteNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	_, err := record.Open(record.ModeReplay, dir, "never recorded")
	if err == nil {
		t.Fatal("replaying a nonexistent cassette succeeded")
	}
	if !strings.Contains(err.Error(), "never-recorded.json") {
		t.Errorf("error does not name the missing file: %v", err)
	}
}

// TestWrapKeepsTheUnderlyingTransport. The fetcher's transport carries the
// egress guard's DialContext. Replacing it to get determinism would trade SSRF
// protection for test convenience; wrapping keeps the guard underneath.
func TestWrapKeepsTheUnderlyingTransport(t *testing.T) {
	var reached atomic.Bool
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached.Store(true)
		return &http.Response{
			StatusCode: 200,
			Body:       http.NoBody,
			Header:     http.Header{},
			Request:    r,
		}, nil
	})

	dir := t.TempDir()
	rec, err := record.Open(record.ModeRecord, dir, "wrapped")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: rec.Wrap(base)}

	resp, err := client.Get("https://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !reached.Load() {
		t.Error("recording bypassed the wrapped transport — the guard would be bypassed too")
	}
	rec.Close()

	// On replay the wrapped transport must NOT be reached: no socket, no guard
	// decision, nothing to be non-deterministic about.
	reached.Store(false)
	play, err := record.Open(record.ModeReplay, dir, "wrapped")
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := (&http.Client{Transport: play.Wrap(base)}).Get("https://example.com/x")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if reached.Load() {
		t.Error("replay called through to the underlying transport")
	}
}

// TestOffModeIsATruePassThrough: with recording off, nothing should change —
// no client override, no wrapping, no file.
func TestOffModeIsATruePassThrough(t *testing.T) {
	rec, err := record.Open(record.ModeOff, t.TempDir(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Enabled() {
		t.Error("ModeOff reported as enabled")
	}
	if rec.Client() != nil {
		t.Error("ModeOff returned a client; providers should use their own default")
	}
	base := http.DefaultTransport
	if got := rec.Wrap(base); got == nil {
		t.Error("Wrap returned nil")
	}
	if err := rec.Close(); err != nil {
		t.Errorf("Close on a disabled recorder: %v", err)
	}
}

// TestNilRecorderIsUsable so callers need no conditional at every call site.
func TestNilRecorderIsUsable(t *testing.T) {
	var rec *record.Recorder
	if rec.Enabled() {
		t.Error("nil recorder reported as enabled")
	}
	if rec.Client() != nil {
		t.Error("nil recorder returned a client")
	}
	if rec.Wrap(http.DefaultTransport) == nil {
		t.Error("nil recorder dropped the base transport")
	}
	if err := rec.Close(); err != nil {
		t.Error(err)
	}
}

// TestFromEnvRefusesToGuessADirectory. A cassette is a file that gets
// committed; choosing a location for the user either scatters recordings
// through the working tree or buries them where nobody reviews them.
func TestFromEnvRefusesToGuessADirectory(t *testing.T) {
	t.Setenv(record.EnvMode, "record")
	t.Setenv(record.EnvDir, "")
	if _, err := record.FromEnv("q"); err == nil {
		t.Error("recording started with no cassette directory set")
	}
}

func TestFromEnvIsOffByDefault(t *testing.T) {
	t.Setenv(record.EnvMode, "")
	t.Setenv(record.EnvDir, "")
	rec, err := record.FromEnv("q")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Enabled() {
		t.Error("recording is on by default")
	}
}

func TestFromEnvRejectsAnUnknownMode(t *testing.T) {
	t.Setenv(record.EnvMode, "yes-please")
	if _, err := record.FromEnv("q"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestFromEnvRecordsToTheNamedDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(record.EnvMode, "record")
	t.Setenv(record.EnvDir, dir)

	rec, err := record.FromEnv("What is the consensus on byte-level LLMs?")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "what-is-the-consensus-on-byte-level-llms.json")
	if rec.Path != want {
		t.Errorf("cassette path = %q, want %q", rec.Path, want)
	}
}

// TestSlugIsStableAndReadable: cassettes are reviewed in pull requests, and the
// same question must land on the same file so a re-record replaces its
// predecessor rather than accumulating beside it.
func TestSlugIsStableAndReadable(t *testing.T) {
	cases := map[string]string{
		"What is the consensus on byte-level LLMs?": "what-is-the-consensus-on-byte-level-llms",
		"  Trailing and leading   ":                 "trailing-and-leading",
		"Punctuation!!! everywhere???":              "punctuation-everywhere",
		"MambaByte: 1.31 BPB":                       "mambabyte-1-31-bpb",
		"":                                          "unnamed",
		"???":                                       "unnamed",
	}
	for in, want := range cases {
		if got := record.Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}

	// Same input, same output — otherwise a re-record leaves an orphan.
	q := "a question"
	if record.Slug(q) != record.Slug(q) {
		t.Error("Slug is not deterministic")
	}

	long := record.Slug(strings.Repeat("word ", 100))
	if len(long) > 80 {
		t.Errorf("slug is %d bytes; filesystems cap a component at 255", len(long))
	}
	if strings.HasSuffix(long, "-") {
		t.Error("truncation left a trailing dash")
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
