// Package wire is the protocol between the AlixAI core and the sandboxed
// parser worker (cmd/alixai-parser), and the types both sides share. It has no
// dependency beyond the standard library, so the worker can import it without
// importing the launcher (os/exec, Job Objects), which is what lets
// TestParserHasNoNetwork prove the worker's import graph.
//
// # Conversation
//
//	host   -> worker Init      the process limits, sent after the host has confined the process
//	worker -> host   Hello     who the worker is and what confinement it installed on itself
//	host   -> worker Request   the format, the limits, the body size (only after the Hello is verified)
//	host   -> worker Body      the document bytes
//	worker -> host   Result    JSON header: blocks, pages, warnings, stats
//	worker -> host   Text      the extracted text
//	  or
//	worker -> host   Error     a typed failure
//
// The worker reads nothing it did not ask for before Init, and nothing from a
// document before it has confined itself and the host has checked the Hello.
// That order is what makes the Hello trustworthy: no untrusted byte has been
// parsed yet. A worker may serve several Request/Body pairs on one Init.
//
// Every frame is a type byte, a big-endian uint32 length and the payload. The
// host treats every byte from the worker as untrusted: lengths are checked
// before allocation, the result is re-validated, and a worker that sends
// anything out of order is killed.
package wire

// ProtocolVersion changes when the conversation or a frame's shape changes.
const ProtocolVersion = 1

// Format is a document format the worker can parse.
type Format string

// The supported formats.
const (
	FormatTXT  Format = "txt"
	FormatMD   Format = "md"
	FormatHTML Format = "html"
	FormatCSV  Format = "csv"
	FormatPDF  Format = "pdf"
	FormatDOCX Format = "docx"
)

// Formats lists every supported format.
func Formats() []Format {
	return []Format{FormatTXT, FormatMD, FormatHTML, FormatCSV, FormatPDF, FormatDOCX}
}

// Valid reports whether f is a supported format.
func (f Format) Valid() bool {
	for _, g := range Formats() {
		if f == g {
			return true
		}
	}
	return false
}

// Component names a pipeline component and its version. It mirrors
// rag.ComponentVersion without importing the generated contracts.
type Component struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Sandbox is what the worker reports about its own confinement, before it has
// seen any document. It is an attestation, not a guarantee: the host
// cross-checks everything it can verify (the Job Object it created, the
// limits it passed) and treats the rest as the worker's word, which is
// trustworthy at this point only because no untrusted input has been read.
type Sandbox struct {
	// Applied lists the mechanisms the worker installed on itself, by stable
	// name (for example "rlimit_cpu", "no_new_privs", "landlock_fs",
	// "seccomp_net", "job_object").
	Applied []string `json:"applied"`
	// Missing lists best-effort mechanisms that could not be installed, each
	// as "name: reason". A missing best-effort layer lowers the sandbox level
	// reported to the operator but does not stop parsing.
	Missing []string `json:"missing,omitempty"`
	// Observed are limits the worker read back from the OS, by name (bytes,
	// milliseconds or 0/1 flags). The host compares them with what it asked for.
	Observed map[string]int64 `json:"observed,omitempty"`
}

// Init is the host's first frame. It carries the process-level limits the
// worker applies to itself (the host and the OS enforce the same limits from
// outside, so the worker's own application is defence in depth, and the
// attestation in the Hello lets the host verify what the OS actually applied).
type Init struct {
	Protocol int `json:"protocol"`
	// MemoryBytes is the memory budget of the process; 0 means the host has not
	// set one (never true for a production worker).
	MemoryBytes int64 `json:"memory_bytes"`
	// CPUMillis is the CPU time the process may use in total.
	CPUMillis int64 `json:"cpu_millis"`
	// WallMillis is the wall-clock time one request may take; the worker exits
	// on its own a little after it, in case the host cannot kill it.
	WallMillis int64 `json:"wall_millis"`
	// SelfGuard makes the worker watch its own memory and exit when it passes
	// MemoryBytes. It is the only memory enforcement where the OS has none
	// (macOS); elsewhere it is defence in depth.
	SelfGuard bool `json:"self_guard"`
	// Threads is GOMAXPROCS for the worker. Parsing is sequential, so 1 is the
	// default and keeps the process small.
	Threads int `json:"threads"`
}

// Hello is the worker's reply to Init.
type Hello struct {
	Protocol  int       `json:"protocol"`
	Component Component `json:"component"`
	GoOS      string    `json:"goos"`
	GoArch    string    `json:"goarch"`
	Sandbox   Sandbox   `json:"sandbox"`
}

// Limits are the bounds the worker enforces inside its parsers. Process-level
// bounds (wall time, memory, CPU) are enforced by the host and the OS; they
// are passed to the worker as command-line flags so it can also guard itself.
type Limits struct {
	// MaxInputBytes is the largest document accepted (also the host's cap).
	MaxInputBytes int64 `json:"max_input_bytes"`
	// MaxOutputBytes caps the extracted text. A document that would produce
	// more is rejected, never truncated.
	MaxOutputBytes int64 `json:"max_output_bytes"`
	// MaxExpandedBytes caps everything decompressed from one document: archive
	// entries and PDF streams together. Counted as bytes actually inflated,
	// never from a size field in the file.
	MaxExpandedBytes int64 `json:"max_expanded_bytes"`
	// MaxExpansionRatio caps inflated bytes per compressed byte, for one
	// entry or stream once it is over a small floor.
	MaxExpansionRatio int `json:"max_expansion_ratio"`
	// MaxEntries caps archive entries (checked from the central directory
	// before anything is inflated).
	MaxEntries int `json:"max_entries"`
	// MaxDepth caps nesting: XML and HTML elements, PDF objects and form
	// XObjects. Archives are never nested into at all.
	MaxDepth int `json:"max_depth"`
	// MaxPages caps PDF pages.
	MaxPages int `json:"max_pages"`
	// MaxBlocks caps blocks plus table rows in one result, so the result
	// frame stays bounded whatever the text size.
	MaxBlocks int `json:"max_blocks"`
}

// Request is the host's second frame.
type Request struct {
	Protocol int    `json:"protocol"`
	Format   Format `json:"format"`
	// Size is the length of the Body frame that follows.
	Size   int64  `json:"size"`
	Limits Limits `json:"limits"`
}

// BlockKind mirrors chunk.BlockKind.
type BlockKind string

// The block kinds.
const (
	KindHeading   BlockKind = "heading"
	KindParagraph BlockKind = "paragraph"
	KindCode      BlockKind = "code"
	KindTable     BlockKind = "table"
)

// Span is a half-open byte range of the extracted text.
type Span struct {
	Start int `json:"s"`
	End   int `json:"e"`
}

// Block is one structural unit of the extracted text. Offsets are UTF-8 byte
// offsets into the Text frame, End exclusive.
type Block struct {
	Kind      BlockKind `json:"kind"`
	Level     int       `json:"level,omitempty"`
	Title     string    `json:"title,omitempty"`
	Start     int       `json:"start"`
	End       int       `json:"end"`
	Rows      []Span    `json:"rows,omitempty"`
	HasHeader bool      `json:"has_header,omitempty"`
	// Hidden marks text the parser found hidden from a human reader (SEC-012).
	// Hidden text is always its own block, or makes the whole block hidden when
	// it sits inside a heading, a code block or a table.
	Hidden bool `json:"hidden,omitempty"`
	// HiddenReason says why, for the audit trail: one of the Reason* values.
	HiddenReason string `json:"hidden_reason,omitempty"`
}

// The reasons a block is hidden.
const (
	HideDisplayNone     = "display_none"
	HideVisibility      = "visibility_hidden"
	HideHiddenAttr      = "hidden_attribute"
	HideAria            = "aria_hidden"
	HideZeroOpacity     = "zero_opacity"
	HideTinyFont        = "tiny_font"
	HideOffscreen       = "offscreen"
	HideClipped         = "clipped"
	HideSameColour      = "same_colour_as_background"
	HideTransparent     = "transparent_text"
	HideVanish          = "vanish"
	HideDeletedText     = "deleted_text"
	HideInvisibleRender = "invisible_render_mode"
	HideOptionalContent = "optional_content_off"
	HideComment         = "comment"
	HideRawHTML         = "hidden_html_in_markdown"
	HideInvisibleChars  = "invisible_unicode"
	HideOutsidePage     = "outside_page"
)

// Stats are measurements of one parse, for the efficiency review and audit.
type Stats struct {
	InputBytes    int64 `json:"input_bytes"`
	OutputBytes   int64 `json:"output_bytes"`
	ExpandedBytes int64 `json:"expanded_bytes"`
	Pages         int   `json:"pages,omitempty"`
	Blocks        int   `json:"blocks"`
	HiddenBlocks  int   `json:"hidden_blocks,omitempty"`
	// ParseNanos is the time the worker spent in the parser, excluding process
	// start and framing.
	ParseNanos int64 `json:"parse_ns"`
}

// Result is the worker's success frame (the text follows in a Text frame).
type Result struct {
	Protocol  int       `json:"protocol"`
	Component Component `json:"component"`
	Format    Format    `json:"format"`
	// TextSize is the length of the Text frame that follows.
	TextSize int64 `json:"text_size"`
	// PageStarts is the byte offset at which each page begins, strictly
	// ascending; empty when the format has no pages.
	PageStarts []int   `json:"page_starts,omitempty"`
	Blocks     []Block `json:"blocks"`
	// Warnings are stable codes for things that were handled but the operator
	// may want to know (a charset fallback, a repaired cross-reference table).
	Warnings []string `json:"warnings,omitempty"`
	// NeedsOCR lists one-based pages that carry images and no extractable
	// text. The worker never OCRs; the host decides.
	NeedsOCR []int `json:"needs_ocr,omitempty"`
	Stats    Stats `json:"stats"`
}

// The failure reasons a worker reports. Every one except ReasonInternal is a
// property of the document and its limits, so it is deterministic: retrying
// the same bytes gives the same answer.
const (
	ReasonMalformed      = "malformed"
	ReasonUnsupported    = "unsupported"
	ReasonFormatMismatch = "format_mismatch"
	ReasonEncrypted      = "encrypted"
	ReasonNeedsOCR       = "needs_ocr"
	ReasonArchiveBomb    = "archive_bomb"
	ReasonOutputTooLarge = "output_too_large"
	ReasonInputTooLarge  = "input_too_large"
	ReasonNestingTooDeep = "nesting_too_deep"
	ReasonTooManyPages   = "too_many_pages"
	ReasonBadRequest     = "bad_request"
	ReasonInternal       = "internal"
)

// Failure is the worker's error frame.
type Failure struct {
	Reason string `json:"reason"`
	// Message is non-sensitive and never quotes document content.
	Message string `json:"message"`
}
