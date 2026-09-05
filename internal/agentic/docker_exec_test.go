package agentic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStubDocker installs an executable shell script named "docker" at the front of
// PATH so exec.CommandContext finds it instead of any real docker. script must
// start with a #!/bin/sh line.
func withStubDocker(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing docker stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestExecDockerExecer_Success(t *testing.T) {
	withStubDocker(t, `#!/bin/sh
echo "hello world"
exit 0`)

	execer := execDockerExecer{}
	output, ok, err := execer.Exec(context.Background(), []string{"exec", "apiv2", "sh", "-c", "echo hello"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !ok {
		t.Errorf("ok = false, want true")
	}
	if output != "hello world\n" {
		t.Errorf("output = %q, want %q", output, "hello world\n")
	}
}

func TestExecDockerExecer_NonZeroExitIsNotAnError(t *testing.T) {
	withStubDocker(t, `#!/bin/sh
echo "test failed: assertion error"
exit 1`)

	execer := execDockerExecer{}
	output, ok, err := execer.Exec(context.Background(), []string{"exec", "apiv2", "sh", "-c", "go test ./..."})
	// A non-zero exit must NOT be reported as an error: a failing command inside a
	// healthy docker is a test result, and turning it into an error would make every
	// red test look like broken infrastructure.
	if err != nil {
		t.Fatalf("Exec returned an error for a non-zero exit: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
	if !strings.Contains(output, "test failed: assertion error") {
		t.Errorf("output = %q, expected to contain failure text", output)
	}
}

func TestExecDockerExecer_CapturesStderr(t *testing.T) {
	withStubDocker(t, `#!/bin/sh
echo "stderr message" >&2
exit 1`)

	execer := execDockerExecer{}
	output, ok, err := execer.Exec(context.Background(), []string{"exec", "apiv2", "sh", "-c", "echo stderr >&2; exit 1"})
	// A non-zero exit must NOT be reported as an error: a failing command inside a
	// healthy docker is a test result, and turning it into an error would make every
	// red test look like broken infrastructure.
	if err != nil {
		t.Fatalf("Exec returned an error for a non-zero exit: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
	if !strings.Contains(output, "stderr message") {
		t.Errorf("output = %q, expected to contain stderr text", output)
	}
}

func TestExecDockerExecer_BinaryMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	execer := execDockerExecer{}
	output, ok, err := execer.Exec(context.Background(), []string{"exec", "apiv2", "sh", "-c", "echo hello"})
	if ok {
		t.Errorf("ok = true, want false")
	}
	if err == nil {
		t.Errorf("err = nil, want non-nil error")
	}
	if !strings.Contains(err.Error(), "running docker command") {
		t.Errorf("err = %v, expected to contain 'running docker command'", err)
	}
	if output != "" {
		t.Errorf("output = %q, want empty", output)
	}
}

func TestExecDockerExecer_PassesArgumentsThrough(t *testing.T) {
	tmpfile := filepath.Join(t.TempDir(), "args.txt")
	withStubDocker(t, `#!/bin/sh
printf '%s\n' "$@" > "$ARG_FILE"
exit 0`)
	t.Setenv("ARG_FILE", tmpfile)

	execer := execDockerExecer{}
	args := []string{"exec", "-w", "/app", "apiv2", "sh", "-c", "go test ./..."}
	_, ok, err := execer.Exec(context.Background(), args)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !ok {
		t.Errorf("ok = false, want true")
	}

	content, err := os.ReadFile(tmpfile)
	if err != nil {
		t.Fatalf("reading args file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != len(args) {
		t.Fatalf("expected %d lines, got %d", len(args), len(lines))
	}
	for i, line := range lines {
		if line != args[i] {
			t.Errorf("line %d = %q, want %q", i, line, args[i])
		}
	}
	for _, line := range lines {
		if line == "docker" {
			t.Errorf("args file contains 'docker', but it should not be in the argument list")
		}
	}
}

func TestExecDockerExecer_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	execer := execDockerExecer{}
	output, ok, err := execer.Exec(ctx, []string{"exec", "apiv2", "sh", "-c", "echo hello"})
	if ok {
		t.Errorf("ok = true, want false")
	}
	if err == nil {
		t.Errorf("err = nil, want non-nil error")
	}
	if output != "" {
		t.Errorf("output = %q, want empty", output)
	}
}
