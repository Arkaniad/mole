package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lajosdeme/mole/internal/core"
	"github.com/lajosdeme/mole/internal/dataset"
	"github.com/lajosdeme/mole/internal/llm"
	"github.com/lajosdeme/mole/internal/llm/jsonish"
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
	// Coerced counts VALUES dropped because the field's declared type could not
	// hold them — a `number` field given "roughly $1.2m".
	//
	// Counted because it was not: a row could arrive with four fields, lose three
	// to coercion, keep its key, and be reported as an accepted row. The dataset
	// then had empty cells with no number anywhere saying why, and the honest
	// reading — the schema's types do not match what the sources write — was
	// invisible. Rejected rows had a counter; silently emptied ones did not.
	Coerced int
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

		row, coerced, ok := m.accept(ctx, in, cand)
		out.Coerced += coerced
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
// The int is how many values the field types could not hold — dropped, and
// counted so the caller can report it.
func (m *RowMiner) accept(ctx context.Context, in RowInput, cand minedRow) (dataset.Row, int, bool) {
	match, ok := FindQuote(in.Text, cand.Quote)
	if !ok {
		m.logger().DebugContext(ctx, "row rejected: quote not found in source",
			"source", in.SourceURL, "quote", truncateForLog(cand.Quote))
		return dataset.Row{}, 0, false
	}

	var coerced int
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
			coerced++
			m.logger().DebugContext(ctx, "value dropped: not a "+string(f.Type),
				"source", in.SourceURL, "field", f.Name, "value", truncateForLog(raw))
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
		return dataset.Row{}, coerced, false
	}

	return dataset.Row{
		Values:      values,
		Source:      in.SourceURL,
		Quote:       TruncateQuote(match.Text),
		QuoteOffset: int64(in.Offset + match.Offset),
		LeadID:      in.Lead.ID,
	}, coerced, true
}

// CoerceNumberForTest exposes the number coercion.
//
// Exported for the tests that pin scaled figures exactly. The rule they check —
// "£32.7 billion" is 32700000000 and not 32700000000.000004 — is arithmetic that
// a live run got wrong, and testing it through Mine would need a model reply per
// case for no extra coverage.
func CoerceNumberForTest(v string) (string, bool) { return coerceNumber(v) }

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
//
// Three rules here are repairs, each measured through the whole accept path.
//
// A comma is not always a thousands separator. Stripping every comma turned the
// German and French "1,5" into 15 — a silent factor of ten, and worse than a
// drop, because the merge then records the wrong figure as a DISAGREEMENT with
// the correct source and Cell.Others makes it look adjudicated.
//
// A magnitude suffix is only a magnitude when it is attached. "500 m" is five
// hundred metres and became five hundred million, because the spaces were
// removed before the suffix was looked for.
//
// ParseFloat accepts "NaN", "Infinity", "-Inf" and hexadecimal floats. None of
// those is a figure a page states, and +Inf in a number column poisons every
// moment computed over it afterwards.
func coerceNumber(v string) (string, bool) {
	v = strings.TrimSpace(v)

	// A magnitude spelled out in WORDS counts whether or not it is attached.
	//
	// The attached-only rule below exists because "5 m" might be metres or minutes,
	// and that ambiguity is real for a single letter. It does not exist for a word:
	// nobody writes "32.7 billion" meaning metres. Refusing them cost real data —
	// "£32.7 billion" is how a page states a revenue, and a live run dropped the
	// value rather than reading it.
	var spelled int
	lowerV := strings.ToLower(v)
	for word, places := range map[string]int{
		"trillion": 12, "billion": 9, "million": 6, "thousand": 3,
	} {
		if rest, ok := strings.CutSuffix(lowerV, word); ok {
			spelled = places
			v = strings.TrimSpace(v[:len(rest)])
			break
		}
	}

	// Attached or not, decided before any whitespace is touched.
	attached := !strings.ContainsAny(v, " \u00a0")

	clean := strings.NewReplacer(" ", "", "\u00a0", "", "$", "", "£", "", "€", "",
		"%", "").Replace(v)

	// A parenthesised negative, which is how accounts write a loss.
	if strings.HasPrefix(clean, "(") && strings.HasSuffix(clean, ")") {
		clean = "-" + strings.Trim(clean, "()")
	}

	// The magnitude is a count of DECIMAL PLACES to shift, not a float to multiply
	// by.
	//
	// Multiplying was the obvious version and it is wrong for the commonest input
	// there is. "£32.7 billion" became 32.7 × 1e9, which in float64 is
	// 32700000000.000004 — not equal to its own truncation, so the integer path
	// below was skipped and a live run put `32700000000.000004` in a revenue column
	// somebody was going to sum. Shifting the digits is exact for every input a page
	// states, because a page states decimal digits.
	shift := spelled
	if attached && shift == 0 {
		lower := strings.ToLower(clean)
		switch {
		case strings.HasSuffix(lower, "bn"):
			clean, shift = clean[:len(clean)-2], 9
		case strings.HasSuffix(lower, "m"):
			clean, shift = clean[:len(clean)-1], 6
		case strings.HasSuffix(lower, "k"):
			clean, shift = clean[:len(clean)-1], 3
		case strings.HasSuffix(lower, "b"):
			clean, shift = clean[:len(clean)-1], 9
		}
	}

	clean, ok := normaliseSeparators(clean)
	if !ok {
		return "", false
	}

	n, err := strconv.ParseFloat(clean, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return "", false
	}
	// Hexadecimal float literals ("0x1p10") parse and are not a figure any page
	// states about a company.
	if strings.ContainsAny(clean, "xX") {
		return "", false
	}
	if shift > 0 {
		if scaled, ok := shiftDecimal(clean, shift); ok {
			return scaled, true
		}
		// Unshiftable shapes (an exponent, say) fall back to the float path, which
		// is imprecise but better than dropping a stated figure.
		n *= math.Pow(10, float64(shift))
	}
	if n == math.Trunc(n) && math.Abs(n) < 1e15 {
		return strconv.FormatInt(int64(n), 10), true
	}
	return strconv.FormatFloat(n, 'f', -1, 64), true
}

// shiftDecimal moves a decimal point right by n places, exactly.
//
// String arithmetic on purpose: "32.7" shifted 9 places is 32700000000 and no
// float is involved, so no rounding error can reach a cell somebody sums. Reports
// false for anything that is not a plain signed decimal — an exponent, a stray
// letter — which the caller then handles as before.
func shiftDecimal(s string, n int) (string, bool) {
	sign := ""
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		sign, s = "-", rest
	} else if rest, ok := strings.CutPrefix(s, "+"); ok {
		s = rest
	}

	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return "", false
	}
	for _, part := range []string{whole, frac} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return "", false
			}
		}
	}

	if len(frac) <= n {
		// The point moves past the fraction: pad with zeros.
		whole += frac + strings.Repeat("0", n-len(frac))
		frac = ""
	} else {
		whole += frac[:n]
		frac = frac[n:]
	}

	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if frac != "" {
		return sign + whole + "." + frac, true
	}
	if whole == "0" {
		// Never "-0".
		return "0", true
	}
	return sign + whole, true
}

// normaliseSeparators decides what a comma and a period mean in one number.
//
// The rules a human reader applies without noticing:
//
//	1,234.56   comma groups, period decimal      (English)
//	1.234,56   period groups, comma decimal      (German, Spanish, Italian)
//	1,5        one separator, two digits after   -> decimal
//	1,234      one separator, three digits after -> grouping, and AMBIGUOUS
//
// The last line is the honest limit: "1,234" is one thousand two hundred and
// thirty-four in English and 1.234 in German, and nothing in the string says
// which. Grouping is chosen because it is the commoner intent on the pages this
// reads, and the choice is written down here rather than left in the code.
func normaliseSeparators(s string) (string, bool) {
	lastComma := strings.LastIndex(s, ",")
	lastDot := strings.LastIndex(s, ".")

	switch {
	case lastComma < 0 && lastDot < 0:
		return s, true
	case lastComma >= 0 && lastDot >= 0:
		// Both present: the rightmost is the decimal point.
		if lastComma > lastDot {
			return strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), ",", "."), true
		}
		return strings.ReplaceAll(s, ",", ""), true
	case lastComma >= 0:
		// A comma alone. Exactly two digits after it is a decimal comma;
		// three is a group.
		switch len(s) - lastComma - 1 {
		case 3:
			return strings.ReplaceAll(s, ",", ""), true
		case 1, 2:
			return strings.Replace(s, ",", ".", 1), true
		default:
			// Several commas, or an unusual grouping: treat them all as groups.
			return strings.ReplaceAll(s, ",", ""), true
		}
	default:
		return s, true
	}
}

// coerceDate normalises to an ISO date, or to a bare year when that is all the
// source gave.
//
// A year is a legitimate answer — "founded 1998" is what most pages say — so
// demanding a full date would drop the commonest form of the commonest date
// field.
//
// AMBIGUITY, stated rather than left to be discovered: "03/04/2024" is read as
// 3 April, not 4 March. Day-first is tried before month-first, so a US source
// writing 4 March is recorded as 3 April — and because month-first is still in
// the list, "04/13/2024" falls through to it and IS read as 13 April. The same
// column can therefore mix both conventions depending on whether the day exceeds
// twelve.
//
// There is no way to resolve this from the string. It is left day-first because
// that is the majority convention outside the United States, and a schema that
// needs certainty should ask for an ISO date in its field description.
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
		// Digits only: Atoi accepts "+123" and "-123", and neither is a year.
		if n, err := strconv.Atoi(v); err == nil && n >= 1000 && v[0] != '+' && v[0] != '-' {
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
//
// Same three-step shape as parseMined, and for the same measured reason. Rows are
// mined by the cheap tier, a row is BIGGER than a claim — one quote plus a value
// per field — and asking for twenty-five of them makes hitting the output ceiling
// mid-JSON the normal case rather than a corner. The strict path discards every
// row in a truncated reply, including the twenty that arrived whole; parseMined
// was given this path after it cost three mining calls in four on a live 3B model,
// and nothing about that finding was specific to claims.
//
// Salvaging is safe for exactly the §11.5 reason: a recovered row still has to
// carry a quote that appears verbatim in the passage and still has to fill a key,
// so a half-parsed one dies at accept() regardless.
func parseRows(raw string) ([]minedRow, error) {
	// "no rows here" is a legitimate answer and the prompt asks for it. A model
	// that says so as a bare `[]` must not be reported as a failed chunk.
	if empty, ok := emptyRowSet(raw); ok {
		return empty, nil
	}
	if body := jsonish.ExtractObject(raw); body != "" {
		var resp rowResponse
		if err := json.Unmarshal([]byte(body), &resp); err == nil {
			return resp.Rows, nil
		}
	}
	if rows := salvageRows(raw); len(rows) > 0 {
		return rows, nil
	}
	return nil, fmt.Errorf("actors: no usable rows in the model's reply: %s",
		truncateForLog(raw))
}

func emptyRowSet(raw string) ([]minedRow, bool) {
	body := strings.TrimSpace(jsonish.StripFence(raw))
	var asObject rowResponse
	if err := json.Unmarshal([]byte(body), &asObject); err == nil && len(asObject.Rows) == 0 {
		return nil, true
	}
	var asArray []minedRow
	if err := json.Unmarshal([]byte(body), &asArray); err == nil && len(asArray) == 0 {
		return nil, true
	}
	return nil, false
}

// salvageRows pulls every complete row object out of a possibly-truncated reply.
//
// jsonish.Objects returns nested objects as well as the wrapper, so a row's own
// `values` object comes back too — and unmarshals into a minedRow with no quote,
// which the filter here drops and accept() would drop again.
func salvageRows(raw string) []minedRow {
	var out []minedRow
	for _, obj := range jsonish.Objects(raw) {
		var r minedRow
		if err := json.Unmarshal([]byte(obj), &r); err == nil &&
			r.Quote != "" && len(r.Values) > 0 {
			out = append(out, r)
		}
	}
	return out
}
