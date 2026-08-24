package main

import (
	"testing"

	"github.com/freegle/splitter/internal/config"
)

func TestProxyCommand_Registered(t *testing.T) {
	if _, ok := commands["proxy"]; !ok {
		t.Fatal(`"proxy" command not registered`)
	}
}

func TestResolveProxyListen_OverrideWinsOverConfig(t *testing.T) {
	cfg := &config.Config{Listen: "127.0.0.1:9925"}
	got := resolveProxyListen(cfg, "127.0.0.1:1234")
	if got != "127.0.0.1:1234" {
		t.Errorf("resolveProxyListen() = %q, want override", got)
	}
}

func TestResolveProxyListen_FallsBackToConfig(t *testing.T) {
	cfg := &config.Config{Listen: "127.0.0.1:9925"}
	got := resolveProxyListen(cfg, "")
	if got != "127.0.0.1:9925" {
		t.Errorf("resolveProxyListen() = %q, want config value", got)
	}
}
