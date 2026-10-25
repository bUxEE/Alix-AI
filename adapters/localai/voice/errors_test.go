package voice

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// roleCall is one way of reaching the runtime; every failure mapping holds for
// all four.
type roleCall struct {
	name string
	run  func(c *Client) error
}

func roleCalls() []roleCall {
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)
	return []roleCall{
		{"stt", func(c *Client) error {
			_, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
			return err
		}},
		{"stt stream", func(c *Client) error {
			s, err := c.TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: f})
			if err != nil {
				return err
			}
			defer s.Close()
			_ = s.Send(pcm)
			_ = s.CloseSend()
			_, err = pv.CollectTranscript(s)
			return err
		}},
		{"tts", func(c *Client) error {
			s, err := c.SynthesizeStream(bg, testScope(), ttsReq("hello"))
			if err != nil {
				return err
			}
			defer s.Close()
			_, err = recvAll(s)
			return err
		}},
		{"vad", func(c *Client) error {
			_, err := c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: pcm, Format: f})
			return err
		}},
	}
}

// promptMarker stands for the text a runtime error body may echo (the
// transcript, the text being spoken). It must never reach an error.
const promptMarker = "SECRET-PROMPT-TEXT"

func TestErrorMapping(t *testing.T) {
	statuses := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   contracts.ErrCode
	}{
		{"model or route not found", 404, `{"error":{"message":"` + promptMarker + `"}}`, nil, contracts.ErrUnavailable},
		{"service unavailable", 503, "", nil, contracts.ErrUnavailable},
		{"bad gateway", 502, "", nil, contracts.ErrUnavailable},
		{"gateway timeout", 504, "", nil, contracts.ErrTimeout},
		{"request timeout", 408, "", nil, contracts.ErrTimeout},
		{"too many requests", 429, "", map[string]string{"Retry-After": "7"}, contracts.ErrResourceExhausted},
		{"insufficient storage", 507, "", nil, contracts.ErrResourceExhausted},
		{"request too large", 413, "", nil, contracts.ErrValidationFailed},
		{"bad api key", 401, `{"error":{"message":"` + promptMarker + `"}}`, nil, contracts.ErrInternal},
		{"forbidden", 403, "", nil, contracts.ErrInternal},
		{"rejected request", 400, `{"error":{"message":"` + promptMarker + `"}}`, nil, contracts.ErrInternal},
		{"model load failure", 500, `{"error":{"message":"` + promptMarker + ` failed to load model whisper"}}`, nil, contracts.ErrResourceExhausted},
		{"out of memory", 500, `{"error":{"message":"` + promptMarker + ` out of memory"}}`, nil, contracts.ErrResourceExhausted},
		{"backend not ready", 500, `{"error":{"message":"grpc service not ready"}}`, nil, contracts.ErrUnavailable},
		{"plain 500", 500, `{"error":{"message":"` + promptMarker + `"}}`, nil, contracts.ErrInternal},
		{"opaque 500", 500, "", nil, contracts.ErrInternal},
		{"redirect is not followed", 307, "", map[string]string{"Location": "/v1/audio/speech"}, contracts.ErrInternal},
		{"unexpected 2xx", 202, "", nil, contracts.ErrInternal},
	}
	for _, call := range roleCalls() {
		for _, tc := range statuses {
			t.Run(call.name+"/"+tc.name, func(t *testing.T) {
				srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
					for k, v := range tc.header {
						w.Header().Set(k, v)
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				})
				c, key := srv.client(t)
				err := call.run(c)
				wantCode(t, err, tc.want)
				if msg := err.Error(); strings.Contains(msg, promptMarker) || strings.Contains(msg, key) {
					t.Errorf("error text carries a response body or the key: %v", msg)
				}
				var ce *contracts.Error
				if !errors.As(err, &ce) || ce.Details["upstream_status"] != tc.status {
					t.Errorf("error lacks the upstream status %d: %#v", tc.status, err)
				}
				if ce != nil && tc.header["Retry-After"] != "" && ce.Details["retry_after_s"] != 7 {
					t.Errorf("retry_after_s = %v, want 7", ce.Details["retry_after_s"])
				}
				if n := len(srv.requests()); n != 1 {
					t.Errorf("runtime received %d requests, want 1 (no retry, no redirect)", n)
				}
			})
		}
	}
}

func TestErrorMappingTransport(t *testing.T) {
	for _, call := range roleCalls() {
		t.Run(call.name+"/runtime down", func(t *testing.T) {
			srv := newStub(t, validTranscript)
			c, _ := srv.client(t)
			srv.Close() // nothing listens any more: connection refused
			wantCode(t, call.run(c), contracts.ErrUnavailable)
		})
		t.Run(call.name+"/deadline", func(t *testing.T) {
			srv := newStub(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			c, _ := srv.client(t, func(rt *localai.Config, _ *Config) { rt.RequestTimeout = 150 * time.Millisecond })
			err := call.run(c)
			wantCode(t, err, contracts.ErrTimeout)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("a timeout must still match context.DeadlineExceeded: %v", err)
			}
		})
		t.Run(call.name+"/cancelled by the caller", func(t *testing.T) {
			srv := newStub(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			c, _ := srv.client(t)
			ctx, cancel := context.WithCancel(bg)
			time.AfterFunc(100*time.Millisecond, cancel)
			var err error
			f := pv.PCM16Mono(16000)
			pcm := tonePCM(f, 100*time.Millisecond)
			switch call.name {
			case "stt":
				_, err = c.Transcribe(ctx, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
			case "stt stream":
				var s pv.STTStream
				if s, err = c.TranscribeStream(ctx, testScope(), pv.STTStreamRequest{Format: f}); err == nil {
					defer s.Close()
					_ = s.Send(pcm)
					_ = s.CloseSend()
					_, err = pv.CollectTranscript(s)
				}
			case "tts":
				var s pv.AudioStream
				if s, err = c.SynthesizeStream(ctx, testScope(), ttsReq("hello")); err == nil {
					defer s.Close()
					_, err = recvAll(s)
				}
			case "vad":
				_, err = c.DetectSpeech(ctx, testScope(), pv.DetectRequest{Audio: pcm, Format: f})
			}
			if !errors.Is(err, context.Canceled) || contracts.HasCode(err, contracts.ErrTimeout) || contracts.HasCode(err, contracts.ErrUnavailable) {
				t.Errorf("cancellation must stay a plain context.Canceled, got %v (code %q)", err, contracts.CodeOf(err))
			}
		})
	}
}

func TestErrorMappingMalformedAnswers(t *testing.T) {
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)
	cases := map[string]struct {
		h    http.HandlerFunc
		call func(c *Client) error
	}{
		"stt body is not JSON": {
			func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, "<html>") },
			func(c *Client) error {
				_, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
				return err
			},
		},
		"stt body past the response cap": {
			func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, `{"text":"`+strings.Repeat("a", 2000)+`"}`)
			},
			func(c *Client) error {
				_, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
				return err
			},
		},
		"vad body past the response cap": {
			func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, `{"segments":[`+strings.Repeat(`{"start":0,"end":0.01},`, 200)+`{"start":1,"end":1.1}]}`)
			},
			func(c *Client) error {
				_, err := c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: pcm, Format: f})
				return err
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newStub(t, tc.h)
			c, _ := srv.client(t, func(rt *localai.Config, _ *Config) { rt.MaxResponseBytes = 1500 })
			wantCode(t, tc.call(c), contracts.ErrSchemaInvalid)
		})
	}
}
