package main

import "testing"

func TestRegister_AddsCommand(t *testing.T) {
	called := false
	register("__test_cmd__", func(args []string) error {
		called = true
		return nil
	})
	defer delete(commands, "__test_cmd__")

	fn, ok := commands["__test_cmd__"]
	if !ok {
		t.Fatal("register did not add the command to the registry")
	}
	if err := fn(nil); err != nil {
		t.Fatalf("running registered command: %v", err)
	}
	if !called {
		t.Error("registered function was not invoked")
	}
}

func TestVersionCommand_Registered(t *testing.T) {
	if _, ok := commands["version"]; !ok {
		t.Fatal(`"version" command not registered`)
	}
}

func TestRunVersion_Succeeds(t *testing.T) {
	if err := runVersion(nil); err != nil {
		t.Fatalf("runVersion: %v", err)
	}
}

func TestReportCommand_Registered(t *testing.T) {
	if _, ok := commands["report"]; !ok {
		t.Fatal(`"report" command not registered`)
	}
}

func TestRegisterReport_DispatchesToVerb(t *testing.T) {
	called := false
	registerReport("__test_verb__", func(args []string) error {
		called = true
		if len(args) != 1 || args[0] != "extra" {
			t.Errorf("args = %v, want [extra]", args)
		}
		return nil
	})
	defer delete(reportCommands, "__test_verb__")

	if err := runReport([]string{"__test_verb__", "extra"}); err != nil {
		t.Fatalf("runReport: %v", err)
	}
	if !called {
		t.Error("registered report verb was not invoked")
	}
}

func TestRunReport_UnknownVerb_Errors(t *testing.T) {
	if err := runReport([]string{"__does_not_exist__"}); err == nil {
		t.Fatal("expected an error for an unknown report sub-command")
	}
}

func TestRunReport_NoVerb_Errors(t *testing.T) {
	if err := runReport(nil); err == nil {
		t.Fatal("expected an error when no report sub-command is given")
	}
}
