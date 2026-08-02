package llm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lajosdeme/mole/internal/llm"
)

// clearProviderEnv removes every variable Detect consults, so a test asserts
// what it set rather than what the developer's shell happens to hold.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ANTHROPIC_API_KEY", "MOLE_LLM_API_KEY", "DEEPSEEK_API_KEY", "OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	// Point the profile check at an empty directory so a real `ant auth login`
	// on the developer's machine cannot decide the outcome.
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir())
}

// TestDetectPrefersExplicitKeyOverEverythingElse pins the precedence §4.2
// depends on: a key someone deliberately exported must beat an ambient
// credential, or a session silently runs against the wrong account.
func TestDetectPrefersExplicitKeyOverEverythingElse(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-explicit")
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")

	got, ok := llm.Detect(context.Background(), noNetwork())
	if !ok {
		t.Fatal("no provider detected despite ANTHROPIC_API_KEY being set")
	}
	if got.Config.Kind != llm.KindAnthropic || got.Config.APIKey != "sk-ant-explicit" {
		t.Errorf("detected %s/%q, want anthropic with the explicit key", got.Config.Kind, got.Config.APIKey)
	}
	if got.Free {
		t.Error("a hosted key was reported as free, which would disable USD budgeting")
	}
}

func TestDetectFallsBackThroughVendorKeys(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-deepseek")

	got, ok := llm.Detect(context.Background(), noNetwork())
	if !ok {
		t.Fatal("DEEPSEEK_API_KEY was not detected")
	}
	if got.Config.Kind != llm.KindOpenAICompatible {
		t.Errorf("kind = %s, want openai-compatible", got.Config.Kind)
	}
	if got.Config.StrongModel == "" || got.Config.CheapModel == "" {
		t.Error("no models resolved; an OpenAI-compatible endpoint has no knowable default")
	}
}

// TestDetectFindsAnthropicProfileWithoutAKey covers the case the credential
// chain exists for: `ant auth login` leaves no key in the environment, and
// treating that as unconfigured is what would force a user to set one anyway.
func TestDetectFindsAnthropicProfileWithoutAKey(t *testing.T) {
	clearProviderEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials", "default.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_CONFIG_DIR", dir)

	got, ok := llm.Detect(context.Background(), noNetwork())
	if !ok {
		t.Fatal("an ant auth login profile was not detected")
	}
	if got.Config.APIKey != "" {
		t.Error("a key was invented for a profile-based credential")
	}
	if got.Config.Kind != llm.KindAnthropic {
		t.Errorf("kind = %s, want anthropic", got.Config.Kind)
	}
}

// TestDetectReportsNothingWhenNothingIsConfigured: returning a provider here
// would produce a confident failure at the first model call instead of an
// actionable message from doctor.
func TestDetectReportsNothingWhenNothingIsConfigured(t *testing.T) {
	clearProviderEnv(t)
	if got, ok := llm.Detect(context.Background(), noNetwork()); ok {
		t.Errorf("detected %+v on a machine with nothing configured", got.Config)
	}
}

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:11434/v1": true,
		"http://localhost:8080/v1":  true,
		"http://[::1]:1234/v1":      true,
		"https://api.anthropic.com": false,
		"http://10.0.0.5:8080/v1":   false,
		"":                          false,
		"://nonsense":               false,
	}
	for in, want := range cases {
		if got := llm.IsLoopback(in); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestReachableAcceptsAnyNon5xx: doctor uses this to earn a green tick. A 404
// on /models still proves a runtime is listening, and demanding 200 would
// report working local setups as broken.
func TestReachableAcceptsAnyNon5xx(t *testing.T) {
	for _, status := range []int{200, 404, 401} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		if !llm.Reachable(context.Background(), srv.URL, srv.Client()) {
			t.Errorf("status %d reported unreachable", status)
		}
		srv.Close()
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	if llm.Reachable(context.Background(), srv.URL, srv.Client()) {
		t.Error("a 502 was reported as reachable")
	}
}

func TestReachableIsFalseWhenNothingListens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening on that port

	if llm.Reachable(context.Background(), url, nil) {
		t.Error("a closed port was reported as reachable")
	}
}

// noNetwork fails any local probe instantly, so a developer running ollama does
// not change what these tests assert.
func noNetwork() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, http.ErrServerClosed
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
