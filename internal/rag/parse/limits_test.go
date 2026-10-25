package parse

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/rag/parse/parsetest"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// wantRefused asserts err is a deterministic refusal of the document: the
// validation_failed code (not retryable) and a reason, optionally one of several.
func wantRefused(t testing.TB, err error, reasons ...string) *contracts.Error {
	t.Helper()
	var ce *contracts.Error
	if err == nil || !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a refusal of the document", err)
	}
	if ce.Code != contracts.ErrValidationFailed || ce.Retryable {
		t.Fatalf("err = %v: code %s (retryable %v), want validation_failed, not retryable", err, ce.Code, ce.Retryable)
	}
	got, _ := ce.Details["reason"].(string)
	for _, r := range reasons {
		if got == r {
			return ce
		}
	}
	t.Fatalf("reason = %q, want one of %v (%v)", got, reasons, err)
	return nil
}

// stillWorks proves the Runner is unharmed: a normal document parses.
func stillWorks(t testing.TB, r *Runner) {
	t.Helper()
	res, err := r.Parse(ctx(t), wire.FormatMD, []byte("# Fine\n\nStill parsing after the hostile document.\n"))
	if err != nil {
		t.Fatalf("the runner is not usable after the hostile document: %v", err)
	}
	if !strings.Contains(res.Text, "Still parsing") {
		t.Fatalf("text = %q", res.Text)
	}
}

// peakOf is the worker's peak memory recorded on a refusal.
func peakOf(ce *contracts.Error) int64 {
	n, _ := ce.Details["peak_memory_bytes"].(int64)
	return n
}

// SEC-013: oversized documents (huge file, huge extracted text, huge single
// line) exhaust memory, disk or index size.
func TestOversizeRejected(t *testing.T) {
	t.Run("input over the limit never starts a process", func(t *testing.T) {
		r := newTestRunner(t, func(c *Config) { c.MaxInputBytes = 1 << 20 })
		_, err := r.Parse(ctx(t), wire.FormatTXT, bytes.Repeat([]byte("a "), 1<<20)) // 2 MiB
		wantRefused(t, err, wire.ReasonInputTooLarge)
		if n := r.Spawned(); n != 0 {
			t.Errorf("%d worker(s) were started for a document refused up front", n)
		}
		stillWorks(t, r)
	})

	t.Run("a format that is not supported is refused up front", func(t *testing.T) {
		r := newTestRunner(t)
		_, err := r.Parse(ctx(t), wire.Format("exe"), []byte("MZ"))
		wantRefused(t, err, wire.ReasonUnsupported)
		if r.Spawned() != 0 {
			t.Error("a worker was started for an unsupported format")
		}
	})

	// Output over the cap is rejected, never truncated: a half-parsed document
	// must not look like a whole one.
	const outCap = 64 << 10
	words := strings.Repeat("lorem ipsum dolor sit amet ", 12000) // ~320 KiB
	var csvBig strings.Builder
	for i := 0; i < 6000; i++ {
		fmt.Fprintf(&csvBig, "%d,name %d,some longer text in this cell\n", i, i)
	}
	cases := []struct {
		name   string
		format wire.Format
		doc    []byte
	}{
		{"text", wire.FormatTXT, []byte(words)},
		{"one huge line", wire.FormatTXT, []byte(strings.Repeat("x", 1<<20))},
		{"markdown", wire.FormatMD, []byte("# T\n\n" + words)},
		{"html", wire.FormatHTML, []byte("<p>" + words + "</p>")},
		{"csv", wire.FormatCSV, []byte(csvBig.String())},
		{"docx", wire.FormatDOCX, parsetest.DOCX(t, parsetest.Para(words), nil)},
		{"pdf", wire.FormatPDF, parsetest.SimplePDF(pdfLines(3000))},
	}
	for _, c := range cases {
		t.Run("output over the cap: "+c.name, func(t *testing.T) {
			r := newTestRunner(t, func(cfg *Config) { cfg.MaxOutputBytes = outCap })
			_, err := r.Parse(ctx(t), c.format, c.doc)
			ce := wantRefused(t, err, wire.ReasonOutputTooLarge)
			if p := peakOf(ce); p > 256<<20 {
				t.Errorf("the worker's peak memory was %d bytes for a document refused on size", p)
			}
			stillWorks(t, r)
		})
	}

	t.Run("too many blocks", func(t *testing.T) {
		r := newTestRunner(t, func(c *Config) { c.MaxBlocks = 500 })
		_, err := r.Parse(ctx(t), wire.FormatCSV, []byte(csvBig.String()))
		wantRefused(t, err, wire.ReasonOutputTooLarge)
		stillWorks(t, r)
	})

	t.Run("within the cap is not refused", func(t *testing.T) {
		r := newTestRunner(t, func(cfg *Config) { cfg.MaxOutputBytes = outCap })
		res := mustParse(t, r, wire.FormatTXT, []byte(strings.Repeat("small ", 100)))
		if res.Stats.OutputBytes == 0 || res.Stats.OutputBytes > outCap {
			t.Errorf("output bytes = %d", res.Stats.OutputBytes)
		}
	})
}

func pdfLines(n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "BT /F1 8 Tf 20 %d Td (line number %d of a very long page of text) Tj ET\n", 780-(i%150)*5, i)
	}
	return sb.String()
}

// SEC-014: archive bombs (high-ratio zip, nested archives, entry-count floods,
// docx container bombs) exhaust resources during ingestion. Each fixture is
// built by streaming here (nothing hostile is committed), is parsed by a real
// sandboxed worker, and must be refused without the worker exceeding its memory
// and time limits; only that one document fails.
func TestZipBombRejected(t *testing.T) {
	const memCap = 512 << 20
	newRunner := func(t *testing.T) *Runner {
		return newTestRunner(t, func(c *Config) {
			c.MaxExpandedBytes = 64 << 20
			c.MaxExpansionRatio = 100
			c.MaxEntries = 2000
			c.MemoryBytes = memCap
			c.WallTimeout = 30 * time.Second
		})
	}
	valid := parsetest.DOCX(t, parsetest.Para("an ordinary document"), nil)

	// Parts that compress about 8:1 and produce no text: they inflate a lot but
	// stay under the ratio and under the output cap, so only the total budget can
	// stop them.
	bookmarks := func(open, close string, size int) []byte {
		var b bytes.Buffer
		b.WriteString(`<?xml version="1.0"?>` + open)
		for i := 0; b.Len() < size; i++ {
			fmt.Fprintf(&b, `<w:bookmarkStart w:id="%d" w:name="bookmark_%d_%d"/>`, i, i, i*7919)
		}
		b.WriteString(close)
		return b.Bytes()
	}
	docOpen, docClose := `<w:document `+parsetest.DocxNS+`><w:body>`, `</w:body></w:document>`
	fnOpen, fnClose := `<w:footnotes `+parsetest.DocxNS+`><w:footnote w:id="1">`, `</w:footnote></w:footnotes>`
	enOpen, enClose := `<w:endnotes `+parsetest.DocxNS+`><w:endnote w:id="1">`, `</w:endnote></w:endnotes>`

	// Valid XML that inflates 1000:1: about 100 MiB from about 100 KiB.
	bomb := parsetest.RepeatedXML(`<w:bookmarkStart w:id="1" w:name="x"/>`, 2<<20)
	small := uint32(100)
	zero := uint32(0)

	entry := func(name string, data []byte) parsetest.RawEntry { return parsetest.RawEntry{Name: name, Data: data} }
	var flood []parsetest.RawEntry
	flood = append(flood, entry("word/document.xml", []byte("<a/>")))
	for i := 0; i < 3000; i++ {
		flood = append(flood, parsetest.RawEntry{Name: fmt.Sprintf("m/%d", i), Data: []byte("x"), Stored: true})
	}

	cases := []struct {
		name    string
		doc     []byte
		reasons []string
	}{
		{"compression ratio", parsetest.RawZip([]parsetest.RawEntry{{Name: "word/document.xml", Data: bomb}}),
			[]string{wire.ReasonArchiveBomb}},
		{"total inflated size", parsetest.RawZip([]parsetest.RawEntry{
			entry("word/document.xml", bookmarks(docOpen, docClose, 25<<20)),
			entry("word/footnotes.xml", bookmarks(fnOpen, fnClose, 25<<20)),
			entry("word/endnotes.xml", bookmarks(enOpen, enClose, 25<<20)),
		}), []string{wire.ReasonArchiveBomb}},
		{"entry count flood", parsetest.RawZip(flood), []string{wire.ReasonArchiveBomb}},
		{"nested archive", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", valid)}), []string{wire.ReasonArchiveBomb}},
		{"nested archive three deep", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", parsetest.RawZip([]parsetest.RawEntry{entry("a.zip", valid)}))}), []string{wire.ReasonArchiveBomb}},
		{"entry smaller header than its data", parsetest.RawZip([]parsetest.RawEntry{{Name: "word/document.xml", Data: bytes.Repeat([]byte("<a/>"), 1<<20), DeclaredUSize: &small}}),
			[]string{wire.ReasonMalformed, wire.ReasonArchiveBomb}},
		{"entry with a wrong checksum", parsetest.RawZip([]parsetest.RawEntry{{Name: "word/document.xml", Data: []byte("<w:document " + parsetest.DocxNS + "><w:body/></w:document>"), DeclaredCRC: &zero}}),
			[]string{wire.ReasonMalformed}},
		{"overlapping entries", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", []byte("<a/>")), {Name: "word/styles.xml", ShareDataWith: 1}}),
			[]string{wire.ReasonArchiveBomb}},
		{"entry name with ..", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", []byte("<a/>")), entry("../../evil.xml", []byte("x"))}), []string{wire.ReasonMalformed}},
		{"absolute entry name", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", []byte("<a/>")), entry("/etc/passwd", []byte("x"))}), []string{wire.ReasonMalformed}},
		{"entry name with a backslash", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", []byte("<a/>")), entry(`..\..\evil`, []byte("x"))}), []string{wire.ReasonMalformed}},
		{"entry name with a drive", parsetest.RawZip([]parsetest.RawEntry{entry("word/document.xml", []byte("<a/>")), entry(`C:\evil`, []byte("x"))}), []string{wire.ReasonMalformed}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRunner(t)
			start := time.Now()
			_, err := r.Parse(ctx(t), wire.FormatDOCX, c.doc)
			ce := wantRefused(t, err, c.reasons...)
			if el := time.Since(start); el > 25*time.Second {
				t.Errorf("took %s to refuse", el)
			}
			if p := peakOf(ce); p > memCap/2 {
				t.Errorf("the worker's peak memory was %d bytes (limit %d) while refusing the bomb", p, memCap)
			}
			stillWorks(t, r)
		})
	}

	t.Run("pdf stream bomb", func(t *testing.T) {
		r := newRunner(t)
		d := &parsetest.PDFDoc{}
		font := d.Add("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
		big := d.Flate("", bytes.Repeat([]byte("q Q "), 40<<20)) // 160 MiB of operators
		pg := d.Add(fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", d.Len()+2, big, font))
		pn := d.Add(fmt.Sprintf("<< /Type /Pages /Kids [%d 0 R] /Count 1 >>", pg))
		root := d.Add(fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pn))
		_, err := r.Parse(ctx(t), wire.FormatPDF, d.Bytes(root))
		wantRefused(t, err, wire.ReasonArchiveBomb)
		stillWorks(t, r)
	})

	t.Run("a legitimate document is not refused", func(t *testing.T) {
		r := newRunner(t)
		res := mustParse(t, r, wire.FormatDOCX, valid)
		if !strings.Contains(res.Text, "an ordinary document") {
			t.Errorf("text = %q", res.Text)
		}
	})
}
