package web

import (
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// NewProvider builds a search Provider from dialogue.analyst.web config.
// Secrets must resolve cred_ref; never pass the secret itself.
func NewProvider(web *an.ConfigWebConfig, secrets SecretResolver) (Provider, error) {
	if web == nil || web.Provider == nil || *web.Provider == an.ConfigWebConfigProviderEmpty {
		return nil, contracts.New(contracts.ErrUnavailable, "web: search provider is not configured")
	}
	cred := ""
	if web.CredRef != nil {
		cred = strings.TrimSpace(*web.CredRef)
	}
	switch *web.Provider {
	case an.ConfigWebConfigProviderBrave:
		if secrets == nil || cred == "" {
			return nil, contracts.New(contracts.ErrUnavailable, "web: brave requires cred_ref")
		}
		return &Brave{Secrets: secrets, CredRef: cred, Timeout: 15 * time.Second}, nil
	case an.ConfigWebConfigProviderTavily:
		if secrets == nil || cred == "" {
			return nil, contracts.New(contracts.ErrUnavailable, "web: tavily requires cred_ref")
		}
		return &Tavily{Secrets: secrets, CredRef: cred, Timeout: 15 * time.Second}, nil
	case an.ConfigWebConfigProviderSearxng:
		origin := ""
		if web.Origin != nil {
			origin = strings.TrimSpace(*web.Origin)
		}
		if origin == "" {
			return nil, contracts.New(contracts.ErrUnavailable, "web: searxng requires origin")
		}
		return &SearXNG{Origin: origin, Secrets: secrets, CredRef: cred, Timeout: 15 * time.Second}, nil
	case an.ConfigWebConfigProviderProviderNative:
		return nil, contracts.New(contracts.ErrUnavailable, "web: provider_native search is owned by the model router, not this seam")
	default:
		return nil, contracts.New(contracts.ErrValidationFailed, "web: unknown search provider")
	}
}
