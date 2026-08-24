package judge

import (
	"encoding/json"
	"fmt"
)

// Verdict is the judge's parsed answer to one item.
type Verdict struct {
	Equivalent bool    `json:"equivalent"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// Agree reports whether this verdict counts as agreement: equivalent and
// at least half confident, per DESIGN.md.
func (v Verdict) Agree() bool {
	return v.Equivalent && v.Confidence >= 0.5
}

// ParseVerdict extracts the first brace-balanced {...} block from text and
// decodes it as a Verdict, tolerating surrounding prose or markdown
// fencing a judge model may add despite the "Answer ONLY JSON" instruction.
func ParseVerdict(text string) (Verdict, error) {
	block, err := firstJSONObject(text)
	if err != nil {
		return Verdict{}, err
	}
	var v Verdict
	if err := json.Unmarshal([]byte(block), &v); err != nil {
		return Verdict{}, fmt.Errorf("decoding judge verdict JSON %q: %w", block, err)
	}
	return v, nil
}

// firstJSONObject returns the first brace-balanced {...} substring of
// text. '{' and '}' are single-byte ASCII characters, so a plain byte scan
// never misidentifies a UTF-8 continuation byte as one of them.
func firstJSONObject(text string) (string, error) {
	start := -1
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 {
				return text[start : i+1], nil
			}
		}
	}
	if start == -1 {
		return "", fmt.Errorf("no JSON object found in judge response")
	}
	return "", fmt.Errorf("no balanced JSON object found in judge response")
}
