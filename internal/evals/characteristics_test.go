package evals

import (
	"strings"
	"testing"
	"time"
)

func TestLanguage(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"empty", nil, ""},
		{"single go", []string{"internal/foo/bar.go"}, "go"},
		{"single vue", []string{"components/Foo.vue"}, "vue"},
		{"single php", []string{"app/Models/User.php"}, "php"},
		{"mixed go and vue", []string{"internal/foo.go", "components/Foo.vue"}, "mixed"},
		{"two go files stay go", []string{"a.go", "b.go"}, "go"},
		{"unrecognised extension only", []string{"image.png"}, ""},
		{"ts and js are distinct", []string{"a.ts", "b.js"}, "mixed"},
		{"sql", []string{"migrations/001_init.sql"}, "sql"},
		{"shell", []string{"scripts/deploy.sh"}, "shell"},
		{"markdown", []string{"docs/README.md"}, "markdown"},
		{"yaml", []string{"config/app.yml"}, "yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Language(tt.files); got != tt.want {
				t.Errorf("Language(%v) = %q, want %q", tt.files, got, tt.want)
			}
		})
	}
}

func TestLayer(t *testing.T) {
	layers := map[string]string{
		"iznik-nuxt3/":     "frontend-ui",
		"components/":      "frontend-ui",
		"pages/":           "frontend-ui",
		"*.vue":            "frontend-ui",
		"iznik-server-go/": "backend-api",
		"*handler*.go":     "backend-api",
		"migrations/":      "database",
		"*.sql":            "database",
		"docker*":          "infra",
		".circleci/":       "infra",
		"*_test.*":         "tests",
		"tests/":           "tests",
		"docs/":            "docs",
		"*.md":             "docs",
		"Makefile":         "build",
		"scripts/":         "build",
	}

	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"empty", nil, ""},
		{"nuxt dir prefix", []string{"iznik-nuxt3/pages/index.vue"}, "frontend-ui"},
		{"components dir", []string{"components/Foo.vue"}, "frontend-ui"},
		{"go handler file", []string{"iznik-server-go/user_handler.go"}, "backend-api"},
		{"migrations dir", []string{"migrations/001_init.sql"}, "database"},
		{"docker compose file glob", []string{"docker-compose.yml"}, "infra"},
		{"circleci dir", []string{".circleci/config.yml"}, "infra"},
		{"test file glob", []string{"internal/foo_test.go"}, "tests"},
		{"docs dir", []string{"docs/README.md"}, "docs"},
		{"bare markdown", []string{"README.md"}, "docs"},
		{"makefile exact", []string{"Makefile"}, "build"},
		{"scripts dir", []string{"scripts/install.sh"}, "build"},
		{"no match", []string{"random/thing.xyz"}, ""},
		{"uses only the first file", []string{"docs/README.md", "internal/foo.go"}, "docs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Layer(tt.files, layers); got != tt.want {
				t.Errorf("Layer(%v) = %q, want %q", tt.files, got, tt.want)
			}
		})
	}
}

func TestNature(t *testing.T) {
	layers := map[string]string{"tests/": "tests", "*_test.*": "tests"}

	tests := []struct {
		name    string
		subject string
		files   []string
		want    string
	}{
		{"fix keyword", "fix: nil pointer in handler", nil, "bugfix"},
		{"bug keyword", "bug in the ripple cap", nil, "bugfix"},
		{"revert keyword", "revert accidental change", nil, "bugfix"},
		{"add keyword", "add donate CTA to emails", nil, "feature"},
		{"feat keyword", "feat(chitchat): say hello", nil, "feature"},
		{"new keyword", "new endpoint for postcards", nil, "feature"},
		{"refactor keyword", "refactor the router package", nil, "refactor"},
		{"rename keyword", "rename internal/foo to internal/bar", nil, "refactor"},
		{"docs keyword", "docs: update coding standards", nil, "docs"},
		{"bump keyword", "bump go-sqlite to v1.57", nil, "config"},
		{"config keyword", "config: raise idle_minutes default", nil, "config"},
		{"only test files changed", "improve coverage", []string{"internal/foo_test.go"}, "test-writing"},
		{"no signal at all", "tidy things up", []string{"internal/foo.go"}, ""},
		{"fix wins over add when both present", "fix and add missing validation", nil, "bugfix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, evidence := Nature(tt.subject, tt.files, layers)
			if got != tt.want {
				t.Errorf("Nature(%q, %v) = %q, want %q (evidence %q)", tt.subject, tt.files, got, tt.want, evidence)
			}
			if got != "" && evidence == "" {
				t.Errorf("Nature(%q, %v) returned a label with no evidence", tt.subject, tt.files)
			}
		})
	}
}

func TestFramework(t *testing.T) {
	tests := []struct {
		name          string
		files         []string
		language      string
		contentSample string
		want          string
	}{
		{"vue extension", []string{"components/Foo.vue"}, "vue", "", "vue-nuxt"},
		{"nuxt path without vue extension", []string{"iznik-nuxt3/plugins/foo.js"}, "js", "", "vue-nuxt"},
		{"blade template", []string{"resources/views/foo.blade.php"}, "php", "", "laravel-blade"},
		{"go gorm import", []string{"internal/store/x.go"}, "go", `import "gorm.io/gorm"`, "go-gorm"},
		{"go fiber import", []string{"internal/api/x.go"}, "go", `import "github.com/gofiber/fiber/v2"`, "go-fiber"},
		{"go stdlib default", []string{"internal/store/x.go"}, "go", `import "net/http"`, "go-stdlib"},
		{"non-go non-vue language", []string{"a.sql"}, "sql", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Framework(tt.files, tt.language, tt.contentSample); got != tt.want {
				t.Errorf("Framework(%v, %q, ...) = %q, want %q", tt.files, tt.language, got, tt.want)
			}
		})
	}
}

func TestSpecClarity(t *testing.T) {
	tests := []struct {
		name   string
		brief  string
		files  []string
		bucket string
	}{
		{"terse", "fix bug", nil, "terse"},
		{"normal", "Fix the nil pointer dereference that happens when a chat has no members left in it.", nil, "normal"},
		{"detailed", func() string {
			s := ""
			for len(s) < 250 {
				s += "This is a very detailed brief describing exactly what should change and why. "
			}
			return s
		}(), nil, "detailed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bucket, evidence := SpecClarity(tt.brief, tt.files)
			if bucket != tt.bucket {
				t.Errorf("SpecClarity(%q) bucket = %q, want %q", tt.brief, bucket, tt.bucket)
			}
			if evidence == "" {
				t.Error("SpecClarity should always return non-empty evidence")
			}
		})
	}

	t.Run("names a touched file by stem", func(t *testing.T) {
		_, evidence := SpecClarity("fix the handler in user_handler.go", []string{"internal/api/user_handler.go"})
		if !strings.Contains(evidence, "names a file/function/route: true") {
			t.Errorf("evidence = %q, want it to record the brief naming a touched file", evidence)
		}
	})

	t.Run("does not claim a name when none present", func(t *testing.T) {
		_, evidence := SpecClarity("things are broken somewhere", []string{"internal/api/user_handler.go"})
		if !strings.Contains(evidence, "names a file/function/route: false") {
			t.Errorf("evidence = %q, want it to record no name found", evidence)
		}
	})
}

func TestRung(t *testing.T) {
	tests := []struct {
		name         string
		difficulty   string
		turnType     string
		contextBytes int
		want         int
	}{
		{"simple small", DifficultySimple, "single_file_edit", 100, 1},
		{"simple large context", DifficultySimple, "single_file_edit", 9000, 2},
		{"simple multi-file", DifficultySimple, "multi_file_edit", 100, 2},
		{"unknown small", "", "single_file_edit", 100, 3},
		{"unknown large", "", "multi_file_edit", 100, 4},
		{"challenging small", DifficultyChallenging, "single_file_edit", 100, 5},
		{"challenging multi-file", DifficultyChallenging, "multi_file_edit", 100, 6},
		{"challenging large context single file", DifficultyChallenging, "single_file_edit", 9000, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Rung(tt.difficulty, tt.turnType, tt.contextBytes); got != tt.want {
				t.Errorf("Rung(%q, %q, %d) = %d, want %d", tt.difficulty, tt.turnType, tt.contextBytes, got, tt.want)
			}
		})
	}
}

func TestGitArchaeologyDifficulty(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("commit subject itself is a fix", func(t *testing.T) {
		got, evidence := GitArchaeologyDifficulty("fix: nil pointer", base, nil)
		if got != DifficultyChallenging {
			t.Errorf("difficulty = %q, want %q", got, DifficultyChallenging)
		}
		if evidence == "" {
			t.Error("expected non-empty evidence")
		}
	})

	t.Run("follow-up fix within 14 days", func(t *testing.T) {
		followups := []FollowupCommit{
			{SHA: "abc123", Subject: "fix regression from previous commit", Date: base.Add(5 * 24 * time.Hour)},
		}
		got, evidence := GitArchaeologyDifficulty("add new feature", base, followups)
		if got != DifficultyChallenging {
			t.Errorf("difficulty = %q, want %q", got, DifficultyChallenging)
		}
		if !strings.Contains(evidence, "abc123") {
			t.Errorf("evidence = %q, want it to name the follow-up sha", evidence)
		}
	})

	t.Run("follow-up fix outside 14 days does not count", func(t *testing.T) {
		followups := []FollowupCommit{
			{SHA: "abc123", Subject: "fix regression", Date: base.Add(30 * 24 * time.Hour)},
		}
		got, _ := GitArchaeologyDifficulty("add new feature", base, followups)
		if got != DifficultySimple {
			t.Errorf("difficulty = %q, want %q", got, DifficultySimple)
		}
	})

	t.Run("no fix pattern anywhere is simple", func(t *testing.T) {
		got, _ := GitArchaeologyDifficulty("add new feature", base, []FollowupCommit{
			{SHA: "abc123", Subject: "add another feature", Date: base.Add(1 * 24 * time.Hour)},
		})
		if got != DifficultySimple {
			t.Errorf("difficulty = %q, want %q", got, DifficultySimple)
		}
	})

	t.Run("follow-up before the commit is ignored", func(t *testing.T) {
		got, _ := GitArchaeologyDifficulty("add new feature", base, []FollowupCommit{
			{SHA: "abc123", Subject: "fix something else entirely", Date: base.Add(-1 * 24 * time.Hour)},
		})
		if got != DifficultySimple {
			t.Errorf("difficulty = %q, want %q (a follow-up must come after the commit)", got, DifficultySimple)
		}
	})
}

func TestSizeBucket(t *testing.T) {
	tests := []struct {
		diffLines int
		want      string
	}{
		{5, "tiny"},
		{10, "tiny"},
		{11, "small"},
		{50, "small"},
		{51, "medium"},
		{200, "medium"},
		{201, "large"},
	}
	for _, tt := range tests {
		s := Size{DiffLines: tt.diffLines}
		if got := s.SizeBucket(); got != tt.want {
			t.Errorf("Size{DiffLines: %d}.SizeBucket() = %q, want %q", tt.diffLines, got, tt.want)
		}
	}
}

func TestCharacteristicsJSONRoundTrip(t *testing.T) {
	c := Characteristics{
		Framework:    "go-gorm",
		SpecClarity:  "normal",
		Size:         Size{Files: 2, DiffLines: 30, ContextBytes: 4096},
		TaskDate:     "2026-08-24T00:00:00Z",
		Localization: LocalizationGiven,
		BriefSource:  BriefSourceCommitSubject,
		CommitSHA:    "abc123",
		Evidence:     map[string]string{"nature": "subject keyword \"fix\""},
	}
	back := ParseCharacteristics(c.JSON())
	if back.Framework != c.Framework || back.CommitSHA != c.CommitSHA || back.Size.Files != c.Size.Files {
		t.Errorf("round trip mismatch: got %+v, want %+v", back, c)
	}
}

func TestParseCharacteristics_EmptyIsZeroValue(t *testing.T) {
	c := ParseCharacteristics("")
	if c.Framework != "" || c.CommitSHA != "" {
		t.Errorf("ParseCharacteristics(\"\") should be the zero value, got %+v", c)
	}
}
