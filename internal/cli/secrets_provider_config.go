package cli

import (
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Application-package credential_ref values (cred:slug) for OpenAPI, MCP and RAG
// api calls into the host. The default build opens the credential service
// (internal/credentials) independently of external_providers. The noexternal
// build leaves the provider unset (a declared credential_ref fails closed at adapter build).
//
//	application_credentials:
//	  credentials:
//	    - ref: cred:help-api
//	      application: help-assistant
//	      tenant: acme
//	      env: ALIXAI_SECRET_HELP_API

const (
	appCredSectionName = "application_credentials"
	appCredServeName   = "application_credentials"
	appCredEnvPrefix   = "ALIXAI_SECRET_"
)

var appCredRefRE = regexp.MustCompile(`^cred:[a-z][a-z0-9_.-]{2,63}$`)

// looksLikeLiteralKey refuses config fields that look like pasted API keys
// (shared by application_credentials and external_providers credential bindings).
func looksLikeLiteralKey(s string) bool {
	if len(s) < 12 {
		return false
	}
	for _, p := range []string{"sk-", "sk-or-", "sk-ant-", "xai-", "AIza"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// appCredConfig is the application_credentials config section.
type appCredConfig struct {
	Credentials []appCredBinding `yaml:"credentials"`
}

// Validate implements config.Section.
func (c appCredConfig) Validate() error {
	seen := map[string]bool{}
	for _, b := range c.Credentials {
		if err := b.Validate(); err != nil {
			return err
		}
		if seen[b.Ref] {
			return contracts.New(contracts.ErrValidationFailed, "application_credentials: duplicate ref "+b.Ref)
		}
		seen[b.Ref] = true
	}
	return nil
}

// appCredBinding provisions one package credential_ref.
type appCredBinding struct {
	Ref         string `yaml:"ref"`
	Application string `yaml:"application"`
	Tenant      string `yaml:"tenant"`
	Env         string `yaml:"env"`
	File        string `yaml:"file"`
	Label       string `yaml:"label"`
}

func (c appCredBinding) Validate() error {
	if !appCredRefRE.MatchString(c.Ref) {
		return contracts.New(contracts.ErrValidationFailed, "application_credentials: ref must be a package credential_ref (cred:…)")
	}
	if c.Application == "" || c.Tenant == "" {
		return contracts.New(contracts.ErrValidationFailed, "application_credentials: application and tenant are required")
	}
	if (c.Env == "") == (c.File == "") {
		return contracts.New(contracts.ErrValidationFailed, "application_credentials: name exactly one of env or file")
	}
	if c.Env != "" {
		if !strings.HasPrefix(c.Env, appCredEnvPrefix) {
			return contracts.New(contracts.ErrValidationFailed, "application_credentials: env must use prefix "+appCredEnvPrefix)
		}
		if looksLikeLiteralKey(c.Env) {
			return contracts.New(contracts.ErrValidationFailed, "application_credentials: env names a variable, not a literal key")
		}
	}
	if c.File != "" && (strings.ContainsAny(c.File, `/\`) || looksLikeLiteralKey(c.File)) {
		return contracts.New(contracts.ErrValidationFailed, "application_credentials: file is a name in the secret directory, not a path or literal key")
	}
	if looksLikeLiteralKey(c.Label) {
		return contracts.New(contracts.ErrValidationFailed, "application_credentials: label must not look like a literal key")
	}
	return nil
}

var appCredSection appCredConfig

func init() {
	config.Register(appCredSectionName, &appCredSection)
}
