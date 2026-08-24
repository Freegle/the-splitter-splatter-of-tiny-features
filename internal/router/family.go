// Package router implements Phase 4: router statistics (per-category
// Wilson-bound agreement, computed by "splitter router update" from
// verifications x features) and the live routing decisions the proxy
// consults (routable lookup, dual-dispatch shadowing, the session-level
// escalation circuit breaker), plus model family normalisation shared by
// both.
package router

import "strings"

// Family normalises an exact model id to a family key that survives model
// version bumps: strip date suffixes, strip generation numbers, keep
// variant words and parameter size tags, lowercase. overrides maps an
// exact model id (as it appears on the wire, matched verbatim before any
// normalisation) to a family string that takes precedence over the rule
// below; a nil or non-matching overrides map falls through to the rules.
//
// Examples (DESIGN.md "Model families"):
//
//	claude-opus-5, claude-opus-5-20260101       -> claude-opus
//	claude-sonnet-4-6                           -> claude-sonnet
//	claude-haiku-4-5, claude-haiku-4-5@20260115 -> claude-haiku
//	qwen2.5-coder:7b, qwen3-coder:7b            -> qwen-coder:7b
//	Qwen/Qwen2.5-Coder-32B-Instruct             -> qwen-coder-32b-instruct
//	gemini-2.5-flash                            -> gemini-flash
//	gpt-4o-mini                                 -> gpt-4o-mini (unchanged:
//	  letter-adjacent digits with no dot/dash separation stay)
func Family(model string, overrides map[string]string) string {
	if v, ok := overrides[model]; ok {
		return v
	}

	m := strings.ToLower(strings.TrimSpace(model))
	m = stripNamespace(m)
	m = stripDateSuffix(m)
	return joinKeptTokens(tokenize(m))
}

// stripNamespace removes a "namespace/" prefix (as in Together-style model
// ids like "Qwen/Qwen2.5-Coder-32B-Instruct"), keeping only the text after
// the last "/". A model id with no "/" is returned unchanged.
func stripNamespace(m string) string {
	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		return m[idx+1:]
	}
	return m
}

// stripDateSuffix removes a trailing "-YYYYMMDD" or "@YYYYMMDD" segment (8
// digits), if present, trying "-" then "@" as the separator.
func stripDateSuffix(m string) string {
	for _, sep := range []string{"-", "@"} {
		idx := strings.LastIndex(m, sep)
		if idx < 0 {
			continue
		}
		suffix := m[idx+1:]
		if len(suffix) == 8 && isDigitsAndDots(suffix) {
			return m[:idx]
		}
	}
	return m
}

// segment is one dash/colon-delimited token of a model id, together with
// the separator character that preceded it ("" for the first token).
type segment struct {
	sep string
	tok string
}

// tokenize splits m into segments at each '-' or ':', recording each
// token's preceding separator so joinKeptTokens can reproduce the original
// punctuation around whichever tokens survive stripping.
func tokenize(m string) []segment {
	var segs []segment
	sep := ""
	start := 0
	for i := 0; i <= len(m); i++ {
		if i == len(m) || m[i] == '-' || m[i] == ':' {
			segs = append(segs, segment{sep: sep, tok: m[start:i]})
			if i < len(m) {
				sep = string(m[i])
			}
			start = i + 1
		}
	}
	return segs
}

// joinKeptTokens strips a trailing generation-number suffix from each
// token (stripTrailingVersion) and rejoins the tokens that still have
// content, each with its own recorded separator; a token that strips to
// empty (a whole dash-token that was a pure version number, e.g. "5" or
// "2.5") is dropped along with that separator, never leaving a stray "--".
func joinKeptTokens(segs []segment) string {
	var b strings.Builder
	wroteAny := false
	for _, s := range segs {
		tok := stripTrailingVersion(s.tok)
		if tok == "" {
			continue
		}
		if wroteAny {
			b.WriteString(s.sep)
		}
		b.WriteString(tok)
		wroteAny = true
	}
	return b.String()
}

// stripTrailingVersion removes a trailing run of digits/dots from tok when
// it is preceded by at least one non-digit character (a generation number
// fused onto a name, e.g. "qwen2.5" -> "qwen"), returns "" when the whole
// token is digits/dots (a standalone version segment, e.g. "5" or "2.5"),
// and returns tok unchanged when it has no trailing digit/dot run at all
// (it ends in a letter, e.g. "7b", "32b", "4o": a parameter size tag or a
// product name fragment, not a version number).
func stripTrailingVersion(tok string) string {
	end := len(tok)
	start := end
	for start > 0 && isDigitOrDot(tok[start-1]) {
		start--
	}
	if start == 0 {
		return ""
	}
	if start == end {
		return tok
	}
	return tok[:start]
}

func isDigitOrDot(c byte) bool {
	return c == '.' || (c >= '0' && c <= '9')
}

// isDigitsAndDots reports whether s is non-empty and consists only of
// digits and dots.
func isDigitsAndDots(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigitOrDot(s[i]) {
			return false
		}
	}
	return true
}
