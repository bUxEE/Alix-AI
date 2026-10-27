package dataegress

import (
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const selfHostedProviderID providers.ProviderID = "self_hosted"

// IsSelfHosted reports whether prof is a synthesized private self-hosted profile (ADR 0045).
func IsSelfHosted(prof providers.ProviderProfile) bool {
	return prof.ProviderID == selfHostedProviderID
}
