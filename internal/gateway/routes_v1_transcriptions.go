package gateway

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"

	"github.com/buxee/alix-ai/pkg/contracts"
	voicegen "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// Dictation (record-to-text) of the public API (1.1.0). The contract is the
// transcription paths of schemas/api/paths/voice.yaml and the schemas under
// schemas/voice (transcription-capabilities, dictation-transcript).
// TestRoutesMatchOpenAPISpec fails on any difference between the routes
// registered here and the document.
//
//	GET  /v1/dictations/capabilities
//	POST /v1/sessions/{session_id}/dictations
//
// Dictation is not the voice-conversation WebSocket: no VAD, the user's record
// and stop are the segmentation, the transcript is returned for the host to put
// in the chat input, and AlixAI never submits it as a turn. Raw audio is not
// retained. Paths use "dictations" (not "transcriptions") so they are not
// mistaken for an OpenAI-compatible surface. The area Needs only
// ApplicationService and Wants TranscriptionService so rigs that provide only
// the application service keep starting; without the service every route
// answers unavailable.

func init() { RegisterArea("v1_transcriptions", registerV1Transcriptions) }

// transcriptionsDefaultMaxSeconds is used when the service reports nothing.
// Body size = WAV header + max_seconds * 16000 Hz * 2 bytes (PCM s16le mono).
const (
	transcriptionsDefaultMaxSeconds = 60
	transcriptionsHardMaxSeconds    = 300
	transcriptionsWAVHeaderMax      = 256
	transcriptionsSampleRateHz      = 16000
	transcriptionsBytesPerSecond    = transcriptionsSampleRateHz * 2 // s16le mono
)

var transcriptionsLanguagePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// TranscriptionService is what the dictation routes need. The composition root
// provides it when a recogniser is available; until then every route answers
// unavailable. Every method takes the verified Scope and answers for it alone.
type TranscriptionService interface {
	// Capabilities reports whether dictation is available for the scope.
	Capabilities(ctx context.Context, scope contracts.Scope) (voicegen.TranscriptionCapabilities, error)
	// MaxSeconds is the longest clip the route may accept for the scope. The
	// gateway derives the body-size cap from it (bounded by a hard ceiling of
	// 300 s) and reads the body only after scope and content-type checks.
	MaxSeconds(ctx context.Context, scope contracts.Scope) (int64, error)
	// Transcribe recognises one user-recorded WAV clip for the session named by
	// the scope. audio is the raw body (audio/wav); language is an optional BCP 47
	// hint. The implementation computes duration from the PCM and never trusts a
	// client duration; raw audio must not be retained after the call.
	Transcribe(ctx context.Context, scope contracts.Scope, audio []byte, language string) (voicegen.DictationTranscript, error)
}

type transcriptionsHandler struct {
	apps ApplicationService
	svc  TranscriptionService // nil while none is provided
}

func registerV1Transcriptions(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	svc, _ := Want[TranscriptionService](n)
	if !n.Ready() {
		return
	}
	h := &transcriptionsHandler{apps: apps, svc: svc}
	v1 := rt.V1()
	v1.HandleFunc("GET /dictations/capabilities", h.capabilities)
	v1.HandleFunc("POST /sessions/{session_id}/dictations", h.transcribe)
}

func (h *transcriptionsHandler) begin(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	if h.svc == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "dictation is not available on this server"))
		return contracts.Scope{}, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return scope, true
}

func (h *transcriptionsHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the capabilities route takes no query: the scope is the verified token"))
		return
	}
	got, err := h.svc.Capabilities(r.Context(), scope)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if got.Languages == nil {
		got.Languages = []voicegen.Language{}
	}
	writeJSON(w, http.StatusOK, got)
}

func (h *transcriptionsHandler) bodyLimit(ctx context.Context, scope contracts.Scope) int64 {
	sec := int64(transcriptionsDefaultMaxSeconds)
	if s, err := h.svc.MaxSeconds(ctx, scope); err == nil && s > 0 {
		sec = s
	}
	if sec > transcriptionsHardMaxSeconds {
		sec = transcriptionsHardMaxSeconds
	}
	return int64(transcriptionsWAVHeaderMax) + sec*int64(transcriptionsBytesPerSecond)
}

func (h *transcriptionsHandler) transcribe(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.begin(w, r)
	if !ok {
		return
	}
	if !sessionPath(w, r, scope) {
		return
	}
	language := r.URL.Query().Get("language")
	if language != "" && (len(language) > 35 || !transcriptionsLanguagePattern.MatchString(language)) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "language is not a language tag"))
		return
	}
	for k := range r.URL.Query() {
		if k != "language" {
			WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the query holds a parameter this route does not take: the scope is the verified token"))
			return
		}
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "audio/wav" {
		WriteErrorStatus(w, r, http.StatusUnsupportedMediaType, contracts.New(contracts.ErrValidationFailed, "Content-Type must be audio/wav"))
		return
	}
	limit := h.bodyLimit(r.Context(), scope)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			WriteError(w, r, err) // 413 + validation_failed via problemFor
			return
		}
		WriteError(w, r, err)
		return
	}
	if len(raw) == 0 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the audio body is empty"))
		return
	}
	got, err := h.svc.Transcribe(r.Context(), scope, raw, language)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, got)
}
