package judge

import "testing"

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    Verdict
		wantErr bool
	}{
		{
			name: "plain JSON",
			text: `{"equivalent": true, "confidence": 0.9, "reason": "same output"}`,
			want: Verdict{Equivalent: true, Confidence: 0.9, Reason: "same output"},
		},
		{
			name: "prose wrapped, lenient parse extracts the object",
			text: "Sure, here's my answer:\n\n" +
				`{"equivalent": false, "confidence": 0.7, "reason": "different error handling"}` +
				"\n\nLet me know if that helps.",
			want: Verdict{Equivalent: false, Confidence: 0.7, Reason: "different error handling"},
		},
		{
			name: "markdown fenced",
			text: "```json\n" + `{"equivalent": true, "confidence": 1, "reason": "identical"}` + "\n```",
			want: Verdict{Equivalent: true, Confidence: 1, Reason: "identical"},
		},
		{
			name:    "no JSON object at all",
			text:    "I cannot determine equivalence.",
			wantErr: true,
		},
		{
			name:    "unbalanced braces",
			text:    `{"equivalent": true`,
			wantErr: true,
		},
		{
			name:    "empty text",
			text:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseVerdict(tt.text)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseVerdict(%q) = %+v, nil; want an error", tt.text, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVerdict(%q): %v", tt.text, err)
			}
			if got != tt.want {
				t.Errorf("ParseVerdict(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}

func TestVerdict_Agree(t *testing.T) {
	tests := []struct {
		name string
		v    Verdict
		want bool
	}{
		{"equivalent, exactly half confident", Verdict{Equivalent: true, Confidence: 0.5}, true},
		{"equivalent, just under half confident", Verdict{Equivalent: true, Confidence: 0.49}, false},
		{"equivalent, fully confident", Verdict{Equivalent: true, Confidence: 1}, true},
		{"not equivalent, fully confident", Verdict{Equivalent: false, Confidence: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v.Agree(); got != tt.want {
				t.Errorf("%+v.Agree() = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
