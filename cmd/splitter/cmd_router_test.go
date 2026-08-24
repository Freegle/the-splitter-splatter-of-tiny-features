package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/freegle/splitter/internal/router"
)

func TestRouterCommand_Registered(t *testing.T) {
	if _, ok := commands["router"]; !ok {
		t.Fatal(`"router" command not registered`)
	}
}

func TestRunRouter_NoSubcommand_Errors(t *testing.T) {
	if err := runRouter(nil); err == nil {
		t.Fatal("expected an error when no router sub-command is given")
	}
}

func TestRunRouter_UnknownSubcommand_Errors(t *testing.T) {
	if err := runRouter([]string{"bogus"}); err == nil {
		t.Fatal("expected an error for an unknown router sub-command")
	}
}

func TestWriteRouterUpdateTable_EmptyResult(t *testing.T) {
	var buf bytes.Buffer
	writeRouterUpdateTable(&buf, &router.UpdateResult{})
	if !strings.Contains(buf.String(), "nothing to update") {
		t.Errorf("output = %q, want a message about nothing to update", buf.String())
	}
}

func TestWriteRouterUpdateTable_RowsAndDivergence(t *testing.T) {
	var buf bytes.Buffer
	writeRouterUpdateTable(&buf, &router.UpdateResult{
		Rows: []router.CategoryStats{
			{
				Category: "single_file_edit|iznik-server-go", Families: "claude-sonnet>qwen-coder:7b",
				N: 12, Agreed: 5, WilsonLB: 0.22, Routable: false,
				DisabledReason: "divergent_version:qwen3-coder:7b(n=12,rate=41.7%,family=91.2%)",
				Diverged: []router.VersionDivergence{
					{Version: "qwen3-coder:7b", N: 12, AgreementRate: 0.417, FamilyRate: 0.912},
				},
			},
			{
				Category: "question_answer|docs", Families: "claude-haiku>gemini-flash",
				N: 40, Agreed: 39, WilsonLB: 0.91, Routable: true,
			},
		},
	})
	out := buf.String()

	if !strings.Contains(out, "single_file_edit|iznik-server-go") {
		t.Errorf("output missing first category row: %q", out)
	}
	if !strings.Contains(out, "question_answer|docs") {
		t.Errorf("output missing second category row: %q", out)
	}
	if !strings.Contains(out, "divergence flagged") || !strings.Contains(out, "qwen3-coder:7b") {
		t.Errorf("output missing divergence flag line: %q", out)
	}
}

func TestWriteRouterUpdateTable_NoDivergenceSaysSo(t *testing.T) {
	var buf bytes.Buffer
	writeRouterUpdateTable(&buf, &router.UpdateResult{
		Rows: []router.CategoryStats{
			{Category: "question_answer|docs", Families: "claude-haiku>gemini-flash", N: 40, Agreed: 39, WilsonLB: 0.91, Routable: true},
		},
	})
	if !strings.Contains(buf.String(), "no per-exact-version divergence flagged") {
		t.Errorf("output = %q, want the no-divergence line", buf.String())
	}
}
