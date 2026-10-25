//go:build noexternal

package cli

import (
	"log/slog"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The local-only build (ADR-0022): this file replaces providers_external.go and
// imports nothing of milestone X, so a binary built with -tags noexternal
// contains no external adapter, credential service, OAuth engine, data-egress
// policy, provider registry, budget or mode manager that only they need.

// ExternalProvidersBuilt reports whether this binary carries external provider
// adapters.
func ExternalProvidersBuilt() bool { return false }

// ExternalProvider has the shape of the tagged build so that untagged code
// that names the type compiles in both.
type ExternalProvider interface {
	providers.AIRuntimeProvider
	providers.CapabilityProvider
	providers.MultimodalGenerator
}

// ExternalProviderSpec is empty in this build: there is nothing to configure.
type ExternalProviderSpec struct {
	Provider string
	Logger   *slog.Logger
}

// ExternalProviderOpenAI is the id of the OpenAI adapter.
const ExternalProviderOpenAI = "openai"

// NewExternalProvider always fails: this binary has no external adapter.
func NewExternalProvider(ExternalProviderSpec) (ExternalProvider, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "cli: this binary was built without external providers (noexternal)")
}
