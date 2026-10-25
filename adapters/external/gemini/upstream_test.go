package gemini

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

// fixtureFS holds the recorded Gemini generateContent fixtures: whole response
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
		panic(fmt.Sprintf("gemini: missing fixture %s", name))
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

// fakeUpstream is a loopback HTTP server that plays the Gemini service from the
// recorded fixtures, one scenario per request. It implements suite.Upstream.
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
	if r.Method == http.MethodGet && r.URL.Path == "/v1beta/models" {
		writeJSON(w, http.StatusForbidden, render("models_unauthorized.json", map[string]string{"MARKER": suite.UpstreamMarker}))
		return
	}
	isStream := strings.HasSuffix(r.URL.Path, ":streamGenerateContent") && r.URL.RawQuery == "alt=sse"
	isGen := strings.HasSuffix(r.URL.Path, ":generateContent") && r.URL.RawQuery == ""
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1beta/models/") || (!isStream && !isGen) {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	seen := parseRequest(r, body, isStream)

	u.mu.Lock()
	u.seen = append(u.seen, seen)
	var sc suite.Scenario
	have := len(u.queue) > 0
	if have {
		sc, u.queue = u.queue[0], u.queue[1:]
	}
	n := len(u.seen)
	u.mu.Unlock()

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
	delay := ""
	if sc.RetryAfter > 0 {
		delay = strconv.FormatFloat(sc.RetryAfter.Seconds(), 'f', -1, 64) + "s"
	}
	kv := map[string]string{
		"IN": strconv.Itoa(in), "OUT": strconv.Itoa(out), "TOTAL": strconv.Itoa(in + out), "TEXT": jstr(sc.Text),
		"MARKER": suite.UpstreamMarker, "KEY": seen.Key, "RID": "FixtureResp" + strconv.Itoa(n), "RETRY_DELAY": delay,
		"CALL_ID": jstr(sc.Call.ID), "NAME": jstr(sc.Call.Name), "ARGS_RAW": args, "FINISH": "STOP",
	}
	if isStream {
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
		writeJSON(w, 400, render("error_auth.json", kv)) // an invalid key is a 400 with reason API_KEY_INVALID
	case suite.ScQuota:
		writeJSON(w, 429, render("error_quota.json", kv))
	case suite.ScRateLimit:
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
		writeJSON(w, 400, render("error_auth.json", kv))
		return
	case suite.ScQuota:
		writeJSON(w, 429, render("error_quota.json", kv))
		return
	case suite.ScRateLimit:
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
		emit("stream_tool.sse", nil)
	case suite.ScHostedTool:
		emit("stream_hosted_tool.sse", nil)
	case suite.ScMalformed:
		_, _ = io.WriteString(w, "data: {\"candidates\": [{\""+suite.UpstreamMarker+"\n\n")
		flush()
	case suite.ScStall, suite.ScTruncated:
		emit("stream_text.sse", map[string]string{"TEXT": kv["TEXT"]})
		if sc.Kind == suite.ScStall {
			select {
			case <-r.Context().Done():
			case <-rel:
			}
		}
		// the connection ends here, with no finishReason
	default:
		rs := []rune(sc.Text)
		for i := 0; i < len(rs); i += 8 {
			emit("stream_text.sse", map[string]string{"TEXT": jstr(string(rs[i:min(i+8, len(rs))]))})
		}
		emit("stream_final.sse", nil)
	}
}

// parseRequest maps a generateContent request to what the suite asserts on.
func parseRequest(r *http.Request, raw []byte, isStream bool) suite.Seen {
	s := suite.Seen{Key: r.Header.Get("x-goog-api-key"), Stream: isStream}
	if a := r.Header.Get("Authorization"); a != "" {
		s.Key = strings.TrimPrefix(a, "Bearer ")
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		s.Body = string(raw)
		return s
	}
	for _, k := range []string{"cachedContent", "safetySettings", "labels", "serviceTier", "store"} {
		if _, ok := body[k]; ok {
			s.Native = append(s.Native, k)
		}
	}
	if gc, ok := body["generationConfig"].(map[string]any); ok {
		if v, ok := gc["maxOutputTokens"].(float64); ok {
			s.MaxOutputToks = int(v)
		}
		_, s.HasSchema = gc["responseJsonSchema"]
		for _, k := range []string{"thinkingConfig", "responseModalities", "speechConfig", "mediaResolution", "responseSchema"} {
			if _, ok := gc[k]; ok {
				s.Native = append(s.Native, k)
			}
		}
	}
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			m, _ := t.(map[string]any)
			for k, v := range m {
				if k != "functionDeclarations" {
					s.Native = append(s.Native, "tool:"+k) // googleSearch, codeExecution, urlContext, fileSearch, ...
					continue
				}
				decls, _ := v.([]any)
				for _, d := range decls {
					dm, _ := d.(map[string]any)
					name, _ := dm["name"].(string)
					s.Tools = append(s.Tools, suite.SeenTool{Name: name, Kind: "function"})
				}
			}
		}
		if tc, ok := body["toolConfig"].(map[string]any); ok {
			if fc, ok := tc["functionCallingConfig"].(map[string]any); ok {
				mode, _ := fc["mode"].(string)
				s.ToolChoice = strings.ToLower(mode)
			}
		}
	}
	if si, ok := body["systemInstruction"].(map[string]any); ok {
		parts, _ := si["parts"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			t, _ := pm["text"].(string)
			s.System = append(s.System, t)
		}
	}
	contents, _ := body["contents"].([]any)
	for _, cc := range contents {
		c, _ := cc.(map[string]any)
		role, _ := c["role"].(string)
		if role == "model" {
			role = "assistant"
		}
		parts, _ := c["parts"].([]any)
		for _, pp := range parts {
			p, _ := pp.(map[string]any)
			switch {
			case p["functionCall"] != nil:
				fc, _ := p["functionCall"].(map[string]any)
				id, _ := fc["id"].(string)
				name, _ := fc["name"].(string)
				in, _ := json.Marshal(fc["args"])
				s.Items = append(s.Items, suite.SeenItem{Role: "assistant", CallID: id, Name: name, Text: string(in)})
			case p["functionResponse"] != nil:
				fr, _ := p["functionResponse"].(map[string]any)
				id, _ := fr["id"].(string)
				resp, _ := fr["response"].(map[string]any)
				t, _ := resp["result"].(string)
				s.Items = append(s.Items, suite.SeenItem{Role: "tool", CallID: id, Text: t})
			case p["text"] != nil:
				t, _ := p["text"].(string)
				s.Items = append(s.Items, suite.SeenItem{Role: role, Text: t})
			default:
				for k := range p {
					s.Media = append(s.Media, k) // inlineData, fileData, ...
				}
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
