package agentic

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/freegle/splitter/internal/evals"
)

// GradeResult is the outcome of one harness-triggered grading pass (either
// the pre-loop baseline or the post-loop final grade): DESIGN.md's
// fail-to-pass grading runs the same test command twice.
type GradeResult struct {
	Output string
	Exit0  bool
	// ByTest maps each Go test name observed in `go test -json` output to
	// its last reported pass/fail action. Empty when command was not a
	// `go test -json` invocation (the coarse, non-Go grading path).
	ByTest map[string]bool
}

// RunGrading runs command in the sandbox via tests (the same TestExecutor a
// task's ToolExecutor uses for its own run_tests calls: v1's CommandRunner
// parks .git and optionally denies network, arena mode's ArenaRunner execs
// into the arena's containers), and parses its output into a GradeResult.
func RunGrading(ctx context.Context, tests TestExecutor, dir, command string) (GradeResult, error) {
	output, ok, err := tests.RunTests(ctx, dir, command)
	if err != nil {
		return GradeResult{}, err
	}
	gr := GradeResult{Output: output, Exit0: ok}
	if strings.Contains(command, "-json") {
		gr.ByTest = parseGoTestResults(output)
	}
	return gr, nil
}

// goTestEvent is one line of `go test -json` output.
type goTestEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
}

// parseGoTestResults reads `go test -json` output and returns, per test
// name (including subtests, whose Test field carries a "/" separated
// path), whether its last reported pass/fail action was "pass". A test
// name never reported pass or fail (e.g. a build failure prevented it from
// running at all) is simply absent from the result.
func parseGoTestResults(output string) map[string]bool {
	results := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "pass":
			results[ev.Test] = true
		case "fail":
			results[ev.Test] = false
		}
	}
	return results
}

// goTestFuncPattern matches a top-level Go test function declaration.
var goTestFuncPattern = regexp.MustCompile(`(?m)^func\s+(Test\w+)\s*\(`)

// HeldOutTestNames extracts the Go test function names defined by
// payload's held-out test files (their post-commit content: new files in
// full, modified files from their hunks' New side), the fail-to-pass
// grading target. Returns nil when payload's files are not all Go (the
// coarse grading path applies instead; see internal/evals.buildHoldoutPayload
// which only sets TestCmd in that all-Go case).
func HeldOutTestNames(payload evals.HoldoutPayload) []string {
	seen := map[string]bool{}
	var names []string
	for _, f := range payload.Files {
		content := f.Content
		if content == "" {
			for _, h := range f.Hunks {
				content += h.New + "\n"
			}
		}
		for _, m := range goTestFuncPattern.FindAllStringSubmatch(content, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				names = append(names, m[1])
			}
		}
	}
	return names
}

// ScoreFailToPass computes tests_ran/tests_passed/regressions from a
// baseline (pre-loop) and final (post-loop) grading pass, per DESIGN.md
// "Grade = fail-to-pass on the held-out tests AND no new failures in the
// task package's pre-existing tests (both recorded separately)".
//
// When heldOutNames is empty (a coarse, non-Go-test-json grade, e.g. a
// harvested live task's configured [tests] command), tests_ran/tests_passed
// become 1/0 from final.Exit0 alone and regressions is always 0: there is
// no per-test baseline to compare against.
//
// Otherwise: tests_ran/tests_passed count how many held-out test names the
// FINAL pass actually observed running, and how many of those passed
// (DESIGN.md pairs "tests_ran" and "tests_passed (held-out)" as one grading
// triple; a name a build failure prevented from running at all is not
// counted as "ran"). regressions counts baseline-passing, non-held-out
// test names that the final pass reports failing.
func ScoreFailToPass(baseline, final GradeResult, heldOutNames []string) (testsRan, testsPassed, regressions int) {
	if len(heldOutNames) == 0 {
		testsRan = 1
		if final.Exit0 {
			testsPassed = 1
		}
		return testsRan, testsPassed, 0
	}

	for _, name := range heldOutNames {
		passed, ran := final.ByTest[name]
		if !ran {
			continue
		}
		testsRan++
		if passed {
			testsPassed++
		}
	}

	heldOutSet := make(map[string]bool, len(heldOutNames))
	for _, n := range heldOutNames {
		heldOutSet[n] = true
	}
	for name, passedBefore := range baseline.ByTest {
		if !passedBefore || heldOutSet[name] {
			continue
		}
		if passedAfter, ran := final.ByTest[name]; ran && !passedAfter {
			regressions++
		}
	}
	return testsRan, testsPassed, regressions
}
