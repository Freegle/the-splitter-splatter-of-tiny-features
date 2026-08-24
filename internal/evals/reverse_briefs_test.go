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

	"github.com/freegle/splitter/internal/anthropic"
	"github.com/freegle/splitter/internal/judge"
	"github.com/freegle/splitter/internal/store"
)

func openReverseBriefsTestDB(t *testing.T) *sql.DB {
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

// fixIdentifier is a distinctive token standing in for a real fix's
// implementation detail (a function name), used both in the seeded task's
// reference diff and as the thing a compliant rewritten brief must never
// contain.
const fixIdentifier = "guardAgainstNilPointer"

// insertCommitSubjectTask inserts one origin='history' task whose brief is
// still the mechanical commit-subject one and whose reference response's
// diff mentions fixIdentifier, mirroring what seed-history produces before
// reversal.
func insertCommitSubjectTask(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	reqJSON, err := json.Marshal(userTextRequest("fix: guard against nil pointer in Greet\n\nGreet crashed when name was empty."))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	reqCompressed, err := store.Compress(reqJSON)
	if err != nil {
		t.Fatalf("compress request: %v", err)
	}

	refBlocks := []anthropic.ContentBlock{
		editToolUseBlock("toolu_0", "internal/greet.go", []diffHunk{
			{Old: "func Greet(name string) string {\n\treturn \"Hello, \" + name", New: fixIdentifier + "(name)\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name"},
		}),
	}
	refJSON, err := buildSeedReferenceMessage(refBlocks)
	if err != nil {
		t.Fatalf("buildSeedReferenceMessage: %v", err)
	}
	refCompressed, err := store.Compress(refJSON)
	if err != nil {
		t.Fatalf("compress reference: %v", err)
	}

	characteristics := Characteristics{BriefSource: BriefSourceCommitSubject, CommitSHA: "deadbeef"}
	id, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS:             "2026-08-24T00:00:00Z",
		RepoHead:              sql.NullString{String: "parentsha", Valid: true},
		Brief:                 "fix: guard against nil pointer in Greet\n\nGreet crashed when name was empty.",
		TurnType:              sql.NullString{String: "single_file_edit", Valid: true},
		RequestZstd:           reqCompressed,
		ReferenceResponseZstd: refCompressed,
		Origin:                OriginHistory,
		Characteristics:       sql.NullString{String: characteristics.JSON(), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}
	return id
}

// fakeBatchServer builds an httptest.Server implementing enough of the
// Anthropic Message Batches API for reverse-briefs' submit/poll: it
// captures every submitted prompt keyed by custom_id, and always reports
// the batch as ended with rewrittenText as every item's successful reply.
type fakeBatchServer struct {
	*httptest.Server
	submittedPrompts map[string]string
	rewrittenText    string
}

func newFakeBatchServer(t *testing.T, rewrittenText string) *fakeBatchServer {
	t.Helper()
	f := &fakeBatchServer{submittedPrompts: map[string]string{}, rewrittenText: rewrittenText}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Requests []struct {
				CustomID string `json:"custom_id"`
				Params   struct {
					Messages []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
				} `json:"params"`
			} `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding batch submit body: %v", err)
		}
		for _, req := range body.Requests {
			if len(req.Params.Messages) > 0 {
				f.submittedPrompts[req.CustomID] = req.Params.Messages[0].Content
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"batch_rb_1","processing_status":"in_progress"}`))
	})
	mux.HandleFunc("/v1/messages/batches/batch_rb_1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"batch_rb_1","processing_status":"ended","results_url":"` + f.resultsURL() + `"}`))
	})
	mux.HandleFunc("/results", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-jsonl")
		var sb strings.Builder
		for customID := range f.submittedPrompts {
			line, err := json.Marshal(map[string]any{
				"custom_id": customID,
				"result": map[string]any{
					"type": "succeeded",
					"message": map[string]any{
						"content": []map[string]string{{"type": "text", "text": f.rewrittenText}},
						"usage":   map[string]int{"input_tokens": 10, "output_tokens": 5},
					},
				},
			})
			if err != nil {
				t.Fatalf("marshaling fake result line: %v", err)
			}
			sb.Write(line)
			sb.WriteString("\n")
		}
		_, _ = w.Write([]byte(sb.String()))
	})

	f.Server = httptest.NewServer(mux)
	return f
}

func (f *fakeBatchServer) resultsURL() string {
	return f.Server.URL + "/results"
}

func TestReverseBriefs_SubmitAndPoll(t *testing.T) {
	db := openReverseBriefsTestDB(t)
	taskID := insertCommitSubjectTask(t, db)

	rewritten := "Users see the app crash when they leave the name field empty."
	server := newFakeBatchServer(t, rewritten)
	defer server.Close()

	jcfg := judge.Config{Upstream: server.URL, Model: "claude-haiku-4-5"}

	submitResult, err := ReverseBriefsSubmit(context.Background(), db, jcfg)
	if err != nil {
		t.Fatalf("ReverseBriefsSubmit: %v", err)
	}
	if submitResult.ItemCount != 1 {
		t.Fatalf("ItemCount = %d, want 1", submitResult.ItemCount)
	}

	prompt, ok := server.submittedPrompts["rb-1"]
	if !ok {
		// Fall back to whatever custom_id was actually used, in case the
		// task id is not 1 in some future test ordering change.
		for _, p := range server.submittedPrompts {
			prompt = p
			ok = true
		}
	}
	if !ok {
		t.Fatal("expected exactly one submitted prompt")
	}
	if !strings.Contains(prompt, "Do NOT name any function") {
		t.Error("prompt's requirements must explicitly forbid naming the fix's functions/variables/approach")
	}
	if !strings.Contains(prompt, "Commit message:") {
		t.Error("prompt should include the commit message as context")
	}

	afterSubmit, err := store.GetEvalTask(db, taskID)
	if err != nil {
		t.Fatalf("GetEvalTask after submit: %v", err)
	}
	if !strings.Contains(afterSubmit.Brief, "guard against nil pointer") {
		t.Error("brief should be unchanged (still the mechanical commit-subject text) until poll applies the rewrite")
	}
	c := ParseCharacteristics(afterSubmit.Characteristics.String)
	if c.ReverseBrief == nil || c.ReverseBrief.Status != ReverseBriefSubmitted {
		t.Errorf("reverse_brief state = %+v, want status=submitted", c.ReverseBrief)
	}

	// A second submit call must not resubmit an already-submitted task.
	secondSubmit, err := ReverseBriefsSubmit(context.Background(), db, jcfg)
	if err != nil {
		t.Fatalf("second ReverseBriefsSubmit: %v", err)
	}
	if secondSubmit.Eligible != 0 {
		t.Errorf("second submit Eligible = %d, want 0 (already submitted)", secondSubmit.Eligible)
	}

	pollResult, err := ReverseBriefsPoll(context.Background(), db, jcfg)
	if err != nil {
		t.Fatalf("ReverseBriefsPoll: %v", err)
	}
	if pollResult.BatchesEnded != 1 || pollResult.Rewritten != 1 {
		t.Errorf("pollResult = %+v, want BatchesEnded=1 Rewritten=1", pollResult)
	}

	after, err := store.GetEvalTask(db, taskID)
	if err != nil {
		t.Fatalf("GetEvalTask after poll: %v", err)
	}
	if after.Brief != rewritten {
		t.Errorf("brief = %q, want %q", after.Brief, rewritten)
	}
	if strings.Contains(after.Brief, fixIdentifier) {
		t.Errorf("rewritten brief must not contain the fix identifier %q", fixIdentifier)
	}
	afterC := ParseCharacteristics(after.Characteristics.String)
	if afterC.BriefSource != BriefSourceReverseEngineered {
		t.Errorf("brief_source = %q, want %q", afterC.BriefSource, BriefSourceReverseEngineered)
	}
	if afterC.ReverseBrief == nil || afterC.ReverseBrief.Status != ReverseBriefDone {
		t.Errorf("reverse_brief state = %+v, want status=done", afterC.ReverseBrief)
	}
}

func TestReverseBriefs_SubmitSkipsNonCommitSubjectTasks(t *testing.T) {
	db := openReverseBriefsTestDB(t)

	c := Characteristics{BriefSource: BriefSourceSession}
	reqCompressed, err := store.Compress([]byte(`{}`))
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if _, _, err := store.InsertEvalTask(db, store.EvalTaskRow{
		CreatedTS: "2026-08-24T00:00:00Z", Brief: "already a real ask",
		RequestZstd: reqCompressed, Origin: OriginHistory,
		Characteristics: sql.NullString{String: c.JSON(), Valid: true},
	}); err != nil {
		t.Fatalf("InsertEvalTask: %v", err)
	}

	server := newFakeBatchServer(t, "should never be used")
	defer server.Close()

	result, err := ReverseBriefsSubmit(context.Background(), db, judge.Config{Upstream: server.URL})
	if err != nil {
		t.Fatalf("ReverseBriefsSubmit: %v", err)
	}
	if result.Eligible != 0 || result.ItemCount != 0 {
		t.Errorf("result = %+v, want no eligible tasks (brief_source is not commit_subject)", result)
	}
}
