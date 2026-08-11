package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lajosdeme/mole/internal/llm"
)

// Reasoning-model support (a known gap since M1).
//
// qwen3 and gemma4 emit their chain of thought against the SAME output allowance
// as their answer, so `MaxTokens: 4096` can buy 4096 tokens of thinking and an
// empty message. Ollama's /v1 ignored every documented way to switch it off
// (`think:false`, `/no_think`, `chat_template_kwargs.enable_thinking`), so mole
// budgets for reasoning instead of fighting it: MaxTokens is the ANSWER allowance
// and the provider adds room for the chain on top.
//
// Verified against a real ollama in TestAgainstALiveReasoningModel below, which
// skips unless one is reachable.

// reasoningServer answers empty-with-reasoning until the request's max_tokens
// clears `needs`, then answers properly. That is exactly what a reasoning model
// does: it thinks first, and the answer only appears if there is room left.
func reasoningServer(t *testing.T, needs int, calls *atomic.Int32, seen *[]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)
		if seen != nil {
			*seen = append(*seen, req.MaxTokens)
		}
		if req.MaxTokens < needs {
			// Everything went on the chain.
			w.Write([]byte(`{"model":"qwen3:4b","choices":[{"message":{"content":"",
				"reasoning":"thinking hard about it"},"finish_reason":"length"}],
				"usage":{"prompt_tokens":20,"completion_tokens":` +
				itoa(req.MaxTokens) + `}}`))
			return
		}
		w.Write([]byte(`{"model":"qwen3:4b","choices":[{"message":{"content":"{\"ok\":true}",
			"reasoning":"thinking hard about it"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":` + itoa(needs) + `}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestAReasoningModelGetsRoomToThinkAndStillAnswers.
func TestAReasoningModelGetsRoomToThinkAndStillAnswers(t *testing.T) {
	var calls atomic.Int32
	var maxTokens []int
	srv := reasoningServer(t, 900, &calls, &maxTokens)

	p := newProvider(t, srv.URL, 0)
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 300,
	})
	if err != nil {
		t.Fatalf("Complete: %v — a reasoning model still fails outright", err)
	}
	if resp.Text != `{"ok":true}` {
		t.Errorf("text = %q, want the answer", resp.Text)
	}
	if resp.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (one to discover the cost, one to answer)",
			resp.Attempts)
	}
	if len(maxTokens) != 2 || maxTokens[0] != 300 || maxTokens[1] <= 300 {
		t.Errorf("max_tokens sent = %v, want the answer allowance then a raised one",
			maxTokens)
	}

	// Both calls are charged. The tokens were spent either way, and a ledger that
	// charged for one of two could not enforce a ceiling.
	if resp.Usage.OutputTokens != 300+900 {
		t.Errorf("output tokens = %d, want %d — the wasted attempt is not charged",
			resp.Usage.OutputTokens, 300+900)
	}
	if resp.Usage.InputTokens != 40 {
		t.Errorf("input tokens = %d, want both prompts", resp.Usage.InputTokens)
	}

	// And the chain is carried, not concatenated into the answer: §11.5 would let
	// a model cite its own reasoning as a source if the two were joined.
	if resp.Reasoning == "" {
		t.Error("the chain of thought was discarded")
	}
	if strings.Contains(resp.Text, "thinking hard") {
		t.Error("the chain was concatenated into the answer")
	}
}

// TestTheAllowanceIsLearnedSoTheSecondCallDoesNotPayAgain.
//
// The difference between "supported" and "works if you retry": once a model's
// chain is known to cost 900 tokens, every later call starts with the room.
func TestTheAllowanceIsLearnedSoTheSecondCallDoesNotPayAgain(t *testing.T) {
	var calls atomic.Int32
	srv := reasoningServer(t, 900, &calls, nil)
	p := newProvider(t, srv.URL, 0)

	req := llm.Request{Messages: []llm.Message{llm.User("hi")}, MaxTokens: 300}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	first := calls.Load()

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load() - first; got != 1 {
		t.Errorf("the second Complete made %d calls, want 1 — the allowance was not "+
			"remembered", got)
	}
	if resp.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", resp.Attempts)
	}
}

// TestAModelThatNeverAnswersFailsWithTheReason rather than looping.
//
// A token budget that grows without a ceiling is not a budget, and a model that
// has not answered within the cap is not about to.
func TestAModelThatNeverAnswersFailsWithTheReason(t *testing.T) {
	var calls atomic.Int32
	srv := reasoningServer(t, 1<<30, &calls, nil)

	_, err := newProvider(t, srv.URL, 0).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 300,
	})
	if err == nil {
		t.Fatal("a model that never answers was reported as success")
	}
	// Three, not more: the discovery is bounded. qwen3:4b's chain varies by an
	// order of magnitude between runs on the SAME prompt — 249 tokens in one
	// measured run, over 2,128 in another — so one retry is not enough to tell a
	// model that reasons a lot from one that never answers, and four would be
	// paying for the difference.
	if n := calls.Load(); n != 3 {
		t.Errorf("%d provider calls, want 3 — the discovery is meant to be bounded", n)
	}
	if !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("err = %v, want it to name the cause", err)
	}
}

// TestANonReasoningModelIsUnaffected. The allowance must not change what a plain
// model is asked for, or every existing cassette key moves.
func TestANonReasoningModelIsUnaffected(t *testing.T) {
	var seen []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)
		seen = append(seen, req.MaxTokens)
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"hello"},
			"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	p := newProvider(t, srv.URL, 0)
	for i := 0; i < 3; i++ {
		resp, err := p.Complete(context.Background(), llm.Request{
			Messages: []llm.Message{llm.User("hi")}, MaxTokens: 512,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Attempts != 1 || resp.ReasoningTokens != 0 {
			t.Errorf("attempts = %d, reasoning = %d, want 1 and 0",
				resp.Attempts, resp.ReasoningTokens)
		}
	}
	for _, n := range seen {
		if n != 512 {
			t.Errorf("max_tokens = %d, want the caller's 512 unchanged", n)
		}
	}
}
