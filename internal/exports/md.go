package exports

import (
	"fmt"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func renderMarkdown(blocks []IdentifiedBlock) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Export\n\n")
	for i, blk := range blocks {
		if i > 0 {
			b.WriteString("\n")
		}
		if err := writeMDBlock(&b, blk.Block); err != nil {
			return nil, err
		}
	}
	return []byte(b.String()), nil
}

func writeMDBlock(b *strings.Builder, blk conversation.RichBlock) error {
	switch v := blk.Value.(type) {
	case conversation.MarkdownBlock:
		b.WriteString(strings.TrimSpace(v.Text))
		b.WriteString("\n")
	case conversation.CodeBlock:
		lang := ""
		if v.Language != nil {
			lang = string(*v.Language)
		}
		fence := codeFence(v.Code)
		fmt.Fprintf(b, "%s%s\n%s\n%s\n", fence, lang, v.Code, fence)
	case conversation.TableBlock:
		writeMDTable(b, datasetFromTable("", v))
	case conversation.ChartBlock:
		if v.Title != nil && strings.TrimSpace(string(*v.Title)) != "" {
			fmt.Fprintf(b, "### %s\n\n", strings.TrimSpace(string(*v.Title)))
		}
		if d := strings.TrimSpace(string(v.Description)); d != "" {
			fmt.Fprintf(b, "*%s*\n\n", d)
		}
		writeMDTable(b, datasetFromChart("", v))
	case conversation.CitationListBlock:
		b.WriteString("### Citations\n\n")
		for _, c := range v.Citations {
			title := strings.TrimSpace(c.DisplayName)
			if title == "" {
				title = string(c.SourceID)
			}
			fmt.Fprintf(b, "- %s\n", title)
		}
	case conversation.DiagramBlock:
		b.WriteString("### Diagram\n\n")
		if v.Title != nil && strings.TrimSpace(string(*v.Title)) != "" {
			fmt.Fprintf(b, "*%s*\n\n", strings.TrimSpace(string(*v.Title)))
		}
		src := string(v.Source)
		fence := codeFence(src)
		fmt.Fprintf(b, "%s\n%s\n%s\n", fence, src, fence)
	case conversation.ActionCardBlock:
		fmt.Fprintf(b, "### %s\n", strings.TrimSpace(v.Title))
	case conversation.ClarificationChoicesBlock:
		b.WriteString("### Choices\n\n")
		for _, ch := range v.Choices {
			fmt.Fprintf(b, "- %s\n", ch.Label)
		}
	case conversation.ConfirmationPanelBlock:
		fmt.Fprintf(b, "### Confirmation\n\n%s\n", strings.TrimSpace(v.Summary))
	default:
		b.WriteString("_unsupported block_\n")
	}
	return nil
}

func writeMDTable(b *strings.Builder, set Dataset) {
	if len(set.Headers) == 0 {
		return
	}
	if set.Name != "" && set.Name != "table" && set.Name != "chart" {
		fmt.Fprintf(b, "**%s**\n\n", set.Name)
	}
	b.WriteString("| ")
	b.WriteString(strings.Join(escapePipes(set.Headers), " | "))
	b.WriteString(" |\n|")
	for range set.Headers {
		b.WriteString(" --- |")
	}
	b.WriteByte('\n')
	for _, row := range set.Rows {
		cells := make([]string, len(set.Headers))
		for i := range set.Headers {
			if i < len(row) {
				cells[i] = row[i]
			}
		}
		b.WriteString("| ")
		b.WriteString(strings.Join(escapePipes(cells), " | "))
		b.WriteString(" |\n")
	}
}

func escapePipes(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(c, "|", "\\|"), "\n", " ")
	}
	return out
}

func codeFence(body string) string {
	n := 3
	for strings.Contains(body, strings.Repeat("`", n)) {
		n++
	}
	return strings.Repeat("`", n)
}
