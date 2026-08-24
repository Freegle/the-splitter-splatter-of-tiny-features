package evals

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Size records an eval task's scope: file/line counts. DESIGN.md: size is
// non-monotonic (SWE-bench: 51-200 line patches resolve BETTER than 1-10
// line precision edits), so it is recorded and bucketed in scorecards but
// NEVER used to derive the difficulty label.
type Size struct {
	Files        int `json:"files"`
	DiffLines    int `json:"diff_lines"`
	ContextBytes int `json:"context_bytes"`
}

// SizeBucket buckets s for scorecard grouping: "tiny" (<=10 diff lines,
// the SWE-bench precision-edit band), "small" (<=50), "medium" (<=200,
// SWE-bench's best-resolving band), "large" (above that).
func (s Size) SizeBucket() string {
	switch {
	case s.DiffLines <= 10:
		return "tiny"
	case s.DiffLines <= 50:
		return "small"
	case s.DiffLines <= 200:
		return "medium"
	default:
		return "large"
	}
}

// ReverseBriefState tracks eval reverse-briefs' submit/poll progress for
// one history-origin task, stored inside its characteristics JSON so no
// extra table or column is needed to track a pending batch.
type ReverseBriefState struct {
	Status   string `json:"status"` // "submitted" or "done"
	BatchID  string `json:"batch_id,omitempty"`
	CustomID string `json:"custom_id,omitempty"`
}

// Characteristics is the full mechanically-derived profile recorded in
// eval_tasks.characteristics. language/layer/nature/difficulty also live as
// their own eval_tasks columns (DESIGN.md's schema); this struct is the
// source of truth for those labels' evidence plus every dimension that has
// no dedicated column (framework, spec_clarity, size, task_date,
// localization, brief_source).
type Characteristics struct {
	Framework    string             `json:"framework,omitempty"`
	SpecClarity  string             `json:"spec_clarity,omitempty"`
	Size         Size               `json:"size"`
	TaskDate     string             `json:"task_date,omitempty"`
	Localization string             `json:"localization,omitempty"`
	BriefSource  string             `json:"brief_source,omitempty"`
	CommitSHA    string             `json:"commit_sha,omitempty"`
	ReverseBrief *ReverseBriefState `json:"reverse_brief,omitempty"`
	// AgenticTestCmd is the shell command internal/agentic runs (network
	// denied) to grade this task's held-out tests, derived at seed-history
	// time from the held-out test files' language. Empty when this task has
	// no holdout payload, or its holdout tests are not in a language
	// seed-history knows how to run (see DECISIONS.md: Go only for now).
	AgenticTestCmd string `json:"agentic_test_cmd,omitempty"`
	// Evidence records, per dimension name, the mechanical reasoning behind
	// that dimension's label (language/layer/nature/difficulty/framework/
	// spec_clarity all get an entry when derived).
	Evidence map[string]string `json:"evidence,omitempty"`
}

// JSON marshals c. A marshal failure (never expected for this struct) is
// reported as a JSON object carrying the error text, so a caller storing
// the result in a NOT-NULL-adjacent TEXT column never has to handle a
// second error path.
func (c Characteristics) JSON() string {
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Sprintf(`{"marshal_error":%q}`, err.Error())
	}
	return string(b)
}

// ParseCharacteristics decodes a characteristics JSON string. An empty or
// unparseable string yields the zero value rather than an error: every
// caller in this package treats a missing profile as "nothing derived yet"
// rather than a fatal condition.
func ParseCharacteristics(s string) Characteristics {
	var c Characteristics
	if s == "" {
		return c
	}
	_ = json.Unmarshal([]byte(s), &c)
	return c
}

// setEvidence records why into c's evidence map for dimension, creating the
// map on first use.
func (c *Characteristics) setEvidence(dimension, why string) {
	if c.Evidence == nil {
		c.Evidence = map[string]string{}
	}
	c.Evidence[dimension] = why
}

// languageByExt maps a lowercase file extension to the eval_tasks.language
// taxonomy DESIGN.md's schema comment pins: go|php|js|ts|vue|sql|shell|
// markdown|yaml.
var languageByExt = map[string]string{
	".go":   "go",
	".php":  "php",
	".vue":  "vue",
	".js":   "js",
	".mjs":  "js",
	".cjs":  "js",
	".jsx":  "js",
	".ts":   "ts",
	".tsx":  "ts",
	".sql":  "sql",
	".sh":   "shell",
	".bash": "shell",
	".md":   "markdown",
	".yml":  "yaml",
	".yaml": "yaml",
}

// Language derives eval_tasks.language from files' extensions: the single
// recognised language when every file agrees, "mixed" when more than one
// distinct language is touched, "" when no file has a recognised extension.
func Language(files []string) string {
	set := map[string]bool{}
	for _, f := range files {
		if lang, ok := languageByExt[strings.ToLower(filepath.Ext(f))]; ok {
			set[lang] = true
		}
	}
	switch len(set) {
	case 0:
		return ""
	case 1:
		for lang := range set {
			return lang
		}
	}
	return "mixed"
}

// Layer derives eval_tasks.layer from the FIRST touched file's path
// against the configured [layers] pattern table (config.Config.Layers),
// mirroring internal/feature.Subsystem's "first touched file" convention.
// Patterns are tried in sorted key order for determinism when more than one
// would match. Returns "" when files is empty or the first file matches no
// configured pattern.
func Layer(files []string, layers map[string]string) string {
	if len(files) == 0 {
		return ""
	}
	return layerForPath(files[0], layers)
}

func layerForPath(path string, layers map[string]string) string {
	patterns := make([]string, 0, len(layers))
	for p := range layers {
		patterns = append(patterns, p)
	}
	sortStrings(patterns)
	for _, p := range patterns {
		if layerPatternMatches(p, path) {
			return layers[p]
		}
	}
	return ""
}

// layerPatternMatches reports whether pattern (a config [layers] key, e.g.
// "iznik-nuxt3/", "*.vue", "docker*", "*handler*.go") matches path. A
// pattern ending in "/" matches when any path segment equals it (after
// dropping the trailing slash), globs and all, since these entries name a
// directory anywhere in the path rather than a leading prefix only.
// Anything else is matched with filepath.Match against the path's base
// name, falling back to the whole path (so a pattern like "docs/" without
// a trailing slash, or an accidental typo, still has a chance to match).
func layerPatternMatches(pattern, path string) bool {
	p := strings.ToLower(pattern)
	lp := strings.ToLower(path)

	if strings.HasSuffix(p, "/") {
		dir := strings.TrimSuffix(p, "/")
		for _, seg := range strings.Split(lp, "/") {
			if ok, _ := filepath.Match(dir, seg); ok {
				return true
			}
		}
		return false
	}

	base := lp
	if idx := strings.LastIndex(lp, "/"); idx >= 0 {
		base = lp[idx+1:]
	}
	if ok, _ := filepath.Match(p, base); ok {
		return true
	}
	ok, _ := filepath.Match(p, lp)
	return ok
}

// sortStrings sorts ss in place, ascending. A tiny local helper so this
// file does not need to import "sort" solely for one call site's clarity
// at the call site above.
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j-1] > ss[j]; j-- {
			ss[j-1], ss[j] = ss[j], ss[j-1]
		}
	}
}

// natureKeywordGroups maps commit-subject keywords to eval_tasks.nature
// values, in priority order (first matching group wins), per DESIGN.md.
var natureKeywordGroups = []struct {
	nature string
	words  []string
}{
	{"bugfix", []string{"fix", "bug", "revert"}},
	{"feature", []string{"add", "feat", "new"}},
	{"refactor", []string{"refactor", "rename", "move", "extract"}},
	{"docs", []string{"docs"}},
	{"config", []string{"bump", "config", "setting"}},
}

// Nature derives eval_tasks.nature from commitSubject's keywords first
// (priority order above), falling back to the diff-shape rule ("only test
// files changed" -> test-writing per DESIGN.md) when no keyword matches.
// Returns ("", "") when neither signal fires.
func Nature(commitSubject string, touchedFiles []string, layers map[string]string) (nature, evidence string) {
	lower := strings.ToLower(commitSubject)
	for _, group := range natureKeywordGroups {
		for _, w := range group.words {
			if strings.Contains(lower, w) {
				return group.nature, fmt.Sprintf("commit subject contains keyword %q", w)
			}
		}
	}
	if len(touchedFiles) > 0 && allFilesAreTests(touchedFiles, layers) {
		return "test-writing", "diff touches only test files"
	}
	return "", ""
}

func allFilesAreTests(files []string, layers map[string]string) bool {
	for _, f := range files {
		if layerForPath(f, layers) != "tests" {
			return false
		}
	}
	return true
}

// Framework derives the characteristics.framework tag: .vue/nuxt paths ->
// vue-nuxt, *.blade.php -> laravel-blade, else for go tasks a cheap
// substring check of contentSample (whatever file content the caller
// already has in hand, never fetched specially for this) for gorm/fiber
// imports, defaulting to go-stdlib. Returns "" for every other language,
// since DESIGN.md only asks for this facet on the Vue/Nuxt, Laravel-Blade
// and Go axes relevant to this codebase family.
func Framework(files []string, language, contentSample string) string {
	for _, f := range files {
		lf := strings.ToLower(f)
		if strings.HasSuffix(lf, ".vue") || strings.Contains(lf, "nuxt") {
			return "vue-nuxt"
		}
		if strings.HasSuffix(lf, ".blade.php") {
			return "laravel-blade"
		}
	}
	if language == "go" {
		switch {
		case strings.Contains(contentSample, "gorm.io/gorm"):
			return "go-gorm"
		case strings.Contains(contentSample, "gofiber/fiber"):
			return "go-fiber"
		default:
			return "go-stdlib"
		}
	}
	return ""
}

// funcCallPattern matches a bare `name()` style token, one mechanical
// signal that a brief names a specific function.
var funcCallPattern = regexp.MustCompile(`\w+\(\)`)

// SpecClarity buckets brief by length (terse <60 chars, detailed >240,
// else normal) and records, as evidence, whether it names one of
// touchedFiles by stem, a function-call-shaped token, or a path (contains
// "/"): DESIGN.md's mechanical proxy for "does the brief name a file,
// function or route".
func SpecClarity(brief string, touchedFiles []string) (bucket, evidence string) {
	n := len([]rune(brief))
	switch {
	case n < 60:
		bucket = "terse"
	case n > 240:
		bucket = "detailed"
	default:
		bucket = "normal"
	}
	named := namesTarget(brief, touchedFiles)
	evidence = fmt.Sprintf("brief length %d chars (%s), names a file/function/route: %t", n, bucket, named)
	return bucket, evidence
}

func namesTarget(brief string, touchedFiles []string) bool {
	lower := strings.ToLower(brief)
	for _, f := range touchedFiles {
		base := filepath.Base(f)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem != "" && strings.Contains(lower, strings.ToLower(stem)) {
			return true
		}
	}
	if funcCallPattern.MatchString(brief) {
		return true
	}
	return strings.Contains(brief, "/")
}

// fixSubjectPattern flags a commit subject as describing a fix, the
// evidence rule seed-history's git archaeology difficulty derivation uses
// both for the commit itself and for candidate follow-up commits.
var fixSubjectPattern = regexp.MustCompile(`(?i)(fix|bug|revert|typo|oops|correct|broke)`)

// FollowupCommit is one later commit considered as evidence that an
// earlier commit needed correcting.
type FollowupCommit struct {
	SHA     string
	Subject string
	Date    time.Time
}

// followupWindow bounds how long after a commit a later commit still
// counts as revisiting it, per DESIGN.md.
const followupWindow = 14 * 24 * time.Hour

// GitArchaeologyDifficulty derives a seed-history task's difficulty from
// git evidence: challenging when the commit's own subject matches the fix
// pattern (it encodes a task a previous change got wrong), or when any
// followups entry within followupWindow of commitDate also matches;
// simple otherwise. followups need not be pre-filtered by date, this
// function applies the window itself. evidence names the matched subject
// or follow-up sha.
func GitArchaeologyDifficulty(commitSubject string, commitDate time.Time, followups []FollowupCommit) (difficulty, evidence string) {
	if fixSubjectPattern.MatchString(commitSubject) {
		return DifficultyChallenging, fmt.Sprintf("commit subject itself matches a fix pattern: %q", commitSubject)
	}
	for _, f := range followups {
		delta := f.Date.Sub(commitDate)
		if delta < 0 || delta > followupWindow {
			continue
		}
		if fixSubjectPattern.MatchString(f.Subject) {
			return DifficultyChallenging, fmt.Sprintf("follow-up commit %s within 14 days matches a fix pattern: %q", f.SHA, f.Subject)
		}
	}
	return DifficultySimple, "no fix-pattern follow-up within 14 days and the commit subject is not itself a fix"
}
