//go:build !nodecision

package cli

import (
	"strings"

	"github.com/buxee/alix-ai/adapters/decision/clm"
	dlocalai "github.com/buxee/alix-ai/adapters/decision/localai"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The optional decision layer's scorer providers (master plan 8.5, D-05).
//
// This file is the composition root of the two scorer adapters, behind
// `//go:build !nodecision`: `go build -tags nodecision ./cmd/alixai` compiles
// decision_providers_nodecision.go instead, and the binary then holds no
// adapters/decision code at all (the optional layer is removable at build time).
//
// Nothing here turns the layer on. decisionProvidersBuild only constructs a provider
// from an explicit settings value; construction opens no connection, and the layer
// stays off until the decision engine is configured with Enabled (internal/decision,
// off by default). A provider is registered under the `router` role by whoever wires
// the engine, with decision.RegisterRouter.
//
// Two kinds exist:
//
//   - "localai": LocalAI's typed Decisions/SystemOne API. The connection (endpoint, API
//     key reference, TLS) is the registered "localai" section, so the scorer reaches
//     the runtime under the same endpoint policy as text; only the model is its own.
//     Whether the runtime hosts the API is never assumed: Probe reports it.
//   - "clm": a supervised CLM sidecar, with an endpoint of its own (loopback, unix
//     socket or https).

// Provider kinds.
const (
	decisionProvidersKindLocalAI = "localai"
	decisionProvidersKindCLM     = "clm"
)

// decisionProvidersSettings selects and configures one scorer provider.
type decisionProvidersSettings struct {
	// Kind is decisionProvidersKindLocalAI or decisionProvidersKindCLM.
	Kind string
	// Model and ModelRevision name the scorer model and the revision the operator
	// pins (empty: reported as "unverified", which the engine's calibration gate
	// refuses until a measured entry exists).
	Model         string
	ModelRevision string
	// Endpoint is the CLM service endpoint (kind "clm" only; empty is the adapter's
	// loopback default).
	Endpoint string
	// Mode is the CLM scoring route, "rank" (default) or "systemone".
	Mode string
	// APIKeyRef names the CLM service key ("env:NAME" or "file:PATH", the adapters/localai/voice forms); the key itself is
	// never accepted here and never logged. Empty sends no key (kind "clm" only).
	APIKeyRef string
}

// decisionProvidersAvailable reports whether this binary contains the scorer
// adapters.
func decisionProvidersAvailable() bool { return true }

// decisionProvidersKinds lists the provider kinds this binary can build.
func decisionProvidersKinds() []string {
	return []string{decisionProvidersKindLocalAI, decisionProvidersKindCLM}
}

// decisionProvidersBuild builds the provider s selects. The result implements
// internal/decision.DecisionModelProvider; it is returned as any so that the
// nodecision stub has the same signature without importing the decision packages.
// It fails with validation_failed on an unknown kind or an invalid configuration
// (including a plaintext non-loopback endpoint).
func decisionProvidersBuild(s decisionProvidersSettings) (any, error) {
	switch strings.ToLower(strings.TrimSpace(s.Kind)) {
	case decisionProvidersKindLocalAI:
		rt := localaiSection
		return decisionProvidersAs(dlocalai.New(dlocalai.Config{
			Endpoint:      rt.Endpoint,
			APIKeyRef:     rt.APIKeyRef,
			TLS:           dlocalai.TLSConfig{CAFile: rt.TLS.CAFile, CertFile: rt.TLS.CertFile, KeyFile: rt.TLS.KeyFile, ServerName: rt.TLS.ServerName},
			Model:         s.Model,
			ModelRevision: s.ModelRevision,
		}))
	case decisionProvidersKindCLM:
		return decisionProvidersAs(clm.New(clm.Config{
			Endpoint: s.Endpoint, APIKeyRef: s.APIKeyRef, Model: s.Model, ModelRevision: s.ModelRevision, Mode: s.Mode,
		}))
	}
	return nil, contracts.New(contracts.ErrValidationFailed, "decision: unknown scorer provider kind (want localai or clm)")
}

// decisionProvidersAs checks, at compile time, that each adapter satisfies the
// engine's provider contract, and passes a constructor's result through.
func decisionProvidersAs[P decision.DecisionModelProvider](p P, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return p, nil
}

// init makes serve's decision wiring (cmd_decision.go) pass the CLM api_key_ref: the settings of the
// nodecision stub have no such field, so the shared file builds through a hook this build replaces.
func init() {
	decWireBuildProvider = func(p decWireProviderConfig) (any, error) {
		return decisionProvidersBuild(decisionProvidersSettings{
			Kind: p.Kind, Model: p.Model, ModelRevision: p.ModelRevision, Endpoint: p.Endpoint, Mode: p.Mode, APIKeyRef: p.APIKeyRef,
		})
	}
}
