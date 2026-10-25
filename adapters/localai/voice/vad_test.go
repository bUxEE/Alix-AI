package voice

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/providers/voice/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestVADSegments(t *testing.T) {
	f := pv.PCM16Mono(16000)
	// 0.5 s silence, 1 s tone, 0.5 s silence: 2 s of audio.
	audio := fake.Speech(fake.Silence(f, 500*time.Millisecond), fake.Tone(f, time.Second, 8192), fake.Silence(f, 500*time.Millisecond))
	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"segments":[{"start":0.5,"end":1.5}]}`)
	})
	c, key := srv.client(t)

	got, err := c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: audio, Format: f})
	if err != nil {
		t.Fatal(err)
	}
	want := []pv.SpeechSegment{{Start: 500 * time.Millisecond, End: 1500 * time.Millisecond}}
	if len(got.Segments) != 1 || got.Segments[0] != want[0] || got.Model != "vad-model" {
		t.Fatalf("result = %+v, want segments %v", got, want)
	}

	r := srv.only(t)
	if r.Method != http.MethodPost || r.Path != "/v1/vad" || r.Header.Get("Authorization") != "Bearer "+key {
		t.Fatalf("request = %s %s", r.Method, r.Path)
	}
	var body struct {
		Model string    `json:"model"`
		Audio []float64 `json:"audio"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "vad-model" || len(body.Audio) != len(audio)/2 {
		t.Fatalf("body has model %q and %d samples, want vad-model and %d", body.Model, len(body.Audio), len(audio)/2)
	}
	// Samples are the PCM scaled by 1/32768: silence first, then the tone.
	if body.Audio[0] != 0 || body.Audio[8000+1] == 0 {
		t.Errorf("samples do not follow the audio: %v ... %v", body.Audio[0], body.Audio[8001])
	}
	for i, s := range body.Audio {
		if s < -1 || s > 1 {
			t.Fatalf("sample %d = %v is outside [-1, 1]", i, s)
		}
	}
}

func TestVADAnswers(t *testing.T) {
	f := pv.PCM16Mono(16000)
	audio := fake.Silence(f, 2*time.Second)
	run := func(t *testing.T, body string) (pv.DetectResult, error) {
		srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) })
		c, _ := srv.client(t)
		return c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: audio, Format: f})
	}
	t.Run("no speech", func(t *testing.T) {
		for _, body := range []string{`{"segments":null}`, `{"segments":[]}`} {
			res, err := run(t, body)
			if err != nil || len(res.Segments) != 0 {
				t.Errorf("%s: got %+v, %v", body, res, err)
			}
		}
	})
	t.Run("several segments in order", func(t *testing.T) {
		res, err := run(t, `{"segments":[{"start":0.1,"end":0.4},{"start":0.4,"end":1.99}]}`)
		if err != nil || len(res.Segments) != 2 || res.Segments[1].End != 1990*time.Millisecond {
			t.Fatalf("got %+v, %v", res, err)
		}
	})
	for name, body := range map[string]string{
		"no segments key":           `{}`,
		"an error object":           `{"error":{"message":"x"}}`,
		"not JSON":                  `nope`,
		"segments not a list":       `{"segments":{"start":0}}`,
		"segment without an end":    `{"segments":[{"start":0.1}]}`,
		"negative start":            `{"segments":[{"start":-1,"end":0.5}]}`,
		"end before start":          `{"segments":[{"start":1,"end":0.5}]}`,
		"past the end of the audio": `{"segments":[{"start":0.5,"end":9}]}`,
		"overlapping":               `{"segments":[{"start":0.1,"end":1},{"start":0.5,"end":1.5}]}`,
		"absurd time":               `{"segments":[{"start":0,"end":1e300}]}`,
	} {
		if _, err := run(t, body); !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
			t.Errorf("%s: error = %v, want schema_invalid", name, err)
		}
	}
}

func TestVADRefusesWhatTheRuntimeCannotTake(t *testing.T) {
	srv := newStub(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, `{"segments":[]}`) })
	c, _ := srv.client(t)
	for _, f := range []pv.AudioFormat{
		pv.PCM16Mono(8000),
		pv.PCM16Mono(48000),
		{Encoding: pv.EncodingPCM16, SampleRate: 16000, Channels: 2},
	} {
		_, err := c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: fake.Silence(f, time.Second), Format: f})
		wantCode(t, err, contracts.ErrValidationFailed)
	}
	f := pv.PCM16Mono(16000)
	_, err := c.DetectSpeech(bg, testScope(), pv.DetectRequest{Audio: fake.Silence(f, time.Second), Format: f, Profile: "nope"})
	wantCode(t, err, contracts.ErrValidationFailed)
	_, err = c.DetectSpeech(bg, contracts.Scope{}, pv.DetectRequest{Audio: fake.Silence(f, time.Second), Format: f})
	wantCode(t, err, contracts.ErrScopeMissing)
	if n := len(srv.requests()); n != 0 {
		t.Errorf("runtime received %d requests for audio it cannot take", n)
	}
}

func TestVADAudioJSON(t *testing.T) {
	pcm := []byte{0x00, 0x00, 0xFF, 0x7F, 0x00, 0x80, 0x01, 0x00} // 0, 32767, -32768, 1
	var got struct {
		Model string    `json:"model"`
		Audio []float64 `json:"audio"`
	}
	if err := json.Unmarshal(vadAudioJSON("m", pcm), &got); err != nil {
		t.Fatal(err)
	}
	want := []float64{0, 32767.0 / 32768, -1, 1.0 / 32768}
	for i, w := range want {
		if d := got.Audio[i] - w; d > 1e-6 || d < -1e-6 {
			t.Errorf("sample %d = %v, want %v", i, strconv.FormatFloat(got.Audio[i], 'g', -1, 64), w)
		}
	}
	if got.Model != "m" || len(got.Audio) != 4 {
		t.Errorf("decoded %+v", got)
	}
}
