package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// MaxRetries bounds how many retryable statuses (429/529) Complete
	// waits out; 0 uses anthropicMaxRetries. Set negative to disable
	// retrying entirely (tests asserting error surfacing).
	MaxRetries int
	// RetryBase scales the backoff ladder; 0 uses 15s.
	RetryBase time.Duration
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

	// A subscription-backed agentic sitting makes hundreds of calls, so a
	// 429 (or a 5xx blip) is expected traffic shaping, not a failure: wait
	// out the window rather than turning it into sixteen dead tasks, as
	// happened twice. Honour Retry-After when the API sends one.
	var respBody []byte
	for attempt := 0; ; attempt++ {
		httpReq.Body = nil
		httpReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		if rc, gerr := httpReq.GetBody(); gerr == nil {
			httpReq.Body = rc
		}

		resp, derr := http.DefaultClient.Do(httpReq)
		if derr != nil {
			return nil, fmt.Errorf("calling %s: %w", url, derr)
		}
		respBody, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading response body from %s: %w", url, err)
		}

		// Only traffic shaping is retried; a 5xx surfaces immediately so a
		// genuine backend fault is not hidden behind minutes of backoff.
		retryable := resp.StatusCode == 429 || resp.StatusCode == 529
		if !retryable || attempt >= c.maxRetries() {
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return nil, fmt.Errorf("anthropic backend %s returned status %d: %s", url, resp.StatusCode, extractErrorMessage(respBody))
			}
			return respBody, nil
		}

		wait := retryAfterDelay(resp.Header.Get("Retry-After"), attempt, c.RetryBase)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting out status %d from %s: %w", resp.StatusCode, url, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// anthropicMaxRetries bounds how many times Complete waits out a
// retryable status before giving up.
const anthropicMaxRetries = 8

// retryAfterDelay parses a Retry-After header (seconds), falling back to
// capped exponential backoff from attempt.
func (c *AnthropicClient) maxRetries() int {
	switch {
	case c.MaxRetries < 0:
		return 0
	case c.MaxRetries == 0:
		return anthropicMaxRetries
	default:
		return c.MaxRetries
	}
}

func retryAfterDelay(header string, attempt int, base time.Duration) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
			d := time.Duration(secs) * time.Second
			if d > 10*time.Minute {
				d = 10 * time.Minute
			}
			return d
		}
	}
	if base <= 0 {
		base = 15 * time.Second
	}
	d := time.Duration(1<<uint(attempt)) * base
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
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
