package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/pricing"
)

// RowMiner extracts schema-shaped rows from a passage (M9, §13).
//
// It is Miner with a different output shape, and that is the whole design. §13's
// dataset mode is "per-lead row extraction", which could have been a second pass
// over the same documents — one call for claims and another for rows. It is one
// call instead: the same fetch, the same chunking, the same model tier, the same
// ledger row, and the same §11.5 quote check.
//
// The quote check is why this is not a shortcut. A row is a claim with columns,
// so a row whose quote is not in the text it came from was not read there, and it
// dies exactly as a fabricated claim does. A CSV is more, not less, likely to be
// believed without checking — nobody reads a spreadsheet sceptically — so the
// standard cannot drop just because the output has a header line.
type RowMiner struct {
	LLM     llm.Provider
	Pricing *pricing.Table
	Log     *slog.Logger

	SessionID string
	Schema    dataset.Schema
}

// RowInput is one passage to extract from.
type RowInput struct {
	Lead      core.Lead
	SourceURL string
	Title     string
	Text      string
	Offset    int
	MaxRows   int
}

// RowOutput is what one passage yielded.
type RowOutput struct {
	Rows  []dataset.Row
	Usage llm.Usage
	// Call is the ledger row, recorded whether or not the call succeeded — the
	// tokens were spent either way.
	Call    core.ToolCall
	HasCall bool

	Proposed int
	// Rejected counts rows dropped because their quote was not in the passage,
	// or because they filled no key field. A rising rate is the signal that a
	// model has started inventing table contents, which is the failure mode this
	// output mode makes hardest to notice.
	Rejected int
}

const defaultMaxRows = 25

func (m *RowMiner) logger() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Mine extracts rows, keeping only those the passage supports.
func (m *RowMiner) Mine(ctx context.Context, in RowInput) (RowOutput, error) {
	var out RowOutput
	if in.MaxRows <= 0 {
		in.MaxRows = defaultMaxRows
	}

	prompt := rowUserPrompt(fenceToken(), in.Lead.Query, m.Schema, in.MaxRows,
		in.Title, in.SourceURL, in.Text)
	resp, err := m.LLM.Complete(ctx, llm.Request{
		Tier:      llm.TierCheap,
		System:    rowSystemPrompt,
		Messages:  []llm.Message{llm.User(prompt)},
		MaxTokens: 4096,
	})
	if resp == nil {
		return out, err
	}
	out.Usage = resp.Usage
	out.Call = toolCallFor(m.SessionID, in.Lead, resp, "rows:"+in.SourceURL, m.Pricing, m.Log, err)
	out.HasCall = true

	if err != nil {
		return out, err
	}
	if resp.Refused {
		return out, fmt.Errorf("actors: model refused (%s)", resp.RefusalCategory)
	}

	proposed, err := parseRows(resp.Text)
	if err != nil {
		return out, err
	}

	now := time.Now().UTC()
	for _, cand := range proposed {
		out.Proposed++

		row, ok := m.accept(ctx, in, cand)
		if !ok {
			out.Rejected++
			continue
		}
		row.RetrievedAt = now
		out.Rows = append(out.Rows, row)
		if len(out.Rows) >= in.MaxRows {
			break
		}
	}
	return out, nil
}

// accept applies §11.5 and the schema to one proposed row.
func (m *RowMiner) accept(ctx context.Context, in RowInput, cand minedRow) (dataset.Row, bool) {
	match, ok := FindQuote(in.Text, cand.Quote)
	if !ok {
		m.logger().DebugContext(ctx, "row rejected: quote not found in source",
			"source", in.SourceURL, "quote", truncateForLog(cand.Quote))
		return dataset.Row{}, false
	}

	values := map[string]string{}
	for _, f := range m.Schema.Fields {
		raw, present := cand.Values[f.Name]
		if !present {
			continue
		}
		v, ok := coerceField(f, raw)
		if !ok {
			// A value the field's type cannot hold is dropped rather than
			// carried as text: a number column holding "roughly $1.2m" merges
			// against nothing and renders as a value somebody will sort.
			continue
		}
		if v != "" {
			values[f.Name] = v
		}
	}

	// A row with no key field identifies nothing, so it can neither be merged nor
	// meaningfully reported — and a page that produced one was not answering the
	// question. Dropped and counted.
	var hasKey bool
	for _, k := range m.Schema.Keys() {
		if v, ok := values[k]; ok && strings.TrimSpace(v) != "" {
			hasKey = true
			break
		}
	}
	if !hasKey {
		m.logger().DebugContext(ctx, "row rejected: no key field",
			"source", in.SourceURL)
		return dataset.Row{}, false
	}

	return dataset.Row{
		Values:      values,
		Source:      in.SourceURL,
		Quote:       TruncateQuote(match.Text),
		QuoteOffset: int64(in.Offset + match.Offset),
		LeadID:      in.Lead.ID,
	}, true
}

// coerceField normalises a value to its declared type.
//
// Normalising here rather than at merge time is deliberate: the merge compares
// values, and "1,200,000" against "1200000" is a disagreement only if nobody
// looked at the type. Two sources that agree must not be recorded as
// contradicting each other over punctuation.
func coerceField(f dataset.Field, raw string) (string, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", true
	}
	switch f.Type {
	case dataset.TypeNumber:
		return coerceNumber(v)
	case dataset.TypeDate:
		return coerceDate(v)
	default:
		return v, true
	}
}

// coerceNumber accepts what a page actually writes and returns a bare number.
func coerceNumber(v string) (string, bool) {
	clean := strings.NewReplacer(",", "", " ", "", " ", "", "$", "", "£", "", "€", "",
		"%", "").Replace(v)
	// A leading + or a trailing minus for negatives ("1200-") is not worth
	// supporting; a parenthesised negative is, because accounts write it.
	if strings.HasPrefix(clean, "(") && strings.HasSuffix(clean, ")") {
		clean = "-" + strings.Trim(clean, "()")
	}
	// Magnitude suffixes, because a page writes "$1.2m" far more often than
	// "1200000" and dropping the row would lose the fact rather than the noise.
	mult := 1.0
	switch {
	case strings.HasSuffix(strings.ToLower(clean), "bn"):
		clean, mult = clean[:len(clean)-2], 1e9
	case strings.HasSuffix(strings.ToLower(clean), "m"):
		clean, mult = clean[:len(clean)-1], 1e6
	case strings.HasSuffix(strings.ToLower(clean), "k"):
		clean, mult = clean[:len(clean)-1], 1e3
	case strings.HasSuffix(strings.ToLower(clean), "b"):
		clean, mult = clean[:len(clean)-1], 1e9
	}
	n, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return "", false
	}
	n *= mult
	if n == float64(int64(n)) && n < 1e15 && n > -1e15 {
		return strconv.FormatInt(int64(n), 10), true
	}
	return strconv.FormatFloat(n, 'f', -1, 64), true
}

// coerceDate normalises to an ISO date, or to a bare year when that is all the
// source gave.
//
// A year is a legitimate answer — "founded 1998" is what most pages say — so
// demanding a full date would drop the commonest form of the commonest date
// field.
func coerceDate(v string) (string, bool) {
	for _, layout := range []string{
		"2006-01-02", "2006/01/02", "02/01/2006", "01/02/2006",
		"2 January 2006", "January 2, 2006", "2 Jan 2006", "Jan 2, 2006",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Format("2006-01-02"), true
		}
	}
	if len(v) == 7 {
		if t, err := time.Parse("2006-01", v); err == nil {
			return t.Format("2006-01"), true
		}
	}
	if len(v) == 4 {
		if _, err := strconv.Atoi(v); err == nil {
			return v, true
		}
	}
	return "", false
}

// -----------------------------------------------------------------------------
// The reply
// -----------------------------------------------------------------------------

type minedRow struct {
	Values map[string]string `json:"values"`
	Quote  string            `json:"quote"`
}

type rowResponse struct {
	Rows []minedRow `json:"rows"`
}

// parseRows reads the reply, tolerating the wrapping a model adds and nothing
// about the content.
func parseRows(raw string) ([]minedRow, error) {
	body := extractJSONObject(raw)
	if body == "" {
		return nil, fmt.Errorf("actors: no row list in the model's reply: %s",
			truncateForLog(raw))
	}
	var resp rowResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return nil, fmt.Errorf("actors: parse rows: %w; reply was: %s",
			err, truncateForLog(body))
	}
	return resp.Rows, nil
}

// extractJSONObject finds the outermost object that parses.
//
// The same lenience the coderunner needed for the same reason: a model told to
// print JSON prints a sentence and then JSON often enough that refusing would be
// a fight rather than a boundary.
func extractJSONObject(raw string) string {
	for start := 0; start < len(raw); start++ {
		if raw[start] != '{' {
			continue
		}
		for end := len(raw) - 1; end > start; end-- {
			if raw[end] != '}' {
				continue
			}
			if candidate := raw[start : end+1]; json.Valid([]byte(candidate)) {
				return candidate
			}
		}
	}
	return ""
}
