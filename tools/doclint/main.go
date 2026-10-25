// Command doclint fails when the documentation breaks the CLM-8B consistency rules (ADR 0029).
//
//	go run ./tools/doclint docs README.md
package main

import (
	"fmt"
	"io"
	"os"
)

func main() { os.Exit(run(os.Args[1:], ".", os.Stdout, os.Stderr)) }

// run lints the root-relative files and directories in args; 0 clean, 1 findings, 2 usage or I/O error.
func run(args []string, root string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: doclint <file-or-dir>...")
		return 2
	}
	findings, err := LintPaths(root, args)
	if err != nil {
		fmt.Fprintln(stderr, "doclint:", err)
		return 2
	}
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
	}
	if len(findings) > 0 {
		fmt.Fprintf(stderr, "doclint: %d finding(s)\n", len(findings))
		return 1
	}
	fmt.Fprintln(stdout, "doclint: ok")
	return 0
}
