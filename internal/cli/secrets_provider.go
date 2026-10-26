//go:build !noexternal

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	openapitransport "github.com/buxee/alix-ai/adapters/openapi"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const (
	appCredProviderID = pv.ProviderID("host")
	appCredPurpose    = "adapter_call"
)

func init() {
	registerServeProvider(appCredServeName, appCredServeProvide)
}

// appCredAcceptKeys accepts host adapter secrets: they are not probed against an
// LLM provider.
type appCredAcceptKeys struct{}

func (appCredAcceptKeys) ValidateKey(_ context.Context, req credentials.ValidationRequest) (credentials.ValidationResult, error) {
	if req.ProviderID != appCredProviderID {
		return credentials.ValidationResult{}, contracts.New(contracts.ErrUnavailable, "no key validator exists for this provider")
	}
	return credentials.ValidationResult{Accepted: true}, nil
}

// appCredBound is one package ref mapped to a stored credential.
type appCredBound struct {
	scope contracts.Scope
	refID string
}

// appCredProvider resolves package credential_ref values through the credential
// service. Safe for concurrent use; re-reads on every Secret call.
type appCredProvider struct {
	svc *credentials.Service
	mu  sync.RWMutex
	by  map[string]appCredBound // package cred:slug → binding
	log *slog.Logger
}

var _ openapitransport.SecretProvider = (*appCredProvider)(nil)

// Secret implements openapitransport.SecretProvider.
func (p *appCredProvider) Secret(ctx context.Context, ref string) (openapitransport.Secret, error) {
	if p == nil || p.svc == nil {
		return "", openapitransport.ErrNoSecret
	}
	p.mu.RLock()
	b, ok := p.by[ref]
	p.mu.RUnlock()
	if !ok {
		return "", openapitransport.ErrNoSecret
	}
	sec, err := p.svc.Resolve(ctx, b.scope, b.refID, credentials.Use{
		Provider: appCredProviderID, Purpose: appCredPurpose,
	})
	if err != nil {
		if p.log != nil {
			p.log.Warn("an application credential could not be resolved",
				slog.String("application_id", b.scope.ApplicationID),
				slog.String("reason", string(credentials.ReasonOf(err))))
		}
		return "", err
	}
	return openapitransport.Secret(sec.Reveal()), nil
}

func (p *appCredProvider) bind(ref string, b appCredBound) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.by == nil {
		p.by = map[string]appCredBound{}
	}
	p.by[ref] = b
}

// appCredServeProvide opens the credential service (independent of External mode),
// binds configured package refs, and registers the OpenAPI/MCP/RAG secret provider.
// With no bindings the provider stays unset: adapters without credential_ref are
// unchanged; adapters that declare one fail closed at build (same rule as OpenAPI).
func appCredServeProvide(ctx context.Context, d serveDeps) error {
	if len(appCredSection.Credentials) == 0 {
		return nil
	}
	if d.DB == nil || d.Audit == nil || d.DataDir == "" {
		return errors.New("application_credentials: the store, the audit log and the data directory are required")
	}
	deployment := identity.CurrentConfig().DeploymentID
	if deployment == "" {
		return errors.New("application_credentials: identity.deployment_id is not configured")
	}
	p, err := openBoundAppCredProvider(ctx, appCredServiceDeps{
		DB: d.DB, Audit: d.Audit, Clock: d.Clock, Log: d.Log,
		DeploymentID: deployment, Dir: filepath.Join(d.DataDir, "application_credentials"),
	})
	if err != nil {
		return err
	}
	RegisterOpenAPISecretProvider(p)
	if d.AddService != nil {
		d.AddService(func(context.Context) (stop func()) {
			return func() { RegisterOpenAPISecretProvider(nil) }
		})
	}
	return nil
}

// openBoundAppCredProvider builds the secret provider from application_credentials
// (serve and audit share this so both fail closed the same way).
func openBoundAppCredProvider(ctx context.Context, d appCredServiceDeps) (*appCredProvider, error) {
	svc, err := openAppCredService(d)
	if err != nil {
		return nil, err
	}
	p := &appCredProvider{svc: svc, by: map[string]appCredBound{}, log: d.Log}
	for _, b := range appCredSection.Credentials {
		if err := b.Validate(); err != nil {
			return nil, err
		}
		if err := p.registerBinding(ctx, d.DeploymentID, b); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// stackWireApplicationCredentials registers application_credentials for audit
// the same way serve does.
func stackWireApplicationCredentials(ctx context.Context, s *stack) error {
	if len(appCredSection.Credentials) == 0 {
		return nil
	}
	deployment := identity.CurrentConfig().DeploymentID
	if deployment == "" {
		return errors.New("application_credentials: identity.deployment_id is not configured")
	}
	p, err := openBoundAppCredProvider(ctx, appCredServiceDeps{
		DB: s.db, Audit: s.log, Clock: s.clk, DeploymentID: deployment,
		Dir: filepath.Join(s.dataDir, "application_credentials"),
	})
	if err != nil {
		return err
	}
	RegisterOpenAPISecretProvider(p)
	s.clearSecrets = func() { RegisterOpenAPISecretProvider(nil) }
	return nil
}

func (p *appCredProvider) registerBinding(ctx context.Context, deployment string, b appCredBinding) error {
	scope := contracts.Scope{
		DeploymentID: deployment, ApplicationID: b.Application, TenantID: b.Tenant,
		WorkspaceID: b.Tenant, PrincipalID: "application-credential", Roles: []string{"operator"},
		CapabilityScopes: []string{"credentials.bind"}, SessionID: "application-credential",
	}
	label := b.Label
	if label == "" {
		label = b.Ref
	}
	if existing, err := p.svc.List(ctx, scope, appCredProviderID); err == nil {
		for _, e := range existing {
			if e.Label != nil && *e.Label == label && e.Status == pv.CredentialRefStatusActive {
				p.bind(b.Ref, appCredBound{scope: scope, refID: string(e.RefID)})
				return nil
			}
		}
	}
	backend := credentials.BackendEnv
	locator := b.Env
	if b.File != "" {
		backend, locator = credentials.BackendFile, b.File
	}
	got, err := p.svc.AddExternalRef(ctx, scope, credentials.ExternalRefRequest{
		ProviderID: appCredProviderID, Label: label, Backend: backend, Locator: locator,
	})
	if err != nil {
		return fmt.Errorf("application_credentials %s: %w", b.Ref, err)
	}
	p.bind(b.Ref, appCredBound{scope: scope, refID: string(got.RefID)})
	return nil
}

type appCredServiceDeps struct {
	DB           *store.DB
	Audit        audit.Appender
	Clock        clock.Clock
	Log          *slog.Logger
	DeploymentID string
	Dir          string
	Validator    credentials.KeyValidator
}

// openAppCredService builds a credential service with encrypted-file, env and
// file backends — the same set external_providers uses — under Dir.
func openAppCredService(d appCredServiceDeps) (*credentials.Service, error) {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("application_credentials: %w", err)
	}
	if err := fsperm.Restrict(d.Dir, true); err != nil && runtime.GOOS != "windows" {
		return nil, fmt.Errorf("application_credentials: %w", err)
	}
	secrets := filepath.Join(d.Dir, "secrets")
	keys := credentials.KeyFileSource{Path: filepath.Join(secrets, "master.key")}
	file, err := credentials.NewEncryptedFile(filepath.Join(secrets, "credentials.enc"), keys)
	if err != nil {
		return nil, err
	}
	val := d.Validator
	if val == nil {
		val = appCredAcceptKeys{}
	}
	envBackend := credentials.NewEnvBackend("")
	fileBackend, err := credentials.NewFileBackend(secrets)
	if err != nil {
		return nil, err
	}
	return credentials.New(credentials.Config{
		DB: d.DB, Audit: d.Audit, DeploymentID: d.DeploymentID,
		Backends: []credentials.Backend{file, envBackend, fileBackend},
		Default:  credentials.BackendEncryptedFile, Validator: val, Clock: d.Clock, Logger: d.Log,
	})
}
