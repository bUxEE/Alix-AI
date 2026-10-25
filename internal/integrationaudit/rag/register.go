package rag

import "github.com/buxee/alix-ai/internal/integrationaudit"

// Checks builds the RAG checks of master plan 4.5, from the options, so that a test, or a
// composition root that holds the deployment's store, can give them their own deployment
// and scratch directory without any global state:
//
//	3   application.rag_sources    undeclared and unreachable sources; every declared field verified live
//	3   application.rag_citations  every citation resolves, permission-checked, to the original
//	6   application.rag_scope      chunk-level cross-scope probes, and the suite's retrieve probes
//	10  application.rag_smoke      the RAG path of the runtime smoke test
func Checks(o Options) []integrationaudit.Check {
	return []integrationaudit.Check{
		sourcesCheck(o),
		citationsCheck(o),
		scopeCheck(o),
		smokeCheck(o),
	}
}

// init registers the checks with the audit's default registry, so that importing this package
// (internal/cli does, for the binary) is all it takes for the RAG parts of sections 3, 6 and
// 10 to stop being "not run". The deployment is found through SetDeploymentSource, or is the
// default one (DefaultDeployment).
func init() {
	for _, c := range Checks(Options{}) {
		integrationaudit.Register(c)
	}
}
