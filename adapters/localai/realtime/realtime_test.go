package realtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestRealtimeTranscriptEvents(t *testing.T) {
	t.Run("manual_deltas_become_interims_then_final_then_done", func(t *testing.T) {
		appends := make(chan int, 1)
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			n, ok := rc.until(evAudioCommit)
			if !ok {
				return
			}
			appends <- n
			rc.send(map[string]any{"type": evCommitted, "item_id": "i1"})
			rc.send(transDelta("i1", "ciao "))
			rc.send(transDelta("i1", "mondo"))
			rc.send(transCompleted("i1", " ciao mondo "))
			rc.drain(2 * time.Second)
		})
		c := srv.client(t)
		s := openStream(t, c)
		for range 3 {
			if err := s.Send(pcm(100 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CloseSend(); err != nil {
			t.Fatal(err)
		}
		evs, err := recvAll(s)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Recv ended with %v, want io.EOF", err)
		}
		if got := <-appends; got != 3 {
			t.Errorf("server saw %d audio events, want 3", got)
		}
		if len(evs) != 4 {
			t.Fatalf("got %d events, want 2 interims, 1 final and done: %+v", len(evs), evs)
		}
		for i, ev := range evs {
			if ev.Seq != i+1 {
				t.Errorf("event %d has Seq %d", i, ev.Seq)
			}
		}
		if evs[0].Text != "ciao" || evs[0].Final || evs[1].Text != "ciao mondo" || evs[1].Final {
			t.Errorf("interims = %+v %+v, want the text so far", evs[0], evs[1])
		}
		if !evs[2].Final || evs[2].Text != "ciao mondo" {
			t.Errorf("final = %+v, want \"ciao mondo\" trimmed", evs[2])
		}
		done := evs[3]
		if !done.Done || done.Model != "rt-pipeline" || done.Language != "it" || done.Offset != 300*time.Millisecond {
			t.Errorf("done = %+v", done)
		}
		if got := srv.queries[0]; got != "model=rt-pipeline" {
			t.Errorf("upgrade query = %q, want only the model", got)
		}
		if got := srv.auths[0]; got != "Bearer TESTONLY-realtime-key" {
			t.Errorf("Authorization = %q", got)
		}
	})

	t.Run("collect_transcript_joins_the_final_text", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			if _, ok := rc.until(evAudioCommit); !ok {
				return
			}
			rc.send(transCompleted("i1", "book a table for two"))
			rc.drain(2 * time.Second)
		})
		tr, err := srv.client(t).Transcribe(bg, testScope(), pv.TranscribeRequest{Format: fmt16k, Audio: pcm(time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		if tr.Text != "book a table for two" || tr.Duration != time.Second || tr.Model != "rt-pipeline" {
			t.Errorf("transcript = %+v", tr)
		}
	})

	t.Run("server_vad_utterances_become_segments", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			// Runtime-side turn detection: the client never commits; it only
			// appends audio and a tail of silence.
			if _, ok := rc.recv(); !ok {
				return
			}
			rc.send(map[string]any{"type": evSpeechStarted})
			rc.send(transCompleted("a", "first"))
			rc.send(map[string]any{"type": evSpeechStarted})
			rc.send(transCompleted("b", "second"))
			rc.drain(2 * time.Second)
		})
		c := srv.client(t, func(_ *localai.Config, cfg *Config) {
			cfg.ServerVAD, cfg.SilenceMs, cfg.SettleTimeout = true, 100, 100*time.Millisecond
		})
		s := openStream(t, c)
		if err := s.Send(pcm(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if err := s.CloseSend(); err != nil {
			t.Fatal(err)
		}
		tr, err := pv.CollectTranscript(s)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Text != "first second" {
			t.Errorf("text = %q, want the two utterances joined", tr.Text)
		}
		for _, ty := range srv.clientTypes() {
			if ty == evAudioCommit {
				t.Error("the client committed manually while the runtime's turn detection was active")
			}
		}
	})

	t.Run("silence_is_an_empty_complete_transcript", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			if _, ok := rc.until(evAudioClear); !ok {
				return
			}
			rc.send(map[string]any{"type": evCleared})
			rc.drain(2 * time.Second)
		})
		s := openStream(t, srv.client(t))
		if err := s.CloseSend(); err != nil {
			t.Fatal(err)
		}
		tr, err := pv.CollectTranscript(s)
		if err != nil || tr.Text != "" {
			t.Fatalf("transcript = %+v, err %v", tr, err)
		}
	})

	t.Run("a_cut_connection_is_partial_and_unavailable", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			if _, ok := rc.until(evAudioCommit); !ok {
				return
			}
			rc.send(transDelta("i1", "half a sen"))
		})
		s := openStream(t, srv.client(t))
		_ = s.Send(pcm(100 * time.Millisecond))
		_ = s.CloseSend()
		tr, err := pv.CollectTranscript(s)
		wantCode(t, err, contracts.ErrUnavailable)
		if tr.Text != "" {
			t.Errorf("a partial transcript %q was returned as a success", tr.Text)
		}
	})

	t.Run("a_transcription_failure_is_an_error_not_an_empty_transcript", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			if _, ok := rc.until(evAudioCommit); !ok {
				return
			}
			rc.send(map[string]any{"type": evError, "error": map[string]any{"code": "transcription_failed", "message": "secret words here"}})
			rc.drain(time.Second)
		})
		s := openStream(t, srv.client(t))
		_ = s.Send(pcm(100 * time.Millisecond))
		_ = s.CloseSend()
		_, err := pv.CollectTranscript(s)
		wantCode(t, err, contracts.ErrUnavailable)
		if strings.Contains(err.Error(), "secret words") {
			t.Errorf("the runtime's message leaked into the error: %v", err)
		}
	})
}

func TestRealtimeCancel(t *testing.T) {
	// serve handshakes, then reports whether the client's connection dropped.
	serve := func(t *testing.T) (*rtServer, chan bool) {
		dropped := make(chan bool, 1)
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				dropped <- false
				return
			}
			dropped <- rc.drain(3 * time.Second)
		})
		return srv, dropped
	}

	t.Run("close_unblocks_a_waiting_recv_and_drops_the_connection", func(t *testing.T) {
		srv, dropped := serve(t)
		s := openStream(t, srv.client(t))
		errc := make(chan error, 1)
		go func() { _, err := s.Recv(); errc <- err }()
		time.Sleep(50 * time.Millisecond)
		start := time.Now()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errc:
			if !errors.Is(err, providers.ErrStreamClosed) {
				t.Errorf("Recv after Close = %v, want ErrStreamClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Recv did not return after Close")
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("cancel took %v", d)
		}
		if !<-dropped {
			t.Error("the server never saw the connection drop: the runtime keeps working after a cancel")
		}
		if err := s.Close(); err != nil {
			t.Errorf("second Close = %v, want nil (idempotent)", err)
		}
		if err := s.Send(pcm(10 * time.Millisecond)); !errors.Is(err, providers.ErrStreamClosed) {
			t.Errorf("Send after Close = %v, want ErrStreamClosed", err)
		}
	})

	t.Run("caller_context_cancel_ends_recv_promptly", func(t *testing.T) {
		srv, dropped := serve(t)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		s, err := srv.client(t).TranscribeStream(ctx, testScope(), pv.STTStreamRequest{Format: fmt16k})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		errc := make(chan error, 1)
		go func() { _, err := s.Recv(); errc <- err }()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Recv after cancel = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Recv did not return after the context was cancelled")
		}
		if !<-dropped {
			t.Error("the server never saw the connection drop after the context was cancelled")
		}
	})

	t.Run("caller_deadline_is_a_timeout", func(t *testing.T) {
		srv, _ := serve(t)
		ctx, cancel := context.WithTimeout(bg, 150*time.Millisecond)
		defer cancel()
		s, err := srv.client(t).TranscribeStream(ctx, testScope(), pv.STTStreamRequest{Format: fmt16k})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		_, err = s.Recv()
		wantCode(t, err, contracts.ErrTimeout)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error %v does not match context.DeadlineExceeded", err)
		}
	})

	t.Run("a_context_cancelled_before_open_never_dials", func(t *testing.T) {
		srv, _ := serve(t)
		ctx, cancel := context.WithCancel(bg)
		cancel()
		_, err := srv.client(t).TranscribeStream(ctx, testScope(), pv.STTStreamRequest{Format: fmt16k})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n := srv.connections(); n != 0 {
			t.Errorf("%d connections were opened for a cancelled context", n)
		}
	})
}

// SEC-070, SEC-094: a Realtime session is configured so the runtime neither
// answers nor calls tools on its own, and the client never lets it.
func TestRealtimeToolCallingDisabled(t *testing.T) {
	// forbidden are the client events that would make the runtime answer or act.
	forbidden := map[string]bool{
		"response.create": true, "conversation.item.create": true, "response.cancel": true,
		"conversation.item.delete": true, "conversation.item.truncate": true,
	}
	noForbidden := func(t *testing.T, srv *rtServer) {
		t.Helper()
		for _, ty := range srv.clientTypes() {
			if forbidden[ty] {
				t.Errorf("the client sent %q", ty)
			}
		}
	}
	update := func(t *testing.T, srv *rtServer) map[string]any {
		t.Helper()
		evs := srv.clientEvents()
		if len(evs) == 0 || evs[0]["type"] != evSessionUpdate {
			t.Fatalf("first client event = %v, want session.update", evs)
		}
		return evs[0]["session"].(map[string]any)
	}
	input := func(sess map[string]any) map[string]any {
		return sess["audio"].(map[string]any)["input"].(map[string]any)
	}

	t.Run("session_configuration_manual", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			rc.until(evAudioCommit)
			rc.send(transCompleted("i1", "hello"))
			rc.drain(time.Second)
		})
		if _, err := srv.client(t).Transcribe(bg, testScope(), pv.TranscribeRequest{Format: fmt16k, Audio: pcm(100 * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
		sess := update(t, srv)
		if sess["type"] != "transcription" {
			t.Errorf("session type = %v, want a transcription session", sess["type"])
		}
		if sess["tool_choice"] != "none" {
			t.Errorf("tool_choice = %v, want none", sess["tool_choice"])
		}
		if tools, ok := sess["tools"].([]any); !ok || len(tools) != 0 {
			t.Errorf("tools = %v, want an empty list", sess["tools"])
		}
		in := input(sess)
		if td, present := in["turn_detection"]; !present || td != nil {
			t.Errorf("turn_detection = %v (present %v), want an explicit null", td, present)
		}
		if got := in["transcription"].(map[string]any)["model"]; got != "rt-pipeline" {
			t.Errorf("transcription model = %v", got)
		}
		noForbidden(t, srv)
		if got := srv.queries[0]; got != "model=rt-pipeline" {
			t.Errorf("query = %q: only the model may be sent (never localai_assistant)", got)
		}
	})

	t.Run("session_configuration_server_vad_never_answers", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			rc.drain(time.Second)
		})
		c := srv.client(t, func(_ *localai.Config, cfg *Config) { cfg.ServerVAD = true })
		s := openStream(t, c)
		_ = s.Close()
		td := input(update(t, srv))["turn_detection"].(map[string]any)
		if td["type"] != "server_vad" || td["create_response"] != false || td["interrupt_response"] != false {
			t.Errorf("turn_detection = %v, want server_vad with answers and interruptions off", td)
		}
	})

	t.Run("a_model_answer_is_a_violation_and_is_never_relayed", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			ev   map[string]any
		}{
			{"response_created", map[string]any{"type": "response.created", "response": map[string]any{"id": "r1"}}},
			{"audio_delta", map[string]any{"type": "response.output_audio.delta", "delta": "AAAA"}},
			{"function_call_args", map[string]any{"type": "response.function_call_arguments.done", "arguments": `{"x":1}`}},
			{"mcp_call", map[string]any{"type": "response.mcp_call.in_progress"}},
			{"function_call_item", map[string]any{"type": evItemAdded, "item": map[string]any{"type": "function_call", "name": "delete_everything"}}},
			{"assistant_item", map[string]any{"type": evItemAdded, "item": map[string]any{"type": "message", "role": "assistant"}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv := newRT(t, func(rc *rtConn) {
					if _, ok := rc.handshake("transcription"); !ok {
						return
					}
					rc.send(transDelta("i1", "so far"))
					rc.send(tc.ev)
					rc.send(transCompleted("i1", "must not be delivered"))
					rc.drain(time.Second)
				})
				s := openStream(t, srv.client(t))
				_ = s.Send(pcm(50 * time.Millisecond))
				_ = s.CloseSend()
				tr, err := pv.CollectTranscript(s)
				wantCode(t, err, contracts.ErrInternal)
				var ce *contracts.Error
				if !errors.As(err, &ce) || ce.Details["reason"] != reasonSideDoor {
					t.Errorf("error %v lacks the %s reason", err, reasonSideDoor)
				}
				if tr.Text != "" {
					t.Errorf("text %q was delivered from a session that misbehaved", tr.Text)
				}
				noForbidden(t, srv)
			})
		}
	})

	t.Run("a_session_that_is_not_transcription_only_gets_no_audio", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("realtime"); !ok {
				return
			}
			rc.drain(time.Second)
		})
		_, err := srv.client(t).TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: fmt16k})
		wantCode(t, err, contracts.ErrInternal)
		var ce *contracts.Error
		if !errors.As(err, &ce) || ce.Details["reason"] != reasonNotTransOnly {
			t.Errorf("error %v lacks the %s reason", err, reasonNotTransOnly)
		}
		time.Sleep(50 * time.Millisecond)
		for _, ty := range srv.clientTypes() {
			if ty == evAudioAppend || ty == evAudioCommit {
				t.Errorf("the client sent %s to a session that could answer", ty)
			}
		}
	})

	t.Run("the_endpoint_allowlist_admits_only_the_model_parameter", func(t *testing.T) {
		parse := func(s string) *url.URL {
			u, err := url.Parse(s)
			if err != nil {
				t.Fatal(err)
			}
			return u
		}
		ok := parse("http://127.0.0.1:1/v1/realtime?model=m")
		if !lookupEndpoint(http.MethodGet, ok) {
			t.Fatal("the one allowed upgrade was refused")
		}
		for _, bad := range []struct{ method, target string }{
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime?model=m&localai_assistant=1"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime?localai_assistant=true"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime?model="},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime?model=a&model=b"},
			{http.MethodGet, "http://127.0.0.1:1/realtime?model=m"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime/sessions?model=m"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime/calls?model=m"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime/?model=m"},
			{http.MethodPost, "http://127.0.0.1:1/v1/realtime?model=m"},
			{http.MethodGet, "http://127.0.0.1:1/v1/realtime?model=m#x"},
		} {
			if lookupEndpoint(bad.method, parse(bad.target)) {
				t.Errorf("%s %s was allowed", bad.method, bad.target)
			}
		}
		// The check is in the transport: a request through the client's own
		// http client is refused before any I/O.
		srv := newRT(t, func(rc *rtConn) { t.Error("a forbidden request reached the server") })
		c := srv.client(t)
		resp, err := c.hc.Get(srv.URL + "/v1/realtime?model=m&localai_assistant=true")
		if err == nil {
			resp.Body.Close()
			t.Fatal("the forbidden request was sent")
		}
		if !errors.Is(err, errForbiddenEndpoint) {
			t.Errorf("err = %v, want errForbiddenEndpoint", err)
		}
		if n := srv.connections(); n != 0 {
			t.Errorf("server saw %d requests", n)
		}
	})
}

func TestRealtimeValidation(t *testing.T) {
	srv := newRT(t, func(rc *rtConn) { t.Error("an invalid request reached the runtime") })
	c := srv.client(t)
	open := func(sc contracts.Scope, req pv.STTStreamRequest) error {
		_, err := c.TranscribeStream(bg, sc, req)
		return err
	}
	good := pv.STTStreamRequest{Format: fmt16k}
	wantCode(t, open(contracts.Scope{}, good), contracts.ErrScopeMissing)
	wantCode(t, open(testScope(), pv.STTStreamRequest{Format: pv.AudioFormat{Encoding: pv.EncodingPCM16, SampleRate: 16000, Channels: 2}}), contracts.ErrValidationFailed)
	wantCode(t, open(testScope(), pv.STTStreamRequest{Format: fmt16k, Profile: "nope"}), contracts.ErrValidationFailed)
	wantCode(t, open(testScope(), pv.STTStreamRequest{Format: pv.AudioFormat{}}), contracts.ErrValidationFailed)

	noModel, err := New(localai.Config{Endpoint: srv.URL}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = noModel.TranscribeStream(bg, testScope(), good)
	wantCode(t, err, contracts.ErrValidationFailed)

	// Construction: a non-loopback plaintext endpoint and a bad model are refused.
	if _, err := New(localai.Config{Endpoint: "http://203.0.113.9:8080"}, Config{Model: "m"}); err == nil {
		t.Error("a plaintext non-loopback endpoint was accepted")
	}
	if _, err := New(localai.Config{Endpoint: srv.URL}, Config{Model: "bad name"}); err == nil {
		t.Error("an invalid model name was accepted")
	}

	// Send limits and ordering.
	live := newRT(t, func(rc *rtConn) {
		rc.handshake("transcription")
		rc.drain(time.Second)
	})
	s := openStream(t, live.client(t, func(_ *localai.Config, cfg *Config) { cfg.MaxChunkBytes = 1000 }))
	wantCode(t, s.Send(make([]byte, 2000)), contracts.ErrValidationFailed)
	wantCode(t, s.Send(make([]byte, 3)), contracts.ErrValidationFailed)
	if err := s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.Send(make([]byte, 100)), contracts.ErrValidationFailed)
}

func TestRealtimeSecretNeverPrinted(t *testing.T) {
	c, err := New(localai.Config{Endpoint: "http://127.0.0.1:1"}, testRealtimeConfig(), WithAPIKey("TESTONLY-do-not-print"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.String(); strings.Contains(got, "TESTONLY") {
		t.Errorf("String() leaked the key: %s", got)
	}
	if diff := cmp.Diff(newAPIKey("x").String(), redacted); diff != "" {
		t.Error(diff)
	}
}
