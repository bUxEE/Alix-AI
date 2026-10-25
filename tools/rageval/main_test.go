package main

import (
	"os"
	"testing"
)

// TestMain lets the test binary serve as the child of the scale sweep: the
// sweep re-executes os.Executable() with the subcommand "scenario", which in a
// test binary is this binary, so the real child-process path is exercised.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "scenario" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
