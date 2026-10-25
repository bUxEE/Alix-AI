package probes

import "github.com/buxee/alix-ai/internal/integrationaudit"

// Checks builds the runtime and application checks of master plan 4.5, one for each
// of the sections the static checks of internal/integrationaudit leave open:
//
//	2   application.deterministic_results    typed, stable results
//	6   application.scope_probes             positive and negative scope probes
//	8   application.safe_execution           prepare, confirm, commit, idempotency, expiry, cancel, stale, uncertain
//	9   application.failure_degradation      typed failures, no generic success
//	10  application.runtime_smoke            text and action paths, audit events, cleanup
//
// The checks are built from the options so that a test, or a composition root
// that wires a transport, can give them their own provisioner and scratch
// directory without any global state.
func Checks(o Options) []integrationaudit.Check {
	return []integrationaudit.Check{
		resultsCheck(o),
		scopeCheck(o),
		safetyCheck(o),
		degradationCheck(o),
		smokeCheck(o),
	}
}

// init registers the checks with the audit's default registry, so that importing this
// package (internal/cli does, for the binary) is all it takes for the sections to stop
// being "not run". The provisioner is found through SetProvisionerSource and the
// adapter itself.
func init() {
	for _, c := range Checks(Options{}) {
		integrationaudit.Register(c)
	}
}
