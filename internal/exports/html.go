package exports

import (
	"fmt"
	"html"
	"strings"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/yuin/goldmark"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
)

// renderHTML builds a standalone, print-friendly report. Every model text is
// escaped or run through the blocks Markdown sanitiser then goldmark; charts are
// inline SVG; there is no script and no external resource. CSP is strict.
func renderHTML(blks []IdentifiedBlock, san *blocks.Validator) ([]byte, error) {
	if san == nil {
		san = blocks.Default()
	}
	var body strings.Builder
	for _, blk := range blks {
		section, err := htmlBlock(blk.Block, san)
		if err != nil {
			return nil, err
		}
		body.WriteString(section)
	}
	doc := `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox"/>
<title>Export</title>
<style>
:root { color-scheme: light; }
body { margin: 0; font: 15px/1.5 system-ui, sans-serif; color: #0f172a; background: #fff; }
main { max-width: 52rem; margin: 0 auto; padding: 1.5rem 1.25rem 3rem; }
h1,h2,h3 { line-height: 1.25; }
table { border-collapse: collapse; width: 100%; margin: 1rem 0; }
th, td { border: 1px solid #cbd5e1; padding: 0.4rem 0.6rem; text-align: left; vertical-align: top; }
th { background: #f1f5f9; }
pre, code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 0.92em; }
pre { background: #f8fafc; border: 1px solid #e2e8f0; padding: 0.75rem; overflow: auto; }
.chart { margin: 1rem 0; }
.chart svg { max-width: 100%; height: auto; }
.citations { list-style: disc; padding-left: 1.25rem; }
@media print {
  body { background: #fff; }
  main { max-width: none; padding: 0; }
  a { color: inherit; text-decoration: none; }
}
</style>
</head>
<body>
<main>
` + body.String() + `
</main>
</body>
</html>
`
	if strings.Contains(strings.ToLower(doc), "<script") {
		return nil, fmt.Errorf("html export refused: script tag")
	}
	return []byte(doc), nil
}

func htmlBlock(blk conversation.RichBlock, san *blocks.Validator) (string, error) {
	switch v := blk.Value.(type) {
	case conversation.MarkdownBlock:
		htmlBody, err := markdownToHTML(v.Text, san)
		if err != nil {
			return "", err
		}
		return `<section class="md">` + htmlBody + `</section>`, nil
	case conversation.CodeBlock:
		lang := ""
		if v.Language != nil {
			lang = html.EscapeString(*v.Language)
		}
		return fmt.Sprintf(`<section class="code"><pre><code class="language-%s">%s</code></pre></section>`, lang, html.EscapeString(v.Code)), nil
	case conversation.TableBlock:
		return `<section class="table">` + htmlTable(datasetFromTable("", v)) + `</section>`, nil
	case conversation.ChartBlock:
		svg, err := chartSVG(v)
		if err != nil {
			return "", err
		}
		cap := html.EscapeString(chartTitle(v))
		desc := html.EscapeString(string(v.Description))
		return fmt.Sprintf(`<section class="chart"><h2>%s</h2><p>%s</p>%s%s</section>`,
			cap, desc, svg, htmlTable(datasetFromChart("", v))), nil
	case conversation.CitationListBlock:
		var b strings.Builder
		b.WriteString(`<section class="citations"><h2>Citations</h2><ul class="citations">`)
		for _, c := range v.Citations {
			title := strings.TrimSpace(c.DisplayName)
			if title == "" {
				title = string(c.SourceID)
			}
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(title))
		}
		b.WriteString(`</ul></section>`)
		return b.String(), nil
	case conversation.DiagramBlock:
		title := ""
		if v.Title != nil {
			title = html.EscapeString(string(*v.Title))
		}
		return fmt.Sprintf(`<section class="diagram"><h2>%s</h2><pre>%s</pre></section>`, title, html.EscapeString(string(v.Source))), nil
	case conversation.ActionCardBlock:
		return fmt.Sprintf(`<section class="action"><h2>%s</h2></section>`, html.EscapeString(v.Title)), nil
	case conversation.ClarificationChoicesBlock:
		var b strings.Builder
		b.WriteString(`<section class="choices"><h2>Choices</h2><ul>`)
		for _, ch := range v.Choices {
			fmt.Fprintf(&b, `<li>%s</li>`, html.EscapeString(ch.Label))
		}
		b.WriteString(`</ul></section>`)
		return b.String(), nil
	case conversation.ConfirmationPanelBlock:
		return fmt.Sprintf(`<section class="confirm"><h2>Confirmation</h2><p>%s</p></section>`, html.EscapeString(v.Summary)), nil
	default:
		return `<section><p>Unsupported block</p></section>`, nil
	}
}

func htmlTable(set Dataset) string {
	if len(set.Headers) == 0 {
		return ""
	}
	var b strings.Builder
	if set.Name != "" && set.Name != "table" && set.Name != "chart" {
		fmt.Fprintf(&b, `<h3>%s</h3>`, html.EscapeString(set.Name))
	}
	b.WriteString(`<table><thead><tr>`)
	for _, h := range set.Headers {
		fmt.Fprintf(&b, `<th>%s</th>`, html.EscapeString(h))
	}
	b.WriteString(`</tr></thead><tbody>`)
	for _, row := range set.Rows {
		b.WriteString(`<tr>`)
		for i := range set.Headers {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			fmt.Fprintf(&b, `<td>%s</td>`, html.EscapeString(cell))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

func markdownToHTML(src string, san *blocks.Validator) (string, error) {
	clean, _, err := san.SanitizeMarkdown(src)
	if err != nil {
		// Fail closed to escaped plain text rather than refusing the whole export.
		return `<p>` + html.EscapeString(src) + `</p>`, nil
	}
	md := goldmark.New(goldmark.WithRendererOptions(goldmarkhtml.WithHardWraps()))
	var buf strings.Builder
	if err := md.Convert([]byte(clean), &buf); err != nil {
		return `<p>` + html.EscapeString(clean) + `</p>`, nil
	}
	out := buf.String()
	if strings.Contains(strings.ToLower(out), "<script") {
		return `<p>` + html.EscapeString(clean) + `</p>`, nil
	}
	return out, nil
}
