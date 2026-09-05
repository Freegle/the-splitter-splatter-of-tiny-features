package judge

import (
	"testing"
)

func TestExtractErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{
			name: "anthropic error shape",
			body: []byte(`{"error":{"type":"invalid_request_error","message":"max_tokens is too large"}}`),
			want: "max_tokens is too large",
		},
		{
			name: "non-JSON body",
			body: []byte("502 Bad Gateway"),
			want: "502 Bad Gateway",
		},
		{
			name: "empty body",
			body: []byte(""),
			want: "",
		},
		{
			name: "JSON with no error object",
			body: []byte(`{"ok":false}`),
			want: `{"ok":false}`,
		},
		{
			name: "error object with empty message",
			body: []byte(`{"error":{"message":""}}`),
			want: `{"error":{"message":""}}`,
		},
		{
			name: "JSON string value (wrong shape)",
			body: []byte(`"just a string"`),
			want: `"just a string"`,
		},
		{
			name: "JSON array (wrong shape)",
			body: []byte(`[{"error":{"message":"fail"}}]`),
			want: `[{"error":{"message":"fail"}}]`,
		},
		{
			name: "error message is whitespace",
			body: []byte(`{"error":{"message":" "}}`),
			want: " ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractErrorMessage(tt.body)
			if got != tt.want {
				t.Errorf("extractErrorMessage(%s) = %q, want %q", string(tt.body), got, tt.want)
			}
		})
	}
}
