package parse

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Sandbox levels, recorded with every result and shown by the readiness check
// so an operator can see when confinement is below what the platform can do.
const (
	// LevelL1 is process isolation with mandatory resource limits: a separate
	// disposable process, wall-time kill, memory and CPU limits, no inherited
	// secrets, no network code. It is the floor: below it nothing parses.
	LevelL1 = "L1"
	// LevelL2 adds OS confinement of the worker itself: on Linux no filesystem
	// access (Landlock), no sockets and no program execution (seccomp).
	LevelL2 = "L2"
)

// SandboxReport says how the worker that produced a result was confined.
type SandboxReport struct {
	Level    string   `json:"level"`
	Platform string   `json:"platform"`
	Host     []string `json:"host"`
	Worker   []string `json:"worker"`
	// Missing are best-effort mechanisms that could not be installed, each as
	// "name: reason".
	Missing []string `json:"missing,omitempty"`
}

// Stats are measurements of one parse, for the efficiency review and audit.
type Stats struct {
	wire.Stats
	// Spawn is the time from starting the process to a verified sandbox report.
	Spawn time.Duration `json:"spawn_ns"`
	// Total is the time from the call to the result.
	Total time.Duration `json:"total_ns"`
	// PeakMemoryBytes is the worker's peak memory as the host measured it:
	// committed bytes (Job Object) on Windows, resident set (rusage) elsewhere.
	PeakMemoryBytes int64 `json:"peak_memory_bytes"`
	// PeakKind says what PeakMemoryBytes counts: "commit" or "rss".
	PeakKind string `json:"peak_kind"`
	// PeakWorkingSetBytes is the peak resident set where it is known separately
	// (Windows), else equal to PeakMemoryBytes.
	PeakWorkingSetBytes int64 `json:"peak_working_set_bytes"`
}

// Result is a validated parse of one document. The text and structure came from
// the sandboxed worker, and the host has checked them as it would any untrusted
// input: spans inside the text, on code point boundaries, UTF-8 throughout.
type Result struct {
	// Parser identifies the parser and its version, recorded with every output
	// (and in the index generation: a changed parser creates a new one).
	Parser rag.ComponentVersion
	Format wire.Format
	Text   string
	// PageStarts is the byte offset at which each page begins; empty when the
	// format has no pages.
	PageStarts []int
	Blocks     []wire.Block
	// Warnings are stable codes for things handled but worth knowing (a charset
	// fallback, a repaired cross-reference table).
	Warnings []string
	// NeedsOCR lists one-based pages that are only an image. OCR is off by
	// default; when it is off and no page has text the parse is an error
	// instead (see Runner.Parse).
	NeedsOCR []int
	Sandbox  SandboxReport
	Stats    Stats
}

// HiddenBlocks is the number of blocks the parser flagged as hidden text.
func (r *Result) HiddenBlocks() int {
	n := 0
	for _, b := range r.Blocks {
		if b.Hidden {
			n++
		}
	}
	return n
}

// Document maps the result onto the chunker's input. Offsets are the worker's
// UTF-8 byte offsets, page starts carry over, and a block the parser flagged
// hidden keeps its Hidden flag, which the chunker carries to every chunk cut
// from it (SEC-012). Hidden text is always its own block, never a gap between
// blocks, which is the contract chunk.Document states: a parent chunk's text
// is the verbatim slice from its first child to its last, so unflagged hidden
// text in a gap would reach a model unflagged inside a parent.
func (r *Result) Document(sourceID string) chunk.Document {
	doc := chunk.Document{SourceID: sourceID, Text: r.Text, PageStarts: r.PageStarts}
	doc.Blocks = make([]chunk.Block, len(r.Blocks))
	for i, b := range r.Blocks {
		cb := chunk.Block{
			Kind:      chunk.BlockKind(b.Kind),
			Level:     b.Level,
			Title:     b.Title,
			Start:     b.Start,
			End:       b.End,
			HasHeader: b.HasHeader,
			Hidden:    b.Hidden,
		}
		if len(b.Rows) > 0 {
			cb.Rows = make([]chunk.Row, len(b.Rows))
			for j, row := range b.Rows {
				cb.Rows[j] = chunk.Row{Start: row.Start, End: row.End}
			}
		}
		doc.Blocks[i] = cb
	}
	return doc
}

// Component is the parser identity and version this build of AlixAI expects of
// a worker, in the form an index generation records.
func Component() rag.ComponentVersion {
	c := formats.Component()
	return rag.ComponentVersion{Name: c.Name, Version: actions.OpaqueVersion(c.Version)}
}

// newResult validates the worker's output and builds the Result. The worker is
// untrusted: its result is checked against its text before anything uses it.
func newResult(res wire.Result, text []byte, lim wire.Limits) (*Result, error) {
	if res.Protocol != wire.ProtocolVersion {
		return nil, protocolError("result has protocol %d", res.Protocol)
	}
	if res.Component != formats.Component() {
		return nil, protocolError("result is from parser %s %s, expected %s", res.Component.Name, res.Component.Version, formats.Component().Version)
	}
	if err := res.Validate(text, lim.MaxBlocks); err != nil {
		return nil, protocolError("invalid result: %v", err)
	}
	for _, p := range res.NeedsOCR {
		if p < 1 || p > len(res.PageStarts) {
			return nil, protocolError("needs_ocr page %d is out of range", p)
		}
	}
	return &Result{
		Parser:     Component(),
		Format:     res.Format,
		Text:       string(text),
		PageStarts: res.PageStarts,
		Blocks:     res.Blocks,
		Warnings:   res.Warnings,
		NeedsOCR:   res.NeedsOCR,
		Stats:      Stats{Stats: res.Stats},
	}, nil
}

func protocolError(format string, args ...any) error {
	return contracts.New(contracts.ErrInternal, "parser worker protocol error: "+fmt.Sprintf(format, args...)).WithDetails("reason", "protocol")
}
