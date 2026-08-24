package feature

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// editFamilyTools are the tool_use names whose input carries a touched file
// path: the tools turn_type rules 1-2 and files_touched key off.
var editFamilyTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// editFilePath extracts the file_path or notebook_path input field from an
// edit-family tool_use block. Empty when b is not an edit-family tool_use
// block, or neither input field is present.
func editFilePath(b anthropic.ContentBlock) string {
	if b.Type != anthropic.BlockToolUse || !editFamilyTools[b.Name] {
		return ""
	}
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	if err := json.Unmarshal(b.Input, &in); err != nil {
		return ""
	}
	if in.FilePath != "" {
		return in.FilePath
	}
	return in.NotebookPath
}

// FilesTouched extracts the file_path/notebook_path inputs of the
// edit-family tool_use blocks in respBlocks, in first-encountered order
// with duplicates removed, made repo-relative when the path is under
// repoPath (kept absolute otherwise).
func FilesTouched(respBlocks []anthropic.ContentBlock, repoPath string) []string {
	seen := map[string]bool{}
	var out []string
	for _, b := range respBlocks {
		fp := editFilePath(b)
		if fp == "" {
			continue
		}
		rel := repoRelative(fp, repoPath)
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	return out
}

// Subsystem returns the first path segment of filesTouched's first entry,
// or "" when filesTouched is empty.
func Subsystem(filesTouched []string) string {
	if len(filesTouched) == 0 {
		return ""
	}
	first := filesTouched[0]
	if idx := strings.IndexByte(first, '/'); idx >= 0 {
		return first[:idx]
	}
	return first
}

// repoRelative rewrites path to be relative to repoPath when it lies under
// it. A path outside repoPath, an empty repoPath, or an empty path is
// returned unchanged.
func repoRelative(path, repoPath string) string {
	if path == "" || repoPath == "" {
		return path
	}
	rel, err := filepath.Rel(filepath.Clean(repoPath), filepath.Clean(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}
