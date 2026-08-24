package evals

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// SplitTestFiles partitions files into test files and non-test files, using
// the same [layers] "tests" classification internal/feature.Subsystem's
// sibling rules already use (layerForPath(f, layers) == "tests": DESIGN.md's
// defaults match *_test.*, tests/, spec/). Order within each returned slice
// matches files' order. Used by seed-history (DESIGN.md "Agentic eval
// mode": "seed-history splits each commit's diff into TEST files and
// NON-test files") to decide which of a commit's changed files become the
// held-out grading payload.
func SplitTestFiles(files []string, layers map[string]string) (testFiles, nonTestFiles []string) {
	for _, f := range files {
		if layerForPath(f, layers) == "tests" {
			testFiles = append(testFiles, f)
		} else {
			nonTestFiles = append(nonTestFiles, f)
		}
	}
	return testFiles, nonTestFiles
}

// HoldoutFile is one held-out test file's post-commit state: either brand
// new content, or hunks to apply over the sandbox's parent-tree copy. This
// mirrors seedTouchedFile's shape (not reused directly: that type also
// carries parent-state content the holdout payload does not need to ship).
type HoldoutFile struct {
	Path  string `json:"path"`
	IsNew bool   `json:"is_new"`
	// Content is the file's full post-commit content, set only when IsNew.
	Content string `json:"content,omitempty"`
	// Hunks are old_string -> new_string replacements applying this file's
	// test changes onto its parent-tree content, set only when !IsNew.
	Hunks []diffHunk `json:"hunks,omitempty"`
}

// HoldoutPayload is the JSON shape stored (zstd-compressed) in
// eval_tasks.holdout_tests_zstd: DESIGN.md "the sandbox gets the PARENT
// tree plus the commit's TESTS applied". TestCmd is the shell command
// internal/agentic runs (network-denied) to grade the held-out tests;
// empty when seed-history could not derive a safe command for these files'
// language (the task is stored for future use but not agentic-gradable
// today, see DECISIONS.md).
type HoldoutPayload struct {
	Files   []HoldoutFile `json:"files"`
	TestCmd string        `json:"test_cmd,omitempty"`
}

// DecodeHoldoutPayload decodes an eval_tasks.holdout_tests_zstd blob
// (already zstd-decompressed) into a HoldoutPayload.
func DecodeHoldoutPayload(data []byte) (HoldoutPayload, error) {
	var p HoldoutPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return HoldoutPayload{}, fmt.Errorf("decoding holdout payload: %w", err)
	}
	return p, nil
}

// goTestCommand builds the `go test` command for a set of held-out Go test
// file paths, scoped to the unique package directories they live in (not
// `go test ./...`, to keep the grading run's blast radius close to the
// commit's own package). Returns "" when paths is empty.
func goTestCommand(paths []string) string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range paths {
		dir := filepath.Dir(p)
		if dir == "" || dir == "/" {
			dir = "."
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		return ""
	}
	var pkgs []string
	for _, d := range dirs {
		pkgs = append(pkgs, "./"+strings.TrimPrefix(d, "./")+"/...")
	}
	return "go test -json " + strings.Join(pkgs, " ")
}

// buildHoldoutPayload builds the agentic holdout payload for a seed-history
// commit from its touched files, when any of them classify as test files.
// ok is false when the commit has no test-file changes (single-turn-only,
// per DESIGN.md) or seed-history has no safe test command for their
// language (Go only today, see DECISIONS.md).
func buildHoldoutPayload(touched []seedTouchedFile, layers map[string]string, subsystem string) (payload HoldoutPayload, ok bool) {
	var allPaths []string
	for _, tf := range touched {
		allPaths = append(allPaths, tf.Path)
	}
	testPaths, _ := SplitTestFiles(allPaths, layers)
	if len(testPaths) == 0 {
		return HoldoutPayload{}, false
	}

	testSet := make(map[string]bool, len(testPaths))
	for _, p := range testPaths {
		testSet[p] = true
	}

	var goPaths []string
	for _, tf := range touched {
		if !testSet[tf.Path] {
			continue
		}
		payload.Files = append(payload.Files, HoldoutFile{
			Path: tf.Path, IsNew: tf.IsNew, Content: tf.NewContent, Hunks: tf.Hunks,
		})
		if strings.EqualFold(filepath.Ext(tf.Path), ".go") {
			goPaths = append(goPaths, stripSubsystem(tf.Path, subsystem))
		}
	}

	// A command is derived only when every held-out test speaks one
	// toolchain. Commands use SUBSYSTEM-RELATIVE paths: the arena executes
	// them inside the subsystem's own container at its root (v1 standalone
	// grading remains go-only since only Go runs outside the Docker stack).
	if len(goPaths) == len(testPaths) {
		payload.TestCmd = goTestCommand(goPaths)
		return payload, true
	}
	if cmd := phpTestCommand(testPaths, subsystem); cmd != "" {
		payload.TestCmd = cmd
		return payload, true
	}
	if cmd := vitestCommand(testPaths, subsystem); cmd != "" {
		payload.TestCmd = cmd
		return payload, true
	}

	return payload, true
}

// stripSubsystem removes the leading monorepo path segment (the
// subsystem, e.g. "iznik-server-go/") from a task file path: derived test
// commands run at the subsystem root, both in arena containers (docker
// exec -w /app) and the v1 sandbox (executor dir joined with subsystem).
func stripSubsystem(p, subsystem string) string {
	if subsystem != "" {
		return strings.TrimPrefix(p, subsystem+"/")
	}
	return p
}

// phpTestCommand derives the artisan invocation for held-out Laravel
// tests: every path must be an iznik-batch *Test.php. artisan runs in the
// batch container at the app root, so the filter is by class name.
func phpTestCommand(testPaths []string, subsystem string) string {
	if subsystem != "iznik-batch" {
		return ""
	}
	var classes []string
	for _, p := range testPaths {
		base := filepath.Base(p)
		if !strings.HasSuffix(base, "Test.php") {
			return ""
		}
		classes = append(classes, strings.TrimSuffix(base, ".php"))
	}
	if len(classes) == 0 {
		return ""
	}
	return "php artisan test --filter='" + strings.Join(classes, "|") + "'"
}

// vitestCommand derives the vitest invocation for held-out nuxt specs:
// every path must be an iznik-nuxt3 .spec.js/.spec.ts, run from the nuxt
// container's /app root with the subsystem prefix stripped.
func vitestCommand(testPaths []string, subsystem string) string {
	if subsystem != "iznik-nuxt3" {
		return ""
	}
	var specs []string
	for _, p := range testPaths {
		if !strings.HasSuffix(p, ".spec.js") && !strings.HasSuffix(p, ".spec.ts") {
			return ""
		}
		specs = append(specs, stripSubsystem(p, subsystem))
	}
	if len(specs) == 0 {
		return ""
	}
	return "npx vitest run " + strings.Join(specs, " ")
}
