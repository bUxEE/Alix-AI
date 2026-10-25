package meta

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

// fixtureFS holds the recorded OpenAI-compatible Chat Completions fixtures: whole response
// bodies, error bodies and the server-sent events of a stream, as templates
// with {{PLACEHOLDER}} members. They were written from the vendor's documented
// shapes (see doc.go) and are replayed by a loopback server; no fixture holds a
// key.
//
//go:embed testdata/*
var fixtureFS embed.FS

func fixture(name string) string {
	b, err := fixtureFS.ReadFile("testdata/" + name)
	if err != nil {
		panic(fmt.Sprintf("meta-llama: missing fixture %s", name))
	}
	return string(b)
}

// render fills the placeholders of a fixture; values are JSON-encoded where the
// template expects a JSON value.
func render(name string, kv map[string]string) string {
	pairs := make([]string, 0, len(kv)*2)
	for k, v := range kv {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(fixture(name))
}

func jstr(s string) string { b, _ := json.Marshal(s); return string(b) }

// fakeUpstream is a loopback HTTP server that plays a hosted Llama service from
// the recorded fixtures, one scenario per request. It implements
// suite.Upstream.
type fakeUpstream struct {
	srv *http.Server
	ln  net.Listener
	URL string

	mu    sync.Mutex
	queue []suite.Scenario
	seen  []suite.Seen
	down  bool
	rel   chan struct{}
}

var _ suite.Upstream = (*fakeUpstream)(nil)

func newFakeUpstream() (*fakeUpstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &fakeUpstream{ln: ln, URL: "http://" + ln.Addr().String(), rel: make(chan struct{})}
	u.srv = &http.Server{Handler: http.HandlerFunc(u.handle), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = u.srv.Serve(ln) }()
	return u, nil
}

// Close stops the server.
func (u *fakeUpstream) Close() {
	u.Release()
	_ = u.srv.Close()
}

// Script implements suite.Upstream.
func (u *fakeUpstream) Script(sc ...suite.Scenario) {
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
func (u *fakeUpstream) Seen() []suite.Seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]suite.Seen(nil), u.seen...)
}

// SetDown implements suite.Upstream.
func (u *fakeUpstream) SetDown(down bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.down = down
}

// Release implements suite.Upstream.
func (u *fakeUpstream) Release() {
	u.mu.Lock()
	defer u.mu.Unlock()
	close(u.rel)
	u.rel = make(chan struct{})
}

func (u *fakeUpstream) drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

func (u *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	down, rel := u.down, u.rel
	u.mu.Unlock()
	if down {
		u.drop(w)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/status" {
		writeJSON(w, http.StatusOK, "{\"status\":\"operational\"}")
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		writeJSON(w, http.StatusUnauthorized, render("models_unauthorized.json", map[string]string{"MARKER": suite.UpstreamMarker}))
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
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

	w.Header().Set("X-Request-Id", "req_fixture"+strconv.Itoa(n))
	if !have {
		writeJSON(w, http.StatusInternalServerError, render("error_no_script.json", map[string]string{"MARKER": suite.UpstreamMarker}))
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
		"CALL_ID": jstr(sc.Call.ID), "NAME": jstr(sc.Call.Name), "ARGS_STR": jstr(args),
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
		writeJSON(w, 429, render("error_quota.json", kv)) // the spend-cap shape: no retry-after
	case suite.ScRateLimit:
		w.Header().Set("Retry-After", strconv.Itoa(int(sc.RetryAfter/time.Second)))
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.Header().Set("X-Ratelimit-Reset-Requests", sc.RetryAfter.String())
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

func (u *fakeUpstream) stream(w http.ResponseWriter, r *http.Request, sc suite.Scenario, kv map[string]string, rel chan struct{}) {
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
		_, _ = io.WriteString(w, render(name, m))
		flush()
	}
	switch sc.Kind {
	case suite.ScToolCall:
		emit("stream_tool_open.sse", nil)
		args := sc.Call.Args
		for i := 0; i < len(args); i += 12 {
			emit("stream_tool_delta.sse", map[string]string{"ARGS_DELTA": jstr(args[i:min(i+12, len(args))])})
		}
		emit("stream_finish.sse", map[string]string{"STOP": jstr("tool_calls")})
		emit("stream_usage.sse", nil)
		emit("stream_done.sse", nil)
	case suite.ScHostedTool:
		emit("stream_first.sse", nil)
		emit("stream_hosted_tool.sse", nil)
	case suite.ScMalformed:
		_, _ = io.WriteString(w, "data: {\"id\": \"x\", \""+suite.UpstreamMarker+"\n\n")
		flush()
	case suite.ScStall, suite.ScTruncated:
		emit("stream_first.sse", nil)
		emit("stream_text_delta.sse", map[string]string{"TEXT": kv["TEXT"]})
		if sc.Kind == suite.ScStall {
			select {
			case <-r.Context().Done():
			case <-rel:
			}
		}
		// the connection ends here, with no finish chunk, no usage and no [DONE]
	default:
		emit("stream_first.sse", nil)
		rs := []rune(sc.Text)
		for i := 0; i < len(rs); i += 8 {
			emit("stream_text_delta.sse", map[string]string{"TEXT": jstr(string(rs[i:min(i+8, len(rs))]))})
		}
		emit("stream_finish.sse", map[string]string{"STOP": jstr("stop")})
		emit("stream_usage.sse", nil)
		emit("stream_done.sse", nil)
	}
}

// parseRequest maps a Chat Completions request to what the suite asserts on.
func parseRequest(r *http.Request, raw []byte) suite.Seen {
	s := suite.Seen{Key: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		s.Body = string(raw)
		return s
	}
	s.Stream, _ = body["stream"].(bool)
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if v, ok := body[k].(float64); ok {
			s.MaxOutputToks = int(v)
		}
	}
	// host-native extras: none is ever sent
	for _, k := range []string{
		"store", "user", "logprobs", "top_logprobs", "service_tier", "reasoning_format", "reasoning", "provider", "models", "route",
		"transforms", "plugins", "metadata", "parallel_tool_calls", "n", "web_search_options", "search_settings", "compound_custom",
		"prediction", "audio", "modalities", "functions", "function_call", "safety_identifier", "prompt_cache_key", "prompt_cache_retention",
		"previous_response_id", "max_tool_calls", "seed", "verbosity",
	} {
		if _, ok := body[k]; ok {
			s.Native = append(s.Native, k)
		}
	}
	if rf, ok := body["response_format"].(map[string]any); ok && rf["type"] == "json_schema" {
		s.HasSchema = true
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			m, _ := t.(map[string]any)
			kind, _ := m["type"].(string)
			fn, _ := m["function"].(map[string]any)
			name, _ := fn["name"].(string)
			s.Tools = append(s.Tools, suite.SeenTool{Name: name, Kind: kind})
		}
		s.ToolChoice, _ = body["tool_choice"].(string)
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
	msgs, _ := body["messages"].([]any)
	for _, mm := range msgs {
		m, _ := mm.(map[string]any)
		role, _ := m["role"].(string)
		if parts, ok := m["content"].([]any); ok {
			for _, pp := range parts {
				if p, ok := pp.(map[string]any); ok {
					if typ, _ := p["type"].(string); typ != "text" {
						s.Media = append(s.Media, typ)
					}
				}
			}
		}
		switch role {
		case "system", "developer":
			s.System = append(s.System, texts(m["content"]))
		case "tool":
			id, _ := m["tool_call_id"].(string)
			s.Items = append(s.Items, suite.SeenItem{Role: "tool", CallID: id, Text: texts(m["content"])})
		case "assistant":
			if t := texts(m["content"]); t != "" {
				s.Items = append(s.Items, suite.SeenItem{Role: "assistant", Text: t})
			}
			calls, _ := m["tool_calls"].([]any)
			for _, cc := range calls {
				c, _ := cc.(map[string]any)
				id, _ := c["id"].(string)
				fn, _ := c["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args, _ := fn["arguments"].(string)
				s.Items = append(s.Items, suite.SeenItem{Role: "assistant", CallID: id, Name: name, Text: args})
			}
		default:
			s.Items = append(s.Items, suite.SeenItem{Role: role, Text: texts(m["content"])})
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
