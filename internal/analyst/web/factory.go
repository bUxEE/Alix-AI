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
		// Native search is wired by the serve layer (ChainSearcher + NativeCaller).
		// Building a standalone Provider here is not used; callers use NewFallbackProvider.
		return nil, contracts.New(contracts.ErrWebSearchUnavailable, "web: provider_native requires the native search seam")
	default:
		return nil, contracts.New(contracts.ErrValidationFailed, "web: unknown search provider")
	}
}

// NewFallbackProvider builds a non-native search Provider from web config's fallback
// (or from provider when it is searxng/brave/tavily). Empty name refuses.
func NewFallbackProvider(name an.ConfigWebConfigFallback, web *an.ConfigWebConfig, secrets SecretResolver) (Provider, error) {
	if web == nil || name == an.ConfigWebConfigFallbackEmpty || name == "" {
		return nil, contracts.New(contracts.ErrWebSearchUnavailable, "web: no fallback search provider")
	}
	cred := ""
	if web.CredRef != nil {
		cred = strings.TrimSpace(*web.CredRef)
	}
	switch name {
	case an.ConfigWebConfigFallbackBrave:
		if secrets == nil || cred == "" {
			return nil, contracts.New(contracts.ErrUnavailable, "web: brave requires cred_ref")
		}
		return &Brave{Secrets: secrets, CredRef: cred, Timeout: 15 * time.Second}, nil
	case an.ConfigWebConfigFallbackTavily:
		if secrets == nil || cred == "" {
			return nil, contracts.New(contracts.ErrUnavailable, "web: tavily requires cred_ref")
		}
		return &Tavily{Secrets: secrets, CredRef: cred, Timeout: 15 * time.Second}, nil
	case an.ConfigWebConfigFallbackSearxng:
		origin := ""
		if web.Origin != nil {
			origin = strings.TrimSpace(*web.Origin)
		}
		if origin == "" {
			return nil, contracts.New(contracts.ErrUnavailable, "web: searxng requires origin")
		}
		return &SearXNG{Origin: origin, Secrets: secrets, CredRef: cred, Timeout: 15 * time.Second}, nil
	default:
		return nil, contracts.New(contracts.ErrValidationFailed, "web: unknown fallback search provider")
	}
}
