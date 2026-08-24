package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// Edit-family tool names recognised in a response's tool_use blocks.
const (
	toolEdit         = "Edit"
	toolMultiEdit    = "MultiEdit"
	toolWrite        = "Write"
	toolNotebookEdit = "NotebookEdit"
)

// textEdit is one literal old_string -> new_string replacement, the unit
// both the Edit tool and each entry of MultiEdit's edits array describe.
type textEdit struct {
	Old        string
	New        string
	ReplaceAll bool
}

// fileEdit is one edit-family tool call targeting one file: FilePath is
// exactly as the tool call gave it (possibly absolute, not yet resolved
// into a worktree). Edits is used for Edit and MultiEdit, Content for
// Write; a NotebookEdit carries neither (application is unsupported, see
// applyFileEdits).
type fileEdit struct {
	Tool     string
	FilePath string
	Edits    []textEdit
	Content  string
}

// extractFileEdits scans a message's content blocks for edit-family
// tool_use calls, in content order, decoding each into a fileEdit. A block
// whose input cannot be decoded, or names a file, is skipped rather than
// failing the whole extraction: a malformed single tool call should not
// block scoring the rest of the response.
func extractFileEdits(msgJSON []byte) ([]fileEdit, error) {
	var msg messageContent
	if err := json.Unmarshal(msgJSON, &msg); err != nil {
		return nil, fmt.Errorf("decoding message content: %w", err)
	}

	var edits []fileEdit
	for _, b := range msg.Content {
		if b.Type != anthropic.BlockToolUse {
			continue
		}
		if fe, ok := decodeFileEdit(b); ok {
			edits = append(edits, fe)
		}
	}
	return edits, nil
}

func decodeFileEdit(b anthropic.ContentBlock) (fileEdit, bool) {
	switch b.Name {
	case toolEdit:
		var in struct {
			FilePath   string `json:"file_path"`
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all"`
		}
		if err := json.Unmarshal(b.Input, &in); err != nil || in.FilePath == "" {
			return fileEdit{}, false
		}
		return fileEdit{
			Tool:     toolEdit,
			FilePath: in.FilePath,
			Edits:    []textEdit{{Old: in.OldString, New: in.NewString, ReplaceAll: in.ReplaceAll}},
		}, true

	case toolMultiEdit:
		var in struct {
			FilePath string `json:"file_path"`
			Edits    []struct {
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(b.Input, &in); err != nil || in.FilePath == "" {
			return fileEdit{}, false
		}
		fe := fileEdit{Tool: toolMultiEdit, FilePath: in.FilePath}
		for _, e := range in.Edits {
			fe.Edits = append(fe.Edits, textEdit{Old: e.OldString, New: e.NewString, ReplaceAll: e.ReplaceAll})
		}
		return fe, true

	case toolWrite:
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(b.Input, &in); err != nil || in.FilePath == "" {
			return fileEdit{}, false
		}
		return fileEdit{Tool: toolWrite, FilePath: in.FilePath, Content: in.Content}, true

	case toolNotebookEdit:
		var in struct {
			NotebookPath string `json:"notebook_path"`
		}
		if err := json.Unmarshal(b.Input, &in); err != nil || in.NotebookPath == "" {
			return fileEdit{}, false
		}
		return fileEdit{Tool: toolNotebookEdit, FilePath: in.NotebookPath}, true

	default:
		return fileEdit{}, false
	}
}

// relativize resolves a tool call's file_path to a path relative to
// repoPath, so it can be located inside a comparison worktree. A relative
// input path is trusted as already repo-relative. An absolute path outside
// repoPath cannot be resolved and the second return is false.
func relativize(path, repoPath string) (string, bool) {
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		return filepath.Clean(path), true
	}
	if repoPath == "" {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(repoPath), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// applyFileEdits applies each fileEdit to worktreeDir, resolving
// FilePath relative to repoPath. It returns the repo-relative paths
// touched (attempted), in order, deduplicated, and a lint-shaped entry for
// every failure: a path that cannot be relativized into the worktree, an
// Edit/MultiEdit whose old_string is not found (its count must be >=1), or
// an unsupported NotebookEdit. A file that failed to apply is still
// counted as touched: it still participates in similarity scoring against
// whatever the other side produced, which correctly reflects disagreement
// rather than silently excluding the comparison.
func applyFileEdits(worktreeDir string, edits []fileEdit, repoPath string) ([]string, []lintEntry) {
	var touched []string
	var failures []lintEntry
	seen := map[string]bool{}

	markTouched := func(rel string) {
		if !seen[rel] {
			seen[rel] = true
			touched = append(touched, rel)
		}
	}

	for _, fe := range edits {
		rel, ok := relativize(fe.FilePath, repoPath)
		if !ok {
			failures = append(failures, lintEntry{Tool: "apply", OK: false, Output: fe.FilePath + ": path outside repo"})
			continue
		}
		full := filepath.Join(worktreeDir, rel)

		var applyErr error
		switch fe.Tool {
		case toolWrite:
			applyErr = applyWrite(full, fe.Content)
		case toolEdit, toolMultiEdit:
			applyErr = applyTextEdits(full, fe.Edits)
		case toolNotebookEdit:
			applyErr = fmt.Errorf("NotebookEdit application not supported")
		default:
			continue
		}

		markTouched(rel)
		if applyErr != nil {
			failures = append(failures, lintEntry{Tool: "apply", OK: false, Output: rel + ": " + applyErr.Error()})
		}
	}
	return touched, failures
}

func applyWrite(fullPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
}

// applyTextEdits applies edits to fullPath in order, as literal
// old_string -> new_string replacements (all occurrences when ReplaceAll,
// else the first occurrence only). Every old_string must occur at least
// once in the file's current content (after any earlier edits in this same
// call have already been applied); the first that does not aborts the
// whole call with no partial write, matching the real Edit/MultiEdit
// tools' atomic contract.
func applyTextEdits(fullPath string, edits []textEdit) error {
	original, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
	}
	content := string(original)
	for _, e := range edits {
		if strings.Count(content, e.Old) == 0 {
			return fmt.Errorf("old_string not found")
		}
		if e.ReplaceAll {
			content = strings.ReplaceAll(content, e.Old, e.New)
		} else {
			content = strings.Replace(content, e.Old, e.New, 1)
		}
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
}

// languageForPath maps a repo-relative file path's extension to the
// language name used as the first half of a thresholds override key.
// Returns "" for an empty path.
func languageForPath(relPath string) string {
	if relPath == "" {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(relPath))
	switch ext {
	case ".go":
		return "go"
	case ".php":
		return "php"
	case ".js", ".mjs", ".cjs":
		return "js"
	case ".ts":
		return "ts"
	case ".vue":
		return "vue"
	case ".py":
		return "python"
	case ".sql":
		return "sql"
	case ".sh":
		return "shell"
	case ".md":
		return "markdown"
	case ".yml", ".yaml":
		return "yaml"
	case "":
		return ""
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

// subsystemOf returns relPath's first path segment (the DESIGN.md
// subsystem rule: "first path segment of the first repo-relative file"),
// or "" when relPath has no directory component.
func subsystemOf(relPath string) string {
	idx := strings.IndexByte(relPath, '/')
	if idx < 0 {
		return ""
	}
	return relPath[:idx]
}
