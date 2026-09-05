package proxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPackedRef(t *testing.T) {
	t.Parallel()

	baseContent := `# pack-refs with: peeled fully-peeled sorted
a3f91c7d84b26e05f1c9d3a7b82e405c1d6f8b92 refs/heads/master
b7e24a0d19f83c65be0147d2a9c3f581e0b64a37 refs/heads/feature/x
c05d8f36a1b94e72d83f6015c7a2be49318d0f6e refs/tags/v1.0
^d41862b7e05a39cf1d780b6a24e93f0c5871ade4
e93c07a5d82f416b0e35c19d7a640b28f5c31e07 refs/remotes/origin/master
`

	tests := []struct {
		name    string
		content string
		ref     string
		want    string
	}{
		{
			name:    "refs_heads_master",
			content: baseContent,
			ref:     "refs/heads/master",
			want:    "a3f91c7d84b26e05f1c9d3a7b82e405c1d6f8b92",
		},
		{
			name:    "refs_heads_feature_x_with_slash",
			content: baseContent,
			ref:     "refs/heads/feature/x",
			want:    "b7e24a0d19f83c65be0147d2a9c3f581e0b64a37",
		},
		{
			name:    "refs_tags_v1_0_returns_tag_not_peel",
			content: baseContent,
			ref:     "refs/tags/v1.0",
			want:    "c05d8f36a1b94e72d83f6015c7a2be49318d0f6e",
		},
		{
			name:    "refs_remotes_origin_master_keeps_going_past_peel",
			content: baseContent,
			ref:     "refs/remotes/origin/master",
			want:    "e93c07a5d82f416b0e35c19d7a640b28f5c31e07",
		},
		{
			name:    "nonexistent_ref_returns_empty",
			content: baseContent,
			ref:     "refs/heads/nonexistent",
			want:    "",
		},
		{
			name:    "prefix_match_returns_empty_exact_only",
			content: baseContent,
			ref:     "refs/heads/mast",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			packedRefsPath := filepath.Join(dir, "packed-refs")
			if err := os.WriteFile(packedRefsPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("os.WriteFile failed: %v", err)
			}

			got := readPackedRef(dir, tt.ref)
			if got != tt.want {
				t.Errorf("readPackedRef(%q, %q) = %q, want %q", dir, tt.ref, got, tt.want)
			}
		})
	}
}

func TestReadPackedRef_NoFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	got := readPackedRef(dir, "refs/heads/master")
	if got != "" {
		t.Errorf("readPackedRef on missing file = %q, want empty", got)
	}
}

func TestReadPackedRef_MalformedLines(t *testing.T) {
	t.Parallel()
	malformedContent := `# comment line
no_space_here_at_all
   lots of whitespace   
# another comment
deadbeef1234567890abcdef1234567890abcdef refs/heads/valid
`
	dir := t.TempDir()
	packedRefsPath := filepath.Join(dir, "packed-refs")
	if err := os.WriteFile(packedRefsPath, []byte(malformedContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile failed: %v", err)
	}

	got := readPackedRef(dir, "refs/heads/valid")
	want := "deadbeef1234567890abcdef1234567890abcdef"
	if got != want {
		t.Errorf("readPackedRef with malformed lines = %q, want %q", got, want)
	}
}
