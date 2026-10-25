package gateway

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The RAG source registry as the /v1 API sees it (master plan 14.2, "register,
// update or remove a source", "inspect source and index status"; 10.2).
//
// routes_v1_rag.go (M2-11) takes this interface from the service registry
// (Need[RAGSources]) and never imports internal/rag: the composition root
// (internal/cli, cmd_rag_sources.go) implements it over the durable registry and
// the ingestion service, wraps it in RAGSourcesScoped and provides the result.
//
// # What a caller can say
//
// A source is registered by the package-level name its package declares it
// under, and by nothing else: the root, rules, limits and deletion behaviour
// come from the signed, audited, active package; the tenant and workspace it
// serves come from the verified scope, never from a body or a path. A client
// cannot name a folder, another tenant's workspace or a declaration the package
// does not make. Re-registering a declaration is the same registration, not a
// second source.
//
// # Who may call
//
// The interface does not say who may register, re-audit or remove a source: it
// carries the verified scope of the caller and the implementation decides, from
// the deployment's own configuration of operators (rag.operators in internal/
// cli), never from a role the host application or a package asserts. A route
// maps the error codes: permission_denied is 403, quarantined (no active
// package) 403 like every other area, validation_failed with details.reason
// not_found 404, and the rest by the gateway's mapping.

// RAGSources is the part of the RAG source registry a tenant-facing request may
// reach. Every method takes the verified scope and works inside it: a source of
// another application, tenant or workspace is reported exactly like one that
// does not exist.
type RAGSources interface {
	// Register registers the source the application's active package declares
	// under in.PackageSourceID, for the scope's tenant and workspace, and has
	// ingestion compare it with what was last scanned (queueing a sync when it is
	// new or its rules changed). It is idempotent per scope and declaration: it
	// returns the existing registration, re-bound to the active package. An
	// application with no active package is contracts.ErrQuarantined; a name the
	// package does not declare is contracts.ErrValidationFailed.
	Register(ctx context.Context, scope contracts.Scope, in RAGSourcesRegistration) (RAGSourcesRecord, error)

	// Reaudit re-binds the sources registered in the scope to the active package
	// after it was replaced or re-audited: each is rebuilt from the package and
	// ingestion rescans it when its rules changed. A source the package no longer
	// declares is removed, with its declared deletion behaviour. It touches this
	// scope's registrations only; the process re-binds the other scopes itself.
	Reaudit(ctx context.Context, scope contracts.Scope) (RAGSourcesReaudit, error)

	// Remove removes a source of the scope: it stops being served at once and
	// ingestion applies its declared deletion behaviour (audited). If that fails
	// the source stays unserved and the call can be repeated.
	Remove(ctx context.Context, scope contracts.Scope, sourceID string) error

	// Status reports the source with that id in the scope, or, with an empty
	// sourceID, every source registered in the scope ordered by id. A source id
	// the scope does not hold is validation_failed with details.reason not_found.
	Status(ctx context.Context, scope contracts.Scope, sourceID string) ([]RAGSourcesRecord, error)
}

// RAGSourcesRegistration is what a caller may say when it registers a source.
type RAGSourcesRegistration struct {
	// PackageSourceID is the name the active package declares the source under.
	PackageSourceID string
}

// RAGSourcesRecord is one registered source as the API reads it.
type RAGSourcesRecord struct {
	// ApplicationID, TenantID and WorkspaceID are the scope the source was
	// registered for: the three levels every record carries so that a result can
	// be checked against the caller's scope (RAGSourcesScoped does).
	ApplicationID string
	TenantID      string
	WorkspaceID   string
	// PackageSourceID and PackageVersion say which declaration of which package
	// version the registration was last bound to.
	PackageSourceID string
	PackageVersion  string
	// Serving is true while the source is bound to the application's active
	// package and may be read. A source registered under a package that has been
	// replaced is false until it is re-bound (Reaudit, or the process's own
	// re-bind), and so is one whose removal began: nothing is claimed as served
	// that is not.
	Serving bool
	// Status is the status of the source (schema rag/source-status).
	Status rag.SourceStatus
}

// RAGSourcesReaudit counts what a Reaudit did.
type RAGSourcesReaudit struct {
	// Checked registrations of the scope, Rebound to the active package, Queued
	// of those whose rules changed and a sync was queued, Removed because the
	// package no longer declares them, Failed that could not be rebuilt or
	// checked (they stay unserved and the process tries again).
	Checked, Rebound, Queued, Removed, Failed int
}

// ragSourcesMaxName bounds a declaration name: the package schema's slug is far
// shorter.
const ragSourcesMaxName = 128

// RAGSourcesScoped wraps a RAGSources so that the scope is checked in the
// gateway, whatever the implementation does, and what it returns is checked
// against the scope again:
//
//   - a call with an incomplete or malformed scope (a missing level, a
//     wildcard) is refused before it reaches the implementation, with the
//     scope's own code, and the implementation is only ever given the exact
//     scope the middleware verified;
//   - a declaration name or a source id that is not well formed is refused
//     before it reaches the implementation: validation_failed, never a query;
//   - a record the implementation returns for another application, tenant or
//     workspace is never passed on. A listing drops it, a single source is
//     validation_failed with details.reason not_found (the same answer as for a
//     source that does not exist), and a registration that answers with one is
//     an internal error: the caller never sees a source that is not its own.
//
// It returns nil for a nil implementation (Provide refuses it).
func RAGSourcesScoped(inner RAGSources) RAGSources {
	if inner == nil {
		return nil
	}
	return ragSourcesScoped{inner: inner}
}

type ragSourcesScoped struct{ inner RAGSources }

var _ RAGSources = ragSourcesScoped{}

func (g ragSourcesScoped) Register(ctx context.Context, scope contracts.Scope, in RAGSourcesRegistration) (RAGSourcesRecord, error) {
	if err := scope.Validate(); err != nil {
		return RAGSourcesRecord{}, err
	}
	if !ragSourcesNameOK(in.PackageSourceID) {
		return RAGSourcesRecord{}, contracts.New(contracts.ErrValidationFailed, "the source name is not well formed")
	}
	rec, err := g.inner.Register(ctx, scope, in)
	if err != nil {
		return RAGSourcesRecord{}, err
	}
	if !ragSourcesOwns(scope, rec) {
		return RAGSourcesRecord{}, contracts.New(contracts.ErrInternal, "the source registry answered for another scope")
	}
	return rec, nil
}

func (g ragSourcesScoped) Reaudit(ctx context.Context, scope contracts.Scope) (RAGSourcesReaudit, error) {
	if err := scope.Validate(); err != nil {
		return RAGSourcesReaudit{}, err
	}
	return g.inner.Reaudit(ctx, scope)
}

func (g ragSourcesScoped) Remove(ctx context.Context, scope contracts.Scope, sourceID string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !ids.Valid(sourceID, ragSourcesIDPrefix) {
		return contracts.New(contracts.ErrValidationFailed, "the source id is malformed")
	}
	return g.inner.Remove(ctx, scope, sourceID)
}

func (g ragSourcesScoped) Status(ctx context.Context, scope contracts.Scope, sourceID string) ([]RAGSourcesRecord, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if sourceID != "" && !ids.Valid(sourceID, ragSourcesIDPrefix) {
		return nil, contracts.New(contracts.ErrValidationFailed, "the source id is malformed")
	}
	recs, err := g.inner.Status(ctx, scope, sourceID)
	if err != nil {
		return nil, err
	}
	own := make([]RAGSourcesRecord, 0, len(recs))
	for _, rec := range recs {
		if !ragSourcesOwns(scope, rec) {
			continue
		}
		if sourceID != "" && string(rec.Status.SourceID) != sourceID {
			continue
		}
		own = append(own, rec)
	}
	if sourceID != "" && len(own) == 0 {
		return nil, ragSourcesNotFound()
	}
	return own, nil
}

// ragSourcesIDPrefix is the prefix of a registered source's id (src_<ULID>).
const ragSourcesIDPrefix = "src"

func ragSourcesNotFound() error {
	return contracts.New(contracts.ErrValidationFailed, "no such source").WithDetails("reason", "not_found")
}

// ragSourcesOwns reports whether the record is of the scope's application,
// tenant and workspace.
func ragSourcesOwns(scope contracts.Scope, rec RAGSourcesRecord) bool {
	return rec.ApplicationID == scope.ApplicationID && rec.TenantID == scope.TenantID && rec.WorkspaceID == scope.WorkspaceID
}

// ragSourcesNameOK reports whether s can be a declaration name: non-empty, short,
// valid UTF-8, no padding and no control character.
func ragSourcesNameOK(s string) bool {
	if s == "" || len(s) > ragSourcesMaxName || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
