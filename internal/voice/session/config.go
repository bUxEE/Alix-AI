package session

import (
	"context"
	"slices"

	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ConfigSection is the configuration section this package owns:
//
//	voice_session:
//	  retain_raw_audio: false
//	  retain_tenants: []
//	  discard_tenants: []
//	  max_sessions: 4                 # voice connections the process serves at once
//	  max_sessions_per_tenant: 2      # one tenant's share of them
//	  max_sessions_per_principal: 1   # one user's share of the tenant's
//
// (the section "voice" belongs to internal/voice/profile). With none of it set
// no tenant's raw audio is kept, which is the default of master plan 13.
const ConfigSection = "voice_session"

// Config is the voice_session section. The zero value is the default: raw
// audio is never retained.
type Config struct {
	// RetainRawAudio is the deployment-wide default for a tenant that is in
	// neither list below. False unless the operator turns it on.
	RetainRawAudio bool `yaml:"retain_raw_audio"`
	// RetainTenants are the tenants whose raw audio is retained whatever the
	// default says.
	RetainTenants []string `yaml:"retain_tenants"`
	// DiscardTenants are the tenants whose raw audio is never retained,
	// whatever the default and RetainTenants say: a tenant that asked for no
	// retention keeps it however the deployment is configured.
	DiscardTenants []string `yaml:"discard_tenants"`
	// MaxSessions, MaxSessionsPerTenant and MaxSessionsPerPrincipal are
	// Deps.MaxSessions, Deps.MaxSessionsPerTenant and
	// Deps.MaxSessionsPerPrincipal; 0 takes the default.
	MaxSessions             int `yaml:"max_sessions"`
	MaxSessionsPerTenant    int `yaml:"max_sessions_per_tenant"`
	MaxSessionsPerPrincipal int `yaml:"max_sessions_per_principal"`
}

// Validate refuses a negative cap, which would otherwise read as the default.
func (c Config) Validate() error {
	if c.MaxSessions < 0 || c.MaxSessionsPerTenant < 0 || c.MaxSessionsPerPrincipal < 0 {
		return contracts.New(contracts.ErrValidationFailed, "voice_session: max_sessions, max_sessions_per_tenant and max_sessions_per_principal are counts (0 takes the default)")
	}
	return nil
}

// registered is the section registered with the configuration loader. Owners
// read it through this pointer (config.Load replaces the fields wholesale).
var registered = &Config{}

func init() { config.Register(ConfigSection, registered) }

// CurrentConfig returns the voice_session section as of the last successful
// config.Load (the defaults before any).
func CurrentConfig() Config {
	c := *registered
	c.RetainTenants = slices.Clone(c.RetainTenants)
	c.DiscardTenants = slices.Clone(c.DiscardTenants)
	return c
}

// retains reports whether the tenant's raw audio is retained under c.
func (c Config) retains(tenant string) bool {
	switch {
	case slices.Contains(c.DiscardTenants, tenant):
		return false
	case slices.Contains(c.RetainTenants, tenant):
		return true
	}
	return c.RetainRawAudio
}

// AudioPolicy says, per tenant, whether the raw audio of an utterance is
// retained (master plan 13: "raw audio is not retained by default"). It is read
// when a voice session opens and fails closed: an error refuses the session.
type AudioPolicy interface {
	RetainRawAudio(ctx context.Context, scope contracts.Scope) (bool, error)
}

// AudioPolicyFunc adapts a function to AudioPolicy.
type AudioPolicyFunc func(ctx context.Context, scope contracts.Scope) (bool, error)

// RetainRawAudio implements AudioPolicy.
func (f AudioPolicyFunc) RetainRawAudio(ctx context.Context, scope contracts.Scope) (bool, error) {
	return f(ctx, scope)
}

// ConfigPolicy is the AudioPolicy of the voice_session configuration section,
// read afresh on every call. It is the default when Deps.Policy is nil.
type ConfigPolicy struct{}

// RetainRawAudio implements AudioPolicy.
func (ConfigPolicy) RetainRawAudio(_ context.Context, scope contracts.Scope) (bool, error) {
	return CurrentConfig().retains(scope.TenantID), nil
}
