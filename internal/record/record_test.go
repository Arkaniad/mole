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
