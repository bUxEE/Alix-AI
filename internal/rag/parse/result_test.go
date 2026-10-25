package parse

import (
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/formats"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

func TestNewResultNeedsOCRPageRange(t *testing.T) {
	// A worker may only name pages the result has: three pages, so 1 to 3.
	text := []byte("a\fb\fc")
	for _, c := range []struct {
		pages []int
		ok    bool
	}{
		{[]int{1, 3}, true},
		{[]int{0}, false},
		{[]int{4}, false},
		{[]int{1 << 20}, false},
	} {
		res := wire.Result{
			Protocol: wire.ProtocolVersion, Component: formats.Component(), Format: wire.FormatPDF,
			TextSize: int64(len(text)), PageStarts: []int{0, 2, 4}, NeedsOCR: c.pages,
		}
		_, err := newResult(res, text, wire.Limits{MaxBlocks: 10})
		if (err == nil) != c.ok {
			t.Errorf("needs_ocr %v on 3 pages: err = %v", c.pages, err)
		}
	}
}
