package feature

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

func TestEditFilePath(t *testing.T) {
	tests := []struct {
		name string
		b    anthropic.ContentBlock
		want string
	}{
		{
			name: "Edit file_path",
			b:    editToolUse("Edit", "/repo/a.go"),
			want: "/repo/a.go",
		},
		{
			name: "NotebookEdit notebook_path",
			b: anthropic.ContentBlock{
				Type: anthropic.BlockToolUse, Name: "NotebookEdit",
				Input: json.RawMessage(`{"notebook_path":"/repo/nb.ipynb"}`),
			},
			want: "/repo/nb.ipynb",
		},
		{
			name: "non edit-family tool ignored",
			b:    toolUse("Bash"),
			want: "",
		},
		{
			name: "text block ignored",
			b:    textBlock("hi"),
			want: "",
		},
		{
			name: "unparseable input yields empty",
			b:    anthropic.ContentBlock{Type: anthropic.BlockToolUse, Name: "Edit", Input: json.RawMessage(`not json`)},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := editFilePath(tt.b); got != tt.want {
				t.Errorf("editFilePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFilesTouched(t *testing.T) {
	tests := []struct {
		name     string
		blocks   []anthropic.ContentBlock
		repoPath string
		want     []string
	}{
		{
			name: "repo-relative conversion",
			blocks: []anthropic.ContentBlock{
				editToolUse("Edit", "/repo/internal/feature/foo.go"),
			},
			repoPath: "/repo",
			want:     []string{"internal/feature/foo.go"},
		},
		{
			name: "outside repo kept absolute",
			blocks: []anthropic.ContentBlock{
				editToolUse("Write", "/etc/hosts"),
			},
			repoPath: "/repo",
			want:     []string{"/etc/hosts"},
		},
		{
			name: "duplicates removed, order preserved",
			blocks: []anthropic.ContentBlock{
				editToolUse("Edit", "/repo/a.go"),
				editToolUse("Edit", "/repo/b.go"),
				editToolUse("Edit", "/repo/a.go"),
			},
			repoPath: "/repo",
			want:     []string{"a.go", "b.go"},
		},
		{
			name:     "no edit blocks yields nil",
			blocks:   []anthropic.ContentBlock{textBlock("hi"), toolUse("Bash")},
			repoPath: "/repo",
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilesTouched(tt.blocks, tt.repoPath)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("FilesTouched() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestSubsystem(t *testing.T) {
	tests := []struct {
		name         string
		filesTouched []string
		want         string
	}{
		{name: "nested path", filesTouched: []string{"internal/feature/foo.go"}, want: "internal"},
		{name: "top level file", filesTouched: []string{"README.md"}, want: "README.md"},
		{name: "empty", filesTouched: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Subsystem(tt.filesTouched); got != tt.want {
				t.Errorf("Subsystem() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepoRelative(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		repoPath string
		want     string
	}{
		{name: "under repo", path: "/repo/a/b.go", repoPath: "/repo", want: "a/b.go"},
		{name: "outside repo", path: "/other/a.go", repoPath: "/repo", want: "/other/a.go"},
		{name: "empty repo path", path: "/repo/a.go", repoPath: "", want: "/repo/a.go"},
		{name: "empty path", path: "", repoPath: "/repo", want: ""},
		{name: "equal to repo path", path: "/repo", repoPath: "/repo", want: "/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := repoRelative(tt.path, tt.repoPath); got != tt.want {
				t.Errorf("repoRelative() = %q, want %q", got, tt.want)
			}
		})
	}
}
