package sdkgo

import (
	"context"
	"fmt"
	"net/url"
	"regexp"

	"github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

var dictationLanguage = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// DictationClient is record-to-text (1.1.0): capabilities and one-shot clip transcription.
// Not the voice WebSocket. The transcript is data for the host; AlixAI never submits it as a turn.
type DictationClient struct{ c *Client }

// Dictations returns the dictation (record-to-text) client.
func (c *Client) Dictations() *DictationClient { return &DictationClient{c: c} }

// Capabilities is GET /v1/dictations/capabilities.
func (d *DictationClient) Capabilities(ctx context.Context, options ...CallOption) (*voice.TranscriptionCapabilities, error) {
	req := request{op: opGetDictationCapabilities, accept: "application/json"}
	return doJSON(ctx, d.c.t, req, applyOptions(options).deadline(d.c.timeout), checkTranscriptionCapabilities)
}

// DictationCreateOptions are optional arguments for Create.
type DictationCreateOptions struct {
	Language string
}

// Create is POST /v1/sessions/{session_id}/dictations with raw audio/wav (PCM s16le mono 16 kHz).
func (d *DictationClient) Create(ctx context.Context, sessionID string, wav []byte, createOpts *DictationCreateOptions, options ...CallOption) (*voice.DictationTranscript, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	if len(wav) < 44 {
		return nil, invalidArgument("wav", "audio must be WAV bytes (PCM s16le mono 16 kHz)")
	}
	query := url.Values{}
	if createOpts != nil && createOpts.Language != "" {
		if len(createOpts.Language) > 35 || !dictationLanguage.MatchString(createOpts.Language) {
			return nil, invalidArgument("language", "the language is not a BCP 47 language tag")
		}
		query.Set("language", createOpts.Language)
	}
	req := request{
		op:          opCreateSessionDictation,
		params:      map[string]string{"session_id": sessionID},
		query:       query,
		accept:      "application/json",
		contentType: "audio/wav",
		body:        wav,
	}
	return doJSON(ctx, d.c.t, req, applyOptions(options).deadline(d.c.timeout), checkDictationTranscript)
}

func checkTranscriptionCapabilities(c *voice.TranscriptionCapabilities) error {
	if !c.Locality.Valid() {
		return fmt.Errorf("transcription capabilities: locality is unknown")
	}
	if c.SampleRateHz != 16000 || c.Format != "audio/wav" {
		return fmt.Errorf("transcription capabilities: sample_rate_hz and format must be the fixed WAV shape")
	}
	if c.MaxSeconds < 1 {
		return fmt.Errorf("transcription capabilities: max_seconds must be at least 1")
	}
	return nil
}

func checkDictationTranscript(t *voice.DictationTranscript) error {
	if t.SegmentKind != "user_recorded" {
		return fmt.Errorf("dictation transcript: segment_kind must be user_recorded")
	}
	if !t.Locality.Valid() {
		return fmt.Errorf("dictation transcript: locality is unknown")
	}
	if t.DurationMS < 0 {
		return fmt.Errorf("dictation transcript: duration_ms is negative")
	}
	return nil
}
