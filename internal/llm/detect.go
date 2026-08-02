package llm

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// Auto-detection.
//
// The goal from §4.2 is that a machine which already has a usable model needs
// no configuration at all. Obtaining API access is the one step Mole cannot
// remove; discovering access that already exists is entirely avoidable work.

// Detected describes a provider found without configuration.
type Detected struct {
	Config Config
	// Reason is shown to the user, so it has to name what was found rather
	// than just asserting success.
	Reason string
	// Free is true for local runtimes, where token-mode budgeting is the only
	// meaningful ceiling because no money changes hands.
	Free bool
}

// LocalEndpoint is a runtime worth probing on localhost.
type LocalEndpoint struct {
	Name    string
	BaseURL string
	// Probe is the path that lists models; a 200 means something is listening
	// and speaking the right dialect.
	Probe string
}

// LocalEndpoints are the runtimes checked, in order.
func LocalEndpoints() []LocalEndpoint {
	return []LocalEndpoint{
		{Name: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Probe: "http://127.0.0.1:11434/api/tags"},
		{Name: "llama.cpp / vLLM", BaseURL: "http://127.0.0.1:8080/v1", Probe: "http://127.0.0.1:8080/v1/models"},
		{Name: "LM Studio", BaseURL: "http://127.0.0.1:1234/v1", Probe: "http://127.0.0.1:1234/v1/models"},
	}
}

// Detect looks for a usable provider without any configuration.
//
// Order matters: an explicitly configured hosted key beats a local model,
// because someone who set a key meant to use it. Local comes last as the
// zero-cost fallback that makes Mole runnable with no account at all.
func Detect(ctx context.Context, client *http.Client) (*Detected, bool) {
	if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
		return &Detected{
			Config: Config{Kind: KindAnthropic, APIKey: v},
			Reason: "ANTHROPIC_API_KEY in environment",
		}, true
	}
	if v := os.Getenv("MOLE_LLM_API_KEY"); v != "" {
		return &Detected{
			Config: Config{Kind: KindAnthropic, APIKey: v},
			Reason: "MOLE_LLM_API_KEY in environment",
		}, true
	}

	// An `ant auth login` profile is resolved by the SDK itself, so an empty
	// key here is a working configuration rather than a missing one.
	if hasAnthropicProfile() {
		return &Detected{
			Config: Config{Kind: KindAnthropic},
			Reason: "ant auth login profile",
		}, true
	}

	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
		return &Detected{
			Config: Config{
				Kind: KindOpenAICompatible, APIKey: v,
				BaseURL:     "https://api.deepseek.com/v1",
				StrongModel: "deepseek-chat", CheapModel: "deepseek-chat",
			},
			Reason: "DEEPSEEK_API_KEY in environment",
		}, true
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		return &Detected{
			Config: Config{
				Kind: KindOpenAICompatible, APIKey: v,
				BaseURL: openAIDefaultBaseURL, StrongModel: "gpt-4o", CheapModel: "gpt-4o-mini",
			},
			Reason: "OPENAI_API_KEY in environment",
		}, true
	}

	if d, ok := detectLocal(ctx, client); ok {
		return d, true
	}
	return nil, false
}

// hasAnthropicProfile reports whether `ant auth login` has stored credentials
// the SDK can resolve.
//
// Deliberately does NOT look at ~/.claude/.credentials.json. That is Claude
// Code's own subscription credential for a different product; using it here
// would be reaching into another application's store for a token it was not
// issued to hand out (§4.2).
func hasAnthropicProfile() bool {
	dir := os.Getenv("ANTHROPIC_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = home + "/.config/anthropic"
	}
	entries, err := os.ReadDir(dir + "/credentials")
	return err == nil && len(entries) > 0
}

// detectLocal probes localhost runtimes.
func detectLocal(ctx context.Context, client *http.Client) (*Detected, bool) {
	if client == nil {
		client = &http.Client{}
	}

	for _, ep := range LocalEndpoints() {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, ep.Probe, nil)
		if err != nil {
			cancel()
			continue
		}
		resp, err := client.Do(req)
		cancel()
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}

		return &Detected{
			Config: Config{Kind: KindOpenAICompatible, BaseURL: ep.BaseURL},
			Reason: ep.Name + " on " + ep.BaseURL,
			Free:   true,
		}, true
	}
	return nil, false
}

// IsLoopback reports whether a base URL points at this machine.
func IsLoopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// Reachable probes an OpenAI-compatible endpoint.
//
// Only meaningful for local runtimes, where the check is free and instant. A
// hosted provider cannot be verified without spending money, so `doctor`
// reports those as configured-but-unverified and points at `config test-llm`
// rather than printing a green tick it has not earned.
func Reachable(ctx context.Context, baseURL string, client *http.Client) bool {
	if client == nil {
		client = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/models", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Any answer proves something is listening and speaking HTTP; a 404 on
	// /models still means the runtime is up.
	return resp.StatusCode < 500
}
