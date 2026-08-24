package verify

import "encoding/json"

// textMessage builds a complete Anthropic message JSON (the shape Verify
// expects) with a single text block.
func textMessage(text string) []byte {
	return mustMarshalMessage([]any{
		map[string]any{"type": "text", "text": text},
	})
}

// editMessage builds a message with a single Edit tool_use block.
func editMessage(filePath, oldString, newString string) []byte {
	return mustMarshalMessage([]any{
		toolUseBlock("Edit", map[string]any{
			"file_path":  filePath,
			"old_string": oldString,
			"new_string": newString,
		}),
	})
}

// writeMessage builds a message with a single Write tool_use block.
func writeMessage(filePath, content string) []byte {
	return mustMarshalMessage([]any{
		toolUseBlock("Write", map[string]any{
			"file_path": filePath,
			"content":   content,
		}),
	})
}

func toolUseBlock(name string, input map[string]any) map[string]any {
	return map[string]any{
		"type":  "tool_use",
		"id":    "toolu_" + name,
		"name":  name,
		"input": input,
	}
}

func mustMarshalMessage(content []any) []byte {
	b, err := json.Marshal(map[string]any{
		"type":    "message",
		"role":    "assistant",
		"content": content,
	})
	if err != nil {
		panic(err)
	}
	return b
}
