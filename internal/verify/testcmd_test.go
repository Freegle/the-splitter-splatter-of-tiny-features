package verify

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunTestCommand_Success(t *testing.T) {
	dir := t.TempDir()
	entry := runTestCommand(context.Background(), dir, "true", 0)
	if !entry.OK {
		t.Errorf("runTestCommand OK = false, want true")
	}
	if entry.Command != "true" {
		t.Errorf("runTestCommand Command = %q, want %q", entry.Command, "true")
	}
}

func TestRunTestCommand_Failure(t *testing.T) {
	dir := t.TempDir()
	entry := runTestCommand(context.Background(), dir, "echo boom; exit 1", 0)
	if entry.OK {
		t.Errorf("runTestCommand OK = true, want false")
	}
	if entry.Output != "boom" {
		t.Errorf("runTestCommand Output = %q, want %q", entry.Output, "boom")
	}
}

func TestRunTestCommand_PortBaseBySide(t *testing.T) {
	dir := t.TempDir()
	entry0 := runTestCommand(context.Background(), dir, "printenv SPLITTER_PORT_BASE", 0)
	if !entry0.OK {
		t.Fatalf("runTestCommand side 0 failed: %v", entry0.Output)
	}
	if entry0.Output != "20000" {
		t.Errorf("runTestCommand side 0 Output = %q, want %q", entry0.Output, "20000")
	}

	entry1 := runTestCommand(context.Background(), dir, "printenv SPLITTER_PORT_BASE", 1)
	if !entry1.OK {
		t.Fatalf("runTestCommand side 1 failed: %v", entry1.Output)
	}
	if entry1.Output != "21000" {
		t.Errorf("runTestCommand side 1 Output = %q, want %q", entry1.Output, "21000")
	}
}

func TestRunTestCommand_RunsInDir(t *testing.T) {
	dir := t.TempDir()
	entry := runTestCommand(context.Background(), dir, "pwd", 0)
	if !entry.OK {
		t.Fatalf("runTestCommand failed: %v", entry.Output)
	}

	wantResolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks on temp dir: %v", err)
	}
	gotResolved, err := filepath.EvalSymlinks(entry.Output)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks on pwd output: %v", err)
	}

	if gotResolved != wantResolved {
		t.Errorf("runTestCommand Output resolved = %q, want %q", gotResolved, wantResolved)
	}
}

func TestEncodeTestResult_Empty(t *testing.T) {
	got := encodeTestResult(testEntry{})
	if got != "" {
		t.Errorf("encodeTestResult(empty) = %q, want empty string", got)
	}
}

func TestEncodeTestResult_LargeOutputFitsCap(t *testing.T) {
	e := testEntry{
		Command: "go test",
		OK:      true,
		Output:  strings.Repeat("x", 10000),
	}
	result := encodeTestResult(e)

	if len(result) > resultCapBytes {
		t.Errorf("encodeTestResult length = %d, want <= %d", len(result), resultCapBytes)
	}

	var gotEntry testEntry
	if err := json.Unmarshal([]byte(result), &gotEntry); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if gotEntry.Command != "go test" {
		t.Errorf("Command = %q, want %q", gotEntry.Command, "go test")
	}
	if !gotEntry.OK {
		t.Errorf("OK = false, want true")
	}
}
