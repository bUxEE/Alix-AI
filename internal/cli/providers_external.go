//go:build !noexternal

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/adapters/external/anthropic"
	"github.com/buxee/alix-ai/adapters/external/gemini"
	"github.com/buxee/alix-ai/adapters/external/meta"
	"github.com/buxee/alix-ai/adapters/external/mistral"
	"github.com/buxee/alix-ai/adapters/external/moonshot"
	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/adapters/external/openrouter"
	"github.com/buxee/alix-ai/adapters/external/selfhosted"
	"github.com/buxee/alix-ai/adapters/external/xai"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/dialogue/extmode"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/extmodal"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/modes/hybrid"
	"github.com/buxee/alix-ai/internal/oauth"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/verify"
	voiceprofile "github.com/buxee/alix-ai/internal/voice/profile"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The composition root of External Provider Mode (ADR-0022, additive plan 12):
// the one file in the dependency graph of ./cmd/alixai that imports a concrete
// external adapter. A binary built with -tags noexternal compiles
// providers_noexternal.go instead and contains no external code at all.
//
// Nothing here runs at start-up except the assignment of the factory table in
// init: no provider is constructed, no goroutine started, no file or socket
// opened, until a caller (the mode manager's wiring, when external mode is
// enabled for the deployment) calls NewExternalProvider.

// ExternalProvidersBuilt reports whether this binary carries external provider
// adapters.
func ExternalProvidersBuilt() bool { return true }

// ExternalProvider is what an external adapter offers the router and the
// registry: a name and a health probe, the model description for capability
// gating, and generation with proposals only.
type ExternalProvider interface {
	providers.AIRuntimeProvider
	providers.CapabilityProvider
	providers.MultimodalGenerator
}

// ExternalProviderSpec describes one provider to build.
type ExternalProviderSpec struct {
	// Provider is the provider id: "openai" or "anthropic" (the adapters that have landed).
	Provider string
	// OpenAI configures the OpenAI adapter. Its CredentialRef is a reference to
	// the credential service, never a key.
	OpenAI openai.Config
	// Anthropic, Gemini and Meta configure their adapters the same way. Gemini authenticates with an API key
	// (Keys) or, when Gemini.Auth is gemini.AuthOAuth, with the access tokens of GeminiTokens.
	Anthropic  anthropic.Config
	Gemini     gemini.Config
	Meta       meta.Config
	OpenRouter openrouter.Config
	XAI        xai.Config
	Moonshot   moonshot.Config
	Mistral    mistral.Config
	SelfHosted selfhosted.Config
	// GeminiTokens resolves the OAuth access token of a Gemini connection per call (extFinalEngineTokens over the
	// shared OAuth engine). Only a Gemini.Auth of gemini.AuthOAuth uses it.
	GeminiTokens gemini.TokenSource
	// Egress is the deployment's provider destination registry. The adapter's
	// own origin is added to it when the provider has no entry yet; an entry
	// the operator already set is never widened.
	Egress *egress.ProviderRegistry
	// Keys resolves the credential reference per call (usually
	// openai.ServiceKeys{Service: credentialService}; the Anthropic adapter takes anthropic.ServiceKeys or
	// any value with the same Key method).
	Keys openai.KeySource
	// Logger receives the adapter's debug records (nil: discarded).
	Logger *slog.Logger
}

type externalProviderFactory func(ExternalProviderSpec) (ExternalProvider, error)

// externalProviderFactories is filled once in init and read only afterwards.
var externalProviderFactories map[string]externalProviderFactory

func init() {
	externalProviderFactories = map[string]externalProviderFactory{
		ExternalProviderOpenAI:     externalProviderBuildOpenAI,
		ExternalProviderAnthropic:  externalProviderBuildAnthropic,
		ExternalProviderGemini:     externalProviderBuildGemini,
		ExternalProviderMeta:       externalProviderBuildMeta,
		ExternalProviderOpenRouter: externalProviderBuildOpenRouter,
		ExternalProviderXAI:        externalProviderBuildXAI,
		ExternalProviderMoonshot:   externalProviderBuildMoonshot,
		ExternalProviderMistral:    externalProviderBuildMistral,
		ExternalProviderSelfHosted: externalProviderBuildSelfHosted,
	}
	extWireEngineSeams = extWireSeams
	registerServeProvider(extWireProviderName, extWireProvide)
	serveExternalCoversController = func() bool {
		if !externalSection.Enabled {
			return false
		}
		return len(externalSection.Routing["controller"].Profiles) > 0
	}
}

// ExternalProviderOpenAI is the id of the OpenAI adapter.
const ExternalProviderOpenAI = openai.ProviderName

// ExternalProviderAnthropic is the id of the Anthropic adapter.
const ExternalProviderAnthropic = anthropic.ProviderName

// ExternalProviderGemini and ExternalProviderMeta are the ids of the Gemini and Meta adapters.
const (
	ExternalProviderGemini     = gemini.ProviderName
	ExternalProviderMeta       = meta.ProviderName
	ExternalProviderOpenRouter = openrouter.ProviderName
	ExternalProviderXAI        = xai.ProviderName
	ExternalProviderMoonshot   = moonshot.ProviderName
	ExternalProviderMistral    = mistral.ProviderName
	ExternalProviderSelfHosted = selfhosted.ProviderName
)

// NewExternalProvider builds the adapter for spec.Provider. It sends nothing.
func NewExternalProvider(spec ExternalProviderSpec) (ExternalProvider, error) {
	f, ok := externalProviderFactories[spec.Provider]
	if !ok {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "cli: no external adapter is built for provider %q", spec.Provider)
	}
	return f(spec)
}

// extWireAllowOrigin adds the adapter's own origin to the provider's destination allowlist when the provider
// has no entry yet. An entry the operator already set is never widened.
func extWireAllowOrigin(reg *egress.ProviderRegistry, provider, base string) error {
	if reg.Allowlist(provider) != nil {
		return nil
	}
	dest, err := egress.DestinationFromURL(base)
	if err != nil {
		return err
	}
	al, err := egress.NewAllowlist(dest)
	if err != nil {
		return err
	}
	return reg.Set(provider, al)
}

func externalProviderBuildSelfHosted(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry")
	}
	if err := extWireAllowOrigin(spec.Egress, selfhosted.ProviderName, spec.SelfHosted.BaseURL); err != nil {
		return nil, err
	}
	var opts []selfhosted.Option
	if spec.Logger != nil {
		opts = append(opts, selfhosted.WithLogger(spec.Logger))
	}
	keys := spec.Keys
	if keys == nil {
		keys = selfhosted.OpenKeys{}
	}
	c, err := selfhosted.New(spec.SelfHosted, spec.Egress.Dialer(selfhosted.ProviderName), keys, opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildOpenAI(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry")
	}
	base := spec.OpenAI.BaseURL
	if base == "" {
		base = openai.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, openai.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []openai.Option
	if spec.Logger != nil {
		opts = append(opts, openai.WithLogger(spec.Logger))
	}
	c, err := openai.New(spec.OpenAI, spec.Egress.Dialer(openai.ProviderName), spec.Keys, opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildAnthropic(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry")
	}
	base := spec.Anthropic.BaseURL
	if base == "" {
		base = anthropic.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, anthropic.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []anthropic.Option
	if spec.Logger != nil {
		opts = append(opts, anthropic.WithLogger(spec.Logger))
	}
	if spec.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a key source")
	}
	c, err := anthropic.New(spec.Anthropic, spec.Egress.Dialer(anthropic.ProviderName), spec.Keys, opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildGemini(spec ExternalProviderSpec) (ExternalProvider, error) {
	oauthConn := spec.Gemini.Auth == gemini.AuthOAuth
	if spec.Egress == nil || (!oauthConn && spec.Keys == nil) || (oauthConn && spec.GeminiTokens == nil) {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry and a key source (a token source for an OAuth connection)")
	}
	base := spec.Gemini.BaseURL
	if base == "" {
		base = gemini.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, gemini.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []gemini.Option
	if spec.Logger != nil {
		opts = append(opts, gemini.WithLogger(spec.Logger))
	}
	var keys gemini.KeySource
	if oauthConn {
		opts = append(opts, gemini.WithTokenSource(spec.GeminiTokens))
	} else {
		keys = spec.Keys
	}
	c, err := gemini.New(spec.Gemini, spec.Egress.Dialer(gemini.ProviderName), keys, opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildMeta(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil || spec.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry and a key source")
	}
	base := spec.Meta.BaseURL
	if base == "" {
		if spec.Meta.Dialect == meta.DialectOpenAICompat {
			return nil, contracts.New(contracts.ErrValidationFailed, "cli: a hosted Llama endpoint needs its origin (external_providers.meta_base_url)")
		}
		base = meta.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, meta.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []meta.Option
	if spec.Logger != nil {
		opts = append(opts, meta.WithLogger(spec.Logger))
	}
	c, err := meta.New(spec.Meta, spec.Egress.Dialer(meta.ProviderName), spec.Keys, opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildOpenRouter(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil || spec.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry and a key source")
	}
	base := spec.OpenRouter.BaseURL
	if base == "" {
		base = openrouter.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, openrouter.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []openrouter.Option
	if spec.Logger != nil {
		opts = append(opts, openrouter.WithLogger(spec.Logger))
	}
	c, err := openrouter.New(spec.OpenRouter, spec.Egress.Dialer(openrouter.ProviderName), openrouter.KeySource(spec.Keys), opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildXAI(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil || spec.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry and a key source")
	}
	base := spec.XAI.BaseURL
	if base == "" {
		base = xai.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, xai.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []xai.Option
	if spec.Logger != nil {
		opts = append(opts, xai.WithLogger(spec.Logger))
	}
	c, err := xai.New(spec.XAI, spec.Egress.Dialer(xai.ProviderName), xai.KeySource(spec.Keys), opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildMoonshot(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil || spec.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry and a key source")
	}
	base := spec.Moonshot.BaseURL
	if base == "" {
		base = moonshot.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, moonshot.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []moonshot.Option
	if spec.Logger != nil {
		opts = append(opts, moonshot.WithLogger(spec.Logger))
	}
	c, err := moonshot.New(spec.Moonshot, spec.Egress.Dialer(moonshot.ProviderName), moonshot.KeySource(spec.Keys), opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

func externalProviderBuildMistral(spec ExternalProviderSpec) (ExternalProvider, error) {
	if spec.Egress == nil || spec.Keys == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "cli: external providers need a destination registry and a key source")
	}
	base := spec.Mistral.BaseURL
	if base == "" {
		base = mistral.DefaultBaseURL
	}
	if err := extWireAllowOrigin(spec.Egress, mistral.ProviderName, base); err != nil {
		return nil, err
	}
	var opts []mistral.Option
	if spec.Logger != nil {
		opts = append(opts, mistral.WithLogger(spec.Logger))
	}
	c, err := mistral.New(spec.Mistral, spec.Egress.Dialer(mistral.ProviderName), mistral.KeySource(spec.Keys), opts...)
	if err != nil {
		return nil, fmt.Errorf("cli: %w", err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------------------------
// The providers and oauth areas of `alixai serve` (milestone X, X-13).
//
// This is the composition root of the operator API: it builds the X services over the process's store and audit
// log and provides ONE gateway.ProvidersService. Nothing here runs unless the "external_providers" section says
// `enabled: true` (the default is off, byte-for-byte Local mode): with it off no X service is built, no secret file
// is created, and every route of the areas answers unavailable. The local-only build (-tags noexternal) compiles
// none of this and the areas answer unavailable there too, because the gateway only WANTS the service.
//
//	external_providers:
//	  enabled: true
//	  deployment_operators: ["app_console/usr_ops"]          # "<application id>/<principal id>"
//	  tenant_admins: ["app_console/ten_acme/usr_admin"]      # "<application id>/<tenant id>/<principal id>"
//	  openai_base_url: https://api.openai.com                # where a manual OpenAI key is validated
//
// Operators are read from this section and from nothing a token says. With the lists empty nobody passes. Every
// entry names the application as well as the principal: each application's token issuer chooses the subject it
// signs, so a principal id alone is something any application could claim. Secrets
// live in an encrypted file under <data_dir>/external (key file beside it, both owner only); no OS keyring is
// touched at start-up. OAuth provider registrations (client ids, endpoints) are not configured here: OpenAI API
// access is key-based, so every provider starts with the manual-credential fallback until a ProviderConfig exists.

const externalSectionName = "external_providers"

type externalConfig struct {
	Enabled             bool     `yaml:"enabled"`
	DeploymentOperators []string `yaml:"deployment_operators"`
	TenantAdmins        []string `yaml:"tenant_admins"`
	OpenAIBaseURL       string   `yaml:"openai_base_url"`
	AnthropicBaseURL    string   `yaml:"anthropic_base_url"`
	GeminiBaseURL       string   `yaml:"gemini_base_url"`
	// MetaBaseURL, MetaDialect ("meta-model-api", the default, or "openai-compatible" for a hosted Llama
	// endpoint, which needs the origin) and MetaAPIPrefix configure the Meta adapter. MetaModels maps the
	// slash-free model id of a registry profile to the wire id the service wants (a registry model id cannot
	// contain "/").
	MetaBaseURL   string            `yaml:"meta_base_url"`
	MetaDialect   string            `yaml:"meta_dialect"`
	MetaAPIPrefix string            `yaml:"meta_api_prefix"`
	MetaModels    map[string]string `yaml:"meta_models"`
	// Routing names, per role, the registry profiles that may serve it (in order of preference) and the
	// conditions on which the local provider serves instead. A role that is not listed stays local.
	Routing map[string]externalRoute `yaml:"routing"`
	// HybridFactClass is the data class the operator assigns to the facts of a directive in Hybrid mode
	// (public or internal for the text to leave). Empty, the default, keeps every turn local.
	HybridFactClass string `yaml:"hybrid_fact_class"`
	// DirectiveClass is the data class a response directive (and its deterministic facts) may leave at
	// in External mode (public, internal or confidential). Empty, the default, keeps the directive
	// local so the deterministic template realises the reply. The tenant egress policy must still
	// allow categories dialogue_directive and deterministic_fact at that class.
	DirectiveClass string `yaml:"directive_class"`
	// RevalidateEvery is how often expired provider reviews are looked for (default one hour).
	RevalidateEvery time.Duration `yaml:"revalidate_every"`
	// BudgetSweepEvery is how often the timed-out budget reservations are marked expired (default 15 minutes).
	BudgetSweepEvery time.Duration `yaml:"budget_sweep_every"`
	// OAuth is the popup / provider-catalog section (1.1.0). Off unless providers are enabled under it.
	OAuth externalOAuthConfig `yaml:"oauth"`
	// Credentials lists env/file secret references bound at startup (never plaintext keys in YAML).
	Credentials []externalCredRefConfig `yaml:"credentials"`
	// OpenRouter / xAI / Moonshot / Mistral origins and slash-free model maps (like meta_models).
	OpenRouterBaseURL string            `yaml:"openrouter_base_url"`
	XAIBaseURL        string            `yaml:"xai_base_url"`
	MoonshotBaseURL   string            `yaml:"moonshot_base_url"`
	MistralBaseURL    string            `yaml:"mistral_base_url"`
	OpenRouterModels  map[string]string `yaml:"openrouter_models"`
	XAIModels         map[string]string `yaml:"xai_models"`
	MoonshotModels    map[string]string `yaml:"moonshot_models"`
	MistralModels     map[string]string `yaml:"mistral_models"`
}

// externalRoute is the routing of one role.
type externalRoute struct {
	Profiles      []string `yaml:"profiles"`
	LocalFallback []string `yaml:"local_fallback"`
}

var externalSection externalConfig

func init() {
	config.Register(externalSectionName, &externalSection)
	registerServeProvider("providers_external", externalServeProvider)
}

// Validate refuses an operator entry that could never match, so a typo stops serve instead of silently leaving
// an operator out (or in).
func (c externalConfig) Validate() error {
	for _, e := range c.DeploymentOperators {
		if !externalEntryOK(e, 2) {
			return contracts.New(contracts.ErrValidationFailed, `external_providers.deployment_operators: entries are "<application id>/<principal id>"`)
		}
	}
	for _, e := range c.TenantAdmins {
		if !externalEntryOK(e, 3) {
			return contracts.New(contracts.ErrValidationFailed, `external_providers.tenant_admins: entries are "<application id>/<tenant id>/<principal id>"`)
		}
	}
	if c.OpenAIBaseURL != "" {
		if _, err := egress.DestinationFromURL(c.OpenAIBaseURL); err != nil {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.openai_base_url: not a bare http(s) origin")
		}
	}
	if c.AnthropicBaseURL != "" {
		if _, err := egress.DestinationFromURL(c.AnthropicBaseURL); err != nil {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.anthropic_base_url: not a bare http(s) origin")
		}
	}
	for name, v := range map[string]string{"gemini_base_url": c.GeminiBaseURL, "meta_base_url": c.MetaBaseURL} {
		if v != "" {
			if _, err := egress.DestinationFromURL(v); err != nil {
				return contracts.New(contracts.ErrValidationFailed, "external_providers."+name+": not a bare http(s) origin")
			}
		}
	}
	switch meta.Dialect(c.MetaDialect) {
	case "", meta.DialectModelAPI:
	case meta.DialectOpenAICompat:
		if c.MetaBaseURL == "" {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.meta_dialect openai-compatible needs meta_base_url")
		}
	default:
		return contracts.New(contracts.ErrValidationFailed, "external_providers.meta_dialect: unknown dialect")
	}
	if c.HybridFactClass != "" && !pv.DataClassification(c.HybridFactClass).Valid() {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.hybrid_fact_class: not a data class")
	}
	if c.DirectiveClass != "" && !pv.EgressEligibleClass(c.DirectiveClass).Valid() {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.directive_class: must be public, internal or confidential")
	}
	if c.RevalidateEvery < 0 {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.revalidate_every must not be negative")
	}
	if c.BudgetSweepEvery < 0 {
		return contracts.New(contracts.ErrValidationFailed, "external_providers.budget_sweep_every must not be negative")
	}
	for role, r := range c.Routing {
		if _, err := modes.Role(role).External(); err != nil {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.routing: this role is never routed to an external provider: "+role)
		}
		if role == string(modes.RoleTTS) && len(r.Profiles) > 0 && !externalTTSAdapterReady() {
			return contracts.New(contracts.ErrValidationFailed, "external_providers.routing: external TTS adapter is not registered")
		}
		for _, id := range r.Profiles {
			if !externalIDRE.MatchString(id) {
				return contracts.New(contracts.ErrValidationFailed, "external_providers.routing: not a profile id in role "+role)
			}
		}
		for _, cond := range r.LocalFallback {
			if !slices.Contains(extWireConditions, modes.Condition(cond)) {
				return contracts.New(contracts.ErrValidationFailed, "external_providers.routing: unknown local_fallback condition "+cond)
			}
		}
	}
	for name, v := range map[string]string{
		"openrouter_base_url": c.OpenRouterBaseURL, "xai_base_url": c.XAIBaseURL, "moonshot_base_url": c.MoonshotBaseURL,
		"mistral_base_url": c.MistralBaseURL,
	} {
		if v != "" {
			if _, err := egress.DestinationFromURL(v); err != nil {
				return contracts.New(contracts.ErrValidationFailed, "external_providers."+name+": not a bare http(s) origin")
			}
		}
	}
	if err := c.OAuth.Validate(); err != nil {
		return err
	}
	for _, r := range c.Credentials {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return c.extWirePolicy().Validate()
}

// externalLightTextModelOK refuses a light deployment that has neither a local
// text model nor an external controller route. It runs after a successful Load
// (config check and serve), when every section pointer holds the new values —
// Validate cannot see sibling sections while Load still works on copies.
func externalLightTextModelOK() error {
	if !externalSection.Enabled {
		return nil
	}
	if len(inferenceSection.Models) == 0 && len(externalSection.Routing["controller"].Profiles) == 0 {
		return contracts.New(contracts.ErrValidationFailed,
			"no text model available: external_providers is enabled with no controller route and no inference.models")
	}
	return nil
}

// externalTTSReady is set when an external TTS adapter registers itself so routing
// tts to a profile is accepted (ADR 0043). providers_external_tts.go sets it on init.
var externalTTSReady atomic.Bool

// RegisterExternalTTSAdapter marks that an external TTS adapter is available so
// external_providers.routing may name profiles for tts.
func RegisterExternalTTSAdapter() { externalTTSReady.Store(true) }

func externalTTSAdapterReady() bool { return externalTTSReady.Load() }

// extWireConditions are the failure conditions a routing entry may name for the local fallback.
var extWireConditions = []modes.Condition{
	modes.CondUnavailable, modes.CondTimeout, modes.CondRateLimited, modes.CondUnsupportedModality, modes.CondEgressDenied,
	modes.CondBudgetExhausted, modes.CondInvalidCredential, modes.CondStaleProfile, modes.CondMalformedOutput,
}

// extWirePolicy is the routing policy of the section. A role with no entry has no profile, which is local. A
// realisation entry that names no condition takes the Hybrid default (outage, modality, malformed, egress).
func (c externalConfig) extWirePolicy() modes.StaticPolicy {
	out := modes.StaticPolicy{}
	for role, r := range c.Routing {
		rp := modes.RolePolicy{}
		for _, id := range r.Profiles {
			rp.Profiles = append(rp.Profiles, pv.ProfileID(id))
		}
		for _, cond := range r.LocalFallback {
			rp.LocalFallback = append(rp.LocalFallback, modes.Condition(cond))
		}
		if modes.Role(role) == modes.RoleRealisation && len(rp.LocalFallback) == 0 {
			rp.LocalFallback = hybrid.RealisationPolicy().LocalFallback
		}
		out[modes.Role(role)] = rp
	}
	return out
}

var externalIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// externalEntryOK reports whether an operator entry is exactly parts identifiers joined by "/". An identifier
// cannot hold a "/", so an entry reads one way only.
func externalEntryOK(entry string, parts int) bool {
	ids := strings.Split(entry, "/")
	if len(ids) != parts {
		return false
	}
	for _, id := range ids {
		if !externalIDRE.MatchString(id) {
			return false
		}
	}
	return true
}

// externalOperatorKey is the deployment_operators entry of a principal of an application.
func externalOperatorKey(application, principal string) string { return application + "/" + principal }

// externalAdminKey is the tenant_admins entry of a principal of a tenant of an application.
func externalAdminKey(application, tenant, principal string) string {
	return application + "/" + tenant + "/" + principal
}

// isDeploymentOperator reports whether the section lists the principal of the application as a deployment operator.
// It is the rule of the operator commands (alixai providers, the readiness external leg) and of AuthorizeOperator.
func (c externalConfig) isDeploymentOperator(application, principal string) bool {
	return application != "" && principal != "" && slices.Contains(c.DeploymentOperators, externalOperatorKey(application, principal))
}

// externalModeChanged is the audit record of a mode change: who, where, from what to what. No content.
type externalModeChanged struct {
	Level     string `json:"level"`
	Previous  string `json:"previous"`
	Mode      string `json:"mode"`
	ChangedBy string `json:"changed_by"`
}

var externalEventModeChanged = audit.MustRegister[externalModeChanged](audit.TypeSpec{
	Name:        "providers.mode_changed",
	Description: "the mode (local, hybrid or external) of a deployment, application or tenant was changed by an operator",
	Levels:      audit.LevelTenant | audit.LevelApplication,
	Required:    []string{"level", "mode", "changed_by"},
})

// externalServeProvider provides gateway.ProvidersService when the section enables external mode.
func externalServeProvider(_ context.Context, d serveDeps) error {
	sec := externalSection
	if !sec.Enabled {
		return nil
	}
	if d.DB == nil || d.Audit == nil || d.DataDir == "" {
		return errors.New("external_providers: the store, the audit log and the data directory are required")
	}
	deployment := identity.CurrentConfig().DeploymentID
	if deployment == "" {
		return errors.New("external_providers: identity.deployment_id is not configured")
	}
	svc, err := newExternalAdmin(externalAdminDeps{
		DB: d.DB, Audit: d.Audit, Clock: d.Clock, Log: d.Log, DeploymentID: deployment,
		Dir: filepath.Join(d.DataDir, "external"), Section: sec,
	})
	if err != nil {
		return err
	}
	if err := gateway.Provide[gateway.ProvidersService](d.Services, svc); err != nil {
		return err
	}
	if err := gateway.Provide[gateway.ProvidersOAuthFlowService](d.Services, svc); err != nil {
		return err
	}
	if err := principalLLMProvide(d.Services, svc); err != nil {
		return err
	}
	if err := privateLLMProvide(d.Services, svc); err != nil {
		return err
	}
	if err := bindConfiguredCredentialRefs(context.Background(), svc, sec); err != nil {
		return err
	}
	return nil
}

// bindConfiguredCredentialRefs registers external_providers.credentials entries that name an application and tenant.
func bindConfiguredCredentialRefs(ctx context.Context, a *externalAdmin, sec externalConfig) error {
	for _, r := range sec.Credentials {
		if r.Application == "" || r.Tenant == "" {
			continue // CLI `providers credential add-ref` binds those; config entries without scope wait for an operator
		}
		scope := contracts.Scope{
			DeploymentID: a.deployment, ApplicationID: r.Application, TenantID: r.Tenant,
			WorkspaceID: r.Tenant, PrincipalID: "credential-ref", Roles: []string{"operator"},
			CapabilityScopes: []string{"providers.manage"}, SessionID: "credential-ref",
		}
		if err := bindCredentialRefsAtStartup(ctx, a, scope, []externalCredRefConfig{r}); err != nil {
			return err
		}
	}
	return nil
}

type externalAdminDeps struct {
	DB           *store.DB
	Audit        audit.Appender
	Clock        clock.Clock
	Log          *slog.Logger
	DeploymentID string
	// Dir holds the encrypted credential file, its key file and the mode file; created owner only.
	Dir     string
	Section externalConfig
	// Validator replaces the provider key validator (tests); nil builds the real one.
	Validator credentials.KeyValidator
}

// externalAdmin implements gateway.ProvidersService over the X packages.
type externalAdmin struct {
	deployment string
	clk        clock.Clock
	log        *slog.Logger
	audit      audit.Appender
	creds      *credentials.Service
	oauth      *oauth.Engine
	reg        *providerreg.Registry
	egress     *dataegress.Service
	budget     *providerbudget.Service
	modes      *externalModeFile
	resolver   *modes.Resolver
	deployOps  map[string]bool
	tenantOps  map[string]bool
	hostIDPath string           // SIWC ext_agent_host_id persistence
	oauthEg    *egress.Registry // destination allowlist for token/key exchange
}

var _ gateway.ProvidersService = (*externalAdmin)(nil)

func newExternalAdmin(d externalAdminDeps) (*externalAdmin, error) {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("external_providers: %w", err)
	}
	// Directory must be owner-traversable (0700); Restrict is the shared helper.
	if err := fsperm.Restrict(d.Dir, true); err != nil && runtime.GOOS != "windows" {
		return nil, fmt.Errorf("external_providers: %w", err)
	}
	// The secrets live in a directory of their own that the credentials package creates (owner only, with an ACL on
	// Windows) and refuses to use otherwise; Dir itself is created by os.MkdirAll, which on Windows only inherits.
	secrets := filepath.Join(d.Dir, "secrets")
	keys := credentials.KeyFileSource{Path: filepath.Join(secrets, "master.key")}
	file, err := credentials.NewEncryptedFile(filepath.Join(secrets, "credentials.enc"), keys)
	if err != nil {
		return nil, err
	}
	val := d.Validator
	if val == nil {
		base := d.Section.OpenAIBaseURL
		if base == "" {
			base = openai.DefaultBaseURL
		}
		val = externalKeyValidator{base: base, deployment: d.DeploymentID, sec: d.Section}
	}
	envBackend := credentials.NewEnvBackend("")
	fileBackend, err := credentials.NewFileBackend(secrets)
	if err != nil {
		return nil, err
	}
	cs, err := credentials.New(credentials.Config{
		DB: d.DB, Audit: d.Audit, DeploymentID: d.DeploymentID,
		Backends: []credentials.Backend{file, envBackend, fileBackend},
		Default:  credentials.BackendEncryptedFile, Validator: val, Clock: d.Clock, Logger: d.Log,
	})
	if err != nil {
		return nil, err
	}
	oauthReg := extFinalOAuthRegistry()
	oauthProviders, err := buildOAuthProviders(d.Section)
	if err != nil {
		return nil, err
	}
	eng, err := oauth.New(oauth.Config{
		Credentials: cs, Egress: oauthReg, Providers: oauthProviders, Clock: d.Clock, Logger: d.Log,
		PopupOrigins: d.Section.OAuth.PopupOrigins,
	})
	if err != nil {
		return nil, err
	}
	reg, err := providerreg.New(d.DB, providerreg.Config{Clock: d.Clock, Credentials: cs})
	if err != nil {
		return nil, err
	}
	eg, err := dataegress.New(dataegress.Deps{DB: d.DB, Clock: d.Clock, Audit: d.Audit})
	if err != nil {
		return nil, err
	}
	bg, err := providerbudget.New(d.DB, providerbudget.Config{Clock: d.Clock})
	if err != nil {
		return nil, err
	}
	mf, err := openExternalModeFile(filepath.Join(d.Dir, "modes.json"))
	if err != nil {
		return nil, err
	}
	res, err := modes.NewResolver(mf, eg)
	if err != nil {
		return nil, err
	}
	a := &externalAdmin{
		deployment: d.DeploymentID, clk: d.Clock, log: d.Log, audit: d.Audit, creds: cs, oauth: eng, reg: reg, egress: eg,
		budget: bg, modes: mf, resolver: res, deployOps: map[string]bool{}, tenantOps: map[string]bool{},
		hostIDPath: filepath.Join(d.Dir, "siwc_host_id"), oauthEg: oauthReg,
	}
	for _, p := range d.Section.DeploymentOperators {
		a.deployOps[p] = true
	}
	for _, e := range d.Section.TenantAdmins {
		a.tenantOps[e] = true
	}
	return a, nil
}

// AuthorizeOperator implements gateway.ProvidersService from the section's lists alone. The application of the
// verified scope is part of the match: the principal id is what the application's own issuer signed, so the same id
// in another application is another principal.
func (a *externalAdmin) AuthorizeOperator(_ context.Context, scope contracts.Scope, level gateway.ProvidersOperatorLevel) error {
	if scope.DeploymentID != a.deployment || scope.ApplicationID == "" || scope.PrincipalID == "" {
		return contracts.New(contracts.ErrPermissionDenied, "the provider routes are for operators")
	}
	if a.deployOps[externalOperatorKey(scope.ApplicationID, scope.PrincipalID)] {
		return nil
	}
	if level == gateway.ProvidersOperatorTenant && scope.TenantID != "" &&
		a.tenantOps[externalAdminKey(scope.ApplicationID, scope.TenantID, scope.PrincipalID)] {
		return nil
	}
	return contracts.New(contracts.ErrPermissionDenied, "the provider routes are for operators")
}

func (a *externalAdmin) operator(scope contracts.Scope) dataegress.Operator {
	return dataegress.Operator{DeploymentID: scope.DeploymentID, PrincipalID: scope.PrincipalID}
}

// ---- profiles and connections ----

// profileFor returns the application's live profile of a provider.
func (a *externalAdmin) profileFor(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (pv.ProviderProfile, error) {
	all, err := a.reg.List(ctx, scope)
	if err != nil {
		return pv.ProviderProfile{}, err
	}
	for _, p := range all {
		if p.ProviderID == provider && p.Lifecycle != pv.ProfileLifecycleRetired {
			return p, nil
		}
	}
	return pv.ProviderProfile{}, contracts.New(contracts.ErrValidationFailed, "no provider profile is registered for this provider").WithDetails("reason", "no_profile")
}

// externalBestRef is the connection the host should see: an active reference, else the most recently updated.
func externalBestRef(refs []pv.CredentialRef) (pv.CredentialRef, bool) {
	var best pv.CredentialRef
	found := false
	for _, r := range refs {
		switch {
		case !found:
			best, found = r, true
		case r.Status == pv.CredentialRefStatusActive && best.Status != pv.CredentialRefStatusActive:
			best = r
		case (r.Status == pv.CredentialRefStatusActive) == (best.Status == pv.CredentialRefStatusActive) && r.UpdatedAt > best.UpdatedAt:
			best = r
		}
	}
	return best, found
}

func externalStateOf(s pv.CredentialRefStatus) pv.ConnectionStatusState {
	switch s {
	case pv.CredentialRefStatusPending:
		return pv.ConnectionStatusStatePendingAuthorization
	case pv.CredentialRefStatusActive:
		return pv.ConnectionStatusStateConnected
	case pv.CredentialRefStatusExpired:
		return pv.ConnectionStatusStateExpired
	case pv.CredentialRefStatusRevoked:
		return pv.ConnectionStatusStateRevoked
	case pv.CredentialRefStatusQuarantined:
		return pv.ConnectionStatusStateQuarantined
	case pv.CredentialRefStatusReauthorizationRequired:
		return pv.ConnectionStatusStateReauthorizationRequired
	}
	return pv.ConnectionStatusStateInvalid
}

// status computes the connection status of one profile for the scope's tenant. It holds no reference id, no
// fingerprint and no secret: only what pv.ConnectionStatus can carry.
func (a *externalAdmin) status(ctx context.Context, scope contracts.Scope, prof pv.ProviderProfile) (pv.ConnectionStatus, error) {
	refs, err := a.creds.List(ctx, scope, prof.ProviderID)
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	st := pv.ConnectionStatus{
		AsOf:      common.Timestamp(a.clk.Now().UTC().Format(time.RFC3339Nano)),
		Provider:  pv.ProviderRef{ProfileID: prof.ProfileID, ProviderID: prof.ProviderID, ModelID: prof.ModelID, ModelVersion: prof.ModelVersion},
		Lifecycle: prof.Lifecycle,
		State:     pv.ConnectionStatusStateNotConnected,
		Warnings:  []pv.ConnectionWarning{},
		Health:    pv.ConnectionStatusHealthUnknown,
	}
	if ack := prof.BillingNoticeAck; ack != nil && ack.NoticeVersion == prof.BillingNotice.Version && ack.NoticeDigest == providerreg.NoticeDigest(prof.BillingNotice) {
		st.BillingNoticeAcknowledged = true
	}
	if ref, ok := externalBestRef(refs); ok {
		kind := ref.Kind
		st.State, st.Kind, st.Account = externalStateOf(ref.Status), &kind, ref.Account
		st.ConnectedAt, st.ExpiresAt = ref.Times.AuthorizedAt, ref.Times.ExpiresAt
		if ref.Warnings != nil {
			st.Warnings = ref.Warnings
		}
	}
	if states, err := a.budget.States(ctx, scope, prof.ProviderID, pv.ChannelInteractive); err == nil && len(states) > 0 {
		worst := pv.BudgetStatusOk
		for _, s := range states {
			if s.State == pv.BudgetStatusExhausted || (s.State == pv.BudgetStatusWarning && worst == pv.BudgetStatusOk) {
				worst = s.State
			}
		}
		st.BudgetState = &worst
	}
	return st, nil
}

func (a *externalAdmin) Connections(ctx context.Context, scope contracts.Scope) ([]pv.ConnectionStatus, error) {
	profs, err := a.reg.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make([]pv.ConnectionStatus, 0, len(profs))
	for _, p := range profs {
		if p.Lifecycle == pv.ProfileLifecycleRetired {
			continue
		}
		st, err := a.status(ctx, scope, p)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// bindIfFree binds a new reference to a profile that has none. A profile is application-level and may serve many
// tenants while a reference belongs to one tenant, so a profile that is already bound is never rebound here.
func (a *externalAdmin) bindIfFree(ctx context.Context, scope contracts.Scope, prof pv.ProviderProfile, ref pv.CredentialRefID) {
	if prof.CredRef != nil {
		return
	}
	if _, err := a.reg.BindCredential(ctx, scope, prof.ProfileID, ref); err != nil {
		a.log.Warn("providers: the new credential was not bound to the profile", "reason", string(contracts.CodeOf(err)))
	}
}

// ---- OAuth and manual credentials ----

func (a *externalAdmin) OAuthStart(ctx context.Context, scope contracts.Scope, req gateway.ProvidersOAuthStart) (gateway.ProvidersOAuthStarted, error) {
	return a.oauthStartWithOptions(ctx, scope, req)
}

// externalOAuthError keeps the typed code and the fixed reason, and says "manual_fallback" the way the API documents.
func externalOAuthError(err error) error {
	if oauth.IsManualFallback(err) {
		return contracts.New(contracts.ErrValidationFailed, "this provider is connected with a manual credential, not OAuth").WithDetails("reason", "manual_fallback")
	}
	return err
}

func (a *externalAdmin) OAuthCallback(ctx context.Context, scope contracts.Scope, cb gateway.ProvidersOAuthCallback) (pv.ConnectionStatus, error) {
	conn, err := a.oauth.Complete(ctx, scope, oauth.CallbackFromQuery(cb.RedirectURI, cb.Query))
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	prof, err := a.profileFor(ctx, scope, conn.Ref.Binding.ProviderID)
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	a.bindIfFree(ctx, scope, prof, conn.Ref.RefID)
	return a.status(ctx, scope, prof)
}

func (a *externalAdmin) SubmitCredential(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in gateway.ProvidersCredentialInput) (pv.ConnectionStatus, error) {
	prof, err := a.profileFor(ctx, scope, provider)
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	ref, err := a.creds.AddAPIKey(ctx, scope, credentials.AddAPIKeyRequest{
		ProviderID: provider, Label: in.Label, Key: credentials.Secret(in.Key.Reveal()), ExpiresAt: in.ExpiresAt, Scopes: in.Scopes,
	})
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	a.bindIfFree(ctx, scope, prof, ref.RefID)
	return a.status(ctx, scope, prof)
}

func (a *externalAdmin) Disconnect(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (pv.ConnectionStatus, error) {
	prof, err := a.profileFor(ctx, scope, provider)
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	refs, err := a.creds.List(ctx, scope, provider)
	if err != nil {
		return pv.ConnectionStatus{}, err
	}
	var first error
	for _, r := range refs {
		if r.Status == pv.CredentialRefStatusRevoked {
			continue
		}
		var err error
		if r.Kind == pv.CredentialKindOauth {
			_, err = a.oauth.Disconnect(ctx, scope, r.RefID)
		} else {
			_, err = a.creds.Revoke(ctx, scope, r.RefID)
		}
		if err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return pv.ConnectionStatus{}, first
	}
	return a.status(ctx, scope, prof)
}

func (a *externalAdmin) AcknowledgeBillingNotice(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in gateway.ProvidersBillingAckInput) (pv.BillingNoticeAck, error) {
	prof, err := a.profileFor(ctx, scope, provider)
	if err != nil {
		return pv.BillingNoticeAck{}, err
	}
	got, err := a.reg.AcknowledgeBillingNotice(ctx, scope, prof.ProfileID, providerreg.Acknowledgement{
		NoticeVersion: in.NoticeVersion, ConsumerSubscriptionIsNotAPIAccess: in.ConsumerSubscriptionIsNotAPIAccess,
		SeparateAPIBillingUnderstood: in.SeparateAPIBillingUnderstood,
	})
	if err != nil {
		return pv.BillingNoticeAck{}, err
	}
	if got.BillingNoticeAck == nil {
		return pv.BillingNoticeAck{}, contracts.New(contracts.ErrInternal, "the acknowledgement was not recorded")
	}
	return *got.BillingNoticeAck, nil
}

func (a *externalAdmin) Disclosure(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, in gateway.ProvidersDisclosureInput) (pv.EgressDisclosure, error) {
	prof, err := a.profileFor(ctx, scope, provider)
	if err != nil {
		return pv.EgressDisclosure{}, err
	}
	states, _ := a.budget.States(ctx, scope, provider, pv.ChannelInteractive)
	budget := pv.BudgetStatusOk
	for _, s := range states {
		if s.State == pv.BudgetStatusExhausted || (s.State == pv.BudgetStatusWarning && budget == pv.BudgetStatusOk) {
			budget = s.State
		}
	}
	return a.egress.Disclose(ctx, scope, prof, dataegress.DisclosureInput{
		Role: in.Role, Sources: in.Sources, EstimatedToksIn: in.EstimatedToksIn, MaxOutputToks: in.MaxOutputToks, Budget: budget,
	})
}

// ---- mode ----

func (a *externalAdmin) modeView(ctx context.Context, scope contracts.Scope) (gateway.ProvidersModeView, error) {
	r, err := a.resolver.Resolve(ctx, scope)
	if err != nil {
		return gateway.ProvidersModeView{}, err
	}
	v := gateway.ProvidersModeView{Effective: r.Mode, Configured: r.Configured, DecidedBy: r.DecidedBy, KillSwitch: r.KillSwitch, Levels: map[gateway.ProvidersModeLevel]pv.DeploymentMode{}}
	for lv, m := range r.Levels {
		v.Levels[gateway.ProvidersModeLevel(lv)] = m
	}
	return v, nil
}

func (a *externalAdmin) Mode(ctx context.Context, scope contracts.Scope) (gateway.ProvidersModeView, error) {
	return a.modeView(ctx, scope)
}

// SetMode persists the mode and audits the change in the same step: if the audit record cannot be appended the
// previous mode is restored and the call fails, so no change goes unrecorded.
func (a *externalAdmin) SetMode(ctx context.Context, scope contracts.Scope, level gateway.ProvidersModeLevel, mode pv.DeploymentMode) (gateway.ProvidersModeView, error) {
	// The route authorises first; the service holds the line as well, so no other caller can write a mode
	// without being an operator of the level: the deployment and application levels are the deployment's,
	// the tenant level is also its tenant administrator's.
	need := gateway.ProvidersOperatorDeployment
	if level == gateway.ProvidersModeTenant {
		need = gateway.ProvidersOperatorTenant
	}
	if err := a.AuthorizeOperator(ctx, scope, need); err != nil {
		return gateway.ProvidersModeView{}, err
	}
	lv := modes.Level(level)
	prev, had, err := a.modes.Mode(ctx, lv, scope)
	if err != nil {
		return gateway.ProvidersModeView{}, err
	}
	if !had {
		prev = modes.ModeLocal
	}
	if err := a.modes.set(lv, scope, mode); err != nil {
		return gateway.ProvidersModeView{}, err
	}
	loc, err := audit.ForScope(scope)
	if err == nil {
		_, err = externalEventModeChanged.Append(ctx, a.audit, loc, externalModeChanged{
			Level: string(level), Previous: string(prev), Mode: string(mode), ChangedBy: scope.PrincipalID,
		})
	}
	if err != nil {
		if had {
			_ = a.modes.set(lv, scope, prev)
		} else {
			_ = a.modes.unset(lv, scope)
		}
		return gateway.ProvidersModeView{}, contracts.Wrap(contracts.ErrUnavailable, err, "the mode change could not be recorded and was not applied")
	}
	return a.modeView(ctx, scope)
}

// ---- egress policy ----

func (a *externalAdmin) EgressPolicy(ctx context.Context, scope contracts.Scope, level pv.EgressLevel, source pv.SourceID) (pv.EgressPolicy, bool, error) {
	return a.egress.GetPolicy(ctx, scope, level, source)
}

func (a *externalAdmin) PutEgressPolicy(ctx context.Context, scope contracts.Scope, in gateway.ProvidersEgressPolicyInput) (pv.EgressPolicy, error) {
	need := gateway.ProvidersOperatorTenant
	if in.Level == pv.EgressLevelDeployment || in.Level == pv.EgressLevelApplication {
		need = gateway.ProvidersOperatorDeployment
	}
	if err := a.AuthorizeOperator(ctx, scope, need); err != nil {
		return pv.EgressPolicy{}, err
	}
	pin := dataegress.PolicyInput{
		Level: in.Level, SourceID: in.SourceID, ExpectedRevision: in.ExpectedRevision, Enabled: in.Enabled, SourceMode: in.SourceMode,
		SourceClass: in.SourceClass, AllowedProviders: in.AllowedProviders, AllowedModels: in.AllowedModels,
		AllowedClasses: in.AllowedClasses, AllowedRegions: in.AllowedRegions, Files: in.Files, Audio: in.Audio, WebQuery: in.WebQuery,
	}
	if in.Level == pv.EgressLevelDeployment {
		return a.egress.PutDeploymentPolicy(ctx, a.operator(scope), pin)
	}
	return a.egress.PutPolicy(ctx, scope, pin)
}

// ---- budgets and usage ----

func (a *externalAdmin) Budgets(ctx context.Context, scope contracts.Scope) (gateway.ProvidersBudgetsView, error) {
	if err := a.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorTenant); err != nil {
		return gateway.ProvidersBudgetsView{}, err
	}
	all, err := a.budget.ListBudgets(ctx, scope)
	if err != nil {
		return gateway.ProvidersBudgetsView{}, err
	}
	// A tenant administrator sees the provider-wide budgets and its own tenant's, never another tenant's.
	list := all
	if a.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment) != nil {
		list = nil
		for _, b := range all {
			if b.Subject.Kind == pv.BudgetSubjectKindProvider || (b.Subject.TenantID != nil && string(*b.Subject.TenantID) == scope.TenantID) {
				list = append(list, b)
			}
		}
	}
	ids := make([]pv.BudgetID, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.BudgetID)
	}
	// One read for every state, not one per budget.
	states, err := a.budget.StatesOf(ctx, scope, ids)
	if err != nil {
		return gateway.ProvidersBudgetsView{}, err
	}
	return gateway.ProvidersBudgetsView{Budgets: list, States: append([]pv.BudgetState{}, states...)}, nil
}

func (a *externalAdmin) PutBudget(ctx context.Context, scope contracts.Scope, b pv.Budget) (pv.Budget, error) {
	// A provider-wide budget limits every tenant: a deployment operator. The others limit one tenant,
	// workspace or principal.
	need := gateway.ProvidersOperatorTenant
	if b.Subject.Kind == pv.BudgetSubjectKindProvider {
		need = gateway.ProvidersOperatorDeployment
	}
	if err := a.AuthorizeOperator(ctx, scope, need); err != nil {
		return pv.Budget{}, err
	}
	// A tenant administrator limits its own tenant only; a changed budget keeps its subject (providerbudget).
	if b.Subject.TenantID != nil && string(*b.Subject.TenantID) != scope.TenantID &&
		a.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment) != nil {
		return pv.Budget{}, contracts.New(contracts.ErrScopeViolation, "a budget can only be set inside the caller's tenant").
			WithDetails("reason", "other_tenant")
	}
	return a.budget.PutBudget(ctx, scope, b)
}

func (a *externalAdmin) Usage(ctx context.Context, scope contracts.Scope, q gateway.ProvidersUsageQuery) (gateway.ProvidersUsageResult, error) {
	need := gateway.ProvidersOperatorTenant
	if q.Wide {
		need = gateway.ProvidersOperatorDeployment
	}
	if err := a.AuthorizeOperator(ctx, scope, need); err != nil {
		return gateway.ProvidersUsageResult{}, err
	}
	// The export is the tenant's (every workspace of it) or, for a deployment operator who asks, the application's.
	// PrincipalID (when set) narrows to one principal; operators only (already gated above).
	exp, err := a.budget.ExportUsage(ctx, scope, providerbudget.UsageFilter{From: q.From, To: q.To, TenantWide: !q.Wide, ApplicationWide: q.Wide, ProviderID: q.ProviderID, Channel: q.Channel, PrincipalID: q.PrincipalID})
	if err != nil {
		return gateway.ProvidersUsageResult{}, err
	}
	var buf bytes.Buffer
	if q.CSV {
		if err := exp.WriteCSV(&buf); err != nil {
			return gateway.ProvidersUsageResult{}, contracts.Wrap(contracts.ErrInternal, err, "the usage export could not be written")
		}
		return gateway.ProvidersUsageResult{ContentType: "text/csv; charset=utf-8", Body: buf.Bytes()}, nil
	}
	if err := json.NewEncoder(&buf).Encode(exp); err != nil {
		return gateway.ProvidersUsageResult{}, contracts.Wrap(contracts.ErrInternal, err, "the usage export could not be written")
	}
	return gateway.ProvidersUsageResult{ContentType: "application/json", Body: buf.Bytes()}, nil
}

// ---- kill switch ----

func (a *externalAdmin) killView(ctx context.Context) (gateway.ProvidersKillSwitch, error) {
	st, err := a.egress.KillSwitch(ctx, a.deployment)
	if err != nil {
		return gateway.ProvidersKillSwitch{}, err
	}
	out := gateway.ProvidersKillSwitch{Engaged: st.Engaged, Reason: st.Reason, ChangedBy: st.ChangedBy}
	if !st.ChangedAt.IsZero() {
		t := st.ChangedAt
		out.ChangedAt = &t
	}
	return out, nil
}

func (a *externalAdmin) KillSwitch(ctx context.Context, _ contracts.Scope) (gateway.ProvidersKillSwitch, error) {
	return a.killView(ctx)
}

func (a *externalAdmin) SetKillSwitch(ctx context.Context, scope contracts.Scope, engage bool, reason string) (gateway.ProvidersKillSwitch, error) {
	if err := a.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment); err != nil {
		return gateway.ProvidersKillSwitch{}, err
	}
	var err error
	if engage {
		err = a.egress.EngageKillSwitch(ctx, a.operator(scope), reason)
	} else {
		err = a.egress.ReleaseKillSwitch(ctx, a.operator(scope))
	}
	if err != nil {
		return gateway.ProvidersKillSwitch{}, err
	}
	return a.killView(ctx)
}

// ---- the persistent mode source ----

// externalModeFile is the modes.ModeSource of the deployment: one owner-only JSON file, written atomically and
// read from memory. Mode settings are the operator's decisions and live nowhere a request can reach.
type externalModeFile struct {
	path string
	mu   sync.RWMutex
	m    map[string]modes.Mode
}

var _ modes.ModeSource = (*externalModeFile)(nil)

func openExternalModeFile(path string) (*externalModeFile, error) {
	f := &externalModeFile{path: path, m: map[string]modes.Mode{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return f, nil
	case err != nil:
		return nil, fmt.Errorf("external_providers: the mode file cannot be read: %w", err)
	}
	var doc map[string]modes.Mode
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("external_providers: the mode file is not valid; fix or remove it (external mode stays off meanwhile)")
	}
	for k, m := range doc {
		if m.Valid() {
			f.m[k] = m
		}
	}
	return f, nil
}

func externalModeKey(level modes.Level, s contracts.Scope) (string, bool) {
	switch level {
	case modes.LevelDeployment:
		return "d/" + s.DeploymentID, true
	case modes.LevelApplication:
		return "a/" + s.DeploymentID + "/" + s.ApplicationID, true
	case modes.LevelTenant:
		return "t/" + s.DeploymentID + "/" + s.ApplicationID + "/" + s.TenantID, true
	}
	return "", false
}

// Mode implements modes.ModeSource.
func (f *externalModeFile) Mode(_ context.Context, level modes.Level, scope contracts.Scope) (modes.Mode, bool, error) {
	k, ok := externalModeKey(level, scope)
	if !ok {
		return "", false, contracts.New(contracts.ErrValidationFailed, "modes: not a valid level")
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	m, found := f.m[k]
	return m, found, nil
}

func (f *externalModeFile) set(level modes.Level, scope contracts.Scope, m modes.Mode) error {
	k, ok := externalModeKey(level, scope)
	if !ok || !m.Valid() {
		return contracts.New(contracts.ErrValidationFailed, "modes: not a valid level or mode")
	}
	return f.write(func(doc map[string]modes.Mode) { doc[k] = m })
}

func (f *externalModeFile) unset(level modes.Level, scope contracts.Scope) error {
	k, ok := externalModeKey(level, scope)
	if !ok {
		return contracts.New(contracts.ErrValidationFailed, "modes: not a valid level")
	}
	return f.write(func(doc map[string]modes.Mode) { delete(doc, k) })
}

// write applies change to a copy, saves it (temp file, owner only, rename) and only then swaps it in, so a failed
// write leaves memory and disk as they were.
func (f *externalModeFile) write(change func(map[string]modes.Mode)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := make(map[string]modes.Mode, len(f.m)+1)
	for k, v := range f.m {
		next[k] = v
	}
	change(next)
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "modes: the mode file cannot be encoded")
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return contracts.Wrap(contracts.ErrUnavailable, err, "modes: the mode file cannot be written")
	}
	if err := os.Chmod(tmp, 0o600); err != nil && runtime.GOOS != "windows" {
		_ = os.Remove(tmp)
		return contracts.Wrap(contracts.ErrUnavailable, err, "modes: the mode file cannot be protected")
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return contracts.Wrap(contracts.ErrUnavailable, err, "modes: the mode file cannot be replaced")
	}
	f.m = next
	return nil
}

// ---- the manual-key validator ----

// externalOneKey is a KeySource that returns the one key being validated, whatever the reference.
type externalOneKey struct{ key credentials.Secret }

func (k externalOneKey) Key(context.Context, contracts.Scope, string) (credentials.Secret, error) {
	return k.key, nil
}

// externalKeyValidator makes the minimally scoped request that proves a manual key works (see
// providers_external_validators.go): the provider's model list or one model, through the destination allowlist, with the candidate key and nothing else (no tenant, no prompt).
type externalKeyValidator struct {
	// base is the OpenAI origin; the other providers' origins and the Meta dialect come from sec (zero: the defaults).
	base       string
	deployment string
	sec        externalConfig
}

// ---------------------------------------------------------------------------------------------
// External mode in the dialogue pipeline, the revalidation job and the multimodal text gate (X-26).
//
// With external_providers.enabled the engine is built with a selector and a guard that can route a turn outward
// (extmode.Runtime), over the same mode file, kill switch, egress guard, profile registry and budget ledger the
// operator API uses. Off, or in the noexternal build, the engine keeps the local selector and LocalOnlyGuard:
// extWireSeams returns its inputs and the provider below does nothing.
//
// What is wired, and what is deliberately not:
//
//   - Controller turns: extmode.Runtime, with the routing policy wrapped in hybrid.ControllerLocalPolicy, so the
//     controller stays local under Hybrid and is routed only under External mode.
//   - Hybrid realisation: hybrid.Hybrid is built (audit log, budget ledger as Usage, the sender; FactClass empty
//     until the operator sets hybrid_fact_class, which keeps every turn local) and held on the stack. The dialogue
//     core has no response-step seam that hands a directive to it, so nothing calls Realise yet.
//   - RAG answers stay local: the citation resolver of the grounded answerer is private to the RAG area, so an
//     external answerer would issue references /v1/rag/citations could not open.
//   - The revalidation job runs on the one queue and pool of the RAG stack, under the service identity
//     providerreg.revalidate.
//
// Spoken text, vision questions and image prompts are what the user said: category user_message at class
// internal (the class extmode gives the user's own words); a file excerpt is file_content at class confidential,
// so it leaves only where every level of the egress policy turns the files setting on up to confidential.

// extWireProviderName sorts after "rag": the provider needs the queue and pool of the RAG stack, and runs after the
// operator API provider (providers_external).
const extWireProviderName = "rag_external"

// extWireOnCompose, when set, sees every stack the provider builds (tests).
var extWireOnCompose func(*extWireStack)

// analystExternalAdmin is the live externalAdmin for analyst profile / credentials lookup.
var analystExternalAdmin atomic.Pointer[externalAdmin]

// extWireDelegate is the selector and the guard the engine is built with. Until the provider has built the
// runtime it is the local selector and the local guard.
type extWireDelegate struct {
	local    dialogue.ProviderSelector
	guard    dialogue.PromptGuard
	packages dialogue.PackageSource
	budget   providers.Budget
	rt       atomic.Pointer[extmode.Runtime]
	// hy is the Hybrid realisation, set with the runtime; until then (and with external mode off) the delegate
	// declines every response and the engine realises it as it always did.
	hy atomic.Pointer[extGapHybrid]
}

var (
	_ dialogue.ProviderSelector = (*extWireDelegate)(nil)
	_ dialogue.PromptGuard      = (*extWireDelegate)(nil)
	_ dialogue.ResponseRealiser = (*extWireDelegate)(nil)
)

func extWireSeams(in extWireSeamsIn) extWireSeamsOut {
	if !externalSection.Enabled {
		return extWireSeamsOut{Selector: in.Selector, Guard: in.Guard}
	}
	d := &extWireDelegate{local: in.Selector, guard: in.Guard, packages: in.Packages, budget: in.Budget}
	return extWireSeamsOut{Selector: d, Guard: d, Handle: d}
}

// Select implements dialogue.ProviderSelector.
func (d *extWireDelegate) Select(ctx context.Context, scope contracts.Scope, role providers.Role) (dialogue.Provider, error) {
	if rt := d.rt.Load(); rt != nil {
		return rt.Select(ctx, scope, role)
	}
	return d.local.Select(ctx, scope, role)
}

// ResolvedControllerProfile returns the controller profile the turn's Select/Guard path would use
// for this scope (principal LLM bindings, private pins, then deployment default), including the
// principal credential overlay when a binding applies. Not the first deployment-policy profile alone.
func (d *extWireDelegate) ResolvedControllerProfile(ctx context.Context, scope contracts.Scope) (pv.ProviderProfile, bool) {
	if d == nil {
		return pv.ProviderProfile{}, false
	}
	rt := d.rt.Load()
	if rt == nil {
		return pv.ProviderProfile{}, false
	}
	p, err := rt.ControllerProfile(ctx, scope)
	if err != nil {
		return pv.ProviderProfile{}, false
	}
	return p, true
}

// ControllerAnalystCapable reports whether the resolved controller profile is analyst_capable.
func (d *extWireDelegate) ControllerAnalystCapable(ctx context.Context, scope contracts.Scope) (bool, error) {
	prof, ok := d.ResolvedControllerProfile(ctx, scope)
	if !ok || prof.AnalystCapable == nil {
		return false, nil
	}
	return *prof.AnalystCapable, nil
}

// Guard implements dialogue.PromptGuard.
func (d *extWireDelegate) Guard(ctx context.Context, in dialogue.GuardInput) (dialogue.PromptRequest, error) {
	if rt := d.rt.Load(); rt != nil {
		return rt.Guard(ctx, in)
	}
	return d.guard.Guard(ctx, in)
}

// extWireStack is what the provider built over the services of the operator API.
type extWireStack struct {
	admin      *externalAdmin
	runtime    *extmode.Runtime
	hybrid     *hybrid.Hybrid
	text       extWireTextEgress
	policy     modes.StaticPolicy
	sender     *extWireSender
	queue      *jobs.Queue
	every      time.Duration
	sweepEvery time.Duration
	deployment string
	log        *slog.Logger
	voice      *extFinalVoice
}

type extWireDeps struct {
	Admin    *externalAdmin
	Section  externalConfig
	Delegate *extWireDelegate
	Local    modes.LocalProviders
	Audit    audit.Appender
	Log      *slog.Logger
	Clock    clock.Clock
}

// extWireCompose builds the runtime, the Hybrid orchestration and the text gate over the services of the admin.
func extWireCompose(d extWireDeps) (*extWireStack, error) {
	a := d.Admin
	if a == nil || d.Delegate == nil {
		return nil, errors.New("external_providers: the operator service and the engine seams are required")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	policy := d.Section.extWirePolicy()
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	sender := &extWireSender{admin: a, sec: d.Section, reg: extFinalProviderRegistry(), log: d.Log, cache: map[string]ExternalProvider{}}
	serveExternalSender.Store(sender)
	gs := extmode.GeneratorSender{For: sender.generatorFor}
	st := &extWireStack{admin: a, policy: policy, sender: sender, text: extWireTextEgress{guard: a.egress}, deployment: a.deployment, log: d.Log}
	// Always bind to this admin — never the package holder from another serve/test.
	pll := newPrincipalLLM(a)
	basePolicy := principalLLMPolicy(hybrid.ControllerLocalPolicy(policy, a.resolver), a.resolver, pll)
	routePolicy := privateLLMPolicy(basePolicy, a.resolver, a.reg)
	profiles := privateLLMProfiles(principalLLMProfiles(a.reg, pll), pll)
	rt, err := extmode.New(extmode.Config{
		Local: d.Delegate.local, Modes: a.modes, KillSwitch: a.egress, Policy: routePolicy,
		LocalProviders: d.Local, Profiles: profiles, Egress: a.egress, Budgets: a.budget, Packages: d.Delegate.packages,
		Sender: gs, Budget: d.Delegate.budget, Clock: d.Clock, DirectiveClass: d.Section.DirectiveClass,
		OnOutcome: func(scope contracts.Scope, o modes.Outcome) { st.recordOutcome(scope, o) },
	})
	if err != nil {
		return nil, err
	}
	// The reply asked of a provider is capped by the request budget of the deployment profile (Lite allows less
	// than Hybrid's default of 512 tokens).
	maxOut := min(512, d.Delegate.budget.MaxOutputTokens)
	hy, err := hybrid.New(hybrid.Config{
		Modes: a.modes, KillSwitch: a.egress, Policy: privateLLMPolicy(principalLLMPolicy(policy, a.resolver, pll), a.resolver, a.reg), LocalProviders: d.Local, Profiles: profiles, Egress: a.egress,
		Budgets: a.budget, Sender: gs, Budget: d.Delegate.budget, MaxOutputToks: maxOut,
		Local: extGapLocalGen{sel: d.Delegate.local}, FactClass: pv.DataClassification(d.Section.HybridFactClass), Audit: d.Audit, Usage: a.budget, Clock: d.Clock,
	})
	if err != nil {
		return nil, err
	}
	st.runtime, st.hybrid = rt, hy
	if st.voice, err = extFinalNewVoice(a, policy, st.text, d.Log); err != nil {
		return nil, err
	}
	st.every = d.Section.RevalidateEvery
	if st.every <= 0 {
		st.every = providerreg.DefaultRevalidateInterval
	}
	st.sweepEvery = d.Section.BudgetSweepEvery
	if st.sweepEvery <= 0 {
		st.sweepEvery = extGapSweepDefaultEvery
	}
	return st, nil
}

// extWireRouteRecord is the audit record of a routed call that did not simply serve: the typed outcome and every
// fallback hop. Only the caller of the runtime audits them. No content, no prompt, no secret.
type extWireRouteRecord struct {
	Role        string           `json:"role"`
	Kind        string           `json:"kind"`
	Reason      string           `json:"reason,omitempty"`
	Mode        string           `json:"mode"`
	Target      string           `json:"target"`
	Profile     string           `json:"profile,omitempty"`
	Hops        []extWireHopData `json:"hops,omitempty"`
	DataSent    bool             `json:"data_sent"`
	Reservation string           `json:"reservation_id,omitempty"`
}

type extWireHopData struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Condition string `json:"condition,omitempty"`
}

var extWireEventRoute = audit.MustRegister[extWireRouteRecord](audit.TypeSpec{
	Name:        "providers.route_outcome",
	Description: "an external-route call did not simply serve, or fell back: the typed outcome and every hop; codes only, never content",
	Levels:      audit.LevelTenant,
	Required:    []string{"role", "kind", "mode", "target"},
})

// recordOutcome audits the outcome of a routed call when it failed or took a fallback hop. The call has already
// happened: a record that cannot be written is logged, never turned into a different answer.
func (s *extWireStack) recordOutcome(scope contracts.Scope, o modes.Outcome) {
	if o.Served() && len(o.Hops) == 0 {
		return
	}
	rec := extWireRouteRecord{
		Role: string(o.Role), Kind: string(o.Kind), Reason: o.Reason, Mode: string(o.Mode), Target: string(o.Target),
		Profile: string(o.Profile), DataSent: o.DataSent, Reservation: string(o.ReservationID),
	}
	for _, h := range o.Hops {
		rec.Hops = append(rec.Hops, extWireHopData{From: h.From, To: h.To, Condition: string(h.Condition)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loc, err := audit.ForScope(scope)
	if err == nil {
		_, err = extWireEventRoute.Append(ctx, s.admin.audit, loc, rec)
	}
	if err != nil {
		s.log.Warn("providers: the route outcome could not be recorded", "reason", string(contracts.CodeOf(err)))
	}
}

// extWireSender builds the adapter of a profile on first use and keeps it by profile, model version and credential
// reference. The destination allowlist is the one registry of the process; each adapter adds its own origin to it.
type extWireSender struct {
	admin *externalAdmin
	sec   externalConfig
	reg   *egress.ProviderRegistry
	log   *slog.Logger
	mu    sync.Mutex
	cache map[string]ExternalProvider
}

func (s *extWireSender) generatorFor(p pv.ProviderProfile) (providers.MultimodalGenerator, error) {
	ref := ""
	if p.CredRef != nil {
		ref = string(*p.CredRef)
	} else if p.ProviderID != providerreg.ProviderIDSelfHosted {
		return nil, contracts.New(contracts.ErrUnauthenticated, "the provider profile has no credential")
	}
	key := strings.Join([]string{string(p.ProfileID), string(p.ProviderID), string(p.ModelID), string(p.ModelVersion), ref, p.Endpoint}, "|")
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[key]; ok {
		return c, nil
	}
	spec := ExternalProviderSpec{Provider: string(p.ProviderID), Egress: s.reg, Logger: s.log}
	bearer := extFinalBearerKeys{provider: string(p.ProviderID), service: s.admin.creds, engine: s.admin.oauth}
	switch string(p.ProviderID) {
	case ExternalProviderOpenAI:
		spec.OpenAI = openai.Config{BaseURL: s.sec.OpenAIBaseURL, CredentialRef: ref, Model: string(p.ModelID)}
		spec.Keys = bearer
	case ExternalProviderAnthropic:
		spec.Anthropic = anthropic.Config{BaseURL: s.sec.AnthropicBaseURL, CredentialRef: ref, Model: string(p.ModelID)}
		spec.Keys = anthropic.ServiceKeys{Service: s.admin.creds}
	case ExternalProviderGemini:
		spec.Gemini = gemini.Config{BaseURL: s.sec.GeminiBaseURL, CredentialRef: ref, Model: string(p.ModelID)}
		spec.Keys = gemini.ServiceKeys{Service: s.admin.creds}
	case ExternalProviderMeta:
		// A reply that names the wire id is reported under the registry id, which the router compares with.
		wire, reply := string(p.ModelID), ""
		if w, ok := s.sec.MetaModels[wire]; ok {
			wire, reply = w, string(p.ModelID)
		}
		spec.Meta = meta.Config{
			Dialect: meta.Dialect(s.sec.MetaDialect), BaseURL: s.sec.MetaBaseURL, APIPrefix: s.sec.MetaAPIPrefix, CredentialRef: ref, Model: wire,
			ReplyModel: reply,
		}
		spec.Keys = meta.ServiceKeys{Service: s.admin.creds}
	case ExternalProviderOpenRouter:
		wire, reply := string(p.ModelID), ""
		if w, ok := s.sec.OpenRouterModels[wire]; ok {
			wire, reply = w, string(p.ModelID)
		}
		spec.OpenRouter = openrouter.Config{BaseURL: s.sec.OpenRouterBaseURL, CredentialRef: ref, Model: wire, ReplyModel: reply}
		spec.Keys = bearer
	case ExternalProviderXAI:
		wire, reply := string(p.ModelID), ""
		if w, ok := s.sec.XAIModels[wire]; ok {
			wire, reply = w, string(p.ModelID)
		}
		spec.XAI = xai.Config{BaseURL: s.sec.XAIBaseURL, CredentialRef: ref, Model: wire, ReplyModel: reply}
		spec.Keys = bearer
	case ExternalProviderMoonshot:
		wire, reply := string(p.ModelID), ""
		if w, ok := s.sec.MoonshotModels[wire]; ok {
			wire, reply = w, string(p.ModelID)
		}
		spec.Moonshot = moonshot.Config{BaseURL: s.sec.MoonshotBaseURL, CredentialRef: ref, Model: wire, ReplyModel: reply}
		spec.Keys = bearer
	case ExternalProviderMistral:
		wire, reply := string(p.ModelID), ""
		if w, ok := s.sec.MistralModels[wire]; ok {
			wire, reply = w, string(p.ModelID)
		}
		spec.Mistral = mistral.Config{BaseURL: s.sec.MistralBaseURL, CredentialRef: ref, Model: wire, ReplyModel: reply}
		spec.Keys = bearer
	case string(providerreg.ProviderIDSelfHosted):
		spec.SelfHosted = selfhosted.Config{BaseURL: p.Endpoint, CredentialRef: ref, Model: string(p.ModelID)}
		if ref != "" {
			spec.Keys = extFinalBearerKeys{provider: string(p.ProviderID), service: s.admin.creds, engine: s.admin.oauth}
		}
	default:
		return nil, contracts.New(contracts.ErrUnavailable, "no external adapter is built for this provider")
	}
	c, err := NewExternalProvider(spec)
	if err != nil {
		return nil, err
	}
	if spec.Provider == ExternalProviderGemini {
		var eng extFinalTokenEngine
		var kinds extFinalCredentialKinds
		if s.admin.oauth != nil {
			eng = s.admin.oauth
		}
		if s.admin.creds != nil {
			kinds = s.admin.creds
		}
		if c, err = extFinalGeminiBoth(spec, c, eng, kinds); err != nil {
			return nil, err
		}
	}
	s.cache[key] = c
	return c, nil
}

// extWireTextEgress backs extmodal.TextEgress with the data-egress guard: the text is evaluated as a one-part
// candidate against the registry profile of the target, so the kill switch, the policy chain, the allowed classes
// and the allowlists decide, and a nil error means the guard allowed it.
type extWireTextEgress struct{ guard dataegress.Guard }

var _ extmodal.TextEgress = extWireTextEgress{}

// AllowText implements extmodal.TextEgress. It returns the envelope and the decision of the guard, which the
// budget reservation of the call belongs to.
func (e extWireTextEgress) AllowText(ctx context.Context, scope contracts.Scope, t extmodal.Target, p extmodal.Purpose, text string) (extmodal.TextDecision, error) {
	if e.guard == nil {
		return extmodal.TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "the data-egress guard is not wired")
	}
	role, category, class := pv.RoleVision, string(pv.SendCategoryUserMessage), string(pv.DataClassificationInternal)
	switch p {
	case extmodal.PurposeSpeech:
		role = pv.RoleTTS
	case extmodal.PurposeVisionAsk:
	case extmodal.PurposeImagePrompt:
		role = pv.RoleImageGeneration
	case extmodal.PurposeExcerpt:
		category, class = string(pv.SendCategoryFileContent), string(pv.DataClassificationConfidential)
	default:
		return extmodal.TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "this text has no purpose the guard knows")
	}
	res, err := e.guard.Evaluate(ctx, dataegress.Candidate{
		Scope: scope, Profile: t.Profile, Role: role, Channel: pv.ChannelInteractive, TraceID: "trc_extmodal",
		Instructions: "Multimodal text check.",
		Messages: []dataegress.Message{{
			Role:  pv.RequestEnvelopeMessageRoleUser,
			Parts: []dataegress.Part{{Kind: dataegress.PartText, Text: text, Category: category, Class: class}},
		}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  1, TimeoutMS: 1000,
	})
	if err != nil {
		return extmodal.TextDecision{}, err
	}
	// The caller sends text as it is, so it passes only when the guard kept it unchanged (not scrubbed, cut or dropped).
	if res.Envelope == nil || len(res.Parts) != 1 || !res.Parts[0].Kept || res.Parts[0].Why != "" {
		return extmodal.TextDecision{}, contracts.New(contracts.ErrPermissionDenied, "the data-egress guard allows this text only changed").WithDetails("reason", "text_changed")
	}
	return extmodal.TextDecision{EnvelopeID: res.Envelope.EnvelopeID, DecisionID: res.Decision.DecisionID}, nil
}

// ---- the revalidation job ----

// The shape of the scope providerreg runs its revalidation job under (providerreg/jobs.go): it names the
// application and reaches no tenant data. The authority holds the pool to exactly that shape.
const (
	extWireJobTenant     = "alixai-providerreg"
	extWireJobPrincipal  = "svc_providerreg_revalidate"
	extWireJobRole       = "service"
	extWireJobCapability = "providerreg.revalidate"
)

// extWireRevalidateAuthority is the authority of the pool for providerreg.revalidate: that identity, that job type,
// no payload, this deployment and the service scope of the job, nothing wider.
type extWireRevalidateAuthority struct{ deployment string }

var _ jobs.ServiceAuthorizer = extWireRevalidateAuthority{}

// AuthorizeService implements jobs.ServiceAuthorizer.
func (a extWireRevalidateAuthority) AuthorizeService(_ context.Context, identity string, job jobs.Job) error {
	deny := func(msg string) error { return contracts.New(contracts.ErrPermissionDenied, msg) }
	payload := bytes.TrimSpace(job.Payload)
	switch {
	case identity != providerreg.ServiceIdentity:
		return deny("unknown service identity")
	case job.Type != providerreg.JobRevalidate:
		return deny("the service identity does not run this job type")
	case len(payload) > 0 && !bytes.Equal(payload, []byte("null")):
		return deny("the revalidation job carries no payload")
	}
	s := job.Scope
	if err := s.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "the revalidation job has no valid scope")
	}
	if a.deployment == "" || s.DeploymentID != a.deployment || s.TenantID != extWireJobTenant || s.WorkspaceID != extWireJobTenant ||
		s.PrincipalID != extWireJobPrincipal || !slices.Equal(s.Roles, []string{extWireJobRole}) ||
		!slices.Equal(s.CapabilityScopes, []string{extWireJobCapability}) {
		return deny("the revalidation job is not under the service scope")
	}
	return nil
}

// extWireProvide builds the external-mode runtime over the services of the operator API, puts the revalidation job
// on the queue and pool of the RAG stack, and starts the schedule that queues it. It does nothing unless the
// section enables external mode.
func extWireProvide(_ context.Context, d serveDeps) error {
	sec := externalSection
	if !sec.Enabled {
		return nil
	}
	del, ok := d.External.(*extWireDelegate)
	if !ok {
		return errors.New("external_providers: the engine was built without the external seams")
	}
	svc, ok := gateway.Lookup[gateway.ProvidersService](d.Services)
	if !ok {
		return errors.New("external_providers: the operator service is not provided")
	}
	admin, ok := svc.(*externalAdmin)
	if !ok {
		return errors.New("external_providers: the operator service is not the one this build wires")
	}
	rs, ok := gateway.Lookup[*ragStack](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "external_providers: the revalidation job runs on the queue and pool of the RAG stack, which were not provided")
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	st, err := extWireCompose(extWireDeps{
		Admin: admin, Section: sec, Delegate: del, Local: modes.FromRegistry(d.Providers), Audit: d.Audit, Log: d.Log, Clock: d.Clock,
	})
	if err != nil {
		return err
	}
	st.queue = rs.queue
	if err := rs.queue.Register(providerreg.JobSpec()); err != nil {
		return err
	}
	if err := rs.shareJobs(providerreg.ServiceIdentity, extWireRevalidateAuthority{deployment: admin.deployment}, admin.reg.Handle); err != nil {
		return err
	}
	if err := rs.queue.Register(extGapSweepSpec()); err != nil {
		return err
	}
	if err := rs.shareJobs(extGapSweepIdentity, extGapSweepAuthority{deployment: admin.deployment}, admin.extGapHandleSweep); err != nil {
		return err
	}
	// Hybrid realisation and external RAG answering (X-27). Both decide per call from the effective mode of the
	// scope: with Local mode, or when the deployment names nothing for the role, they decline and the local paths
	// run exactly as without external mode.
	del.hy.Store(&extGapHybrid{h: st.hybrid, modes: admin.resolver, routed: len(st.policy[modes.RoleRealisation].Profiles) > 0})
	if err := extGapWireKnowledge(d, rs, st, admin); err != nil {
		return err
	}
	del.rt.Store(st.runtime)
	extFinalVoiceHolder.Store(st.voice)
	analystExternalAdmin.Store(admin)
	// Analyst companion (ADR 0047): web_query egress, controller profile, credentials, budgets.
	serveExternalEgress = func() dataegress.Guard { return admin.egress }
	serveExternalControllerProfile = func(ctx context.Context, scope contracts.Scope) (pv.ProviderProfile, bool) {
		return del.ResolvedControllerProfile(ctx, scope)
	}
	webProv := "searxng"
	if p := dialogue.Loaded().Analyst.Web.Provider; p != "" {
		webProv = p
	}
	if r, err := admin.creds.Resolver(contracts.Scope{
		DeploymentID: admin.deployment, ApplicationID: "alixai-web", TenantID: "alixai-web",
		WorkspaceID: "alixai-web", PrincipalID: "svc_analyst_web", Roles: []string{"service"},
		CapabilityScopes: []string{"credentials.resolve"}, SessionID: "analyst-web",
	}, credentials.ResolverOptions{Provider: pv.ProviderID(webProv), Purpose: "web_search"}); err == nil {
		bindAnalystWebSecrets(r)
	}
	bindAnalystBudgets(admin.budget)
	// One AddService only: a second registration would overwrite tests (and any
	// harness) that keep a single start handle — fold analyst cleanup into stop.
	if d.AddService != nil {
		d.AddService(func(ctx context.Context) (stop func()) {
			stopRev := st.startRevalidation(ctx)
			return func() {
				stopRev()
				analystExternalAdmin.Store(nil)
				serveExternalSender.Store(nil)
				serveExternalEgress = func() dataegress.Guard { return nil }
				serveExternalControllerProfile = func(context.Context, contracts.Scope) (pv.ProviderProfile, bool) {
					return pv.ProviderProfile{}, false
				}
				bindAnalystWebSecrets(nil)
				bindAnalystBudgets(nil)
			}
		})
	}
	if extWireOnCompose != nil {
		extWireOnCompose(st)
	}
	return nil
}

// startRevalidation is the service of the process (serveDeps.AddService): it queues the revalidation job at once
// and every period until ctx ends. The function it returns stops the loop and waits for it, before the store closes.
func (s *extWireStack) startRevalidation(ctx context.Context) (stop func()) {
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	sweepDone := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.admin.reg.RunRevalidationSchedule(rctx, s.queue, s.deployment, s.every, func(err error) {
			s.log.Warn("providers: scheduling the revalidation job failed; the next pass goes on", "reason", string(contracts.CodeOf(err)))
		})
	}()
	go func() {
		defer close(sweepDone)
		s.extGapRunSweepSchedule(rctx)
	}()
	return func() {
		cancel()
		<-done
		<-sweepDone
	}
}

// ---------------------------------------------------------------------------------------------
// What X-26 left out (X-27): Hybrid realisation in the response step, external RAG answering, the budget sweep.
//
// One credential per profile: a profile binds ONE credential reference, and a reference belongs to one tenant, so a
// second tenant that wants its own key needs its own profile (bindIfFree never rebinds). That is by design here.
//
// The external voice factory with Attest and the Gemini OAuth token source were left to X-28; see the extFinal
// section at the end of this file.

// extGapHybridRealiser is what the response step needs of hybrid.Hybrid.
type extGapHybridRealiser interface {
	Realise(ctx context.Context, t hybrid.Turn) (hybrid.Realised, error)
}

var _ extGapHybridRealiser = (*hybrid.Hybrid)(nil)

// extGapHybrid is the Hybrid realisation behind the delegate: the orchestration, the mode resolver of the
// deployment, and whether the deployment routes the realisation role at all.
type extGapHybrid struct {
	h      extGapHybridRealiser
	modes  *modes.Resolver
	routed bool
}

// RealiseResponse implements dialogue.ResponseRealiser. It takes a turn only in Hybrid mode and only when the
// deployment lists profiles for the realisation role; every other turn is declined, so Local mode, External mode
// (whose responses go through the selector) and a Hybrid deployment with no routing keep the engine's own path. Taking
// a turn does not mean sending it: hybrid.Hybrid applies the fail-closed rules of X-22 (egress policy, fact class,
// budget, verification) and answers locally or from the template wherever they say so.
func (d *extWireDelegate) RealiseResponse(ctx context.Context, in dialogue.ResponseInput) (verify.Outcome, bool, error) {
	hy := d.hy.Load()
	if hy == nil || !hy.routed || hy.h == nil || hy.modes == nil {
		return verify.Outcome{}, false, nil
	}
	if r, err := hy.modes.Resolve(ctx, in.Scope); err != nil || r.Mode != modes.ModeHybrid {
		return verify.Outcome{}, false, nil
	}
	got, err := hy.h.Realise(ctx, hybrid.Turn{Scope: in.Scope, Directive: in.Directive, Receipt: in.Receipt, Options: in.Options})
	if err != nil {
		extFinalAttestTurn(in, verify.Outcome{}, true)
		return verify.Outcome{}, true, err
	}
	out := verify.Outcome{Text: got.Text, Source: got.Source, Reason: got.Reason, Violations: got.Violations}
	extFinalAttestTurn(in, out, false)
	return out, true, nil
}

// extGapLocalGen is the local text generator of the Hybrid orchestration: the controller of the engine's own local
// selector, looked up per call, so a turn Hybrid keeps local is written by the same model, with the same budget, as
// without the seam.
type extGapLocalGen struct{ sel dialogue.ProviderSelector }

var _ providers.TextGenerator = extGapLocalGen{}

func (g extGapLocalGen) generator(ctx context.Context, scope contracts.Scope) (providers.TextGenerator, error) {
	if g.sel == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "no local model is available")
	}
	p, err := g.sel.Select(ctx, scope, providers.RoleController)
	if err != nil {
		return nil, err
	}
	if p.Generator == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "no local model is available")
	}
	return p.Generator, nil
}

// Chat implements providers.TextGenerator.
func (g extGapLocalGen) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	gen, err := g.generator(ctx, scope)
	if err != nil {
		return providers.ChatResult{}, err
	}
	return gen.Chat(ctx, scope, req)
}

// ChatStream implements providers.TextGenerator.
func (g extGapLocalGen) ChatStream(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatStream, error) {
	gen, err := g.generator(ctx, scope)
	if err != nil {
		return nil, err
	}
	return gen.ChatStream(ctx, scope, req)
}

// ---- external RAG answering ----

// extGapKnowledge is the dialogue.Knowledge of an engine with external mode on. In External mode the question is
// answered by extmode.Knowledge (excerpts filtered source by source by the egress guard, then the external answerer);
// in Local and Hybrid mode, and whenever the mode cannot be read, it is the local path of serve unchanged, so RAG
// stays local there. Both are bounded by the same request and result checks as POST /v1/rag/answer.
type extGapKnowledge struct {
	modes    *modes.Resolver
	local    dialogue.Knowledge
	external dialogue.Knowledge
}

var _ dialogue.Knowledge = extGapKnowledge{}

// Answer implements dialogue.Knowledge.
func (k extGapKnowledge) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	if r, err := k.modes.Resolve(ctx, scope); err == nil && r.Mode == modes.ModeExternal {
		return k.external.Answer(ctx, scope, req)
	}
	return k.local.Answer(ctx, scope, req)
}

// extGapQueries is the RAG area with its answer replaced: search and citation opening stay the local service's.
type extGapQueries struct {
	gateway.RAGQueries
	answer dialogue.Knowledge
}

// Answer implements gateway.RAGQueries.
func (q extGapQueries) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	return q.answer.Answer(ctx, scope, req)
}

// extGapNewKnowledge builds the Knowledge of an engine with external mode on. The external answerer is built over the
// local answerer's citation resolver, so a reference it issues opens through /v1/rag/citations like any other.
func extGapNewKnowledge(rt *extmode.Runtime, res *modes.Resolver, local gateway.RAGQueries, la *ragServeLocalAnswer, retr extmode.Retriever, catalog query.SourceCatalog, clk clock.Clock) (dialogue.Knowledge, error) {
	gen, err := providers.WithBudget(rt.ExternalGenerator(), la.Budget)
	if err != nil {
		return nil, err
	}
	ext, err := answer.New(answer.Deps{Generator: gen, Catalog: catalog, Clock: clk, Resolver: la.Answerer.Resolver()}, la.Config)
	if err != nil {
		return nil, fmt.Errorf("external_providers: the external answerer: %w", err)
	}
	know, err := rt.NewKnowledge(retr, la.Answerer, ext)
	if err != nil {
		return nil, err
	}
	return extGapKnowledge{
		modes:    res,
		local:    gateway.RAGQueriesScoped(local),
		external: gateway.RAGQueriesScoped(extGapQueries{RAGQueries: local, answer: know}),
	}, nil
}

// extGapWireKnowledge gives the engine, when serve has one and the RAG area is provided, the Knowledge above.
func extGapWireKnowledge(d serveDeps, rs *ragStack, st *extWireStack, admin *externalAdmin) error {
	eng, ok := gateway.Lookup[*dialogue.Engine](d.Services)
	if !ok {
		return nil
	}
	local, ok1 := gateway.Lookup[gateway.RAGQueries](d.Services)
	la, ok2 := gateway.Lookup[*ragServeLocalAnswer](d.Services)
	if !ok1 || !ok2 {
		return nil
	}
	know, err := extGapNewKnowledge(st.runtime, admin.resolver, local, la, rs.pipeline, rs.sources, d.Clock)
	if err != nil {
		return err
	}
	eng.SetKnowledge(know)
	return nil
}

// ---- the budget sweep job ----

// The sweep marks the timed-out budget reservations expired and drops their holds (providerbudget.Service.Sweep). It is
// a class 6 maintenance job, one per application with a live profile and period, under a service identity that may run
// that job type and nothing else, in a scope that names the application and reaches no tenant data.
const (
	extGapSweepJob          = "providerbudget.sweep"
	extGapSweepIdentity     = "providerbudget.sweep"
	extGapSweepTenant       = "alixai-providerbudget"
	extGapSweepPrincipal    = "svc_providerbudget_sweep"
	extGapSweepSession      = "ses_providerbudget_sweep"
	extGapSweepRole         = "service"
	extGapSweepCapability   = "providerbudget.sweep"
	extGapSweepDefaultEvery = 15 * time.Minute
	extGapSweepAttempts     = 3
)

func extGapSweepSpec() jobs.TypeSpec {
	return jobs.TypeSpec{Name: extGapSweepJob, Class: jobs.ClassMaintenance, MaxAttempts: extGapSweepAttempts}
}

func extGapSweepScope(deployment, application string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: deployment, ApplicationID: application, TenantID: extGapSweepTenant, WorkspaceID: extGapSweepTenant,
		PrincipalID: extGapSweepPrincipal, Roles: []string{extGapSweepRole}, CapabilityScopes: []string{extGapSweepCapability},
		SessionID: extGapSweepSession,
	}
}

// extGapSweepAuthority is the authority of the pool for the sweep: that identity, that job type, no payload, this
// deployment and the service scope above, nothing wider.
type extGapSweepAuthority struct{ deployment string }

var _ jobs.ServiceAuthorizer = extGapSweepAuthority{}

// AuthorizeService implements jobs.ServiceAuthorizer.
func (a extGapSweepAuthority) AuthorizeService(_ context.Context, identity string, job jobs.Job) error {
	deny := func(msg string) error { return contracts.New(contracts.ErrPermissionDenied, msg) }
	payload := bytes.TrimSpace(job.Payload)
	switch {
	case identity != extGapSweepIdentity:
		return deny("unknown service identity")
	case job.Type != extGapSweepJob:
		return deny("the service identity does not run this job type")
	case len(payload) > 0 && !bytes.Equal(payload, []byte("null")):
		return deny("the budget sweep carries no payload")
	}
	s := job.Scope
	if err := s.Validate(); err != nil {
		return contracts.Wrap(contracts.ErrPermissionDenied, err, "the budget sweep has no valid scope")
	}
	if a.deployment == "" || s.DeploymentID != a.deployment || s.TenantID != extGapSweepTenant || s.WorkspaceID != extGapSweepTenant ||
		s.PrincipalID != extGapSweepPrincipal || !slices.Equal(s.Roles, []string{extGapSweepRole}) ||
		!slices.Equal(s.CapabilityScopes, []string{extGapSweepCapability}) {
		return deny("the budget sweep is not under the service scope")
	}
	return nil
}

// extGapHandleSweep attaches the sweep handler to the pool. The job runs only as the declared service, with no
// payload; the work is idempotent (an expired reservation is not touched again), so at-least-once delivery is safe.
func (a *externalAdmin) extGapHandleSweep(pool *jobs.Pool) error {
	if pool == nil {
		return contracts.New(contracts.ErrValidationFailed, "external_providers: no job pool to attach the budget sweep to")
	}
	return pool.Handle(extGapSweepJob, func(ctx context.Context, job jobs.Job) error {
		if job.ServiceIdentity != extGapSweepIdentity {
			return jobs.Permanent(contracts.New(contracts.ErrPermissionDenied, "the budget sweep runs only under its service identity"))
		}
		if p := bytes.TrimSpace(job.Payload); len(p) > 0 && !bytes.Equal(p, []byte("null")) {
			return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "the budget sweep carries no payload"))
		}
		if err := job.Scope.Validate(); err != nil {
			return jobs.Permanent(err)
		}
		n, err := a.budget.Sweep(ctx, job.Scope)
		if err == nil && n > 0 {
			a.log.Info("providers: timed-out budget reservations expired", "count", n)
		}
		return err
	})
}

// extGapScheduleSweep queues one sweep job for every application that holds a live profile; at most one per
// application and period (the idempotency key names the period), so calling it more often is harmless.
func (s *extWireStack) extGapScheduleSweep(ctx context.Context) (queued int, err error) {
	if s.queue == nil {
		return 0, contracts.New(contracts.ErrUnavailable, "external_providers: the budget sweep needs a job queue")
	}
	apps, err := s.admin.reg.LiveApplications(ctx)
	if err != nil {
		return 0, err
	}
	every := s.sweepEvery
	if every <= 0 {
		every = extGapSweepDefaultEvery
	}
	period := s.admin.clk.Now().UTC().Truncate(every).Format("20060102T150405Z")
	for _, app := range apps {
		out, err := s.queue.Enqueue(ctx, extGapSweepScope(s.deployment, app), jobs.EnqueueRequest{
			Type: extGapSweepJob, IdempotencyKey: "sweep:" + period, ServiceIdentity: extGapSweepIdentity,
		})
		if err != nil {
			return queued, err
		}
		if !out.Duplicate {
			queued++
		}
	}
	return queued, nil
}

// extGapRunSweepSchedule queues the sweep now and every period until ctx ends. A failed pass is logged and the
// schedule goes on: Reserve expires what it meets lazily in the meantime.
func (s *extWireStack) extGapRunSweepSchedule(ctx context.Context) {
	every := s.sweepEvery
	if every <= 0 {
		every = extGapSweepDefaultEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if _, err := s.extGapScheduleSweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("providers: scheduling the budget sweep failed; the next pass goes on", "reason", string(contracts.CodeOf(err)))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ---------------------------------------------------------------------------------------------
// What X-27 left out (X-28): the Gemini OAuth token source and the external voice factory.

// extFinalTokenEngine is the part of the shared OAuth engine a token source needs. *oauth.Engine implements it.
type extFinalTokenEngine interface {
	Token(ctx context.Context, scope contracts.Scope, req oauth.TokenRequest) (credentials.Secret, error)
}

var _ extFinalTokenEngine = (*oauth.Engine)(nil)

// extFinalEngineTokens is the gemini.TokenSource of the composition root: the shared OAuth engine, which refreshes the
// access token when it is near expiry and resolves it through the credential service (scope, provider, revocation
// and expiry checks, and the audit of the use). The adapter may not import the engine, so the wrapper lives here.
type extFinalEngineTokens struct{ e extFinalTokenEngine }

var _ gemini.TokenSource = extFinalEngineTokens{}

// Bearer implements gemini.TokenSource.
func (w extFinalEngineTokens) Bearer(ctx context.Context, scope contracts.Scope, ref string, scopes []string, forceRefresh bool) (credentials.Secret, error) {
	if w.e == nil {
		return "", contracts.New(contracts.ErrInternal, "gemini: the OAuth engine is not wired").
			WithDetails("provider_error_kind", string(pv.ProviderErrorKindAuthenticationFailed)).WithDetails("sent", false)
	}
	return w.e.Token(ctx, scope, oauth.TokenRequest{Ref: pv.CredentialRefID(ref), Scopes: scopes, ForceRefresh: forceRefresh})
}

// extFinalBearerKeys resolves API keys directly and OAuth access tokens through the engine (SIWC refresh +
// chatgpt.tokens.use.direct gate). oauth_issued_key (OpenRouter) resolves like an API key.
type extFinalBearerKeys struct {
	provider string
	service  *credentials.Service
	engine   *oauth.Engine
	scopes   []string
}

var _ openai.KeySource = extFinalBearerKeys{}

func (k extFinalBearerKeys) Key(ctx context.Context, scope contracts.Scope, ref string) (credentials.Secret, error) {
	if k.service == nil {
		return "", contracts.New(contracts.ErrInternal, k.provider+": the credential service is not wired")
	}
	got, err := k.service.Get(ctx, scope, pv.CredentialRefID(ref))
	if err != nil {
		return "", err
	}
	switch got.Kind {
	case pv.CredentialKindOauth:
		if k.engine == nil {
			return "", contracts.New(contracts.ErrInternal, k.provider+": the OAuth engine is not wired")
		}
		scopes := k.scopes
		if k.provider == ExternalProviderOpenAI {
			scopes = append([]string{}, scopes...)
			if !slices.Contains(scopes, "chatgpt.tokens.use.direct") {
				scopes = append(scopes, "chatgpt.tokens.use.direct")
			}
		}
		return k.engine.Token(ctx, scope, oauth.TokenRequest{Ref: pv.CredentialRefID(ref), Scopes: scopes})
	default:
		return external.ExtResolveKey(ctx, k.provider, k.service, k.scopes, scope, ref)
	}
}

// extFinalCredentialKinds reads the kind of a credential reference in the caller's scope. *credentials.Service implements it.
type extFinalCredentialKinds interface {
	Get(ctx context.Context, scope contracts.Scope, ref pv.CredentialRefID) (pv.CredentialRef, error)
}

// extFinalGemini serves one Gemini profile with the authentication of the credential bound to it: an API key client
// or an OAuth client, chosen per call from the kind of the reference in the scope of the call. The choice is made
// from the credential service, never from the request, and a reference that is not found in the scope, or is of
// another kind, is refused before anything is sent (sent=false, authentication_failed): no other credential, and no
// other client, is tried.
type extFinalGemini struct {
	ExternalProvider                  // the API key client: name, capabilities, health
	oauth            ExternalProvider // the OAuth client
	kinds            extFinalCredentialKinds
	ref              pv.CredentialRefID
}

var _ ExternalProvider = extFinalGemini{}

// extFinalGeminiBoth completes an API key Gemini client with its OAuth twin over the engine. Without an engine or a
// credential service there is nothing to tell the kinds apart with, so the API key client is returned alone.
func extFinalGeminiBoth(spec ExternalProviderSpec, keyClient ExternalProvider, engine extFinalTokenEngine, kinds extFinalCredentialKinds) (ExternalProvider, error) {
	if engine == nil || kinds == nil {
		return keyClient, nil
	}
	spec.Gemini.Auth, spec.GeminiTokens = gemini.AuthOAuth, extFinalEngineTokens{e: engine}
	oc, err := externalProviderBuildGemini(spec)
	if err != nil {
		return nil, err
	}
	return extFinalGemini{ExternalProvider: keyClient, oauth: oc, kinds: kinds, ref: pv.CredentialRefID(spec.Gemini.CredentialRef)}, nil
}

func (g extFinalGemini) pick(ctx context.Context, scope contracts.Scope) (ExternalProvider, error) {
	refuse := func(code contracts.ErrCode) error {
		return contracts.New(code, "gemini: the credential cannot be used for this connection").
			WithDetails("provider_error_kind", string(pv.ProviderErrorKindAuthenticationFailed)).WithDetails("sent", false)
	}
	if g.kinds == nil || g.oauth == nil {
		return nil, refuse(contracts.ErrInternal)
	}
	ref, err := g.kinds.Get(ctx, scope, g.ref)
	if err != nil {
		code := contracts.CodeOf(err)
		if code == contracts.ErrInternal && !contracts.HasCode(err, contracts.ErrInternal) {
			code = contracts.ErrPermissionDenied
		}
		return nil, refuse(code)
	}
	switch ref.Kind {
	case pv.CredentialKindOauth:
		return g.oauth, nil
	case pv.CredentialKindAPIKey:
		return g.ExternalProvider, nil
	}
	return nil, refuse(contracts.ErrPermissionDenied)
}

// Generate implements providers.MultimodalGenerator.
func (g extFinalGemini) Generate(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichResult, error) {
	c, err := g.pick(ctx, scope)
	if err != nil {
		return providers.RichResult{}, err
	}
	return c.Generate(ctx, scope, req)
}

// GenerateStream implements providers.MultimodalGenerator.
func (g extFinalGemini) GenerateStream(ctx context.Context, scope contracts.Scope, req providers.RichRequest) (providers.RichStream, error) {
	c, err := g.pick(ctx, scope)
	if err != nil {
		return nil, err
	}
	return c.GenerateStream(ctx, scope, req)
}

// ---- external voice ----
//
// voiceProviderFactory (cmd_voice.go) is assigned by the LocalAI voice transport (transport_voice_localai.go), which
// links no external package. That file calls voiceLocalAIExternal, a hook this file sets in init, on the providers it
// built, so the order of the two init functions does not matter. With external mode off the hook returns the
// providers untouched. With it on, the speech roles that the deployment routes (routing: stt, tts) are replaced by
// the gates below; the voice activity detector never leaves.
//
//   - Speech to text. extmodal.NewSTT: the finalised local transcript or the local recogniser first; the raw audio
//     goes to an external recogniser only when no local one is installed, the provider and the tenant both permit raw
//     audio, the local VAD cut the utterance, the provider accepts its size, the data-egress guard allows the audio
//     of the class its session states (extAudioEgress: the audio setting of the egress policy, off by default; no
//     session states a class today, so the audio is unclassified and stays) and the provider budget reserves the
//     call by the length of the audio. Streaming recognition is local only.
//   - Text to speech. extmodal.NewTTS: the modality is admitted, the text is a verified response (Attest), the
//     data-egress guard allows the text and the provider budget of the process reserves the call, in that order; a
//     refusal means the provider is never called.
//
// A call is routed externally only in Hybrid or External mode (never under the kill switch), for a role whose policy
// lists a profile the registry admits. A refusal made before anything is sent (modality off, text not verified,
// egress denied, budget refused) is served by the local voice provider only when the role's policy names that
// fallback (egress_denied, or budget_exhausted / rate_limited for the budget); any other failure is returned and
// chooses nothing else.
//
// Attest. A spoken response must be a verified one. The dialogue engine realises most responses itself, and the
// only turns this composition sees are the ones the realiser seam takes (Hybrid, with the realisation role routed):
// for those it attests the outcome the engine will produce, computed with verify.Realise or verify.Degraded exactly
// as the engine does, so unverified model text is never speakable. A response of another turn is not attested, so
// external speech refuses it and the local voice speaks it (when the policy names the fallback). The external speech
// clients themselves come from extFinalVoiceSource (STT and TTS adapters); the modality policy
// (extmodal.PolicySource) defaults off: external voice stays dormant until admitted, which is the safe default.

// extFinalVoiceHolder is the external voice stack of the process, set when external mode is composed.
var extFinalVoiceHolder atomic.Pointer[extFinalVoice]

func init() {
	voiceLocalAIExternal = extFinalWrapVoice
}

// extFinalVoiceKit is the external speech clients of one profile and what the adapter reports for them.
type extFinalVoiceKit struct {
	STT  voice.STT
	TTS  voice.TTS
	Caps providers.ModelCapabilities
}

// extFinalVoiceSource provides the speech clients of a profile. No external adapter provides any yet.
type extFinalVoiceSource interface {
	Kit(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile) (extFinalVoiceKit, bool)
}

type extFinalNoVoice struct{}

func (extFinalNoVoice) Kit(context.Context, contracts.Scope, pv.ProviderProfile) (extFinalVoiceKit, bool) {
	return extFinalVoiceKit{}, false
}

// extFinalVoice is the external voice stack: the modality gate, the mode and routing it follows, and the registry.
type extFinalVoice struct {
	svc      *extmodal.Service
	modes    *modes.Resolver
	policy   modes.PolicySource
	profiles modes.Profiles
	source   extFinalVoiceSource
	log      *slog.Logger
}

// extFinalVoiceSourceHook, when set (providers_external_stt.go), supplies the external speech
// clients for routed STT/TTS profiles. Nil keeps the dormant NoVoice source.
var extFinalVoiceSourceHook func(*externalAdmin) extFinalVoiceSource

func extFinalNewVoice(a *externalAdmin, policy modes.PolicySource, text extmodal.TextEgress, log *slog.Logger) (*extFinalVoice, error) {
	svc, err := extmodal.New(extmodal.Deps{Policy: extmodal.NewMemoryPolicy(), Egress: text, Audio: extAudioEgress{guard: a.egress}, Budgets: a.budget, Clock: a.clk})
	if err != nil {
		return nil, err
	}
	source := extFinalVoiceSource(extFinalNoVoice{})
	if extFinalVoiceSourceHook != nil {
		source = extFinalVoiceSourceHook(a)
	}
	return &extFinalVoice{svc: svc, modes: a.resolver, policy: policy, profiles: a.reg, source: source, log: log}, nil
}

// extFinalWrapVoice is the voiceLocalAIExternal hook: with external mode on, it routes the speech roles the deployment
// names through the gates. The stack is looked up per call (it is composed after the factory may have run).
func extFinalWrapVoice(p voiceprofile.Providers) voiceprofile.Providers {
	if !externalSection.Enabled {
		return p
	}
	routing := externalSection.extWirePolicy()
	if len(routing[modes.RoleSTT].Profiles) > 0 {
		p.STT = extFinalSTT{local: p.STT}
	}
	if len(routing[modes.RoleTTS].Profiles) > 0 {
		p.TTS = extFinalTTS{local: p.TTS}
	}
	return p
}

// extFinalRoute is where one call of a speech role goes.
type extFinalRoute struct {
	target extmodal.Target
	kit    extFinalVoiceKit
	policy modes.RolePolicy
	// external is true when the call is routed to the external provider; false means the local one.
	external bool
}

// route decides where a call of role goes. A mode of Local (or the kill switch), a role with no profile and a stack
// that is not composed are local. A profile the registry refuses is an error, never another provider.
func (v *extFinalVoice) route(ctx context.Context, scope contracts.Scope, role modes.Role) (extFinalRoute, error) {
	if v == nil {
		return extFinalRoute{}, nil
	}
	res, err := v.modes.Resolve(ctx, scope)
	if err != nil {
		return extFinalRoute{}, err
	}
	if res.Mode == modes.ModeLocal {
		return extFinalRoute{}, nil
	}
	rp, err := v.policy.RolePolicy(ctx, scope, role)
	if err != nil || len(rp.Profiles) == 0 {
		return extFinalRoute{}, nil
	}
	rq := providerreg.Requirement{Role: pv.RoleSTT, DataClass: pv.DataClassificationInternal, SpeechToText: true}
	if role == modes.RoleTTS {
		rq = providerreg.Requirement{Role: pv.RoleTTS, DataClass: pv.DataClassificationInternal, TextToSpeech: true}
	}
	out := extFinalRoute{policy: rp}
	for _, id := range rp.Profiles {
		adm, err := v.profiles.Check(ctx, scope, id, rq)
		if err != nil {
			if providerreg.ReasonOf(err) == providerreg.ReasonCapability {
				continue
			}
			return out, err
		}
		kit, ok := v.source.Kit(ctx, scope, adm.Profile)
		if !ok {
			continue
		}
		out.target, out.kit, out.external = extmodal.Target{Profile: adm.Profile, Caps: kit.Caps}, kit, true
		return out, nil
	}
	return out, nil
}

// extFinalLocalAllowed says whether a refusal made before anything was sent may be served locally: only when the
// role's policy names the fallback of that refusal, as the text router decides (a provider budget that refuses or
// cannot reserve the call: budget_exhausted or rate_limited; any other refusal: egress_denied). Nothing sent means
// nothing duplicated.
func extFinalLocalAllowed(rp modes.RolePolicy, err error) bool {
	switch string(extmodal.ReasonOf(err)) {
	case "":
		return false
	case providerbudget.ReasonRateLimited:
		return rp.FallsBackOn(modes.CondRateLimited)
	case providerbudget.ReasonBudgetExhausted, providerbudget.ReasonQuotaExhausted, providerbudget.ReasonConcurrencyLimit,
		providerbudget.ReasonNoBudget, string(extmodal.ReasonNotEstimable):
		return rp.FallsBackOn(modes.CondBudgetExhausted)
	}
	return rp.FallsBackOn(modes.CondEgressDenied)
}

// extFinalSTT lives in providers_external_stt.go (STT adapters + factory).

// extFinalTTS is the text-to-speech of the voice profile with external mode on.
type extFinalTTS struct{ local voice.TTS }

var _ voice.TTS = extFinalTTS{}

// SynthesizeStream implements voice.TTS.
func (t extFinalTTS) SynthesizeStream(ctx context.Context, scope contracts.Scope, req voice.SynthesizeRequest) (voice.AudioStream, error) {
	v := extFinalVoiceHolder.Load()
	rt, err := v.route(ctx, scope, modes.RoleTTS)
	if err != nil {
		return nil, err
	}
	if !rt.external || rt.kit.TTS == nil {
		if t.local == nil {
			return nil, contracts.New(contracts.ErrUnavailable, "voice: no speech synthesiser is installed")
		}
		return t.local.SynthesizeStream(ctx, scope, req)
	}
	st, err := extmodal.NewTTS(v.svc, rt.target, rt.kit.TTS).SynthesizeStream(ctx, scope, req)
	if err != nil && t.local != nil && extFinalLocalAllowed(rt.policy, err) {
		return t.local.SynthesizeStream(ctx, scope, req)
	}
	return st, err
}

// extFinalAttestTurn attests the response of a turn the realiser seam took, as the outcome the engine will produce for
// it (see the section comment). failed is a realiser that returned an error: the engine then shows the template.
func extFinalAttestTurn(in dialogue.ResponseInput, got verify.Outcome, failed bool) {
	v := extFinalVoiceHolder.Load()
	if v == nil || in.Directive == nil {
		return
	}
	var out verify.Outcome
	var err error
	if !failed && got.Source == verify.SourceModel {
		out, err = verify.Realise(got.Text, in.Directive, in.Receipt, in.Options)
	} else {
		out, err = verify.Degraded(in.Directive, in.Receipt, in.Options)
	}
	if err == nil {
		err = v.svc.Attest(in.Scope, out)
	}
	if err != nil {
		v.log.Debug("providers: a response was not attested for external speech", "reason", string(contracts.CodeOf(err)))
	}
}

// extFinalOAuthRegistry is the destination registry of the OAuth engine (the token and revocation endpoints of the
// providers), and extFinalProviderRegistry the one of the provider adapters. Both carry the platform deny-list of
// every dialer of the process (PT-001): an allowlist entry can never reach the gateway, the admin interface, the
// metrics listener or the LocalAI runtime.
func extFinalOAuthRegistry() *egress.Registry { return egress.NewRegistry(platformDenyOption()) }

func extFinalProviderRegistry() *egress.ProviderRegistry {
	return egress.NewProviderRegistry(platformDenyOption())
}
