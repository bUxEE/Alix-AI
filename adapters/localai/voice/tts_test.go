package voice

import (
	"bytes"
	"encoding/json"
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

func ttsReq(text string) pv.SynthesizeRequest { return pv.SynthesizeRequest{Text: text} }

func TestTTSStreamChunks(t *testing.T) {
	const rate = 24000
	f := pv.PCM16Mono(rate)
	pcm := tonePCM(f, 400*time.Millisecond)
	// Pieces that split sample frames in the middle: the stream must carry the
	// odd byte over so every chunk is whole frames.
	pieces := [][]byte{pcm[:3], pcm[3:8], pcm[8:5001], pcm[5001:5002], pcm[5002:]}

	firstOut, release := make(chan struct{}), make(chan struct{})
	srv := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		fl := w.(http.Flusher)
		_, _ = w.Write(streamedWAVHeader(rate))
		_, _ = w.Write(pieces[0])
		_, _ = w.Write(pieces[1])
		_, _ = w.Write(pieces[2])
		fl.Flush()
		close(firstOut)
		select { // the rest only after the client has read the first chunk
		case <-release:
		case <-r.Context().Done():
			return
		}
		for _, p := range pieces[3:] {
			_, _ = w.Write(p)
			fl.Flush()
		}
	})
	c, key := srv.client(t)
	s, err := c.SynthesizeStream(bg, testScope(), pv.SynthesizeRequest{Text: "Buongiorno", Voice: "anna", Language: "it-IT", Profile: "it-fast"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Incremental: a chunk arrives while the runtime is still synthesising.
	<-firstOut
	first, err := s.Recv()
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	rest, err := recvAll(s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with %v, want io.EOF after the Done chunk", err)
	}
	chunks := append([]pv.AudioChunk{first}, rest...)

	var got []byte
	for i, ch := range chunks {
		if ch.Seq != i+1 {
			t.Errorf("chunk %d has Seq %d", i, ch.Seq)
		}
		if ch.Format != f {
			t.Errorf("chunk %d format = %+v, want the rate the header states (%d Hz mono)", i, ch.Format, rate)
		}
		if len(ch.Data)%f.FrameBytes() != 0 {
			t.Errorf("chunk %d holds %d bytes: not whole sample frames", i, len(ch.Data))
		}
		if ch.Done != (i == len(chunks)-1) {
			t.Errorf("chunk %d Done = %v", i, ch.Done)
		}
		got = append(got, ch.Data...)
	}
	if !bytes.Equal(got, pcm) {
		t.Errorf("audio differs: got %d bytes, want %d", len(got), len(pcm))
	}
	if d := chunks[len(chunks)-1]; d.Model != "tts-model" || d.Timings.Total <= 0 {
		t.Errorf("done chunk = %+v", d)
	}

	r := srv.only(t)
	if r.Method != http.MethodPost || r.Path != "/v1/audio/speech" || r.Header.Get("Authorization") != "Bearer "+key {
		t.Fatalf("request = %s %s", r.Method, r.Path)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"model": "tts-model", "input": "Buongiorno", "voice": "anna", "language": "it", "response_format": "wav", "stream": true}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, body[k], v)
		}
	}
	if len(body) != len(want) {
		t.Errorf("body = %v, want exactly %v", body, want)
	}
}

func TestTTSCollectAndWholeFile(t *testing.T) {
	f := pv.PCM16Mono(22050)
	pcm := tonePCM(f, 300*time.Millisecond)

	t.Run("a complete file with extra header chunks, tts_no_stream", func(t *testing.T) {
		srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "audio/x-wav")
			h := wavHeader(f, len(pcm))
			// A LIST chunk between fmt and data, as encoders add.
			list := append([]byte("LIST\x05\x00\x00\x00abcde"), 0) // odd size, padded
			out := append(append(append([]byte(nil), h[:36]...), list...), h[36:]...)
			out = append(out, pcm...)
			out = append(out, []byte("trailing chunk, never read")...)
			_, _ = w.Write(out)
		})
		c, _ := srv.client(t, func(_ *localai.Config, cfg *Config) { cfg.TTSNoStream = true })
		s, err := c.SynthesizeStream(bg, testScope(), ttsReq("hello"))
		if err != nil {
			t.Fatal(err)
		}
		a, err := pv.CollectAudio(s, 1<<20)
		if err != nil || !bytes.Equal(a.Data, pcm) || a.Format != f {
			t.Fatalf("CollectAudio = %d bytes %+v, %v", len(a.Data), a.Format, err)
		}
		var body map[string]any
		_ = json.Unmarshal(srv.only(t).Body, &body)
		if _, streamed := body["stream"]; streamed {
			t.Errorf("stream flag sent although tts_no_stream is set: %v", body)
		}
	})
	t.Run("requested rate is passed on and the header decides the format", func(t *testing.T) {
		srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(append(streamedWAVHeader(16000), pcm[:1000]...))
		})
		c, _ := srv.client(t)
		req := ttsReq("hello")
		req.Format = pv.PCM16Mono(22050)
		s, err := c.SynthesizeStream(bg, testScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		a, err := pv.CollectAudio(s, 1<<20)
		if err != nil || a.Format != pv.PCM16Mono(16000) {
			t.Fatalf("CollectAudio = %+v, %v", a.Format, err)
		}
		var body map[string]any
		_ = json.Unmarshal(srv.only(t).Body, &body)
		if body["sample_rate"] != float64(22050) {
			t.Errorf("sample_rate = %v", body["sample_rate"])
		}
	})
}

func TestTTSStreamFailures(t *testing.T) {
	const rate = 16000
	pcm := tonePCM(pv.PCM16Mono(rate), 100*time.Millisecond)
	audio := func(w http.ResponseWriter) { w.Header().Set("Content-Type", "audio/wav") }
	cases := []struct {
		name string
		h    http.HandlerFunc
		want contracts.ErrCode
	}{
		{"connection cut mid-stream", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = w.Write(append(streamedWAVHeader(rate), pcm...))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // closes the connection without the chunked terminator
		}, contracts.ErrUnavailable},
		{"complete file shorter than its header says", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			h := wavHeader(pv.PCM16Mono(rate), len(pcm)+1000)
			_, _ = w.Write(append(h, pcm...))
		}, contracts.ErrUnavailable},
		{"body ends inside the header", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = w.Write(streamedWAVHeader(rate)[:30])
		}, contracts.ErrUnavailable},
		{"header and no audio", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = w.Write(streamedWAVHeader(rate))
		}, contracts.ErrSchemaInvalid},
		{"a half sample at the end", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = w.Write(append(streamedWAVHeader(rate), pcm[:101]...))
		}, contracts.ErrSchemaInvalid},
		{"not a WAV stream", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = io.WriteString(w, "ID3 this is an mp3, not what was asked for")
		}, contracts.ErrSchemaInvalid},
		{"8-bit audio", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			h := streamedWAVHeader(rate)
			h[34] = 8
			_, _ = w.Write(append(h, pcm...))
		}, contracts.ErrSchemaInvalid},
		{"a rate the seam does not carry", func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = w.Write(append(streamedWAVHeader(96000), pcm...))
		}, contracts.ErrSchemaInvalid},
		{"JSON instead of audio", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"error":"x"}`)
		}, contracts.ErrSchemaInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStub(t, tc.h)
			c, _ := srv.client(t)
			s, err := c.SynthesizeStream(bg, testScope(), ttsReq("hello"))
			if err == nil {
				defer s.Close()
				_, err = recvAll(s)
			}
			wantCode(t, err, tc.want)
		})
	}

	t.Run("audio past the response cap", func(t *testing.T) {
		srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
			audio(w)
			_, _ = w.Write(append(streamedWAVHeader(rate), make([]byte, 40000)...))
		})
		c, _ := srv.client(t, func(rt *localai.Config, _ *Config) { rt.MaxResponseBytes = 20000 })
		s, err := c.SynthesizeStream(bg, testScope(), ttsReq("hello"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		_, err = recvAll(s)
		wantCode(t, err, contracts.ErrResourceExhausted)
	})
	t.Run("a bad request never reaches the runtime", func(t *testing.T) {
		srv := newStub(t, func(http.ResponseWriter, *http.Request) {})
		c, _ := srv.client(t, func(_ *localai.Config, cfg *Config) { cfg.MaxTextRunes = 5 })
		for name, req := range map[string]pv.SynthesizeRequest{
			"empty text":      ttsReq("  "),
			"over the cap":    ttsReq("far too long"),
			"path as a voice": {Text: "hi", Voice: "../../etc/passwd"},
			"unknown profile": {Text: "hi", Profile: "nope"},
			"bad language":    {Text: "hi", Language: "??"},
		} {
			if _, err := c.SynthesizeStream(bg, testScope(), req); !contracts.HasCode(err, contracts.ErrValidationFailed) {
				t.Errorf("%s: error = %v, want validation_failed", name, err)
			}
		}
		if _, err := c.SynthesizeStream(bg, contracts.Scope{}, ttsReq("hi")); !contracts.HasCode(err, contracts.ErrScopeMissing) {
			t.Errorf("no scope: error = %v", err)
		}
		if n := len(srv.requests()); n != 0 {
			t.Errorf("runtime received %d requests for invalid input", n)
		}
	})
}

// Closing the stream is how playback is cancelled: it must drop the runtime
// connection at once and unblock a Recv that is waiting.
func TestTTSCloseCancelsSynthesis(t *testing.T) {
	const rate = 16000
	gone := make(chan struct{})
	srv := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(append(streamedWAVHeader(rate), make([]byte, 2000)...))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a long synthesis that only ends when the client leaves
		close(gone)
	})
	c, _ := srv.client(t)
	s, err := c.SynthesizeStream(bg, testScope(), ttsReq("a long text"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := s.Recv(); res <- err }() // blocks: no more audio is coming
	time.Sleep(50 * time.Millisecond)
	_ = s.Close()
	_ = s.Close() // idempotent
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
	if _, err := s.Recv(); !errors.Is(err, providers.ErrStreamClosed) {
		t.Errorf("Recv after Close = %v", err)
	}
}
