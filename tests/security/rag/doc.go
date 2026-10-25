// Package ragsec is the RAG security suite of master plan 12.6 and 16.4: what
// ingestion and retrieval do with content that is hostile, hidden, oversized,
// archived, misnamed, out of date or another tenant's.
//
// Files, API responses and retrieved passages are data, not instructions. They
// cannot change system policy, enable tools, request secrets or expand scope.
// Each test here drives the real pipeline end to end (the production source
// provider, the sandboxed parser worker, the chunker, the FTS5 and vector
// indexes, the query pipeline and the grounded answerer, over the real job
// queue) and asserts on what the pipeline did, never on what a script said. The
// only scripted part is the model: the fake provider (internal/providers/fake)
// records every prompt it receives, and the tests read that record.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-003  TestSecRAGInjectionDocCannotChangePolicy
//	SEC-004  TestSecRAGInjectionDocCannotEnableTools
//	SEC-005  TestSecRAGInjectionDocCannotRequestSecrets
//	SEC-006  TestSecRAGInjectionDocCannotExpandScope
//	SEC-012  TestSecRAGHiddenTextFlagged
//	SEC-013  TestSecRAGOversizedFileRejected
//	SEC-014  TestSecRAGArchiveBomb
//	SEC-015  TestSecRAGPathTraversal
//	SEC-017  TestSecRAGStaleSourceNotPresentedAsCurrent
//	SEC-018  TestSecRAGCrossTenantZeroLeak
//	SEC-018  TestSecRAGCrossTenantStoresReturnNoForeignRow (SEC-021, read at the stores)
//	SEC-018  TestSecRAGLeakDetectorsSeeAPlantedLeak (the negative control of the two above)
//
// # The fixture corpus
//
// Text fixtures are committed under testdata/: prompt-injection documents in
// English and Italian, the look-alike policies of two tenants that differ only
// in a marker string, and an HTML page with white-on-white text. Everything
// hostile in binary form (docx with a vanish run, archive bombs, zip-slip
// entry names, oversized files) is built at run time with
// internal/rag/parse/parsetest, the way the parser's own tests do it: nothing
// hostile is committed, and no secret is a literal (the sentinels are generated
// per run).
//
// # The parser worker
//
// The tests that parse run the production worker (cmd/alixai-parser), built
// once by TestMain, or taken from ALIXAI_PARSER_BIN when a cross-compiled test
// binary runs elsewhere. A stand-in parser would prove nothing about hidden
// text or bombs.
//
// # Positive controls
//
// Every "X never appears" assertion has a control that the thing was there to
// be found: the other tenant's marker does appear for the other tenant, the
// injected text does appear in the fenced evidence it was retrieved into, the
// hidden marker was flagged by the parser rather than lost by it. A test that
// passes because nothing was ingested is a failure.
package ragsec
