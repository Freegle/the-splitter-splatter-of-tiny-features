package verify

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// difftTimeout bounds one difft invocation.
const difftTimeout = 30 * time.Second

// difftAvailable reports whether the difft binary can be used for
// structural file comparison. It is a variable, not a plain function call,
// so tests can force the token-Levenshtein fallback path even on a
// machine that has difft installed.
var difftAvailable = func() bool {
	_, err := exec.LookPath("difft")
	return err == nil
}

// meanSimilarity scores the similarity of an edit turn's two worktrees
// over the union of files either side touched: a file touched by both
// sides is compared with fileSimilarity, a file touched by only one side
// scores 0 (DESIGN.md: "a file edited on one side only scores 0"). An
// empty union (no file could be resolved into either worktree) scores 0.
func meanSimilarity(ctx context.Context, frontierDir, localDir string, frontierTouched, localTouched []string) float64 {
	frontierSet := toSet(frontierTouched)
	localSet := toSet(localTouched)
	union := unionPaths(frontierTouched, localTouched)
	if len(union) == 0 {
		return 0
	}

	var total float64
	for _, rel := range union {
		if frontierSet[rel] && localSet[rel] {
			total += fileSimilarity(ctx, filepath.Join(frontierDir, rel), filepath.Join(localDir, rel))
		}
	}
	return total / float64(len(union))
}

func toSet(paths []string) map[string]bool {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		m[p] = true
	}
	return m
}

// unionPaths returns the deduplicated union of a and b, in a's order
// followed by any new entries from b.
func unionPaths(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, p := range a {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range b {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// firstOf returns a's first element, else b's first element, else "".
func firstOf(a, b []string) string {
	if len(a) > 0 {
		return a[0]
	}
	if len(b) > 0 {
		return b[0]
	}
	return ""
}

// fileSimilarity compares two files' resulting content and returns a
// similarity score in [0,1]: difft's structural diff when the difft binary
// is available and runs successfully, else token-level normalized
// Levenshtein similarity over the raw file contents.
func fileSimilarity(ctx context.Context, pathA, pathB string) float64 {
	if difftAvailable() {
		if sim, ok := difftSimilarity(ctx, pathA, pathB); ok {
			return sim
		}
	}
	contentA, errA := os.ReadFile(pathA)
	contentB, errB := os.ReadFile(pathB)
	if errA != nil || errB != nil {
		return 0
	}
	return tokenSimilarity(string(contentA), string(contentB))
}

// difftResult decodes the fields of `difft --display json` output
// (DFT_UNSTABLE=yes) needed to derive a similarity score: "status" is
// "unchanged" or "changed" (among other values not produced when both
// input files exist, which is the only case fileSimilarity uses this for);
// "aligned_lines" pairs old/new line numbers, its length approximates the
// file's line count; "chunks" is an array of hunks, each an array of
// changed line-level entries.
type difftResult struct {
	Status       string            `json:"status"`
	AlignedLines [][2]int          `json:"aligned_lines"`
	Chunks       []json.RawMessage `json:"chunks"`
}

// difftSimilarity runs `difft --display json pathA pathB` and maps its
// output to a similarity score via a changed-lines / total-lines
// heuristic. ok is false when the binary is missing, fails, times out, or
// produces output this cannot parse; callers fall back to
// tokenSimilarity in that case.
func difftSimilarity(ctx context.Context, pathA, pathB string) (float64, bool) {
	cctx, cancel := context.WithTimeout(ctx, difftTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "difft", "--display", "json", pathA, pathB)
	cmd.Env = append(os.Environ(), "DFT_UNSTABLE=yes")
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}

	var res difftResult
	if err := json.Unmarshal(out, &res); err != nil {
		return 0, false
	}
	if res.Status == "unchanged" {
		return 1, true
	}

	totalLines := len(res.AlignedLines)
	if totalLines == 0 {
		data, err := os.ReadFile(pathB)
		if err != nil {
			return 0, false
		}
		totalLines = strings.Count(string(data), "\n") + 1
	}

	changed := 0
	for _, chunk := range res.Chunks {
		var pairs []json.RawMessage
		if err := json.Unmarshal(chunk, &pairs); err == nil {
			changed += len(pairs)
		}
	}
	if totalLines == 0 {
		if changed > 0 {
			return 0, true
		}
		return 1, true
	}

	sim := 1 - float64(changed)/float64(totalLines)
	if sim < 0 {
		sim = 0
	}
	return sim, true
}

// tokenSimilarity returns a normalized Levenshtein similarity in [0,1]
// between the whitespace-separated tokens of a and b: 1 - (edit distance /
// longer token count). Two token sequences that are both empty are
// treated as identical (similarity 1).
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

// tokenLevenshtein returns the edit distance between token slices a and b
// (insertions, deletions and substitutions, each cost 1), via the standard
// two-row dynamic programming table.
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
