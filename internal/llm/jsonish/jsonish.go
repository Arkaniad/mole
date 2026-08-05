// Package jsonish parses the JSON that models actually return.
//
// Every model boundary in mole asks for JSON and gets something adjacent to it: wrapped
// in a code fence, preceded by a sentence of preamble, an object inside an array, or
// truncated mid-structure because the output ceiling arrived first. Each boundary grew
// its own tolerance layer, and by M4 there were three near-identical copies —
// byte-for-byte in the case of MatchBrace and StripFence — plus three variants of the
// object scan.
//
// That is where the next fix lands in two of them and not the third. Two of these
// functions have already been the site of a real bug: the actor's salvage loop broke on
// the unterminated outer wrapper and silently recovered nothing, and the verifier's
// array branch accepted a shape that merely unmarshalled and skipped salvage entirely.
//
// Being lenient here costs nothing, and that is the load-bearing argument: every caller
// validates what it recovers. A salvaged claim still has to carry a quote that appears
// verbatim in its source (§11.5); a salvaged verdict still has to name a pair in the
// batch it was asked about. Leniency changes only whether good answers are thrown away
// alongside a bad one.
package jsonish

import "strings"

// StripFence removes a markdown code fence, which models add despite being told to
// return JSON only.
func StripFence(s string) string {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "```")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	// Drop the language tag on the opening fence.
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[j+1:]
	}
	if k := strings.Index(rest, "```"); k >= 0 {
		rest = rest[:k]
	}
	return strings.TrimSpace(rest)
}

// MatchBrace returns the index just past the object starting at start, and whether one
// was found.
//
// Respects string literals and their escapes, so a '}' inside a quoted value does not
// end the scan early.
func MatchBrace(s string, start int) (int, bool) {
	depth, inString, escaped := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// ExtractObject finds the outermost complete JSON object in a string, tolerating code
// fences and surrounding prose. Empty when there is none.
func ExtractObject(s string) string {
	s = StripFence(s)
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	if end, ok := MatchBrace(s, start); ok {
		return s[start:end]
	}
	return ""
}

// Objects returns every complete {...} object in the text, in order.
//
// Ignores the surrounding structure deliberately. In a truncated response the outer
// wrapper is itself unterminated, so a scan that STOPS at the first unmatched brace finds
// nothing at all — the bug that made the actor's salvage path a silent no-op. Continuing
// past it is what reaches the complete objects nested inside.
func Objects(raw string) []string {
	var out []string
	for i := 0; i < len(raw); i++ {
		if raw[i] != '{' {
			continue
		}
		end, ok := MatchBrace(raw, i)
		if !ok {
			continue
		}
		out = append(out, raw[i:end])
		// Do NOT skip past a matched object: the wrapper matches too, and its contents
		// are where the useful objects are. Callers discard what does not unmarshal.
	}
	return out
}
