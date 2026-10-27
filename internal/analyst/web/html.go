package web

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

var skipTags = map[string]bool{
	"script": true, "style": true, "template": true, "noscript": true,
	"svg": true, "math": true, "iframe": true, "object": true, "embed": true,
	"canvas": true, "audio": true, "video": true,
}

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "section": true, "article": true,
	"header": true, "footer": true, "blockquote": true, "pre": true, "hr": true,
}

// ExtractText strips scripts/styles and returns plain text plus a title.
func ExtractText(rawHTML string) (title, text string) {
	z := html.NewTokenizer(strings.NewReader(rawHTML))
	var b strings.Builder
	var titleB strings.Builder
	skip := 0
	inTitle := false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			text = collapseSpace(b.String())
			if utf8.RuneCountInString(text) > maxExtractRunes {
				text = truncateRunes(text, maxExtractRunes)
			}
			title = collapseSpace(titleB.String())
			if title == "" {
				title = "untitled"
			}
			if len(title) > 512 {
				title = truncateRunes(title, 512)
			}
			return title, text
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := strings.ToLower(string(name))
			if skipTags[tag] {
				skip++
				continue
			}
			if tag == "title" {
				inTitle = true
			}
			if skip == 0 && blockTags[tag] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := strings.ToLower(string(name))
			if skipTags[tag] && skip > 0 {
				skip--
				continue
			}
			if tag == "title" {
				inTitle = false
			}
			if skip == 0 && blockTags[tag] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if skip > 0 {
				continue
			}
			t := string(z.Text())
			if inTitle {
				titleB.WriteString(t)
			} else {
				b.WriteString(t)
			}
		}
	}
}

func collapseSpace(s string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}

func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
