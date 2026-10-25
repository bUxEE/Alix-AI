package source

import (
	"sort"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// HashPrefix starts every content hash a provider reports.
const HashPrefix = "sha256:"

// Version is one observed state of an item. Hash is the content address
// ("sha256:" and 64 lower-case hex digits) and is what a citation carries as
// the version; Size and ModTime are the cheap fingerprint a later scan compares
// before it reads the file again.
type Version struct {
	Hash    string
	Size    int64
	ModTime time.Time
	// MIME is the sniffed type (see Classify), never the one the file name
	// suggests.
	MIME string
}

// Record is what the caller remembers of one live item between scans. A
// provider's Discover takes the previous records and returns the next ones in
// ChangeSet.Next; the caller persists them. Store ModTime and SeenAt at full
// (nanosecond) precision: a coarser copy only costs an extra hash, but a
// finer one that is rounded on one side never matches.
type Record struct {
	Key     string
	Size    int64
	ModTime time.Time
	Hash    string
	MIME    string
	// SeenAt is when the scan that produced the record ran. A record whose
	// ModTime is within the provider's racy window of SeenAt is not trusted
	// for the size-and-time shortcut, because a second write in the same
	// timestamp tick would leave both unchanged.
	SeenAt time.Time
}

// Version returns the record's version.
func (r Record) Version() Version {
	return Version{Hash: r.Hash, Size: r.Size, ModTime: r.ModTime, MIME: r.MIME}
}

// RecordOf builds the record for a version seen at the given time.
func RecordOf(key string, v Version, seen time.Time) Record {
	return Record{Key: key, Size: v.Size, ModTime: v.ModTime, Hash: v.Hash, MIME: v.MIME, SeenAt: seen}
}

// ItemInfo is the cheap metadata of one item, without its content.
type ItemInfo struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Item is an item and its current version.
type Item struct {
	Key     string
	Version Version
}

// Reason says why an item was skipped, or why it was tombstoned.
type Reason string

const (
	// ReasonDeleted: the item is gone from a reachable root. Only a provider
	// that reached the root and listed its directory may say it.
	ReasonDeleted Reason = "deleted"
	// ReasonExcluded: the item is still there but the rules (or a changed
	// declaration) no longer admit it.
	ReasonExcluded Reason = "excluded"
	// ReasonSourceRemoved: the source declaration was removed or disabled.
	ReasonSourceRemoved Reason = "source_removed"

	// The skip reasons. An item skipped for one of them is counted in
	// Counts.Skipped; an item that was indexed before and is now skipped is
	// tombstoned with the same reason, because it must leave retrieval.

	// ReasonTooLarge: larger than limits.max_file_bytes.
	ReasonTooLarge Reason = "too_large"
	// ReasonFormat: the sniffed type is not in the declared formats.
	ReasonFormat Reason = "format_not_accepted"
	// ReasonMIMEMismatch: the file name claims a type the bytes contradict.
	ReasonMIMEMismatch Reason = "mime_mismatch"
	// ReasonSymlink: a symbolic link; links are never followed.
	ReasonSymlink Reason = "symlink"
	// ReasonLinkEscape: a symbolic link that leads outside the root (or cannot
	// be resolved).
	ReasonLinkEscape Reason = "symlink_escape"
	// ReasonIrregular: a junction, other reparse point, device, pipe or socket.
	ReasonIrregular Reason = "irregular_file"
	// ReasonUnsafeName: a name that cannot be an item key on this host.
	ReasonUnsafeName Reason = "unsafe_name"
)

// Skip is an item that was found and not accepted.
type Skip struct {
	Key    string
	Reason Reason
}

// Failure is an item or directory that could not be read. It is never a
// deletion: what a Failure hides keeps its previous record and gets no
// tombstone.
type Failure struct {
	// Key is the item, or the directory whose listing failed.
	Key string
	Err *contracts.Error
}

// Tombstone says an item left the source and must leave retrieval at once.
// Its derived data (chunks, embeddings, cached answers) is purged at PurgeAfter
// (zero: not purged until an operator does so).
type Tombstone struct {
	Key    string
	Reason Reason
	// Last is the last version that was live.
	Last       Version
	At         time.Time
	PurgeAfter time.Time
}

// Counts are the figures of a scan, shaped like the counts of the source
// status (schema rag/source-status). Indexed is not here: the pipeline that
// parses and indexes decides it.
type Counts struct {
	// Discovered counts the items found inside the root that the include and
	// exclude rules admit, whether or not they were then accepted.
	Discovered int
	// Skipped counts the discovered items not accepted (size, format, link,
	// name).
	Skipped int
	// Failed counts items and directories that could not be read.
	Failed int
	// Tombstoned counts the tombstones this scan produced.
	Tombstoned int
	// Unchanged counts accepted items whose version did not change.
	Unchanged int
	// Hashed counts the files whose content was read to compute a hash. It is
	// zero for a scan of an untouched tree.
	Hashed int
	// Bytes is the total size of the accepted items.
	Bytes int64
}

// ChangeSet is the result of one scan.
//
// On a non-nil error from the provider the ChangeSet carries diagnostics only
// (Counts, and Skipped and Failed gathered so far): Added, Modified, Deleted
// and Next are empty, so nothing can be applied from a scan that did not
// complete.
type ChangeSet struct {
	// Added are items with no previous record.
	Added []Item
	// Modified are items whose content hash differs from the previous record.
	Modified []Item
	// Deleted are the tombstones for previous items that left the source.
	Deleted []Tombstone
	// Skipped and Failed are the items not accepted or not readable.
	Skipped []Skip
	Failed  []Failure
	Counts  Counts
	// Next is the complete set of live records after this scan, sorted by
	// key: unchanged items (with refreshed metadata), added and modified
	// items, and items that failed to read and keep their previous record.
	Next []Record
}

// Changed reports whether anything needs indexing or purging.
func (c ChangeSet) Changed() bool {
	return len(c.Added) > 0 || len(c.Modified) > 0 || len(c.Deleted) > 0
}

// Availability is whether a source can be reached now. It is the citation's
// availability type, so a status built from a Health needs no mapping.
type Availability = conversation.CitationAvailability

// The two availabilities a provider reports. (The third, removed, is decided
// by the registry when a source is deleted.)
const (
	Available   = conversation.CitationAvailabilityAvailable
	Unavailable = conversation.CitationAvailabilityUnavailable
)

// Health is the answer to Provider.Probe.
//
// A source is only ever reported as checked when it was reached. For an
// unreachable root Availability is unavailable, CheckedAt is the zero time and
// Err explains why; a caller must not advance a status's last_success_at from
// it, and must treat the content it indexed earlier as staleness unknown.
type Health struct {
	Availability Availability
	// CheckedAt is when the root was last verified reachable by this probe;
	// zero when it was not.
	CheckedAt time.Time
	// Err is set exactly when Availability is not available.
	Err *contracts.Error
}

// Reachable reports whether the probe reached the root.
func (h Health) Reachable() bool {
	return h.Availability == Available && !h.CheckedAt.IsZero() && h.Err == nil
}

// TombstonesForRemoval returns the tombstones for every live record of a
// source whose declaration was removed or disabled (master plan 19: deleting a
// source produces deletion tombstones). With on_source_removed "purge" the
// derived data is purged after the declaration's purge window; with "disable"
// it stays inert and PurgeAfter is the zero time, so nothing is purged until
// an operator says so. The result is sorted by key.
func TombstonesForRemoval(def Definition, prev []Record, now time.Time) []Tombstone {
	out := make([]Tombstone, 0, len(prev))
	for _, r := range prev {
		t := Tombstone{Key: r.Key, Reason: ReasonSourceRemoved, Last: r.Version(), At: now}
		if def.Deletion.OnSourceRemoved == ap.RAGSourceDeletionOnSourceRemovedPurge {
			t.PurgeAfter = now.Add(def.Deletion.PurgeAfter)
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// TombstoneFor builds the tombstone of one item that left the source at now,
// applying the declaration's purge window.
func TombstoneFor(def Definition, last Record, reason Reason, now time.Time) Tombstone {
	return Tombstone{
		Key:        last.Key,
		Reason:     reason,
		Last:       last.Version(),
		At:         now,
		PurgeAfter: now.Add(def.Deletion.PurgeAfter),
	}
}
