package fuzzsec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	xhtml "golang.org/x/net/html"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ---------------------------------------------------------------- the detectors

// foldURL is how a URL is read for a scheme: HTML entities are already decoded
// by the tokenizer; a browser then drops leading C0 controls and spaces and
// removes every tab and line break; the check also case-folds and folds the
// full-width and the look-alike letters that a normalising consumer would
// (NFKC maps the full-width forms to ASCII, U+017F to "s", U+212A to "k").
func foldURL(s string) string {
	s = strings.TrimLeftFunc(s, func(r rune) bool { return r <= 0x20 })
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || unicode.Is(unicode.Cf, r):
			continue
		case r >= 0xFF01 && r <= 0xFF5E:
			r -= 0xFEE0
		case r == 0x017F:
			r = 's'
		case r == 0x212A:
			r = 'k'
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// activeScheme reports a URL that runs script or loads a document, however it was
// spelled.
func activeScheme(u string) string {
	f := foldURL(u)
	for _, s := range []string{"javascript:", "vbscript:", "livescript:", "data:", "file:", "blob:", "filesystem:"} {
		if strings.HasPrefix(f, s) {
			return s
		}
	}
	return ""
}

// urlAttrs are the attributes whose value a browser loads or runs.
var urlAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "formaction": true, "xlink:href": true, "data": true, "poster": true,
	"background": true, "srcdoc": true, "ping": true, "manifest": true, "codebase": true, "cite": true, "longdesc": true,
	"usemap": true, "profile": true, "lowsrc": true, "dynsrc": true, "srcset": true,
}

// forbiddenTags are the elements a Markdown rewrite must never leave behind.
var forbiddenTags = map[string]bool{
	"script": true, "iframe": true, "object": true, "embed": true, "style": true, "link": true, "meta": true, "base": true,
	"form": true, "input": true, "button": true, "textarea": true, "select": true, "svg": true, "math": true, "img": true,
	"video": true, "audio": true, "source": true, "frame": true, "frameset": true, "applet": true, "template": true, "noscript": true,
}

// allowedTags are the elements the sanitiser says it writes (and a CommonMark
// renderer produces from the canonical Markdown).
var allowedTags = map[string]bool{
	"p": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "em": true, "strong": true,
	"code": true, "pre": true, "ul": true, "ol": true, "li": true, "blockquote": true, "hr": true, "br": true, "a": true,
}

var (
	reStyleActive = regexp.MustCompile(`(?i)expression\s*\(|url\s*\(\s*['"]?\s*(?:javascript|vbscript|data)|-moz-binding|behaviou?r\s*:|@import`)
	reClassOK     = regexp.MustCompile(`^language-[a-z0-9_+#.-]{1,32}$`)
)

// dangerousHTML reads rendered HTML the way a browser does (tokenised, attribute
// values entity-decoded) and returns every active construct in it: an element
// that runs or loads something, an event-handler attribute, a style with an
// expression, a URL with an active scheme however it is spelled, a comment, and
// anything outside the allowlist of elements and attributes. Text nodes are
// inert and are not looked at: "&lt;script&gt;" and the words "javascript:" in
// prose are text.
func dangerousHTML(doc string) []string {
	var out []string
	z := xhtml.NewTokenizer(strings.NewReader(doc))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return out
		case xhtml.CommentToken, xhtml.DoctypeToken:
			out = append(out, "comment or doctype")
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken, xhtml.EndTagToken:
			tok := z.Token()
			name := strings.ToLower(tok.Data)
			switch {
			case forbiddenTags[name]:
				out = append(out, "element <"+name+">")
			case !allowedTags[name]:
				out = append(out, "element <"+name+"> outside the allowlist")
			}
			for _, a := range tok.Attr {
				key := strings.ToLower(a.Key)
				switch {
				case strings.HasPrefix(key, "on"):
					out = append(out, "event handler "+key)
				case key == "style":
					if reStyleActive.MatchString(foldURL(a.Val)) || reStyleActive.MatchString(a.Val) {
						out = append(out, "style with an expression")
					} else {
						out = append(out, "a style attribute")
					}
				case urlAttrs[key]:
					if s := activeScheme(a.Val); s != "" {
						out = append(out, key+" with the scheme "+s)
					}
					if key != "href" {
						out = append(out, "URL attribute "+key)
					}
				case name == "code" && key == "class" && reClassOK.MatchString(a.Val):
				case name == "ol" && key == "start":
				default:
					out = append(out, "attribute "+key+" on <"+name+">")
				}
			}
		}
	}
}

// parseMD parses canonical Markdown with the CommonMark parser (raw HTML is
// recognised, never honoured).
func parseMD(src string) ast.Node {
	return goldmark.New().Parser().Parse(text.NewReader([]byte(src)))
}

// destOf is a link destination as a renderer reads it: backslash escapes, then
// numeric and named character references.
func destOf(raw []byte) string {
	return string(util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(raw))))
}

// dangerousMarkdown reads sanitised Markdown as a parser does and returns the
// nodes that must not be in it: raw HTML, an image, and a link whose scheme is
// not one the validator was told to allow.
func dangerousMarkdown(src string, allowed map[string]bool) []string {
	var out []string
	root := parseMD(src)
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		check := func(dest string) {
			s := activeScheme(dest)
			f := foldURL(dest)
			scheme, _, _ := strings.Cut(f, ":")
			switch {
			case s != "":
				out = append(out, "a link with the scheme "+s)
			case !strings.Contains(f, ":") || !allowed[scheme]:
				out = append(out, "a link that is not on the allowed schemes: "+fmt.Sprintf("%.40q", dest))
			}
		}
		switch n := n.(type) {
		case *ast.RawHTML, *ast.HTMLBlock:
			out = append(out, "raw HTML")
		case *ast.Image:
			out = append(out, "an image")
		case *ast.Link:
			check(destOf(n.Destination))
		case *ast.AutoLink:
			u := string(n.URL([]byte(src)))
			if n.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(u), "mailto:") {
				u = "mailto:" + u
			}
			check(destOf([]byte(u)))
		}
		return ast.WalkContinue, nil
	})
	return out
}

// hiddenRune reports a character that changes what a reader sees without being
// seen: a control character (a tab and a line feed are layout), a zero-width,
// bidirectional or tag character, a line or paragraph separator.
func hiddenRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case unicode.IsControl(r), r == 0x2028, r == 0x2029, unicode.Is(unicode.Cf, r):
		return true
	}
	return false
}

var (
	cmUnsafe = goldmark.New(goldmark.WithRendererOptions(html.WithUnsafe()))
	cmSafe   = goldmark.New()
	gfmWide  = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(html.WithUnsafe()))
)

// checkSanitised is every property of one sanitised text, for the validator v
// that produced it from in. It returns the first violation, "" when none.
func checkSanitised(v *blocks.Validator, allowed map[string]bool, in, out string) string {
	if !utf8.ValidString(out) {
		return "the output is not valid UTF-8"
	}
	for _, r := range out {
		if hiddenRune(r) {
			return fmt.Sprintf("the output holds the hidden character U+%04X", r)
		}
	}
	if f := dangerousMarkdown(out, allowed); len(f) > 0 {
		return "the output, read as Markdown, holds " + strings.Join(f, "; ")
	}
	// Rendered by every reader a host might use, with raw HTML honoured (the
	// harshest setting): nothing active in the HTML.
	for name, md := range map[string]goldmark.Markdown{"CommonMark unsafe": cmUnsafe, "CommonMark safe": cmSafe, "GFM unsafe": gfmWide} {
		var buf bytes.Buffer
		if err := md.Convert([]byte(out), &buf); err != nil {
			return name + ": " + err.Error()
		}
		if f := dangerousHTML(buf.String()); len(f) > 0 {
			return name + " renders " + strings.Join(f, "; ") + ": " + buf.String()
		}
	}
	// Idempotent: the output is its own sanitised form.
	again, _, err := v.SanitizeMarkdown(out)
	if err != nil {
		return "the output is refused when sanitised again: " + err.Error()
	}
	if again != out {
		return fmt.Sprintf("not idempotent: %q -> %q", out, again)
	}
	return ""
}

// xssSeeds are the classic payloads, the encoded and the nested forms of them,
// and the places a Markdown rewrite has gone wrong before.
var xssSeeds = []string{
	"hello **world**",
	"<script>alert(1)</script>",
	"<SCRIPT SRC=//evil.test/x.js></SCRIPT>",
	"<scr<script>ipt>alert(1)</scr</script>ipt>",
	"<<script>alert(1)//<</script>",
	"\"><script>alert(1)</script>",
	"<img src=x onerror=alert(1)>",
	"<svg/onload=alert(1)>",
	"<body onload=alert(1)>",
	"<input onfocus=alert(1) autofocus>",
	"<details open ontoggle=alert(1)>x</details>",
	"<iframe src=\"javascript:alert(1)\"></iframe>",
	"<object data=\"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==\"></object>",
	"<embed src=javascript:alert(1)>",
	"<p style=\"width:expression(alert(1))\">x</p>",
	"<div style=\"background:url(javascript:alert(1))\">x</div>",
	"<math><mi xlink:href=\"javascript:alert(1)\">x</mi></math>",
	"<a href=\"javascript:alert(1)\">x</a>",
	"<a href=\"&#x6A&#x61&#x76&#x61&#x73&#x63&#x72&#x69&#x70&#x74&#x3A&#x61&#x6C&#x65&#x72&#x74&#x28&#x31&#x29\">x</a>",
	"<a href=https://a.test onclick=alert(1)>x</a>",
	"[x](javascript:alert(1))",
	"[x](JaVaScRiPt:alert(1))",
	"[x](java&#x09;script:alert(1))",
	"[x](&#106;avascript:alert(1))",
	"[x](&#x6A;avascript&colon;alert(1))",
	"[x](\tjavascript:alert(1))",
	"[x](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)",
	"[x](vbscript:msgbox(1))",
	"[x](<javascript:alert(1)>)",
	"[x](https://a.test/\"onmouseover=\"alert(1))",
	"[x](https://a.test \"title\" onclick=alert(1))",
	"[x](https://a.test)",
	"[x](mailto:a@b.test)",
	"![x](javascript:alert(1))",
	"![x](https://a.test/track.png)",
	"[a]: javascript:alert(1)\n\n[a]",
	"<javascript:alert(1)>",
	"<https://a.test/\"onmouseover=\"alert(1)>",
	"<a@b.test>",
	"```html\n<script>alert(1)</script>\n```",
	"`<script>`",
	"\\<script\\>alert(1)\\</script\\>",
	"&lt;script&gt;alert(1)&lt;/script&gt;",
	"&#60;script&#62;alert(1)&#60;/script&#62;",
	"&amp;lt;script&amp;gt;",
	"＜script＞alert(1)＜/script＞",
	"[x](ｊavascript:alert(1))",
	"[x](ſavascript:alert(1))",
	"<ſcript>alert(1)</ſcript>",
	"[x](java​script:alert(1))",
	"‮<script>‬",
	"javascript:alert(1)",
	"see javascript: for details, data:text/html and onclick=x are words",
	"<!-- <script>alert(1)</script> -->",
	"<![CDATA[<script>alert(1)</script>]]>",
	"<?php echo 1; ?>",
	"<style>@import 'javascript:alert(1)';</style>",
	"<meta http-equiv=\"refresh\" content=\"0;url=javascript:alert(1)\">",
	"<base href=\"javascript:alert(1)//\">",
	"<form action=\"javascript:alert(1)\"><button>x</button></form>",
	"{{constructor.constructor('alert(1)')()}}",
	"- a\n- b\n\n1. c\n2) d",
	"> > > quote\n\n---\n\n# h\n\n    code\n\n```go\nx\n```",
	"line one  \nline two\\\nline three",
	"*a **b *c* d** e*",
	"`` ` `` and ``` `` ```",
	"1. \n2. \n\n- \n",
	strings.Repeat("> ", 30) + "deep",
	strings.Repeat("*a ", 40),
	strings.Repeat("[", 60) + "x" + strings.Repeat("](javascript:x)", 60),
}

// plantedHTML are rendered documents that must be flagged by dangerousHTML, and
// cleanHTML ones that must not: the control of the detector.
var plantedHTML = []string{
	"<p><script>alert(1)</script></p>",
	"<p><img src=x onerror=alert(1)></p>",
	"<p><a href=\"java&#x09;script:alert(1)\">x</a></p>",
	"<p><a href=\"&#106;avascript:alert(1)\">x</a></p>",
	"<p><a href=\"JaVaScRiPt:alert(1)\">x</a></p>",
	"<p><a href=\"ｊavascript:alert(1)\">x</a></p>",
	"<p><a href=\"  javascript:alert(1)\">x</a></p>",
	"<p><a href=\"data:text/html;base64,AAAA\">x</a></p>",
	"<p style=\"width:expression(alert(1))\">x</p>",
	"<div style=\"background:url(javascript:alert(1))\">x</div>",
	"<p><iframe src=//x.test></iframe></p>",
	"<object data=x.swf></object>",
	"<embed src=x.swf>",
	"<svg onload=alert(1)></svg>",
	"<!-- c --><p>x</p>",
	"<p><a href=\"https://a.test\" onclick=\"x()\">x</a></p>",
}

var cleanHTML = []string{
	"<p>&lt;script&gt;alert(1)&lt;/script&gt; javascript: is text, data:text/html too, onclick=x</p>",
	"<p><a href=\"https://a.test/x?on=1&amp;style=2\">l</a> <strong>b</strong> <em>c</em></p><pre><code class=\"language-go\">x</code></pre><ol start=\"3\"><li>x</li></ol>",
	"<p><a href=\"mailto:a@b.test\">m</a></p><blockquote><p>q</p></blockquote><hr><ul><li>x<br>y</li></ul><h2>h</h2>",
}

// plantedMarkdown must be flagged by dangerousMarkdown (with only https allowed),
// cleanMarkdown must not.
var plantedMarkdown = []string{
	"<script>alert(1)</script>",
	"a <b>c</b>",
	"![i](https://a.test/i.png)",
	"[x](javascript:alert(1))",
	"[x](java&#x09;script:alert(1))",
	"[x](ftp://a.test/x)",
	"<ftp://a.test/x>",
	"[ref][r]\n\n[r]: javascript:alert(1)",
}

var cleanMarkdown = []string{
	"\\<script\\>alert(1)\\</script\\> javascript: is text",
	"[x](<https://a.test/x>) and <https://a.test/y>",
	"`<script>`\n\n```\n<script>\n```",
}

// SEC-097
// FuzzMarkdownSanitize: the Markdown sanitiser of internal/blocks, driven
// through its exported API (Validator.SanitizeMarkdown, SanitizeResponse and the
// markdown block of Validator.JSON), is safe whatever it is given. For any text,
// under each of three validators (no links allowed, https and mailto, and one
// configured by mistake to allow javascript, data and vbscript, which the core
// refuses to carry out): it never panics and finishes in time; a refusal is a
// typed schema_invalid; and an accepted text is inert, read as Markdown and
// rendered by CommonMark (raw HTML on and off) and GFM: no element that runs or
// loads anything, no event handler, no style with an expression, no URL with an
// active scheme however it is spelled (entities, case, tabs, line breaks, zero
// width or full-width letters), no raw HTML, no image, no link outside the
// allowed schemes, no hidden character; and it is its own sanitised form.
//
//	go test -run '^$' -fuzz '^FuzzMarkdownSanitize$' -fuzztime 5s ./tests/security/fuzz/
func FuzzMarkdownSanitize(f *testing.F) {
	// Controls first: the detectors flag what they must and nothing else.
	for _, p := range plantedHTML {
		if len(dangerousHTML(p)) == 0 {
			f.Fatalf("control: the HTML detector did not flag %q", p)
		}
	}
	for _, c := range cleanHTML {
		if got := dangerousHTML(c); len(got) != 0 {
			f.Fatalf("control: the HTML detector flagged clean HTML %q: %v", c, got)
		}
	}
	httpsOnly := map[string]bool{"https": true, "mailto": true}
	for _, p := range plantedMarkdown {
		if len(dangerousMarkdown(p, httpsOnly)) == 0 {
			f.Fatalf("control: the Markdown detector did not flag %q", p)
		}
	}
	for _, c := range cleanMarkdown {
		if got := dangerousMarkdown(c, httpsOnly); len(got) != 0 {
			f.Fatalf("control: the Markdown detector flagged clean Markdown %q: %v", c, got)
		}
	}
	if len(dangerousMarkdown("[x](<https://a.test>)", map[string]bool{})) == 0 {
		f.Fatal("control: a link is not flagged when no scheme is allowed")
	}
	if !hiddenRune('‮') || !hiddenRune('​') || !hiddenRune(0x07) || hiddenRune('\n') || hiddenRune('a') {
		f.Fatal("control: the hidden-character detector is wrong")
	}

	// Mutation control: a rewrite that did nothing would let the payloads through,
	// and the detectors must see it. The raw seeds, unsanitised, must be flagged
	// at least once each class (otherwise the seeds attack nothing).
	flagged := 0
	for _, s := range xssSeeds {
		var buf bytes.Buffer
		if err := cmUnsafe.Convert([]byte(s), &buf); err != nil {
			f.Fatal(err)
		}
		if len(dangerousHTML(buf.String())) > 0 || len(dangerousMarkdown(s, httpsOnly)) > 0 {
			flagged++
		}
	}
	if flagged < 30 {
		f.Fatalf("control: only %d of %d seed payloads are active before sanitising: the seeds do not attack anything", flagged, len(xssSeeds))
	}

	validators := []struct {
		v       *blocks.Validator
		allowed map[string]bool
	}{
		{blocks.New(blocks.Options{}), map[string]bool{}},
		{blocks.New(blocks.Options{LinkSchemes: []string{"https", "mailto"}}), httpsOnly},
		{blocks.New(blocks.Options{LinkSchemes: []string{"https", "mailto", "javascript", "data", "vbscript", "file", "blob"}}), httpsOnly},
	}
	for _, s := range xssSeeds {
		for i := range validators {
			f.Add(s, uint8(i))
		}
	}
	f.Fuzz(func(t *testing.T, in string, which uint8) {
		vv := validators[int(which)%len(validators)]
		var out, viaJSON string
		var err, jerr error
		within(t, 20*time.Second, "the sanitiser", func() {
			out, _, err = vv.v.SanitizeMarkdown(in)
			if err == nil {
				if o2, _, e2 := vv.v.SanitizeResponse(in); e2 != nil || o2 != out {
					err = fmt.Errorf("SanitizeResponse differs from SanitizeMarkdown: %q %v", o2, e2)
					out = "\x00"
				}
			}
			raw, _ := json.Marshal(map[string]string{"type": "markdown", "text": in})
			var b conversation.RichBlock
			b, jerr = vv.v.JSON(raw, blocks.Context{})
			if jerr == nil {
				if m, ok := b.Value.(conversation.MarkdownBlock); ok {
					viaJSON = m.Text
				}
			}
		})
		if out == "\x00" {
			t.Fatal(err)
		}
		if err != nil {
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid || len(blocks.Violations(err)) == 0 {
				t.Fatalf("an untyped refusal of %s: %v", quoteShort([]byte(in)), err)
			}
		} else {
			if msg := checkSanitised(vv.v, vv.allowed, in, out); msg != "" {
				t.Fatalf("input %s: %s", quoteShort([]byte(in)), msg)
			}
		}
		// The markdown block of the JSON path is sanitised the same way: its text is
		// inert too, and equal to what the text path produced.
		if jerr == nil {
			if viaJSON == "" {
				t.Fatalf("input %s: the block was accepted with no text", quoteShort([]byte(in)))
			}
			if msg := checkSanitised(vv.v, vv.allowed, in, viaJSON); msg != "" {
				t.Fatalf("input %s, as a block: %s", quoteShort([]byte(in)), msg)
			}
			if err == nil && viaJSON != out {
				t.Fatalf("input %s: the block's text %q is not the sanitised text %q", quoteShort([]byte(in)), viaJSON, out)
			}
		}
	})
}
