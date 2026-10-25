package parse

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Config holds the limits of the parsing sandbox. Every field has a default
// (DefaultConfig) sized for the Lite profile, and Validate refuses anything the
// sandbox cannot honour; nothing is silently clamped.
type Config struct {
	// WorkerPath is the absolute path of the alixai-parser executable. A
	// relative path or a bare name is refused: the worker is never looked up on
	// PATH, because what runs against untrusted documents must be what the
	// operator installed.
	WorkerPath string `json:"worker_path" yaml:"worker_path"`

	// MaxInputBytes is the largest document accepted. Larger ones are rejected
	// before a process is started.
	MaxInputBytes int64 `json:"max_input_bytes" yaml:"max_input_bytes"`
	// MaxOutputBytes caps the extracted text; a document that would produce
	// more is rejected, not truncated. At most chunk.MaxDocumentBytes.
	MaxOutputBytes int64 `json:"max_output_bytes" yaml:"max_output_bytes"`
	// MaxExpandedBytes caps everything decompressed from one document.
	MaxExpandedBytes int64 `json:"max_expanded_bytes" yaml:"max_expanded_bytes"`
	// MaxExpansionRatio caps inflated bytes per compressed byte (archive
	// bombs); applied once an entry or stream is over 1 MiB.
	MaxExpansionRatio int `json:"max_expansion_ratio" yaml:"max_expansion_ratio"`
	// MaxEntries caps archive entries.
	MaxEntries int `json:"max_entries" yaml:"max_entries"`
	// MaxDepth caps element, object and form nesting.
	MaxDepth int `json:"max_depth" yaml:"max_depth"`
	// MaxPages caps PDF pages.
	MaxPages int `json:"max_pages" yaml:"max_pages"`
	// MaxBlocks caps blocks plus table rows in one result.
	MaxBlocks int `json:"max_blocks" yaml:"max_blocks"`

	// WallTimeout is the wall-clock time one document may take, from the moment
	// the sandbox is verified to the last byte of the result. The process is
	// killed when it passes.
	WallTimeout time.Duration `json:"wall_timeout" yaml:"wall_timeout"`
	// StartupTimeout is how long the worker may take to start and report its
	// confinement. It is separate from WallTimeout so that a slow start (an
	// antivirus scanning a fresh executable, a cold disk) is never mistaken for
	// a hung parser, and a hung parser never borrows the startup allowance.
	StartupTimeout time.Duration `json:"startup_timeout" yaml:"startup_timeout"`
	// MemoryBytes is the memory budget of the worker process.
	MemoryBytes int64 `json:"memory_bytes" yaml:"memory_bytes"`
	// CPUTime is the total CPU time the worker may use; 0 means WallTimeout.
	CPUTime time.Duration `json:"cpu_time" yaml:"cpu_time"`
	// MaxConcurrent is how many workers may run at once. Parsing is background
	// work (class ingest); one at a time is the Lite default.
	MaxConcurrent int `json:"max_concurrent" yaml:"max_concurrent"`

	// OCR is disabled. It is a hook (the OCR interface) and a switch that
	// defaults to off; no OCR engine ships in this version.
	OCR OCRConfig `json:"ocr" yaml:"ocr"`
}

// OCRConfig selects the optional OCR hook. OCR is heavy (an engine and a
// model), so it stays off (master plan 7.5: specialist workers load only for
// bounded jobs). The hook is the NeedsOCR list in a Result, which names the
// pages that are only an image; with OCR off a document with no text layer
// fails as needs_ocr instead of indexing nothing. Turning the switch on is
// refused by Validate until an engine exists.
type OCRConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
}

// DefaultConfig is the Lite-profile default. WorkerPath is empty: the
// composition root fills it (DefaultWorkerPath finds the worker installed next
// to the service binary).
func DefaultConfig() Config {
	return Config{
		MaxInputBytes:     64 << 20,
		MaxOutputBytes:    16 << 20,
		MaxExpandedBytes:  128 << 20,
		MaxExpansionRatio: 100,
		MaxEntries:        2000,
		MaxDepth:          100,
		MaxPages:          5000,
		MaxBlocks:         400000,
		WallTimeout:       60 * time.Second,
		StartupTimeout:    30 * time.Second,
		MemoryBytes:       512 << 20,
		MaxConcurrent:     1,
	}
}

func bad(format string, args ...any) error {
	return contracts.Newf(contracts.ErrValidationFailed, "parse config: "+format, args...)
}

// maxTextBytes mirrors chunk.MaxDocumentBytes: the chunker refuses more.
const maxTextBytes = 64 << 20

// Validate refuses limits the sandbox cannot honour, with
// contracts.ErrValidationFailed.
func (c Config) Validate() error {
	switch {
	case c.WorkerPath == "":
		return bad("worker_path is empty")
	case !filepath.IsAbs(c.WorkerPath):
		return bad("worker_path %q is not absolute", c.WorkerPath)
	case c.MaxInputBytes < 1 || c.MaxInputBytes > 1<<31:
		return bad("max_input_bytes %d is outside 1 to 2 GiB", c.MaxInputBytes)
	case c.MaxOutputBytes < 1 || c.MaxOutputBytes > maxTextBytes:
		return bad("max_output_bytes %d is outside 1 to %d (the chunker's limit)", c.MaxOutputBytes, maxTextBytes)
	case c.MaxExpandedBytes < 1:
		return bad("max_expanded_bytes %d is not positive", c.MaxExpandedBytes)
	case c.MaxExpansionRatio < 1:
		return bad("max_expansion_ratio %d is not positive", c.MaxExpansionRatio)
	case c.MaxEntries < 1 || c.MaxEntries > 65534:
		return bad("max_entries %d is outside 1 to 65534", c.MaxEntries)
	case c.MaxDepth < 8 || c.MaxDepth > 1000:
		return bad("max_depth %d is outside 8 to 1000", c.MaxDepth)
	case c.MaxPages < 1 || c.MaxPages > 1<<20:
		return bad("max_pages %d is outside 1 to %d", c.MaxPages, 1<<20)
	case c.MaxBlocks < 1 || c.MaxBlocks > 2_000_000:
		return bad("max_blocks %d is outside 1 to 2000000", c.MaxBlocks)
	case c.WallTimeout < time.Second:
		return bad("wall_timeout %s is under one second", c.WallTimeout)
	case c.StartupTimeout < time.Second:
		return bad("startup_timeout %s is under one second", c.StartupTimeout)
	case c.MemoryBytes < 64<<20:
		return bad("memory_bytes %d is under 64 MiB: a Go process commits about 47 MiB at start on Windows, and a small document needs more", c.MemoryBytes)
	case c.CPUTime < 0:
		return bad("cpu_time %s is negative", c.CPUTime)
	case c.MaxConcurrent < 1 || c.MaxConcurrent > 64:
		return bad("max_concurrent %d is outside 1 to 64", c.MaxConcurrent)
	case c.OCR.Enabled:
		// Fail closed: a switch that did nothing when turned on would index a
		// scanned document as empty while the operator believes it was read.
		return bad("ocr.enabled is true but no OCR engine ships in this version; an OCR engine is native code and an optional adapter that runs only at sandbox level L2 or in a container (ADR 0014)")
	}
	return nil
}

func (c Config) wireLimits() wire.Limits {
	return wire.Limits{
		MaxInputBytes:     c.MaxInputBytes,
		MaxOutputBytes:    c.MaxOutputBytes,
		MaxExpandedBytes:  c.MaxExpandedBytes,
		MaxExpansionRatio: c.MaxExpansionRatio,
		MaxEntries:        c.MaxEntries,
		MaxDepth:          c.MaxDepth,
		MaxPages:          c.MaxPages,
		MaxBlocks:         c.MaxBlocks,
	}
}

func (c Config) cpuTime() time.Duration {
	if c.CPUTime > 0 {
		return c.CPUTime
	}
	return c.WallTimeout
}

// String names the limits that matter in a log line, without the path.
func (c Config) String() string {
	return fmt.Sprintf("parse{in=%d out=%d mem=%d wall=%s}", c.MaxInputBytes, c.MaxOutputBytes, c.MemoryBytes, c.WallTimeout)
}
