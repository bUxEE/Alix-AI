package main

import (
	"strings"
	"unicode"
)

// initialisms are words that keep a fixed spelling inside generated names:
// capability_id becomes CapabilityID, base_url BaseURL, openapiEndpoint
// OpenAPIEndpoint. Anything else is capitalised.
var initialisms = map[string]string{
	"id":      "ID",
	"ids":     "IDs",
	"url":     "URL",
	"uri":     "URI",
	"http":    "HTTP",
	"https":   "HTTPS",
	"api":     "API",
	"sha":     "SHA",
	"sha256":  "SHA256",
	"ui":      "UI",
	"ttl":     "TTL",
	"ipc":     "IPC",
	"tcp":     "TCP",
	"mcp":     "MCP",
	"rag":     "RAG",
	"ocr":     "OCR",
	"stt":     "STT",
	"tts":     "TTS",
	"json":    "JSON",
	"ms":      "MS",
	"sse":     "SSE",
	"ulid":    "ULID",
	"openapi": "OpenAPI",
	"alixai":  "AlixAI",
}

// words splits an identifier into words at every non-alphanumeric character
// and at camelCase humps ("candidateValue" -> candidate, Value; "HTTPServer" ->
// HTTP, Server). Digits stay attached to the word before them.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// pascal converts an identifier to an exported Go/TypeScript type or field name.
func pascal(s string) string {
	var b strings.Builder
	for _, w := range words(s) {
		lw := strings.ToLower(w)
		if v, ok := initialisms[lw]; ok {
			b.WriteString(v)
			continue
		}
		rs := []rune(w)
		b.WriteRune(unicode.ToUpper(rs[0]))
		b.WriteString(string(rs[1:]))
	}
	return b.String()
}

// goPkgName is the Go package (and directory) name of a schema area:
// application-package becomes applicationpackage.
func goPkgName(area string) string { return strings.ReplaceAll(area, "-", "") }

// snake converts a schema stem to a file-name stem usable in Go: task-proposal
// becomes task_proposal.
func snake(stem string) string { return strings.ReplaceAll(stem, "-", "_") }

// wrapText greedily wraps text to at most width columns. Existing line breaks
// are kept as paragraph breaks.
func wrapText(text string, width int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		fields := strings.Fields(para)
		if len(fields) == 0 {
			out = append(out, "")
			continue
		}
		line := fields[0]
		for _, f := range fields[1:] {
			if len(line)+1+len(f) > width {
				out = append(out, line)
				line = f
				continue
			}
			line += " " + f
		}
		out = append(out, line)
	}
	// Drop leading and trailing blank lines.
	for len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
