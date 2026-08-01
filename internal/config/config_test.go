package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lajosdeme/mole/internal/config"
)

// isolate points config at a temp dir so tests never touch a real one.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MOLE_CONFIG_DIR", dir)
	// Clear anything the developer's shell might be exporting, or the env
	// overrides would leak into assertions about file contents.
	for _, k := range []string{
		"MOLE_SEARCH_PROVIDER", "MOLE_BRAVE_API_KEY", "BRAVE_API_KEY",
		"MOLE_TAVILY_API_KEY", "TAVILY_API_KEY", "MOLE_LLM_API_KEY",
		"ANTHROPIC_API_KEY", "MOLE_LLM_BASE_URL", "MOLE_LLM_MODEL",
		"MOLE_CONTACT_EMAIL",
	} {
		t.Setenv(k, "")
	}
	return dir
}

func TestLoadWithoutConfigIsNotAHardError(t *testing.T) {
	isolate(t)
	cfg, err := config.Load()
	if !errors.Is(err, config.ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
	if cfg == nil {
		t.Fatal("Load returned nil config; callers need a usable zero value")
	}
}

func TestSetSaveRoundTrip(t *testing.T) {
	isolate(t)

	cfg := &config.Config{}
	for _, kv := range [][2]string{
		{"search.provider", "tavily"},
		{"search.tavily-key", "tvly-secret-value"},
		{"search.brave-key", "brave-secret-value"},
		{"llm.provider", "anthropic"},
		{"llm.api-key", "sk-ant-secret"},
		{"llm.model", "claude-opus-5"},
		{"contact-email", "researcher@example.com"},
	} {
		if err := cfg.Set(kv[0], kv[1]); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Search.Provider != "tavily" {
		t.Errorf("provider = %q", loaded.Search.Provider)
	}
	if loaded.Search.ActiveKey() != "tvly-secret-value" {
		t.Errorf("active key = %q, want the Tavily key", loaded.Search.ActiveKey())
	}

	// Both keys persist, so switching providers does not mean re-entering a
	// key that was already supplied.
	if err := loaded.Set("search.provider", "brave"); err != nil {
		t.Fatal(err)
	}
	if loaded.Search.ActiveKey() != "brave-secret-value" {
		t.Errorf("after switch, active key = %q", loaded.Search.ActiveKey())
	}
}

// TestConfigFileIsOwnerOnly: the file holds API keys and nothing else protects
// them until the keyring lands with the daemon work.
func TestConfigFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions are not meaningful on windows")
	}
	dir := isolate(t)

	cfg := &config.Config{}
	if err := cfg.Set("llm.api-key", "sk-secret"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config mode = %04o, want 0600", mode)
	}

	ok, detail := config.CheckPermissions()
	if !ok {
		t.Errorf("CheckPermissions failed on a freshly written file: %s", detail)
	}

	// A loosened file is reported rather than silently tolerated.
	if err := os.Chmod(filepath.Join(dir, "config.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := config.CheckPermissions(); ok {
		t.Error("world-readable config passed the permission check")
	}
}

// TestSaveOverLooseFileTightensIt covers the upgrade path: a file created by an
// earlier version, or by a user's editor, must not stay world-readable.
func TestSaveOverLooseFileTightensIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions are not meaningful on windows")
	}
	dir := isolate(t)
	path := filepath.Join(dir, "config.json")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	_ = cfg.Set("llm.api-key", "sk-secret")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode after save = %04o, want 0600", mode)
	}
}

func TestSecretsAreMaskedForDisplay(t *testing.T) {
	isolate(t)

	cfg := &config.Config{}
	_ = cfg.Set("llm.api-key", "sk-ant-api03-verylongsecretvalue")
	_ = cfg.Set("llm.model", "claude-opus-5")

	var sawSecret, sawPlain bool
	for _, f := range config.Fields() {
		shown := config.Display(f, cfg)
		if f.Name == "llm.api-key" {
			sawSecret = true
			if strings.Contains(shown, "verylongsecret") {
				t.Errorf("secret leaked into display: %q", shown)
			}
			if !strings.Contains(shown, "*") {
				t.Errorf("secret not masked: %q", shown)
			}
		}
		if f.Name == "llm.model" {
			sawPlain = true
			if shown != "claude-opus-5" {
				t.Errorf("non-secret was mangled: %q", shown)
			}
		}
	}
	if !sawSecret || !sawPlain {
		t.Fatal("field table is missing expected keys")
	}
}

func TestMaskNeverRevealsShortSecrets(t *testing.T) {
	for _, s := range []string{"", "a", "abc", "12345678"} {
		if got := config.Mask(s); strings.Contains(got, s) && s != "" {
			t.Errorf("Mask(%q) = %q leaks the value", s, got)
		}
	}
	long := config.Mask("sk-ant-1234567890abcdef")
	if !strings.HasPrefix(long, "sk-a") {
		t.Errorf("Mask lost the identifying prefix: %q", long)
	}
}

func TestValidationRejectsBadValues(t *testing.T) {
	isolate(t)
	cfg := &config.Config{}

	if err := cfg.Set("search.provider", "google"); err == nil {
		t.Error("unknown search provider accepted")
	}
	if err := cfg.Set("llm.provider", "hotdog"); err == nil {
		t.Error("unknown llm provider accepted")
	}
	if err := cfg.Set("contact-email", "not-an-address"); err == nil {
		t.Error("malformed contact email accepted")
	}
	if err := cfg.Set("nonexistent.key", "x"); err == nil {
		t.Error("unknown key accepted")
	}

	// Provider names are normalized rather than rejected on case.
	if err := cfg.Set("search.provider", "  BRAVE "); err != nil {
		t.Errorf("case/whitespace variant rejected: %v", err)
	}
	if cfg.Search.Provider != "brave" {
		t.Errorf("provider = %q, want normalized to brave", cfg.Search.Provider)
	}
}

// TestEnvOverridesFile: containers and CI supply secrets through the
// environment, and a live env var should win over a stale file.
func TestEnvOverridesFile(t *testing.T) {
	isolate(t)

	cfg := &config.Config{}
	_ = cfg.Set("search.provider", "brave")
	_ = cfg.Set("search.brave-key", "from-file")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BRAVE_API_KEY", "from-env")

	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Search.BraveKey != "from-env" {
		t.Errorf("key = %q, want the environment value", loaded.Search.BraveKey)
	}
}

func TestEnvAloneIsEnoughToBeConfigured(t *testing.T) {
	isolate(t)
	t.Setenv("MOLE_SEARCH_PROVIDER", "tavily")
	t.Setenv("TAVILY_API_KEY", "tvly-env")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("env-only config reported an error: %v", err)
	}
	if cfg.Search.ActiveKey() != "tvly-env" {
		t.Errorf("active key = %q", cfg.Search.ActiveKey())
	}
}
