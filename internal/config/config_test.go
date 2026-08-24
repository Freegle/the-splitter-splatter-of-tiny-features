package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()

	if cfg.Listen != "127.0.0.1:9925" {
		t.Errorf("Listen = %q, want 127.0.0.1:9925", cfg.Listen)
	}
	if cfg.Upstream != "https://api.anthropic.com" {
		t.Errorf("Upstream = %q", cfg.Upstream)
	}
	if cfg.Replay.Backend != "ollama" {
		t.Errorf("Replay.Backend = %q, want ollama", cfg.Replay.Backend)
	}
	if cfg.Replay.MaxConcurrentWorktrees != 2 {
		t.Errorf("Replay.MaxConcurrentWorktrees = %d, want 2", cfg.Replay.MaxConcurrentWorktrees)
	}
	if len(cfg.Backends) != 4 {
		t.Fatalf("len(Backends) = %d, want 4", len(cfg.Backends))
	}
	ollama, ok := cfg.Backends["ollama"]
	if !ok || ollama.Model != "qwen2.5-coder:7b" {
		t.Errorf("Backends[ollama] = %+v", ollama)
	}
	if cfg.Judge.Model != "claude-haiku-4-5" {
		t.Errorf("Judge.Model = %q", cfg.Judge.Model)
	}
	if cfg.Thresholds.DefaultHigh != 0.9 || cfg.Thresholds.DefaultLow != 0.5 {
		t.Errorf("Thresholds = %+v", cfg.Thresholds)
	}
	if cfg.Router.MinN != 30 || cfg.Router.MinWilsonLB != 0.9 || cfg.Router.DualDispatchPct != 5 {
		t.Errorf("Router = %+v", cfg.Router)
	}
	if cfg.Families == nil {
		t.Errorf("Families should be a non-nil empty map")
	}
	if got := cfg.Layers["*.vue"]; got != "frontend-ui" {
		t.Errorf(`Layers["*.vue"] = %q, want "frontend-ui"`, got)
	}
	if got := cfg.Layers["iznik-server-go/"]; got != "backend-api" {
		t.Errorf(`Layers["iznik-server-go/"] = %q, want "backend-api"`, got)
	}
	if got := cfg.Layers["Makefile"]; got != "build" {
		t.Errorf(`Layers["Makefile"] = %q, want "build"`, got)
	}
	wantLayerCount := 20
	if len(cfg.Layers) != wantLayerCount {
		t.Errorf("len(Layers) = %d, want %d", len(cfg.Layers), wantLayerCount)
	}
}

// withHome points $HOME (and unsets SPLITTER_CONFIG) at a fresh temp dir for
// the duration of the test, so default-path resolution is isolated.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SPLITTER_CONFIG", "")
	return home
}

func TestLoad_NoFileNoEnv_UsesDefaults(t *testing.T) {
	home := withHome(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9925" {
		t.Errorf("Listen = %q, want default", cfg.Listen)
	}
	want := filepath.Join(home, ".local/share/splitter/splitter.db")
	if cfg.DBPath != want {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, want)
	}
}

func TestLoad_ExplicitPath_Overrides(t *testing.T) {
	home := withHome(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
listen = "0.0.0.0:1234"
upstream = "https://example.test"

[replay]
backend = "together"
idle_minutes = 5
max_concurrent_worktrees = 4
batch_size = 10

[backends.ollama]
base_url = "http://localhost:11434/v1"
model = "custom-model"

[router]
min_n = 50
min_wilson_lb = 0.95
dual_dispatch_pct = 10
`
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:1234" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.Upstream != "https://example.test" {
		t.Errorf("Upstream = %q", cfg.Upstream)
	}
	if cfg.Replay.Backend != "together" || cfg.Replay.IdleMinutes != 5 {
		t.Errorf("Replay = %+v", cfg.Replay)
	}
	if cfg.Backends["ollama"].Model != "custom-model" {
		t.Errorf("Backends[ollama].Model = %q", cfg.Backends["ollama"].Model)
	}
	if cfg.Router.MinN != 50 {
		t.Errorf("Router.MinN = %d, want 50", cfg.Router.MinN)
	}
	// DBPath was not set in the override file, so the (tilde expanded)
	// default survives.
	wantDB := filepath.Join(home, ".local/share/splitter/splitter.db")
	if cfg.DBPath != wantDB {
		t.Errorf("DBPath should keep default when unset, got %q, want %q", cfg.DBPath, wantDB)
	}
}

func TestLoad_ExplicitPathMissing_Errors(t *testing.T) {
	withHome(t)

	_, err := Load("/nonexistent/splitter-config-test/config.toml")
	if err == nil {
		t.Fatal("Load with missing explicit path should error")
	}
}

func TestLoad_EnvVarPath(t *testing.T) {
	withHome(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`listen = "1.2.3.4:9999"`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("SPLITTER_CONFIG", path)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "1.2.3.4:9999" {
		t.Errorf("Listen = %q, want value from $SPLITTER_CONFIG file", cfg.Listen)
	}
}

func TestLoad_EnvVarPathMissing_Errors(t *testing.T) {
	withHome(t)
	t.Setenv("SPLITTER_CONFIG", "/nonexistent/splitter-config-test/config.toml")

	_, err := Load("")
	if err == nil {
		t.Fatal("Load with missing $SPLITTER_CONFIG path should error")
	}
}

func TestLoad_ExplicitPathBeatsEnvVar(t *testing.T) {
	withHome(t)

	dir := t.TempDir()
	envPath := filepath.Join(dir, "env-config.toml")
	explicitPath := filepath.Join(dir, "explicit-config.toml")
	if err := os.WriteFile(envPath, []byte(`listen = "9.9.9.9:1111"`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(explicitPath, []byte(`listen = "8.8.8.8:2222"`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("SPLITTER_CONFIG", envPath)

	cfg, err := Load(explicitPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "8.8.8.8:2222" {
		t.Errorf("Listen = %q, explicit path should win over $SPLITTER_CONFIG", cfg.Listen)
	}
}

func TestLoad_DefaultPath_WhenPresent(t *testing.T) {
	home := withHome(t)

	dir := filepath.Join(home, ".config", "splitter")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`listen = "127.0.0.1:5555"`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:5555" {
		t.Errorf("Listen = %q, want value from default config path", cfg.Listen)
	}
}

func TestLoad_DefaultPath_MissingIsOk(t *testing.T) {
	withHome(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load should not error when default path is absent: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9925" {
		t.Errorf("Listen = %q, want built-in default", cfg.Listen)
	}
}

func TestExpandTilde(t *testing.T) {
	home := withHome(t)

	tests := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/foo/bar", filepath.Join(home, "foo/bar")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := expandTilde(tt.in); got != tt.want {
			t.Errorf("expandTilde(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLoad_EnvFile_DoesNotOverrideExisting(t *testing.T) {
	withHome(t)

	t.Setenv("SPLITTER_TEST_EXISTING", "original")
	t.Setenv("SPLITTER_TEST_NEW", "")
	os.Unsetenv("SPLITTER_TEST_NEW")
	t.Cleanup(func() { os.Unsetenv("SPLITTER_TEST_NEW") })

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	contents := "# a comment\n\nSPLITTER_TEST_EXISTING=from-file\nSPLITTER_TEST_NEW=\"quoted-value\"\n"
	if err := os.WriteFile(envFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	configPath := filepath.Join(dir, "config.toml")
	configToml := "env_file = \"" + envFile + "\"\n"
	if err := os.WriteFile(configPath, []byte(configToml), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Load(configPath); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := os.Getenv("SPLITTER_TEST_EXISTING"); got != "original" {
		t.Errorf("SPLITTER_TEST_EXISTING = %q, want unchanged 'original'", got)
	}
	if got := os.Getenv("SPLITTER_TEST_NEW"); got != "quoted-value" {
		t.Errorf("SPLITTER_TEST_NEW = %q, want 'quoted-value' loaded from file", got)
	}
}

func TestLoad_ExampleConfigParses(t *testing.T) {
	withHome(t)

	cfg, err := Load("../../config.example.toml")
	if err != nil {
		t.Fatalf("Load(config.example.toml): %v", err)
	}
	if len(cfg.Backends) != 4 {
		t.Errorf("len(Backends) = %d, want 4", len(cfg.Backends))
	}
	for _, name := range []string{"ollama", "together", "gemini", "openai"} {
		if _, ok := cfg.Backends[name]; !ok {
			t.Errorf("Backends missing %q", name)
		}
	}
	if cfg.Judge.Model != "claude-haiku-4-5" {
		t.Errorf("Judge.Model = %q", cfg.Judge.Model)
	}
	if cfg.Router.MinN != 30 {
		t.Errorf("Router.MinN = %d", cfg.Router.MinN)
	}
	if got := cfg.Layers["*.vue"]; got != "frontend-ui" {
		t.Errorf(`Layers["*.vue"] = %q, want "frontend-ui" (config.example.toml should carry the layer defaults)`, got)
	}
	if len(cfg.Layers) == 0 {
		t.Error("Layers should not be empty when loaded from config.example.toml")
	}
}

func TestLoad_EnvFile_MissingIsOk(t *testing.T) {
	withHome(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load should not error when env_file is absent: %v", err)
	}
	if cfg.EnvFile == "" {
		t.Error("EnvFile should still be set to the (tilde expanded) default path")
	}
}
