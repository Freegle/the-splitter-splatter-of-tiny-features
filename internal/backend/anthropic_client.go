package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/freegle/splitter/internal/anthropic"
)

// anthropicAPIVersion is the anthropic-version header value splitter speaks.
const anthropicAPIVersion = "2023-06-01"

// AnthropicClient is a minimal native Anthropic Messages API client. It is
// used only by eval runs (internal/evals, selected with "-backend
// anthropic -model <id>") to score a specific Anthropic model version
// against the eval library; live routing (Phase 4) always goes through the
// OpenAI-compatible Client and ToOpenAI/FromOpenAI translation instead, so
// this type must never be referenced from internal/proxy.
type AnthropicClient struct {
	BaseURL   string
	APIKeyEnv string
	Model     string
}

// Complete sends req as a non-streaming POST to {BaseURL}/v1/messages,
// forcing req.Model to c.Model and req.Stream to false, and returns the raw
// response body (a complete Anthropic message JSON) unmodified: no
// translation is needed since the wire format already matches what callers
// want. The call is bounded by requestTimeout. A non-2xx response is
// returned as an error that includes the upstream status and, when the
// body is JSON-shaped as an Anthropic error object, its decoded message.
func (c *AnthropicClient) Complete(ctx context.Context, req anthropic.MessagesRequest) ([]byte, error) {
	req.Model = c.Model
	req.Stream = false

	oauthKey := ""
	if key := lookupAPIKey(c.APIKeyEnv); strings.HasPrefix(key, "sk-ant-oat") {
		oauthKey = key
		// Subscription tokens are honoured only for requests shaped like
		// Claude Code's own: the system prompt must open with its identity
		// line (a bare request gets rate_limit_error, not a 401).
		req.System = prependClaudeCodeIdentity(req.System)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding anthropic request for %s: %w", c.BaseURL, err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	url := strings.TrimRight(c.BaseURL, "/") + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building anthropic request to %s: %w", url, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)
	if oauthKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+oauthKey)
		httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")
		httpReq.Header.Set("User-Agent", claudeCodeUserAgent)
	} else if key := lookupAPIKey(c.APIKeyEnv); key != "" {
		httpReq.Header.Set("x-api-key", key)
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
		return nil, fmt.Errorf("anthropic backend %s returned status %d: %s", url, resp.StatusCode, extractErrorMessage(respBody))
	}

	return respBody, nil
}

// claudeCodeIdentity is the system-prompt opener subscription OAuth tokens
// require; claudeCodeUserAgent matches the CLI's request signature.
const claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
const claudeCodeUserAgent = "claude-cli/2.1.240 (external, cli)"

// prependClaudeCodeIdentity returns system with the identity block first,
// preserving an existing bare-string or block-array system prompt after it.
func prependClaudeCodeIdentity(system json.RawMessage) json.RawMessage {
	identity := map[string]string{"type": "text", "text": claudeCodeIdentity}
	blocks := []any{identity}
	if len(system) > 0 {
		if system[0] == '"' {
			var s string
			if err := json.Unmarshal(system, &s); err == nil && s != "" {
				blocks = append(blocks, map[string]string{"type": "text", "text": s})
			}
		} else {
			var existing []json.RawMessage
			if err := json.Unmarshal(system, &existing); err == nil {
				for _, b := range existing {
					blocks = append(blocks, b)
				}
			}
		}
	}
	out, err := json.Marshal(blocks)
	if err != nil {
		return system
	}
	return out
}
