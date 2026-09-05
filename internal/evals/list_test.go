package evals

import (
	"database/sql"
	"testing"

	"github.com/freegle/splitter/internal/store"
)

func TestSummarizeCharacteristics(t *testing.T) {
	tests := []struct {
		name string
		task store.EvalTaskRow
		want string
	}{
		{
			name: "all four set",
			task: store.EvalTaskRow{
				Language:   sql.NullString{String: "go", Valid: true},
				Layer:      sql.NullString{String: "backend", Valid: true},
				Nature:     sql.NullString{String: "bugfix", Valid: true},
				Difficulty: sql.NullString{String: "easy", Valid: true},
			},
			want: "go/backend/bugfix/easy",
		},
		{
			name: "all four NULL (zero values)",
			task: store.EvalTaskRow{
				Language:   sql.NullString{},
				Layer:      sql.NullString{},
				Nature:     sql.NullString{},
				Difficulty: sql.NullString{},
			},
			want: "-/-/-/-",
		},
		{
			name: "all four Valid but empty string",
			task: store.EvalTaskRow{
				Language:   sql.NullString{String: "", Valid: true},
				Layer:      sql.NullString{String: "", Valid: true},
				Nature:     sql.NullString{String: "", Valid: true},
				Difficulty: sql.NullString{String: "", Valid: true},
			},
			want: "-/-/-/-",
		},
		{
			name: "mixture: Language and Nature set",
			task: store.EvalTaskRow{
				Language:   sql.NullString{String: "go", Valid: true},
				Layer:      sql.NullString{},
				Nature:     sql.NullString{String: "bugfix", Valid: true},
				Difficulty: sql.NullString{},
			},
			want: "go/-/bugfix/-",
		},
		{
			name: "one field Valid but empty among three set values",
			task: store.EvalTaskRow{
				Language:   sql.NullString{String: "go", Valid: true},
				Layer:      sql.NullString{String: "", Valid: true},
				Nature:     sql.NullString{String: "bugfix", Valid: true},
				Difficulty: sql.NullString{String: "easy", Valid: true},
			},
			want: "go/-/bugfix/easy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeCharacteristics(tt.task)
			if got != tt.want {
				t.Errorf("summarizeCharacteristics() = %q, want %q", got, tt.want)
			}
		})
	}
}
