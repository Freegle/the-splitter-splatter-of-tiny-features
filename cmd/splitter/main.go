// Command splitter is the single static binary exposing every splitter
// subcommand. This file holds only the subcommand registry and dispatch;
// each subcommand lives in its own cmd_*.go file and adds itself here via
// register() from an init() function. Never edit this file to add a
// command.
package main

import (
	"fmt"
	"os"
	"sort"
)

// commands maps a subcommand name to the function that runs it.
var commands = map[string]func(args []string) error{}

// register adds a subcommand to the registry. Call it from an init()
// function in the subcommand's own cmd_*.go file.
func register(name string, fn func(args []string) error) {
	commands[name] = fn
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	name := os.Args[1]
	fn, ok := commands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "splitter: unknown command %q\n\n", name)
		usage()
		os.Exit(2)
	}

	if err := fn(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "splitter %s: %v\n", name, err)
		os.Exit(1)
	}
}

// usage prints the sorted list of registered subcommands to stderr.
func usage() {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintln(os.Stderr, "usage: splitter <command> [args]")
	fmt.Fprintln(os.Stderr, "\ncommands:")
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
}
