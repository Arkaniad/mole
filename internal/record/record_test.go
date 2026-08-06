package record_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/record"
)

func TestRecordThenReplayWithoutNetwork(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":42}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "cassette.json")

	// Record.
	cas, err := record.LoadCassette(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	client := record.NewTransport(record.ModeRecord, cas, nil).Client()
	resp, err := client.Get(srv.URL + "/search?q=byte+level+llm")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"answer":42}` {
		t.Fatalf("recorded body = %q", body)
	}
	if err := cas.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if hits != 1 {
		t.Fatalf("server hits during record = %d, want 1", hits)
	}

	// Replay: the server must not be touched again. Closing it proves it.
	srv.Close()

	cas2, err := record.LoadCassette(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cas2.Len() != 1 {
		t.Fatalf("reloaded cassette has %d interactions, want 1", cas2.Len())
	}
	client2 := record.NewTransport(record.ModeReplay, cas2, nil).Client()
	resp2, err := client2.Get(srv.URL + "/search?q=byte+level+llm")
	if err != nil {
		t.Fatalf("replay get: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body2) != `{"answer":42}` {
		t.Fatalf("replayed body = %q, want the recorded body", body2)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("replayed status = %d", resp2.StatusCode)
	}
	if hits != 1 {
		t.Fatalf("server was hit during replay: %d total", hits)
	}
}

// TestReplayMissIsAnError guards the CI property: a cassette gap must fail the
// test rather than silently make a real, billable network call.
func TestReplayMissIsAnError(t *testing.T) {
	cas, err := record.LoadCassette(filepath.Join(t.TempDir(), "empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	client := record.NewTransport(record.ModeReplay, cas, nil).Client()

	_, err = client.Get("https://example.invalid/never-recorded")
	var miss *record.ErrCassetteMiss
	if !errors.As(err, &miss) {
		t.Fatalf("error = %v, want ErrCassetteMiss", err)
	}
}

// TestSecretsNeverReachDisk is a security test, not a hygiene one: cassettes
// get committed and shared, so a key written into one is a leaked credential.
func TestSecretsNeverReachDisk(t *testing.T) {
	const secret = "sk-ant-super-secret-key-value"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session="+secret)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "secrets.json")
	cas, err := record.LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	client := record.NewTransport(record.ModeRecord, cas, nil).Client()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/messages?api_key="+secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-Api-Key", secret)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if err := cas.Save(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("cassette on disk contains the secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "REDACTED") {
		t.Fatalf("cassette does not show redaction happened:\n%s", raw)
	}
}

// TestKeyIgnoresJSONKeyOrder: Go map iteration order is random, so an
// identical request serialized twice can produce different bytes. Without
// canonicalization every replay would miss.
func TestKeyIgnoresJSONKeyOrder(t *testing.T) {
	u, _ := url.Parse("https://api.example.com/v1/messages")
	a := record.RequestKey("POST", u, []byte(`{"model":"claude-opus-5","max_tokens":100}`))
	b := record.RequestKey("POST", u, []byte(`{"max_tokens":100,"model":"claude-opus-5"}`))
	if a != b {
		t.Errorf("key depends on JSON key order: %s vs %s", a, b)
	}

	c := record.RequestKey("POST", u, []byte(`{"model":"claude-sonnet-5","max_tokens":100}`))
	if a == c {
		t.Error("different request bodies produced the same key")
	}
}

func TestKeyIgnoresQueryOrderAndSecrets(t *testing.T) {
	u1, _ := url.Parse("https://api.example.com/s?b=2&a=1")
	u2, _ := url.Parse("https://API.EXAMPLE.com/s?a=1&b=2")
	if record.RequestKey("GET", u1, nil) != record.RequestKey("GET", u2, nil) {
		t.Error("key depends on query order or host casing")
	}

	// Two calls that differ only in the API key are the same interaction.
	k1, _ := url.Parse("https://api.example.com/s?q=x&api_key=AAA")
	k2, _ := url.Parse("https://api.example.com/s?q=x&api_key=BBB")
	if record.RequestKey("GET", k1, nil) != record.RequestKey("GET", k2, nil) {
		t.Error("key varies with the redacted api_key")
	}
}

func TestModeOffIsPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("live"))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "unused.json")
	cas, _ := record.LoadCassette(path)
	client := record.NewTransport(record.ModeOff, cas, nil).Client()

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "live" {
		t.Fatalf("body = %q", body)
	}
	if cas.Len() != 0 {
		t.Errorf("ModeOff recorded %d interactions, want 0", cas.Len())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("ModeOff wrote a cassette file")
	}
}

func TestPostBodyIsPreservedForTheRealRequest(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cas, _ := record.LoadCassette(filepath.Join(t.TempDir(), "post.json"))
	client := record.NewTransport(record.ModeRecord, cas, nil).Client()

	const body = `{"model":"claude-opus-5"}`
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The transport reads the body to compute the key; it must put it back.
	if got != body {
		t.Fatalf("server received %q, want %q", got, body)
	}
}

// TestAPromptFenceDoesNotDefeatReplay.
//
// §3.2 names every untrusted-content delimiter with a fresh crypto/rand nonce, so
// two identical requests made a minute apart have different bytes. Since the
// cassette key hashes the body, no LLM interaction could ever replay: `mole corpus`
// missed on the very first planner call and reported "ran but produced nothing".
//
// The nonce is masked for keying only — the prompt sent to the model still carries
// a real random fence.
func TestAPromptFenceDoesNotDefeatReplay(t *testing.T) {
	u, _ := url.Parse("https://api.anthropic.com/v1/messages")

	// The shape that actually crosses the wire. encoding/json HTML-escapes angle
	// brackets, so the fence arrives as <tag-…> — a pattern written
	// against literal brackets matches this test's other case and nothing real,
	// which is how the first attempt at this fix passed while replay stayed broken.
	body := func(nonce string) []byte {
		return []byte(`{"messages":[{"text":"between <question-` + nonce +
			`> and </question-` + nonce + `>"}]}`)
	}
	a := record.RequestKey("POST", u, body("f1b9050687783bcb"))
	b := record.RequestKey("POST", u, body("00112233445566aa"))
	if a != b {
		t.Errorf("two runs of the same request keyed differently:\n  %s\n  %s", a, b)
	}

	// Literal brackets must fold to the same key as the escaped form, so a client
	// that does not HTML-escape replays against a cassette recorded by one that does.
	lit := record.RequestKey("POST", u, []byte(
		`{"messages":[{"text":"between <question-f1b9050687783bcb> and </question-f1b9050687783bcb>"}]}`))
	if lit != a {
		t.Errorf("escaped and literal bracket forms keyed differently:\n  %s\n  %s", lit, a)
	}

	// Everything that is not a fence must still separate requests, or the harness
	// replays the wrong recorded response — worse than a miss, because it looks
	// like a pass.
	other := record.RequestKey("POST", u, []byte(
		`{"messages":[{"text":"between <question-f1b9050687783bcb> and a DIFFERENT question"}]}`))
	if other == a {
		t.Error("bodies differing outside the fence collided")
	}
	// A bare 16-hex run is not a fence. Masking one would collide two requests
	// whose page content merely contained a hash.
	h1 := record.RequestKey("POST", u, []byte(`{"text":"sha f1b9050687783bcb"}`))
	h2 := record.RequestKey("POST", u, []byte(`{"text":"sha 00112233445566aa"}`))
	if h1 == h2 {
		t.Error("bare hex runs were masked; only a <tag-NONCE> fence may be")
	}
}

// TestACassetteIsReKeyedOnLoad. Cassettes outlive the key function: masking fences
// changed it, and something else will. Indexing by the recorded Key field would
// make every existing cassette unmatchable the day that happens, with a symptom
// (replays nothing) that reads as a bad recording rather than a changed hash.
//
// Driven through the replay transport rather than the index, so it asserts the
// interaction is actually SERVED, not merely present under some key.
func TestACassetteIsReKeyedOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	// A stale key, as if written by an older build with a different key function.
	if err := os.WriteFile(path, []byte(`[{"key":"deadbeefdeadbeefdeadbeefdeadbeef",
      "request":{"method":"POST","url":"https://api.example.com/v1","body":"{\"a\":1}"},
      "response":{"status":200,"body":"served from cassette"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	cas, err := record.LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	client := record.NewTransport(record.ModeReplay, cas, nil).Client()
	resp, err := client.Post("https://api.example.com/v1", "application/json",
		strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatalf("stale-keyed interaction was not served: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "served from cassette" {
		t.Errorf("body = %q, want the recorded one", got)
	}
}
