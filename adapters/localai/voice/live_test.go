//go:build live

package voice

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
)

// Live tests run against a real LocalAI with voice models installed, for
// example the container of deploy/container/localai (M4-06 runs them):
//
//	ALIXAI_LOCALAI_URL=http://127.0.0.1:8080 \
//	ALIXAI_LOCALAI_STT_MODEL=whisper-tiny ALIXAI_LOCALAI_TTS_MODEL=... ALIXAI_LOCALAI_VAD_MODEL=silero-vad \
//	  go test -tags live -count=1 -v ./adapters/localai/voice/
//
// ALIXAI_LOCALAI_KEY is optional (the runtime's API key). Each test needs its
// own models and is skipped without them; without ALIXAI_LOCALAI_URL every test
// here is skipped. The audio is synthetic (silence, a tone, the TTS model's own
// speech): no recording of a person is used.

func liveClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("ALIXAI_LOCALAI_URL")
	if url == "" {
		t.Skip("ALIXAI_LOCALAI_URL is not set")
	}
	cfg := Config{Models: ModelSet{
		STT: os.Getenv("ALIXAI_LOCALAI_STT_MODEL"),
		TTS: os.Getenv("ALIXAI_LOCALAI_TTS_MODEL"),
		VAD: os.Getenv("ALIXAI_LOCALAI_VAD_MODEL"),
	}}
	opts := []Option{}
	if key := os.Getenv("ALIXAI_LOCALAI_KEY"); key != "" {
		opts = append(opts, WithAPIKey(key))
	}
	// The first call loads the backend and the model.
	c, err := New(localai.Config{Endpoint: url, RequestTimeout: 5 * time.Minute}, cfg, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func needModel(t *testing.T, role, name string) {
	t.Helper()
	if name == "" {
		t.Skipf("no %s model configured (ALIXAI_LOCALAI_%s_MODEL)", role, strings.ToUpper(role))
	}
}

func TestLiveTranscribeSilence(t *testing.T) {
	c := liveClient(t)
	needModel(t, "stt", c.cfg.Models.STT)
	f := pv.PCM16Mono(16000)
	got, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: fake.Silence(f, 2*time.Second), Format: f, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("transcript of silence = %+v", got)
}

func TestLiveSpeechRoundTrip(t *testing.T) {
	c := liveClient(t)
	needModel(t, "tts", c.cfg.Models.TTS)
	needModel(t, "stt", c.cfg.Models.STT)
	s, err := c.SynthesizeStream(bg, testScope(), pv.SynthesizeRequest{Text: "Hello world, this is a test.", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	audio, err := pv.CollectAudio(s, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("synthesized %d bytes at %+v in %v", len(audio.Data), audio.Format, audio.Timings.Total)

	got, err := c.Transcribe(bg, testScope(), pv.TranscribeRequest{Audio: audio.Data, Format: audio.Format, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("transcript = %+v", got)
	if !strings.Contains(strings.ToLower(got.Text), "hello") {
		t.Errorf("transcript %q does not contain the spoken word", got.Text)
	}

	// The streaming path over the same audio.
	st, err := c.TranscribeStream(bg, testScope(), pv.STTStreamRequest{Format: audio.Format, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(audio.Data); i += 4096 {
		if err := st.Send(audio.Data[i:min(i+4096, len(audio.Data))]); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.CloseSend()
	streamed, err := pv.CollectTranscript(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(streamed.Text), "hello") {
		t.Errorf("streamed transcript %q does not contain the spoken word", streamed.Text)
	}
}

func TestLiveVAD(t *testing.T) {
	c := liveClient(t)
	needModel(t, "vad", c.cfg.Models.VAD)
	f := pv.PCM16Mono(16000)
	res, err := c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: fake.Silence(f, 2*time.Second), Format: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Segments) != 0 {
		t.Errorf("silence produced segments: %+v", res.Segments)
	}
	t.Logf("vad of silence = %+v", res)
}
