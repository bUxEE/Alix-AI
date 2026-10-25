//go:build nodecision

package cli

import "github.com/buxee/alix-ai/pkg/contracts"

// The stub of decision_providers.go for `go build -tags nodecision ./cmd/alixai`: it
// imports no decision package and no scorer adapter, so the binary holds no decision
// provider code. The identifiers match the tagged file's, so no caller changes.

type decisionProvidersSettings struct {
	Kind          string
	Model         string
	ModelRevision string
	Endpoint      string
	Mode          string
}

// decisionProvidersAvailable reports whether this binary contains the scorer
// adapters; this one does not.
func decisionProvidersAvailable() bool { return false }

// decisionProvidersKinds lists the provider kinds this binary can build: none.
func decisionProvidersKinds() []string { return nil }

// decisionProvidersBuild always fails: there is no scorer provider in this build.
func decisionProvidersBuild(decisionProvidersSettings) (any, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "decision: this binary was built without decision providers (-tags nodecision)")
}
