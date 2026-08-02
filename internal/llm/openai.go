package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OpenAI-compatible backend.
//
// Covers DeepSeek, Ollama, llama.cpp, vLLM, LiteLLM, Together, and anything
// else speaking /v1/chat/completions. Written against the wire format rather
// than an SDK: the format is small and stable, and every vendor SDK layers its
// own auth and retry behaviour on top of the same three fields.
//
// This path is what makes token-mode budgeting honest for self-hosted users. A
// local model registered at zero rates costs nothing and still reports tokens,
// so the ceiling means something even when no money is involved.

const openAIDefaultBaseURL = "https://api.openai.com/v1"

type openAIProvider struct {
	cfg    Config
	client *http.Client
	source CredentialSource
}

func newOpenAICompatible(cfg Config, httpClient *http.Client) (*openAIProvider, error) {
	if cfg.StrongModel == "" {
		// Unlike Anthropic there is no knowable default here — the endpoint
		// might be DeepSeek, a local Qwen, or a proxy in front of five models.
		// Guessing would produce a 404 at the first call instead of a clear
		// error now.
		return nil, fmt.Errorf("llm: openai-compatible backend needs a model (set llm.model)")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = openAIDefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")

	source := CredentialConfig
	if cfg.APIKey == "" {
		// Local runtimes accept any key, or none. That is not a
		// misconfiguration — it is the normal case for Ollama.
		source = CredentialNotNeeded
	}

	return &openAIProvider{cfg: cfg, client: httpClient, source: source}, nil
}

func (p *openAIProvider) Name() string                       { return string(KindOpenAICompatible) }
func (p *openAIProvider) CredentialSource() CredentialSource { return p.source }
func (p *openAIProvider) BaseURL() string                    { return p.cfg.BaseURL }

func (p *openAIProvider) ModelFor(tier Tier) string {
	if tier == TierCheap && p.cfg.CheapModel != "" {
		return p.cfg.CheapModel
	}
	return p.cfg.StrongModel
}

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Stream    bool            `json:"stream"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// Reasoning models return their chain separately; it is not part
			// of the answer and is deliberately not concatenated into Text.
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		// DeepSeek and some proxies report cache hits under this name.
		PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
		PromptTokensDetails   struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// do issues the request, retrying transient failures.
//
// The Anthropic backend gets retries from its SDK; this one is hand-rolled
// HTTP and had none, so cfg.MaxRetries — a documented field defaulting to 3 —
// was silently ignored for every OpenAI-compatible provider. A single 429 from
// a free tier failed the whole call.
//
// Retry-After is honoured when the server sends it. Guessing an interval is how
// a client that "retries" still fails: a provider asking for 36 seconds will
// refuse three exponential backoffs totalling four.
func (p *openAIProvider) do(ctx context.Context, payload []byte) ([]byte, error) {
	attempts := p.cfg.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			t := time.NewTimer(retryDelay(attempt, lastErr))
			select {
			case <-ctx.Done():
				t.Stop()
				// Report why we gave up: "context deadline exceeded" alone
				// hides that a provider asked for a wait we could not afford.
				return nil, fmt.Errorf("llm: %w (last: %v)", ctx.Err(), lastErr)
			case <-t.C:
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			p.cfg.BaseURL+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("llm: request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if p.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}

		resp, err := p.client.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("llm: %s: %w", p.cfg.BaseURL, err)
			if ctx.Err() != nil {
				return nil, lastErr
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("llm: read: %w", readErr)
			continue
		}

		statusErr := classifyStatus(string(KindOpenAICompatible), resp.StatusCode, string(body))
		if statusErr == nil {
			return body, nil
		}
		if !Retryable(statusErr) {
			// 413 and 401 fail identically however many times they are sent.
			// Returning now keeps a doomed request from burning wall clock the
			// budget is also counting.
			return nil, statusErr
		}
		lastErr = &retryableErr{err: statusErr, after: retryAfter, hasAfter: hasRetryAfter}
	}

	var re *retryableErr
	if errors.As(lastErr, &re) {
		return nil, re.err
	}
	return nil, lastErr
}

// retryableErr carries a server-supplied wait alongside the error.
//
// hasAfter distinguishes "the server said zero" from "the server said nothing".
// They are different instructions: the first means retry now, and treating it
// as absent turns an immediate retry into a second of backoff for no reason.
type retryableErr struct {
	err      error
	after    time.Duration
	hasAfter bool
}

func (e *retryableErr) Error() string { return e.err.Error() }
func (e *retryableErr) Unwrap() error { return e.err }

// retryDelay is the server's requested wait when it gave one, else exponential
// backoff.
func retryDelay(attempt int, lastErr error) time.Duration {
	var re *retryableErr
	if errors.As(lastErr, &re) && re.hasAfter {
		if re.after > 60*time.Second {
			// A wait longer than this is the provider saying "not today".
			// Honour the cap and let the attempt fail rather than parking a
			// worker; the lead's wall clock is a ceiling too.
			return 60 * time.Second
		}
		return re.after
	}
	d := time.Duration(1<<uint(attempt-1)) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// parseRetryAfter reads the header in both permitted forms: seconds, or an HTTP
// date. Providers also send fractional seconds ("36.48"), which the spec does
// not allow but which is plainly a wait in seconds.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true // a date in the past means retry now
	}
	return 0, false
}

func (p *openAIProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()

	model := req.Model
	if model == "" {
		model = p.ModelFor(req.Tier)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	msgs := make([]openAIMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, openAIMessage{Role: string(m.Role), Content: m.Text})
	}

	payload, err := json.Marshal(openAIRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Stream:    false,
	})
	if err != nil {
		return nil, fmt.Errorf("llm: encode: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	body, err := p.do(ctx, payload)
	if err != nil {
		return nil, err
	}

	var parsed openAIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("llm: %s returned no choices", p.cfg.BaseURL)
	}

	choice := parsed.Choices[0]

	// Cache accounting varies by vendor. DeepSeek splits hit/miss; the OpenAI
	// shape nests cached_tokens. Both mean the same thing, and getting it
	// wrong double-counts cached input as fresh input in the ledger.
	cacheRead := parsed.Usage.PromptCacheHitTokens
	if cacheRead == 0 {
		cacheRead = parsed.Usage.PromptTokensDetails.CachedTokens
	}
	inputTokens := parsed.Usage.PromptTokens
	if cacheRead > 0 && inputTokens >= cacheRead {
		// prompt_tokens is the total including cached; subtract so the two
		// fields do not overlap when the ledger sums them.
		inputTokens -= cacheRead
	}

	out := &Response{
		Text:       choice.Message.Content,
		Model:      parsed.Model,
		StopReason: choice.FinishReason,
		Elapsed:    time.Since(start),
		Usage: Usage{
			InputTokens:     inputTokens,
			OutputTokens:    parsed.Usage.CompletionTokens,
			CacheReadTokens: cacheRead,
		},
	}
	if out.Model == "" {
		out.Model = model
	}

	if out.Usage.IsZero() && out.Text != "" {
		// Some local runtimes omit usage entirely. That would charge zero and
		// make the ceiling unenforceable, so it surfaces rather than passing.
		return out, ErrNoUsageReported
	}
	return out, nil
}
