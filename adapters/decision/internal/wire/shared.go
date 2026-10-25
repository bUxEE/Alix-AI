package wire

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ErrorMessage extracts the lower-cased message of an OpenAI-style or plain error
// body, bounded. It is for choosing a classification only and is never copied into
// an error or a log.
func ErrorMessage(body []byte) string {
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal(body, &env) == nil {
		var nested struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		if len(env.Error) > 0 && json.Unmarshal(env.Error, &nested) == nil && (nested.Message != "" || nested.Type != "") {
			return strings.ToLower(nested.Message + " " + nested.Type)
		}
		if env.Message != "" || env.Detail != "" {
			return strings.ToLower(env.Message + " " + env.Detail)
		}
	}
	if len(body) > 2048 {
		body = body[:2048]
	}
	return strings.ToLower(string(body))
}

// Decode decodes a single JSON value from body into v. Anything else (not JSON,
// trailing data, a type mismatch) is schema_invalid with a fixed message; the
// decoder's own error is dropped because it can quote the input.
func Decode(who string, body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(v); err != nil {
		return contracts.New(contracts.ErrSchemaInvalid, who+": the response is not the expected JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return contracts.New(contracts.ErrSchemaInvalid, who+": the response holds more than one JSON value")
	}
	return nil
}

// StateString is the whole of the state a scorer receives, as one deterministic
// string (JSON objects sort their keys): the decision type, the locale and the
// bounded state. Tenant, application and session identifiers, deadlines and
// candidate data versions are never sent; they are the engine's, not the model's.
func StateString(req gen.DecisionRequest) (string, error) {
	b, err := json.Marshal(struct {
		DecisionType gen.DecisionType             `json:"decision_type"`
		Locale       gen.Locale                   `json:"locale,omitempty"`
		State        map[string]gen.BoundedScalar `json:"state"`
	}{req.DecisionType, req.Locale, req.BoundedState})
	if err != nil {
		return "", contracts.New(contracts.ErrSchemaInvalid, "decision wire: the bounded state cannot be encoded")
	}
	return string(b), nil
}

// CandidateText is what a scorer sees of one candidate: its label, then the
// optional text. The identifier is the engine's handle and is not part of it.
func CandidateText(c gen.CandidateSetCandidate) string {
	if c.Text != nil && *c.Text != "" {
		return c.Label + ": " + *c.Text
	}
	return c.Label
}

// Scored is a candidate with the scorer's probability and confidence.
type Scored struct {
	ID         string
	Prob       float64
	Confidence float64
}

// Unit reports whether f is a finite number in [0, 1].
func Unit(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1 }

// Selections turns the scorer's ranking into the result's selections. The first
// element of ranked is the scorer's pick and is always selected; further elements
// are selected only up to max and only when their probability reaches floor, since
// every selection must clear the engine's confidence floor and a weak runner-up
// would fail the whole result. Selections are best first.
func Selections(ranked []Scored, max int, floor float64) []gen.DecisionScoreResultSelection {
	if len(ranked) == 0 || max < 1 {
		return nil
	}
	head, rest := ranked[0], append([]Scored(nil), ranked[1:]...)
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].Prob != rest[j].Prob {
			return rest[i].Prob > rest[j].Prob
		}
		return rest[i].ID < rest[j].ID
	})
	out := []gen.DecisionScoreResultSelection{sel(head)}
	for _, s := range rest {
		if len(out) >= max {
			break
		}
		if s.Prob >= floor {
			out = append(out, sel(s))
		}
	}
	return out
}

func sel(s Scored) gen.DecisionScoreResultSelection {
	return gen.DecisionScoreResultSelection{
		CandidateID: common.Identifier(s.ID), Score: gen.UnitScore(s.Prob), CalibratedConfidence: gen.UnitScore(s.Confidence),
	}
}

// Accepted builds the result of a scorer that answered: identity echoed from the
// request and the manifest, the selections, and OodStatus in_distribution.
//
// in_distribution means only that the request fit the runtime's documented shape
// (candidate count, state size) and was answered; neither runtime reports an
// out-of-distribution signal and the adapters do not invent one. Whether the
// confidence can be trusted is the engine's calibration gate, keyed by model,
// revision and contract version, which fails closed without a measured entry.
func Accepted(req gen.DecisionRequest, m gen.ScorerManifest, selections []gen.DecisionScoreResultSelection, latencyMS int64) gen.DecisionScoreResult {
	model, rev := m.ModelID, m.ModelRevision
	return gen.DecisionScoreResult{
		Disposition:     gen.DecisionScoreResultDispositionAccepted,
		ProviderID:      m.ProviderID,
		ModelID:         &model,
		ModelRevision:   &rev,
		ContractVersion: req.ContractVersion,
		DecisionType:    req.DecisionType,
		SchemaVersion:   req.SchemaVersion,
		TenantID:        req.TenantID,
		ApplicationID:   req.ApplicationID,
		Selections:      selections,
		OodStatus:       gen.DecisionScoreResultOodStatusInDistribution,
		LatencyMS:       common.DurationMS(latencyMS),
		CacheStatus:     gen.DecisionScoreResultCacheStatusBypassed,
	}
}

// Manifest builds a scorer manifest for a model the operator named. The files and
// the signature are empty: an adapter that talks to a runtime cannot hash or sign
// the weights the runtime serves, so it states none (the model registry, D-07,
// owns the hashes).
func Manifest(providerID, modelID, revision, encoderID, encoderRevision string, licence gen.ScorerManifestLicence) gen.ScorerManifest {
	return gen.ScorerManifest{
		ProviderID:    common.Identifier(providerID),
		ModelID:       common.Identifier(modelID),
		ModelRevision: actions.OpaqueVersion(revision),
		Encoder:       gen.ScorerManifestEncoder{EncoderID: common.Identifier(encoderID), Revision: actions.OpaqueVersion(encoderRevision)},
		Files:         []gen.ScorerManifestFileHash{},
		Licence:       licence,
	}
}
