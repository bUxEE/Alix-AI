package chunk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ---------------------------------------------------------------- schemas

const schemasDir = "../../../schemas"

var (
	compilerOnce sync.Once
	compiler     *jsonschema.Compiler
	compilerErr  error
)

// schemaFor compiles a schema reference such as
// urn:alixai:schema:conversation:citation:v1#/$defs/locator.
func schemaFor(t *testing.T, ref string) *jsonschema.Schema {
	t.Helper()
	compilerOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		c.AssertFormat()
		compilerErr = filepath.WalkDir(schemasDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "examples" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".schema.json") {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				return err
			}
			return c.AddResource(doc.(map[string]any)["$id"].(string), doc)
		})
		compiler = c
	})
	if compilerErr != nil {
		t.Fatalf("loading schemas: %v", compilerErr)
	}
	s, err := compiler.Compile(ref)
	if err != nil {
		t.Fatalf("compile %s: %v", ref, err)
	}
	return s
}

func validateJSON(t *testing.T, ref string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := schemaFor(t, ref).Validate(inst); err != nil {
		t.Errorf("%s rejected %s: %v", ref, raw, err)
	}
}

// ---------------------------------------------------------------- builders

func mustChunker(t *testing.T, cfg Config) *Chunker {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return c
}

func mustChunk(t *testing.T, c *Chunker, doc Document) Result {
	t.Helper()
	res, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	checkInvariants(t, doc, c.Config(), res)
	return res
}

// noMerge is a config whose children are exactly the blocks, so a test can
// reason about them one by one.
func noMerge() Config { return Config{ChildMinToks: 0, ChildMaxToks: 256, ParentMaxToks: 2048} }

var identifierRe = regexp.MustCompile(`^[^\x00-\x20\x7f-\xa0*]{1,128}$`)

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// expectedChildText is the text the children of a document must cover: every
// block except headings (which are parents' own text) and table header rows.
// Documents given to checkInvariants have a body under every heading.
func expectedChildText(doc Document) string {
	var sb strings.Builder
	for _, b := range doc.Blocks {
		switch b.Kind {
		case BlockParagraph, BlockCode:
			sb.WriteString(doc.Text[b.Start:b.End])
		case BlockTable:
			rows := b.Rows
			if b.HasHeader && len(rows) > 1 {
				rows = rows[1:]
			}
			for _, r := range rows {
				sb.WriteString(doc.Text[r.Start:r.End])
			}
		}
	}
	return stripSpace(sb.String())
}

// checkInvariants asserts what must hold for every result, whatever the
// document: it is the contract the rest of the pipeline relies on.
func checkInvariants(t *testing.T, doc Document, cfg Config, res Result) {
	t.Helper()
	runes := []rune(doc.Text)
	parents := map[string]Chunk{}
	ids := map[string]bool{}

	check := func(c Chunk, role string) {
		t.Helper()
		if c.ID == "" || !identifierRe.MatchString(c.ID) {
			t.Errorf("%s %q is not a valid identifier", role, c.ID)
		}
		if ids[c.ID] {
			t.Errorf("%s ID %q is not unique", role, c.ID)
		}
		ids[c.ID] = true
		if got := doc.Text[c.ByteSpan.Start:c.ByteSpan.End]; got != c.Text {
			t.Errorf("%s %s: text is not the source at its byte span: %q vs %q", role, c.ID, c.Text, got)
		}
		if got := string(runes[c.Span.Start:c.Span.End]); got != c.Text {
			t.Errorf("%s %s: text is not the source at its code point span: %q vs %q", role, c.ID, c.Text, got)
		}
		if n := utf8.RuneCountInString(c.Text); n != c.Span.End-c.Span.Start || n == 0 || n > MaxChunkChars {
			t.Errorf("%s %s: %d code points for span %+v (cap %d)", role, c.ID, n, c.Span, MaxChunkChars)
		}
		if c.EstToks != EstimateTokens(c.Text) {
			t.Errorf("%s %s: EstToks %d, want %d", role, c.ID, c.EstToks, EstimateTokens(c.Text))
		}
		sum := sha256.Sum256([]byte(c.Text))
		if c.ContentHash != hex.EncodeToString(sum[:]) {
			t.Errorf("%s %s: wrong content hash", role, c.ID)
		}
		if c.Text != strings.TrimSpace(c.Text) {
			t.Errorf("%s %s: text is not trimmed: %q", role, c.ID, c.Text)
		}
		if c.Page != doc.pageAt(c.ByteSpan.Start) || c.PageEnd != doc.pageAt(c.ByteSpan.End-1) {
			t.Errorf("%s %s: pages %d-%d do not match the page starts", role, c.ID, c.Page, c.PageEnd)
		}
		if len(c.Path) > 0 && c.Section != c.Path[len(c.Path)-1] || len(c.Path) == 0 && c.Section != "" {
			t.Errorf("%s %s: section %q does not end path %q", role, c.ID, c.Section, c.Path)
		}
		validateJSON(t, "urn:alixai:schema:conversation:citation:v1#/$defs/locator", c.Locator())
	}

	prevEnd := -1
	for i, p := range res.Parents {
		check(p, "parent")
		if p.Kind != KindSection || p.ParentID != "" || p.Ordinal != i {
			t.Errorf("parent %d: kind %q parent %q ordinal %d", i, p.Kind, p.ParentID, p.Ordinal)
		}
		if p.EstToks > cfg.ParentMaxToks {
			t.Errorf("parent %s: %d est tokens, bound %d", p.ID, p.EstToks, cfg.ParentMaxToks)
		}
		if p.ByteSpan.Start < prevEnd {
			t.Errorf("parent %s overlaps the previous parent", p.ID)
		}
		prevEnd = p.ByteSpan.End
		if len(p.ChildIDs) == 0 {
			t.Errorf("parent %s has no children", p.ID)
		}
		parents[p.ID] = p
	}

	prevEnd = -1
	byParent := map[string][]string{}
	var covered strings.Builder
	for _, c := range res.Children {
		check(c, "child")
		if c.Kind == KindSection {
			t.Errorf("child %s has the parent kind", c.ID)
		}
		p, ok := parents[c.ParentID]
		if !ok {
			t.Errorf("child %s: unknown parent %q", c.ID, c.ParentID)
			continue
		}
		if c.ByteSpan.Start < p.ByteSpan.Start || c.ByteSpan.End > p.ByteSpan.End {
			t.Errorf("child %s lies outside its parent %s", c.ID, p.ID)
		}
		if c.ByteSpan.Start < prevEnd {
			t.Errorf("child %s overlaps the previous child", c.ID)
		}
		prevEnd = c.ByteSpan.End
		if c.EstToks > cfg.ChildMaxToks {
			t.Errorf("child %s: %d est tokens, bound %d", c.ID, c.EstToks, cfg.ChildMaxToks)
		}
		if c.Section != p.Section || strings.Join(c.Path, "\x00") != strings.Join(p.Path, "\x00") {
			t.Errorf("child %s and its parent disagree on section", c.ID)
		}
		if c.Hidden && !p.Hidden {
			t.Errorf("child %s is hidden but its parent is not flagged", c.ID)
		}
		if c.Ordinal != len(byParent[c.ParentID]) {
			t.Errorf("child %s: ordinal %d, want %d", c.ID, c.Ordinal, len(byParent[c.ParentID]))
		}
		byParent[c.ParentID] = append(byParent[c.ParentID], c.ID)
		covered.WriteString(c.Text)
	}
	for _, p := range res.Parents {
		if strings.Join(p.ChildIDs, ",") != strings.Join(byParent[p.ID], ",") {
			t.Errorf("parent %s: ChildIDs %v do not match its children %v", p.ID, p.ChildIDs, byParent[p.ID])
		}
	}

	if got, want := stripSpace(covered.String()), expectedChildText(doc); got != want {
		t.Errorf("children do not cover the body text exactly\n got: %.200q\nwant: %.200q", got, want)
	}
	validateJSON(t, "urn:alixai:schema:rag:shared:v1#/$defs/componentVersion", res.Chunker)
}

// childTexts lists the texts of res.Children.
func childTexts(res Result) []string {
	out := make([]string, len(res.Children))
	for i, c := range res.Children {
		out[i] = c.Text
	}
	return out
}

// idsByText maps child text (unique in the documents that use it) to its ID.
func idsByText(res Result) map[string]string {
	out := map[string]string{}
	for _, c := range res.Children {
		out[c.Text] = c.ID
	}
	return out
}
