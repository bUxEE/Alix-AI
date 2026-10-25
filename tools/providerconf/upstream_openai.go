package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	suite "github.com/buxee/alix-ai/tests/conformance/providers"
)

// fixtureFS holds the recorded OpenAI Responses API fixtures: whole response
// bodies, error bodies and the server-sent events of a stream, as templates
// with {{PLACEHOLDER}} members. They were written from the vendor's documented
// shapes (see adapters/external/openai doc.go) and replayed by a loopback
// server; no fixture holds a key.
//
//go:embed fixtures/openai/*
var fixtureFS embed.FS

func fixture(name string) string {
	b, err := fixtureFS.ReadFile("fixtures/openai/" + name)
	if err != nil {
		panic(fmt.Sprintf("providerconf: missing fixture %s", name))
	}
	return string(b)
}

// render fills the placeholders of a fixture; values are JSON-encoded where
// the template expects a JSON string.
func render(name string, kv map[string]string) string {
	s := fixture(name)
	pairs := make([]string, 0, len(kv)*2)
	for k, v := range kv {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

func jstr(s string) string { b, _ := json.Marshal(s); return string(b) }

// openaiUpstream is a loopback HTTP server that plays the OpenAI service from
// the recorded fixtures, one scenario per request. It implements
// suite.Upstream.
type openaiUpstream struct {
	srv *http.Server
	ln  net.Listener
	URL string

	// forward, when set (live mode only), answers the natural scenarios
	// (text, structured output, tool call) from the real service; every
	// fault scenario still comes from the recorded fixtures.
	forward func(w http.ResponseWriter, r *http.Request, body []byte)

	mu    sync.Mutex
	queue []suite.Scenario
	seen  []suite.Seen
	down  bool
	rel   chan struct{}
}

var _ suite.Upstream = (*openaiUpstream)(nil)

func newOpenAIUpstream() (*openaiUpstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &openaiUpstream{ln: ln, URL: "http://" + ln.Addr().String(), rel: make(chan struct{})}
	u.srv = &http.Server{Handler: http.HandlerFunc(u.handle), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = u.srv.Serve(ln) }()
	return u, nil
}

// Close stops the server.
func (u *openaiUpstream) Close() {
	u.Release()
	_ = u.srv.Close()
}

// Script implements suite.Upstream.
func (u *openaiUpstream) Script(sc ...suite.Scenario) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.queue = nil
	for _, s := range sc {
		n := s.Times
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			u.queue = append(u.queue, s)
		}
	}
}

// Seen implements suite.Upstream.
func (u *openaiUpstream) Seen() []suite.Seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]suite.Seen(nil), u.seen...)
}

// SetDown implements suite.Upstream.
func (u *openaiUpstream) SetDown(down bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.down = down
}

// Release implements suite.Upstream.
func (u *openaiUpstream) Release() {
	u.mu.Lock()
	defer u.mu.Unlock()
	close(u.rel)
	u.rel = make(chan struct{})
}

func (u *openaiUpstream) drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (u *openaiUpstream) handle(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	down, rel := u.down, u.rel
	u.mu.Unlock()
	if down {
		u.drop(w)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, render("models_unauthorized.json", map[string]string{"MARKER": suite.UpstreamMarker}))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	seen := parseRequest(r, body)

	u.mu.Lock()
	u.seen = append(u.seen, seen)
	var sc suite.Scenario
	have := len(u.queue) > 0
	if have {
		sc, u.queue = u.queue[0], u.queue[1:]
	}
	n := len(u.seen)
	u.mu.Unlock()

	w.Header().Set("X-Request-Id", "req_"+strconv.Itoa(n))
	if !have {
		writeJSON(w, http.StatusInternalServerError, render("error_no_script.json", map[string]string{"MARKER": suite.UpstreamMarker}))
		return
	}
	if u.forward != nil && (sc.Kind == suite.ScText || sc.Kind == suite.ScJSON || sc.Kind == suite.ScToolCall) {
		u.forward(w, r, body)
		return
	}
	in, out := sc.ToksIn, sc.ToksOut
	if in == 0 && out == 0 {
		in, out = 50, 10
	}
	kv := map[string]string{
		"IN": strconv.Itoa(in), "OUT": strconv.Itoa(out), "TEXT": jstr(sc.Text), "MARKER": suite.UpstreamMarker, "KEY": seen.Key,
		"CALL_ID": jstr(sc.Call.ID), "NAME": jstr(sc.Call.Name), "ARGS": jstr(sc.Call.Args),
	}
	if seen.Stream {
		u.stream(w, r, sc, kv, rel)
		return
	}
	switch sc.Kind {
	case suite.ScText, suite.ScJSON:
		writeJSON(w, 200, render("text.json", kv))
	case suite.ScToolCall:
		writeJSON(w, 200, render("tool_call.json", kv))
	case suite.ScHostedTool:
		writeJSON(w, 200, render("hosted_tool.json", kv))
	case suite.ScActiveMedia:
		writeJSON(w, 200, render("active_media.json", kv))
	case suite.ScMalformed, suite.ScMedia:
		writeJSON(w, 200, render("malformed.json", kv))
	case suite.ScServerError:
		writeJSON(w, 500, render("error_server.json", kv))
	case suite.ScAuthError:
		writeJSON(w, 401, render("error_auth.json", kv))
	case suite.ScQuota:
		writeJSON(w, 429, render("error_quota.json", kv))
	case suite.ScRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(int(sc.RetryAfter/time.Second)))
		writeJSON(w, 429, render("error_rate_limit.json", kv))
	case suite.ScStall:
		select {
		case <-r.Context().Done():
		case <-rel:
		}
		u.drop(w)
	case suite.ScTruncated:
		u.drop(w)
	default:
		writeJSON(w, 500, render("error_no_script.json", kv))
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (u *openaiUpstream) stream(w http.ResponseWriter, r *http.Request, sc suite.Scenario, kv map[string]string, rel chan struct{}) {
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	// a failure before the stream starts is a plain HTTP error, as the service answers it
	switch sc.Kind {
	case suite.ScServerError:
		writeJSON(w, 500, render("error_server.json", kv))
		return
	case suite.ScAuthError:
		writeJSON(w, 401, render("error_auth.json", kv))
		return
	case suite.ScQuota:
		writeJSON(w, 429, render("error_quota.json", kv))
		return
	case suite.ScRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(int(sc.RetryAfter/time.Second)))
		writeJSON(w, 429, render("error_rate_limit.json", kv))
		return
	case suite.ScStall:
		if sc.Text == "" {
			select {
			case <-r.Context().Done():
			case <-rel:
			}
			u.drop(w)
			return
		}
	case suite.ScTruncated:
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	emit := func(name string, kv map[string]string) {
		_, _ = io.WriteString(w, render(name, kv))
		flush()
	}
	switch sc.Kind {
	case suite.ScToolCall:
		emit("stream_call_open.sse", kv)
		args := sc.Call.Args
		for i := 0; i < len(args); i += 12 {
			emit("stream_call_delta.sse", map[string]string{"ARGS_DELTA": jstr(args[i:min(i+12, len(args))])})
		}
		emit("stream_completed.sse", kv)
	case suite.ScHostedTool:
		emit("stream_open.sse", kv)
		emit("stream_hosted_tool.sse", kv)
	case suite.ScMalformed:
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\": \"response.created\", \""+suite.UpstreamMarker+"\n\n")
		flush()
	case suite.ScStall, suite.ScTruncated:
		emit("stream_open.sse", kv)
		emit("stream_delta.sse", map[string]string{"TEXT": kv["TEXT"]})
		if sc.Kind == suite.ScStall {
			select {
			case <-r.Context().Done():
			case <-rel:
			}
		}
		// the connection ends here, with no response.completed
	default:
		emit("stream_open.sse", kv)
		rs := []rune(sc.Text)
		for i := 0; i < len(rs); i += 8 {
			emit("stream_delta.sse", map[string]string{"TEXT": jstr(string(rs[i:min(i+8, len(rs))]))})
		}
		emit("stream_completed.sse", kv)
	}
}

// parseRequest maps a Responses API request to what the suite asserts on.
func parseRequest(r *http.Request, raw []byte) suite.Seen {
	s := suite.Seen{Key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		s.Body = string(raw)
		return s
	}
	s.Stream, _ = body["stream"].(bool)
	if v, ok := body["max_output_tokens"].(float64); ok {
		s.MaxOutputToks = int(v)
	}
	if v, ok := body["store"].(bool); ok && v {
		s.Native = append(s.Native, "store")
	}
	for _, k := range []string{"previous_response_id", "conversation", "background", "include", "prompt", "max_tool_calls", "metadata", "user"} {
		if _, ok := body[k]; ok {
			s.Native = append(s.Native, k)
		}
	}
	if txt, ok := body["text"].(map[string]any); ok {
		if f, ok := txt["format"].(map[string]any); ok && f["type"] == "json_schema" {
			s.HasSchema = true
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			m, _ := t.(map[string]any)
			kind, _ := m["type"].(string)
			name, _ := m["name"].(string)
			s.Tools = append(s.Tools, suite.SeenTool{Name: name, Kind: kind})
			if kind != "function" {
				s.Native = append(s.Native, "hosted_tool:"+kind)
			}
		}
		s.ToolChoice, _ = body["tool_choice"].(string)
	}
	text := func(content any) string {
		var parts []string
		if l, ok := content.([]any); ok {
			for _, c := range l {
				if m, ok := c.(map[string]any); ok {
					if t, ok := m["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	items, _ := body["input"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		switch typ, _ := m["type"].(string); {
		case typ == "function_call":
			id, _ := m["call_id"].(string)
			name, _ := m["name"].(string)
			args, _ := m["arguments"].(string)
			s.Items = append(s.Items, suite.SeenItem{Role: "assistant", CallID: id, Name: name, Text: args})
		case typ == "function_call_output":
			id, _ := m["call_id"].(string)
			out, _ := m["output"].(string)
			s.Items = append(s.Items, suite.SeenItem{Role: "tool", CallID: id, Text: out})
		default:
			role, _ := m["role"].(string)
			switch role {
			case "developer", "system":
				s.System = append(s.System, text(m["content"]))
			default:
				s.Items = append(s.Items, suite.SeenItem{Role: role, Text: text(m["content"])})
			}
		}
	}
	var sb strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			sb.WriteString(x)
			sb.WriteByte('\n')
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(body)
	s.Body = sb.String()
	return s
}
