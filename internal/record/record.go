// Package record is the record/replay cassette layer.
//
// Every outbound call — search, fetch, academic, LLM — goes through this
// package. On first run it records request/response pairs to disk; thereafter
// it replays them. Three consequences, all of which the project needs before
// the first actor is written:
//
//   - Orchestrator and eval tests are deterministic.
//   - They run in CI without network access.
//   - They cost nothing, so the eval harness can run on every commit.
//
// Building this after the actors exist means auditing every call site to thread
// a transport through, which is why it belongs in M0 rather than M2.
package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type Mode string

const (
	// ModeOff passes everything through untouched.
	ModeOff Mode = "off"
	// ModeRecord always hits the network and overwrites stored interactions.
	ModeRecord Mode = "record"
	// ModeReplay never hits the network; a miss is an error.
	ModeReplay Mode = "replay"
	// ModeAuto replays what exists and records what does not. Convenient
	// locally, but do not use it in CI: a cassette miss would silently make a
	// real network call and a real charge.
	ModeAuto Mode = "auto"
)

// ErrCassetteMiss is returned in ModeReplay when no interaction matches.
type ErrCassetteMiss struct {
	Key     string
	Summary string
}

func (e *ErrCassetteMiss) Error() string {
	return fmt.Sprintf("record: no recorded interaction for %s (key %s)", e.Summary, e.Key)
}

// Interaction is one recorded request/response pair.
type Interaction struct {
	Key      string       `json:"key"`
	Request  ReqSnapshot  `json:"request"`
	Response RespSnapshot `json:"response"`
}

type ReqSnapshot struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
}

type RespSnapshot struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"`
}

// ---------------------------------------------------------------------------
// Redaction
// ---------------------------------------------------------------------------

// redactedHeaders never reach disk. A cassette is a normal file that gets
// committed to a repository and shared; an API key written into one is a leaked
// credential, so redaction happens at the write boundary rather than being left
// to reviewer discipline.
var redactedHeaders = map[string]bool{
	"authorization":        true,
	"x-api-key":            true,
	"anthropic-api-key":    true,
	"openai-api-key":       true,
	"x-subscription-token": true, // Brave Search
	"cookie":               true,
	"set-cookie":           true,
	"proxy-authorization":  true,
	"x-auth-token":         true,
}

// redactedSubstrings catch credential headers the list above does not name.
//
// The list alone already failed once: Brave authenticates with
// X-Subscription-Token, which nothing here matched, so the key went to disk
// verbatim in a world-readable file. An allowlist of known vendors is the wrong
// default for a function whose failure mode is a leaked credential — every new
// provider is a chance to forget. Over-redacting a header costs a slightly less
// informative cassette.
var redactedSubstrings = []string{
	"api-key", "apikey", "api_key",
	"token", "secret", "auth", "password", "credential", "signature",
}

func isRedacted(name string) bool {
	name = strings.ToLower(name)
	if redactedHeaders[name] {
		return true
	}
	for _, s := range redactedSubstrings {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}

const redactedValue = "REDACTED"

// maxRecordedBody bounds what a single interaction writes to disk. Without it a
// misbehaving or hostile endpoint decides how much memory the recorder uses.
const maxRecordedBody = 32 << 20

// redactedQueryParams are stripped from the stored URL. Several providers take
// the key as a query parameter, which would otherwise land in the cassette and
// in the match key.
var redactedQueryParams = map[string]bool{
	"api_key":      true,
	"apikey":       true,
	"key":          true,
	"access_token": true,
	"token":        true,
}

func redactHeaders(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, v := range h {
		if isRedacted(k) {
			out[k] = []string{redactedValue}
			continue
		}
		out[k] = append([]string(nil), v...)
	}
	return out
}

// canonicalURL normalizes a URL for matching and storage: lowercased scheme and
// host, secrets stripped, query parameters sorted so that map iteration order
// in a client cannot change the cassette key.
func canonicalURL(u *url.URL) string {
	c := *u
	c.Scheme = strings.ToLower(c.Scheme)
	c.Host = strings.ToLower(c.Host)
	c.Fragment = ""
	// https://user:pass@host/… is a credential in the URL, and it would
	// otherwise be written into both the stored request and the match key.
	c.User = nil

	q := c.Query()
	for k := range q {
		if redactedQueryParams[strings.ToLower(k)] {
			q.Set(k, redactedValue)
		}
	}
	// url.Values.Encode sorts keys, which is what makes this deterministic.
	c.RawQuery = q.Encode()
	return c.String()
}

// ---------------------------------------------------------------------------
// Cassette
// ---------------------------------------------------------------------------

// Cassette is a set of interactions backed by one JSON file.
type Cassette struct {
	mu           sync.Mutex
	path         string
	interactions map[string]*Interaction
	dirty        bool
}

// LoadCassette reads a cassette, returning an empty one if the file is absent.
func LoadCassette(path string) (*Cassette, error) {
	c := &Cassette{path: path, interactions: map[string]*Interaction{}}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("record: read cassette %s: %w", path, err)
	}

	var list []*Interaction
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("record: parse cassette %s: %w", path, err)
	}
	// Re-key from the stored request rather than trusting the recorded Key field.
	//
	// The key function is allowed to change — masking prompt fences changed it, and
	// something else will. Indexing by the stored key would make every existing
	// cassette silently unmatchable on the day that happens, and the symptom (a run
	// that replays nothing) looks like a broken recording rather than a changed
	// hash. Recomputing costs one pass over a file that is already in memory.
	for _, in := range list {
		key := in.Key
		if u, err := url.Parse(in.Request.URL); err == nil {
			key = RequestKey(in.Request.Method, u, []byte(in.Request.Body))
		}
		in.Key = key
		c.interactions[key] = in
	}
	return c, nil
}

// Save writes the cassette if anything changed. Interactions are sorted by key
// so the file has a stable diff.
func (c *Cassette) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.dirty {
		return nil
	}
	list := make([]*Interaction, 0, len(c.interactions))
	for _, in := range c.interactions {
		list = append(list, in)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })

	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Write-then-rename so an interrupted save cannot truncate a good cassette.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("record: write cassette: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("record: commit cassette: %w", err)
	}
	c.dirty = false
	return nil
}

func (c *Cassette) get(key string) (*Interaction, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	in, ok := c.interactions[key]
	return in, ok
}

func (c *Cassette) put(in *Interaction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.interactions[in.Key] = in
	c.dirty = true
}

// Len reports how many interactions are stored.
func (c *Cassette) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.interactions)
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// Transport is an http.RoundTripper that records or replays. One transport
// covers search, fetch, academic providers, and any LLM SDK that accepts a
// custom *http.Client — which is most of the outbound surface.
type Transport struct {
	Mode     Mode
	Cassette *Cassette
	Base     http.RoundTripper
}

// NewTransport wraps base. A nil base uses http.DefaultTransport.
func NewTransport(mode Mode, cassette *Cassette, base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{Mode: mode, Cassette: cassette, Base: base}
}

// Client returns an *http.Client using this transport.
func (t *Transport) Client() *http.Client { return &http.Client{Transport: t} }

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.Mode == ModeOff || t.Cassette == nil {
		return t.Base.RoundTrip(req)
	}

	body, err := drainBody(req)
	if err != nil {
		return nil, err
	}
	key := RequestKey(req.Method, req.URL, body)

	if t.Mode == ModeReplay || t.Mode == ModeAuto {
		if in, ok := t.Cassette.get(key); ok {
			return buildResponse(req, in.Response), nil
		}
		if t.Mode == ModeReplay {
			return nil, &ErrCassetteMiss{
				Key:     key,
				Summary: req.Method + " " + canonicalURL(req.URL),
			}
		}
	}

	resp, err := t.Base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	respBody, err := readCapped(resp.Body, "response")
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, fmt.Errorf("record: close response body: %w", closeErr)
	}

	t.Cassette.put(&Interaction{
		Key: key,
		Request: ReqSnapshot{
			Method:  req.Method,
			URL:     canonicalURL(req.URL),
			Headers: redactHeaders(req.Header),
			Body:    string(body),
		},
		Response: RespSnapshot{
			Status:  resp.StatusCode,
			Headers: redactHeaders(resp.Header),
			Body:    string(respBody),
		},
	})

	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	return resp, nil
}

// RequestKey derives the cassette key. It deliberately excludes headers:
// matching on them would make every recording depend on the exact SDK version,
// user-agent, and auth scheme in use when it was made.
func RequestKey(method string, u *url.URL, body []byte) string {
	h := sha256.New()
	h.Write([]byte(strings.ToUpper(method)))
	h.Write([]byte{0})
	h.Write([]byte(canonicalURL(u)))
	h.Write([]byte{0})
	h.Write(maskFences(canonicalJSON(body)))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// fencePattern matches a §3.2 prompt fence: a tag whose name ends in a 16-hex
// nonce, as produced by core.PromptFence.
//
// Both bracket encodings, because the body being keyed has been through
// encoding/json, which HTML-escapes `<` and `>` into < and > by default.
// A pattern written against literal angle brackets matches a hand-written test
// fixture and nothing that ever crosses the wire — which is exactly the shape of
// vacuous test this project keeps finding, so the replacement normalizes the
// bracket form too rather than preserving whichever one it matched.
var fencePattern = regexp.MustCompile(
	`(?:<|\\u003c)(/?)([A-Za-z][A-Za-z0-9_]*)-[0-9a-f]{16}(?:>|\\u003e)`)

// maskFences replaces prompt-fence nonces with a constant, for keying only.
//
// Without this, replay cannot work at all. Every prompt that wraps untrusted text
// names its delimiter with a fresh crypto/rand nonce — that is the whole point of
// §3.2, since a fixed delimiter is one a page can imitate — so every request body
// is unique per run, and a cassette recorded on Monday cannot match the identical
// request made on Tuesday. The very first planner call misses and the run produces
// nothing, which is exactly what `mole corpus` did in replay.
//
// The nonce is masked in the KEY only. The prompt actually sent to the model still
// carries a fresh random fence, and the recorded request body still shows the real
// one, so nothing about the injection defence changes — two requests that differ
// only by fence are the same logical request, and this is what says so.
//
// Deliberately narrow: a tag name, a hyphen, exactly sixteen lowercase hex digits,
// a closing bracket. Masking bare hex runs would collide two genuinely different
// requests whose page content happened to contain a hash, and in a replay harness a
// collision returns the wrong recorded response — a far worse failure than a miss.
func maskFences(body []byte) []byte {
	return fencePattern.ReplaceAll(body, []byte("<${1}${2}-FENCE>"))
}

// canonicalJSON re-serializes a JSON body with sorted keys so that map
// iteration order in a client does not produce a different key for an identical
// request. Non-JSON bodies pass through unchanged.
func canonicalJSON(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return body
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return body
	}
	// encoding/json sorts map keys on marshal.
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// readCapped reads at most maxRecordedBody bytes and fails rather than
// truncating. A silently short cassette entry replays as a corrupt response,
// which is a far worse failure than refusing to record an oversized one.
func readCapped(r io.Reader, what string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxRecordedBody+1))
	if err != nil {
		return nil, fmt.Errorf("record: read %s body: %w", what, err)
	}
	if len(b) > maxRecordedBody {
		return nil, fmt.Errorf("record: %s body exceeds %d bytes", what, maxRecordedBody)
	}
	return b, nil
}

func drainBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	b, err := readCapped(req.Body, "request")
	if err != nil {
		return nil, err
	}
	if err := req.Body.Close(); err != nil {
		return nil, fmt.Errorf("record: close request body: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(b))
	return b, nil
}

func buildResponse(req *http.Request, snap RespSnapshot) *http.Response {
	h := http.Header{}
	for k, vs := range snap.Headers {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	body := []byte(snap.Body)
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", snap.Status, http.StatusText(snap.Status)),
		StatusCode:    snap.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
