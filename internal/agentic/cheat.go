package agentic

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Cheat flag types, per DESIGN.md "Leakage containment". Any flag on a
// result demotes it to the untrusted segment; none of these are silently
// dropped, all are recorded in eval_results.cheat_flags for human audit
// against the stored transcript.
const (
	CheatFlagEscape        = "escape"
	CheatFlagGitPoke       = "git_poke"
	CheatFlagToolSmuggling = "tool_smuggling"
	CheatFlagSuspectCopy   = "suspect_copy"
	CheatFlagAttemptedGit  = "attempted_git"
	CheatFlagAllowNetwork  = "allow_network"
)

// CheatFlag is one leakage-containment detector hit.
type CheatFlag struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

// encodeCheatFlags marshals flags to JSON for eval_results.cheat_flags,
// returning "" for an empty slice (stored as SQL NULL, matching every other
// optional column in this package).
func encodeCheatFlags(flags []CheatFlag) string {
	if len(flags) == 0 {
		return ""
	}
	b, err := json.Marshal(flags)
	if err != nil {
		return ""
	}
	return string(b)
}

// smugglingPatterns match content that reaches for git, an HTTP/DNS client,
// or a subprocess-invoking network tool: DESIGN.md "tool_smuggling: ...
// added code matching git invocation, curl/wget/http client calls, or DNS
// lookups". Best-effort, not exhaustive; every hit is recorded for human
// review rather than silently trusted or blocked.
var smugglingPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bgit\s+(clone|fetch|pull|push|remote|ls-remote)\b`),
	regexp.MustCompile(`(?i)exec\.Command\s*\(\s*"git"`),
	regexp.MustCompile(`(?i)exec\.Command\s*\(\s*"(curl|wget|nc|ncat)"`),
	regexp.MustCompile(`(?i)\b(curl|wget)\s`),
	regexp.MustCompile(`\bnet/http\b`),
	regexp.MustCompile(`\bhttp\.(Get|Post|Head|NewRequest)\s*\(`),
	regexp.MustCompile(`\bnet\.Dial(Timeout)?\s*\(`),
	regexp.MustCompile(`\bnet\.LookupHost\s*\(`),
	regexp.MustCompile(`\bfetch\s*\(`),
	regexp.MustCompile(`\baxios\.`),
	regexp.MustCompile(`\brequests\.(get|post)\s*\(`),
	regexp.MustCompile(`\bos/exec\b`),
}

// matchesSmuggling returns the pattern strings that match s.
func matchesSmuggling(s string) []string {
	var hits []string
	for _, re := range smugglingPatterns {
		if re.MatchString(s) {
			hits = append(hits, re.String())
		}
	}
	return hits
}

// detectToolSmuggling returns the smuggling patterns present in newContent
// but not in oldContent: DESIGN.md's diff-based rule ("in files the task
// did not previously have them in"). A file with a matching pattern already
// present before the model touched it (e.g. an existing net/http import)
// never flags on that pattern again.
func detectToolSmuggling(oldContent, newContent string) []string {
	oldSet := map[string]bool{}
	for _, h := range matchesSmuggling(oldContent) {
		oldSet[h] = true
	}
	var introduced []string
	for _, h := range matchesSmuggling(newContent) {
		if !oldSet[h] {
			introduced = append(introduced, h)
		}
	}
	return introduced
}

// containsAttemptedGit reports whether output (typically a run_tests
// invocation's combined output, captured while .git was parked) mentions
// git failing to find a repository: DESIGN.md's attempted_git signal.
func containsAttemptedGit(output string) bool {
	return strings.Contains(strings.ToLower(output), "not a git repository")
}

// suspectCopyThreshold returns the similarity threshold suspect_copy must
// clear to flag, per DESIGN.md: "weighted by patch size (trivial patches
// converge legitimately; the flag threshold rises as patches shrink)". A
// patch of 3 diff lines or fewer is exempt entirely (threshold above 1,
// unreachable); the threshold eases towards the base 0.98 as the patch
// grows.
func suspectCopyThreshold(diffLines int) float64 {
	switch {
	case diffLines <= 3:
		return 1.01
	case diffLines <= 10:
		return 0.995
	default:
		return 0.98
	}
}

// DetectSuspectCopy compares a model's final content for the reference
// fix's touched files (similarity, computed by the caller via
// tokenSimilarity/meanFileSimilarity) against diffLines (the task's
// characteristics.size.diff_lines, the size-weighting DESIGN.md calls for),
// and returns a flag when similarity clears suspectCopyThreshold. Returns
// nil when it does not.
func DetectSuspectCopy(similarity float64, diffLines int) *CheatFlag {
	threshold := suspectCopyThreshold(diffLines)
	if similarity < threshold {
		return nil
	}
	return &CheatFlag{
		Type:   CheatFlagSuspectCopy,
		Detail: fmt.Sprintf("similarity %.4f >= threshold %.4f for a %d-line reference patch", similarity, threshold, diffLines),
	}
}

// tokenSimilarity returns a normalized Levenshtein similarity in [0,1]
// between the whitespace-separated tokens of a and b: 1 - (edit distance /
// longer token count). Two empty token sequences are treated as identical.
// A small, self-contained utility (not internal/verify's unexported
// tokenSimilarity: see DECISIONS.md).
func tokenSimilarity(a, b string) float64 {
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 1
	}
	dist := tokenLevenshtein(ta, tb)
	maxLen := len(ta)
	if len(tb) > maxLen {
		maxLen = len(tb)
	}
	if maxLen == 0 {
		return 1
	}
	sim := 1 - float64(dist)/float64(maxLen)
	if sim < 0 {
		sim = 0
	}
	return sim
}

func tokenLevenshtein(a, b []string) int {
	n, m := len(a), len(b)
	prev := make([]int, m+1)
	curr := make([]int, m+1)
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		curr[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			min := del
			if ins < min {
				min = ins
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[m]
}
