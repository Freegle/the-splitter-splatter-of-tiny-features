package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_SubmitBatch_BodyShape(t *testing.T) {
	var gotMethod, gotPath, gotAPIKey, gotVersion string
	var gotBody batchRequestBody

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("server: decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msgbatch_abc","processing_status":"in_progress"}`))
	}))
	defer server.Close()

	t.Setenv("TEST_JUDGE_KEY", "ant-secret")
	c := &Client{BaseURL: server.URL, APIKeyEnv: "TEST_JUDGE_KEY", Model: "claude-haiku-4-5"}

	batchID, err := c.SubmitBatch(context.Background(), []PromptItem{
		{CustomID: "ji-1", Prompt: "prompt one"},
		{CustomID: "ji-2", Prompt: "prompt two"},
	})
	if err != nil {
		t.Fatalf("SubmitBatch: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/messages/batches" {
		t.Errorf("path = %q, want /v1/messages/batches", gotPath)
	}
	if gotAPIKey != "ant-secret" {
		t.Errorf("x-api-key header = %q, want ant-secret", gotAPIKey)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version header = %q, want 2023-06-01", gotVersion)
	}
	if batchID != "msgbatch_abc" {
		t.Errorf("SubmitBatch returned %q, want msgbatch_abc", batchID)
	}

	if len(gotBody.Requests) != 2 {
		t.Fatalf("len(requests) = %d, want 2", len(gotBody.Requests))
	}
	for i, want := range []struct {
		customID string
		prompt   string
	}{
		{"ji-1", "prompt one"},
		{"ji-2", "prompt two"},
	} {
		req := gotBody.Requests[i]
		if req.CustomID != want.customID {
			t.Errorf("requests[%d].custom_id = %q, want %q", i, req.CustomID, want.customID)
		}
		if req.Params.Model != "claude-haiku-4-5" {
			t.Errorf("requests[%d].params.model = %q, want claude-haiku-4-5", i, req.Params.Model)
		}
		if req.Params.MaxTokens != 512 {
			t.Errorf("requests[%d].params.max_tokens = %d, want 512", i, req.Params.MaxTokens)
		}
		if len(req.Params.Messages) != 1 || req.Params.Messages[0].Role != "user" || req.Params.Messages[0].Content != want.prompt {
			t.Errorf("requests[%d].params.messages = %+v, want one user message with content %q", i, req.Params.Messages, want.prompt)
		}
	}
}

func TestClient_SubmitBatch_NoItems(t *testing.T) {
	c := &Client{BaseURL: "http://unused.invalid", Model: "claude-haiku-4-5"}
	if _, err := c.SubmitBatch(context.Background(), nil); err == nil {
		t.Fatal("SubmitBatch with no items: want an error, got nil")
	}
}

func TestClient_PollBatch_Transitions(t *testing.T) {
	calls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/messages/batches/msgbatch_xyz" {
			t.Errorf("path = %q, want /v1/messages/batches/msgbatch_xyz", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"id":"msgbatch_xyz","processing_status":"in_progress"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msgbatch_xyz","processing_status":"ended","results_url":"` + server.URL + `/results"}`))
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Model: "claude-haiku-4-5"}

	first, err := c.PollBatch(context.Background(), "msgbatch_xyz")
	if err != nil {
		t.Fatalf("PollBatch (1st): %v", err)
	}
	if first.Ended() {
		t.Errorf("first poll: Ended() = true, want false (still in_progress)")
	}

	second, err := c.PollBatch(context.Background(), "msgbatch_xyz")
	if err != nil {
		t.Fatalf("PollBatch (2nd): %v", err)
	}
	if !second.Ended() {
		t.Errorf("second poll: Ended() = false, want true (processing_status ended)")
	}
	if second.ResultsURL != server.URL+"/results" {
		t.Errorf("second poll: ResultsURL = %q, want %s/results", second.ResultsURL, server.URL)
	}
	if calls != 2 {
		t.Errorf("server received %d calls, want 2 (one GET per PollBatch invocation, no internal retry loop)", calls)
	}
}

func TestClient_FetchResults_ShuffledErroredAndLenientVerdict(t *testing.T) {
	// Lines arrive out of submission order (ji-3, then ji-1, then ji-2):
	// FetchResults/callers must key by custom_id, never by position.
	const jsonl = `{"custom_id":"ji-3","result":{"type":"errored","error":{"type":"invalid_request","message":"content policy violation"}}}
{"custom_id":"ji-1","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"Looking at both responses, they behave the same.\n\n{\"equivalent\": true, \"confidence\": 0.85, \"reason\": \"identical output\"}\n\nHope that helps!"}],"usage":{"input_tokens":120,"output_tokens":40}}}}
{"custom_id":"ji-2","result":{"type":"succeeded","message":{"content":[{"type":"text","text":"{\"equivalent\": false, \"confidence\": 0.6, \"reason\": \"local model skipped an edge case\"}"}],"usage":{"input_tokens":90,"output_tokens":25}}}}
`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("results request anthropic-version = %q, want 2023-06-01", got)
		}
		w.Header().Set("Content-Type", "application/x-jsonl")
		_, _ = w.Write([]byte(jsonl))
	}))
	defer server.Close()

	c := &Client{BaseURL: server.URL, Model: "claude-haiku-4-5"}
	lines, err := c.FetchResults(context.Background(), server.URL+"/results")
	if err != nil {
		t.Fatalf("FetchResults: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3", len(lines))
	}

	byID := make(map[string]ResultLine, len(lines))
	for _, l := range lines {
		byID[l.CustomID] = l
	}

	errored, ok := byID["ji-3"]
	if !ok {
		t.Fatal("missing result for ji-3")
	}
	if errored.Succeeded {
		t.Errorf("ji-3: Succeeded = true, want false (batch-level error result)")
	}
	if !strings.Contains(errored.ErrorMessage, "content policy violation") {
		t.Errorf("ji-3: ErrorMessage = %q, want it to surface the upstream error message", errored.ErrorMessage)
	}

	lenient, ok := byID["ji-1"]
	if !ok {
		t.Fatal("missing result for ji-1")
	}
	if !lenient.Succeeded {
		t.Fatalf("ji-1: Succeeded = false, want true")
	}
	if lenient.InputTokens != 120 || lenient.OutputTokens != 40 {
		t.Errorf("ji-1: tokens = %d/%d, want 120/40", lenient.InputTokens, lenient.OutputTokens)
	}
	verdict, err := ParseVerdict(lenient.Text)
	if err != nil {
		t.Fatalf("ParseVerdict(ji-1 text) failed despite lenient parsing: %v\ntext: %s", err, lenient.Text)
	}
	if !verdict.Equivalent || verdict.Confidence != 0.85 || verdict.Reason != "identical output" {
		t.Errorf("ji-1 verdict = %+v, want {true 0.85 identical output}", verdict)
	}

	clean, ok := byID["ji-2"]
	if !ok {
		t.Fatal("missing result for ji-2")
	}
	if !clean.Succeeded {
		t.Fatalf("ji-2: Succeeded = false, want true")
	}
	verdict2, err := ParseVerdict(clean.Text)
	if err != nil {
		t.Fatalf("ParseVerdict(ji-2 text): %v", err)
	}
	if verdict2.Equivalent || verdict2.Confidence != 0.6 {
		t.Errorf("ji-2 verdict = %+v, want equivalent=false confidence=0.6", verdict2)
	}
}
