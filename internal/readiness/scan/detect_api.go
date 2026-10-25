package scan

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	scanMCPDepRE        = regexp.MustCompile(`(?i)(modelcontextprotocol|mark3labs/mcp-go|\bfastmcp\b|fastapi-mcp|mcp-framework|"mcp"\s*:|^\s*["']?mcp(\[[a-z,]+\])?\s*(["',=<>~!]|$))`)
	scanMCPEntryRE      = regexp.MustCompile(`(\bnew\s+McpServer\(|\bFastMCP\(|\bNewMCPServer\(|\bmcp\.NewServer\(|\bStdioServerTransport\b|\bStreamableHTTPServerTransport\b|\bSSEServerTransport\b|mcp\.server\.fastmcp|\bMcpServer\s*\()`)
	scanToolRegRE       = regexp.MustCompile("(?:registerTool|\\.tool|\\.AddTool|add_tool|addTool|mcp\\.NewTool)\\(\\s*[\"'`]([A-Za-z0-9_.:\\-]{1,64})[\"'`]")
	scanToolDynRE       = regexp.MustCompile(`(?:registerTool|\.AddTool|add_tool|addTool)\(\s*[A-Za-z_]`)
	scanToolPyRE        = regexp.MustCompile(`^\s*@\w+\.tool\b`)
	scanToolSchemaRE    = regexp.MustCompile(`(?i)(inputSchema|input_schema|outputSchema|z\.object|InputSchema|\bparameters\s*:|jsonschema)`)
	scanToolHeadRE      = regexp.MustCompile(`(?i)^#{1,6}\s+.*(\bmcp\b.*\btools?\b|^tools?\s*$|\btools?\s+(list|exposed|provided))`)
	scanToolDocItemRE   = regexp.MustCompile("^\\s*[-*|]\\s*`([A-Za-z][A-Za-z0-9_.\\-]{1,63})`")
	scanNativeImportRE  = regexp.MustCompile(`(?i)(from\s+['"]@alixai/|require\(['"]@alixai/|import\s+.*['"]@alixai/|github\.com/buxee/alix-ai/pkg/(sdk-go|bridge)|^\s*from\s+alixai\b|^\s*import\s+alixai\b)`)
	scanNativeOpsRE     = regexp.MustCompile(`(?i)\b(registerOperation|defineCapability|defineOperation|operations\s*[:=]|capabilit(?:y|ies)\s*[:=]|bridge\.(?:Register|Handle))`)
	scanOpenAPIHeadJSON = regexp.MustCompile(`"(openapi|swagger)"\s*:\s*"([23])`)
	scanOpenAPIHeadYAML = regexp.MustCompile(`^(openapi|swagger)\s*:\s*["']?([23])`)
	scanYAMLPathRE      = regexp.MustCompile(`^(?:"([^"]+)"|'([^']+)'|(/[^\s:]*))\s*:\s*(.*)$`)
	scanYAMLCodeRE      = regexp.MustCompile(`^["']?(\d{3}|default|[1-5]XX)["']?\s*:`)
	scanExtRefRE        = regexp.MustCompile(`\$ref["']?\s*:\s*["']?https?://`)
	// an API path with a health or inspection segment anywhere (/health, /v1/health, /api/v1/inspect); status and ping only
	// at the root, since /orders/{id}/status is an ordinary operation.
	scanHealthRE = regexp.MustCompile(`(?i)^/(status|ping)(/|$)|(^|/)(health|healthz|ready|readyz|livez|inspect|_health)(/|$)`)
	// the exact names an inspection operation, tool or handler goes by
	scanInspectNames = map[string]bool{"health": true, "healthz": true, "ready": true, "readyz": true, "livez": true, "inspect": true}
	// a native operation registered under an inspection name, and a native bridge's inspect or health handler
	scanNativeInspectOpRE = regexp.MustCompile("(?:\\.Handle|registerOperation|defineOperation|\\.Register)\\(\\s*[\"'`](health|healthz|ready|readyz|livez|inspect)[\"'`]")
	scanNativeHandlerRE   = regexp.MustCompile(`\b(Inspect|Health)\s*:\s*[A-Za-z_(]`)
	scanReceiptRE         = regexp.MustCompile(`(?i)receipt|confirmation|reference|result`)
	scanHTTPMethods       = map[string]bool{"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true, "trace": true}
)

const scanMaxJSONDepth = 48

// detectMCPAndNative finds MCP SDK dependencies, configuration, server entry points, tool
// registrations and tool listings in docs, and native-adapter SDK use.
func (s *scanState) detectMCPAndNative(f *scanFileRec, lines []string) {
	p := f.Path
	base := scanBase(p)
	isDep := base == "package.json" || strings.HasPrefix(base, "requirements") || base == "pyproject.toml" || base == "go.mod" ||
		base == "cargo.toml" || base == "pom.xml" || strings.HasPrefix(base, "build.gradle") || base == "pipfile" || base == "setup.py" || scanExt(p) == ".csproj"
	if isDep {
		for i, l := range lines {
			if l != "" && scanMCPDepRE.MatchString(l) {
				s.add("mcp.dep", f, i+1, i+1, l)
				break
			}
		}
	}
	if base == ".mcp.json" || base == "mcp.json" || base == "claude_desktop_config.json" {
		s.add("mcp.config", f, 1, 1, p)
	}
	code := scanIsCode(p)
	var gotEntry, gotTool, gotSchema, gotImport, gotOps bool
	for i, l := range lines {
		if l == "" {
			continue
		}
		switch {
		case scanExt(p) == ".json" && strings.Contains(l, `"mcpServers"`):
			s.add("mcp.config", f, i+1, i+1, l)
		case scanIsYAML(p) && strings.HasPrefix(strings.TrimSpace(l), "transport:") && strings.Contains(l, "mcp"):
			s.add("mcp.config", f, i+1, i+1, l)
		}
		if !code {
			continue
		}
		if !gotEntry && scanMCPEntryRE.MatchString(l) {
			gotEntry = true
			s.add("mcp.entry", f, i+1, i+1, l)
		}
		if m := scanToolRegRE.FindStringSubmatch(l); m != nil {
			s.tools[m[1]] = true
			s.add("mcp.tool", f, i+1, i+1, l)
			gotTool = true
			s.toolSchemaNear(f, lines, i)
			if scanInspectNames[strings.ToLower(m[1])] {
				s.add("inspect.endpoint", f, i+1, i+1, "tool "+m[1])
			}
		} else if scanToolOpenRE.MatchString(l) && i+1 < len(lines) {
			if m := scanToolNameLineRE.FindStringSubmatch(lines[i+1]); m != nil {
				s.tools[m[1]] = true
				if scanInspectNames[strings.ToLower(m[1])] {
					s.add("inspect.endpoint", f, i+1, i+2, "tool "+m[1])
				}
			}
			s.add("mcp.tool", f, i+1, i+2, l)
			gotTool = true
			s.toolSchemaNear(f, lines, i)
		} else if scanToolDynRE.MatchString(l) || scanToolPyRE.MatchString(l) {
			s.add("mcp.tool", f, i+1, i+1, l)
			gotTool = true
			s.toolSchemaNear(f, lines, i)
		}
		if !gotImport && scanNativeImportRE.MatchString(l) {
			gotImport = true
			s.add("native.import", f, i+1, i+1, l)
		}
		if !gotOps && scanNativeOpsRE.MatchString(l) {
			gotOps = true
			s.add("native.ops", f, i+1, i+1, l)
		}
		if scanNativeInspectOpRE.MatchString(l) {
			s.add("inspect.endpoint", f, i+1, i+1, l)
		}
	}
	_, _ = gotTool, gotSchema
	if gotImport && gotOps {
		s.add("native.full", f, 1, 1, p)
	}
	if gotImport && code {
		s.detectNativeHandlers(f, lines)
	}
	if scanIsDocText(p) {
		for i := 0; i < len(lines); i++ {
			if !scanToolHeadRE.MatchString(lines[i]) {
				continue
			}
			names := 0
			for j := i + 1; j < len(lines) && j <= i+60 && names < 3; j++ {
				if strings.HasPrefix(lines[j], "#") {
					break
				}
				if scanToolDocItemRE.MatchString(lines[j]) {
					names++
					s.add("mcp.tool.doc", f, j+1, j+1, lines[j])
				}
			}
		}
	}
}

// detectNativeHandlers finds the inspect and health handlers a native bridge is built with (bridge.Config{Inspect: ...,
// Health: ...}). It runs only on a code file that imports an AlixAI SDK, so a Health field of an unrelated struct does not
// count, and it reads code only: a handler named in a comment does not count either.
func (s *scanState) detectNativeHandlers(f *scanFileRec, lines []string) {
	for i, l := range lines {
		code, _, _ := strings.Cut(l, "//")
		if t := strings.TrimSpace(code); t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		if m := scanNativeHandlerRE.FindStringSubmatch(code); m != nil {
			s.add("inspect.endpoint", f, i+1, i+1, "native bridge "+strings.ToLower(m[1])+" handler")
		}
	}
}

// toolSchemaNear looks for an input schema in the lines around a tool registration.
func (s *scanState) toolSchemaNear(f *scanFileRec, lines []string, i int) {
	end := i + 20
	if end > len(lines) {
		end = len(lines)
	}
	for j := i; j < end; j++ {
		if scanToolSchemaRE.MatchString(lines[j]) {
			s.add("mcp.tool.schema", f, i+1, j+1, lines[i])
			return
		}
	}
	// A Python tool is typed by its annotated signature.
	if scanToolPyRE.MatchString(lines[i]) {
		for j := i + 1; j < end && j < i+4; j++ {
			if strings.Contains(lines[j], "def ") && strings.Contains(lines[j], ":") && strings.Contains(lines[j], "->") {
				s.add("mcp.tool.schema", f, i+1, j+1, lines[j])
				return
			}
		}
	}
}

// detectOperationsText reads the adapter configuration allow list: the tools a package's MCP
// adapter lets through, which also names the validate, prepare, commit and status operations.
func (s *scanState) detectOperationsText(f *scanFileRec, lines []string) {
	if !scanIsYAML(f.Path) || !scanInDir(f.Path, "adapters") {
		return
	}
	inTools, inAllow, inMgmt := false, false, false
	for i, l := range lines {
		if scanBlankOrComment(l) {
			continue
		}
		ind := scanIndent(l)
		t := strings.TrimSpace(l)
		if ind == 0 {
			inTools = strings.HasPrefix(t, "tools:")
			inMgmt = strings.HasPrefix(t, "management:")
			inAllow = false
			continue
		}
		// the management operations an adapter configuration names: health and inspect are the inspection surface
		if inMgmt {
			if k, v, ok := strings.Cut(t, ":"); ok && scanInspectNames[k] && strings.Trim(strings.TrimSpace(v), `"'`) != "" {
				s.add("inspect.endpoint", f, i+1, i+1, "management operation "+t)
			}
			continue
		}
		if inTools && strings.HasPrefix(t, "allow:") {
			inAllow = true
			continue
		}
		if inTools && inAllow && strings.HasPrefix(t, "- ") {
			name := strings.Trim(strings.TrimSpace(t[2:]), `"'`)
			if !scanCapIDRE.MatchString(name) {
				continue
			}
			s.tools[name] = true
			s.add("mcp.config", f, i+1, i+1, "adapter allows tool "+name)
			s.add("mcp.tool.allow", f, i+1, i+1, "adapter allows tool "+name)
			if k := scanOpKindOfName(name); k != "" && k != "read" {
				s.add("op."+k, f, i+1, i+1, name)
			} else if k == "read" {
				s.add("op.read", f, i+1, i+1, name)
			}
			switch strings.ToLower(name) {
			case "health", "inspect", "describe":
				s.add("inspect.endpoint", f, i+1, i+1, "management tool "+name)
			}
		} else if inTools && inAllow && ind <= 2 && !strings.HasPrefix(t, "-") {
			inAllow = false
		}
	}
}

// scanTokens splits an identifier into lower-case words (camelCase, snake_case, kebab-case, dots).
func scanTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1])):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

var (
	scanReadWords = map[string]bool{"list": true, "get": true, "find": true, "search": true, "read": true, "query": true, "fetch": true, "lookup": true, "resolve": true, "describe": true, "availability": true, "show": true}
)

// scanOpKindOfName classifies an operation by the words of its name; "" when the name says nothing.
func scanOpKindOfName(name string) string {
	for _, w := range scanTokens(name) {
		switch w {
		case "validate", "validation":
			return "validate"
		case "prepare", "draft", "quote":
			return "prepare"
		case "commit", "confirm", "execute":
			return "commit"
		case "status":
			return "status"
		}
	}
	for _, w := range scanTokens(name) {
		if scanReadWords[w] {
			return "read"
		}
	}
	return ""
}

type scanRawOp struct {
	method, path, id string
	line             int
	typed, errTyped  bool
	idem             bool
	refs             string
}

// classifyOp turns one declared API operation into signals.
func (s *scanState) classifyOp(f *scanFileRec, op scanRawOp) ScanOperation {
	label := strings.ToUpper(op.method) + " " + op.path
	s.add("openapi.op", f, op.line, op.line, label)
	kind := scanOpKindOfName(op.id)
	if kind == "" {
		switch op.method {
		case "get", "head":
			kind = "read"
		default:
			kind = "write"
		}
	}
	switch kind {
	case "write":
		s.add("op.write", f, op.line, op.line, label)
	default:
		s.add("op."+kind, f, op.line, op.line, label)
	}
	if op.typed {
		switch kind {
		case "read", "validate", "prepare", "commit":
			s.add("op."+kind+".typed", f, op.line, op.line, label)
		}
		if kind == "commit" || kind == "write" {
			if scanReceiptRE.MatchString(op.refs + " " + op.id) {
				s.add("op.commit.receipt", f, op.line, op.line, label)
			}
		}
	}
	if op.errTyped {
		s.add("err.typed", f, op.line, op.line, label+" declares a typed error response")
	}
	if op.idem {
		s.add("safe.idem", f, op.line, op.line, label+" takes an idempotency key")
	}
	if scanHealthRE.MatchString(op.path) {
		s.add("inspect.endpoint", f, op.line, op.line, label)
	}
	return ScanOperation{Method: strings.ToUpper(op.method), Path: op.path, OperationID: op.id, Line: op.line, Typed: op.typed}
}

// detectOpenAPI recognises an OpenAPI 3.x or Swagger 2.x document in JSON or YAML and lists the
// operations it declares. It never resolves a $ref: external references are only counted.
func (s *scanState) detectOpenAPI(f *scanFileRec, lines []string) {
	ext := scanExt(f.Path)
	if ext != ".json" && ext != ".yaml" && ext != ".yml" {
		return
	}
	var format string
	var verLine int
	if ext == ".json" {
		head := f.content
		if len(head) > 2048 {
			head = head[:2048]
		}
		m := scanOpenAPIHeadJSON.FindSubmatch(head)
		if m == nil {
			return
		}
		format = scanAPIFormat(string(m[1]), string(m[2]))
		verLine = 1 + bytes.Count(head[:bytes.Index(head, m[0])], []byte{'\n'})
	} else {
		seen := 0
		for i, l := range lines {
			if scanBlankOrComment(l) {
				continue
			}
			if seen++; seen > 60 {
				return
			}
			if m := scanOpenAPIHeadYAML.FindStringSubmatch(l); m != nil {
				format, verLine = scanAPIFormat(m[1], m[2]), i+1
				break
			}
		}
		if format == "" {
			return
		}
	}
	doc := ScanAPIDoc{Path: f.Path, Format: format, Operations: []ScanOperation{}}
	var raw []scanRawOp
	if ext == ".json" {
		if !scanJSONDepthOK(f.content, scanMaxJSONDepth) {
			s.note(f.Path, ScanSkipTooDeep)
			s.add("openapi.unparsed", f, verLine, verLine, "OpenAPI document that could not be parsed within the nesting cap")
			s.apis = append(s.apis, doc)
			return
		}
		var ok bool
		raw, ok = scanParseOpenAPIJSON(f.content)
		if !ok {
			s.note(f.Path, ScanSkipParseError)
			s.add("openapi.unparsed", f, verLine, verLine, "OpenAPI document that could not be parsed")
			s.apis = append(s.apis, doc)
			return
		}
	} else {
		raw = scanParseOpenAPIYAML(lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "$ref") && scanExtRefRE.MatchString(l) {
			doc.ExternalRefs++
		}
	}
	s.add("openapi.doc", f, verLine, verLine, format+" document")
	for _, op := range raw {
		doc.Operations = append(doc.Operations, s.classifyOp(f, op))
	}
	s.apis = append(s.apis, doc)
}

func scanAPIFormat(kind, major string) string {
	if strings.EqualFold(kind, "swagger") {
		return "swagger-2"
	}
	return "openapi-3"
}

func scanLineOfOffset(data []byte, off int) int {
	if off < 0 {
		return 1
	}
	if off > len(data) {
		off = len(data)
	}
	return 1 + bytes.Count(data[:off], []byte{'\n'})
}

func scanParseOpenAPIJSON(data []byte) ([]scanRawOp, bool) {
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, false
	}
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var ops []scanRawOp
	for _, p := range paths {
		var item map[string]json.RawMessage
		if json.Unmarshal(doc.Paths[p], &item) != nil {
			continue
		}
		quoted, _ := json.Marshal(p)
		pOff := bytes.Index(data, quoted)
		methods := make([]string, 0, len(item))
		for m := range item {
			if scanHTTPMethods[strings.ToLower(m)] {
				methods = append(methods, m)
			}
		}
		sort.Strings(methods)
		for _, m := range methods {
			var op struct {
				OperationID string                     `json:"operationId"`
				Responses   map[string]json.RawMessage `json:"responses"`
				Parameters  []json.RawMessage          `json:"parameters"`
			}
			_ = json.Unmarshal(item[m], &op)
			ro := scanRawOp{method: strings.ToLower(m), path: p, id: op.OperationID}
			ro.line = 1
			if pOff >= 0 {
				mq, _ := json.Marshal(m)
				if mo := bytes.Index(data[pOff:], mq); mo >= 0 {
					ro.line = scanLineOfOffset(data, pOff+mo)
				} else {
					ro.line = scanLineOfOffset(data, pOff)
				}
			}
			for code, body := range op.Responses {
				hasSchema := bytes.Contains(body, []byte(`"schema"`)) || bytes.Contains(body, []byte(`"$ref"`))
				if !hasSchema {
					continue
				}
				switch {
				case strings.HasPrefix(code, "2"):
					ro.typed = true
					ro.refs += " " + string(body)
				case strings.HasPrefix(code, "4"), strings.HasPrefix(code, "5"), code == "default":
					ro.errTyped = true
				}
			}
			for _, prm := range op.Parameters {
				if bytes.Contains(bytes.ToLower(prm), []byte("idempotency-key")) {
					ro.idem = true
				}
			}
			ops = append(ops, ro)
		}
	}
	return ops, true
}

func scanParseOpenAPIYAML(lines []string) []scanRawOp {
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "paths:") {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var ops []scanRawOp
	var cur *scanRawOp
	curPath := ""
	pathIndent, methodIndent := -1, -1
	inResp := false
	respIndent := 0
	code := ""
	closeOp := func() {
		if cur != nil {
			ops = append(ops, *cur)
			cur = nil
		}
	}
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if scanBlankOrComment(l) {
			continue
		}
		ind := scanIndent(l)
		if ind < 0 {
			continue
		}
		if ind == 0 {
			break
		}
		t := strings.TrimSpace(l)
		if pathIndent < 0 {
			pathIndent = ind
		}
		if ind == pathIndent {
			closeOp()
			inResp, code = false, ""
			if m := scanYAMLPathRE.FindStringSubmatch(t); m != nil {
				curPath = m[1] + m[2] + m[3]
			} else {
				curPath = ""
			}
			methodIndent = -1
			continue
		}
		if curPath == "" || ind < pathIndent {
			continue
		}
		key, _, _ := strings.Cut(t, ":")
		lk := strings.ToLower(strings.Trim(key, `"'`))
		if methodIndent < 0 && scanHTTPMethods[lk] {
			methodIndent = ind
		}
		if ind == methodIndent && scanHTTPMethods[lk] {
			closeOp()
			cur = &scanRawOp{method: lk, path: curPath, line: i + 1}
			inResp, code = false, ""
			continue
		}
		if cur == nil || ind <= methodIndent {
			continue
		}
		if strings.Contains(strings.ToLower(t), "idempotency-key") {
			cur.idem = true
		}
		switch {
		case lk == "operationid":
			cur.id = strings.Trim(strings.TrimSpace(t[strings.Index(t, ":")+1:]), `"'`)
		case lk == "responses":
			inResp, respIndent, code = true, ind, ""
		case inResp && ind <= respIndent:
			inResp = false
		case inResp:
			if m := scanYAMLCodeRE.FindStringSubmatch(t); m != nil {
				code = m[1]
				continue
			}
			if lk == "schema" || lk == "$ref" || lk == "content" {
				switch {
				case strings.HasPrefix(code, "2"):
					cur.typed = true
					cur.refs += " " + t
				case strings.HasPrefix(code, "4"), strings.HasPrefix(code, "5"), code == "default":
					cur.errTyped = true
				}
			}
			if lk == "$ref" || strings.Contains(t, "$ref") {
				if strings.HasPrefix(code, "2") {
					cur.refs += " " + t
				}
			}
		}
	}
	closeOp()
	return ops
}

var (
	scanToolOpenRE     = regexp.MustCompile(`(?:registerTool|\.tool|\.AddTool|add_tool|addTool)\(\s*$`)
	scanToolNameLineRE = regexp.MustCompile("^\\s*[\"'" + "`" + "]([A-Za-z0-9_.:\\-]{1,64})[\"'" + "`" + "]")
)
