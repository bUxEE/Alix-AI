package localai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/config"
	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// fanout sends every record to all handlers.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (f fanout) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (f fanout) WithGroup(n string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(n)
	}
	return out
}

const (
	promptCanary     = "PROMPT-CANARY-5d21"
	completionCanary = "COMPLETION-CANARY-91ab"
)

// SEC-082
// SEC-083
// The API key never appears in a log line, an error, a
// formatted value or a returned detail, on success and on every failure path,
// even when the runtime echoes the Authorization header back in its error
// bodies. Prompts and completions are not logged either.
func TestAPIKeyNeverLogged(t *testing.T) {
	key := newTestKey(t)
	t.Setenv("ALIXAI_TEST_LOGGED_KEY", key)

	var authedOK atomic.Int32
	echo := "Authorization: Bearer " + key + " api_key=" + key
	mux := http.NewServeMux()
	checkAuth := func(r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+key {
			authedOK.Add(1)
		}
	}
	mux.HandleFunc("/readyz", readyOK)
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) { checkAuth(r); serveJSON(200, versionBody)(w, r) })
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { checkAuth(r); serveJSON(200, modelsBody)(w, r) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(r)
		var in struct {
			Stream   bool `json:"stream"`
			Messages []struct{ Content string }
		}
		_ = decodeJSONBody(r, &in)
		mode := in.Messages[len(in.Messages)-1].Content
		errJSON := func(status int) {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": "bad credentials " + echo}})
			serveJSON(status, string(b))(w, r)
		}
		switch {
		case strings.HasPrefix(mode, "401"):
			errJSON(401)
		case strings.HasPrefix(mode, "500"):
			errJSON(500)
		case strings.HasPrefix(mode, "event") && in.Stream:
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "boom " + echo, "type": "server_error"}})
			sseHandler(string(b), "[DONE]")(w, r)
		case in.Stream:
			sseHandler(streamChunk(completionCanary, ""), streamChunk("", "stop"), usageTrailer, "[DONE]")(w, r)
		default:
			serveJSON(200, chatBody(completionCanary, "stop"))(w, r)
		}
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		checkAuth(r)
		var in struct{ Input []string }
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &in)
		r.Body = io.NopCloser(bytes.NewReader(raw)) // embedHandler reads it again
		if strings.HasPrefix(in.Input[0], "500") {
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": "bad " + echo}})
			serveJSON(500, string(b))(w, r)
			return
		}
		embedHandler(5)(w, r)
	})
	rt := newRuntime(t, mux.ServeHTTP)

	var redactedLog, rawLog bytes.Buffer
	logger := slog.New(fanout{
		alog.NewHandler(config.LogConfig{Level: "debug"}, &redactedLog),
		slog.NewTextHandler(&rawLog, &slog.HandlerOptions{Level: slog.LevelDebug}), // no redaction layer: the adapter itself must be clean
	})

	// The key arrives through a secret reference, as in production.
	cfg := testConfig(rt.URL)
	cfg.APIKeyRef = "env:ALIXAI_TEST_LOGGED_KEY"
	c, err := New(cfg, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	user := func(mode string) providers.ChatRequest { return chatReq(mode + " " + promptCanary) }

	if rep, err := c.Health(bg); err != nil || rep.Status != providers.HealthOK {
		t.Fatalf("Health = %+v, %v", rep, err)
	}
	if _, err := c.Chat(bg, testScope(), user("ok")); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"401", "500"} {
		_, err := c.Chat(bg, testScope(), user(mode))
		wantCode(t, err, contracts.ErrInternal)
		note(err)
		_, err = c.ChatStream(bg, testScope(), user(mode))
		wantCode(t, err, contracts.ErrInternal)
		note(err)
	}
	s, err := c.ChatStream(bg, testScope(), user("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := drain(t, s); err != io.EOF || done == nil {
		t.Fatalf("stream: %v", err)
	}
	s, err = c.ChatStream(bg, testScope(), user("event"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = drain(t, s)
	wantCode(t, err, contracts.ErrInternal)
	note(err)
	if _, err := c.Embed(bg, testScope(), providers.EmbedRequest{Inputs: []string{"fine " + promptCanary}}); err != nil {
		t.Fatal(err)
	}
	_, err = c.Embed(bg, testScope(), providers.EmbedRequest{Inputs: []string{"500 " + promptCanary}})
	wantCode(t, err, contracts.ErrInternal)
	note(err)
	_, err = c.exchange(bg, "POST", "/models/apply", map[string]string{"id": "x"}, "application/json")
	note(err)

	// A client whose runtime is gone, and one whose configuration is refused.
	rt2 := newRuntime(t, serveJSON(200, "{}"))
	c2, _ := New(testConfig(rt2.URL), WithAPIKey(NewSecret(key)), WithLogger(logger))
	rt2.Close()
	_, err = c2.Chat(bg, testScope(), user("ok"))
	wantCode(t, err, contracts.ErrUnavailable)
	note(err)
	_, err = New(Config{Endpoint: "http://10.0.0.8:8080", APIKeyRef: "env:ALIXAI_TEST_LOGGED_KEY"})
	note(err)
	_, err = New(Config{APIKeyRef: key}) // a pasted literal key
	note(err)

	// Every error, in every rendering.
	for i, e := range errs {
		var ce *contracts.Error
		rendered := []string{e.Error(), fmt.Sprintf("%v|%+v|%#v|%s", e, e, e, e)}
		if errors.As(e, &ce) {
			j, _ := json.Marshal(ce)
			rendered = append(rendered, string(j), fmt.Sprint(ce.Details))
		}
		for _, r := range rendered {
			if strings.Contains(r, key) {
				t.Errorf("error %d leaks the key: %.200s", i, r)
			}
			if strings.Contains(r, promptCanary) || strings.Contains(r, completionCanary) {
				t.Errorf("error %d leaks prompt or completion text: %.200s", i, r)
			}
		}
	}
	// The values themselves, however they are formatted.
	for name, v := range map[string]any{"client": c, "client %+v": *c, "config": c.cfg, "transport": c.hc.Transport, "http client": c.hc} {
		for _, f := range []string{"%v", "%+v", "%#v"} {
			if out := fmt.Sprintf(f, v); strings.Contains(out, key) {
				t.Errorf("%s printed with %s leaks the key", name, f)
			}
		}
	}

	if n := authedOK.Load(); n < 8 {
		t.Fatalf("the runtime saw the key on only %d authenticated requests: the test does not exercise the key", n)
	}
	for name, out := range map[string]string{"redacting logger": redactedLog.String(), "plain logger": rawLog.String()} {
		if out == "" {
			t.Fatalf("%s received nothing: the test does not exercise logging", name)
		}
		if strings.Contains(out, key) {
			t.Errorf("%s output contains the key", name)
		}
		for _, canary := range []string{promptCanary, completionCanary} {
			if strings.Contains(out, canary) {
				t.Errorf("%s output contains prompt or completion text", name)
			}
		}
		for _, want := range []string{"/v1/chat/completions", "tok_in"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output lacks %q: counters must be logged under tok_in/tok_out, which the redactor leaves alone", name, want)
			}
		}
		if strings.Contains(strings.ToLower(out), "authorization") || strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s output mentions credentials at all: %s", name, out)
		}
	}
}
