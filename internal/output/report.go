// Package output turns a finished session's claims into a report (§13).
//
// Paid from escrow. §8.3 holds a slice of the budget back at session creation
// precisely so this step is affordable after the research has spent everything
// it was allowed to — a run that produced good claims and cannot afford to
// write them up has wasted the whole budget, not just the last call.
//
// Every claim reaching this point has already had its quote verified against
// the source it came from (§11.5). What the model does here is arrange them; it
// is not asked to judge them, and it cannot introduce a citation, because the
// citation markers are assigned mechanically before the prompt is built.
package output

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/store"
)

// Citation is one numbered source.
type Citation struct {
	N      int
	Source string
	// Quotes are the verified spans supporting the claims cited to this
	// source, so a reader can check the report without re-fetching.
	Quotes []string
	// PublishedAt is the earliest publication date seen for the source, when
	// one was recoverable. §13 asks for it per citation: a 2024 preprint and a
	// 2026 paper carry different weight and the reader has to see which.
	PublishedAt *time.Time
}

// Report is a rendered answer.
type Report struct {
	SessionID string
	Question  string
	Body      string
	Citations []Citation

	// Cost is what generating it charged.
	Cost core.Cost
	// Model names what wrote it.
	Model string

	// Degraded explains why the report is thin, when it is. Surfaced rather
	// than left for a reader to infer from a short answer.
	Degraded string
}

// Generator writes reports.
type Generator struct {
	LLM llm.Provider

	// MaxClaims bounds what reaches the prompt. A session can produce hundreds
	// of claims; the report is paid from a fixed escrow, so the input has to be
	// bounded by something other than optimism.
	MaxClaims int
	// MaxTokens bounds the response.
	MaxTokens int
}

const (
	DefaultMaxClaims = 60
	DefaultMaxTokens = 3000
)

func (g *Generator) withDefaults() *Generator {
	out := *g
	if out.MaxClaims <= 0 {
		out.MaxClaims = DefaultMaxClaims
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}
	return &out
}

// Generate writes a report for a finished session.
func (g *Generator) Generate(ctx context.Context, st store.Store, sessionID string) (*Report, error) {
	gen := g.withDefaults()

	var (
		sess   *core.Session
		claims []*core.Claim
	)
	if err := st.Read(ctx, func(ctx context.Context, q store.Queries) error {
		var err error
		if sess, err = q.GetSession(ctx, sessionID); err != nil {
			return err
		}
		claims, err = q.ListClaims(ctx, sessionID, 10_000)
		return err
	}); err != nil {
		return nil, err
	}

	rep := &Report{SessionID: sessionID, Question: sess.Prompt}

	if len(claims) == 0 {
		// No model call: there is nothing to synthesize, and spending escrow to
		// have a model say so is worse than saying it here.
		rep.Body = fmt.Sprintf("No verifiable evidence was found for %q.", sess.Prompt)
		rep.Degraded = "no claims survived quote verification"
		return rep, nil
	}

	kept := claims
	if len(kept) > gen.MaxClaims {
		// Keep the most confident. Truncating arbitrarily would drop evidence
		// the pipeline rated highest.
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].Confidence > kept[j].Confidence })
		kept = kept[:gen.MaxClaims]
		rep.Degraded = fmt.Sprintf("%d of %d claims included; the rest did not fit the report budget",
			gen.MaxClaims, len(claims))
	}

	rep.Citations = numberSources(kept)
	index := map[string]int{}
	for _, c := range rep.Citations {
		index[c.Source] = c.N
	}

	fence := fenceToken()
	resp, err := gen.LLM.Complete(ctx, llm.Request{
		Tier:      llm.TierStrong,
		System:    reportSystemPrompt,
		Messages:  []llm.Message{llm.User(reportPrompt(fence, sess.Prompt, kept, index))},
		MaxTokens: gen.MaxTokens,
	})
	if resp != nil {
		rep.Model = resp.Model
		rep.Cost = core.Cost{
			InputTokens:      resp.Usage.InputTokens,
			OutputTokens:     resp.Usage.OutputTokens,
			CacheReadTokens:  resp.Usage.CacheReadTokens,
			CacheWriteTokens: resp.Usage.CacheWriteTokens,
		}
	}
	if err != nil {
		// The claims are still worth returning. A failed synthesis should cost
		// the prose, not the evidence.
		rep.Body = fallbackBody(sess.Prompt, kept, index)
		rep.Degraded = "synthesis failed: " + err.Error()
		return rep, nil
	}
	if resp.Refused {
		rep.Body = fallbackBody(sess.Prompt, kept, index)
		rep.Degraded = "model refused to synthesize (" + resp.RefusalCategory + ")"
		return rep, nil
	}

	rep.Body = strings.TrimSpace(resp.Text)
	if rep.Body == "" {
		rep.Body = fallbackBody(sess.Prompt, kept, index)
		rep.Degraded = "synthesis returned nothing"
	}
	return rep, nil
}

// numberSources assigns each distinct source a citation number in first-seen
// order, and collects the verified quotes and earliest date per source.
//
// Assigned mechanically, before the prompt is built, so the model cannot invent
// a citation: every [n] it can legitimately write already maps to a real
// source, and any other number is detectable.
func numberSources(claims []*core.Claim) []Citation {
	var out []Citation
	index := map[string]int{}

	for _, c := range claims {
		n, seen := index[c.Source]
		if !seen {
			out = append(out, Citation{N: len(out) + 1, Source: c.Source})
			n = len(out)
			index[c.Source] = n
		}
		cit := &out[n-1]
		if q := strings.TrimSpace(c.Quote); q != "" && len(cit.Quotes) < 3 {
			cit.Quotes = append(cit.Quotes, q)
		}
		if c.PublishedAt != nil && (cit.PublishedAt == nil || c.PublishedAt.Before(*cit.PublishedAt)) {
			cit.PublishedAt = c.PublishedAt
		}
	}
	return out
}

// fallbackBody lists the claims without synthesis.
//
// Used when the model call fails. Verified evidence with citations is a
// genuinely useful answer — less readable than prose, but not less true — and
// it is what the escrow already paid to collect.
func fallbackBody(question string, claims []*core.Claim, index map[string]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Evidence gathered for %q, unsynthesized:\n\n", question)
	for _, c := range claims {
		fmt.Fprintf(&b, "- %s [%d]\n", strings.TrimSpace(c.Text), index[c.Source])
	}
	return b.String()
}

// Markdown renders the report with its source list.
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString(r.Body)
	b.WriteString("\n")

	if len(r.Citations) > 0 {
		b.WriteString("\n## Sources\n\n")
		for _, c := range r.Citations {
			fmt.Fprintf(&b, "[%d] %s", c.N, c.Source)
			if c.PublishedAt != nil {
				fmt.Fprintf(&b, " (%s)", c.PublishedAt.Format("2006-01-02"))
			}
			b.WriteString("\n")
			// The verified span, so a reader can check the citation without
			// re-fetching. This is the payoff of §11.5 being enforced upstream.
			for _, q := range c.Quotes {
				fmt.Fprintf(&b, "    > %s\n", truncate(q, 200))
			}
			b.WriteString("\n")
		}
	}

	if r.Degraded != "" {
		fmt.Fprintf(&b, "---\n\n_Report is incomplete: %s._\n", r.Degraded)
	}
	return b.String()
}

func truncate(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

func fenceToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
