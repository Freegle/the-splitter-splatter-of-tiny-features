package feature

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/freegle/splitter/internal/anthropic"
)

// MinLabelledFixtures is the minimum number of testdata/labelled/*.json
// fixtures TestFeaturiser_AgreesWithLabelledSample requires. It rises to 50
// when the real captured 50-call labelled sample (BRIEF.md Phase 2
// acceptance) replaces this synthetic-but-realistic set.
const MinLabelledFixtures = 24

// minAgreementRate is the Phase 2 acceptance bar: turn_type agreement
// against the labelled sample must be at least this fraction.
const minAgreementRate = 0.8

// labelledFixture is the on-disk shape of one testdata/labelled/*.json
// file: a captured request/response pair plus the turn_type a human would
// assign it.
type labelledFixture struct {
	Label    string                    `json:"label"`
	Request  anthropic.MessagesRequest `json:"request"`
	Response labelledFixtureResponse   `json:"response"`
}

type labelledFixtureResponse struct {
	Content []anthropic.ContentBlock `json:"content"`
}

// labelledFixturesDir is testdata/labelled relative to this package
// directory: DESIGN.md's layout puts testdata/ at the repo root, two
// levels up from internal/feature.
const labelledFixturesDir = "../../testdata/labelled"

func loadLabelledFixtures(t *testing.T) []labelledFixture {
	t.Helper()

	entries, err := os.ReadDir(labelledFixturesDir)
	if err != nil {
		t.Fatalf("reading %s: %v", labelledFixturesDir, err)
	}

	var fixtures []labelledFixture
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(labelledFixturesDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var f labelledFixture
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if f.Label == "" {
			t.Fatalf("%s: missing \"label\" field", path)
		}
		fixtures = append(fixtures, f)
	}
	return fixtures
}

func TestFeaturiser_AgreesWithLabelledSample(t *testing.T) {
	fixtures := loadLabelledFixtures(t)

	if len(fixtures) < MinLabelledFixtures {
		t.Fatalf("found %d labelled fixtures, want at least %d", len(fixtures), MinLabelledFixtures)
	}

	agree := 0
	for _, f := range fixtures {
		got := ClassifyTurnType(f.Request, f.Response.Content)
		if got == f.Label {
			agree++
		} else {
			t.Logf("disagreement: predicted %q, labelled %q", got, f.Label)
		}
	}

	rate := float64(agree) / float64(len(fixtures))
	t.Logf("turn_type agreement: %d/%d (%.1f%%)", agree, len(fixtures), rate*100)
	if rate < minAgreementRate {
		t.Errorf("agreement rate %.3f is below the required %.3f", rate, minAgreementRate)
	}
}

// TestLabelledFixtures_CoverEveryTurnType asserts every turn_type appears
// at least once in the labelled sample, so the agreement check above
// cannot pass by accident on a sample that never exercises a whole rule.
func TestLabelledFixtures_CoverEveryTurnType(t *testing.T) {
	fixtures := loadLabelledFixtures(t)

	seen := map[string]bool{}
	for _, f := range fixtures {
		seen[f.Label] = true
	}

	for _, want := range []string{
		TurnToolResultSummary, TurnSingleFileEdit, TurnMultiFileEdit,
		TurnPlan, TurnQuestionAnswer, TurnOther,
	} {
		if !seen[want] {
			t.Errorf("no labelled fixture has label %q", want)
		}
	}
}
