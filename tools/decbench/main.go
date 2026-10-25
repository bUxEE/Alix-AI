// Command decbench is the reproducible measurement tool for the decision layer (master
// plan 7.5, 8.3, 8.5, 17.3). The layer is optional, bounded, non-generative and
// non-authoritative: a scorer only ranks a bounded set of authorised candidates, and every
// selected id is re-resolved by the deterministic adapter before anything uses it.
//
//	go run ./tools/decbench run --config all|1|2|3|4|5 --scorer fake|localai|clm \
//	    --corpus <jsonl> --out <dir> [--profile lite|balanced|high] [--artifact <file>]
//	go run ./tools/decbench check <dir>...
//	go run ./tools/decbench gate --baseline <result.json> --candidate <result.json>
//
// run measures five configurations on identical fixtures: deterministic rules only, the
// qualified sub-1B scorer, the CLM-8B scorer, direct controller routing, and the scorer
// followed by the controller when needed. It writes ONE result file per run
// (schemas/bench/decision-result.schema.json). --scorer fake is the scripted fake scorer
// of internal/decision: its results are flagged synthetic and measure the harness, not a
// model. Every result is emulated: a profile is a resource cap on a larger machine, never
// certified hardware. The tool never starts a container or a model server; --scorer
// localai|clm talks to one you started, on loopback.
//
// check validates every result file against the schema and re-derives its aggregates from
// the raw cases. It fails when it finds no result at all.
//
// gate applies the default-enable gate (DefaultEnableGate) to two results.
//
// benchmarks/decision/README.md defines every metric.
//
// Exit status: 0 success, 1 a failed run, check or gate, 2 usage error.
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
  decbench run   --config all|1|2|3|4|5 --scorer fake|localai|clm --corpus <file.jsonl> --out <dir>
                 [--profile lite|balanced|high] [--artifact <file>] [flags]
  decbench check [-reject-synthetic] [-schemas dir] <dir or file>...
  decbench gate  --baseline <result.json> --candidate <result.json> [--baseline-config 4] [--candidate-config 5]

run "decbench run -h" for the run flags.
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
	fmt.Fprintf(stderr, "decbench: unknown command %q\n\n%s", args[0], usage)
	return 2
}
