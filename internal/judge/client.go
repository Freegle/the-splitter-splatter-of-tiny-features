// Package judge implements the Phase 3 Haiku batch judge: submitting the
// verification cascade's queued middle-band items to the Anthropic Message
// Batches API, and later applying their results back to verifications,
// including the tests-win-over-judge conflict rule from DESIGN.md.
package judge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// anthropicAPIVersion is the anthropic-version header value every Anthropic
// API call in splitter sends, including the Message Batches endpoints this
// client speaks.
const anthropicAPIVersion = "2023-06-01"

// judgeMaxTokens is max_tokens for every judge request: the verdict is a
// short JSON object, so a small cap keeps judge spend predictable.
const judgeMaxTokens = 512

// batchRequestTimeout bounds every HTTP call this client makes: submitting
// a batch, checking its status, and fetching its results.
const batchRequestTimeout = 2 * time.Minute

// Config configures Client, Submit and Poll. It carries plain fields
// (rather than *config.Config) so internal/judge does not depend on
// internal/config; callers build one from the loaded config's relevant
// fields.
type Config struct {
	// Upstream is the Anthropic API base URL, e.g. https://api.anthropic.com
	// (the same upstream the proxy forwards to).
	Upstream string
	// APIKeyEnv names the environment variable holding the Anthropic API
	// key (config judge.api_key_env).
	APIKeyEnv string
	// Model is the judge model, e.g. claude-haiku-4-5.
	Model string
	// MaxContextChars truncates the request context section of the judge
	// prompt (config judge.max_context_chars).
	MaxContextChars int
}

// Client is a minimal Anthropic Message Batches API client: create a
// batch, check its status, and stream its JSONL results. Raw HTTP only, no
// provider SDK.
type Client struct {
	BaseURL   string
	APIKeyEnv string
	Model     string
}

// NewClient builds a Client from cfg.
func NewClient(cfg Config) *Client {
	return &Client{BaseURL: cfg.Upstream, APIKeyEnv: cfg.APIKeyEnv, Model: cfg.Model}
}

// PromptItem is one judge item to submit: CustomID identifies it for the
// poll step to key results by (never by order), Prompt is the complete
// single user-turn message text built by BuildPrompt.
type PromptItem struct {
	CustomID string
	Prompt   string
}

type batchMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type batchItemParams struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	Messages  []batchMessage `json:"messages"`
}

type batchRequestItem struct {
	CustomID string          `json:"custom_id"`
	Params   batchItemParams `json:"params"`
}

type batchRequestBody struct {
	Requests []batchRequestItem `json:"requests"`
}

type batchCreateResponse struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
}

// SubmitBatch POSTs items as one Message Batches API request
// ({BaseURL}/v1/messages/batches) and returns the created batch's id.
func (c *Client) SubmitBatch(ctx context.Context, items []PromptItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("judge: SubmitBatch called with no items")
	}

	body := batchRequestBody{Requests: make([]batchRequestItem, 0, len(items))}
	for _, it := range items {
		body.Requests = append(body.Requests, batchRequestItem{
			CustomID: it.CustomID,
			Params: batchItemParams{
				Model:     c.Model,
				MaxTokens: judgeMaxTokens,
				Messages:  []batchMessage{{Role: "user", Content: it.Prompt}},
			},
		})
	}

	var resp batchCreateResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/messages/batches", body, &resp); err != nil {
		return "", fmt.Errorf("submitting judge batch: %w", err)
	}
	if resp.ID == "" {
		return "", fmt.Errorf("submitting judge batch: upstream returned no batch id")
	}
	return resp.ID, nil
}

// BatchStatus is the subset of a Message Batches API batch object
// PollBatch needs: its processing status, and the results URL once ended.
type BatchStatus struct {
	ProcessingStatus string
	ResultsURL       string
}

// Ended reports whether the batch has finished processing: results, if
// any, are ready to fetch from ResultsURL.
func (s BatchStatus) Ended() bool {
	return s.ProcessingStatus == "ended"
}

type batchStatusResponse struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
	ResultsURL       string `json:"results_url"`
}

// PollBatch does a single GET {BaseURL}/v1/messages/batches/<id> and
// returns its current status. It never loops or waits for "ended": a
// caller that wants to wait is expected to be invoked again later, which
// cron drives.
func (c *Client) PollBatch(ctx context.Context, batchID string) (BatchStatus, error) {
	var resp batchStatusResponse
	path := "/v1/messages/batches/" + batchID
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return BatchStatus{}, fmt.Errorf("polling judge batch %s: %w", batchID, err)
	}
	return BatchStatus{ProcessingStatus: resp.ProcessingStatus, ResultsURL: resp.ResultsURL}, nil
}

// ResultLine is one parsed line of a batch's JSONL results, keyed by
// CustomID. Results arrive in arbitrary order, never assumed to match
// submission order.
type ResultLine struct {
	CustomID     string
	Succeeded    bool
	Text         string // concatenated text content of the judge's reply, when Succeeded
	InputTokens  int
	OutputTokens int
	ErrorMessage string // set when !Succeeded: the batch-level error, canceled or expired reason
}

type rawResultContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rawResultMessage struct {
	Content []rawResultContentBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type rawResultLine struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string            `json:"type"`
		Message *rawResultMessage `json:"message"`
		Error   *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"result"`
}

// FetchResults GETs resultsURL and parses it as JSONL, one ResultLine per
// line. A line that is not valid JSON at all is skipped rather than
// failing the whole fetch, since one corrupt line should not lose every
// other item's result.
func (c *Client) FetchResults(ctx context.Context, resultsURL string) ([]ResultLine, error) {
	ctx, cancel := context.WithTimeout(ctx, batchRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building results request to %s: %w", resultsURL, err)
	}
	c.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching results from %s: %w", resultsURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetching results from %s: status %d: %s", resultsURL, resp.StatusCode, string(respBody))
	}

	var out []ResultLine
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw rawResultLine
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		out = append(out, resultLineFrom(raw))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading results from %s: %w", resultsURL, err)
	}
	return out, nil
}

// resultLineFrom converts one decoded JSONL result object into a
// ResultLine. A "succeeded" result with no message body, or any other
// result type (errored, canceled, expired), is reported as not succeeded
// with a diagnostic ErrorMessage.
func resultLineFrom(raw rawResultLine) ResultLine {
	rl := ResultLine{CustomID: raw.CustomID}
	if raw.Result.Type != "succeeded" {
		rl.ErrorMessage = raw.Result.Type
		if raw.Result.Error != nil && raw.Result.Error.Message != "" {
			rl.ErrorMessage = raw.Result.Error.Message
		}
		return rl
	}
	if raw.Result.Message == nil {
		rl.ErrorMessage = "succeeded result carried no message"
		return rl
	}

	rl.Succeeded = true
	rl.InputTokens = raw.Result.Message.Usage.InputTokens
	rl.OutputTokens = raw.Result.Message.Usage.OutputTokens
	var parts []string
	for _, b := range raw.Result.Message.Content {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	rl.Text = strings.Join(parts, "\n")
	return rl
}

// doJSON sends a JSON request (or none, when body is nil) to path under
// c.BaseURL and decodes a JSON response into out (or discards it, when out
// is nil).
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	ctx, cancel := context.WithTimeout(ctx, batchRequestTimeout)
	defer cancel()

	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("building request to %s: %w", url, err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setHeaders(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body from %s: %w", url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned status %d: %s", url, resp.StatusCode, extractErrorMessage(respBody))
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decoding response body from %s: %w", url, err)
		}
	}
	return nil
}

// setHeaders sets the headers every Message Batches API call needs:
// anthropic-version always, and x-api-key when APIKeyEnv names a set,
// non-empty environment variable.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	if key := lookupAPIKey(c.APIKeyEnv); key != "" {
		req.Header.Set("x-api-key", key)
	}
}

// lookupAPIKey returns the value of the named environment variable, or ""
// when envName is empty or the variable is unset or empty.
func lookupAPIKey(envName string) string {
	if envName == "" {
		return ""
	}
	return os.Getenv(envName)
}

// extractErrorMessage returns a human readable message from an error
// response body: the Anthropic-shaped {"error":{"message":"..."}} object
// when present, else the raw body.
func extractErrorMessage(body []byte) string {
	var withObject struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &withObject); err == nil && withObject.Error.Message != "" {
		return withObject.Error.Message
	}
	return string(body)
}
