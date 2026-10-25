package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestTranscribeRequestShape(t *testing.T) {
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 500*time.Millisecond)
	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"text":" ciao a tutti ","language":"it","duration":0.5,"segments":[{"id":0,"start":0,"end":0.5,"text":"x"}]}`)
	})
	c, key := srv.client(t)

	got, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f, Language: "it-IT", Profile: "it-fast"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "ciao a tutti" || got.Language != "it" || got.Model != "stt-it" {
		t.Fatalf("transcript = %+v", got)
	}
	if got.Duration != 500*time.Millisecond {
		t.Errorf("duration = %v, want the length of the audio sent", got.Duration)
	}

	r := srv.only(t)
	if r.Method != http.MethodPost || r.Path != "/v1/audio/transcriptions" {
		t.Fatalf("request = %s %s", r.Method, r.Path)
	}
	if r.Header.Get("Authorization") != "Bearer "+key {
		t.Error("the API key was not sent as a bearer token")
	}
	mp := parseMultipart(t, r)
	wantFields := map[string]string{"model": "stt-it", "response_format": "verbose_json", "temperature": "0", "language": "it"}
	for k, v := range wantFields {
		if mp.fields[k] != v {
			t.Errorf("form field %s = %q, want %q", k, mp.fields[k], v)
		}
	}
	if len(mp.fields) != len(wantFields) {
		t.Errorf("form fields = %v, want exactly %v", mp.fields, wantFields)
	}
	if mp.fileName != "audio.wav" || mp.fileType != "audio/wav" {
		t.Errorf("file part = %q (%s), want a fixed audio.wav of type audio/wav", mp.fileName, mp.fileType)
	}
	// The upload is a canonical WAV file around the exact PCM that was sent.
	if len(mp.file) != wavHeaderBytes+len(pcm) || !bytes.Equal(mp.file[wavHeaderBytes:], pcm) {
		t.Fatalf("file part is %d bytes, want a %d-byte header and the PCM", len(mp.file), wavHeaderBytes)
	}
	h := mp.file[:wavHeaderBytes]
	if string(h[0:4]) != "RIFF" || string(h[8:16]) != "WAVEfmt " || string(h[36:40]) != "data" ||
		binary.LittleEndian.Uint16(h[22:]) != 1 || binary.LittleEndian.Uint32(h[24:]) != 16000 ||
		binary.LittleEndian.Uint16(h[34:]) != 16 || int(binary.LittleEndian.Uint32(h[40:])) != len(pcm) ||
		int(binary.LittleEndian.Uint32(h[4:])) != 36+len(pcm) {
		t.Errorf("WAV header = % x", h)
	}
}

func TestTranscribeOptionalFieldsAndAnswers(t *testing.T) {
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)

	t.Run("no language sent when none asked", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		if _, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f}); err != nil {
			t.Fatal(err)
		}
		mp := parseMultipart(t, srv.only(t))
		if _, ok := mp.fields["language"]; ok {
			t.Error("a language field was sent although none was asked for")
		}
		if mp.fields["model"] != "stt-model" {
			t.Errorf("model = %q, want the default STT model", mp.fields["model"])
		}
	})
	t.Run("stereo 48 kHz is wrapped as it is", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		sf := pv.AudioFormat{Encoding: pv.EncodingPCM16, SampleRate: 48000, Channels: 2}
		stereo := tonePCM(sf, 50*time.Millisecond)
		if _, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: stereo, Format: sf}); err != nil {
			t.Fatal(err)
		}
		h := parseMultipart(t, srv.only(t)).file
		if binary.LittleEndian.Uint16(h[22:]) != 2 || binary.LittleEndian.Uint32(h[24:]) != 48000 {
			t.Errorf("WAV header = % x", h[:wavHeaderBytes])
		}
	})
	t.Run("a bad request never reaches the runtime", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		cases := map[string]pv.TranscribeRequest{
			"empty audio":      {Format: f},
			"odd length":       {Audio: []byte{1, 2, 3}, Format: f},
			"bad language":     {Audio: pcm, Format: f, Language: "not a tag"},
			"unknown profile":  {Audio: pcm, Format: f, Profile: "nope"},
			"oversize":         {Audio: make([]byte, pv.DefaultLimits().MaxAudioBytes+2), Format: f},
			"unsupported rate": {Audio: pcm, Format: pv.PCM16Mono(1000)},
		}
		for name, req := range cases {
			if _, err := c.Transcribe(bg, testScope(), req); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("%s: error = %v, want validation_failed", name, err)
			}
		}
		if n := len(srv.requests()); n != 0 {
			t.Errorf("runtime received %d requests for invalid input", n)
		}
	})
	t.Run("a missing text field is not a transcript", func(t *testing.T) {
		srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `{"language":"en"}`) })
		c, _ := srv.client(t)
		_, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
		wantCode(t, err, contracts.ErrSchemaInvalid)
	})
	t.Run("silence is an empty success", func(t *testing.T) {
		srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `{"text":"","language":"junk value"}`) })
		c, _ := srv.client(t)
		got, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: pcm, Format: f})
		if err != nil || got.Text != "" || got.Language != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func newSTTStream(t *testing.T, c *Client, f pv.AudioFormat) pv.STTStream {
	t.Helper()
	s, err := c.TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: f, Language: "en-GB"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTranscribeStreamEvents(t *testing.T) {
	f := pv.PCM16Mono(16000)
	a, b := tonePCM(f, 100*time.Millisecond), tonePCM(f, 150*time.Millisecond)
	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`{"type":"transcript.text.delta","delta":"Hel"}`,
			`{"type":"transcript.text.delta","delta":"lo "}`,
			`{"type":"transcript.text.delta","delta":"world"}`,
			`{"type":"transcript.text.done","text":"Hello world","language":"en","duration":0.25}`,
			`[DONE]`)
	})
	c, _ := srv.client(t)
	s := newSTTStream(t, c, f)

	if err := s.Send(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(b); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("runtime was contacted before CloseSend (%d requests): live input cannot be streamed to it", n)
	}
	if err := s.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(a); err == nil {
		t.Error("Send after CloseSend succeeded")
	}

	var evs []pv.TranscriptEvent
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	want := []struct {
		text        string
		final, done bool
	}{{"Hel", false, false}, {"Hello ", false, false}, {"Hello world", false, false}, {"Hello world", true, false}, {"", false, true}}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(evs), len(want), evs)
	}
	for i, w := range want {
		if evs[i].Seq != i+1 || evs[i].Text != w.text || evs[i].Final != w.final || evs[i].Done != w.done {
			t.Errorf("event %d = %+v, want %+v", i, evs[i], w)
		}
	}
	if d := evs[4]; d.Language != "en" || d.Model != "stt-model" || d.Offset != 250*time.Millisecond {
		t.Errorf("done event = %+v", d)
	}

	mp := parseMultipart(t, srv.only(t))
	if mp.fields["stream"] != "true" || mp.fields["language"] != "en" || mp.fields["model"] != "stt-model" {
		t.Errorf("form fields = %v", mp.fields)
	}
	if _, ok := mp.fields["response_format"]; ok {
		t.Error("response_format sent on a streaming request")
	}
	if !bytes.Equal(mp.file[wavHeaderBytes:], append(append([]byte(nil), a...), b...)) {
		t.Error("the upload is not the audio of both Send calls in order")
	}
}

func TestTranscribeStreamCollect(t *testing.T) {
	f := pv.PCM16Mono(16000)
	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"transcript.text.delta","delta":"x"}`, `{"type":"transcript.text.done","text":"x y"}`, `[DONE]`)
	})
	c, _ := srv.client(t)
	s := newSTTStream(t, c, f)
	_ = s.Send(tonePCM(f, 100*time.Millisecond))
	_ = s.CloseSend()
	got, err := pv.CollectTranscript(s)
	if err != nil || got.Text != "x y" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestTranscribeStreamFailures(t *testing.T) {
	f := pv.PCM16Mono(16000)
	pcm := tonePCM(f, 100*time.Millisecond)
	cases := []struct {
		name string
		h    http.HandlerFunc
		want contracts.ErrCode
	}{
		{"error event mid-stream", func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"transcript.text.delta","delta":"par"}`, `{"type":"error","error":{"message":"failed to load model","type":"server_error"}}`, `[DONE]`)
		}, contracts.ErrResourceExhausted},
		{"plain error event", func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"error","error":{"message":"boom","type":"server_error"}}`, `[DONE]`)
		}, contracts.ErrInternal},
		{"body ends before [DONE]", func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"transcript.text.done","text":"complete looking"}`)
		}, contracts.ErrUnavailable},
		{"[DONE] without a final transcript", func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"transcript.text.delta","delta":"x"}`, `[DONE]`)
		}, contracts.ErrSchemaInvalid},
		{"event after the final transcript", func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"transcript.text.done","text":"a"}`, `{"type":"transcript.text.delta","delta":"b"}`, `[DONE]`)
		}, contracts.ErrSchemaInvalid},
		{"event that is not JSON", func(w http.ResponseWriter, _ *http.Request) { sse(w, `nonsense`) }, contracts.ErrSchemaInvalid},
		{"neither a stream nor JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>")
		}, contracts.ErrSchemaInvalid},
		{"HTTP failure", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "gone", http.StatusServiceUnavailable) }, contracts.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStub(t, tc.h)
			c, _ := srv.client(t)
			s := newSTTStream(t, c, f)
			_ = s.Send(pcm)
			_ = s.CloseSend()
			_, err := pv.CollectTranscript(s)
			wantCode(t, err, tc.want)
		})
	}
}

func TestTranscribeStreamEdges(t *testing.T) {
	f := pv.PCM16Mono(16000)

	t.Run("no audio completes without a request", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		s := newSTTStream(t, c, f)
		_ = s.CloseSend()
		got, err := pv.CollectTranscript(s)
		if err != nil || got.Text != "" {
			t.Fatalf("got %+v, %v", got, err)
		}
		if n := len(srv.requests()); n != 0 {
			t.Errorf("runtime received %d requests for an empty stream", n)
		}
	})
	t.Run("a runtime that ignores stream answers with JSON", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		s := newSTTStream(t, c, f)
		_ = s.Send(tonePCM(f, 100*time.Millisecond))
		_ = s.CloseSend()
		got, err := pv.CollectTranscript(s)
		if err != nil || got.Text != "hello" || got.Language != "en" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("Send honours the limits", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t, func(_ *localai.Config, cfg *Config) { cfg.MaxChunkBytes, cfg.MaxAudioBytes = 1000, 1500 })
		s := newSTTStream(t, c, f)
		wantCode(t, s.Send(make([]byte, 1002)), contracts.ErrValidationFailed) // over the chunk cap
		wantCode(t, s.Send(nil), contracts.ErrValidationFailed)                // empty
		wantCode(t, s.Send(make([]byte, 3)), contracts.ErrValidationFailed)    // not whole frames
		if err := s.Send(make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
		wantCode(t, s.Send(make([]byte, 600)), contracts.ErrValidationFailed) // over the stream total
	})
	t.Run("Close unblocks a waiting Recv", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		s := newSTTStream(t, c, f)
		res := make(chan error, 1)
		go func() { _, err := s.Recv(); res <- err }()
		time.Sleep(50 * time.Millisecond)
		_ = s.Close()
		select {
		case err := <-res:
			if !errors.Is(err, providers.ErrStreamClosed) {
				t.Fatalf("Recv = %v, want ErrStreamClosed", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Recv stayed blocked after Close")
		}
		if err := s.Send(make([]byte, 2)); !errors.Is(err, providers.ErrStreamClosed) {
			t.Errorf("Send after Close = %v", err)
		}
	})
	t.Run("Close cancels a request in flight", func(t *testing.T) {
		gone := make(chan struct{})
		srv := newStub(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(gone) })
		c, _ := srv.client(t)
		s := newSTTStream(t, c, f)
		_ = s.Send(tonePCM(f, 100*time.Millisecond))
		_ = s.CloseSend()
		res := make(chan error, 1)
		go func() { _, err := s.Recv(); res <- err }()
		for len(srv.requests()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		_ = s.Close()
		select {
		case err := <-res:
			if !errors.Is(err, providers.ErrStreamClosed) {
				t.Fatalf("Recv = %v, want ErrStreamClosed", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Recv stayed blocked after Close")
		}
		select {
		case <-gone:
		case <-time.After(3 * time.Second):
			t.Fatal("the runtime connection was not dropped")
		}
	})
	t.Run("a cancelled context ends a waiting Recv", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		ctx, cancel := context.WithCancel(bg)
		s, err := c.TranscribeStream(ctx, testScope(), pv.STTStreamRequest{Format: f})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		cancel()
		_, err = s.Recv()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Recv = %v, want a wrapped context.Canceled", err)
		}
	})
	t.Run("a bad open is refused", func(t *testing.T) {
		srv := newStub(t, validTranscript)
		c, _ := srv.client(t)
		_, err := c.TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: pv.PCM16Mono(1)})
		wantCode(t, err, contracts.ErrValidationFailed)
		_, err = c.TranscribeStream(bg, contracts.Scope{}, pv.STTStreamRequest{Format: f})
		wantCode(t, err, contracts.ErrScopeMissing)
		if n := len(srv.requests()); n != 0 {
			t.Errorf("runtime contacted %d times", n)
		}
	})
}
