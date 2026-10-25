package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// StructuredMode is a model's structured-output guarantee, as the provider
// profile schema records it. Only StructuredStrict is a guarantee; the rest
// need local validation of the result.
type StructuredMode string

const (
	StructuredNone       StructuredMode = "none"
	StructuredJSONMode   StructuredMode = "json_mode"
	StructuredBestEffort StructuredMode = "schema_best_effort"
	StructuredStrict     StructuredMode = "strict_schema"
)

// Valid reports whether m is a known mode.
func (m StructuredMode) Valid() bool {
	switch m {
	case StructuredNone, StructuredJSONMode, StructuredBestEffort, StructuredStrict:
		return true
	}
	return false
}

// MeteringMode says when a provider reports usage.
type MeteringMode string

const (
	MeteringPerResponse MeteringMode = "per_response"
	MeteringStreamFinal MeteringMode = "stream_final"
	// MeteringNone: the provider reports nothing; usage is estimated and marked
	// Estimated.
	MeteringNone MeteringMode = "none"
)

// ToolCapability describes tool-call support. Tools are proposals only.
type ToolCapability struct {
	Supported       bool `json:"supported"`
	StrictArguments bool `json:"strict_arguments"`
	ParallelCalls   bool `json:"parallel_calls"`
	// MaxTools is the most tool specs one request may carry (0: not stated).
	MaxTools int `json:"max_tools,omitempty"`
}

// ModelCapabilities is what one provider model can do: the subset of the
// provider profile the request path needs to reject an impossible request
// BEFORE any data is sent.
type ModelCapabilities struct {
	Provider         string         `json:"provider"`
	Model            string         `json:"model"`
	Version          string         `json:"version,omitempty"`
	InputModalities  []Modality     `json:"input_modalities"`
	OutputModalities []Modality     `json:"output_modalities"`
	Streaming        bool           `json:"streaming"`
	StructuredOutput StructuredMode `json:"structured_output"`
	// Grammar is true when the model accepts a GBNF grammar constraint.
	Grammar        bool           `json:"grammar,omitempty"`
	Tools          ToolCapability `json:"tools"`
	MaxContextToks int            `json:"max_context_toks"`
	MaxOutputToks  int            `json:"max_output_toks"`
	// ImageGeneration, SpeechToText and TextToSpeech are the voice and image
	// features beyond plain modalities.
	ImageGeneration bool `json:"image_generation,omitempty"`
	SpeechToText    bool `json:"speech_to_text,omitempty"`
	TextToSpeech    bool `json:"text_to_speech,omitempty"`
	// Pricing is nil when unknown: such a model cannot be budgeted.
	Pricing  *PriceTable  `json:"pricing,omitempty"`
	Metering MeteringMode `json:"usage_metering"`
}

// CapabilityProvider is implemented by a provider that can describe its model.
type CapabilityProvider interface {
	Capabilities(ctx context.Context) (ModelCapabilities, error)
}

func modalitySet(ms []Modality) (map[Modality]bool, bool) {
	set := make(map[Modality]bool, len(ms))
	for _, m := range ms {
		if !m.Valid() || set[m] {
			return nil, false
		}
		set[m] = true
	}
	return set, true
}

// Validate rejects an incoherent capability record.
func (c ModelCapabilities) Validate() error {
	if c.Provider == "" || c.Model == "" {
		return invalidRequest("capabilities need a provider and a model")
	}
	in, ok := modalitySet(c.InputModalities)
	if !ok || !in[ModText] {
		return invalidRequest("capabilities: input modalities must be unique, known and include text")
	}
	out, ok := modalitySet(c.OutputModalities)
	if !ok || !out[ModText] {
		return invalidRequest("capabilities: output modalities must be unique, known and include text")
	}
	if !c.StructuredOutput.Valid() {
		return invalidRequest("capabilities: unknown structured output mode")
	}
	if c.Metering != "" && c.Metering != MeteringPerResponse && c.Metering != MeteringStreamFinal && c.Metering != MeteringNone {
		return invalidRequest("capabilities: unknown usage metering mode")
	}
	if c.MaxContextToks <= 0 || c.MaxOutputToks <= 0 || c.MaxOutputToks > c.MaxContextToks {
		return invalidRequest("capabilities: context and output limits must be positive and consistent")
	}
	if !c.Tools.Supported && (c.Tools.StrictArguments || c.Tools.ParallelCalls || c.Tools.MaxTools != 0) {
		return invalidRequest("capabilities: tool options set without tool support")
	}
	if c.Tools.MaxTools < 0 || c.Tools.MaxTools > 1024 {
		return invalidRequest("capabilities: max tools out of range")
	}
	if c.ImageGeneration && !out[ModImage] {
		return invalidRequest("capabilities: image generation without image output")
	}
	if c.SpeechToText && !in[ModAudio] {
		return invalidRequest("capabilities: speech-to-text without audio input")
	}
	if c.TextToSpeech && !out[ModAudio] {
		return invalidRequest("capabilities: text-to-speech without audio output")
	}
	if c.Pricing != nil {
		if err := c.Pricing.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DecodeModelCapabilities parses and validates a capability record. Unknown
// members are an error (fail closed).
func DecodeModelCapabilities(b []byte) (ModelCapabilities, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c ModelCapabilities
	if err := dec.Decode(&c); err != nil {
		return ModelCapabilities{}, invalidRequest("capabilities are not valid JSON for this schema")
	}
	if _, err := dec.Token(); err != io.EOF {
		return ModelCapabilities{}, invalidRequest("capabilities have trailing data")
	}
	if err := c.Validate(); err != nil {
		return ModelCapabilities{}, err
	}
	return c, nil
}

// Clone returns a deep copy.
func (c ModelCapabilities) Clone() ModelCapabilities {
	c.InputModalities = append([]Modality(nil), c.InputModalities...)
	c.OutputModalities = append([]Modality(nil), c.OutputModalities...)
	if c.Pricing != nil {
		p := *c.Pricing
		c.Pricing = &p
	}
	return c
}

// UnsupportedError is the typed rejection of a request the model cannot serve.
// validation_failed: re-sending the identical request can never succeed.
func UnsupportedError(what string) error {
	return contracts.Newf(contracts.ErrValidationFailed, "providers: model does not support %s", what).
		WithDetails("unsupported", what)
}

// IsUnsupported reports whether err rejects a request for a feature the model
// lacks and, if so, which one ("image_input", "tool_calls", ...).
func IsUnsupported(err error) (what string, ok bool) {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrValidationFailed {
		return "", false
	}
	what, ok = ce.Details["unsupported"].(string)
	return what, ok
}
