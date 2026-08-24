package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
)

// ErrTruncatedStream is wrapped into AssembleSSE's error return when the
// captured stream ends before a message_stop event, or an accumulated
// tool_use input never parsed as valid JSON. The message returned alongside
// it is the best-effort partial assembly.
var ErrTruncatedStream = errors.New("sse stream ended before message_stop or with unparseable content")

// ErrUpstreamSSEError is wrapped into AssembleSSE's error return when the
// stream carried an explicit "error" event.
var ErrUpstreamSSEError = errors.New("sse stream reported an error event")

// assembledMessage is the shape AssembleSSE marshals its result into. It
// mirrors the message skeleton delivered by message_start, with Content
// replaced by the fully accumulated blocks.
type assembledMessage struct {
	ID           string         `json:"id,omitempty"`
	Type         string         `json:"type,omitempty"`
	Role         string         `json:"role,omitempty"`
	Model        string         `json:"model,omitempty"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence *string        `json:"stop_sequence,omitempty"`
	Usage        Usage          `json:"usage"`
}

// indexedEvent covers the three content_block_* event shapes, which all
// carry an "index" alongside a type-specific payload.
type indexedEvent struct {
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
}

// contentDelta is the payload of a content_block_delta event's "delta".
type contentDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
}

// messageStartEvent is the payload of a message_start event.
type messageStartEvent struct {
	Message json.RawMessage `json:"message"`
}

// messageStartSkeleton is the subset of message_start's "message" object
// AssembleSSE carries forward; its (always empty) content array is ignored,
// content is rebuilt from content_block_* events instead.
type messageStartSkeleton struct {
	ID    string          `json:"id"`
	Type  string          `json:"type"`
	Role  string          `json:"role"`
	Model string          `json:"model"`
	Usage json.RawMessage `json:"usage"`
}

// messageDeltaEvent is the payload of a message_delta event.
type messageDeltaEvent struct {
	Delta struct {
		StopReason   *string `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	} `json:"delta"`
	Usage json.RawMessage `json:"usage"`
}

// errorEvent is the payload of an "error" event.
type errorEvent struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// sseAssembler accumulates the state needed to reconstruct one complete
// message from a stream of decoded SSE event/data pairs.
type sseAssembler struct {
	id, msgType, role, model string
	usage                    Usage
	stopReason               string
	stopSequence             *string

	blocks      map[int]*ContentBlock
	order       []int
	partialJSON map[int]*strings.Builder

	truncatedInput     bool
	streamErrorMessage string
	warned             map[string]bool
}

func newSSEAssembler() *sseAssembler {
	return &sseAssembler{
		blocks:      map[int]*ContentBlock{},
		partialJSON: map[int]*strings.Builder{},
		warned:      map[string]bool{},
	}
}

// AssembleSSE reconstructs the complete Anthropic message from a captured
// SSE byte stream. message_start supplies the message skeleton and initial
// usage; content_block_delta events accumulate per content-block index
// (input_json_delta partial_json strings concatenate into a tool_use
// block's input); message_delta supplies output tokens and stop_reason.
// Unknown event and delta types are skipped, logging each distinct one seen
// once. A stream that ends before message_stop, contains an accumulated
// tool_use input that never became valid JSON, or carries an explicit error
// event is reported as truncated: messageJSON is still the best-effort
// partial assembly, returned alongside the error.
func AssembleSSE(raw []byte) (messageJSON []byte, usage Usage, stopReason string, err error) {
	asm := newSSEAssembler()
	sawMessageStop := false

	for _, block := range splitSSEBlocks(raw) {
		event, data := parseSSEBlock(block)
		if data == "" {
			continue
		}
		if event == "" {
			event = jsonTypeField(data)
		}

		switch event {
		case "message_start":
			if hErr := asm.handleMessageStart([]byte(data)); hErr != nil {
				log.Printf("splitter: internal/anthropic: %v", hErr)
			}
		case "content_block_start":
			if hErr := asm.handleContentBlockStart([]byte(data)); hErr != nil {
				log.Printf("splitter: internal/anthropic: %v", hErr)
			}
		case "content_block_delta":
			if hErr := asm.handleContentBlockDelta([]byte(data)); hErr != nil {
				log.Printf("splitter: internal/anthropic: %v", hErr)
			}
		case "content_block_stop":
			if hErr := asm.handleContentBlockStop([]byte(data)); hErr != nil {
				log.Printf("splitter: internal/anthropic: %v", hErr)
			}
		case "message_delta":
			if hErr := asm.handleMessageDelta([]byte(data)); hErr != nil {
				log.Printf("splitter: internal/anthropic: %v", hErr)
			}
		case "message_stop":
			sawMessageStop = true
		case "ping":
			// Expected keepalive, nothing to accumulate.
		case "error":
			asm.handleError([]byte(data))
		default:
			asm.warnUnknown("event type", event)
		}
	}

	// A stream truncated mid tool_use never reaches content_block_stop for
	// the open block; finalise whatever partial_json was accumulated so the
	// block still appears in the output.
	for index := range asm.partialJSON {
		asm.finalizeToolUseInput(index)
	}

	messageJSON, marshalErr := asm.finalize()
	if marshalErr != nil {
		return nil, asm.usage, asm.stopReason, marshalErr
	}

	switch {
	case asm.streamErrorMessage != "":
		err = fmt.Errorf("%s: %w", asm.streamErrorMessage, ErrUpstreamSSEError)
	case !sawMessageStop || asm.truncatedInput:
		err = ErrTruncatedStream
	}

	return messageJSON, asm.usage, asm.stopReason, err
}

func (a *sseAssembler) finalize() ([]byte, error) {
	sort.Ints(a.order)

	content := make([]ContentBlock, 0, len(a.order))
	seen := make(map[int]bool, len(a.order))
	for _, index := range a.order {
		if seen[index] {
			continue
		}
		seen[index] = true
		if block := a.blocks[index]; block != nil {
			content = append(content, *block)
		}
	}

	msg := assembledMessage{
		ID:           a.id,
		Type:         a.msgType,
		Role:         a.role,
		Model:        a.model,
		Content:      content,
		StopReason:   a.stopReason,
		StopSequence: a.stopSequence,
		Usage:        a.usage,
	}

	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshaling assembled message: %w", err)
	}
	return b, nil
}

func (a *sseAssembler) getOrCreateBlock(index int) *ContentBlock {
	if b, ok := a.blocks[index]; ok {
		return b
	}
	b := &ContentBlock{}
	a.blocks[index] = b
	a.order = append(a.order, index)
	return b
}

func (a *sseAssembler) handleMessageStart(raw []byte) error {
	var ev messageStartEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("decoding message_start: %w", err)
	}
	var skel messageStartSkeleton
	if len(ev.Message) > 0 {
		if err := json.Unmarshal(ev.Message, &skel); err != nil {
			return fmt.Errorf("decoding message_start message: %w", err)
		}
	}
	a.id, a.msgType, a.role, a.model = skel.ID, skel.Type, skel.Role, skel.Model
	if len(skel.Usage) > 0 {
		mergeUsage(&a.usage, skel.Usage)
	}
	return nil
}

func (a *sseAssembler) handleContentBlockStart(raw []byte) error {
	var ev indexedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("decoding content_block_start: %w", err)
	}

	var cb ContentBlock
	if len(ev.ContentBlock) > 0 {
		if err := json.Unmarshal(ev.ContentBlock, &cb); err != nil {
			return fmt.Errorf("decoding content_block_start content_block: %w", err)
		}
	}

	if _, exists := a.blocks[ev.Index]; !exists {
		a.order = append(a.order, ev.Index)
	}
	a.blocks[ev.Index] = &cb

	if cb.Type == BlockToolUse {
		a.partialJSON[ev.Index] = &strings.Builder{}
	}
	return nil
}

func (a *sseAssembler) handleContentBlockDelta(raw []byte) error {
	var ev indexedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("decoding content_block_delta: %w", err)
	}
	var delta contentDelta
	if len(ev.Delta) > 0 {
		if err := json.Unmarshal(ev.Delta, &delta); err != nil {
			return fmt.Errorf("decoding content_block_delta delta: %w", err)
		}
	}

	block := a.getOrCreateBlock(ev.Index)
	switch delta.Type {
	case "text_delta":
		block.Text += delta.Text
	case "input_json_delta":
		pj, ok := a.partialJSON[ev.Index]
		if !ok {
			pj = &strings.Builder{}
			a.partialJSON[ev.Index] = pj
		}
		pj.WriteString(delta.PartialJSON)
	case "thinking_delta":
		block.Thinking += delta.Thinking
	case "signature_delta":
		block.Signature += delta.Signature
	default:
		a.warnUnknown("content_block_delta type", delta.Type)
	}
	return nil
}

func (a *sseAssembler) handleContentBlockStop(raw []byte) error {
	var ev indexedEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("decoding content_block_stop: %w", err)
	}
	a.finalizeToolUseInput(ev.Index)
	return nil
}

// finalizeToolUseInput converts the accumulated partial_json string for a
// tool_use block index into its ContentBlock.Input. When the accumulated
// text is not valid JSON (the stream was cut off mid-value), it is instead
// preserved verbatim as a JSON string so the overall assembled message
// still marshals cleanly, and the stream is flagged truncated.
func (a *sseAssembler) finalizeToolUseInput(index int) {
	pj, ok := a.partialJSON[index]
	if !ok {
		return
	}
	delete(a.partialJSON, index)

	raw := pj.String()
	block := a.getOrCreateBlock(index)

	switch {
	case raw == "":
		block.Input = json.RawMessage("{}")
	case json.Valid([]byte(raw)):
		block.Input = json.RawMessage(raw)
	default:
		a.truncatedInput = true
		encoded, err := json.Marshal(raw)
		if err != nil {
			encoded = []byte(`"[unparseable partial tool_use input]"`)
		}
		block.Input = encoded
	}
}

func (a *sseAssembler) handleMessageDelta(raw []byte) error {
	var ev messageDeltaEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("decoding message_delta: %w", err)
	}
	if ev.Delta.StopReason != nil {
		a.stopReason = *ev.Delta.StopReason
	}
	if ev.Delta.StopSequence != nil {
		a.stopSequence = ev.Delta.StopSequence
	}
	if len(ev.Usage) > 0 {
		mergeUsage(&a.usage, ev.Usage)
	}
	return nil
}

func (a *sseAssembler) handleError(raw []byte) {
	var ev errorEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		a.streamErrorMessage = "sse error event (undecodable payload)"
		return
	}
	if ev.Error.Message != "" {
		a.streamErrorMessage = ev.Error.Message
	} else {
		a.streamErrorMessage = "sse error event"
	}
}

func (a *sseAssembler) warnUnknown(kind, value string) {
	key := kind + ":" + value
	if a.warned[key] {
		return
	}
	a.warned[key] = true
	log.Printf("splitter: internal/anthropic: ignoring unknown SSE %s %q", kind, value)
}

// mergeUsage applies only the fields present in raw onto dst, leaving
// fields already set by an earlier event (e.g. input_tokens from
// message_start) untouched when a later event (e.g. message_delta) omits
// them.
func mergeUsage(dst *Usage, raw json.RawMessage) {
	var patch struct {
		InputTokens              *int `json:"input_tokens"`
		OutputTokens             *int `json:"output_tokens"`
		CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(raw, &patch); err != nil {
		return
	}
	if patch.InputTokens != nil {
		dst.InputTokens = *patch.InputTokens
	}
	if patch.OutputTokens != nil {
		dst.OutputTokens = *patch.OutputTokens
	}
	if patch.CacheCreationInputTokens != nil {
		dst.CacheCreationInputTokens = *patch.CacheCreationInputTokens
	}
	if patch.CacheReadInputTokens != nil {
		dst.CacheReadInputTokens = *patch.CacheReadInputTokens
	}
}

// splitSSEBlocks splits a raw SSE byte stream into its blank-line separated
// event blocks, normalising CRLF line endings first.
func splitSSEBlocks(raw []byte) []string {
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.Split(normalized, "\n\n")
}

// parseSSEBlock extracts the "event:" name (if any) and the joined "data:"
// payload from one SSE block. Comment lines (starting with ":") and "id:"
// lines are ignored; splitter never uses SSE ids.
func parseSSEBlock(block string) (event, data string) {
	var dataLines []string
	for _, line := range strings.Split(block, "\n") {
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	return event, strings.Join(dataLines, "\n")
}

// jsonTypeField extracts the top-level "type" field from a JSON payload,
// used as a fallback event name when a block has no explicit "event:" line.
func jsonTypeField(data string) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return ""
	}
	return probe.Type
}
