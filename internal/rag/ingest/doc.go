// Package ingest is the ingestion pipeline of master plan 12.3, run as durable,
// bounded jobs on internal/jobs (rule 13): it takes a registered source from
// discovery to an active, searchable index generation and keeps it that way.
//
//	source discovery                       JobSync      class 5
//	  -> authorization and scope check     (the job's verified scope against the definition)
//	  -> content/MIME and size validation  (the source provider)
//	  -> immutable source version + hash   (ragingest_version, one row per item and generation)
//	  -> sandboxed parsing                 JobParse     class 5   (one disposable worker process per document)
//	  -> structure-aware parent/child chunks + FTS indexing
//	                                       JobIndex     class 5
//	  -> queued embeddings                 rag.vector.embed class 5   (internal/rag/vector)
//	  -> index generation activation       JobFinalize  class 5
//
// A rebuild because a component version changed runs the same three stages as
// JobReindexParse, JobReindexIndex and JobReindexFinalize at class 6 ("OCR,
// reindexing and maintenance", 17.1): while an active generation still serves a
// source, rebuilding it yields to ingestion too. JobMaintain (class 6) compacts
// what is hidden and sweeps what nothing serves. Each stage is its own job type,
// so a failure retries on its own and the queue shows how far a source has got.
// A job's payload carries ids only (payloads are clear text); every row carries
// application, tenant and workspace; every job runs under the scope that
// initiated it, which the Pool re-validates just before the handler runs: a
// revoked principal gets a failed job (permission_denied) and no scan, no parse,
// no write. A sync may be requested under a declared service identity
// (SyncOptions.ServiceIdentity), carried by every stage it queues.
//
// # Wiring
//
// The composition root creates the Pause first, because the queue takes its
// DequeuePolicy when it is built:
//
//	pause := ingest.NewPause(diskPressureHook, clock, 0, logger)
//	q, _ := jobs.NewQueue(db, jobs.Options{Config: jobsCfg, Policy: pause.Policy(nil), ...})
//	runner, _ := parse.NewRunner(ingest.FromConfig().ParseConfig())
//	svc, _ := ingest.New(ingest.Deps{DB: db, Queue: q, Index: ix, Vectors: vec, Embedding: emb,
//	        Registry: reg, Parser: ingest.NewRunnerParser(runner), Chunker: chk, Audit: log,
//	        Pause: pause, Operator: operatorPolicy, ...})
//	svc.Handle(pool)                 // before pool.Run
//	svc.Start(ctx)                   // the readiness probe; surface its Readiness to operators
//
// New registers the job types (and vector.EmbedJobSpec when embedding is
// configured: do not also call vector.Pipeline.Register). For ingestion to yield
// to everything a user is waiting for, set the jobs section's
// interactive_up_to_class to 4: the queue's default holds classes 5 and 6 back
// only while class 1 or 2 work waits.
//
// # Incremental and idempotent
//
// A scan compares the source with the snapshot of the last accepted scan
// (ragingest_item, stored at full time precision, which is what the provider's
// size-and-time shortcut and its racy-window rule compare) and returns what was
// added, modified and deleted. Only those items get a new version, parse, index
// write and embedding; an untouched tree costs one directory walk. A modified
// item's new version replaces the old one inside the same generation in one
// index transaction, so the old text keeps serving until the new is indexed.
// Unchanged chunks keep their chunk id (the chunker derives it from content and
// position), so their embeddings are found again by the vector index and not
// made twice; chunks that vanished are removed from the vector index
// (vec_owed). Every stage is safe to run twice: a job delivered again finds
// the version past its stage and completes only what the first delivery did
// not (the next stage's job, the embedding batches, the activation check).
//
// Stage jobs are queued by idempotency key (per version and requeue), in a
// bounded window (Config.EnqueueBatch): each version that finishes takes the
// place of the one that left, so a source of a hundred thousand documents never
// holds a hundred thousand jobs. A sync also finds stage jobs that ended failed
// and queues them again, at most three times, then fails the version (a
// recorded outcome in the status, never half-indexed).
//
// What the parser cannot read is a recorded outcome, not a silent gap: a
// document-caused failure (malformed, encrypted, a bomb, over the time or memory
// limit, needs OCR) fails that version at once and is not retried; an
// environment-caused one (the sandbox could not start) is retried. A document
// that fails keeps its previously indexed version serving until the item
// changes again or is deleted.
//
// # Component versions and generations
//
// A generation records the parser, chunker, OCR (never: there is no OCR engine,
// and the parser's Config.Validate refuses it) and embedder versions it was
// built with. When this build's versions differ from the active generation's, a
// sync builds a new generation beside it: every item is parsed again (class 6),
// the build is validated (index.ValidateGeneration against the number of
// documents the pipeline knows it wrote) and activated in one transaction. The
// old generation keeps answering until then and is retained for one rollback.
// Activation does not wait for embeddings: the generation serves full-text
// results the moment it validates (12.3, "FTS before embeddings"; 18, reduced
// semantic recall is labelled by whoever reads the vector index's Skipped), and
// its vectors arrive as the embedding jobs finish, most of them as reuse of the
// replaced generation's. Those are released once the new generation is fully
// embedded. An embedding runtime that is down costs embeddings and nothing
// else; the jobs that failed are queued again by the next sync.
//
// A build that would leave a serving generation with no documents at all (every
// document failing under a new parser) is refused and the serving generation
// stays. Output of another parser or chunker version than the generation's
// record is never mixed into it: a stage that finds its generation built with
// other components withdraws its version and asks for a sync.
//
// # The mass-deletion guard
//
// An unmounted NAS share or removable drive leaves a reachable, empty directory
// at its mount point, and a scan of it reports everything the source held as
// deleted. Before a scan's Deleted is applied (guard.go) it is refused as a
// whole when the root is empty and the previous scan was not (whatever the size
// of the source), when more than Config.Guard.MaxDeleted items would go, or when
// more than MaxFraction of a source of at least MinPrevious items would go.
// Nothing from a refused scan is applied: no tombstone, no new version, no
// snapshot change. The source is marked unavailable with the reason
// mass_deletion, last_success does not advance, the refusal is audited
// (ingest.mass_deletion_held) in the same transaction that records the hold, and
// the job fails with a typed unavailable error (IsMassDeletion) that is not
// retried; the next scheduled sync is the retry, and a remount clears the hold.
// A genuine mass deletion needs ConfirmMassDeletion: an operator action
// (OperatorAuthorizer, denied when none is configured) bound to the digest of
// exactly the deletion set that was refused, audited, valid for
// Config.ConfirmTTL and used once. Only items that left a reachable root count;
// an item that is merely no longer admitted (a changed declaration, a size
// limit, a link) is a rule's decision and cannot be an unmounted share. There is
// no switch that turns the guard off: raise the limits.
//
// The other behaviours of the provider are kept by not undoing them: a scan that
// cannot finish (unreachable root, interrupted) returns no changes and marks the
// source unavailable without touching the snapshot; reaching max_files or
// max_total_bytes (resource_exhausted) applies nothing, pauses the source and is
// not retried; a directory that cannot be listed keeps its records (they are in
// Next, and no tombstone is written for them); links, junctions and reparse
// points, device files and a name that contradicts its bytes are skipped by the
// provider and, when they were indexed before, tombstoned.
//
// # Deletion
//
// A deletion is first written down: the snapshot row goes, the tombstone row is
// added and the item's versions are superseded in ONE transaction (the outbox of
// outbox.go), so no moment exists at which the item is forgotten but not
// recorded as deleted. The index is then told (the document is hidden in every
// generation at once) and the vector index follows (the embeddings of the item's
// chunks are removed), under the source's write lock so that no stage can write
// the document between "recorded" and "hidden". A crash in between leaves an
// open row that the next sync, removal or maintenance pass finishes.
// RemoveSource tombstones everything a source held, hides it in the index at
// once (withdrawing every generation) and, with the purge behaviour of the
// declaration, removes its embeddings; RemoveTenant does the same for every
// source of the scope's tenant in every workspace and is an operator action.
// Both are audited in the same transaction as the tombstones (deletion.source,
// deletion.tenant; the chain is never cut) and refuse to proceed if the audit
// log cannot record them. The index rows are removed physically by compaction
// after Config.CompactGrace; the per-source purge window of a declaration is
// recorded on each tombstone but the grace of the compaction is global.
//
// # Disk pressure
//
// Pause asks the DiskPressure hook (the resource module provides the
// production one) and caches the answer. Installed as the queue's policy it
// stops ingestion work from being leased while pressure lasts: no attempt is
// spent and nothing is written; interactive classes are untouched. A hook that
// fails is pressure (unknown is treated as pressure). Every stage also checks
// when it starts, for the window between lease and start, and asks to be retried.
// The embedding job is held back too; maintenance, which only removes data, is
// not.
//
// # The parser worker
//
// The worker (cmd/alixai-parser, ADR 0014) is built with
// go build -ldflags="-s -w" -trimpath, pure Go. One disposable process serves
// one document: a reused worker costs 0.14 to 0.47 ms per 4 KiB document but a
// fresh process 37 ms on Linux and 70 to 112 ms on Windows, so the throughput
// of ingestion is bounded by Config.Parser.MaxConcurrent and the class 5 worker
// count; budget accordingly (the real-worker test parsed four small documents
// of four formats and indexed them in under 0.4 s on the Windows dev box).
// Service.Start probes the worker at service start, bounded by
// Config.Parser.ProbeTimeout, and reports the sandbox level to operators
// (Readiness): L2 is Landlock and seccomp on Linux; L1 is limits only (a
// container's seccomp profile blocked them, or Windows or macOS), in which case
// "the parser has no network" rests on the import-graph test alone, and the
// report says so. On Windows exclude the worker executable from antivirus
// scanning: the first start of a freshly built worker took 26.5 s under Avast
// against the parser's own 30 s limit, so the startup limit is configurable
// (Config.Parser.StartupTimeout, default 90 s) and the probe pays that first
// start before any job does. A failed probe makes parse jobs fail closed and
// retryable; it is repeated once a minute. macOS and linux/arm64 guarantees are
// untested.
//
// # Concurrency
//
// Handlers run concurrently across sources and stages. Two in-process locks
// (deps.go) serialise what must not interleave: one scan, removal or
// confirmation of a source at a time (syncLock, held across a directory walk),
// and the write lock (held only for database work) around writing a scan with
// its deletions, around an index stage's "is my version still current" check and
// its write, and around activation. Across processes the snapshot sequence
// number guards the snapshot; running two ingestion processes over one database
// is not supported.
//
// # Cross-module coupling
//
// The embedding job reads chunk text back through vector.ChunkSource
// (chunkreader.go), which selects from the index's own ragindex_chunk table
// under the scope, generation, tombstone and hidden predicates: the index keeps
// the only copy of the text and offers no read by id. This is the one place
// this package reads another module's table.
package ingest
