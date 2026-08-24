package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplayCommand_Registered(t *testing.T) {
	if _, ok := commands["replay"]; !ok {
		t.Fatal(`"replay" command not registered`)
	}
}

func TestRunReplay_UnknownBackendReturnsError(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "splitter.db")
	configPath := filepath.Join(dir, "config.toml")

	toml := "db_path = \"" + dbPath + "\"\n[replay]\nbackend = \"doesnotexist\"\n"
	if err := os.WriteFile(configPath, []byte(toml), 0o644); err != nil {
		t.Fatalf("writing config fixture: %v", err)
	}

	if err := runReplay([]string{"-config", configPath}); err == nil {
		t.Fatal("expected an error for an unknown replay backend")
	}
}
