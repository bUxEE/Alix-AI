// Package source is the source-provider contract of the RAG pipeline (master
// plan 12.1, 12.2, 16.2, 19): the interface every document source implements,
// the definition a registered source is built from, and the change-set types
// a scan produces. It holds no I/O of its own. The folder and mounted-volume
// provider lives in internal/rag/providers/fs; the HTTP document provider in
// internal/rag/providers/httpapi.
//
// # What a provider is allowed to do
//
// A source is explicit. It has one declared root, include and exclude rules, a
// format list and size limits, all taken from the signed package declaration
// (FromPackage, FromDeclaration). A provider only lists and reads inside that
// root, every call carries a verified contracts.Scope that is checked against
// the definition's tenant and workspace binding, and a provider that cannot
// reach its root says so (Health, contracts.ErrUnavailable). It never reports
// an unreachable source as checked, and it never turns "I could not look" into
// "everything was deleted": an unreachable root, an unreadable directory or an
// I/O error yields no tombstones for the items it hides.
//
// # Items, versions and change sets
//
// An item is addressed by its Key: a relative, slash-separated path below the
// root (for an API source, the item's identifier path). A Version is the
// content address of what was read ("sha256:<hex>", which fits the schema's
// opaque version), its size, its modification time and its sniffed MIME type.
// A scan compares the live Records the caller remembers (Provider.Discover's
// prev argument) with what it finds and returns a ChangeSet: Added, Modified,
// Deleted (tombstones), the skipped and failed items with typed reasons, the
// counts the source status reports, and Next, the complete live snapshot the
// caller persists for the following scan. Change detection is by size and
// modification time first and a SHA-256 only when those differ, so an
// untouched tree costs one directory walk and no file reads.
//
// The provider is stateless. Whoever calls it (ingestion, the sync scheduler)
// owns the snapshot, the tombstone table and the purge window, and applies
// Definition.Deletion to the Tombstones it receives.
//
// # Shared helpers
//
// Matcher compiles the package's include and exclude globs (a segment glob
// with "**" across segments; exclusion always wins). Classify sniffs a MIME
// type from the first bytes of a file and refuses to trust the file name over
// the content. Paths holds the host path rules: normalisation of Windows
// verbatim (\\?\) and UNC forms, containment checks that respect Windows case
// folding, item-key validation and the reserved-name rules. They are pure
// string functions parameterised by flavour, so the Windows rules are tested
// on every platform.
package source
