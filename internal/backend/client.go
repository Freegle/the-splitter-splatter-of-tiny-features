package backend

import (
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

// requestTimeout bounds every backend HTTP call, replay or eval alike:
// local models under load and batch-style backends can be slow, but a
// single request must never hang the caller indefinitely.
// 20 minutes, not 5: a deep-reasoning frontier model answering with a
// large agentic context has exceeded 5 minutes in a single call (task 12,
// Opus re-sitting, died on "context deadline exceeded" after looping
// fine for 32 turns). The bound exists to stop a hung connection, and 20
// minutes still does that.
const requestTimeout = 20 * time.Minute

// Client is an OpenAI-compatible chat completions client, sufficient for
// ollama, together, gemini and openai (all expose POST
// {BaseURL}/chat/completions). APIKeyEnv names the environment variable
// holding the bearer token; when it is empty, or the env var is unset or
// empty, no Authorization header is sent (this is how the ollama backend,
// which needs no key, is configured).
type Client struct {
	BaseURL   string
	APIKeyEnv string
	Model     string
}

// Complete sends req as a non-streaming POST to {BaseURL}/chat/completions,
// forcing req.Model to c.Model, and returns the decoded response. The call
// is bounded by requestTimeout regardless of ctx's own deadline. A non-2xx
// response is returned as an error that includes the upstream status and,
// when the body is JSON-shaped as an OpenAI style error object, its decoded
// message.
func (c *Client) Complete(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	sendReq := *req
	sendReq.Model = c.Model

	body, err := json.Marshal(sendReq)
	if err != nil {
		return nil, fmt.Errorf("encoding chat request for %s: %w", c.BaseURL, err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building chat request to %s: %w", url, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := lookupAPIKey(c.APIKeyEnv); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body from %s: %w", url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend %s returned status %d: %s", url, resp.StatusCode, extractErrorMessage(respBody))
	}

	var chatResp ChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("decoding response body from %s: %w", url, err)
	}
	return &chatResp, nil
}

// lookupAPIKey returns the value of the named environment variable, or ""
// when envName is empty or the variable is unset or empty. Callers use an
// empty return to mean "send no auth header".
func lookupAPIKey(envName string) string {
	if envName == "" {
		return ""
	}
	return os.Getenv(envName)
}

// extractErrorMessage returns a human readable message from an error
// response body. It tries the OpenAI/Anthropic shaped {"error":{"message":
// "..."}} object, then a bare {"error": "..."} string, and falls back to
// the raw body when neither shape matches.
func extractErrorMessage(body []byte) string {
	var withObject struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &withObject); err == nil && withObject.Error.Message != "" {
		return withObject.Error.Message
	}

	var withString struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &withString); err == nil && withString.Error != "" {
		return withString.Error
	}

	return string(body)
}
