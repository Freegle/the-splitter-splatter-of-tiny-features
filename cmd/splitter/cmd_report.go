// This file owns only the "report" dispatch map. It defines no report verb
// itself: each component that produces a report (e.g. "report spend" from
// internal/feature, "report agreement" from internal/verify, "report
// weekly" from internal/router) adds its own verb via registerReport() in
// its own cmd_*.go file's init().
package main

import (
	"fmt"
	"os"
	"sort"
)

// reportCommands maps a report sub-verb (the word after "report") to the
// function that runs it.
var reportCommands = map[string]func(args []string) error{}

// registerReport adds a report sub-verb to the registry.
func registerReport(name string, fn func(args []string) error) {
	reportCommands[name] = fn
}

func init() {
	register("report", runReport)
}

// runReport dispatches os.Args[2:] (passed as args here) to the report
// sub-verb named by args[0].
func runReport(args []string) error {
	if len(args) < 1 {
		reportUsage()
		return fmt.Errorf("missing report sub-command")
	}

	name := args[0]
	fn, ok := reportCommands[name]
	if !ok {
		reportUsage()
		return fmt.Errorf("unknown report sub-command %q", name)
	}

	return fn(args[1:])
}

// reportUsage prints the sorted list of registered report sub-commands to
// stderr.
func reportUsage() {
	names := make([]string, 0, len(reportCommands))
	for name := range reportCommands {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(os.Stderr, "usage: splitter report <sub-command> [args]")
	fmt.Fprintln(os.Stderr, "\nsub-commands:")
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
}
