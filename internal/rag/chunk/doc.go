// Package chunk is the structure-aware parent/child chunker of the RAG
// ingestion pipeline (master plan 12.3). It is pure and deterministic: no
// model, no network, no filesystem, no clock, no randomness.
//
// # Shape of the output
//
// A section (a heading and the blocks under it, up to the next heading)
// becomes a parent chunk; its paragraphs, code blocks and table rows become
// child chunks. Children feed FTS and vector indexing, because they are small
// and precise. Parents are what a retrieval step may expand a child to, for
// context. Text before the first heading belongs to an implicit root parent
// that has no section title. A section too large for ParentMaxToks is cut
// into several parents between children, never inside one.
//
// A table is kept intact: every data row is exactly one child, rows are never
// merged with other content, and the rows of one table stay under one parent
// unless the table alone exceeds the parent bound, in which case parents break
// at row boundaries. The header row is not a child of its own; it travels with
// every row as Chunk.Header (outside the span-mapped text, so the span
// invariant below still holds).
//
// # The span invariant
//
// Every chunk's Text is a verbatim slice of the extracted text it was cut
// from (never normalised, never rewritten): Text is
// doc.Text[ByteSpan.Start:ByteSpan.End]. Citations need no more than that.
//
// # Offset unit (decision)
//
// Two spans are carried. ByteSpan is in UTF-8 bytes, for slicing in Go. Span is
// in Unicode code points (runes), and it is the one that goes on the wire:
// Chunk.Locator emits it as the citation span. The schema says only
// "character offsets"; code points are language-neutral (a Go byte offset or a
// JavaScript UTF-16 offset would each be wrong for the other). Whoever resolves
// a citation back to the source text must count code points.
//
// # Chunk IDs
//
// An ID is derived from content and structural position, never from an
// absolute offset, so unchanged content keeps its ID when text before it is
// edited and its spans shift. A child ID hashes (source ID, section path,
// occurrence of that path, child kind, content hash, occurrence of that
// content within the section); a parent ID hashes (source ID, section path,
// occurrence of that path, part number). Consequences, all by design:
//
//   - editing a paragraph changes that child's ID only (not its parent's);
//   - inserting or deleting other paragraphs, or other sections, changes no
//     ID of an unrelated chunk;
//   - renaming a heading re-IDs the children below it (their position
//     changed), but ContentHash stays equal, so an embedding keyed by content
//     hash is still reused;
//   - the same text in two different sources never shares an ID, because the
//     source ID is part of the hash.
//
// The chunker version is deliberately NOT part of an ID: a version bump that
// leaves a chunk's text and position alone keeps its ID and its embedding.
//
// ContentHash is the SHA-256 of the child text alone, with no scope in it. A
// caller that reuses embeddings by content hash must add the scope to its own
// key (SEC-023: reuse must never be a cross-tenant existence oracle).
//
// # Token estimation
//
// EstimateTokens is cheap (one pass, integer arithmetic) and errs high: it
// bounds the evidence package size for the Lite context budget, so it must
// never undercount. See estimate.go for the weights.
//
// # Versioning
//
// Chunker.Component names the chunker and its version; the version folds in a
// fingerprint of the bounds, because changing the bounds changes the chunks
// and a changed component creates a new index generation (12.3). Every Result
// records it.
package chunk
