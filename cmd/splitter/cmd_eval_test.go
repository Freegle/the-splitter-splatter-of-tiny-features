package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/evals"
)

func TestEvalCommand_Registered(t *testing.T) {
	if _, ok := commands["eval"]; !ok {
		t.Fatal(`"eval" command not registered`)
	}
}

func TestRunEval_UnknownSubCommand(t *testing.T) {
	if err := runEval([]string{"bogus"}); err == nil {
		t.Error("expected an error for an unknown eval sub-command")
	}
	if err := runEval(nil); err == nil {
		t.Error("expected an error when no sub-command is given")
	}
}

func TestTruncateOneLine(t *testing.T) {
	if got := truncateOneLine("short", 60); got != "short" {
		t.Errorf("truncateOneLine short = %q", got)
	}
	long := strings.Repeat("x", 100)
	got := truncateOneLine(long, 10)
	if got != strings.Repeat("x", 10)+"..." {
		t.Errorf("truncateOneLine long = %q", got)
	}
	if got := truncateOneLine("line one\nline two", 60); strings.Contains(got, "\n") {
		t.Errorf("truncateOneLine should flatten newlines, got %q", got)
	}
}

func TestFormatPassRates(t *testing.T) {
	if got := formatPassRates(nil); got != "-" {
		t.Errorf("formatPassRates(nil) = %q, want -", got)
	}
	got := formatPassRates(map[string]evals.PassRate{
		"model-b": {Passed: 1, Total: 2},
		"model-a": {Passed: 3, Total: 3},
	})
	want := "model-a:3/3, model-b:1/2"
	if got != want {
		t.Errorf("formatPassRates = %q, want %q (sorted by model name)", got, want)
	}
}

func TestWriteEvalListTable(t *testing.T) {
	var buf bytes.Buffer
	writeEvalListTable(&buf, []evals.ListRow{
		{
			ID: 1, Origin: "history", ShortSHA: "abc12345", Brief: "fix the thing",
			Characteristics: "go/backend-api/bugfix/challenging",
			PassRates:       map[string]evals.PassRate{"qwen2.5-coder:7b": {Passed: 2, Total: 5}},
		},
	})
	out := buf.String()
	for _, want := range []string{"id", "origin", "sha", "history", "abc12345", "fix the thing", "qwen2.5-coder:7b:2/5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
}

func TestPrintEvalRunSummary(t *testing.T) {
	var buf bytes.Buffer
	sc := &evals.Scorecard{ByDimension: map[string]map[string]evals.ScorecardEntry{
		"language": {"go": {N: 3, Passed: 2}},
	}}
	printEvalRunSummary(&buf, &evals.RunSummary{
		RunID: 5, Backend: "ollama", Model: "qwen2.5-coder:7b",
		TasksTotal: 3, TasksScored: 3, TasksPassed: 2, TasksSkipped: 0,
		TokensIn: 300, TokensOut: 60,
		Ladder:       map[string]evals.TrackSummary{"go": {Rungs: map[string]evals.RungSummary{"1": {N: 3, Passed: 2}}}},
		Scorecard:    sc,
		BriefSources: map[string]int{"session": 3},
		PriorModel:   "claude-sonnet-4-6",
		Regressions:  []evals.Regression{{TaskID: 7, ShortSHA: "deadbeef", Brief: "a regressed task"}},
	})
	out := buf.String()
	for _, want := range []string{"run=5", "qwen2.5-coder:7b", "tasks passed:  2", "language:", "go", "regressions vs prior run (model=claude-sonnet-4-6): 1", "a regressed task"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintEvalRunSummary_NoPriorRun(t *testing.T) {
	var buf bytes.Buffer
	printEvalRunSummary(&buf, &evals.RunSummary{
		RunID: 1, Backend: "ollama", Model: "m1",
		Scorecard:    &evals.Scorecard{ByDimension: map[string]map[string]evals.ScorecardEntry{}},
		BriefSources: map[string]int{},
		Ladder:       map[string]evals.TrackSummary{},
	})
	if !strings.Contains(buf.String(), "no prior run") {
		t.Errorf("expected a message about no prior run, got: %q", buf.String())
	}
}
