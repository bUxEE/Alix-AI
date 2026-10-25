// Package isolationsec is the isolation suite of master plan 10.2 and 16.4 (bullet 2): what is
// shared between tenants, workspaces, principals and source versions, and must not be.
//
// The three tests are system tests over the real components, built from their exported APIs and
// observed from outside: ids, counters, results, bytes on disk and the prompts a model was sent.
//
// # Tests and the backlog (docs/security/test-backlog.md, section 4.2)
//
//	SEC-022  TestSecNoSharedCacheAcrossScopes
//	SEC-023  TestSecContentHashReuseDoesNotCrossTenants
//	SEC-029  TestSecNoForeignTextInAnyPrompt
//
// # SEC-022: what caches exist
//
// The retrieval, evidence, query-embedding and prompt-assembly layers (internal/rag/query,
// internal/rag/answer, internal/dialogue, internal/files) state that they keep no cache and the
// tests do not pretend otherwise. What exists is: index.CacheKey, the key any cache of a result of
// the index must use (application, tenant, workspace, principal, roles, capability scopes, namespace,
// the index version and the query), over the version the real index reports; the resident cache of the
// flat vector index, keyed by scope and generation; and the open and preview references of the
// citation resolver, bound to a scope and an expiry. Each is exercised through its exported API with
// two tenants, two workspaces, two applications, two principals and a source update. A result cache
// that a future layer adds is covered by the key test, and by nothing else: the key is the contract.
//
// # SEC-023: content-hash keyed reuse
//
// Embedding reuse (the vector pipeline over the flat index) and content-addressed file storage
// (internal/files) are each run twice, once with a copy of identical content held by another tenant
// and once without, and what the first tenant can observe (ids, sizes, counters, embedder calls,
// result rows, the bytes it holds on disk) is compared. Inside one tenant the same content is reused
// and deduplicated, which proves the test can see reuse.
//
// # SEC-029: foreign text in a prompt
//
// One engine, one session store, one index, one database and one model serve two tenants whose
// callers are the same principal in the same workspace and session. Tenant B is seeded with a unique
// marker in every source a prompt is built from: the application's data, the documents the retrieval
// answers from, the history and the state of its own conversation, what its client supplied (readable
// context and UI tool descriptions), and its settings. Tenant A then asks the same questions, and
// every prompt the model was sent for A is scanned for every marker of B, on every path. Tenant B's
// own turns are the positive control (its markers do reach its prompts), and a marker planted into
// one of A's prompt sources is detected by the same scanner.
//
// # The parser worker
//
// SEC-029 ingests documents through the production folder source and the sandboxed parser worker
// (cmd/alixai-parser), built once by TestMain, or taken from ALIXAI_PARSER_BIN. A stand-in parser
// would prove nothing about what reaches the index.
package isolationsec
