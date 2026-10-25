// Command soak is the mixed-load and soak harness of master plan 7.4, 17 and
// 22.4: it puts a configurable load on a running AlixAI stack (by default one
// interactive session doing booking and RAG turns at a human pace, plus
// continuous re-ingestion of the clinic policies source), samples what the
// stack costs while it runs, and fails the run on the conditions the plan
// forbids. It is the harness of the human-run 24-hour and 7-day soaks (M5-H01);
// its own tests run in seconds against fakes.
//
//	go run ./tools/soak --duration 24h --profile lite [--out <dir>] [flags]
//	go run ./tools/soak check [-reject-synthetic] [-require-pass] <dir or file>...
//
// # What it samples
//
// On an interval, for the length of the run: the memory the stack uses and its
// swap (docker stats and docker inspect for containers, /proc/<pid>/status for
// host processes on Linux); the model roles from /readyz, the queue from
// /v1/ops/queue, or all of them and the governor's unload and reload counters
// from the operator's /metrics (--metrics-url); and, around every turn and
// every re-ingestion, the latency.
//
// # What fails a run
//
//   - swap in use (master plan 7.4: no swap in a certified interactive workload), or
//     swap that could not be read from every sample unless --allow-unmeasured-swap;
//   - steady-state headroom under max(15% of the profile's memory, 1 GB)
//     (RequiredHeadroomMiB), judged on the profile's memory against the sum of
//     everything sampled, never per container;
//   - a protected role (controller, embedder, STT, TTS, VAD) that was configured
//     at the start and is not ok after the warm-up, or any model reload
//     (resource-budgets R3.5): the eviction of the controller, retrieval or
//     speech workers is a defect, not a degradation;
//   - more failed turns or ingestions than --max-error-rate, no turn or no
//     ingestion at all, a stack that could not be read, a restart, a run cut short;
//   - a stack not in the state the decision axis (--decision off|on) declares.
//
// The verdict is a pure function of the recorded observations (Evaluate), so
// `soak check` re-derives it from a stored result.
//
// # The decision-layer axis
//
// The optional scorer (milestone D) is the router role. The harness cannot
// switch it on a running stack, so --decision is a precondition and a label: the
// run refuses to start when the stack is not in that state (off: no router
// configured; on: router configured and ok), and the result records the axis, so
// the same mix run once with the scorer off and once, where qualified (balanced;
// lite with --decision-qualified), with it on can be compared. experimental-4g
// never runs it.
//
// # What it cannot see
//
// The harness does not check the caps of the containers it samples: the profile
// is the one the result stands for (emulated, never certified), and the
// headroom rule is applied to the profile's memory. Make sure the stack runs
// under the profile's compose file (deploy/container/stack/compose.<profile>.yaml).
//
// LocalAI evicts the least recently used backend when a request needs one that
// is not resident (LOCALAI_MAX_ACTIVE_BACKENDS). The governor's counters count
// only the reloads that follow its own unloads, so that eviction is visible only
// as a role leaving ok or as the latency of the turn after it.
//
// # The result
//
// One <stem>.soak file (result.go): the shared envelope fields of tools/bench
// (run_id, timestamp, git, host, profile, emulated, synthetic,
// container_limits), the load mix, the rules, every sample and every operation,
// the summary and the failures. Results are emulated, never certified.
//
// Exit status: 0 the run passed, 1 it failed (or could not start, or a check
// failed), 2 usage error.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

const usage = `usage:
  soak [run] --duration <d> --profile lite|balanced [--out <dir>] [flags]
  soak check [-reject-synthetic] [-require-pass] <dir or file>...

run "soak run -h" for the run flags.
`

// run is main without the process exit, so tests can drive it.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "check":
			return checkCommand(args[1:], stdout, stderr)
		case "run":
			args = args[1:]
		case "-h", "-help", "--help", "help":
			fmt.Fprint(stdout, usage)
			return 0
		}
	}
	return runCommand(ctx, args, stdout, stderr)
}
