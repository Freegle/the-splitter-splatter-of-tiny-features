package main

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/router"
	"github.com/freegle/splitter/internal/store"
)

func TestReportWeeklyVerb_Registered(t *testing.T) {
	if _, ok := reportCommands["weekly"]; !ok {
		t.Fatal(`"weekly" report sub-command not registered`)
	}
}

func nullableStatsJSON(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func TestSummarizeWeekly_TokensCostAndIncidents(t *testing.T) {
	decisions := []store.RouterDecisionRow{
		{Decision: router.DecisionLocal, Stats: nullableStatsJSON(`{"frontier_model":"claude-sonnet-4-6","local_output_tokens":1000}`)},
		{Decision: router.DecisionLocal, Stats: nullableStatsJSON(`{"frontier_model":"claude-sonnet-4-6","local_output_tokens":500}`)},
		{Decision: router.DecisionEscalated, Stats: nullableStatsJSON(`{"files_touched":["a.go"]}`)},
	}

	s := summarizeWeekly(decisions, nil)

	if s.FrontierTokensAvoided != 1500 {
		t.Errorf("FrontierTokensAvoided = %d, want 1500", s.FrontierTokensAvoided)
	}
	wantCost := (1500.0 / 1_000_000) * 15 // claude-sonnet OutputPerMTok = 15
	if diff := s.EstimatedCostSavedUSD - wantCost; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("EstimatedCostSavedUSD = %v, want %v", s.EstimatedCostSavedUSD, wantCost)
	}
	if s.QualityIncidents != 1 {
		t.Errorf("QualityIncidents = %d, want 1", s.QualityIncidents)
	}
}

func TestSummarizeWeekly_ShadowDriftRate(t *testing.T) {
	decisions := []store.RouterDecisionRow{
		{Decision: router.DecisionShadow, Stats: nullableStatsJSON(`{"shadow_agree":true}`)},
		{Decision: router.DecisionShadow, Stats: nullableStatsJSON(`{"shadow_agree":false}`)},
		{Decision: router.DecisionShadow, Stats: nullableStatsJSON(`{"n":10}`)}, // outcome not yet recorded
	}

	s := summarizeWeekly(decisions, nil)

	if s.ShadowTotal != 3 {
		t.Errorf("ShadowTotal = %d, want 3", s.ShadowTotal)
	}
	if s.ShadowDisagreed != 1 {
		t.Errorf("ShadowDisagreed = %d, want 1", s.ShadowDisagreed)
	}
	if s.ShadowPending != 1 {
		t.Errorf("ShadowPending = %d, want 1", s.ShadowPending)
	}
}

func TestSummarizeWeekly_IgnoresOtherDecisionTypes(t *testing.T) {
	decisions := []store.RouterDecisionRow{
		{Decision: router.DecisionFrontier},
		{Decision: router.DecisionKillswitch},
	}
	s := summarizeWeekly(decisions, nil)
	if s.FrontierTokensAvoided != 0 || s.QualityIncidents != 0 || s.ShadowTotal != 0 {
		t.Errorf("s = %+v, want all zero for frontier/killswitch decisions", s)
	}
}

func TestWriteWeeklyReport_ContainsAllSections(t *testing.T) {
	var buf bytes.Buffer
	writeWeeklyReport(&buf, weeklySummary{
		FrontierTokensAvoided: 1500,
		EstimatedCostSavedUSD: 0.0225,
		QualityIncidents:      1,
		ShadowTotal:           3,
		ShadowDisagreed:       1,
		ShadowPending:         1,
		Divergences: []store.RouterStateRow{
			{Category: "single_file_edit|iznik-server-go", Families: "claude-sonnet>qwen-coder:7b", DisabledReason: "divergent_version:qwen3-coder:7b(n=12,rate=41.7%,family=91.2%)"},
		},
	})
	out := buf.String()

	for _, want := range []string{"frontier tokens avoided: 1500", "quality incidents:       1", "divergence flags:", "qwen3-coder:7b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %s", want, out)
		}
	}
}

func TestWriteWeeklyReport_NoShadowOrDivergence(t *testing.T) {
	var buf bytes.Buffer
	writeWeeklyReport(&buf, weeklySummary{})
	out := buf.String()

	if !strings.Contains(out, "no dual-dispatched turns") {
		t.Errorf("output missing no-shadow-turns line: %s", out)
	}
	if !strings.Contains(out, "divergence flags: none") {
		t.Errorf("output missing no-divergence line: %s", out)
	}
}

func TestDecodeStats_InvalidOrNull(t *testing.T) {
	if got := decodeStats(sql.NullString{}); len(got) != 0 {
		t.Errorf("decodeStats(NULL) = %+v, want empty", got)
	}
	if got := decodeStats(nullableStatsJSON("not json")); len(got) != 0 {
		t.Errorf("decodeStats(invalid) = %+v, want empty", got)
	}
	got := decodeStats(nullableStatsJSON(`{"a":1}`))
	if got["a"] != 1.0 {
		t.Errorf("decodeStats valid JSON = %+v, want a=1", got)
	}
}
