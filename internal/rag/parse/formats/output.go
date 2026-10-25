// Package formats holds the document parsers of the sandboxed worker: txt, md,
// html, csv, pdf and docx, all pure Go. Each turns untrusted bytes into
// extracted text plus structure (headings, paragraphs, code, tables, pages)
// that maps onto chunk.Document, and flags hidden text.
//
// The package performs no I/O: it takes bytes and limits and returns an
// Output or an *Error. That is what lets the worker's import graph be checked
// for network and process-spawning packages, and lets the parsers be fuzzed
// in-process. Containment (a separate process, memory and time limits) is the
// launcher's job, not this package's; the limits here are the logical ones
// (output size, decompression budget, nesting, entry counts) that keep one
// document from using unbounded memory or time before the OS limits trip.
package formats

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime/debug"

	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

// AlgorithmVersion is the version of the parsing algorithms. Bump it when a
// change alters the text, blocks or flags produced for the same input; a
// changed parser version creates a new index generation (master plan 12.3).
const AlgorithmVersion = "1.2.0"

// ComponentName is the name the parser records in an index generation.
const ComponentName = "parser"

// Component is the parser identity and version recorded with every output:
// the algorithm version plus a fingerprint of everything outside this package
// that shapes the output (the Markdown scanner of the chunker, the hidden-text
// thresholds).
func Component() wire.Component {
	sum := sha256.Sum256([]byte(fmt.Sprintf("parser/v1 chunk=%s tiny=%v contrast=%v offscreen=%v", chunk.AlgorithmVersion, tinyFontPoints, minContrast, offscreenPx)))
	return wire.Component{Name: ComponentName, Version: AlgorithmVersion + "+" + hex.EncodeToString(sum[:4])}
}

// Output is a successful parse.
type Output struct {
	Text       []byte
	PageStarts []int
	Blocks     []wire.Block
	Warnings   []string
	// NeedsOCR lists one-based pages with images and no text.
	NeedsOCR []int
	// Expanded is the number of bytes inflated while parsing.
	Expanded int64
	// Pages is the page count, 0 when the format has none.
	Pages int
}

func (b *Builder) finish(w *warnings) *Output {
	o := &Output{Text: b.text, Blocks: b.blocks, Warnings: w.list}
	if b.paged {
		o.PageStarts = b.pages
		o.Pages = len(b.pages)
	}
	return o
}

// Parse parses data as the given format under lim. It never panics: a
// parser bug that panics becomes an internal-failure *Error, so one hostile
// document cannot take a long-lived caller down (the worker is disposable
// anyway; this is for in-process users such as fuzzers).
func Parse(format wire.Format, data []byte, lim wire.Limits) (out *Output, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fail(wire.ReasonInternal, "parser panic: %v (%s)", r, firstFrame(debug.Stack()))
		}
	}()
	if int64(len(data)) > lim.MaxInputBytes {
		return nil, fail(wire.ReasonInputTooLarge, "document is %d bytes, over the limit of %d", len(data), lim.MaxInputBytes)
	}
	if err := sniff(format, data); err != nil {
		return nil, err
	}
	switch format {
	case wire.FormatTXT:
		return parseText(data, false, lim)
	case wire.FormatMD:
		return parseText(data, true, lim)
	case wire.FormatHTML:
		return parseHTML(data, lim)
	case wire.FormatCSV:
		return parseCSV(data, lim)
	case wire.FormatPDF:
		return parsePDF(data, lim)
	case wire.FormatDOCX:
		return parseDOCX(data, lim)
	}
	return nil, fail(wire.ReasonUnsupported, "format %q is not supported", format)
}

func firstFrame(stack []byte) string {
	// The stack has goroutine header lines; keep it short and free of data.
	if len(stack) > 160 {
		stack = stack[:160]
	}
	return string(stack)
}

// sniff checks that the bytes look like the format the caller declared. A
// caller picks the format from the file name or declared MIME type, both of
// which an attacker controls, so the worker refuses a mismatch instead of
// handing, say, a zip to the HTML parser.
func sniff(format wire.Format, data []byte) error {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	switch format {
	case wire.FormatPDF:
		if !containsBytes(head, "%PDF-") {
			return fail(wire.ReasonFormatMismatch, "not a PDF: no %%PDF- header")
		}
	case wire.FormatDOCX:
		if len(data) < 4 || data[0] != 'P' || data[1] != 'K' {
			return fail(wire.ReasonFormatMismatch, "not a DOCX: not a zip container")
		}
	default:
		if len(data) >= 4 && (string(data[:4]) == "PK\x03\x04" || string(data[:4]) == "%PDF") {
			return fail(wire.ReasonFormatMismatch, "a %s document that is a binary container", format)
		}
	}
	return nil
}

func containsBytes(b []byte, s string) bool {
	n := len(s)
	for i := 0; i+n <= len(b); i++ {
		if string(b[i:i+n]) == s {
			return true
		}
	}
	return false
}
