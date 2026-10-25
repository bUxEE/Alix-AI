package localai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// canary stands for prompt text a runtime error body might quote back. No
// error the adapter returns may contain it.
const canary = "CANARY-PROMPT-TEXT-7f3a"

// TestErrorMapping covers the HTTP failure table of the package
// documentation, including LocalAI's own error texts (pkg/model/loader.go,
// pkg/model/initializers.go, core/backend/global_admission.go) and the blank
// bodies that opaque-errors mode produces.
func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		code       contracts.ErrCode
	}{
		{"503 model still loading", 503, errBody("model is still loading"), "5", contracts.ErrUnavailable},
		{"503 model_loading object", 503, `{"error":{"message":"loading ` + canary + `","code":"model_loading","type":"model_loading"}}`, "2", contracts.ErrUnavailable},
		{"503 plain", 503, errBody("service unavailable"), "", contracts.ErrUnavailable},
		{"503 blank (opaque errors)", 503, "", "", contracts.ErrUnavailable},
		{"503 backend capacity reached", 503, errBody("backend inference capacity reached (max_concurrent=1); retry after 1s"), "1", contracts.ErrResourceExhausted},
		{"503 load cooldown after a failed load", 503, errBody(`model "m" load is in cooldown after a recent failure; retry after 30s`), "30", contracts.ErrResourceExhausted},
		{"502", 502, errBody("bad gateway"), "", contracts.ErrUnavailable},
		{"507", 507, errBody("insufficient storage"), "", contracts.ErrResourceExhausted},
		{"429", 429, errBody("too many requests"), "3", contracts.ErrResourceExhausted},
		{"500 could not load model", 500, errBody("could not load model - all backends returned error: [llama-cpp]: failed"), "", contracts.ErrResourceExhausted},
		{"500 failed to allocate", 500, errBody("rpc error: ggml_backend_cpu_buffer_type_alloc_buffer: failed to allocate buffer"), "", contracts.ErrResourceExhausted},
		{"500 bad_alloc", 500, errBody("std::bad_alloc"), "", contracts.ErrResourceExhausted},
		{"500 OOM as a word", 500, errBody("worker killed by OOM"), "", contracts.ErrResourceExhausted},
		{"500 out of memory", 500, errBody("CUDA error: out of memory"), "", contracts.ErrResourceExhausted},
		{"500 backend not ready", 500, errBody("grpc service not ready: backend process exited with code 137"), "", contracts.ErrUnavailable},
		{"500 context exceeded", 500, errBody("the request exceeds the available context size"), "", contracts.ErrValidationFailed},
		{"500 no known phrase", 500, errBody("something odd, plenty of room"), "", contracts.ErrInternal},
		{"500 blank (opaque errors)", 500, "", "", contracts.ErrInternal},
		{"500 not JSON", 500, "panic: " + canary, "", contracts.ErrInternal},
		{"504", 504, errBody("gateway timeout"), "", contracts.ErrTimeout},
		{"408", 408, errBody("request timeout"), "", contracts.ErrTimeout},
		{"400 context exceeded", 400, errBody("prompt is too long"), "", contracts.ErrValidationFailed},
		{"400 other", 400, errBody("bad request"), "", contracts.ErrInternal},
		{"422", 422, errBody("unprocessable"), "", contracts.ErrInternal},
		{"413", 413, "", "", contracts.ErrValidationFailed},
		{"401", 401, errBody("An authentication key is required"), "", contracts.ErrInternal},
		{"403", 403, errBody("forbidden"), "", contracts.ErrInternal},
		{"404", 404, errBody("model not found"), "", contracts.ErrUnavailable},
		{"418", 418, errBody("teapot"), "", contracts.ErrInternal},
		{"501", 501, errBody("not implemented"), "", contracts.ErrInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := newRuntime(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				serveJSON(tt.status, tt.body)(w, r)
			})
			c, key := rt.client(t)
			_, err := c.Chat(bg, testScope(), chatReq("hi"))
			wantCode(t, err, tt.code)
			var ce *contracts.Error
			if !errors.As(err, &ce) {
				t.Fatalf("error %T is not a *contracts.Error", err)
			}
			if ce.Retryable != tt.code.Retryable() {
				t.Errorf("Retryable = %v for %s", ce.Retryable, tt.code)
			}
			if ce.Details["upstream_status"] != tt.status {
				t.Errorf("upstream_status = %v, want %d", ce.Details["upstream_status"], tt.status)
			}
			if tt.retryAfter != "" && ce.Details["retry_after_s"] == nil {
				t.Errorf("retry_after_s missing for Retry-After %q", tt.retryAfter)
			}
			if s := err.Error(); strings.Contains(s, canary) || strings.Contains(s, key) {
				t.Errorf("error leaks response or credential text: %q", s)
			}

			// The same classification applies to every call kind.
			_, err = c.ChatStream(bg, testScope(), chatReq("hi"))
			wantCode(t, err, tt.code)
			_, err = c.Embed(bg, testScope(), embedReq())
			wantCode(t, err, tt.code)
		})
	}

	t.Run("redirects are not followed", func(t *testing.T) {
		target := newRuntime(t, serveJSON(200, chatBody("x", "stop")))
		rt := newRuntime(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
		})
		c, _ := rt.client(t)
		_, err := c.Chat(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrInternal)
		if n := len(target.requests()); n != 0 {
			t.Fatalf("redirect target received %d requests", n)
		}
	})

	t.Run("connection refused is unavailable", func(t *testing.T) {
		rt := newRuntime(t, serveJSON(200, "{}"))
		c, _ := rt.client(t)
		rt.Close()
		_, err := c.Chat(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrUnavailable)
		_, err = c.Embed(bg, testScope(), embedReq())
		wantCode(t, err, contracts.ErrUnavailable)
	})

	t.Run("connection dropped mid-response is unavailable", func(t *testing.T) {
		rt := newRuntime(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		})
		c, _ := rt.client(t)
		_, err := c.Chat(bg, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrUnavailable)
	})

	slow := func(release <-chan struct{}) http.HandlerFunc {
		return func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
	}

	t.Run("caller deadline is a timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, slow(release))
		c, _ := rt.client(t)
		ctx, cancel := context.WithTimeout(bg, 100*time.Millisecond)
		defer cancel()
		_, err := c.Chat(ctx, testScope(), chatReq("hi"))
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("timeout must keep matching context.DeadlineExceeded: %v", err)
		}
	})

	t.Run("configured request timeout is a timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, slow(release))
		c, _ := rt.client(t, func(cfg *Config) { cfg.RequestTimeout = 100 * time.Millisecond })
		_, err := c.Embed(bg, testScope(), embedReq())
		wantCode(t, err, contracts.ErrTimeout)
	})

	t.Run("caller cancellation is not a timeout", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		rt := newRuntime(t, slow(release))
		c, _ := rt.client(t)
		ctx, cancel := context.WithCancel(bg)
		time.AfterFunc(100*time.Millisecond, cancel)
		_, err := c.Chat(ctx, testScope(), chatReq("hi"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if contracts.HasCode(err, contracts.ErrTimeout) || contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Errorf("a cancellation must stay a plain context error, got %v", err)
		}
	})
}

// A request over a provider budget is validation_failed, never
// resource_exhausted; the adapter must not turn the runtime's own context
// overflow into anything else either (see "500 context exceeded" above).
func TestErrorMappingKeepsRetryability(t *testing.T) {
	for _, code := range []contracts.ErrCode{contracts.ErrUnavailable, contracts.ErrTimeout, contracts.ErrResourceExhausted} {
		if !code.Retryable() {
			t.Errorf("%s should be retryable", code)
		}
	}
	for _, code := range []contracts.ErrCode{contracts.ErrValidationFailed, contracts.ErrSchemaInvalid, contracts.ErrInternal} {
		if code.Retryable() {
			t.Errorf("%s must not be retryable", code)
		}
	}
}

func errBody(message string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 500, "message": message + " " + canary, "type": ""}})
	return string(b)
}
