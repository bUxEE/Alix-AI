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

// anthropicFixtureFS holds the recorded Anthropic Messages API fixtures (the testdata of adapters/external/anthropic): whole response
// bodies, error bodies and the server-sent events of a stream, as templates
// with {{PLACEHOLDER}} members. They were written from the vendor's documented
// shapes (see doc.go) and are replayed by a loopback server; no fixture holds a
// key.
//
//go:embed fixtures/anthropic/*
var anthropicFixtureFS embed.FS

func anthropicFixture(name string) string {
	b, err := anthropicFixtureFS.ReadFile("fixtures/anthropic/" + name)
	if err != nil {
		panic(fmt.Sprintf("providerconf: missing fixture %s", name))
	}
	return string(b)
}

// render fills the placeholders of a fixture; values are JSON-encoded where the
// template expects a JSON value.
func anthropicRender(name string, kv map[string]string) string {
	pairs := make([]string, 0, len(kv)*2)
	for k, v := range kv {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(anthropicFixture(name))
}

// anthropicUpstream is a loopback HTTP server that plays the Anthropic service from
// the recorded fixtures, one scenario per request. It implements
// suite.Upstream.
type anthropicUpstream struct {
	srv *http.Server
	ln  net.Listener
	URL string

	mu    sync.Mutex
	queue []suite.Scenario
	seen  []suite.Seen
	down  bool
	rel   chan struct{}
}

var _ suite.Upstream = (*anthropicUpstream)(nil)

func newAnthropicUpstream() (*anthropicUpstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &anthropicUpstream{ln: ln, URL: "http://" + ln.Addr().String(), rel: make(chan struct{})}
	u.srv = &http.Server{Handler: http.HandlerFunc(u.handle), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = u.srv.Serve(ln) }()
	return u, nil
}

// Close stops the server.
func (u *anthropicUpstream) Close() {
	u.Release()
	_ = u.srv.Close()
}

// Script implements suite.Upstream.
func (u *anthropicUpstream) Script(sc ...suite.Scenario) {
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
func (u *anthropicUpstream) Seen() []suite.Seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]suite.Seen(nil), u.seen...)
}

// SetDown implements suite.Upstream.
func (u *anthropicUpstream) SetDown(down bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.down = down
}

// Release implements suite.Upstream.
func (u *anthropicUpstream) Release() {
	u.mu.Lock()
	defer u.mu.Unlock()
	close(u.rel)
	u.rel = make(chan struct{})
}

func (u *anthropicUpstream) drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (u *anthropicUpstream) handle(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	down, rel := u.down, u.rel
	u.mu.Unlock()
	if down {
		u.drop(w)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		writeJSON(w, http.StatusUnauthorized, anthropicRender("models_unauthorized.json", map[string]string{"MARKER": suite.UpstreamMarker}))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	seen := anthropicParseRequest(r, body)

	u.mu.Lock()
	u.seen = append(u.seen, seen)
	var sc suite.Scenario
	have := len(u.queue) > 0
	if have {
		sc, u.queue = u.queue[0], u.queue[1:]
	}
	n := len(u.seen)
	u.mu.Unlock()

	w.Header().Set("Request-Id", "req_fixture"+strconv.Itoa(n))
	if !have {
		writeJSON(w, http.StatusInternalServerError, anthropicRender("error_no_script.json", map[string]string{"MARKER": suite.UpstreamMarker}))
		return
	}
	in, out := sc.ToksIn, sc.ToksOut
	if in == 0 && out == 0 {
		in, out = 50, 10
	}
	args := sc.Call.Args
	if args == "" {
		args = "{}"
	}
	kv := map[string]string{
		"IN": strconv.Itoa(in), "OUT": strconv.Itoa(out), "TEXT": jstr(sc.Text), "MARKER": suite.UpstreamMarker, "KEY": seen.Key,
		"CALL_ID": jstr(sc.Call.ID), "NAME": jstr(sc.Call.Name), "ARGS_RAW": args,
	}
	if seen.Stream {
		u.stream(w, r, sc, kv, rel)
		return
	}
	switch sc.Kind {
	case suite.ScText, suite.ScJSON:
		writeJSON(w, 200, anthropicRender("text.json", kv))
	case suite.ScToolCall:
		writeJSON(w, 200, anthropicRender("tool_call.json", kv))
	case suite.ScHostedTool:
		writeJSON(w, 200, anthropicRender("hosted_tool.json", kv))
	case suite.ScActiveMedia:
		writeJSON(w, 200, anthropicRender("active_media.json", kv))
	case suite.ScMalformed, suite.ScMedia:
		writeJSON(w, 200, anthropicRender("malformed.json", kv))
	case suite.ScServerError:
		writeJSON(w, 500, anthropicRender("error_server.json", kv))
	case suite.ScAuthError:
		writeJSON(w, 401, anthropicRender("error_auth.json", kv))
	case suite.ScQuota:
		writeJSON(w, 429, anthropicRender("error_quota.json", kv)) // the spend-cap shape: no retry-after
	case suite.ScRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(int(sc.RetryAfter/time.Second)))
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "0")
		w.Header().Set("Anthropic-Ratelimit-Requests-Reset", time.Now().Add(sc.RetryAfter).UTC().Format(time.RFC3339))
		writeJSON(w, 429, anthropicRender("error_rate_limit.json", kv))
	case suite.ScStall:
		select {
		case <-r.Context().Done():
		case <-rel:
		}
		u.drop(w)
	case suite.ScTruncated:
		u.drop(w)
	default:
		writeJSON(w, 500, anthropicRender("error_no_script.json", kv))
	}
}

func (u *anthropicUpstream) stream(w http.ResponseWriter, r *http.Request, sc suite.Scenario, kv map[string]string, rel chan struct{}) {
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	// a failure before the stream starts is a plain HTTP error, as the service answers it
	switch sc.Kind {
	case suite.ScServerError:
		writeJSON(w, 500, anthropicRender("error_server.json", kv))
		return
	case suite.ScAuthError:
		writeJSON(w, 401, anthropicRender("error_auth.json", kv))
		return
	case suite.ScQuota:
		writeJSON(w, 429, anthropicRender("error_quota.json", kv))
		return
	case suite.ScRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(int(sc.RetryAfter/time.Second)))
		writeJSON(w, 429, anthropicRender("error_rate_limit.json", kv))
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
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	emit := func(name string, extra map[string]string) {
		m := map[string]string{}
		for k, v := range kv {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		_, _ = io.WriteString(w, anthropicRender(name, m))
		flush()
	}
	switch sc.Kind {
	case suite.ScToolCall:
		emit("stream_start.sse", nil)
		emit("stream_tool_open.sse", nil)
		args := sc.Call.Args
		for i := 0; i < len(args); i += 12 {
			emit("stream_tool_delta.sse", map[string]string{"ARGS_DELTA": jstr(args[i:min(i+12, len(args))])})
		}
		emit("stream_block_stop.sse", nil)
		emit("stream_end.sse", map[string]string{"STOP": jstr("tool_use")})
	case suite.ScHostedTool:
		emit("stream_start.sse", nil)
		emit("stream_hosted_tool.sse", nil)
	case suite.ScMalformed:
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\": \"message_start\", \""+suite.UpstreamMarker+"\n\n")
		flush()
	case suite.ScStall, suite.ScTruncated:
		emit("stream_start.sse", nil)
		emit("stream_text_open.sse", nil)
		emit("stream_text_delta.sse", map[string]string{"TEXT": kv["TEXT"]})
		if sc.Kind == suite.ScStall {
			select {
			case <-r.Context().Done():
			case <-rel:
			}
		}
		// the connection ends here, with no message_stop
	default:
		emit("stream_start.sse", nil)
		emit("stream_text_open.sse", nil)
		rs := []rune(sc.Text)
		for i := 0; i < len(rs); i += 8 {
			emit("stream_text_delta.sse", map[string]string{"TEXT": jstr(string(rs[i:min(i+8, len(rs))]))})
		}
		emit("stream_block_stop.sse", nil)
		emit("stream_end.sse", map[string]string{"STOP": jstr("end_turn")})
	}
}

// parseRequest maps a Messages API request to what the suite asserts on.
func anthropicParseRequest(r *http.Request, raw []byte) suite.Seen {
	s := suite.Seen{Key: r.Header.Get("x-api-key")}
	if r.Header.Get("anthropic-beta") != "" {
		s.Native = append(s.Native, "beta:"+r.Header.Get("anthropic-beta"))
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		s.Body = string(raw)
		return s
	}
	s.Stream, _ = body["stream"].(bool)
	if v, ok := body["max_tokens"].(float64); ok {
		s.MaxOutputToks = int(v)
	}
	for _, k := range []string{"thinking", "metadata", "service_tier", "container", "mcp_servers", "context_management", "cache_control", "speed", "fallbacks"} {
		if _, ok := body[k]; ok {
			s.Native = append(s.Native, k)
		}
	}
	if oc, ok := body["output_config"].(map[string]any); ok {
		if f, ok := oc["format"].(map[string]any); ok && f["type"] == "json_schema" {
			s.HasSchema = true
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			m, _ := t.(map[string]any)
			name, _ := m["name"].(string)
			kind, _ := m["type"].(string)
			if kind == "" || kind == "custom" {
				kind = "function" // a custom tool is the function tool of this API
			}
			s.Tools = append(s.Tools, suite.SeenTool{Name: name, Kind: kind})
		}
		if tc, ok := body["tool_choice"].(map[string]any); ok {
			s.ToolChoice, _ = tc["type"].(string)
		}
	}
	texts := func(content any) string {
		if str, ok := content.(string); ok {
			return str
		}
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
	if sys, ok := body["system"]; ok {
		if l, ok := sys.([]any); ok {
			for _, b := range l {
				s.System = append(s.System, texts([]any{b}))
			}
		} else {
			s.System = append(s.System, texts(sys))
		}
	}
	msgs, _ := body["messages"].([]any)
	for _, mm := range msgs {
		m, _ := mm.(map[string]any)
		role, _ := m["role"].(string)
		blocks, ok := m["content"].([]any)
		if !ok {
			s.Items = append(s.Items, suite.SeenItem{Role: role, Text: texts(m["content"])})
			continue
		}
		for _, bb := range blocks {
			b, _ := bb.(map[string]any)
			switch typ, _ := b["type"].(string); typ {
			case "tool_use":
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				in, _ := json.Marshal(b["input"])
				s.Items = append(s.Items, suite.SeenItem{Role: "assistant", CallID: id, Name: name, Text: string(in)})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				s.Items = append(s.Items, suite.SeenItem{Role: "tool", CallID: id, Text: texts(b["content"])})
			case "text":
				t, _ := b["text"].(string)
				s.Items = append(s.Items, suite.SeenItem{Role: role, Text: t})
			default:
				s.Media = append(s.Media, typ)
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
