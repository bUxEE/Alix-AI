package source

import (
	"context"
	"io"

	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Provider is the contract of a document source: the only way the RAG
// pipeline touches an original. A provider is stateless and safe for
// concurrent use; the Definition it receives on every call is the whole
// description of the source, and the verified scope is checked against it
// (Definition.CheckScope) before anything else happens.
//
// Rules every implementation keeps:
//
//   - It reads only inside the declared root and only what the include and
//     exclude rules, the size limits and the accepted formats admit.
//   - An unreachable root is contracts.ErrUnavailable (and Health
//     unavailable), never an empty result, a "not found" or a checked state.
//   - An item is "gone" (ErrNotFound, a Deleted tombstone) only when the root
//     was reached and the item's place was inspected. A failure to read is a
//     Failure, not a deletion.
//   - Errors are typed (contracts.Error) and carry no host path.
type Provider interface {
	// Kinds lists the source types the provider serves.
	Kinds() []ap.RAGSourceType

	// Probe reports whether the root can be reached now. Its error is for a
	// bad scope or definition only; an unreachable root is a Health with
	// Availability unavailable.
	Probe(ctx context.Context, scope contracts.Scope, def Definition) (Health, error)

	// Discover scans the source and compares it with prev, the live records
	// the caller remembers from the last completed scan. It returns what was
	// added, modified and deleted (as tombstones), what was skipped or failed,
	// the counts, and the next snapshot (see ChangeSet). Reaching a declared
	// limit (max_files, max_total_bytes) is contracts.ErrResourceExhausted.
	Discover(ctx context.Context, scope contracts.Scope, def Definition, prev []Record) (ChangeSet, error)

	// DetectDeletions is the cheap half of Discover for event-driven syncs: it
	// inspects only the keys in prev and returns tombstones for those that
	// left the source or are no longer admitted. It does not walk the tree and
	// reads no content. Only Deleted, Failed, Counts and Next of the result are set.
	DetectDeletions(ctx context.Context, scope contracts.Scope, def Definition, prev []Record) (ChangeSet, error)

	// Stat returns the metadata of one item without reading it, or
	// ErrNotFound, ErrEscapesRoot, or contracts.ErrUnavailable. It is how a
	// citation is checked for staleness.
	Stat(ctx context.Context, scope contracts.Scope, def Definition, key string) (ItemInfo, error)

	// FetchVersion reads one item and returns its version (content hash, size,
	// time, sniffed type). It fails for an item the rules do not admit.
	FetchVersion(ctx context.Context, scope contracts.Scope, def Definition, key string) (Version, error)

	// Open returns the content of one item for the parser. The reader refuses
	// to deliver more than limits.max_file_bytes. The caller closes it.
	Open(ctx context.Context, scope contracts.Scope, def Definition, key string) (Content, error)
}

// Content is an open item.
type Content struct {
	Info ItemInfo
	// MIME is the sniffed type.
	MIME string
	io.ReadCloser
}
