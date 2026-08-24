package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/feature"
)

func TestReportSpendVerb_Registered(t *testing.T) {
	if _, ok := reportCommands["spend"]; !ok {
		t.Fatal(`"spend" report sub-command not registered`)
	}
}

func TestWriteSpendTable_HeaderAndTotal(t *testing.T) {
	var buf bytes.Buffer
	writeSpendTable(&buf, []feature.SpendSummary{
		{TurnType: "single_file_edit", Calls: 2, ContextTokens: 1500000, OutputTokens: 150000, CostUSD: 4.75},
		{TurnType: "question_answer", Calls: 1, ContextTokens: 200000, OutputTokens: 10000, CostUSD: 1.25},
	})
	out := buf.String()

	if !strings.Contains(out, "turn_type") || !strings.Contains(out, "est_cost_usd") {
		t.Errorf("output missing expected header columns: %q", out)
	}
	if !strings.Contains(out, "single_file_edit") {
		t.Errorf("output missing single_file_edit row: %q", out)
	}
	if !strings.Contains(out, "TOTAL") {
		t.Errorf("output missing TOTAL row: %q", out)
	}
	if !strings.Contains(out, "6.0000") {
		t.Errorf("output TOTAL cost should sum to 6.0000, got: %q", out)
	}
}

func TestWriteSpendTable_EmptySummaries(t *testing.T) {
	var buf bytes.Buffer
	writeSpendTable(&buf, nil)
	out := buf.String()

	if !strings.Contains(out, "TOTAL") {
		t.Errorf("expected a TOTAL row even with no summaries, got: %q", out)
	}
}
