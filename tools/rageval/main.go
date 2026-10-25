// Command rageval is the reproducible RAG measurement of master plan 17.3 and
// 22.4: it builds a labelled corpus into the real index (internal/rag), asks the
// real query pipeline and the real answer package every labelled question, and
// reports what the plan asks to be measured.
//
//	go run ./tools/rageval run --embedder fake|localai --corpus tests/performance/rag --out <dir> [flags]
//	go run ./tools/rageval check <dir>...
//
// # What it measures
//
//   - recall@k, MRR and nDCG@k of the evidence package, and citation precision,
//     fact accuracy and the unsupported-answer rate of the answers (metrics.go
//     defines each);
//   - exact identifiers, tables, semantic and cross-language questions,
//     conflicting versions, stale, unavailable and deleted sources, permission
//     denials and cross-tenant leakage, each as a kind of question with its own
//     row of metrics;
//   - ingestion throughput (chunk + index + embed; the sandboxed parser is not
//     in it) and index size;
//   - p50/p95 latency per question and, in a sweep over a synthetic corpus,
//     retrieval latency by SCOPE SIZE (1,000, 20,000 and 50,000 chunks) as well
//     as for the corpus as a whole: ADR 0032 shows that one large scope
//     dominates the p95 and that a corpus-wide average hides it (scale.go);
//   - peak memory: the operating system's high-water mark of the process, per
//     phase (each phase of the sweep runs in its own process).
//
// # What a run with the fake embedder is, and is not
//
// The fake embedder is a hashed bag of words. A run with it measures the index,
// the vector scan, the fusion, the authorisation checks and the harness around
// the models; it says nothing about a model, and a paraphrase or a question in
// another language is out of its reach (the per-kind rows show exactly that).
// The answers are written by an extractive, label-blind generator (generator.go):
// the no-model baseline, which makes the answer-stage metrics a measure of
// retrieval plus verification and not of generation. Results are flagged
// synthetic.
//
// # A run with LocalAI (M2-15)
//
// --embedder localai uses a running LocalAI (the tool never starts one): one
// throwaway embedding first loads the model and teaches the run the dimension and
// the name the runtime answers under, which is recorded next to the registry id
// (components.embedder). Then:
//
//   - every question's pipeline timing per stage is recorded (latency.stages_ms):
//     stages_ms.embed is the query-embedding latency, what the model adds to a query;
//   - --docker-container (with --docker-bin, --docker-interval) samples the memory
//     of that container with docker stats for the whole run (container_memory, in the
//     bench envelope's form: cgroup usage, not VmRSS). A container that is not under
//     the profile's cap is a failed run, not a lite result;
//   - the scale sweep embeds with the model too (scale.embedder says so; the default
//     is one 1,000-chunk scope, because a model costs about 40 ms a chunk on the Lite
//     cap), which makes its build's embedding stage and every query's embed stage real;
//   - --scale-reader-cache-kib and --scale-pin-scopes are the two knobs of the
//     reader-cache experiment: the page cache of each reader connection, and asking
//     one scope of a size instead of rotating over all of them;
//   - --mode rerank is allowed with localai only as the harness's lexical reranker, no
//     model, and the result is labelled synthetic (no adapter serves the Reranker role);
//   - the answer generator is still the extractive one: no controller is qualified.
//
// # The result files
//
// One run writes two files, side by side, with one stem:
//
//	<stem>.rag    the full result (result.go, rag-result.schema.json)
//	<stem>.json   the same envelope as tools/bench (schemas/bench/result.schema.json)
//
// `go run ./tools/bench check <dir>` validates the .json; `rageval check <dir>`
// validates the .rag (its schema, every metric re-derived from the raw
// per-question records, the profile label) and that the .json is the projection
// of it. The bench schema is closed and its metrics are those of the
// TaskProposal benchmark, so the full RAG result cannot live in it; the .json
// carries the questions that must be declined, as refusal cases, which fit it
// without bending a field (envelope.go). Extending schemas/bench with a rag
// section and teaching tools/bench check to dispatch on it would let one
// checker validate both; both are M0-11's and are reported, not edited.
//
// Results under lite, balanced or experimental-4g are emulated, never
// certified. The tool applies no cgroup cap: the profile is the one the result
// stands for and the host is recorded in it.
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
  rageval run   --embedder fake|localai --corpus <dir> --out <dir> [flags]
  rageval check [-reject-synthetic] [-schemas dir] <dir or file>...

run "rageval run -h" for the run flags.
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
	case "scenario":
		// The phases of the scale sweep, run in their own process. Not a user
		// command: the sweep starts it with a fixed argument list and the spec on
		// its standard input.
		return scenarioMain(args[1:], os.Stdin, stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "rageval: unknown command %q\n\n%s", args[0], usage)
	return 2
}
