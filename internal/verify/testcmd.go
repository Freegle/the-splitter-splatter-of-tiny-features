package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// testCommandTimeout bounds one [tests] command invocation, per DESIGN.md.
const testCommandTimeout = 10 * time.Minute

// testPortBase and testPortSpacing derive the SPLITTER_PORT_BASE value
// injected into a test command's environment: side 0 (frontier) and side 1
// (local) get distinct bases so a test command that starts a server on
// SPLITTER_PORT_BASE + N does not collide between the two worktrees when
// both run at once.
const (
	testPortBase    = 20000
	testPortSpacing = 1000
)

// testEntry is one [tests] command result, the shape encoded into
// verifications.frontier_tests / local_tests.
type testEntry struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Output  string `json:"output,omitempty"`
}

// runTestCommand runs command via `sh -c` in dir, with SPLITTER_PORT_BASE
// set from side (0 or 1, see testPortBase/testPortSpacing), bounded by
// testCommandTimeout.
func runTestCommand(ctx context.Context, dir, command string, side int) testEntry {
	cctx, cancel := context.WithTimeout(ctx, testCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), fmt.Sprintf("SPLITTER_PORT_BASE=%d", testPortBase+side*testPortSpacing))

	out, err := cmd.CombinedOutput()
	return testEntry{Command: command, OK: err == nil, Output: strings.TrimSpace(string(out))}
}

// encodeTestResult marshals e to JSON, shrinking its Output field until
// the result fits resultCapBytes. Returns "" for the zero value (no
// command was configured/run).
func encodeTestResult(e testEntry) string {
	if e.Command == "" {
		return ""
	}
	original := e.Output
	limit := len(original)
	for {
		e.Output = truncateString(original, limit)
		b, err := json.Marshal(e)
		if err == nil && len(b) <= resultCapBytes {
			return string(b)
		}
		if limit == 0 {
			return `{"truncated":true}`
		}
		limit /= 2
	}
}
