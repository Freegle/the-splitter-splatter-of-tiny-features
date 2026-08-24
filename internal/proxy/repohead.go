// Adapted from seifghazi/claude-code-proxy (MIT), commit 02c9c766.
package proxy

import (
	"os"
	"path/filepath"
	"strings"
)

// readRepoHead returns the commit sha repoPath's working tree is checked
// out at, read directly from the .git directory (never by shelling out to
// git). Returns "" when repoPath is empty or is not a git repository or
// worktree; this is best-effort metadata, never a request failure.
func readRepoHead(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	gitDir := resolveGitDir(repoPath)
	if gitDir == "" {
		return ""
	}
	return resolveHEAD(gitDir)
}

// resolveGitDir returns the .git directory for repoPath, following a
// worktree's ".git" FILE (which contains "gitdir: <path>") to the real
// per-worktree git directory when repoPath's ".git" is not itself a
// directory.
func resolveGitDir(repoPath string) string {
	gitPath := filepath.Join(repoPath, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return gitPath
	}

	data, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(content, prefix) {
		return ""
	}
	dir := strings.TrimSpace(strings.TrimPrefix(content, prefix))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir
}

// resolveHEAD reads gitDir/HEAD and resolves it to a commit sha: a
// detached HEAD holds the sha directly, otherwise HEAD is "ref: <path>"
// and the sha is read from that ref file. A worktree's own gitDir keeps a
// "commondir" file pointing at the main repository's git directory, where
// branch refs actually live; that is tried first, falling back to gitDir
// itself and finally to a packed-refs entry.
func resolveHEAD(gitDir string) string {
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(data))

	const refPrefix = "ref:"
	if !strings.HasPrefix(head, refPrefix) {
		return head
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, refPrefix))

	commonDir := gitDir
	if cd, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		commonDir = c
	}

	if sha := readRefFile(commonDir, ref); sha != "" {
		return sha
	}
	if sha := readRefFile(gitDir, ref); sha != "" {
		return sha
	}
	return readPackedRef(commonDir, ref)
}

func readRefFile(dir, ref string) string {
	data, err := os.ReadFile(filepath.Join(dir, ref))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readPackedRef looks up ref in dir/packed-refs, the flat file git uses to
// store refs that have not been repacked into loose files under refs/.
func readPackedRef(dir, ref string) string {
	data, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) == 2 && parts[1] == ref {
			return parts[0]
		}
	}
	return ""
}
