// Package vector is the vector half of RAG retrieval (master plan 6, 12.3,
// 12.4): the replaceable VectorIndexProvider interface, the embedded
// implementation (Flat, ADR 0011), embedding reuse and the queued embedding job.
//
// # The index
//
// VectorIndexProvider has Upsert, Reuse, Search, four delete methods and Stats.
// Every method takes the caller's contracts.Scope and refuses to run without a
// valid one. A search names the index generations to look in (the active
// generations of the sources the caller is authorised for) and the embedding
// model of its query vector; it reads nothing else. Scope is applied to what is
// READ, before any similarity is computed: Flat reads one contiguous primary-key
// range (application, tenant, workspace, generation), so there is no global
// candidate set that could be post-filtered, and a foreign vector never enters
// the candidates (SEC-021, ADR 0011 R1).
//
// Flat is exact: it scores every vector of the requested generations. Vectors
// are stored as int8 (one byte per dimension, a per-vector scale, cosine
// similarity); recall@10 against a float64 brute force is measured in
// docs/efficiency/M2-07.md. A resident cache (Config.CacheMiB) keeps hot
// generations in memory; it is a latency dial, never a source of truth: an entry
// is usable only when its revision equals the generation's, read in the same
// snapshot as the scan. It is never slower than no cache: a generation that
// needs room is loaded only when every entry it would evict has not served a
// search since the generation last asked, otherwise it is streamed.
//
// A generation is fixed to the source, embedding model and dimension of its
// first batch. A new embedder or chunker version is a new generation, built
// beside the old one and switched by the index module (internal/rag/index owns
// the lifecycle); the vector index only stores under the ID it is given.
//
// # Embedding reuse and SEC-023
//
// Unchanged chunks reuse their embeddings (12.3). The reuse key is
// NewInputKey: SHA-256 over the application, tenant and workspace, the
// embedding model and the EXACT text embedded (EmbeddingText: heading path,
// table header and text), each field length-prefixed. It is not
// chunk.Chunk.ContentHash, which covers Text alone and carries no scope:
// reusing by it would let one tenant learn that another holds identical content
// (an existence oracle by timing, by reuse counts or by equal ids), and would
// ignore a header or path the embedding input added. With scope in the key,
// identical content in another scope has an unrelated key, finds nothing,
// is embedded afresh and shares no row.
//
// Reuse happens INSIDE the index (Reuse copies a stored vector to the new
// generation; it never leaves the provider) and only finds rows of the same
// scope and model, under the same chunk id, with an equal key. The keys live in
// the vector rows, so a deleted chunk leaves no reusable embedding behind:
// vectors are as retained and as deletable as the text they came from.
//
// # The embedding job
//
// Pipeline.Embed reuses what the scope holds, embeds the rest through the
// providers.Embedder role in bounded batches, and stores. It runs as a queued
// job (EmbedJobType, jobs.ClassIngest, priority class 5) whose payload carries
// ids only; a ChunkSource supplied by ingestion reads the text back inside the
// job's own scope. It never holds a database transaction across a model call.
// Every step is idempotent: a crash, a retry and an at-least-once redelivery
// converge, because the second run finds the first run's vectors by reuse.
//
// # Ceilings
//
// Upsert refuses with resource_exhausted past Config.MaxVectorsPerWorkspace and
// MaxVectorsPerDeployment (vectors held across every generation, so a rebuild
// beside the active generation has room). The supported ACTIVE ceiling is half
// of each default (100,000 vectors per workspace, 250,000 per deployment) with
// the default cache, and 25,000 per workspace with the cache off; ADR 0011
// states it with the evidence.
//
// # Deletion
//
// DeleteChunks, DeleteGeneration, DeleteSource and DeleteScope remove vectors.
// Deleting a generation hides it first (one short transaction), then removes its
// rows in bounded batches so the store's single writer is never held for
// seconds; an interrupted delete is finished by repeating the call or by
// Flat.ResumeDrops. Deleted rows leave free pages in the database file until
// reuse or vacuum (the store does not set secure_delete); see ADR 0011.
//
// # Testing a provider
//
// package vectortest holds RunProviderSuite, the contract every implementation
// passes. internal/rag/vector/sqlitevec is the second implementation, the
// sqlite-vec candidate of ADR 0011.
package vector
