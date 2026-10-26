package cli

import (
	"context"
	"log/slog"
	"strings"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/retention"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func init() {
	// Name sorts after providers_external so Authorize can use ProvidersService when wired.
	registerServeProvider("zz_ops_erasure", opsErasureProvide)
}

// opsErasureProvide wires ErasureService over the serve DB.
func opsErasureProvide(_ context.Context, d serveDeps) error {
	if d.DB == nil || d.Audit == nil || d.Services == nil {
		return nil
	}
	eng := &retention.Engine{
		DB: d.DB, Log: d.Audit, Clock: d.Clock, Logg: slog.New(slog.DiscardHandler),
		PrincipalLLM: func(ctx context.Context, application, principal string) (int, int, error) {
			return retentionErasePrincipalLLM(ctx, &retentionEngine{db: d.DB, clk: d.Clock}, application, principal)
		},
	}
	svc := &opsErasureService{jobs: retention.NewJobs(eng), services: d.Services}
	return gateway.Provide[gateway.ErasureService](d.Services, svc)
}

type opsErasureService struct {
	jobs     *retention.Jobs
	services *gateway.Services
}

func (s *opsErasureService) providers() gateway.ProvidersService {
	if s == nil || s.services == nil {
		return nil
	}
	p, _ := gateway.Lookup[gateway.ProvidersService](s.services)
	return p
}

func (s *opsErasureService) Authorize(ctx context.Context, scope contracts.Scope) error {
	if p := s.providers(); p != nil {
		return p.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorTenant)
	}
	return contracts.New(contracts.ErrPermissionDenied, "erasure is for deployment operators and tenant administrators")
}

func (s *opsErasureService) Start(ctx context.Context, scope contracts.Scope, in gateway.ErasureRequest) (gateway.ErasureJobAccepted, error) {
	req, err := opsErasureRequest(ctx, scope, in, s.providers())
	if err != nil {
		return gateway.ErasureJobAccepted{}, err
	}
	job, _, err := s.jobs.Start(ctx, req)
	if err != nil {
		return gateway.ErasureJobAccepted{}, err
	}
	return gateway.ErasureJobAccepted{JobID: job.JobID, Status: string(job.Status)}, nil
}

func (s *opsErasureService) Status(ctx context.Context, scope contracts.Scope, jobID string) (gateway.ErasureJobStatus, error) {
	job, err := s.jobs.Get(ctx, jobID)
	if err != nil {
		return gateway.ErasureJobStatus{}, err
	}
	if job.Application != scope.ApplicationID {
		return gateway.ErasureJobStatus{}, contracts.New(contracts.ErrValidationFailed, "unknown erasure job").
			WithDetails("reason", "not_found")
	}
	// Tenant admins only see jobs for their own tenant; deployment operators see every job of the application.
	// Application-wide jobs have empty TenantID: tenant admins must not see them.
	if p := s.providers(); p != nil && p.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment) != nil {
		if job.Scope == retention.PrincipalScopeApplication || job.TenantID != scope.TenantID {
			return gateway.ErasureJobStatus{}, contracts.New(contracts.ErrValidationFailed, "unknown erasure job").
				WithDetails("reason", "not_found")
		}
	}
	return opsErasureStatus(job), nil
}

// opsErasureAuthorizer is the AuthorizeOperator half of ProvidersService (tests stub only this).
type opsErasureAuthorizer interface {
	AuthorizeOperator(ctx context.Context, scope contracts.Scope, level gateway.ProvidersOperatorLevel) error
}

func opsErasureRequest(ctx context.Context, scope contracts.Scope, in gateway.ErasureRequest, providers opsErasureAuthorizer) (retention.Request, error) {
	tenant := in.TenantID
	if tenant == "" {
		tenant = scope.TenantID
	}
	scopeKind := retention.PrincipalScope(in.Scope)
	if in.Level == gateway.ErasureLevelPrincipal {
		switch in.Scope {
		case "", gateway.ErasureScopeTenant:
			scopeKind = retention.PrincipalScopeTenant
		case gateway.ErasureScopeApplication:
			scopeKind = retention.PrincipalScopeApplication
		default:
			return retention.Request{}, contracts.New(contracts.ErrValidationFailed, "scope must be tenant or application when level is principal")
		}
	} else if in.Scope != "" {
		return retention.Request{}, contracts.New(contracts.ErrValidationFailed, "scope applies only when level is principal")
	}

	isDeployment := providers != nil && providers.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment) == nil
	if scopeKind == retention.PrincipalScopeApplication {
		if !isDeployment {
			return retention.Request{}, contracts.New(contracts.ErrPermissionDenied,
				"application-wide principal erasure is for deployment operators only")
		}
		// Application-wide: tenant_id is not a filter; leave empty on the job.
		tenant = ""
	} else if tenant != scope.TenantID {
		if !isDeployment {
			return retention.Request{}, contracts.New(contracts.ErrPermissionDenied, "a tenant administrator may only erase their own tenant")
		}
	}

	req := retention.Request{
		ApplicationID: scope.ApplicationID,
		TenantID:      tenant,
		WorkspaceID:   in.WorkspaceID,
		PrincipalID:   in.PrincipalID,
		Level:         retention.Level(in.Level),
		Scope:         scopeKind,
		Reason:        strings.TrimSpace(in.Reason),
		Operator:      scope.PrincipalID,
	}
	if req.Reason == "" {
		req.Reason = "erasure request"
	}
	if req.Level == retention.LevelWorkspace && req.WorkspaceID == "" {
		req.WorkspaceID = scope.WorkspaceID
	}
	if req.Level == retention.LevelPrincipal && req.Scope == "" {
		req.Scope = retention.PrincipalScopeTenant
	}
	return req, req.Validate()
}

func opsErasureStatus(job *retention.Job) gateway.ErasureJobStatus {
	out := gateway.ErasureJobStatus{
		JobID: job.JobID, Status: string(job.Status), Level: gateway.ErasureLevel(job.Level),
		Scope: gateway.ErasureScope(job.Scope), TenantID: job.TenantID, WorkspaceID: job.WorkspaceID,
		PrincipalID: job.PrincipalID, Error: job.Error, FinishedAt: job.FinishedAt,
	}
	if job.Status == retention.JobCompleted || job.Status == retention.JobFailed || job.Status == retention.JobRunning {
		c := job.Counts
		out.Counts = &gateway.ErasureCounts{
			AuditEventsErased: c.AuditEventsErased, PreparationsRemoved: c.PreparationsRemoved,
			ReceiptsRemoved: c.ReceiptsRemoved, TranscriptTurnsRemoved: c.TranscriptTurnsRemoved,
			SessionTurnsRemoved: c.SessionTurnsRemoved, SessionsRemoved: c.SessionsRemoved,
			SessionsEnded: c.SessionsEnded, MemoryItemsRemoved: c.MemoryItemsRemoved,
			FilesRemoved: c.FilesRemoved, PrincipalCredentialsRevoked: c.PrincipalCredentialsRevoked,
			PrincipalBindingsRemoved: c.PrincipalBindingsRemoved, More: c.More,
		}
	}
	return out
}

var _ gateway.ErasureService = (*opsErasureService)(nil)
