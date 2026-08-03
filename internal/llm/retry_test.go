package llm_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/llm"
)

func okBody(model string) string {
	return fmt.Sprintf(`{"model":%q,"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":2}}`, model)
}

func newProvider(t *testing.T, baseURL string, retries int) llm.Provider {
	t.Helper()
	p, err := llm.New(llm.Config{
		Kind:        llm.KindOpenAICompatible,
		BaseURL:     baseURL,
		StrongModel: "m", CheapModel: "m",
		MaxRetries: retries,
		Timeout:    10 * time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRateLimitIsRetried. The Anthropic backend gets retries from its SDK; this
// one is hand-rolled HTTP and had none, so cfg.MaxRetries — documented, and
// defaulting to 3 — was silently ignored. A single 429 from a free tier failed
// the whole call, which is exactly what happened on the first live run.
func TestRateLimitIsRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Write([]byte(okBody("m")))
	}))
	defer srv.Close()

	resp, err := newProvider(t, srv.URL, 3).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("a retryable 429 was not retried: %v", err)
	}
	if resp.Text != "hi" {
		t.Errorf("text = %q", resp.Text)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3", got)
	}
}

// TestRetryAfterIsHonoured. Guessing an interval is how a client that "retries"
// still fails: a provider asking for a wait longer than the backoff schedule
// refuses every attempt. Groq asked for 36 seconds.
func TestRetryAfterIsHonoured(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Fractional seconds: not permitted by the spec, but what Groq
			// sends, and plainly a wait.
			w.Header().Set("Retry-After", "0.25")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(okBody("m")))
	}))
	defer srv.Close()

	start := time.Now()
	if _, err := newProvider(t, srv.URL, 2).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 16,
	}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// The server asked for 250ms. A 1s exponential backoff would also "pass" a
	// naive test, so assert the wait matched the request rather than exceeding
	// some floor.
	if elapsed < 200*time.Millisecond {
		t.Errorf("retried after %v, ignoring the server's 250ms request", elapsed)
	}
	if elapsed > 900*time.Millisecond {
		t.Errorf("waited %v for a 250ms Retry-After — backoff overrode the server", elapsed)
	}
}

// TestNonRetryableFailsImmediately. A 413 fails identically however many times
// it is sent; retrying burns wall clock the budget is also counting.
func TestNonRetryableFailsImmediately(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusUnauthorized, http.StatusBadRequest} {
		var calls atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"nope"}}`))
		}))

		_, err := newProvider(t, srv.URL, 3).Complete(context.Background(), llm.Request{
			Messages: []llm.Message{llm.User("hi")}, MaxTokens: 16,
		})
		if err == nil {
			t.Errorf("status %d succeeded", status)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("status %d retried %d times; it cannot succeed", status, got)
		}
		srv.Close()
	}
}

// TestRetriesAreBoundedByMaxRetries so a permanently rate-limited provider
// cannot hold a worker forever.
func TestRetriesAreBoundedByMaxRetries(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL, 2).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("a permanently rate-limited provider eventually succeeded")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("made %d attempts, want 3 (MaxRetries=2 plus the first)", got)
	}
	// The caller needs the classified error, not the retry wrapper.
	if !llm.Retryable(err) {
		t.Errorf("final error lost its rate-limit classification: %v", err)
	}
}

// TestContextCancellationBeatsBackoff: a deadline must win over a long
// server-requested wait, and the message must say what we were waiting for.
func TestContextCancellationBeatsBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := newProvider(t, srv.URL, 3).Complete(ctx, llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %v; the deadline should have cut the 30s backoff short", d)
	}
	if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "rate") {
		t.Errorf("error does not say what we were waiting on: %v", err)
	}
}

// TestEmptyContentFromAReasoningModelIsDiagnosed. qwen3 and gemma4 emit a
// `reasoning` field that mole does not read, charged against the same output
// budget. Ask for too few tokens and the whole allowance goes to reasoning,
// leaving content empty with finish_reason "length" — a successful HTTP call
// that returned nothing.
//
// Measured on a real ollama: qwen3:4b at max_tokens=300 produced 300 completion
// tokens and an empty string. Without this the symptom surfaces as "no JSON
// object in model response", which sends whoever reads the log looking at the
// prompt instead of at MaxTokens.
func TestEmptyContentFromAReasoningModelIsDiagnosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"qwen3:4b","choices":[{"message":{"content":"","reasoning":"thinking..."},
			"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":300}}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL, 0).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 300,
	})
	if err == nil {
		t.Fatal("an empty completion was reported as success")
	}
	if !errors.Is(err, llm.ErrEmptyOutput) {
		t.Errorf("err = %v, want ErrEmptyOutput", err)
	}
	// The message has to name the cause, or it is no better than a parse error.
	for _, want := range []string{"300", "length", "MaxTokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestNormalEmptyResponseIsNotMisdiagnosed: a completion with no tokens at all
// is a different thing, and must not be reported as a reasoning overflow.
func TestNormalEmptyResponseIsNotMisdiagnosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":""},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":0}}`))
	}))
	defer srv.Close()

	_, err := newProvider(t, srv.URL, 0).Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.User("hi")}, MaxTokens: 300,
	})
	if errors.Is(err, llm.ErrEmptyOutput) {
		t.Errorf("a zero-token completion was diagnosed as a reasoning overflow: %v", err)
	}
}
