// Package rag is the RAG half of the integration audit (master plan 4.5 check 3, live half,
// and the RAG parts of checks 6 and 10; 9.4 stage "authorised RAG-source and citation
// probes"; 12.2 and 12.5). The static checks of internal/integrationaudit read what the
// package declares; the runtime probes of internal/integrationaudit/probes call the
// application; these look at the sources the package declares, at the deployment that has to
// serve them, and at AlixAI's own retrieval and citation machinery over them. They are
// deterministic and ask no model anything: the report body carries no timestamp, no
// identifier generated for a run and no host path, and two audits of the same package give
// the same findings.
//
// Importing this package registers four checks with the audit's default registry
// (register.go); internal/cli imports it for the binary. Checks builds the same four from
// Options, so a test, or a composition root that holds the deployment's store, can give them
// their own deployment and scratch directory without any global state.
//
//	3   application.rag_sources     undeclared and unreachable sources; each declared field looked at live
//	3   application.rag_citations   every citation resolves, permission-checked, to the original
//	6   application.rag_scope       chunk-level cross-scope probes; the suite's retrieve probes
//	10  application.rag_smoke       the RAG path of the runtime smoke test
//
// # What they look at, and what they never do
//
// Two things are real and two are the audit's own.
//
// The deployment (Deployment, SetDeploymentSource) is what the checks may ask about the sources
// of an application: the source providers it runs, the operator's mounts, and the sources it
// has registered. The audit never scans for any of it. What a deployment does not tell the
// audit is recorded as unverified, never as absent and never as passed: with no provider for a
// source type, no mounts, or no way to list registrations, the corresponding source or question
// is unverified. A root is only ever probed (Provider.Probe); nothing walks a real root and
// nothing reads a document of a real source.
//
// The lab (lab.go) is the core's own retrieval stack, the real lexical index, query pipeline,
// answer step and citation resolver, in a throwaway SQLite store that exists for one check run.
// It is filled with synthetic passages the audit writes itself, one per hosted source and scope,
// each with a marker no other passage holds, in the tenants, workspaces and principals of the
// package's test suites (generated identifiers; the report names the logical references). It
// never touches the application: nothing in it is a record, a file or a tenant of the
// application. What it proves is how AlixAI's own machinery treats the sources the package
// declares, not what the application's documents say. Its source catalog and its proposer of
// claims are stand-ins, and are the audit's own (see lab.go and standin.go): the catalog admits a
// source to the scope it is registered in; the proposer copies one sentence of a retrieved
// passage, and is not a model.
//
// # Check 3: sources
//
// For every declared source the audit builds the definition the core would register, bound the
// way the declaration says, and looks at the eight fields master plan 4.5 requires. A field is
// verified by something the audit executes or looks at now ("live"), or, where only an
// organisational fact exists, by being stated ("declaration"); the report's source record says
// "live" only when the source was reached and every field held.
//
//	owner       named (declaration)
//	scope       the core's CheckScope admits the scope the declaration binds the source to and
//	            refuses another application, tenant and workspace (probes)
//	include,
//	exclude     compile in the core's own matcher and admit something: for a source shipped in the
//	            package, a file the package holds below its path; for any other root, a name the
//	            rules admit that the exclusions leave standing (the root itself is never listed)
//	credential  none, or a reference the application's inspection reports among its credential
//	            references (never a value)
//	refresh,
//	deletion    stated in a form the core accepts (declaration); the citation check then exercises
//	            them: a replaced document is served in its new text and not its old, a removed source
//	            serves nothing and its references resolve no more
//	citation    the core's preview resolver, which the citation check exercises; any other kind is
//	            unverified (the audit never fetches a URL and cannot call an operation of the
//	            application)
//
// Then the audit compares the declaration with the sources the deployment holds for the
// application. A registered source the package does not declare is critical
// (ragprobe.source_undeclared): registration may only instantiate what an audited package declares.
// A registration bound to a tenant or workspace its declaration does not allow is critical. A
// declared source whose root cannot be reached from the deployment is a warning
// (ragprobe.source_unreachable): retrieval fails closed and serves nothing from it, so it is not
// unsafe, but it cannot be called verified. An unreachable source and a source nothing can reach
// are unverified, and a source is "live" only if none of this applies.
//
// # Check 6: chunk-level scope
//
// In every suite, every fixture principal retrieves the passage of its own scope (it must be
// found, in an evidence package bound to the caller's scope), asks for the passage of every other
// scope, once as a question and once naming the other scope's source (nothing of the other scope,
// not its text, source, generation or chunks, may reach the package, and so the model), and
// presents the other scope's chunk references to the index (they must not be readable and must
// not be live: the answer is that of a chunk that does not exist). Another workspace of the same
// tenant is another scope. The suite's own retrieve probes are executed too: allow must find the
// source's passage in the principal's scope, deny must find nothing of the source's other scopes.
// A source the declared binding refuses cannot be a lab source; a probe along the role, principal
// or document dimension has nothing to run on (a source declaration carries no such rule) and is
// unverified. A denied retrieval is ignored, not refused, by design, so the declared error code
// cannot be compared and is not.
//
// # Check 3: citations
//
// The answer step writes a grounded answer for each principal and hosted source (with the
// extractive proposer), and every citation is held to master plan 12.5: an opaque file reference
// (never a path or a URL); it resolves for the principal it was written for, to the source and
// version it was issued for, with a preview that is a passage of the original; it does not
// resolve for another tenant, workspace or principal, nor for a reference that never existed, and
// the refusals are one and the same (otherwise the answer is an oracle); it stops when the source
// is revoked or removed; it expires.
//
// # Check 10: the RAG smoke path
//
// For every `rag` path of every suite: retrieval for the path's principal, a grounded answer with
// citations that resolve, the path's own utterance put to the same pipeline and compared with what
// the path expects of citations, and the removal of the synthetic sources, after which nothing of
// them may be served. The audit's passages are its own text, so the application's own question can
// be answered from them only by accident: a path that expects a cited answer is verified only if
// the pipeline produces one, and a path that expects none only if retrieval finds nothing. It never
// fails a path for what the synthetic corpus does or does not say, and never lets what a path
// expects change what the stand-in does. The audit events a path expects (rag.retrieved,
// dialogue.turn and the like) are appended by the dialogue layer, which the audit does not run,
// and no code of the RAG pipeline writes them; they are unverified and the audit never appends one.
//
// # Unverified is not pass
//
// Registering a check of a section silences what the static check and the runtime probes record
// for want of one (rag_sources:live, scope_probe:<id>, smoke_path:<id>). Every early exit of these
// checks therefore records what it did not look at: one unverified item for each declared source,
// each retrieve probe and each RAG path, with the reason. A lab that cannot start (no scratch
// directory, no store) is a warning, so that an operator accepts it explicitly, and unverified.
//
// # Extension points
//
// The audit framework hands a check an Input with the verified package, the adapter and the audit's
// scope, and nothing else. The following are therefore seams of this package, supplied by a
// composition root through SetDeploymentSource, and are unverified until it does: the sources the
// deployment has registered (Deployment.Registrations; FromRegistry builds it from the durable
// registry of internal/rag/sources, which needs the deployment's store), the operator's mounts
// (Deployment.Mounts) and the providers it runs (Deployment.Providers; the default is the folder
// provider only).
package rag
