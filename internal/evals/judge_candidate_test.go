package evals

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/judge"
)

func judgeCandidateTestServer(t *testing.T, verdictJSON string, status int) (*config.Config, func() string) {
	t.Helper()
	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == 200 {
			var req struct {
				Messages []struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"messages"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			capturedPrompt = req.Messages[0].Content[0].Text
			resp := map[string]any{"content": []map[string]string{{"type": "text", "text": verdictJSON}}}
			json.NewEncoder(w).Encode(resp)
		} else {
			w.Write([]byte("error"))
		}
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Upstream = srv.URL
	return cfg, func() string { return capturedPrompt }
}

func TestJudgeCandidateChange_ParsesVerdict(t *testing.T) {
	cfg, _ := judgeCandidateTestServer(t, `{"equivalent":true,"confidence":0.9,"reason":"same behavioural change"}`, 200)
	v, err := JudgeCandidateChange(context.Background(), cfg, "m", "brief", []byte(`{"content":[{"type":"text","text":"ref"}]}`), "--- diff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Equivalent || v.Confidence != 0.9 || v.Reason != "same behavioural change" {
		t.Fatalf("got %+v, want equivalent true confidence 0.9 reason same behavioural change", v)
	}
}

func TestJudgeCandidateChange_VerdictSurroundedByProse(t *testing.T) {
	prose := "Here is my assessment.\n" + `{"equivalent":false,"confidence":0.5,"reason":"no tests"}` + "\nHope that helps."
	cfg, _ := judgeCandidateTestServer(t, prose, 200)
	v, err := JudgeCandidateChange(context.Background(), cfg, "m", "brief", []byte(`{"content":[{"type":"text","text":"ref"}]}`), "--- diff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Equivalent || v.Confidence != 0.5 || v.Reason != "no tests" {
		t.Fatalf("got %+v, want equivalent false confidence 0.5 reason no tests", v)
	}
}

func TestJudgeCandidateChange_PromptCarriesBriefReferenceAndCandidate(t *testing.T) {
	brief := "fix the thing"
	refText := "VISIBLE_REF_12345"
	candText := "CANDIDATE_DIFF_67890"
	cfg, getPrompt := judgeCandidateTestServer(t, `{"equivalent":true,"confidence":1.0,"reason":"ok"}`, 200)
	_, err := JudgeCandidateChange(context.Background(), cfg, "m", brief, []byte(`{"content":[{"type":"text","text":"`+refText+`"}]}`), candText)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := getPrompt()
	if !strings.Contains(p, brief) || !strings.Contains(p, refText) || !strings.Contains(p, candText) || !strings.Contains(p, "Answer ONLY JSON") {
		t.Fatalf("prompt missing expected parts: %s", p)
	}
	refIdx := strings.Index(p, refText)
	candIdx := strings.Index(p, candText)
	if refIdx < 0 || candIdx < 0 || refIdx >= candIdx {
		t.Fatalf("candidate should appear after reference in prompt")
	}
}

func TestJudgeCandidateChange_StripsThinkingFromReference(t *testing.T) {
	refJSON := `{"content":[{"type":"thinking","thinking":"PRIVATE_REASONING_MARKER"},{"type":"text","text":"VISIBLE_REFERENCE_MARKER"}]}`
	cfg, getPrompt := judgeCandidateTestServer(t, `{"equivalent":true,"confidence":1.0,"reason":"ok"}`, 200)
	_, err := JudgeCandidateChange(context.Background(), cfg, "m", "brief", []byte(refJSON), "--- diff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := getPrompt()
	if !strings.Contains(p, "VISIBLE_REFERENCE_MARKER") || strings.Contains(p, "PRIVATE_REASONING_MARKER") {
		t.Fatalf("thinking not stripped correctly: %s", p)
	}
}

func TestJudgeCandidateChange_TruncatesLongCandidate(t *testing.T) {
	cand := strings.Repeat("a", 12500) + "TAIL_MARKER"
	cfg, getPrompt := judgeCandidateTestServer(t, `{"equivalent":true,"confidence":1.0,"reason":"ok"}`, 200)
	_, err := JudgeCandidateChange(context.Background(), cfg, "m", "brief", []byte(`{"content":[{"type":"text","text":"ref"}]}`), cand)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p := getPrompt()
	if !strings.Contains(p, "[truncated]") || strings.Contains(p, "TAIL_MARKER") {
		t.Fatalf("candidate not truncated correctly: %s", p)
	}
}

func TestJudgeCandidateChange_RequestShape(t *testing.T) {
	var capturedModel string
	var capturedMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		capturedModel = req.Model
		capturedMaxTokens = req.MaxTokens
		resp := map[string]any{"content": []map[string]string{{"type": "text", "text": `{"equivalent":true,"confidence":1.0,"reason":"ok"}`}}}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	cfg := config.Default()
	cfg.Upstream = srv.URL

	_, err := JudgeCandidateChange(context.Background(), cfg, "my-judge-model", "brief", []byte(`{"content":[{"type":"text","text":"ref"}]}`), "--- diff")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedModel != "my-judge-model" || capturedMaxTokens != 8192 {
		t.Fatalf("request shape wrong: model=%s max_tokens=%d", capturedModel, capturedMaxTokens)
	}
}

func TestJudgeCandidateChange_BackendError(t *testing.T) {
	cfg, _ := judgeCandidateTestServer(t, `{"equivalent":true,"confidence":1.0,"reason":"ok"}`, 500)
	v, err := JudgeCandidateChange(context.Background(), cfg, "m", "brief", []byte(`{"content":[{"type":"text","text":"ref"}]}`), "--- diff")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "judge call") {
		t.Fatalf("error should mention 'judge call': %v", err)
	}
	var zero judge.Verdict
	if v != zero {
		t.Fatalf("got non-zero verdict on error: %+v", v)
	}
}

func TestJudgeCandidateChange_UnparseableVerdict(t *testing.T) {
	cfg, _ := judgeCandidateTestServer(t, "not json at all", 200)
	v, err := JudgeCandidateChange(context.Background(), cfg, "m", "brief", []byte(`{"content":[{"type":"text","text":"ref"}]}`), "--- diff")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parsing judge verdict") {
		t.Fatalf("error should mention 'parsing judge verdict': %v", err)
	}
	var zero judge.Verdict
	if v != zero {
		t.Fatalf("got non-zero verdict on parse error: %+v", v)
	}
}
