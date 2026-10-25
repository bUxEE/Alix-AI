package voice

import (
	"context"
	"encoding/json"

	"github.com/buxee/alix-ai/internal/providers"
	pv "github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// vadWire is the answer of POST /v1/vad (LocalAI v4.10.0,
// core/schema VADResponse): segment bounds are float seconds from the start of
// the audio. The key must be present: LocalAI answers "segments": null when it
// found no speech, and a body without the key is not a VAD answer.
type vadWire struct {
	Segments json.RawMessage `json:"segments"`
}

type vadSegmentWire struct {
	Start *float64 `json:"start"`
	End   *float64 `json:"end"`
}

// DetectSpeech implements voice.VAD.
//
// LocalAI's VAD takes 16 kHz mono float samples, so the request audio must be
// PCM16 mono at 16000 Hz; any other format is validation_failed. The adapter
// does not resample: that would be a guess the caller never asked for, and the
// session service can capture at the right rate. The result is checked against
// the audio (voice.DetectResult.Check) before it is returned.
func (c *Client) DetectSpeech(ctx context.Context, scope contracts.Scope, req pv.DetectRequest) (pv.DetectResult, error) {
	if err := scope.Validate(); err != nil {
		return pv.DetectResult{}, err
	}
	if err := req.Validate(c.limits); err != nil {
		return pv.DetectResult{}, err
	}
	if req.Format.SampleRate != vadSampleRate || req.Format.Channels != 1 {
		return pv.DetectResult{}, contracts.Newf(contracts.ErrValidationFailed,
			"localai voice: the runtime's VAD takes %d Hz mono audio, not %d Hz with %d channels",
			vadSampleRate, req.Format.SampleRate, req.Format.Channels)
	}
	model, err := c.model(providers.RoleVAD, req.Profile)
	if err != nil {
		return pv.DetectResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	started := c.clk.Now()
	resp, err := c.post(ctx, pathVAD, "application/json", "application/json", vadAudioJSON(model, req.Audio))
	if err != nil {
		return pv.DetectResult{}, err
	}
	defer resp.Body.Close()
	var out vadWire
	if err := c.decodeBody(ctx, resp.Body, &out); err != nil {
		return pv.DetectResult{}, err
	}
	if out.Segments == nil {
		return pv.DetectResult{}, schemaInvalid("VAD answer has no segments field")
	}
	var wire []vadSegmentWire
	if err := json.Unmarshal(out.Segments, &wire); err != nil {
		return pv.DetectResult{}, schemaInvalid("VAD segments are not a list of spans")
	}
	res := pv.DetectResult{
		Segments: make([]pv.SpeechSegment, 0, len(wire)),
		Timings:  providers.Timings{Total: c.clk.Now().Sub(started)},
		TraceID:  traceID(resp.Header),
		Model:    model,
	}
	for _, w := range wire {
		if w.Start == nil || w.End == nil {
			return pv.DetectResult{}, schemaInvalid("VAD segment lacks a start or an end")
		}
		start, ok1 := secondsToDuration(*w.Start)
		end, ok2 := secondsToDuration(*w.End)
		if !ok1 || !ok2 {
			return pv.DetectResult{}, schemaInvalid("VAD segment time is out of range")
		}
		res.Segments = append(res.Segments, pv.SpeechSegment{Start: start, End: end})
	}
	if err := res.Check(req); err != nil {
		return pv.DetectResult{}, err
	}
	return res, nil
}
