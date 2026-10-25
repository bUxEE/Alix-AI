package blocks

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MarkdownReport counts what the sanitiser took out of one text. It holds
// numbers only, never text of the input, so a caller can log or audit it.
type MarkdownReport struct {
	// HTMLRemoved counts raw HTML blocks and inline fragments that were dropped.
	HTMLRemoved int
	// ImagesRemoved counts images replaced by their alternative text. An image
	// is a request the reader's client makes on its own, so none is kept.
	ImagesRemoved int
	// LinksRemoved counts links and autolinks reduced to their text because
	// their destination is not on the allowed scheme list or is malformed.
	LinksRemoved int
	// Degraded is true when the canonical output failed the independent
	// bluemonday check and the text was returned as escaped plain text.
	Degraded bool
}

// Changed reports whether anything was removed.
func (r MarkdownReport) Changed() bool {
	return r.HTMLRemoved+r.ImagesRemoved+r.LinksRemoved > 0 || r.Degraded
}

// maxMarkdownDepth bounds the nesting of block quotes, lists and emphasis the
// sanitiser follows. A document nested deeper is refused, not truncated.
const maxMarkdownDepth = 24

// maxURLLength bounds an accepted link destination.
const maxURLLength = 2048

// markdownSanitizer parses Markdown with goldmark (CommonMark, raw HTML not
// honoured), keeps only an allowlisted set of nodes and writes the result
// again as canonical Markdown. Nothing is stripped from the source text: a
// construct that is not on the list is not copied, so a renderer that reads
// the output differently from goldmark (a different parser, a different
// extension set) still finds nothing active in it. The output is then rendered
// and put through bluemonday, whose verdict must be "nothing to remove".
type markdownSanitizer struct {
	md      goldmark.Markdown
	unsafe  goldmark.Markdown
	schemes map[string]bool
	policy  *bluemonday.Policy
}

func newMarkdownSanitizer(schemes []string) *markdownSanitizer {
	allowed := map[string]bool{}
	for _, s := range schemes {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			allowed[s] = true
		}
	}
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "h1", "h2", "h3", "h4", "h5", "h6", "em", "strong", "code", "pre",
		"ul", "ol", "li", "blockquote", "hr", "br", "a")
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("class").Matching(regexp.MustCompile(`^language-[a-z0-9_+#.-]{1,32}$`)).OnElements("code")
	p.AllowAttrs("start").Matching(regexp.MustCompile(`^[0-9]{1,9}$`)).OnElements("ol")
	list := make([]string, 0, len(allowed))
	for s := range allowed {
		list = append(list, s)
	}
	if len(list) > 0 {
		p.AllowURLSchemes(list...)
	}
	p.RequireParseableURLs(true)
	return &markdownSanitizer{
		md:      goldmark.New(),
		unsafe:  goldmark.New(goldmark.WithRendererOptions(html.WithUnsafe())),
		schemes: allowed,
		policy:  p,
	}
}

// maxSettlePasses bounds how often the output is read again to find the form
// that no further pass changes.
const maxSettlePasses = 3

// rewrite parses src and writes it again as canonical Markdown. rep counts what
// was removed.
func (s *markdownSanitizer) rewrite(src string, rep *MarkdownReport) (string, error) {
	source := []byte(src)
	doc := s.md.Parser().Parse(text.NewReader(source))
	w := &mdWriter{source: source, schemes: s.schemes, rep: rep}
	lines, err := w.blockChildren(doc, false, 0)
	if err != nil {
		return "", err
	}
	out := strings.Join(lines, "\n")
	if strings.TrimSpace(out) == "" {
		return "", reject("markdown", Violation{Rule: RuleMarkdownEmpty})
	}
	// Entity references decode to characters the raw text did not contain.
	if rule := textRule(out, kindLines); rule != "" {
		return "", reject("markdown", Violation{Rule: rule})
	}
	return out, nil
}

// sanitize returns the canonical, sanitised form of src. It fails closed: an
// input over the size bound, with invalid UTF-8, control or invisible
// characters, nested too deeply, or empty once sanitised is refused.
//
// The form it returns is a fixed point: reading it again changes nothing. Some
// emphasis does not survive being written and read (what a delimiter run means
// depends on the characters around it, which the writer changes), so the output
// is read again until a pass changes nothing; one that never settles, or does
// not read back at all, falls back to escaped plain text.
func (s *markdownSanitizer) sanitize(src string, maxBytes int) (string, MarkdownReport, error) {
	var rep MarkdownReport
	if len(src) > maxBytes {
		return "", rep, reject("markdown", Violation{Rule: RuleMarkdownTooLarge})
	}
	if rule := textRule(src, kindLines); rule != "" {
		return "", rep, reject("markdown", Violation{Rule: rule})
	}
	src = strings.ReplaceAll(strings.ReplaceAll(src, "\r\n", "\n"), "\r", "\n")
	out, err := s.rewrite(src, &rep)
	if err != nil {
		return "", rep, err
	}
	out, settled := s.settle(out)
	if !settled || !s.passesBluemonday(out) {
		// Fall back to the text with every punctuation character escaped, read
		// and written the same way. If even that does not settle, refuse.
		rep.Degraded = true
		out, settled = s.settle(escapePlainText(src))
		if !settled || !s.passesBluemonday(out) {
			return "", rep, reject("markdown", Violation{Rule: RuleMarkdownUnstable})
		}
	}
	return out, rep, nil
}

// settle reads and writes x until a pass changes nothing, and returns that
// fixed point. It reports false when none is reached within maxSettlePasses or a
// pass cannot read the text back.
func (s *markdownSanitizer) settle(x string) (string, bool) {
	for i := 0; i < maxSettlePasses; i++ {
		var scratch MarkdownReport
		next, err := s.rewrite(x, &scratch)
		if err != nil {
			return "", false
		}
		if next == x {
			return x, true
		}
		x = next
	}
	return "", false
}

// passesBluemonday renders out as HTML with raw HTML switched ON (so anything
// that survived would show) and requires bluemonday to find nothing to remove.
func (s *markdownSanitizer) passesBluemonday(out string) bool {
	var buf bytes.Buffer
	if err := s.unsafe.Convert([]byte(out), &buf); err != nil {
		return false
	}
	rendered := buf.String()
	before, err1 := canonicalHTML(rendered)
	after, err2 := canonicalHTML(s.policy.Sanitize(rendered))
	return err1 == nil && err2 == nil && before == after
}

// canonicalHTML parses a fragment and renders it again, so two spellings of one
// document compare equal and a removed element or attribute does not.
func canonicalHTML(fragment string) (string, error) {
	body := &xhtml.Node{Type: xhtml.ElementNode, DataAtom: atom.Body, Data: "body"}
	nodes, err := xhtml.ParseFragment(strings.NewReader(fragment), body)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	for _, n := range nodes {
		if err := xhtml.Render(&buf, n); err != nil {
			return "", err
		}
	}
	return buf.String(), nil
}

// escapePlainText is the fallback: every ASCII punctuation character is
// backslash-escaped, so no construct of any Markdown dialect can arise.
func escapePlainText(src string) string {
	var sb strings.Builder
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		for _, r := range line {
			if r < 0x80 && isASCIIPunct(byte(r)) {
				sb.WriteByte('\\')
			}
			sb.WriteRune(r)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}

func isASCIIPunct(b byte) bool {
	return (b >= '!' && b <= '/') || (b >= ':' && b <= '@') || (b >= '[' && b <= '`') || (b >= '{' && b <= '~')
}

// ---- the writer ----

type mdWriter struct {
	source  []byte
	schemes map[string]bool
	rep     *MarkdownReport
	// lineStart is true while no character has been written on the current
	// output line of an inline run.
	lineStart bool
}

var errTooDeep = reject("markdown", Violation{Rule: RuleMarkdownTooDeep})

// blockChildren renders the block children of n as lines. Blocks are separated
// by an empty line, except inside a tight list item.
func (w *mdWriter) blockChildren(n ast.Node, tight bool, depth int) ([]string, error) {
	if depth > maxMarkdownDepth {
		return nil, errTooDeep
	}
	var out []string
	var prev ast.Node
	listRun := 0
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if _, ok := c.(*ast.List); ok {
			if _, was := prev.(*ast.List); was {
				listRun++
			} else {
				listRun = 0
			}
		}
		lines, err := w.block(c, listRun, depth)
		if err != nil {
			return nil, err
		}
		if len(lines) == 0 {
			continue
		}
		if len(out) > 0 {
			_, prevText := prev.(*ast.TextBlock)
			_, curList := c.(*ast.List)
			if !(tight && prevText && curList) {
				out = append(out, "")
			}
		}
		out = append(out, lines...)
		prev = c
	}
	return out, nil
}

func (w *mdWriter) block(n ast.Node, listRun, depth int) ([]string, error) {
	switch n := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		s, err := w.inlineChildren(n, 0)
		if err != nil {
			return nil, err
		}
		return textLines(s), nil
	case *ast.Heading:
		s, err := w.inlineChildren(n, 0)
		if err != nil {
			return nil, err
		}
		s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
		if s == "" {
			return nil, nil
		}
		level := n.Level
		if level < 1 {
			level = 1
		} else if level > 6 {
			level = 6
		}
		return []string{strings.Repeat("#", level) + " " + s}, nil
	case *ast.ThematicBreak:
		return []string{"---"}, nil
	case *ast.FencedCodeBlock:
		return codeFence(w.codeText(n), cleanLanguage(string(n.Language(w.source)))), nil
	case *ast.CodeBlock:
		return codeFence(w.codeText(n), ""), nil
	case *ast.Blockquote:
		inner, err := w.blockChildren(n, false, depth+1)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(inner))
		for _, l := range inner {
			if l == "" {
				out = append(out, ">")
			} else {
				out = append(out, "> "+l)
			}
		}
		return out, nil
	case *ast.List:
		return w.list(n, listRun, depth)
	case *ast.HTMLBlock:
		w.rep.HTMLRemoved++
		return nil, nil
	}
	// A node the default parser does not produce: keep the text it holds
	// rather than guess a rendering.
	if n.HasChildren() {
		return w.blockChildren(n, false, depth+1)
	}
	return nil, nil
}

func (w *mdWriter) list(l *ast.List, listRun, depth int) ([]string, error) {
	var out []string
	number := l.Start
	if number < 0 || number > 999999999 {
		number = 1
	}
	ordered := l.IsOrdered()
	for item := l.FirstChild(); item != nil; item = item.NextSibling() {
		inner, err := w.blockChildren(item, l.IsTight, depth+1)
		if err != nil {
			return nil, err
		}
		var marker string
		switch {
		case ordered && listRun%2 == 0:
			marker = strconv.Itoa(number) + "."
		case ordered:
			marker = strconv.Itoa(number) + ")"
		case listRun%2 == 0:
			marker = "-"
		default:
			marker = "*"
		}
		number++
		if len(out) > 0 && !l.IsTight {
			out = append(out, "")
		}
		if len(inner) == 0 {
			out = append(out, marker)
			continue
		}
		pad := strings.Repeat(" ", len(marker)+1)
		for i, line := range inner {
			switch {
			case i == 0:
				out = append(out, marker+" "+line)
			case line == "":
				out = append(out, "")
			default:
				out = append(out, pad+line)
			}
		}
	}
	return out, nil
}

// codeText returns the verbatim lines of a code block.
func (w *mdWriter) codeText(n ast.Node) string {
	var sb strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		sb.Write(seg.Value(w.source))
	}
	return strings.TrimRight(sb.String(), "\n")
}

var reLanguage = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_+#.-]{0,31}$`)

// drawnLanguages are the hints a common Markdown renderer draws instead of showing
// (Streamdown and GitHub run Mermaid on a "mermaid" fence). A diagram is a diagram block,
// held to the diagram rules and off unless the host enables it; in Markdown its source is
// plain code.
var drawnLanguages = map[string]bool{"mermaid": true}

// cleanLanguage keeps a language hint only when it is a plain token that no
// renderer draws.
func cleanLanguage(s string) string {
	if !reLanguage.MatchString(s) || drawnLanguages[strings.ToLower(s)] {
		return ""
	}
	return strings.ToLower(s)
}

// codeFence writes a fenced block whose fence is longer than any run of
// backticks inside it, so the content cannot close it.
func codeFence(content, lang string) []string {
	longest, run := 0, 0
	for i := 0; i < len(content); i++ {
		if content[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	out := []string{fence + lang}
	if content != "" {
		out = append(out, strings.Split(content, "\n")...)
	}
	return append(out, fence)
}

// textLines splits a rendered inline run into output lines, dropping the
// leading white space of each (indentation would turn a line into code) and the
// trailing white space (two spaces are a hard break; this writer uses a
// backslash, which it writes itself). A run with nothing visible is no block.
func textLines(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimLeft(strings.TrimRight(p, " \t"), " \t"))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// ---- inline ----

func (w *mdWriter) inlineChildren(n ast.Node, depth int) (string, error) {
	if depth > maxMarkdownDepth {
		return "", errTooDeep
	}
	if depth == 0 {
		w.lineStart = true
	}
	var sb strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		s, err := w.inline(c, depth)
		if err != nil {
			return "", err
		}
		sb.WriteString(s)
	}
	return sb.String(), nil
}

func (w *mdWriter) inline(n ast.Node, depth int) (string, error) {
	switch n := n.(type) {
	case *ast.Text:
		raw := n.Segment.Value(w.source)
		var out string
		if n.IsRaw() {
			out = escapeText(string(raw), w.lineStart)
		} else {
			out = escapeText(decodeText(raw), w.lineStart)
		}
		if strings.TrimLeft(out, " \t") != "" {
			w.lineStart = false
		}
		switch {
		case n.HardLineBreak():
			out = strings.TrimRight(out, " \t") + "\\\n"
			w.lineStart = true
		case n.SoftLineBreak():
			out = strings.TrimRight(out, " \t") + "\n"
			w.lineStart = true
		}
		return out, nil
	case *ast.String:
		out := escapeText(decodeText(n.Value), w.lineStart)
		if out != "" {
			w.lineStart = false
		}
		return out, nil
	case *ast.CodeSpan:
		return w.codeSpan(n), nil
	case *ast.Emphasis:
		inner, err := w.inlineChildren(n, depth+1)
		if err != nil {
			return "", err
		}
		core := strings.Trim(inner, " \t\n")
		if core == "" {
			return inner, nil
		}
		mark := "*"
		if n.Level >= 2 {
			mark = "**"
		}
		lead := inner[:len(inner)-len(strings.TrimLeft(inner, " \t\n"))]
		trail := inner[len(strings.TrimRight(inner, " \t\n")):]
		w.lineStart = false
		return lead + mark + core + mark + trail, nil
	case *ast.Link:
		inner, err := w.inlineChildren(n, depth+1)
		if err != nil {
			return "", err
		}
		dest, ok := w.safeURL(n.Destination)
		if !ok {
			w.rep.LinksRemoved++
			return inner, nil
		}
		label := strings.TrimSpace(inner)
		if label == "" {
			label = escapeText(dest, false)
		}
		w.lineStart = false
		return "[" + strings.ReplaceAll(label, "\n", " ") + "](<" + dest + ">)", nil
	case *ast.AutoLink:
		raw := string(n.URL(w.source))
		if n.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(raw), "mailto:") {
			raw = "mailto:" + raw
		}
		dest, ok := w.safeURL([]byte(raw))
		if !ok {
			w.rep.LinksRemoved++
			return "", nil
		}
		w.lineStart = false
		return "<" + dest + ">", nil
	case *ast.Image:
		w.rep.ImagesRemoved++
		return w.inlineChildren(n, depth+1)
	case *ast.RawHTML:
		w.rep.HTMLRemoved++
		return "", nil
	}
	return w.inlineChildren(n, depth+1)
}

func (w *mdWriter) codeSpan(n *ast.CodeSpan) string {
	var sb strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			sb.Write(t.Segment.Value(w.source))
		}
	}
	content := strings.ReplaceAll(sb.String(), "\n", " ")
	if strings.TrimSpace(content) == "" {
		return ""
	}
	longest, run := 0, 0
	for i := 0; i < len(content); i++ {
		if content[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(content, "`") || strings.HasSuffix(content, "`") ||
		strings.HasPrefix(content, " ") || strings.HasSuffix(content, " ") {
		content = " " + content + " "
	}
	w.lineStart = false
	return fence + content + fence
}

var reEntity = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)

// decodeText returns the text a CommonMark reader shows for raw inline text:
// backslash escapes of ASCII punctuation and entity references resolved in a
// single pass (so "\&amp;" stays the five characters "&amp;").
func decodeText(raw []byte) string {
	var sb strings.Builder
	for i := 0; i < len(raw); {
		b := raw[i]
		switch {
		case b == '\\' && i+1 < len(raw) && isASCIIPunct(raw[i+1]):
			sb.WriteByte(raw[i+1])
			i += 2
		case b == '&':
			if m := reEntity.Find(raw[i:]); m != nil {
				dec := util.ResolveEntityNames(util.ResolveNumericReferences(m))
				if !bytes.Equal(dec, m) && utf8.Valid(dec) {
					sb.Write(dec)
					i += len(m)
					continue
				}
			}
			sb.WriteByte(b)
			i++
		default:
			sb.WriteByte(b)
			i++
		}
	}
	return sb.String()
}

// escapeText backslash-escapes what could start a Markdown construct. The
// escapes are applied to every character that matters in any common dialect,
// so the output is read the same everywhere. atLineStart says whether s begins
// an output line.
func escapeText(s string, atLineStart bool) string {
	var sb strings.Builder
	sb.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		c := s[i]
		start := i == 0 && atLineStart
		switch {
		case c == '\\' || c == '`' || c == '*' || c == '_' || c == '[' || c == ']' ||
			c == '<' || c == '>' || c == '|' || c == '~':
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		case c == '&':
			if reEntity.MatchString(s[i:]) {
				sb.WriteByte('\\')
			}
			sb.WriteByte(c)
			i++
		case c == '#':
			// Only an ATX heading marker at the start of a line means anything; a
			// '#' elsewhere is inert in every dialect, and escaping it by what
			// follows would depend on how goldmark split the text into nodes, which
			// is not stable when the output is parsed again.
			if start {
				sb.WriteByte('\\')
			}
			sb.WriteByte(c)
			i++
		case (c == '-' || c == '+' || c == '=') && start:
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		case c >= '0' && c <= '9' && start:
			j := i
			for j < len(s) && j-i < 10 && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			sb.WriteString(s[i:j])
			if j < len(s) && (s[j] == '.' || s[j] == ')') {
				sb.WriteByte('\\')
			}
			i = j
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return sb.String()
}

// safeURL decodes a link destination the way a renderer does and accepts it
// only when it is a plain absolute URL whose scheme is on the allowed list. It
// returns the canonical form of the URL.
func (w *mdWriter) safeURL(dest []byte) (string, bool) {
	if len(w.schemes) == 0 || len(dest) == 0 || len(dest) > maxURLLength {
		return "", false
	}
	d := util.ResolveEntityNames(util.ResolveNumericReferences(util.UnescapePunctuations(dest)))
	if !utf8.Valid(d) {
		return "", false
	}
	s := string(d)
	for _, r := range s {
		// Browsers drop tabs and line breaks inside a URL, so "java\tscript:" is
		// "javascript:"; a URL with any space or control character is refused
		// rather than normalised, as is one with a backslash.
		if r <= 0x20 || r == 0x7f || r == '\\' || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return "", false
		}
		if isBidiControl(r) || r == 0xFEFF || (r >= 0x200B && r <= 0x200F) || (r >= 0x2060 && r <= 0x2064) {
			return "", false
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" || !w.schemes[scheme] {
		return "", false
	}
	switch scheme {
	case "mailto":
		if u.Opaque == "" || u.Host != "" {
			return "", false
		}
	default:
		if u.Host == "" || u.User != nil {
			return "", false
		}
	}
	u.Scheme = scheme
	out := u.String()
	if out == "" || len(out) > maxURLLength || strings.ContainsAny(out, "<>\"` ") {
		return "", false
	}
	return out, true
}

// String implements fmt.Stringer for debugging reports.
func (r MarkdownReport) String() string {
	return fmt.Sprintf("html=%d images=%d links=%d degraded=%t", r.HTMLRemoved, r.ImagesRemoved, r.LinksRemoved, r.Degraded)
}
