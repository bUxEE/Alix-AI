// Command schemacheck compiles every JSON Schema (2020-12) under the given
// directories and checks the examples next to them:
//
//	go run ./tools/schemacheck [-v] [-require-examples] [-root dir]... <dir>...
//
// For a schema <dir>/<name>.schema.json it requires every file in
// <dir>/examples/<name>/valid/*.json to validate and every file in
// <dir>/examples/<name>/invalid/*.json to be rejected, i.e. for
// schemas/<area>/<name>.schema.json the examples live in
// schemas/<area>/examples/<name>/{valid,invalid}/*.json.
//
// $refs of the form urn:alixai:schema:<area>:<name>:v<N> resolve to
// <schemas root>/<area>/<name>.schema.json, where the schemas root is the
// nearest ancestor directory named "schemas" of each checked directory (so
// checking schemas/conversation alone still resolves refs into schemas/common)
// or any directory given with -root. No file or network reference is followed.
//
// Exit status: 0 everything behaves as declared; 1 a mismatch, a schema that
// does not load or compile, or a path with no schemas; 2 usage error.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schemacheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var roots stringList
	verbose := fs.Bool("v", false, "also list why each invalid example is rejected")
	require := fs.Bool("require-examples", false, "fail for any schema that has no examples")
	fs.Var(&roots, "root", "extra schemas root used to resolve urn:alixai:schema refs (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: schemacheck [-v] [-require-examples] [-root dir]... <dir>...")
		fmt.Fprintln(stderr, "  compiles every *.schema.json under each <dir> (JSON Schema 2020-12) and checks")
		fmt.Fprintln(stderr, "  <schema-dir>/examples/<name>/valid/*.json pass and .../invalid/*.json fail")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}

	report, err := Check(fs.Args(), Options{Roots: roots, RequireExamples: *require})
	if err != nil {
		fmt.Fprintf(stderr, "schemacheck: %v\n", err)
		return 1
	}
	report.Write(stdout, *verbose)
	if !report.OK() {
		return 1
	}
	return 0
}
