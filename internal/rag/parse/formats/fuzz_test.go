package formats

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/parse/wire"
)

var fuzzFormats = []wire.Format{wire.FormatTXT, wire.FormatMD, wire.FormatHTML, wire.FormatCSV, wire.FormatPDF, wire.FormatDOCX}

// FuzzParse feeds arbitrary bytes to every parser (SEC-016: a malicious file
// must not crash, hang or blow up a parser). The first input byte-selects the
// format. A parse may refuse a document, but it must never panic (Parse turns a
// panic into an internal error, which is the failure here) and a success must be
// a valid result, the same check the host and the chunker apply. The limits are
// small so that a bomb is refused quickly and a run covers many inputs.
//
//	go test -run '^$' -fuzz FuzzParse -fuzztime 60s ./internal/rag/parse/formats/
//
// Under a plain `go test` it runs the seeds (the sample documents of the golden
// test) and any crashing input saved under testdata/fuzz.
func FuzzParse(f *testing.F) {
	seeds := []struct {
		format wire.Format
		file   string
	}{
		{wire.FormatTXT, "sample.txt"}, {wire.FormatTXT, "latin1.txt"}, {wire.FormatMD, "sample.md"},
		{wire.FormatHTML, "sample.html"}, {wire.FormatCSV, "sample.csv"}, {wire.FormatPDF, "sample.pdf"},
		{wire.FormatDOCX, "sample.docx"},
	}
	for _, s := range seeds {
		b, err := os.ReadFile(filepath.Join("..", "testdata", s.file))
		if err != nil {
			f.Fatal(err)
		}
		for i, ff := range fuzzFormats {
			if ff == s.format {
				f.Add(uint8(i), b)
			}
		}
	}
	lim := wire.Limits{
		MaxInputBytes:     1 << 20,
		MaxOutputBytes:    1 << 20,
		MaxExpandedBytes:  4 << 20,
		MaxExpansionRatio: 100,
		MaxEntries:        200,
		MaxDepth:          50,
		MaxPages:          50,
		MaxBlocks:         20000,
	}
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		format := fuzzFormats[int(which)%len(fuzzFormats)]
		out, err := Parse(format, data, lim)
		if err != nil {
			if fe, ok := err.(*Error); ok && fe.Reason == wire.ReasonInternal {
				t.Fatalf("%s: %v", format, err)
			}
			return
		}
		checkOutput(t, out)
		if int64(len(out.Text)) > lim.MaxOutputBytes {
			t.Fatalf("%s: %d bytes of text over the output limit %d", format, len(out.Text), lim.MaxOutputBytes)
		}
	})
}
