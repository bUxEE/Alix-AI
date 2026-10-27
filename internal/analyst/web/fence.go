package web

import (
	"fmt"
	"strings"
)

const (
	fenceOpen  = "<<<UNTRUSTED_WEB_DATA>>>"
	fenceClose = "<<<END_UNTRUSTED_WEB_DATA>>>"
)

// Fence wraps raw web text so a model must treat it as data, not instructions.
func Fence(kind, body string) string {
	var b strings.Builder
	b.WriteString(fenceOpen)
	b.WriteByte('\n')
	if kind != "" {
		fmt.Fprintf(&b, "kind: %s\n", kind)
	}
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(fenceClose)
	return b.String()
}

func fenceHits(hits []SearchHit) string {
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. title=%q url=%q\n   %s\n", i+1, h.Title, h.URL, h.Snippet)
	}
	return Fence("search_results", b.String())
}

func fencePage(title, url, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "title=%q\nurl=%q\n\n%s", title, url, text)
	return Fence("fetched_page", b.String())
}
