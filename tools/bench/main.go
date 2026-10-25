// Command bench is the reproducible measurement tool behind every model
// decision (master plan 7.5, 8.3, 17.3). It sends a corpus of synthetic cases
// to a text-generation provider and writes one JSON result per run, in a
// format that is the same for every model, quantisation, backend and profile
// (schemas/bench/result.schema.json).
//
//	go run ./tools/bench run --provider fake|localai --model <registry id> \
//	    --profile lite|balanced|4g --corpus <file.jsonl> --out benchmarks/results/
//	go run ./tools/bench check <dir>...
//	go run ./tools/bench gate --corpus <file.jsonl> [--out report.json]
//
// run measures. The fake provider answers every case with the expected
// proposal, so a run with it exercises the harness and says nothing about a
// model (the result is flagged synthetic). The localai provider talks to a
// running LocalAI through adapters/localai; start it yourself (the tool never
// starts a container). Results under lite, balanced or experimental-4g are
// emulated, never certified.
//
// check validates every result file under the given directories against the
// schema and re-derives each result's aggregates from its raw cases. It fails
// when it finds no result at all.
//
// gate runs the deterministic out-of-scope gate (internal/dialogue/scopegate) on a corpus
// with no model and reports what it refused, what it passed and its false refusals. run
// --gate puts the same gate in front of the model (M0-21).
//
// benchmarks/README.md defines every metric.
//
// Exit status: 0 success, 1 a failed run or a failed check, 2 usage error.
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage:
  bench run   --provider fake|localai --model <registry id> --profile lite|balanced|4g
              --corpus <file.jsonl> --out <dir> [flags]
  bench check [-reject-synthetic] [-schemas dir] <dir>...
  bench gate  --corpus <file.jsonl> [--out report.json]

run "bench run -h" for the run flags.
`

// run is main without the process exit, so tests can drive it.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], stdout, stderr)
	case "check":
		return checkCommand(args[1:], stdout, stderr)
	case "gate":
		return gateCommand(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "bench: unknown command %q\n\n%s", args[0], usage)
	return 2
}
