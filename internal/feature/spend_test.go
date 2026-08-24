package feature

import (
	"math"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

func TestSpend_AggregatesAndPricesPerRow(t *testing.T) {
	rows := []store.SpendRow{
		{TurnType: TurnSingleFileEdit, Model: "claude-sonnet-4-6", ContextTokens: 1_000_000, OutputTokens: 100_000},
		{TurnType: TurnSingleFileEdit, Model: "claude-haiku-4-5", ContextTokens: 500_000, OutputTokens: 50_000},
		{TurnType: TurnQuestionAnswer, Model: "claude-opus-5", ContextTokens: 200_000, OutputTokens: 10_000},
	}

	got := Spend(rows)
	if len(got) != 2 {
		t.Fatalf("Spend() returned %d summaries, want 2", len(got))
	}

	byType := map[string]SpendSummary{}
	for _, s := range got {
		byType[s.TurnType] = s
	}

	sfe := byType[TurnSingleFileEdit]
	if sfe.Calls != 2 {
		t.Errorf("single_file_edit Calls = %d, want 2", sfe.Calls)
	}
	if sfe.ContextTokens != 1_500_000 {
		t.Errorf("single_file_edit ContextTokens = %d, want 1500000", sfe.ContextTokens)
	}
	if sfe.OutputTokens != 150_000 {
		t.Errorf("single_file_edit OutputTokens = %d, want 150000", sfe.OutputTokens)
	}
	wantCost := (3.0 + 1.5) + (0.5 + 0.25)
	if math.Abs(sfe.CostUSD-wantCost) > 1e-9 {
		t.Errorf("single_file_edit CostUSD = %v, want %v", sfe.CostUSD, wantCost)
	}

	qa := byType[TurnQuestionAnswer]
	if qa.Calls != 1 {
		t.Errorf("question_answer Calls = %d, want 1", qa.Calls)
	}
	wantQACost := 0.2*5 + 0.01*25
	if math.Abs(qa.CostUSD-wantQACost) > 1e-9 {
		t.Errorf("question_answer CostUSD = %v, want %v", qa.CostUSD, wantQACost)
	}
}

func TestSpend_SortedByDescendingCost(t *testing.T) {
	rows := []store.SpendRow{
		{TurnType: "cheap", Model: "claude-haiku-4-5", ContextTokens: 1000, OutputTokens: 100},
		{TurnType: "expensive", Model: "claude-opus-5", ContextTokens: 1_000_000, OutputTokens: 500_000},
	}
	got := Spend(rows)
	if len(got) != 2 {
		t.Fatalf("Spend() returned %d summaries, want 2", len(got))
	}
	if got[0].TurnType != "expensive" || got[1].TurnType != "cheap" {
		t.Errorf("Spend() order = [%s, %s], want [expensive, cheap]", got[0].TurnType, got[1].TurnType)
	}
}

func TestSpend_EmptyInput(t *testing.T) {
	got := Spend(nil)
	if len(got) != 0 {
		t.Errorf("Spend(nil) returned %d summaries, want 0", len(got))
	}
}
