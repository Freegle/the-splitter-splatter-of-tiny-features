package agentic

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/evals"
)

// referenceFilePathArgs decodes the file_path field common to Edit,
// MultiEdit and Write tool_use inputs.
type referenceFilePathArgs struct {
	FilePath string `json:"file_path"`
}

// referenceEditsByPath groups a reference response's Edit/MultiEdit/Write
// tool_use blocks by the file they touch, in first-appearance order,
// excluding any path in excludePaths (the task's held-out test paths:
// suspect_copy only ever compares the withheld NON-test fix, DESIGN.md
// "the final model patch... compared to the withheld upstream fix").
func referenceEditsByPath(refContent []anthropic.ContentBlock, excludePaths map[string]bool) (order []string, byPath map[string][]anthropic.ContentBlock) {
	byPath = map[string][]anthropic.ContentBlock{}
	for _, b := range refContent {
		if b.Type != anthropic.BlockToolUse {
			continue
		}
		if b.Name != "Edit" && b.Name != "MultiEdit" && b.Name != "Write" {
			continue
		}
		var args referenceFilePathArgs
		if err := json.Unmarshal(b.Input, &args); err != nil || args.FilePath == "" {
			continue
		}
		if excludePaths[args.FilePath] {
			continue
		}
		if _, seen := byPath[args.FilePath]; !seen {
			order = append(order, args.FilePath)
		}
		byPath[args.FilePath] = append(byPath[args.FilePath], b)
	}
	return order, byPath
}

// snapshotReferenceFiles reads dir's current content for every path in
// paths, before the model's loop has touched anything: the "original
// content" ApplyReconstructedEdits needs to reconstruct each reference
// file's final (post-fix) content. A path that cannot be read (e.g. a
// reference Write block whose target did not exist in the parent tree) is
// simply omitted; its similarity contribution is computed against "" by
// the caller, which is the correct behaviour for a genuinely new file.
func snapshotReferenceFiles(dir string, paths []string) map[string]string {
	snapshot := make(map[string]string, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(filepath.Join(dir, p))
		if err == nil {
			snapshot[p] = string(data)
		}
	}
	return snapshot
}

// EvaluateSuspectCopy compares the sandbox's final content for every
// non-test file the withheld reference fix touched against that
// reference's own reconstructed final content, and returns a CheatFlag
// when the mean per-file similarity clears the diffLines-weighted
// threshold (DetectSuspectCopy). parentSnapshot is each reference-touched
// path's content at sandbox creation time (before the model ran, from
// snapshotReferenceFiles), the "original content" basis
// evals.ApplyReconstructedEdits needs. Returns nil when there is nothing
// to compare (no non-test reference edits) or on any decode error (a
// malformed reference is not itself cheating evidence).
func EvaluateSuspectCopy(sandboxDir string, refMsgJSON []byte, excludeTestPaths map[string]bool, parentSnapshot map[string]string, diffLines int) *CheatFlag {
	var refMsg struct {
		Content []anthropic.ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(refMsgJSON, &refMsg); err != nil {
		return nil
	}

	order, byPath := referenceEditsByPath(refMsg.Content, excludeTestPaths)
	if len(order) == 0 {
		return nil
	}

	var total float64
	for _, path := range order {
		refFinal, err := evals.ApplyReconstructedEdits(parentSnapshot[path], byPath[path])
		if err != nil {
			continue
		}
		modelFinal := ""
		if data, rerr := os.ReadFile(filepath.Join(sandboxDir, path)); rerr == nil {
			modelFinal = string(data)
		}
		total += tokenSimilarity(modelFinal, refFinal)
	}
	similarity := total / float64(len(order))

	return DetectSuspectCopy(similarity, diffLines)
}
