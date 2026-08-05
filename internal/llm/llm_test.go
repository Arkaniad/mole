package llm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lajosdeme/mole/internal/llm"
)

// TestTheConfiguredTimeoutIsHonoured.
//
// Ten minutes was hardcoded, which is generous for an API and reachable for a local model:
// a 12B on an integrated GPU, adjudicating eight claim pairs in one call after ollama had
// just reloaded 8.4GB of weights, exceeded it and the batch was skipped. A ceiling that
// cannot be raised turns a slow model into a broken one — so the field has to actually
// reach the client rather than being stored and ignored.
func TestTheConfiguredTimeoutIsHonoured(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	p, err := llm.New(llm.Config{
		Kind:        llm.KindOpenAICompatible,
		BaseURL:     slow.URL,
		StrongModel: "test-model",
		APIKey:      "k",
		Timeout:     150 * time.Millisecond,
		MaxRetries:  -1, // no retries, or the deadline is measured across all of them
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = p.Complete(context.Background(), llm.Request{
		Tier:     llm.TierStrong,
		Messages: []llm.Message{llm.User("hello")},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a 150ms timeout against a 3s server returned no error")
	}
	// Generous upper bound: the point is that it gave up long before the server would
	// have answered, not that the deadline is precise.
	if elapsed > 2*time.Second {
		t.Errorf("gave up after %v; the configured timeout was ignored", elapsed)
	}
}
