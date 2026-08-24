package agentic

import (
	"reflect"
	"testing"

	"github.com/freegle/splitter/internal/evals"
)

func TestParseGoTestResults(t *testing.T) {
	output := `{"Action":"run","Test":"TestA"}
{"Action":"output","Test":"TestA","Output":"=== RUN   TestA\n"}
{"Action":"pass","Test":"TestA","Elapsed":0}
{"Action":"run","Test":"TestB"}
{"Action":"fail","Test":"TestB","Elapsed":0}
not json at all
{"Action":"pass","Test":"TestB/subtest"}
`
	got := parseGoTestResults(output)
	want := map[string]bool{"TestA": true, "TestB": false, "TestB/subtest": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGoTestResults = %v, want %v", got, want)
	}
}

func TestParseGoTestResults_LastActionWins(t *testing.T) {
	// A retried/flaky reporting shape: whichever pass/fail line for a test
	// name comes LAST determines its recorded outcome.
	output := `{"Action":"fail","Test":"TestFlaky"}
{"Action":"pass","Test":"TestFlaky"}
`
	got := parseGoTestResults(output)
	if !got["TestFlaky"] {
		t.Errorf("parseGoTestResults last-action-wins: got %v, want TestFlaky=true", got)
	}
}

func TestScoreFailToPass_HappyPath(t *testing.T) {
	baseline := GradeResult{ByTest: map[string]bool{"TestHeldOut": false, "TestExisting": true}}
	final := GradeResult{ByTest: map[string]bool{"TestHeldOut": true, "TestExisting": true}}

	testsRan, testsPassed, regressions := ScoreFailToPass(baseline, final, []string{"TestHeldOut"})
	if testsRan != 1 || testsPassed != 1 || regressions != 0 {
		t.Errorf("ScoreFailToPass = (%d,%d,%d), want (1,1,0)", testsRan, testsPassed, regressions)
	}
}

func TestScoreFailToPass_Regression(t *testing.T) {
	baseline := GradeResult{ByTest: map[string]bool{"TestHeldOut": false, "TestExisting": true}}
	// The fix makes the held-out test pass, but breaks a previously
	// passing, non-held-out test in the same package.
	final := GradeResult{ByTest: map[string]bool{"TestHeldOut": true, "TestExisting": false}}

	testsRan, testsPassed, regressions := ScoreFailToPass(baseline, final, []string{"TestHeldOut"})
	if testsRan != 1 || testsPassed != 1 {
		t.Errorf("ScoreFailToPass held-out counts = (%d,%d), want (1,1)", testsRan, testsPassed)
	}
	if regressions != 1 {
		t.Errorf("ScoreFailToPass regressions = %d, want 1", regressions)
	}
}

func TestScoreFailToPass_HeldOutNeverRan(t *testing.T) {
	// A build failure (or a wrong package) means the held-out test name
	// never appears in the final pass at all: not counted as ran or passed.
	baseline := GradeResult{ByTest: map[string]bool{}}
	final := GradeResult{ByTest: map[string]bool{}}

	testsRan, testsPassed, regressions := ScoreFailToPass(baseline, final, []string{"TestHeldOut"})
	if testsRan != 0 || testsPassed != 0 || regressions != 0 {
		t.Errorf("ScoreFailToPass = (%d,%d,%d), want (0,0,0)", testsRan, testsPassed, regressions)
	}
}

func TestScoreFailToPass_CoarseNonGoTask(t *testing.T) {
	baseline := GradeResult{}
	final := GradeResult{Exit0: true}

	testsRan, testsPassed, regressions := ScoreFailToPass(baseline, final, nil)
	if testsRan != 1 || testsPassed != 1 || regressions != 0 {
		t.Errorf("ScoreFailToPass coarse pass = (%d,%d,%d), want (1,1,0)", testsRan, testsPassed, regressions)
	}

	final.Exit0 = false
	testsRan, testsPassed, regressions = ScoreFailToPass(baseline, final, nil)
	if testsRan != 1 || testsPassed != 0 || regressions != 0 {
		t.Errorf("ScoreFailToPass coarse fail = (%d,%d,%d), want (1,0,0)", testsRan, testsPassed, regressions)
	}
}

func TestHeldOutTestNames(t *testing.T) {
	payload := evals.HoldoutPayload{
		Files: []evals.HoldoutFile{
			{Path: "a_test.go", IsNew: true, Content: "package a\n\nfunc TestOne(t *testing.T) {}\n\nfunc TestTwo(t *testing.T) {}\n"},
		},
	}
	got := HeldOutTestNames(payload)
	want := []string{"TestOne", "TestTwo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HeldOutTestNames = %v, want %v", got, want)
	}
}

func TestHeldOutTestNames_FromHunks(t *testing.T) {
	// Modified test files hold Hunks (an unexported evals type), whose Old/New
	// fields are still readable from this package: exercise that path.
	holdoutJSON := `{"files":[{"path":"b_test.go","is_new":false,"hunks":[{"Old":"","New":"func TestFromHunk(t *testing.T) {}\n"}]}]}`
	payload, err := evals.DecodeHoldoutPayload([]byte(holdoutJSON))
	if err != nil {
		t.Fatalf("DecodeHoldoutPayload: %v", err)
	}
	got := HeldOutTestNames(payload)
	want := []string{"TestFromHunk"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HeldOutTestNames = %v, want %v", got, want)
	}
}
