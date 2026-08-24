package main

import "fmt"

// version identifies this build. It is the zero value "dev" for a local
// build; release builds override it via -ldflags "-X main.version=...".
var version = "dev"

func init() {
	register("version", runVersion)
}

// runVersion prints the build version to stdout.
func runVersion(args []string) error {
	fmt.Println("splitter " + version)
	return nil
}
