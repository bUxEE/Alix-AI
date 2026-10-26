package sdktest

import (
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

func (f *Fake) SetDictationUnavailable(unavailable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dictationUnavailable = unavailable
}

func (f *Fake) SetDictationDisabled(disabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dictationDisabled = disabled
}

func (f *Fake) SetDictationSilent(silent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dictationSilent = silent
}

func (f *Fake) SetDictationText(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dictationText = text
}

func (c *call) dictationCapabilities() {
	// serve() already holds f.mu for the request.
	if c.f.dictationUnavailable {
		c.fail(http.StatusServiceUnavailable, contracts.ErrUnavailable, "the server has no transcription service")
		return
	}
	available := !c.f.dictationDisabled
	langs := []voice.Language{}
	locality := voice.TranscriptionCapabilitiesLocalityNone
	if available {
		langs = []voice.Language{"en", "it"}
		locality = voice.TranscriptionCapabilitiesLocalityLocal
	}
	c.json(http.StatusOK, voice.TranscriptionCapabilities{
		Available:    available,
		Locality:     locality,
		MaxSeconds:   60,
		SampleRateHz: 16000,
		Format:       "audio/wav",
		Languages:    langs,
	})
}

func (c *call) dictationCreate() {
	sessionID := c.param("session_id")
	if sessionID != c.claims.SessionID {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the session is not the token's")
		return
	}
	unavailable := c.f.dictationUnavailable || c.f.dictationDisabled
	silent := c.f.dictationSilent
	text := c.f.dictationText
	if unavailable {
		c.fail(http.StatusServiceUnavailable, contracts.ErrUnavailable, "the server has no transcription service")
		return
	}
	ctype := strings.Split(c.r.Header.Get("Content-Type"), ";")[0]
	ctype = strings.TrimSpace(strings.ToLower(ctype))
	if ctype != "audio/wav" {
		c.fail(http.StatusUnsupportedMediaType, contracts.ErrValidationFailed, "Content-Type must be audio/wav")
		return
	}
	body, ok := c.readBody()
	if !ok {
		return
	}
	if len(body) < 44 || string(body[0:4]) != "RIFF" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the body is not a WAV", "reason", "malformed_wav")
		return
	}
	if silent {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the clip is silent or too short", "reason", "silent")
		return
	}
	if text == "" {
		text = "book Tuesday"
	}
	language := c.r.URL.Query().Get("language")
	if language == "" {
		language = "en"
	}
	duration := int64(1)
	if len(body) > 44 {
		if ms := int64((len(body) - 44) / 32); ms > 0 {
			duration = ms
		}
	}
	c.json(http.StatusOK, voice.DictationTranscript{
		Text:        text,
		Language:    voice.Language(language),
		DurationMS:  duration,
		Locality:    voice.DictationTranscriptLocalityLocal,
		SegmentKind: "user_recorded",
	})
}
