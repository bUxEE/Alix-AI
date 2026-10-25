// Command providerconf runs the AlixAI provider conformance suite (additive
// plan section 15, tests/conformance/providers) against one provider and
// writes the conformance report that qualifies a profile in the registry.
//
//	go run ./tools/providerconf run                       # OpenAI, recorded fixtures
//	go run ./tools/providerconf run --out report.json     # write the report to a file
//	go run ./tools/providerconf run --profile mine.json   # the operator's own profile record
//	go run ./tools/providerconf check report.json         # validate a report against the schema
//
// A run is made against recorded fixtures served by a loopback server (the
// real adapter, a placeholder credential, no network beyond 127.0.0.1) unless
// live mode is asked for twice: the --live flag AND ALIXAI_PROVIDERCONF_LIVE=1,
// in a binary built with -tags live. Live mode spends real money on the
// operator's account (human-gated, task X-H02); this tool never switches to it
// on its own, and the verify commands never use it.
//
// The report is validated against schemas/providers/conformance-report before
// it is written; an invalid report is an error and nothing is written. The exit
// code is 0 for a passed report, 1 for a failed one (the report is still
// written) or an error, 2 for a usage error.
package main

import "os"

func main() {
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr, os.Getenv))
}
