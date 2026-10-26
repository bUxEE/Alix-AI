package exports

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestGoldenFormats(t *testing.T) {
	blocks := []IdentifiedBlock{
		{ID: "act_trn_x_b1", Index: 1, Block: mustBlock(t, "table.json")},
		{ID: "act_trn_x_b2", Index: 2, Block: mustBlock(t, "chart-bar.json")},
		{ID: "act_trn_x_b3", Index: 3, Block: conversation.RichBlock{Value: conversation.MarkdownBlock{
			Type: "markdown", Text: "Hello <script>alert(1)</script> & friends.",
		}}},
	}
	cfg := DefaultConfig()
	bom := false
	cfg.CSVExcelBOM = &bom

	csv, err := renderCSV(datasetsFromBlocks(blocks), false)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "table-chart.csv", csv)

	xlsx, err := renderXLSX(datasetsFromBlocks(blocks[:2]))
	if err != nil {
		t.Fatal(err)
	}
	if len(xlsx) < 100 || xlsx[0] != 'P' || xlsx[1] != 'K' {
		t.Fatalf("xlsx is not a zip: %d bytes", len(xlsx))
	}
	// Deterministic: same input twice.
	again, err := renderXLSX(datasetsFromBlocks(blocks[:2]))
	if err != nil || !bytes.Equal(xlsx, again) {
		t.Fatalf("xlsx not deterministic: %v equal=%v", err, bytes.Equal(xlsx, again))
	}

	html, err := renderHTML(blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "report.html", html)
	lower := strings.ToLower(string(html))
	if strings.Contains(lower, "<script") {
		t.Fatal("html contains script")
	}
	if strings.Contains(lower, `href="http`) || strings.Contains(lower, `src="http`) ||
		strings.Contains(lower, "url(http") {
		t.Fatal("html contains external URL")
	}
	if !strings.Contains(string(html), "Content-Security-Policy") {
		t.Fatal("html missing CSP")
	}
	if strings.Contains(string(html), "<script>alert(1)</script>") {
		t.Fatal("xss canary survived")
	}

	md, err := renderMarkdown(blocks)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "report.md", md)
}

func TestFormulaInjection(t *testing.T) {
	set := Dataset{
		Name:    "evil",
		Headers: []string{"=CMD"},
		Rows:    [][]string{{"=1+1", "+2", "-3", "@SUM(A1)", "ok"}},
	}
	csv, err := renderCSV([]Dataset{set}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\n=1+1", "\n+2", "\n-3", "\n@SUM"} {
		if bytes.Contains(csv, []byte(bad)) {
			t.Fatalf("csv still has formula starter %q in %q", bad, csv)
		}
	}
	if !bytes.Contains(csv, []byte("'=1+1")) || !bytes.Contains(csv, []byte("'=CMD")) {
		t.Fatalf("csv missing guarded cells: %s", csv)
	}
	xlsx, err := renderXLSX([]Dataset{set})
	if err != nil {
		t.Fatal(err)
	}
	ss := zipFile(t, xlsx, "xl/sharedStrings.xml")
	for _, want := range []string{"&#39;=1+1", "&#39;+2", "&#39;-3", "&#39;@SUM(A1)", "&#39;=CMD"} {
		if !bytes.Contains(ss, []byte(want)) {
			t.Fatalf("xlsx sharedStrings missing %q in %s", want, ss)
		}
	}
}

func zipFile(t *testing.T, zipped []byte, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatalf("zip missing %s", name)
	return nil
}

func TestFormatApplicability(t *testing.T) {
	mdOnly := []IdentifiedBlock{{ID: "b1", Index: 1, Block: conversation.RichBlock{Value: conversation.MarkdownBlock{Text: "hi"}}}}
	if sets := datasetsFromBlocks(mdOnly); len(sets) != 0 {
		t.Fatalf("markdown should not yield datasets: %d", len(sets))
	}
}

func mustBlock(t *testing.T, name string) conversation.RichBlock {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "conversation", "examples", "rich-block", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	var b conversation.RichBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if err != nil {
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("wrote %s", path)
			return
		}
		t.Fatalf("missing golden %s (set UPDATE_GOLDEN=1 to write)", path)
	}
	if !bytes.Equal(want, got) {
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Logf("updated %s", path)
			return
		}
		t.Fatalf("%s mismatch\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}
