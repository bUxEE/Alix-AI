package answer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The model's output is one JSON object, {"claims":[{"text":"...","evidence_ids":["e1"]}]}:
// the answer as short claims, each with the handles of the passages that state it.
// The core never takes the claims on trust. It checks every handle against the
// package it built, every claim against the passages it cites (support.go), and
// writes the citations itself.

// modelClaim is one claim as the model wrote it.
type modelClaim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// modelOutput is the model's whole output.
type modelOutput struct {
	Claims *[]modelClaim `json:"claims"`
}

// outputSchema is the JSON Schema the runtime is asked to constrain the output
// to. Its only variable part is the list of handles: this package's, and no
// others, so a runtime that enforces the schema cannot name a passage the
// model was not given. The core checks every handle again, because enforcement
// is the runtime's, not the core's.
func outputSchema(handles []string, maxClaims int) json.RawMessage {
	enum := make([]any, len(handles))
	for i, h := range handles {
		enum[i] = h
	}
	doc := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"claims"},
		"properties": map[string]any{
			"claims": map[string]any{
				"type":     "array",
				"maxItems": maxClaims,
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"text", "evidence_ids"},
					"properties": map[string]any{
						"text": map[string]any{"type": "string", "minLength": 1, "maxLength": MaxClaimRunes},
						"evidence_ids": map[string]any{
							"type":        "array",
							"minItems":    1,
							"maxItems":    maxEvidencePerClaim,
							"uniqueItems": true,
							"items":       map[string]any{"type": "string", "enum": enum},
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc) // map keys are sorted: the same handles give the same bytes
	if err != nil {
		panic("answer: output schema does not marshal: " + err.Error())
	}
	return b
}

// parseOutput decodes the model's output strictly: one JSON document, the two
// known members and no others, a claims array that is present. Anything else is
// untrusted output refused whole (schema_invalid): a response the core cannot
// read is a failure, not an answer and not "no evidence". The error never echoes
// the output.
func parseOutput(raw []byte) ([]modelClaim, error) {
	if len(raw) > maxOutputBytes {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "answer: model output is too large").
			WithDetails("output_len", len(raw))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out modelOutput
	if err := dec.Decode(&out); err != nil {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "answer: model output does not match the claims schema").
			WithDetails("output_len", len(raw))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "answer: model output holds more than one document").
			WithDetails("output_len", len(raw))
	}
	if out.Claims == nil {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "answer: model output has no claims member").
			WithDetails("output_len", len(raw))
	}
	return *out.Claims, nil
}
