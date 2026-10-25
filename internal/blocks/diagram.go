package blocks

import (
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// diagramTypes are the Mermaid diagram kinds a block may open with. A source
// that opens with anything else (a directive, front matter, a kind the host's
// renderer may handle differently) is refused: the set is closed, not a deny
// list.
var diagramTypes = map[string]bool{
	"flowchart": true, "graph": true, "sequencediagram": true, "classdiagram": true,
	"statediagram": true, "statediagram-v2": true, "erdiagram": true, "gantt": true,
	"pie": true, "journey": true, "mindmap": true, "timeline": true,
}

// diagramForbiddenStatements are statements that carry a callback, a URL or a
// tooltip script: Mermaid's click, callback and link statements.
var diagramForbiddenStatements = map[string]bool{
	"click": true, "callback": true, "call": true, "link": true, "links": true, "href": true,
}

var (
	reMermaidEntity = regexp.MustCompile(`#([0-9]{1,7}|[A-Za-z][A-Za-z0-9]{1,31});`)
	reCSSURL        = regexp.MustCompile(`(?i)url\s*\(`)
	reNodeShape     = regexp.MustCompile(`@\s*\{`)
)

// decodeMermaid resolves HTML entities and Mermaid's own "#35;" / "#quot;"
// entity codes, twice, so a double-encoded "&amp;#106;avascript:" is judged by
// what it finally says.
func decodeMermaid(s string) string {
	for i := 0; i < 2; i++ {
		prev := s
		s = html.UnescapeString(s)
		s = reMermaidEntity.ReplaceAllStringFunc(s, func(m string) string {
			body := m[1 : len(m)-1]
			if n, err := strconv.Atoi(body); err == nil {
				if n > 0 && n <= 0x10FFFF {
					return string(rune(n))
				}
				return m
			}
			if dec := html.UnescapeString("&" + body + ";"); dec != "&"+body+";" {
				return dec
			}
			return m
		})
		if s == prev {
			break
		}
	}
	return s
}

// checkDiagram holds a diagram to typed text: Mermaid source from a closed set
// of diagram kinds, bounded, with nothing that calls out (click, callback,
// link, URLs, node-shape images), nothing that reconfigures the renderer
// (directives, front matter) and no markup. Statements are split on newlines
// and on ';' (Mermaid accepts both), so "A-->B;click A ..." is read as the two
// statements it is. The host's renderer must still run in strict mode; this is
// the core's share of that defence.
func (v *Validator) checkDiagram(d conversation.DiagramBlock, col *collector) {
	lim := v.limits
	src := string(d.Source)
	if len(src) > lim.MaxDiagramBytes || strings.Count(src, "\n") >= lim.MaxDiagramLines {
		col.add(RuleDiagramBounds, "/source")
		return
	}
	if d.Title != nil {
		if rule := textRule(string(*d.Title), kindLine); rule != "" {
			col.add(rule, "/title")
		}
	}
	if rule := textRule(string(d.Description), kindLine); rule != "" {
		col.add(rule, "/description")
	}
	if rule := textRule(src, kindLines); rule != "" {
		col.add(rule, "/source")
		return
	}
	for _, form := range []string{src, decodeMermaid(src)} {
		if rule := textRule(form, kindLines); rule != "" {
			col.add(rule, "/source")
			return
		}
		if rule := diagramTextRule(form); rule != "" {
			col.add(rule, "/source")
			return
		}
		if rule := diagramStatementsRule(form); rule != "" {
			col.add(rule, "/source")
			return
		}
	}
}

// diagramTextRule rejects markup, URLs, directives and node-shape images in
// any spelling of the source.
func diagramTextRule(s string) Rule {
	switch {
	case reTagStart.MatchString(s):
		return RuleMarkup
	case strings.Contains(s, "%%{"):
		return RuleDiagramStatement
	case strings.Contains(s, "://") || reActiveScheme.MatchString(s) || reDataURL.MatchString(s) || reFileScheme.MatchString(s):
		return RuleScheme
	case reCSSURL.MatchString(s) || reNodeShape.MatchString(s):
		return RuleScheme
	case hasTemplateMarker(s):
		return RuleExpression
	}
	// Characters that are removed or ignored inside a scheme: "java\u0000script:".
	squashed := strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToLower(s))
	if strings.Contains(squashed, "javascript:") || strings.Contains(squashed, "vbscript:") {
		return RuleScheme
	}
	return ""
}

// diagramStatementsRule checks the statements of the source: the first must
// name an allowed diagram kind and none may be a callback or link statement.
func diagramStatementsRule(s string) Rule {
	first := true
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == ';' }) {
		stmt := strings.TrimSpace(line)
		if stmt == "" {
			continue
		}
		if strings.HasPrefix(stmt, "%%") {
			continue // a Mermaid comment ("%%{" directives were refused above)
		}
		if strings.HasPrefix(stmt, "---") && first {
			return RuleDiagramStatement // front matter can configure the renderer
		}
		words := strings.FieldsFunc(stmt, func(r rune) bool { return r == ' ' || r == '\t' || r == ':' })
		if len(words) == 0 {
			continue
		}
		word := strings.ToLower(words[0])
		if first {
			if !diagramTypes[word] {
				return RuleDiagramType
			}
			first = false
			continue
		}
		if diagramForbiddenStatements[word] {
			return RuleDiagramStatement
		}
	}
	if first {
		return RuleDiagramType // nothing but comments
	}
	return ""
}
