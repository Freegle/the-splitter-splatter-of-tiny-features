package store

import (
	"database/sql"
	"testing"
)

func TestUpsertRouterState_InsertsThenUpdatesOnConflict(t *testing.T) {
	db, _ := openTestDB(t)

	if err := UpsertRouterState(db, RouterStateRow{
		Category: "single_file_edit|iznik-server-go",
		Families: "claude-sonnet>qwen-coder:7b",
		N:        10, Agreed: 9, WilsonLB: 0.6, Routable: false,
		UpdatedTS: "2026-08-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRouterState (insert): %v", err)
	}

	if err := UpsertRouterState(db, RouterStateRow{
		Category: "single_file_edit|iznik-server-go",
		Families: "claude-sonnet>qwen-coder:7b",
		N:        50, Agreed: 48, WilsonLB: 0.91, Routable: true,
		UpdatedTS: "2026-08-24T01:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRouterState (update): %v", err)
	}

	rows, err := AllRouterState(db)
	if err != nil {
		t.Fatalf("AllRouterState: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1 (upsert must update, not duplicate)", len(rows))
	}
	if rows[0].N != 50 || rows[0].Agreed != 48 || !rows[0].Routable {
		t.Errorf("row = %+v, want the updated values", rows[0])
	}
}

func TestUpsertRouterState_DifferentFamiliesSameCategoryAreSeparateRows(t *testing.T) {
	db, _ := openTestDB(t)

	base := RouterStateRow{Category: "single_file_edit|iznik-server-go", N: 10, Agreed: 9, UpdatedTS: "2026-08-24T00:00:00Z"}
	a := base
	a.Families = "claude-sonnet>qwen-coder:7b"
	b := base
	b.Families = "claude-opus>qwen-coder:7b"

	if err := UpsertRouterState(db, a); err != nil {
		t.Fatalf("UpsertRouterState a: %v", err)
	}
	if err := UpsertRouterState(db, b); err != nil {
		t.Fatalf("UpsertRouterState b: %v", err)
	}

	rows, err := AllRouterState(db)
	if err != nil {
		t.Fatalf("AllRouterState: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (UNIQUE(category, families), not just category)", len(rows))
	}
}

func TestDisableRouterState_SetsReasonAndForcesUnroutable(t *testing.T) {
	db, _ := openTestDB(t)

	if err := UpsertRouterState(db, RouterStateRow{
		Category: "tool_result_summary|iznik-server-go", Families: "claude-sonnet>qwen-coder:7b",
		N: 50, Agreed: 48, WilsonLB: 0.91, Routable: true, UpdatedTS: "2026-08-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRouterState: %v", err)
	}

	if err := DisableRouterState(db, "tool_result_summary|iznik-server-go", "claude-sonnet>qwen-coder:7b", "escalation", "2026-08-24T01:00:00Z"); err != nil {
		t.Fatalf("DisableRouterState: %v", err)
	}

	rows, err := AllRouterState(db)
	if err != nil {
		t.Fatalf("AllRouterState: %v", err)
	}
	if len(rows) != 1 || rows[0].Routable || rows[0].DisabledReason != "escalation" {
		t.Errorf("rows = %+v, want disabled with reason escalation", rows)
	}
}

func TestDisableRouterState_NoMatchingRow_IsNoOp(t *testing.T) {
	db, _ := openTestDB(t)
	if err := DisableRouterState(db, "no|such", "no>such", "escalation", "2026-08-24T00:00:00Z"); err != nil {
		t.Fatalf("DisableRouterState on missing row returned an error, want a silent no-op: %v", err)
	}
}

func TestRouterStateDivergences_FiltersByReasonPrefix(t *testing.T) {
	db, _ := openTestDB(t)

	if err := UpsertRouterState(db, RouterStateRow{
		Category: "single_file_edit|a", Families: "claude-sonnet>qwen-coder:7b",
		N: 12, Agreed: 5, DisabledReason: "divergent_version:qwen3-coder:7b(n=12,rate=41.7%,family=91.2%)",
		UpdatedTS: "2026-08-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRouterState divergent: %v", err)
	}
	if err := UpsertRouterState(db, RouterStateRow{
		Category: "single_file_edit|b", Families: "claude-sonnet>qwen-coder:7b",
		N: 40, Agreed: 38, DisabledReason: "escalation", UpdatedTS: "2026-08-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRouterState escalation: %v", err)
	}
	if err := UpsertRouterState(db, RouterStateRow{
		Category: "single_file_edit|c", Families: "claude-sonnet>qwen-coder:7b",
		N: 40, Agreed: 39, Routable: true, UpdatedTS: "2026-08-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("UpsertRouterState healthy: %v", err)
	}

	rows, err := RouterStateDivergences(db)
	if err != nil {
		t.Fatalf("RouterStateDivergences: %v", err)
	}
	if len(rows) != 1 || rows[0].Category != "single_file_edit|a" {
		t.Errorf("rows = %+v, want exactly the divergent_version row", rows)
	}
}

func TestInsertAndQueryRouterDecisions(t *testing.T) {
	db, _ := openTestDB(t)

	callID := int64(42)
	id, err := InsertRouterDecision(db, RouterDecisionRow{
		TS:       "2026-08-24T00:00:00Z",
		Decision: "local",
	})
	if err != nil {
		t.Fatalf("InsertRouterDecision (minimal): %v", err)
	}
	if id == 0 {
		t.Fatal("InsertRouterDecision returned id 0")
	}

	_, err = InsertRouterDecision(db, RouterDecisionRow{
		TS:        "2026-08-25T00:00:00Z",
		SessionID: nullableString("sess-1"),
		CallID:    nullableInt64(callID),
		Category:  nullableString("single_file_edit|iznik-server-go"),
		Decision:  "shadow",
		Stats:     nullableString(`{"n":10}`),
	})
	if err != nil {
		t.Fatalf("InsertRouterDecision (full): %v", err)
	}

	all, err := RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince (all): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(all) = %d, want 2", len(all))
	}

	sinceLater, err := RouterDecisionsSince(db, "2026-08-25T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince (filtered): %v", err)
	}
	if len(sinceLater) != 1 || sinceLater[0].Decision != "shadow" {
		t.Fatalf("sinceLater = %+v, want only the shadow row", sinceLater)
	}
	if sinceLater[0].SessionID.String != "sess-1" || sinceLater[0].CallID.Int64 != callID {
		t.Errorf("row = %+v, unexpected session/call id", sinceLater[0])
	}
}

func TestUpdateRouterDecisionStats(t *testing.T) {
	db, _ := openTestDB(t)

	id, err := InsertRouterDecision(db, RouterDecisionRow{TS: "2026-08-24T00:00:00Z", Decision: "shadow", Stats: nullableString(`{"n":10}`)})
	if err != nil {
		t.Fatalf("InsertRouterDecision: %v", err)
	}

	if err := UpdateRouterDecisionStats(db, id, `{"n":10,"shadow_agree":false}`); err != nil {
		t.Fatalf("UpdateRouterDecisionStats: %v", err)
	}

	rows, err := RouterDecisionsSince(db, "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("RouterDecisionsSince: %v", err)
	}
	if len(rows) != 1 || rows[0].Stats.String != `{"n":10,"shadow_agree":false}` {
		t.Errorf("rows = %+v, want updated stats", rows)
	}
}

func nullableString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullableInt64(i int64) sql.NullInt64 {
	return sql.NullInt64{Int64: i, Valid: true}
}
