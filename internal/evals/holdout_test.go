package evals

import (
	"reflect"
	"testing"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func TestSplitTestFiles(t *testing.T) {
	layers := config.Default().Layers
	files := []string{"internal/greet/greet.go", "internal/greet/greet_test.go", "docs/readme.md"}

	testFiles, nonTestFiles := SplitTestFiles(files, layers)

	if want := []string{"internal/greet/greet_test.go"}; !reflect.DeepEqual(testFiles, want) {
		t.Errorf("testFiles = %v, want %v", testFiles, want)
	}
	if want := []string{"internal/greet/greet.go", "docs/readme.md"}; !reflect.DeepEqual(nonTestFiles, want) {
		t.Errorf("nonTestFiles = %v, want %v", nonTestFiles, want)
	}
}

func TestGoTestCommand(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"empty", nil, ""},
		{"one package", []string{"internal/greet/greet_test.go"}, "go test -json ./internal/greet/..."},
		{
			"two packages, deduped and ordered by first appearance",
			[]string{"internal/greet/greet_test.go", "internal/util/util_test.go", "internal/greet/other_test.go"},
			"go test -json ./internal/greet/... ./internal/util/...",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goTestCommand(c.paths); got != c.want {
				t.Errorf("goTestCommand(%v) = %q, want %q", c.paths, got, c.want)
			}
		})
	}
}

// TestSeedHistory_HoldoutPayloadForTestFileCommit builds a real throwaway
// git repo whose feature commit changes both a Go source file and its
// _test.go file, seeds it, and asserts the resulting task carries a holdout
// payload with a derived Go test command scoped to that package: the input
// internal/agentic's fail-to-pass grading needs.
func TestSeedHistory_HoldoutPayloadForTestFileCommit(t *testing.T) {
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-q", "-b", "main")
	writeSeedFile(t, repoPath, ".gitattributes", "* text=auto eol=lf\n")
	commitGit(t, repoPath, "root", "2024-01-01T00:00:00Z")

	writeSeedFile(t, repoPath, "internal/greet/greet.go",
		"package greet\n\nfunc Greet(name string) string {\n\treturn \"Hello, World!\"\n}\n")
	writeSeedFile(t, repoPath, "internal/greet/greet_test.go",
		"package greet\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"World\") != \"Hello, World!\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n}\n")
	commitGit(t, repoPath, "add greet function with a test", "2024-01-02T00:00:00Z")

	writeSeedFile(t, repoPath, "internal/greet/greet.go",
		"package greet\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name + \"!\"\n}\n")
	writeSeedFile(t, repoPath, "internal/greet/greet_test.go",
		"package greet\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif Greet(\"World\") != \"Hello, World!\" {\n\t\tt.Fatal(\"wrong\")\n\t}\n\tif Greet(\"Alice\") != \"Hello, Alice!\" {\n\t\tt.Fatal(\"ignores name\")\n\t}\n}\n")
	commitGit(t, repoPath, "fix: greet ignores the name parameter", "2024-01-03T00:00:00Z")

	db := openRunTestDB(t)
	cfg := config.Default()

	summary, err := SeedHistory(db, cfg, SeedHistoryOptions{RepoPath: repoPath, Since: "2020-01-01"})
	if err != nil {
		t.Fatalf("SeedHistory: %v", err)
	}
	if summary.Inserted == 0 {
		t.Fatalf("expected at least one task inserted, summary=%+v", summary)
	}

	tasks, err := store.EvalTasksByOrigin(db, OriginHistory)
	if err != nil {
		t.Fatalf("EvalTasksByOrigin: %v", err)
	}

	var found *store.EvalTaskRow
	for i, task := range tasks {
		c := ParseCharacteristics(task.Characteristics.String)
		if c.CommitSHA != "" && task.Brief != "" && len(task.HoldoutTestsZstd) > 0 {
			found = &tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no seeded task carries a holdout payload; tasks=%+v", tasks)
	}

	holdoutJSON, err := store.Decompress(found.HoldoutTestsZstd)
	if err != nil {
		t.Fatalf("decompressing holdout payload: %v", err)
	}
	payload, err := DecodeHoldoutPayload(holdoutJSON)
	if err != nil {
		t.Fatalf("DecodeHoldoutPayload: %v", err)
	}
	if len(payload.Files) != 1 || payload.Files[0].Path != "internal/greet/greet_test.go" {
		t.Errorf("payload.Files = %+v, want one file internal/greet/greet_test.go", payload.Files)
	}
	if want := "go test -json ./internal/greet/..."; payload.TestCmd != want {
		t.Errorf("payload.TestCmd = %q, want %q", payload.TestCmd, want)
	}

	c := ParseCharacteristics(found.Characteristics.String)
	if c.AgenticTestCmd != payload.TestCmd {
		t.Errorf("characteristics.AgenticTestCmd = %q, want %q", c.AgenticTestCmd, payload.TestCmd)
	}
}
