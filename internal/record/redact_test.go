package record_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/record"
)

// TestProviderKeysNeverReachDisk. A cassette is a normal file that gets
// committed and shared, so a key written into one is a leaked credential.
//
// The named-header list already failed once in exactly this way: Brave
// authenticates with X-Subscription-Token, which nothing matched, and the key
// went to disk verbatim. Every vendor below is a header some provider actually
// uses; the point of the table is that adding a provider must not require
// remembering to add a redaction rule.
func TestProviderKeysNeverReachDisk(t *testing.T) {
	headers := map[string]string{
		"X-Subscription-Token": "brave-secret-value",
		"Authorization":        "Bearer tavily-secret-value",
		"X-Api-Key":            "anthropic-secret-value",
		"Api-Key":              "azure-secret-value",
		"X-Goog-Api-Key":       "google-secret-value",
		"X-Auth-Token":         "openstack-secret-value",
		"Cookie":               "session=cookie-secret-value",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "sid=response-secret-value")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "cassette.json")
	cas, err := record.LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	client := record.NewTransport(record.ModeRecord, cas, nil).Client()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/search?q=x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if err := cas.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for name, value := range headers {
		if strings.Contains(string(saved), value) {
			t.Errorf("LEAK: %s written verbatim to the cassette", name)
		}
	}
	if strings.Contains(string(saved), "response-secret-value") {
		t.Error("LEAK: Set-Cookie from the response written to the cassette")
	}
}

// TestQueryAndUserinfoCredentialsAreStripped: several providers take the key as
// a query parameter, and https://user:pass@host is a credential in the URL. The
// stored request and the match key are both derived from it.
func TestQueryAndUserinfoCredentialsAreStripped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "cassette.json")
	cas, _ := record.LoadCassette(path)
	client := record.NewTransport(record.ModeRecord, cas, nil).Client()

	resp, err := client.Get(srv.URL + "/s?api_key=query-secret-value&q=hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := cas.Save(); err != nil {
		t.Fatal(err)
	}

	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "query-secret-value") {
		t.Error("LEAK: api_key query parameter written to the cassette")
	}
	if !strings.Contains(string(saved), "hello") {
		t.Error("a non-secret query parameter was dropped, which would break matching")
	}
}

// TestReplayStillMatchesAfterRedaction: redaction happens at the write
// boundary, and the key excludes headers, so a redacted recording must still
// replay. Otherwise the fix above would quietly break every cassette.
func TestReplayStillMatchesAfterRedaction(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "cassette.json")
	cas, _ := record.LoadCassette(path)
	rec := record.NewTransport(record.ModeRecord, cas, nil).Client()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("X-Subscription-Token", "brave-secret-value")
	resp, err := rec.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := cas.Save(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := record.LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	play := record.NewTransport(record.ModeReplay, reloaded, nil).Client()

	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	// A different key: headers are deliberately not part of the match.
	req2.Header.Set("X-Subscription-Token", "a-completely-different-key")
	resp2, err := play.Do(req2)
	if err != nil {
		t.Fatalf("replay missed after redaction: %v", err)
	}
	resp2.Body.Close()

	if hits != 1 {
		t.Errorf("origin hit %d times, want 1 — replay went to the network", hits)
	}
}
