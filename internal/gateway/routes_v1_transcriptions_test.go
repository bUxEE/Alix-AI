package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	voicegen "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// Scripted TranscriptionService for the dictation routes.

type transcriptionsTestFake struct {
	mu    sync.Mutex
	calls []string
	caps  voicegen.TranscriptionCapabilities
	text  string
}

func (f *transcriptionsTestFake) Capabilities(_ context.Context, _ contracts.Scope) (voicegen.TranscriptionCapabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Capabilities")
	return f.caps, nil
}

func (f *transcriptionsTestFake) MaxSeconds(_ context.Context, _ contracts.Scope) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.caps.MaxSeconds > 0 {
		return f.caps.MaxSeconds, nil
	}
	return transcriptionsDefaultMaxSeconds, nil
}

func (f *transcriptionsTestFake) Transcribe(_ context.Context, _ contracts.Scope, audio []byte, language string) (voicegen.DictationTranscript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Transcribe")
	lang := language
	if lang == "" {
		lang = "en"
	}
	return voicegen.DictationTranscript{
		Text:        f.text,
		Language:    voicegen.Language(lang),
		DurationMS:  int64(len(audio)),
		Locality:    voicegen.DictationTranscriptLocalityLocal,
		SegmentKind: "user_recorded",
	}, nil
}

var transcriptionsFakeSets sync.Map

func init() {
	registerTestServices("v1_transcriptions", func(t testing.TB, e *apiEnv, s *Services) {
		f := &transcriptionsTestFake{
			caps: voicegen.TranscriptionCapabilities{
				Available:    true,
				Locality:     voicegen.TranscriptionCapabilitiesLocalityLocal,
				MaxSeconds:   transcriptionsDefaultMaxSeconds,
				SampleRateHz: transcriptionsSampleRateHz,
				Format:       "audio/wav",
				Languages:    []voicegen.Language{"en", "it"},
			},
			text: "hello from dictation",
		}
		transcriptionsFakeSets.Store(e, f)
		if err := Provide[TranscriptionService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func transcriptionsFakeOf(e *apiEnv) *transcriptionsTestFake {
	v, _ := transcriptionsFakeSets.Load(e)
	return v.(*transcriptionsTestFake)
}

func TestTranscriptionRoutesMatchInventory(t *testing.T) {
	e := newAPIEnv(t)
	got := e.srv.AreaRoutes("v1_transcriptions")
	want := []string{
		"GET /v1/dictations/capabilities",
		"POST /v1/sessions/{session_id}/dictations",
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] && (got[0] != want[0] || got[1] != want[1]) {
			// order from the mux may vary; check set membership
			break
		}
	}
	have := map[string]bool{}
	for _, r := range got {
		have[r] = true
	}
	for _, r := range want {
		if !have[r] {
			t.Errorf("missing route %s (have %v)", r, got)
		}
	}
}

func TestTranscriptionCapabilitiesAndTranscribe(t *testing.T) {
	e := newAPIEnv(t)
	_ = transcriptionsFakeOf(e)

	caps := e.call(http.MethodGet, "/v1/dictations/capabilities", nil)
	wantStatus(t, caps, http.StatusOK)
	settingsValidateSpec(t, "TranscriptionCapabilities", caps.Body.Bytes())

	wantError(t, e.call(http.MethodGet, "/v1/dictations/capabilities?x=1", nil), http.StatusBadRequest, contracts.ErrValidationFailed)

	wav := []byte("RIFF....WAVEfmt ") // body shape is the service's concern; gateway only checks Content-Type and size
	rec := e.callRaw(e.token(), http.MethodPost, "/v1/sessions/"+apiSess+"/dictations", "audio/wav", wav)
	wantStatus(t, rec, http.StatusOK)
	settingsValidateSpec(t, "DictationTranscript", rec.Body.Bytes())
	var got voicegen.DictationTranscript
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text == "" || got.SegmentKind != "user_recorded" {
		t.Fatalf("transcript = %+v", got)
	}

	wantError(t, e.callRaw(e.token(), http.MethodPost, "/v1/sessions/"+apiSess+"/dictations", "application/json", wav), http.StatusUnsupportedMediaType, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodPost, "/v1/sessions/ses_other/dictations", nil), http.StatusForbidden, contracts.ErrScopeViolation)

	limit := transcriptionsWAVHeaderMax + transcriptionsDefaultMaxSeconds*transcriptionsBytesPerSecond
	tooBig := make([]byte, limit+1)
	big := e.callRaw(e.token(), http.MethodPost, "/v1/sessions/"+apiSess+"/dictations", "audio/wav", tooBig)
	if big.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413 (%s)", big.Code, big.Body.String())
	}
	wantError(t, big, http.StatusRequestEntityTooLarge, contracts.ErrValidationFailed)
}

func TestTranscriptionBodyLimitFollowsMaxSeconds(t *testing.T) {
	e := newAPIEnv(t)
	f := transcriptionsFakeOf(e)
	f.caps.MaxSeconds = 1 // 1 s → small body cap
	limit := transcriptionsWAVHeaderMax + 1*transcriptionsBytesPerSecond
	tooBig := make([]byte, limit+1)
	big := e.callRaw(e.token(), http.MethodPost, "/v1/sessions/"+apiSess+"/dictations", "audio/wav", tooBig)
	if big.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized for max_seconds=1 = %d, want 413 (%s)", big.Code, big.Body.String())
	}
}

func TestTranscriptionRoutesUnavailableWithoutService(t *testing.T) {
	e := newAPIEnv(t)
	h := &transcriptionsHandler{apps: e.apps, svc: nil}
	scope := e.scopeOf()
	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.capabilities(rec, httptest.NewRequest(http.MethodGet, "/v1/dictations/capabilities", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("capabilities = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}
