package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/adapters/localai"
	lvoice "github.com/buxee/alix-ai/adapters/localai/voice"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/voice/packs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// voiceLocalAIFullManifest is a pack with STT, TTS and VAD whose TTS engine is ttsEngine.
func voiceLocalAIFullManifest(id, ttsEngine string) []byte {
	role := func(r, engine, file string) string {
		return fmt.Sprintf(`  %s:
    name: "%s model"
    engine: %s
    registry_id: null
    files:
      - name: %s
        url: https://example.test/%s
        revision: 0123456789abcdef0123456789abcdef01234567
        size_bytes: 10
        sha256: %s
    licence:
      spdx: MIT
      url: https://example.test/licence
      redistribution_notes: "test licence"
`, r, r, engine, file, file, strings.Repeat("ab", 32))
	}
	return []byte(fmt.Sprintf(`id: %s
name: "Test pack %s"
version: "1"
language: en
status: candidate
models:
%s%s%squality:
  stt_wer: null
  tts_time_to_first_audio_ms: null
  tts_real_time_factor: null
  measurement: null
resources:
  disk_bytes: 30
  steady_rss_mb: null
  peak_rss_mb: null
  cpu_cores: null
notes: []
`, id, id, role("stt", "whisper.cpp", "stt-"+id+".bin"), role("tts", ttsEngine, "tts-"+id+".onnx"), role("vad", "whisper.cpp", "vad-"+id+".bin")))
}

// voiceLocalAIUseManifests approves the packs piper, kokoro and other, each with all three roles.
func voiceLocalAIUseManifests(t *testing.T) {
	t.Helper()
	old := voiceShippedManifests
	t.Cleanup(func() { voiceShippedManifests = old })
	voiceShippedManifests = func() (packs.Manifests, error) {
		return packs.LoadManifests(fstest.MapFS{
			"piper.yaml":  {Data: voiceLocalAIFullManifest("piper", "piper")},
			"kokoro.yaml": {Data: voiceLocalAIFullManifest("kokoro", "kokoro")},
			"other.yaml":  {Data: voiceLocalAIFullManifest("other", "xtts")},
		})
	}
}

// voiceLocalAIUseRuntime points the localai section at endpoint, and puts everything back.
func voiceLocalAIUseRuntime(t *testing.T, endpoint string) {
	t.Helper()
	oldRT, oldInf := localaiSection, inferenceSection
	t.Cleanup(func() { localaiSection, inferenceSection = oldRT, oldInf })
	localaiSection = localai.Config{Endpoint: endpoint}
	inferenceSection = inferenceConfig{}
}

// voiceLocalAISpeechServer records the JSON body of every /v1/audio/speech request and refuses it
// (the request is what the test reads, not the answer).
type voiceLocalAISpeechServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func voiceLocalAINewSpeechServer(t *testing.T) *voiceLocalAISpeechServer {
	t.Helper()
	s := &voiceLocalAISpeechServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		s.mu.Lock()
		s.bodies = append(s.bodies, m)
		s.mu.Unlock()
		http.Error(w, "refused", http.StatusServiceUnavailable)
	}))
	t.Cleanup(s.Close)
	return s
}

func voiceLocalAIScope() contracts.Scope { return voiceSessionScope() }

// speechBody makes one TTS request through tts and returns the JSON the runtime received.
func (s *voiceLocalAISpeechServer) speechBody(t *testing.T, tts voice.TTS) map[string]any {
	t.Helper()
	_, _ = tts.SynthesizeStream(context.Background(), voiceLocalAIScope(), voice.SynthesizeRequest{Text: "hello"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		t.Fatal("the runtime received no speech request")
	}
	return s.bodies[len(s.bodies)-1]
}

func TestVoiceProviderFactoryBuildsTheLocalAIVoiceClient(t *testing.T) {
	voiceLocalAIUseManifests(t)
	voiceResetConfig(t)
	voiceEnableConfig(t, "piper")
	srv := voiceLocalAINewSpeechServer(t)
	voiceLocalAIUseRuntime(t, srv.URL)

	got, err := voiceProviderFactory(context.Background())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if got.STT == nil || got.TTS == nil || got.VAD == nil {
		t.Fatalf("a pack with all three roles must give all three providers: stt %v tts %v vad %v", got.STT, got.TTS, got.VAD)
	}
	for name, p := range map[string]any{"stt": got.STT, "tts": got.TTS, "vad": got.VAD} {
		if _, ok := p.(*lvoice.Client); !ok {
			t.Errorf("%s provider is %T, want the LocalAI voice client", name, p)
		}
	}
	// The client reaches the configured runtime with the pack's model for the role.
	body := srv.speechBody(t, got.TTS)
	if body["model"] != "tts-piper.onnx" {
		t.Errorf("TTS model = %v, want the first file of the pack's TTS model", body["model"])
	}
	if strings.Contains(fmt.Sprint(got.TTS), "secret") {
		t.Errorf("client description leaks something: %v", got.TTS)
	}

	// A pack that provides fewer roles gives fewer providers, found through the recorded activation
	// when the configuration names none. Test manifests are STT+VAD (conversation-capable, no TTS).
	t.Run("an STT+VAD pack and the recorded activation", func(t *testing.T) {
		voiceUseTestManifests(t)
		voiceResetConfig(t)
		voiceEnableConfig(t, "")
		voiceLocalAIUseRuntime(t, srv.URL) // after the configuration load, which resets the sections
		r := newVoiceServeRig(t)
		r.installVoicePack(t, "en", true)
		old := voiceLocalAIDataDir
		t.Cleanup(func() { voiceLocalAIDataDir = old })
		voiceLocalAIDataDir = func() string { return r.dataDr }
		got, err := voiceProviderFactory(context.Background())
		if err != nil {
			t.Fatalf("factory: %v", err)
		}
		if got.STT == nil || got.VAD == nil || got.TTS != nil {
			t.Errorf("an STT+VAD pack must give STT and VAD only: stt %v tts %v vad %v", got.STT, got.TTS, got.VAD)
		}
	})
}

func TestVoiceStaysTextOnlyWithoutARuntimeOrAPack(t *testing.T) {
	voiceLocalAIUseManifests(t)
	srv := voiceLocalAINewSpeechServer(t)

	cases := []struct {
		name     string
		pack     string // voice.active_pack
		endpoint string // localai.endpoint
		models   []string
		want     string
	}{
		{"no runtime", "piper", "", nil, "no LocalAI runtime"},
		{"no active pack", "", srv.URL, nil, "no language pack is active"},
		{"a pack this binary does not approve", "unknown", srv.URL, nil, "not an approved pack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			voiceResetConfig(t)
			voiceEnableConfig(t, tc.pack)
			voiceLocalAIUseRuntime(t, tc.endpoint)
			inferenceSection = inferenceConfig{Models: tc.models}
			old := voiceLocalAIDataDir
			t.Cleanup(func() { voiceLocalAIDataDir = old })
			voiceLocalAIDataDir = func() string { return t.TempDir() } // no marker anywhere

			got, err := voiceProviderFactory(context.Background())
			if !contracts.HasCode(err, contracts.ErrUnavailable) {
				t.Fatalf("err = %v, want unavailable", err)
			}
			if !strings.Contains(err.Error(), "no voice runtime adapter") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to say %q", err, tc.want)
			}
			if got.STT != nil || got.TTS != nil || got.VAD != nil || got.Close != nil {
				t.Errorf("providers were built although the factory refused: %+v", got)
			}
		})
	}

	t.Run("a configured inference path is a runtime", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "piper")
		voiceLocalAIUseRuntime(t, "")
		inferenceSection = inferenceConfig{ModelsDir: "m", Models: []string{"granite-4.0-350m-q4_k_m"}}
		if _, err := voiceProviderFactory(context.Background()); err != nil {
			t.Fatalf("factory: %v", err)
		}
	})

	t.Run("an endpoint the policy refuses", func(t *testing.T) {
		voiceResetConfig(t)
		voiceEnableConfig(t, "piper")
		voiceLocalAIUseRuntime(t, "http://203.0.113.7:8080") // plaintext to a non-loopback host
		if _, err := voiceProviderFactory(context.Background()); err == nil {
			t.Fatal("a plaintext non-loopback endpoint must be refused")
		}
	})
}

func TestTTSNoStreamFollowsThePackEngine(t *testing.T) {
	voiceLocalAIUseManifests(t)
	for engine, want := range map[string]bool{
		"piper": true, "kokoro": true, "Piper": true, " kokoro ": true,
		"xtts": false, "whisper.cpp": false, "": false,
	} {
		if got := voiceLocalAITTSNoStream(engine); got != want {
			t.Errorf("voiceLocalAITTSNoStream(%q) = %v, want %v", engine, got, want)
		}
	}

	// End to end: what the runtime is asked for follows the engine of the active pack.
	for _, tc := range []struct {
		pack     string
		wantStrm bool
	}{{"piper", false}, {"kokoro", false}, {"other", true}} {
		t.Run(tc.pack, func(t *testing.T) {
			voiceResetConfig(t)
			voiceEnableConfig(t, tc.pack)
			srv := voiceLocalAINewSpeechServer(t)
			voiceLocalAIUseRuntime(t, srv.URL)
			got, err := voiceProviderFactory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			body := srv.speechBody(t, got.TTS)
			stream, _ := body["stream"].(bool)
			if stream != tc.wantStrm {
				t.Errorf("pack %s: stream = %v, want %v", tc.pack, stream, tc.wantStrm)
			}
		})
	}
}
