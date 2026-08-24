package evals

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

func judgeFailsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "splitter.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(db); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

func TestJudgeFailsFlipsEquivalentResults(t *testing.T) {
	db := judgeFailsTestDB(t)

	comp := func(s string) []byte {
		t.Helper()
		b, err := store.Compress([]byte(s))
		if err != nil {
			t.Fatalf("compress: %v", err)
		}
		return b
	}
	if _, err := db.Exec(`INSERT INTO eval_tasks (id, created_ts, brief, origin, request_zstd, reference_response_zstd)
		VALUES (1, 't', 'fix the thing', 'history', X'00', ?), (2, 't', 'other thing', 'history', X'00', ?)`,
		comp(`{"content":[{"type":"text","text":"ref"}]}`), comp(`{"content":[{"type":"text","text":"ref2"}]}`)); err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO eval_runs (id, ts, backend, model) VALUES (9, 't', 'b', 'm')`); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO eval_results (eval_run_id, eval_task_id, passed, stage, response_zstd)
		VALUES (9, 1, 0, 'band', ?), (9, 2, 0, 'ast', ?)`,
		comp(`{"content":[{"type":"text","text":"candidate same"}]}`),
		comp(`{"content":[{"type":"text","text":"candidate different, no tests"}]}`)); err != nil {
		t.Fatalf("insert results: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		text := req.Messages[0].Content[0].Text
		verdict := `{"equivalent": true, "confidence": 0.9, "reason": "same substance"}`
		if strings.Contains(text, "candidate different") {
			verdict = `{"equivalent": false, "confidence": 0.8, "reason": "skipped the tests the shipped change made"}`
		}
		resp := map[string]any{"content": []map[string]string{{"type": "text", "text": verdict}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Upstream = srv.URL
	sum, err := JudgeFails(context.Background(), db, cfg, "judge-model")
	if err != nil {
		t.Fatalf("JudgeFails: %v", err)
	}
	if sum.Judged != 2 || sum.FlippedToPass != 1 || sum.Errored != 0 {
		t.Fatalf("summary %+v, want judged 2 flippedToPass 1 errored 0", sum)
	}

	var passed1, passed2 int
	var stage1, verdict2 string
	if err := db.QueryRow(`SELECT passed, stage FROM eval_results WHERE eval_task_id=1`).Scan(&passed1, &stage1); err != nil {
		t.Fatalf("read result 1: %v", err)
	}
	if err := db.QueryRow(`SELECT passed, judge_verdict FROM eval_results WHERE eval_task_id=2`).Scan(&passed2, &verdict2); err != nil {
		t.Fatalf("read result 2: %v", err)
	}
	if passed1 != 1 || stage1 != "judge" {
		t.Fatalf("equivalent result not flipped: passed=%d stage=%s", passed1, stage1)
	}
	if passed2 != 0 || !strings.Contains(verdict2, "skipped the tests") {
		t.Fatalf("inequivalent result mishandled: passed=%d verdict=%s", passed2, verdict2)
	}

	again, err := JudgeFails(context.Background(), db, cfg, "judge-model")
	if err != nil {
		t.Fatalf("JudgeFails rerun: %v", err)
	}
	if again.Considered != 0 {
		t.Fatalf("already-judged results reconsidered: %+v", again)
	}
}
