package formats

import (
	"fmt"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// Error is a deterministic parse failure: the document, under the limits it
// was given, cannot be parsed. Reason is one of the wire.Reason* values.
type Error struct {
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

func fail(reason, format string, args ...any) *Error {
	return &Error{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func malformed(format string, args ...any) *Error {
	return fail(wire.ReasonMalformed, format, args...)
}

// budget counts everything a document inflates, across every archive entry and
// every PDF stream, against Limits.MaxExpandedBytes. The count is of bytes
// actually produced by the decompressor, never of a size field in the file.
type budget struct {
	limit int64
	ratio int
	used  int64
}

// ratioFloor is the inflated size under which the ratio limit is not applied:
// a few kilobytes of blank page or padded XML legitimately compress hundreds
// to one, and cannot hurt.
const ratioFloor = 1 << 20

func newBudget(l wire.Limits) *budget {
	return &budget{limit: l.MaxExpandedBytes, ratio: l.MaxExpansionRatio}
}

// take records n inflated bytes and fails once the total passes the limit.
func (b *budget) take(n int64) error {
	b.used += n
	if b.used > b.limit {
		return fail(wire.ReasonArchiveBomb, "decompressed data passes the limit of %d bytes", b.limit)
	}
	return nil
}

// room is how many more bytes may be inflated.
func (b *budget) room() int64 { return b.limit - b.used }

// checkRatio fails when inflated/compressed is over the limit and the inflated
// size is over the floor.
func (b *budget) checkRatio(inflated, compressed int64) error {
	if inflated <= ratioFloor || b.ratio <= 0 {
		return nil
	}
	if compressed < 1 {
		compressed = 1
	}
	if inflated/compressed > int64(b.ratio) {
		return fail(wire.ReasonArchiveBomb, "expansion ratio %d:1 is over the limit of %d:1", inflated/compressed, b.ratio)
	}
	return nil
}
