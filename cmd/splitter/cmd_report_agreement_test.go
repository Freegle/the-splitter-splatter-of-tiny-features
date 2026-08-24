package main

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

func TestTopReasonsByCategory(t *testing.T) {
	rows := []store.DisagreementReason{
		{TurnType: "single_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{String: `{"equivalent":false,"confidence":0.8,"reason":"missed edge case"}`, Valid: true}},
		{TurnType: "single_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{String: `{"equivalent":false,"confidence":0.7,"reason":"missed edge case"}`, Valid: true}},
		{TurnType: "single_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{String: `{"equivalent":false,"confidence":0.6,"reason":"different formatting"}`, Valid: true}},
		{TurnType: "single_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{String: `{"equivalent":false,"confidence":0.6,"reason":"wrong variable name"}`, Valid: true}},
		{TurnType: "single_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{String: `{"equivalent":false,"confidence":0.6,"reason":"off by one"}`, Valid: true}},
		// A row decided by the exact/ast stage (no judge_verdict) contributes no reason.
		{TurnType: "single_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{}},
		// A different category must not mix into the first one's counts.
		{TurnType: "multi_file_edit", Subsystem: "internal", JudgeVerdict: sql.NullString{String: `{"equivalent":false,"confidence":0.5,"reason":"missed edge case"}`, Valid: true}},
	}

	got := topReasonsByCategory(rows)

	list, ok := got[categoryKey("single_file_edit", "internal")]
	if !ok {
		t.Fatalf("no entry for single_file_edit|internal in %+v", got)
	}
	if len(list) != topReasonsPerCategory {
		t.Fatalf("len(list) = %d, want %d (capped)", len(list), topReasonsPerCategory)
	}
	if list[0].Reason != "missed edge case" || list[0].Count != 2 {
		t.Errorf("list[0] = %+v, want missed edge case x2 (most frequent first)", list[0])
	}
	// "different formatting", "off by one" and "wrong variable name" are
	// tied at count 1; alphabetical order breaks the tie deterministically.
	if list[1].Count != 1 || list[2].Count != 1 {
		t.Errorf("list[1:3] = %+v, want both at count 1", list[1:])
	}
	if list[1].Reason != "different formatting" || list[2].Reason != "off by one" {
		t.Errorf("tie-break order = [%s, %s], want alphabetical [different formatting, off by one]", list[1].Reason, list[2].Reason)
	}

	other, ok := got[categoryKey("multi_file_edit", "internal")]
	if !ok || len(other) != 1 || other[0].Reason != "missed edge case" || other[0].Count != 1 {
		t.Errorf("multi_file_edit|internal = %+v, want one row: missed edge case x1", other)
	}
}

func TestFormatReasons(t *testing.T) {
	if got := formatReasons(nil); got != "-" {
		t.Errorf("formatReasons(nil) = %q, want -", got)
	}
	got := formatReasons([]reasonCount{{Reason: "a", Count: 2}, {Reason: "b", Count: 1}})
	if got != "a (2); b (1)" {
		t.Errorf("formatReasons = %q, want %q", got, "a (2); b (1)")
	}
}

func TestWriteEditTurnJudgeShare(t *testing.T) {
	var buf bytes.Buffer
	writeEditTurnJudgeShare(&buf, 10, 3)
	got := buf.String()
	if !strings.Contains(got, "3/10") || !strings.Contains(got, "30.0%") {
		t.Errorf("writeEditTurnJudgeShare output = %q, want it to mention 3/10 and 30.0%%", got)
	}
}

func TestWriteEditTurnJudgeShare_NoEditTurns(t *testing.T) {
	var buf bytes.Buffer
	writeEditTurnJudgeShare(&buf, 0, 0)
	got := buf.String()
	if !strings.Contains(got, "0/0") {
		t.Errorf("writeEditTurnJudgeShare(0,0) output = %q, want it to mention 0/0 without dividing by zero", got)
	}
}

func TestWriteJudgeSpend(t *testing.T) {
	var buf bytes.Buffer
	// 1,000,000 input tokens and 1,000,000 output tokens across 500 replays:
	// cost = $0.50 + $2.50 = $3.00 total, scaled to 100 replays = $0.60.
	writeJudgeSpend(&buf, 1_000_000, 1_000_000, 500)
	got := buf.String()
	if !strings.Contains(got, "200000 input tokens") || !strings.Contains(got, "200000 output tokens") {
		t.Errorf("writeJudgeSpend output = %q, want tokens scaled to per-100-replays", got)
	}
	if !strings.Contains(got, "$0.6000") {
		t.Errorf("writeJudgeSpend output = %q, want $0.6000", got)
	}
}

func TestWriteJudgeSpend_NoReplays(t *testing.T) {
	var buf bytes.Buffer
	writeJudgeSpend(&buf, 500, 100, 0)
	got := buf.String()
	if !strings.Contains(got, "no replays recorded yet") {
		t.Errorf("writeJudgeSpend(0 replays) output = %q, want it to say no replays recorded yet", got)
	}
}
