package output

import (
	"fmt"
	"strconv"
	"strings"
)

// Body validation.
//
// The prompt cannot be the only defence. §13's claim is that a report's citations
// are trustworthy because the numbers are assigned mechanically before the prompt is
// built — which is true of NUMBERS and was never true of the brackets around them. A
// live run emitted
//
//	[subword tokenizers often need a massive vocabulary to cover diverse scripts…]
//
// which reads as a citation, points at nothing, and passed every check because no
// check looked. A later run's entire body was
//
//	source does NOT support this on re-read
//
//	[1][4]
//
// — a fragment of the instructions plus a bare marker group. Both are model failures
// rather than pipeline failures, and both produced a report worse than the
// unsynthesized evidence listing that was already available for free.
//
// So the body is checked, and a body that fails falls back to that listing with the
// reason declared. Rejecting costs readability; accepting costs a reader's trust in
// every citation mole prints.

// bodyProblem describes why a synthesis was rejected.
type bodyProblem struct {
	Reason string
}

func (p bodyProblem) Error() string { return p.Reason }

// validateBody checks a synthesized body against the citations it was given.
//
// Returns nil when the body is usable.
func validateBody(body string, findings []Finding, citations []Citation) error {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return bodyProblem{"synthesis returned nothing"}
	}

	groups := bracketGroups(trimmed)
	valid := 0
	for _, g := range groups {
		ns, ok := citationNumbers(g)
		if !ok {
			// A bracket holding anything but citation numbers. This is the forged
			// citation: it looks like evidence and is not.
			//
			// It also rejects a markdown link, deliberately. The prompt asks for
			// numbered citations and nothing else, and a link in the body is
			// page-derived text reaching the answer outside the citation mechanism
			// — which is the one thing §13's numbering exists to prevent.
			return bodyProblem{fmt.Sprintf(
				"the answer contains a citation-shaped marker that is not a citation: [%.60s]", g)}
		}
		for _, n := range ns {
			if n < 1 || n > len(citations) {
				return bodyProblem{fmt.Sprintf(
					"the answer cites source [%d], and only %d source(s) exist", n, len(citations))}
			}
		}
		valid += len(ns)
	}
	if valid == 0 {
		return bodyProblem{"the answer cites nothing; every factual sentence was required to carry a citation"}
	}

	// A synthesis has to say more than the least of its own inputs. Relative rather
	// than a word count, so it scales with the material: a body shorter than the
	// shortest finding it was given has arranged nothing, and the evidence listing
	// says strictly more.
	prose := len(strings.TrimSpace(stripMarkers(trimmed)))
	if floor := shortestFinding(findings); floor > 0 && prose < floor {
		return bodyProblem{fmt.Sprintf(
			"the answer is %d characters of prose, less than the shortest single finding (%d) it was given",
			prose, floor)}
	}
	return nil
}

// bracketGroups returns the contents of every [...] in the text.
func bracketGroups(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '[' {
			continue
		}
		end := strings.IndexByte(s[i:], ']')
		if end < 0 {
			// An unclosed bracket is not a marker; nothing to check.
			break
		}
		out = append(out, s[i+1:i+end])
		i += end
	}
	return out
}

// citationNumbers parses a bracket group's contents as citation numbers.
//
// Accepts "3" and "3, 4" — models write both — but nothing else. A group is all
// numbers or it is not a citation.
func citationNumbers(g string) ([]int, bool) {
	g = strings.TrimSpace(g)
	if g == "" {
		return nil, false
	}
	fields := strings.FieldsFunc(g, func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		return nil, false
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// stripMarkers removes citation markers, leaving the prose.
func stripMarkers(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '[' {
			if end := strings.IndexByte(s[i:], ']'); end >= 0 {
				i += end
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// shortestFinding is the length of the shortest finding text put in front of the
// model, or 0 when there were none.
func shortestFinding(findings []Finding) int {
	floor := 0
	for _, f := range findings {
		if f.Claim == nil {
			continue
		}
		n := len(strings.TrimSpace(f.Claim.Text))
		if n == 0 {
			continue
		}
		if floor == 0 || n < floor {
			floor = n
		}
	}
	return floor
}
