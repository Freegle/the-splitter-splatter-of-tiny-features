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

// lintTimeout bounds one linter invocation.
const lintTimeout = 30 * time.Second

// lintEntry is one linter (or apply-failure) result, the shape encoded
// into verifications.frontier_lint / local_lint.
type lintEntry struct {
	Tool   string `json:"tool"`
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
}

// runCommand runs name with args in dir, bounded by timeout (as a child of
// ctx), and returns its combined stdout+stderr.
func runCommand(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// lintFile runs the appropriate linter for relPath's extension inside
// worktreeDir, when a suitable binary is present. It returns the zero
// lintEntry (Tool == "") when no linter applies or its binary is not
// installed: callers skip appending a zero entry, so "no linter available"
// never masquerades as "linted clean".
func lintFile(ctx context.Context, worktreeDir, relPath string) lintEntry {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".go":
		return lintGo(ctx, worktreeDir, relPath)
	case ".php":
		return lintPHP(ctx, worktreeDir, relPath)
	case ".js", ".ts", ".vue", ".mjs", ".cjs":
		return lintESLint(ctx, worktreeDir, relPath)
	default:
		return lintEntry{}
	}
}

// lintGo runs golangci-lint (--fast, scoped to the file's directory) when
// installed, else falls back to `gofmt -l` plus `go vet` on the nearest
// enclosing Go module, per DECISIONS.md (golangci-lint is not installed on
// the reference machine).
func lintGo(ctx context.Context, worktreeDir, relPath string) lintEntry {
	full := filepath.Join(worktreeDir, relPath)
	dir := filepath.Dir(full)

	if _, err := exec.LookPath("golangci-lint"); err == nil {
		out, runErr := runCommand(ctx, dir, lintTimeout, "golangci-lint", "run", "--fast", ".")
		return lintEntry{Tool: "golangci-lint", OK: runErr == nil, Output: strings.TrimSpace(out)}
	}

	if _, err := exec.LookPath("gofmt"); err != nil {
		return lintEntry{}
	}

	ok := true
	var parts []string

	fmtOut, fmtErr := runCommand(ctx, worktreeDir, lintTimeout, "gofmt", "-l", full)
	switch {
	case fmtErr != nil:
		ok = false
		parts = append(parts, "gofmt: "+fmtErr.Error()+": "+strings.TrimSpace(fmtOut))
	case strings.TrimSpace(fmtOut) != "":
		ok = false
		parts = append(parts, "gofmt: not formatted")
	}

	if _, err := exec.LookPath("go"); err == nil {
		if modDir, pkgRel, found := findGoModule(worktreeDir, dir); found {
			vetOut, vetErr := runCommand(ctx, modDir, lintTimeout, "go", "vet", "./"+filepath.ToSlash(pkgRel))
			if vetErr != nil {
				ok = false
				parts = append(parts, "go vet: "+strings.TrimSpace(vetOut))
			}
		}
	}

	return lintEntry{Tool: "gofmt+govet", OK: ok, Output: strings.Join(parts, "; ")}
}

// findGoModule walks up from startDir to worktreeRoot (inclusive) looking
// for the nearest go.mod, returning its directory and startDir's path
// relative to it (as a `go vet` package argument, "." when they are the
// same directory).
func findGoModule(worktreeRoot, startDir string) (modDir string, pkgRel string, found bool) {
	root := filepath.Clean(worktreeRoot)
	dir := filepath.Clean(startDir)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			rel, err := filepath.Rel(dir, startDir)
			if err != nil || rel == "" {
				rel = "."
			}
			return dir, rel, true
		}
		if dir == root {
			return "", "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", false
		}
		dir = parent
	}
}

// lintPHP runs `php -l` when the php binary is present.
func lintPHP(ctx context.Context, worktreeDir, relPath string) lintEntry {
	if _, err := exec.LookPath("php"); err != nil {
		return lintEntry{}
	}
	full := filepath.Join(worktreeDir, relPath)
	out, err := runCommand(ctx, worktreeDir, lintTimeout, "php", "-l", full)
	return lintEntry{Tool: "php -l", OK: err == nil, Output: strings.TrimSpace(out)}
}

// lintESLint runs `eslint --no-eslintrc` when the eslint binary is
// present.
func lintESLint(ctx context.Context, worktreeDir, relPath string) lintEntry {
	if _, err := exec.LookPath("eslint"); err != nil {
		return lintEntry{}
	}
	full := filepath.Join(worktreeDir, relPath)
	out, err := runCommand(ctx, worktreeDir, lintTimeout, "eslint", "--no-eslintrc", full)
	return lintEntry{Tool: "eslint", OK: err == nil, Output: strings.TrimSpace(out)}
}

// resultCapBytes bounds the encoded JSON of a lint or test result, per
// DESIGN.md.
const resultCapBytes = 2048

// outputTruncateBytes bounds one entry's raw Output field before the
// whole array is capped, so a single very long tool output does not by
// itself force encodeLintEntries to drop other entries.
const outputTruncateBytes = 400

// encodeLintEntries marshals entries to JSON, truncating each entry's
// Output and then, if the array is still over resultCapBytes, dropping
// trailing entries until it fits. Returns "" for an empty (nil) slice, so
// FrontierLint/LocalLint stay empty when nothing linted.
func encodeLintEntries(entries []lintEntry) string {
	if len(entries) == 0 {
		return ""
	}
	capped := make([]lintEntry, len(entries))
	copy(capped, entries)
	for i := range capped {
		capped[i].Output = truncateString(capped[i].Output, outputTruncateBytes)
	}
	for len(capped) > 0 {
		b, err := json.Marshal(capped)
		if err == nil && len(b) <= resultCapBytes {
			return string(b)
		}
		capped = capped[:len(capped)-1]
	}
	return "[]"
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
