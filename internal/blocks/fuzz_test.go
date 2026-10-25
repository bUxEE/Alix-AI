package blocks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// FuzzSanitizeMarkdown: whatever text goes in, the sanitiser does not panic, and
// what comes out (when it is accepted) is inert when rendered with raw HTML
// switched on, and is its own sanitised form.
func FuzzSanitizeMarkdown(f *testing.F) {
	for _, seed := range []string{
		"hello **world**",
		"<script>alert(1)</script>",
		"[x](javascript:alert(1))",
		"![i](https://a.test/i.png)",
		"&lt;b&gt; &#60;i&#62; &amp;lt;",
		"- a\n- b\n\n1. c\n2) d",
		"> > > quote\n\n---\n\n# h\n\n    code\n\n```go\nx\n```",
		"<a href=\"javascript:x\">y</a>",
		"[a]: javascript:alert(1)\n\n[a]",
		"line one  \nline two\\\nline three",
		"`` ` `` and ``` `` ```",
		"<https://example.test> <mailto:a@b.test> <javascript:x>",
		"\\<script\\>",
		"*a **b *c* d** e*",
		"1. \n2. \n\n- \n",
	} {
		f.Add(seed)
	}
	v := New(Options{LinkSchemes: []string{"https", "mailto"}})
	f.Fuzz(func(t *testing.T, in string) {
		out, _, err := v.SanitizeMarkdown(in)
		if err != nil {
			return // refused: fail closed is fine
		}
		if !utf8.ValidString(out) {
			t.Fatalf("invalid UTF-8 output for %q", in)
		}
		var buf bytes.Buffer
		if err := goldmark.New(goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe())).Convert([]byte(out), &buf); err != nil {
			t.Fatalf("render: %v", err)
		}
		allowed := map[string]bool{"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
			"em": true, "strong": true, "code": true, "pre": true, "ul": true, "ol": true, "li": true,
			"blockquote": true, "hr": true, "br": true, "a": true}
		z := xhtml.NewTokenizer(&buf)
		for {
			tt := z.Next()
			if tt == xhtml.ErrorToken {
				break
			}
			if tt == xhtml.CommentToken || tt == xhtml.DoctypeToken {
				t.Fatalf("comment or doctype from %q -> %q", in, out)
			}
			if tt == xhtml.StartTagToken || tt == xhtml.SelfClosingTagToken || tt == xhtml.EndTagToken {
				tok := z.Token()
				if !allowed[tok.Data] {
					t.Fatalf("element <%s> from %q -> %q", tok.Data, in, out)
				}
				for _, a := range tok.Attr {
					switch {
					case tok.Data == "a" && a.Key == "href":
						low := strings.ToLower(a.Val)
						if !strings.HasPrefix(low, "https://") && !strings.HasPrefix(low, "mailto:") {
							t.Fatalf("link %q from %q -> %q", a.Val, in, out)
						}
					case tok.Data == "code" && a.Key == "class" && strings.HasPrefix(a.Val, "language-"):
					case tok.Data == "ol" && a.Key == "start":
					default:
						t.Fatalf("attribute %s on <%s> from %q -> %q", a.Key, tok.Data, in, out)
					}
				}
			}
		}
		again, _, err := v.SanitizeMarkdown(out)
		if err != nil {
			t.Fatalf("output refused when sanitised again: %q -> %q: %v", in, out, err)
		}
		if again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", in, out, again)
		}
	})
}

// FuzzBlockJSON: a block document never panics the validator; one that is
// accepted is accepted again, unchanged, in the form the validator returned.
func FuzzBlockJSON(f *testing.F) {
	for _, dir := range []string{"valid", "invalid"} {
		entries, err := os.ReadDir(filepath.Join(blocksSchemasRoot, "conversation", "examples", "rich-block", dir))
		if err != nil {
			f.Fatal(err)
		}
		for _, e := range entries {
			raw, err := os.ReadFile(filepath.Join(blocksSchemasRoot, "conversation", "examples", "rich-block", dir, e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(raw)
		}
	}
	v := Default()
	f.Fuzz(func(t *testing.T, doc []byte) {
		// With no facts a chart that carries a number is refused; a fact set
		// that holds every small integer lets the rest of the chart be checked.
		var facts []actions.Fact
		for i := int64(0); i < 40; i++ {
			facts = append(facts, blocksIntegerFact("n", i))
		}
		ctx := Context{Facts: NewFactSet(facts)}
		b, err := v.JSON(doc, ctx)
		if err != nil {
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid && contracts.CodeOf(err) != contracts.ErrInternal {
				t.Fatalf("an untyped refusal: %v", err)
			}
			return
		}
		enc, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("an accepted block does not encode: %v", err)
		}
		again, err := v.JSON(enc, ctx)
		if err != nil {
			t.Fatalf("an accepted block is refused the second time: %v\n%s", err, enc)
		}
		enc2, _ := json.Marshal(again)
		if !bytes.Equal(enc, enc2) {
			t.Fatalf("not stable:\n%s\n%s", enc, enc2)
		}
	})
}
