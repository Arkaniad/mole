package llm_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/llm"
)

// The live check, against an actual reasoning model.
//
// Everything above is a fake that behaves the way a reasoning model was OBSERVED
// to behave; this is the observation itself, kept runnable. It skips unless
// MOLE_REASONING_TEST_URL and MOLE_REASONING_TEST_MODEL are set, because it needs
// a model on the machine and takes tens of seconds:
//
//	MOLE_REASONING_TEST_URL=http://localhost:11434/v1 \
//	MOLE_REASONING_TEST_MODEL=qwen3:4b \
//	  go test ./internal/llm -run Live -v
//
// What it pins is the property the whole feature exists for: a request whose
// answer allowance is smaller than the model's chain of thought still comes back
// with an answer.
func TestAgainstALiveReasoningModel(t *testing.T) {
	base := os.Getenv("MOLE_REASONING_TEST_URL")
	model := os.Getenv("MOLE_REASONING_TEST_MODEL")
	if base == "" || model == "" {
		t.Skip("set MOLE_REASONING_TEST_URL and MOLE_REASONING_TEST_MODEL to run this")
	}

	p, err := llm.New(llm.Config{
		Kind:        llm.KindOpenAICompatible,
		BaseURL:     base,
		StrongModel: model,
		Timeout:     4 * time.Minute,
	}, &http.Client{Timeout: 4 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	// 80 tokens of answer allowance. Measured: qwen3:4b spends more than that
	// thinking before it writes anything, so this is the failing case.
	resp, err := p.Complete(context.Background(), llm.Request{
		System:    "Reply with JSON only. No prose.",
		Messages:  []llm.Message{llm.User(`Reply with exactly this JSON: {"ok":true}`)},
		MaxTokens: 80,
	})
	if err != nil {
		t.Fatalf("a live reasoning model still fails: %v", err)
	}
	if strings.TrimSpace(resp.Text) == "" {
		t.Fatal("empty content from a live reasoning model")
	}
	if !strings.Contains(resp.Text, "ok") {
		t.Errorf("text = %q, want the requested JSON", resp.Text)
	}
	t.Logf("attempts=%d reasoning_tokens=%d output_tokens=%d text=%q",
		resp.Attempts, resp.ReasoningTokens, resp.Usage.OutputTokens, resp.Text)
	if resp.Reasoning == "" {
		t.Error("the chain of thought was not captured")
	}
	if resp.Usage.OutputTokens == 0 {
		t.Error("no output tokens reported; the ceiling would be unenforceable")
	}
}
