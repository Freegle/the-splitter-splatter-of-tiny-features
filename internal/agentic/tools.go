package agentic

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// Tool names the loop exposes to the model. No general bash and no network
// tools: DESIGN.md "the tool surface is the sandbox boundary for what the
// model can do, unshare is the boundary for what its commands can reach".
const (
	toolReadFile = "read_file"
	toolListDir  = "list_dir"
	toolGrep     = "grep"
	toolEdit     = "edit"
	toolWrite    = "write"
	toolRunTests = "run_tests"
)

// toolResultCap bounds every tool result, per DESIGN.md.
const toolResultCap = 8 * 1024

func truncateResult(s string) string {
	if len(s) <= toolResultCap {
		return s
	}
	return s[:toolResultCap] + "\n...[truncated]"
}

// toolDefinitions returns the six tools the loop exposes to the model, as
// Anthropic tool schemas (translated to OpenAI function tools by
// internal/backend.ToOpenAI when the run uses an OpenAI-compatible
// backend, or sent as-is to the native Anthropic client).
func toolDefinitions() []anthropic.Tool {
	return []anthropic.Tool{
		{
			Name:        toolReadFile,
			Description: "Read a file's contents.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}`),
		},
		{
			Name:        toolListDir,
			Description: "List a directory's entries.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"dir_path":{"type":"string"}},"required":["dir_path"]}`),
		},
		{
			Name:        toolGrep,
			Description: "Search files under path (default: repo root) for lines matching a regular expression pattern.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`),
		},
		{
			Name:        toolEdit,
			Description: "Replace the first occurrence of old_string with new_string in file_path. old_string must already occur in the file.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"]}`),
		},
		{
			Name:        toolWrite,
			Description: "Create or overwrite file_path with content.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`),
		},
		{
			Name:        toolRunTests,
			Description: "Run this task's test command in the sandbox and report the output.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}
}

// resolveSandboxPath resolves userPath against root (root-relative when not
// absolute), evaluating symlinks along the way (tolerating a path that does
// not exist yet, for a write target), and returns both the resolved
// absolute path and its path relative to root. err is non-nil when the
// resolved path falls outside root: DESIGN.md's escape detector ("any tool
// path argument that resolves outside the sandbox root after symlink
// resolution").
func resolveSandboxPath(root, userPath string) (resolved, rel string, err error) {
	cleanRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolving sandbox root: %w", err)
	}

	var joined string
	if filepath.IsAbs(userPath) {
		joined = filepath.Clean(userPath)
	} else {
		joined = filepath.Clean(filepath.Join(root, userPath))
	}

	resolved, err = evalSymlinksTolerant(joined)
	if err != nil {
		return "", "", fmt.Errorf("resolving path: %w", err)
	}

	rel, err = filepath.Rel(cleanRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes sandbox root")
	}
	return resolved, rel, nil
}

// evalSymlinksTolerant resolves symlinks along path, walking up to the
// nearest existing ancestor when path (or its trailing components) does
// not exist yet, so escape detection still works for a write target that
// will be created.
func evalSymlinksTolerant(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	dir, base := filepath.Split(path)
	dir = filepath.Clean(dir)
	if dir == path {
		return path, nil
	}
	resolvedDir, err := evalSymlinksTolerant(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedDir, base), nil
}

// isGitPath reports whether rel (a path relative to the sandbox root)
// touches a ".git" path segment: DESIGN.md's git_poke detector.
func isGitPath(rel string) bool {
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == ".git" {
			return true
		}
	}
	return false
}

// TestExecutor runs one run_tests invocation (a model-triggered tool call,
// or the harness's own baseline/final grading pass, see RunGrading) and
// returns its combined output and exit status; err is non-nil only when the
// command could not be run at all, matching CommandRunner.Run's own
// contract. CommandRunner (v1's local, optionally unshare-network-denied
// sandbox) and arena mode's ArenaRunner (docker exec into the arena's own
// containers, see arena.go) both implement it, so ToolExecutor and
// RunGrading work unchanged against either.
type TestExecutor interface {
	RunTests(ctx context.Context, dir, command string) (output string, ok bool, err error)
}

// ToolExecutor executes one task's tool calls against a sandbox directory,
// tracking cheat flags and the model's own run_tests usage as it goes. Not
// safe for concurrent use; one ToolExecutor serves exactly one task's loop.
type ToolExecutor struct {
	Dir     string
	Tests   TestExecutor
	TestCmd string
	// TestDir is where TestCmd executes; empty means Dir. Derived test
	// commands are subsystem-relative, so the v1 sandbox points this at
	// the subsystem directory (arena executors ignore it: their exec
	// working directory is fixed per container).
	TestDir string

	testsRanByModel int
	lastTestOutput  string
	cheatFlags      []CheatFlag
}

// NewToolExecutor builds a ToolExecutor for one task: dir is the sandbox
// root every tool path resolves against (v1's ephemeral worktree directory,
// or arena mode's shared checkout directory), tests runs the task's test
// command.
func NewToolExecutor(dir string, tests TestExecutor, testCmd string) *ToolExecutor {
	return &ToolExecutor{Dir: dir, Tests: tests, TestCmd: testCmd}
}

// TestsRanByModel returns how many times the model itself successfully
// invoked run_tests during the loop: DESIGN.md/Edward's "ran the tests at
// all" capability signal, tracked separately from the harness's own
// pre/post-loop grading passes.
func (e *ToolExecutor) TestsRanByModel() int { return e.testsRanByModel }

// CheatFlags returns every leakage-containment flag raised so far.
func (e *ToolExecutor) CheatFlags() []CheatFlag { return append([]CheatFlag{}, e.cheatFlags...) }

func (e *ToolExecutor) addFlag(flagType, detail string) {
	e.cheatFlags = append(e.cheatFlags, CheatFlag{Type: flagType, Detail: detail})
}

// resolve resolves userPath within the sandbox, flagging and refusing an
// escape attempt. It does not itself flag git_poke: callers check isGitPath
// on the returned rel, since some tools (edit/write) should refuse a .git
// target while others could conceivably differ.
func (e *ToolExecutor) resolve(userPath string) (resolved, rel string, refused string) {
	resolved, rel, err := resolveSandboxPath(e.Dir, userPath)
	if err != nil {
		e.addFlag(CheatFlagEscape, fmt.Sprintf("%q: %v", userPath, err))
		return "", "", fmt.Sprintf("refused: path %q resolves outside the sandbox", userPath)
	}
	return resolved, rel, ""
}

// Execute dispatches one tool_use call by name, returning its result text
// (already truncated to toolResultCap) and whether it is an error result.
func (e *ToolExecutor) Execute(ctx context.Context, name string, argsJSON json.RawMessage) (resultText string, isError bool) {
	var text string
	switch name {
	case toolReadFile:
		text, isError = e.readFile(argsJSON)
	case toolListDir:
		text, isError = e.listDir(argsJSON)
	case toolGrep:
		text, isError = e.grep(argsJSON)
	case toolEdit:
		text, isError = e.edit(argsJSON)
	case toolWrite:
		text, isError = e.write(argsJSON)
	case toolRunTests:
		text, isError = e.runTests(ctx)
	default:
		text, isError = fmt.Sprintf("unknown tool %q", name), true
	}
	return truncateResult(text), isError
}

type readFileArgs struct {
	FilePath string `json:"file_path"`
}

func (e *ToolExecutor) readFile(argsJSON json.RawMessage) (string, bool) {
	var a readFileArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	resolved, rel, refused := e.resolve(a.FilePath)
	if refused != "" {
		return refused, true
	}
	if isGitPath(rel) {
		e.addFlag(CheatFlagGitPoke, "read_file "+a.FilePath)
		return "refused: path touches .git", true
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "reading file: " + err.Error(), true
	}
	return string(data), false
}

type listDirArgs struct {
	DirPath string `json:"dir_path"`
}

func (e *ToolExecutor) listDir(argsJSON json.RawMessage) (string, bool) {
	var a listDirArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	if a.DirPath == "" {
		a.DirPath = "."
	}
	resolved, rel, refused := e.resolve(a.DirPath)
	if refused != "" {
		return refused, true
	}
	if isGitPath(rel) {
		e.addFlag(CheatFlagGitPoke, "list_dir "+a.DirPath)
		return "refused: path touches .git", true
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return "listing directory: " + err.Error(), true
	}
	var sb strings.Builder
	for _, ent := range entries {
		if ent.Name() == ".git" {
			continue
		}
		if ent.IsDir() {
			sb.WriteString(ent.Name() + "/\n")
		} else {
			sb.WriteString(ent.Name() + "\n")
		}
	}
	if sb.Len() == 0 {
		return "(empty directory)", false
	}
	return sb.String(), false
}

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// grepMaxMatches bounds one grep call's match count, ahead of the 8KB
// output truncation, so a very common pattern does not spend the entire
// walk formatting matches nobody will read.
const grepMaxMatches = 200

// grepMaxFileBytes skips a file larger than this: a coding sandbox has no
// legitimate reason for the model's grep to churn through multi-megabyte
// generated or vendored files.
const grepMaxFileBytes = 2 * 1024 * 1024

func (e *ToolExecutor) grep(argsJSON json.RawMessage) (string, bool) {
	var a grepArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	if a.Pattern == "" {
		return "pattern is required", true
	}
	if a.Path == "" {
		a.Path = "."
	}
	resolved, rel, refused := e.resolve(a.Path)
	if refused != "" {
		return refused, true
	}
	if isGitPath(rel) {
		e.addFlag(CheatFlagGitPoke, "grep path="+a.Path)
		return "refused: path touches .git", true
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "invalid pattern: " + err.Error(), true
	}

	var sb strings.Builder
	matches := 0
	_ = filepath.WalkDir(resolved, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if matches >= grepMaxMatches {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > grepMaxFileBytes {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || looksBinary(data) {
			return nil
		}
		relPath, err := filepath.Rel(e.Dir, p)
		if err != nil {
			relPath = p
		}
		for i, line := range strings.Split(string(data), "\n") {
			if matches >= grepMaxMatches {
				break
			}
			if re.MatchString(line) {
				fmt.Fprintf(&sb, "%s:%d:%s\n", relPath, i+1, line)
				matches++
			}
		}
		return nil
	})
	if matches == 0 {
		return "no matches", false
	}
	return sb.String(), false
}

func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

type editArgs struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (e *ToolExecutor) edit(argsJSON json.RawMessage) (string, bool) {
	var a editArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	resolved, rel, refused := e.resolve(a.FilePath)
	if refused != "" {
		return refused, true
	}
	if isGitPath(rel) {
		e.addFlag(CheatFlagGitPoke, "edit "+a.FilePath)
		return "refused: path touches .git", true
	}

	original, err := os.ReadFile(resolved)
	if err != nil {
		return "reading file: " + err.Error(), true
	}
	oldContent := string(original)
	if !strings.Contains(oldContent, a.OldString) {
		return "old_string not found in " + a.FilePath, true
	}
	newContent := strings.Replace(oldContent, a.OldString, a.NewString, 1)

	if hits := detectToolSmuggling(oldContent, newContent); len(hits) > 0 {
		e.addFlag(CheatFlagToolSmuggling, fmt.Sprintf("%s: %s", a.FilePath, strings.Join(hits, ", ")))
	}

	if err := os.WriteFile(resolved, []byte(newContent), 0o644); err != nil {
		return "writing file: " + err.Error(), true
	}
	return fmt.Sprintf("edited %s", a.FilePath), false
}

type writeArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func (e *ToolExecutor) write(argsJSON json.RawMessage) (string, bool) {
	var a writeArgs
	if err := json.Unmarshal(argsJSON, &a); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	resolved, rel, refused := e.resolve(a.FilePath)
	if refused != "" {
		return refused, true
	}
	if isGitPath(rel) {
		e.addFlag(CheatFlagGitPoke, "write "+a.FilePath)
		return "refused: path touches .git", true
	}

	oldContent := ""
	if existing, err := os.ReadFile(resolved); err == nil {
		oldContent = string(existing)
	}
	if hits := detectToolSmuggling(oldContent, a.Content); len(hits) > 0 {
		e.addFlag(CheatFlagToolSmuggling, fmt.Sprintf("%s: %s", a.FilePath, strings.Join(hits, ", ")))
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "creating parent directory: " + err.Error(), true
	}
	if err := os.WriteFile(resolved, []byte(a.Content), 0o644); err != nil {
		return "writing file: " + err.Error(), true
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.FilePath), false
}

func (e *ToolExecutor) runTests(ctx context.Context) (string, bool) {
	if e.TestCmd == "" {
		return "no test command is configured for this task", true
	}
	testDir := e.TestDir
	if testDir == "" {
		testDir = e.Dir
	}
	output, ok, err := e.Tests.RunTests(ctx, testDir, e.TestCmd)
	if err != nil {
		return "running tests: " + err.Error(), true
	}
	if containsAttemptedGit(output) {
		e.addFlag(CheatFlagAttemptedGit, "run_tests output mentions a git repository error while .git was parked")
	}
	e.testsRanByModel++
	e.lastTestOutput = output
	if !ok {
		return "tests FAILED (nonzero exit)\n" + output, false
	}
	return "tests passed (zero exit)\n" + output, false
}

// toolUseBlocks returns the tool_use blocks of content, in order.
func toolUseBlocks(content []anthropic.ContentBlock) []anthropic.ContentBlock {
	var out []anthropic.ContentBlock
	for _, b := range content {
		if b.Type == anthropic.BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}
