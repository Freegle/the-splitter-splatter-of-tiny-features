package evals

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// diffHunk is one old_string -> new_string replacement reconstructed from
// a unified diff hunk: context lines bracket the change on either side (as
// far as the diff's context window reaches), giving old_string a real
// chance of matching exactly one place in the file and giving a
// pure-insertion or pure-deletion hunk an anchor at all.
type diffHunk struct {
	Old string
	New string
}

// parsedFileDiff is one file's reconstructed hunks plus a coarse changed
// (+ or -) line count, the evidence behind characteristics.size.diff_lines
// for a seed-history task.
type parsedFileDiff struct {
	Hunks        []diffHunk
	ChangedLines int
}

// parseUnifiedDiff reads `git diff -U<n>` output for a single file and
// reconstructs each hunk's old_string/new_string pair: context (" ") and
// removed ("-") lines build old_string, context and added ("+") lines
// build new_string, in the order they appear in the hunk. A "\ No newline
// at end of file" marker line is ignored. Anything before the first "@@ "
// hunk header (the diff/index/---/+++ preamble) is skipped.
func parseUnifiedDiff(diffText string) parsedFileDiff {
	var result parsedFileDiff
	var oldLines, newLines []string
	inHunk := false

	flush := func() {
		if inHunk {
			result.Hunks = append(result.Hunks, diffHunk{
				Old: strings.Join(oldLines, "\n"),
				New: strings.Join(newLines, "\n"),
			})
		}
		oldLines = nil
		newLines = nil
	}

	for _, line := range strings.Split(diffText, "\n") {
		switch {
		case strings.HasPrefix(line, "@@ "):
			flush()
			inHunk = true
		case !inHunk:
			continue
		case strings.HasPrefix(line, "\\"):
			continue
		case strings.HasPrefix(line, "-"):
			oldLines = append(oldLines, line[1:])
			result.ChangedLines++
		case strings.HasPrefix(line, "+"):
			newLines = append(newLines, line[1:])
			result.ChangedLines++
		case strings.HasPrefix(line, " "):
			text := line[1:]
			oldLines = append(oldLines, text)
			newLines = append(newLines, text)
		default:
			// The trailing empty string from splitting a newline-terminated
			// stream, or any other unrecognised line shape: contributes
			// nothing rather than corrupting the reconstruction.
		}
	}
	flush()
	return result
}

// editEntry is one MultiEdit array element's JSON shape.
type editEntry struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// editInput is the Edit tool's input JSON shape.
type editInput struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// multiEditInput is the MultiEdit tool's input JSON shape.
type multiEditInput struct {
	FilePath string      `json:"file_path"`
	Edits    []editEntry `json:"edits"`
}

// writeInput is the Write tool's input JSON shape.
type writeInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// editToolUseBlock builds an Edit (single hunk) or MultiEdit (multiple
// hunks) tool_use content block reconstructing path's change as hunks.
func editToolUseBlock(id, path string, hunks []diffHunk) anthropic.ContentBlock {
	if len(hunks) == 1 {
		input, _ := json.Marshal(editInput{FilePath: path, OldString: hunks[0].Old, NewString: hunks[0].New})
		return anthropic.ContentBlock{Type: anthropic.BlockToolUse, ID: id, Name: "Edit", Input: input}
	}
	entries := make([]editEntry, len(hunks))
	for i, h := range hunks {
		entries[i] = editEntry{OldString: h.Old, NewString: h.New}
	}
	input, _ := json.Marshal(multiEditInput{FilePath: path, Edits: entries})
	return anthropic.ContentBlock{Type: anthropic.BlockToolUse, ID: id, Name: "MultiEdit", Input: input}
}

// writeToolUseBlock builds a Write tool_use content block for a new file.
func writeToolUseBlock(id, path, content string) anthropic.ContentBlock {
	input, _ := json.Marshal(writeInput{FilePath: path, Content: content})
	return anthropic.ContentBlock{Type: anthropic.BlockToolUse, ID: id, Name: "Write", Input: input}
}

// seedReferenceMessage is the wire shape buildSeedReferenceMessage
// marshals: a complete Anthropic assistant message, the same envelope
// shape internal/backend.FromOpenAI produces, so a synthesized reference
// response is interchangeable with a captured one anywhere that shape is
// expected (internal/verify.Verify reads only its content array).
type seedReferenceMessage struct {
	ID         string                   `json:"id"`
	Type       string                   `json:"type"`
	Role       string                   `json:"role"`
	Model      string                   `json:"model"`
	Content    []anthropic.ContentBlock `json:"content"`
	StopReason string                   `json:"stop_reason"`
	Usage      anthropic.Usage          `json:"usage"`
}

// buildSeedReferenceMessage wraps blocks (one Edit/MultiEdit/Write
// tool_use per touched file) in a complete synthesized assistant message.
func buildSeedReferenceMessage(blocks []anthropic.ContentBlock) ([]byte, error) {
	msg := seedReferenceMessage{
		ID: "msg_seed_history", Type: "message", Role: "assistant", Model: "synthesized-reference",
		Content: blocks, StopReason: "tool_use",
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling synthesized reference message: %w", err)
	}
	return b, nil
}

// ApplyReconstructedEdits applies content's Edit/MultiEdit/Write tool_use
// blocks to originalContent, literal old_string -> new_string replacement
// (first occurrence only, matching internal/verify's non-replace_all
// semantics), in block order. Originally added so this package's own tests
// could confirm a seed-history reference response reapplies to reproduce a
// commit's actual post-state content exactly (DESIGN.md's round-trip
// requirement); exported so internal/agentic's suspect_copy detector can
// reuse it too, to reconstruct the withheld reference fix's final file
// content from a task's frozen reference_response_zstd without duplicating
// this apply logic.
func ApplyReconstructedEdits(originalContent string, blocks []anthropic.ContentBlock) (string, error) {
	content := originalContent
	for _, b := range blocks {
		if b.Type != anthropic.BlockToolUse {
			continue
		}
		switch b.Name {
		case "Write":
			var in writeInput
			if err := json.Unmarshal(b.Input, &in); err != nil {
				return "", fmt.Errorf("decoding Write input: %w", err)
			}
			content = in.Content
		case "Edit":
			var in editInput
			if err := json.Unmarshal(b.Input, &in); err != nil {
				return "", fmt.Errorf("decoding Edit input: %w", err)
			}
			applied, err := applyOneEdit(content, in.OldString, in.NewString)
			if err != nil {
				return "", err
			}
			content = applied
		case "MultiEdit":
			var in multiEditInput
			if err := json.Unmarshal(b.Input, &in); err != nil {
				return "", fmt.Errorf("decoding MultiEdit input: %w", err)
			}
			for _, e := range in.Edits {
				applied, err := applyOneEdit(content, e.OldString, e.NewString)
				if err != nil {
					return "", err
				}
				content = applied
			}
		}
	}
	return content, nil
}

func applyOneEdit(content, oldString, newString string) (string, error) {
	if !strings.Contains(content, oldString) {
		return "", fmt.Errorf("old_string not found: %q", oldString)
	}
	return strings.Replace(content, oldString, newString, 1), nil
}
