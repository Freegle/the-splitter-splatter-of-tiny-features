// Package config loads splitter's TOML configuration and the env file it
// references.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors the TOML schema documented in DESIGN.md.
type Config struct {
	Listen   string `toml:"listen"`
	Upstream string `toml:"upstream"`
	DBPath   string `toml:"db_path"`
	RepoPath string `toml:"repo_path"`
	EnvFile  string `toml:"env_file"`

	Replay     ReplayConfig             `toml:"replay"`
	Backends   map[string]BackendConfig `toml:"backends"`
	Judge      JudgeConfig              `toml:"judge"`
	Thresholds ThresholdsConfig         `toml:"thresholds"`
	Tests      map[string]string        `toml:"tests"`
	Router     RouterConfig             `toml:"router"`

	// Families maps an exact model id to a family override string, used by
	// internal/router.Family instead of the built-in normalisation rules.
	Families map[string]string `toml:"families"`

	// Layers maps a path prefix or glob pattern to an eval task layer name
	// (frontend-ui, backend-api, database, infra, tests, docs, build). Built
	// from the DESIGN.md defaults for this codebase family; consumers match
	// a touched file's path against these patterns to derive its layer.
	Layers map[string]string `toml:"layers"`

	// Evals controls the eval library's ladder evaluation (internal/evals).
	Evals EvalsConfig `toml:"evals"`

	// ModelCutoffs maps a model family or exact model id to its training
	// data cutoff as "YYYY-MM", the eval library's contamination guard
	// (DESIGN.md "task_date + contamination guard"). An exact model id
	// match wins over a family match.
	ModelCutoffs map[string]string `toml:"model_cutoffs"`
}

// EvalsConfig controls the eval library's per-track ladder evaluation
// (DESIGN.md "Ladder evaluation").
type EvalsConfig struct {
	// LadderTrack selects what a "track" is: "language" (default, a model
	// can be past its ceiling on one language while still climbing
	// another), "layer", or "none" for one global ladder.
	LadderTrack string `toml:"ladder_track"`
	// StopWilsonUpper abandons a rung (and all higher rungs in its track)
	// once the Wilson UPPER bound of its pass rate drops below this value,
	// with at least StopMinN scored tasks.
	StopWilsonUpper float64 `toml:"stop_wilson_upper"`
	StopMinN        int     `toml:"stop_min_n"`
	// FutilityConsecutiveFails abandons a rung immediately after this many
	// consecutive failures with zero passes, regardless of StopMinN.
	FutilityConsecutiveFails int `toml:"futility_consecutive_fails"`
}

// ReplayConfig controls the Phase 3 replay worker.
type ReplayConfig struct {
	Backend                string `toml:"backend"`
	IdleMinutes            int    `toml:"idle_minutes"`
	MaxConcurrentWorktrees int    `toml:"max_concurrent_worktrees"`
	BatchSize              int    `toml:"batch_size"`
}

// BackendConfig describes one OpenAI-compatible replay or routing backend.
type BackendConfig struct {
	BaseURL   string `toml:"base_url"`
	APIKeyEnv string `toml:"api_key_env"`
	Model     string `toml:"model"`
}

// JudgeConfig controls the Haiku batch judge.
type JudgeConfig struct {
	Model           string `toml:"model"`
	APIKeyEnv       string `toml:"api_key_env"`
	MaxContextChars int    `toml:"max_context_chars"`
}

// ThresholdPair is a per (language, turn_type) similarity threshold override.
type ThresholdPair struct {
	High float64 `toml:"high"`
	Low  float64 `toml:"low"`
}

// ThresholdsConfig holds the verification cascade's similarity thresholds.
type ThresholdsConfig struct {
	DefaultHigh float64 `toml:"default_high"`
	DefaultLow  float64 `toml:"default_low"`
	// Overrides is keyed "<language>/<turn_type>".
	Overrides map[string]ThresholdPair `toml:"overrides"`
}

// RouterConfig controls Phase 4 routability decisions.
type RouterConfig struct {
	MinN            int     `toml:"min_n"`
	MinWilsonLB     float64 `toml:"min_wilson_lb"`
	DualDispatchPct int     `toml:"dual_dispatch_pct"`
}

// defaultConfigRelPath is appended to the home directory to find the
// fallback config file when no explicit path or env var is given.
const defaultConfigRelPath = ".config/splitter/config.toml"

// Default returns the built-in configuration matching DESIGN.md.
func Default() *Config {
	return &Config{
		Listen:   "127.0.0.1:9925",
		Upstream: "https://api.anthropic.com",
		DBPath:   "~/.local/share/splitter/splitter.db",
		RepoPath: "/home/edward/FreegleDockerWSL",
		EnvFile:  "~/.config/splitter/env",
		Replay: ReplayConfig{
			Backend:                "ollama",
			IdleMinutes:            30,
			MaxConcurrentWorktrees: 2,
			BatchSize:              100,
		},
		Backends: map[string]BackendConfig{
			"ollama": {
				BaseURL: "http://localhost:11434/v1",
				Model:   "qwen2.5-coder:7b",
			},
			"together": {
				BaseURL:   "https://api.together.xyz/v1",
				APIKeyEnv: "TOGETHER_API_KEY",
				Model:     "Qwen/Qwen2.5-Coder-32B-Instruct",
			},
			"gemini": {
				BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
				APIKeyEnv: "GEMINI_API_KEY",
				Model:     "gemini-2.5-flash",
			},
			"openai": {
				BaseURL:   "https://api.openai.com/v1",
				APIKeyEnv: "OPENAI_API_KEY",
				Model:     "gpt-4o-mini",
			},
		},
		Judge: JudgeConfig{
			Model:           "claude-haiku-4-5",
			APIKeyEnv:       "ANTHROPIC_API_KEY",
			MaxContextChars: 8000,
		},
		Thresholds: ThresholdsConfig{
			DefaultHigh: 0.9,
			DefaultLow:  0.5,
			Overrides:   map[string]ThresholdPair{},
		},
		Tests: map[string]string{},
		Router: RouterConfig{
			MinN:            30,
			MinWilsonLB:     0.9,
			DualDispatchPct: 5,
		},
		Families: map[string]string{},
		Layers:   defaultLayers(),
		Evals: EvalsConfig{
			LadderTrack:              "language",
			StopWilsonUpper:          0.2,
			StopMinN:                 8,
			FutilityConsecutiveFails: 6,
		},
		ModelCutoffs: map[string]string{},
	}
}

// defaultLayers returns the built-in path prefix/glob to layer mapping for
// this codebase family, as listed in DESIGN.md's eval library section.
func defaultLayers() map[string]string {
	return map[string]string{
		"iznik-nuxt3/":     "frontend-ui",
		"components/":      "frontend-ui",
		"pages/":           "frontend-ui",
		"*.vue":            "frontend-ui",
		"*.css":            "frontend-ui",
		"iznik-server-go/": "backend-api",
		"*api*/":           "backend-api",
		"*handler*.go":     "backend-api",
		"migrations/":      "database",
		"*.sql":            "database",
		"docker*":          "infra",
		".circleci/":       "infra",
		"*.yml":            "infra",
		"*_test.*":         "tests",
		"tests/":           "tests",
		"spec/":            "tests",
		"docs/":            "docs",
		"*.md":             "docs",
		"Makefile":         "build",
		"scripts/":         "build",
	}
}

// Load resolves the config file via the precedence chain (explicit path
// argument, $SPLITTER_CONFIG, ~/.config/splitter/config.toml, built-in
// defaults), decodes it over the built-in defaults, expands tildes in
// filesystem paths, and loads the env file into the process environment
// without overriding vars that are already set.
func Load(path string) (*Config, error) {
	cfg := Default()

	resolved, required, err := resolveConfigPath(path)
	if err != nil {
		return nil, fmt.Errorf("resolving config path: %w", err)
	}

	if resolved != "" {
		data, readErr := os.ReadFile(resolved)
		switch {
		case readErr == nil:
			if _, decErr := toml.Decode(string(data), cfg); decErr != nil {
				return nil, fmt.Errorf("parsing config %s: %w", resolved, decErr)
			}
		case os.IsNotExist(readErr) && !required:
			// Fall through to built-in defaults.
		default:
			return nil, fmt.Errorf("reading config %s: %w", resolved, readErr)
		}
	}

	cfg.DBPath = expandTilde(cfg.DBPath)
	cfg.RepoPath = expandTilde(cfg.RepoPath)
	cfg.EnvFile = expandTilde(cfg.EnvFile)

	if err := loadEnvFile(cfg.EnvFile); err != nil {
		return nil, fmt.Errorf("loading env file %s: %w", cfg.EnvFile, err)
	}

	return cfg, nil
}

// resolveConfigPath applies the precedence chain and returns the tilde
// expanded path to try, and whether that path was explicitly requested
// (in which case a missing file is an error rather than a silent fallback).
func resolveConfigPath(explicit string) (path string, required bool, err error) {
	if explicit != "" {
		return expandTilde(explicit), true, nil
	}
	if envPath := os.Getenv("SPLITTER_CONFIG"); envPath != "" {
		return expandTilde(envPath), true, nil
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		// No home directory available: fall back to built-in defaults only.
		return "", false, nil
	}
	return filepath.Join(home, defaultConfigRelPath), false, nil
}

// expandTilde replaces a leading "~" or "~/" with the user's home directory.
// Paths that do not start with "~" are returned unchanged.
func expandTilde(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// loadEnvFile parses path as KEY=VALUE lines and sets each into the process
// environment, skipping keys that are already set. A missing file is not an
// error. Blank lines and lines starting with "#" are ignored.
func loadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("setting env var %s: %w", key, err)
		}
	}
	return nil
}
