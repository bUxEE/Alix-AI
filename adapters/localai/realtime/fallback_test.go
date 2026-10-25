package realtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// deadClient is a Realtime client whose runtime refuses connections.
func deadClient(t *testing.T) *Client {
	t.Helper()
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	c, err := New(localai.Config{Endpoint: url, RequestTimeout: 2 * time.Second}, testRealtimeConfig())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func transcribeVia(s pv.STT, ctx context.Context) (pv.Transcript, error) {
	st, err := s.TranscribeStream(ctx, testScope(), pv.STTStreamRequest{Format: fmt16k})
	if err != nil {
		return pv.Transcript{}, err
	}
	if err := st.Send(pcm(100 * time.Millisecond)); err != nil {
		_ = st.Close()
		return pv.Transcript{}, err
	}
	if err := st.CloseSend(); err != nil {
		_ = st.Close()
		return pv.Transcript{}, err
	}
	return pv.CollectTranscript(st)
}

func TestFallbackToSeparateSTTTTS(t *testing.T) {
	scene := func(t *testing.T, rt pv.STT, opts ...SpeechOption) (*Speech, *fake.Provider, *[]FallbackEvent) {
		t.Helper()
		f := fake.New()
		var events []FallbackEvent
		opts = append(opts, WithFallbackHook(func(e FallbackEvent) { events = append(events, e) }))
		return NewSpeech(rt, f, f, opts...), f, &events
	}

	t.Run("refused_connection_falls_back_for_a_stream", func(t *testing.T) {
		sp, f, events := scene(t, deadClient(t))
		f.Script("book a table")
		tr, err := transcribeVia(sp, bg)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Text != "book a table" {
			t.Errorf("text = %q, want the separate adapter's", tr.Text)
		}
		if n, last := sp.Fallbacks(); n != 1 || last.Reason != contracts.ErrUnavailable || last.Role != providers.RoleSTT || last.Op != "transcribe_stream" {
			t.Errorf("Fallbacks = %d %+v", n, last)
		}
		if len(*events) != 1 {
			t.Errorf("hook called %d times, want 1", len(*events))
		}
		if f.CallCount() != 1 {
			t.Errorf("separate adapter saw %d calls, want 1", f.CallCount())
		}
	})

	t.Run("no_realtime_route_falls_back_for_a_batch_call", func(t *testing.T) {
		missing := httptest.NewServer(http.NotFoundHandler()) // no websocket route: 404 on the upgrade
		t.Cleanup(missing.Close)
		c, err := New(localai.Config{Endpoint: missing.URL}, testRealtimeConfig())
		if err != nil {
			t.Fatal(err)
		}
		sp, f, _ := scene(t, c)
		f.Script("hello")
		tr, err := sp.Transcribe(bg, testScope(), pv.TranscribeRequest{Format: fmt16k, Audio: pcm(time.Second)})
		if err != nil || tr.Text != "hello" {
			t.Fatalf("transcript = %+v, err %v", tr, err)
		}
		if n, _ := sp.Fallbacks(); n != 1 {
			t.Errorf("Fallbacks = %d, want 1", n)
		}
	})

	t.Run("a_runtime_that_cannot_run_the_session_falls_back", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			rc.send(map[string]any{"type": evSessionCreated})
			rc.recv()
			rc.send(map[string]any{"type": evError, "error": map[string]any{"code": "session_update_error", "message": "model is not a valid pipeline model"}})
		})
		sp, f, _ := scene(t, srv.client(t))
		f.Script("ciao")
		tr, err := transcribeVia(sp, bg)
		if err != nil || tr.Text != "ciao" {
			t.Fatalf("transcript = %+v, err %v", tr, err)
		}
	})

	t.Run("a_healthy_realtime_serves_and_nothing_falls_back", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			rc.until(evAudioCommit)
			rc.send(transCompleted("i1", "from realtime"))
			rc.drain(time.Second)
		})
		sp, f, _ := scene(t, srv.client(t))
		tr, err := transcribeVia(sp, bg)
		if err != nil || tr.Text != "from realtime" {
			t.Fatalf("transcript = %+v, err %v", tr, err)
		}
		if n, _ := sp.Fallbacks(); n != 0 || f.CallCount() != 0 {
			t.Errorf("fallbacks %d, separate calls %d, want 0 and 0", n, f.CallCount())
		}
	})

	t.Run("no_fallback_when_the_failure_is_not_unavailability", func(t *testing.T) {
		sp, f, _ := scene(t, deadClient(t))

		// The caller's own cancellation.
		ctx, cancel := context.WithCancel(bg)
		cancel()
		_, err := transcribeVia(sp, ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled call = %v, want context.Canceled", err)
		}
		// A request the runtime was never going to accept.
		_, err = sp.TranscribeStream(bg, contracts.Scope{}, pv.STTStreamRequest{Format: fmt16k})
		wantCode(t, err, contracts.ErrScopeMissing)
		_, err = sp.TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: fmt16k, Profile: "unknown"})
		wantCode(t, err, contracts.ErrValidationFailed)

		// A runtime that is not transcription-only: a security failure.
		bad := newRT(t, func(rc *rtConn) {
			rc.handshake("realtime")
			rc.drain(time.Second)
		})
		sp2, f2, _ := scene(t, bad.client(t))
		_, err = transcribeVia(sp2, bg)
		wantCode(t, err, contracts.ErrInternal)

		// A resource failure is reported, not hidden behind another adapter.
		full := newRT(t, func(rc *rtConn) {
			rc.send(map[string]any{"type": evSessionCreated})
			rc.recv()
			rc.send(map[string]any{"type": evError, "error": map[string]any{"code": "buffer_size_exceeded"}})
		})
		sp3, f3, _ := scene(t, full.client(t))
		_, err = transcribeVia(sp3, bg)
		wantCode(t, err, contracts.ErrResourceExhausted)

		for name, n := range map[string]int{
			"cancel/validation": f.CallCount(), "side door": f2.CallCount(), "resources": f3.CallCount(),
		} {
			if n != 0 {
				t.Errorf("%s: the separate adapter was called %d times", name, n)
			}
		}
		if n, _ := sp.Fallbacks(); n != 0 {
			t.Errorf("Fallbacks = %d, want 0", n)
		}
	})

	t.Run("a_stream_that_failed_after_audio_is_not_rerun", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			rc.until(evAudioCommit)
			rc.send(transDelta("i1", "par"))
			// the connection drops
		})
		sp, f, _ := scene(t, srv.client(t))
		f.Script("must not be used")
		_, err := transcribeVia(sp, bg)
		wantCode(t, err, contracts.ErrUnavailable)
		if f.CallCount() != 0 {
			t.Errorf("the separate adapter ran %d times after a stream had carried audio", f.CallCount())
		}
		if n, _ := sp.Fallbacks(); n != 0 {
			t.Errorf("Fallbacks = %d, want 0", n)
		}
	})

	t.Run("a_batch_call_cut_part_way_reruns_because_the_caller_holds_the_audio", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) {
			if _, ok := rc.handshake("transcription"); !ok {
				return
			}
			rc.until(evAudioCommit)
		})
		sp, f, _ := scene(t, srv.client(t))
		f.Script("second try")
		tr, err := sp.Transcribe(bg, testScope(), pv.TranscribeRequest{Format: fmt16k, Audio: pcm(time.Second)})
		if err != nil || tr.Text != "second try" {
			t.Fatalf("transcript = %+v, err %v", tr, err)
		}
		if n, last := sp.Fallbacks(); n != 1 || last.Op != "transcribe" {
			t.Errorf("Fallbacks = %d %+v", n, last)
		}
	})

	t.Run("speech_synthesis_never_goes_through_realtime", func(t *testing.T) {
		srv := newRT(t, func(rc *rtConn) { t.Error("text to speech reached the realtime runtime") })
		sp, f, _ := scene(t, srv.client(t))
		st, err := sp.SynthesizeStream(bg, testScope(), pv.SynthesizeRequest{Text: "verified response text"})
		if err != nil {
			t.Fatal(err)
		}
		audio, err := pv.CollectAudio(st, 1<<20)
		if err != nil || len(audio.Data) == 0 {
			t.Fatalf("audio = %d bytes, err %v", len(audio.Data), err)
		}
		calls := f.Calls()
		if len(calls) != 1 || calls[0].Op != fake.OpSynthesize || calls[0].Text != "verified response text" {
			t.Errorf("separate adapter calls = %+v", calls)
		}
		if srv.connections() != 0 {
			t.Error("a realtime connection was opened for speech")
		}
		// With the separate adapter failing, the failure surfaces: no model fallback.
		f.FailNext(fake.FailUnavailable)
		_, err = sp.SynthesizeStream(bg, testScope(), pv.SynthesizeRequest{Text: "x"})
		wantCode(t, err, contracts.ErrUnavailable)
	})

	t.Run("realtime_not_configured_uses_the_separate_adapter_without_a_fallback_record", func(t *testing.T) {
		sp, f, _ := scene(t, nil)
		f.Script("plain")
		tr, err := transcribeVia(sp, bg)
		if err != nil || tr.Text != "plain" {
			t.Fatalf("transcript = %+v, err %v", tr, err)
		}
		if n, _ := sp.Fallbacks(); n != 0 {
			t.Errorf("Fallbacks = %d, want 0", n)
		}
		none := NewSpeech(nil, nil, nil)
		_, err = transcribeVia(none, bg)
		wantCode(t, err, contracts.ErrUnavailable)
		_, err = none.SynthesizeStream(bg, testScope(), pv.SynthesizeRequest{Text: "x"})
		wantCode(t, err, contracts.ErrUnavailable)
	})
}
