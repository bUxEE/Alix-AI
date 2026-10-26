package cli

import (
	"context"

	"github.com/buxee/alix-ai/internal/exports"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// exportsProvideName sorts after turnevents, files and rag_history so the hub,
// file store and history service exist.
const exportsProvideName = "turnevents_exports"

func init() { registerServeProvider(exportsProvideName, exportsProvide) }

func exportsProvide(_ context.Context, d serveDeps) error {
	hub, ok := gateway.Lookup[*stream.Hub](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "exports: the stream hub is required")
	}
	fs, ok := gateway.Lookup[gateway.FilesService](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "exports: the files service is required")
	}
	putter, ok := fs.(exportsFileStore)
	if !ok {
		return contracts.New(contracts.ErrInternal, "exports: the files service cannot store generated files")
	}
	hist, ok := gateway.Lookup[*history.Service](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "exports: the history service is required")
	}
	svc, err := exports.New(exports.Options{
		Source: exports.MultiSource{
			exports.HistorySource{Store: hist},
			exports.HubSource{Hub: hub},
		},
		Files:  putter,
		Audit:  d.Audit,
		Clock:  d.Clock,
		Config: exports.ConfigSection(),
		Log:    d.Log,
	})
	if err != nil {
		return err
	}
	return gateway.Provide[gateway.ExportsService](d.Services, exportsGateway{svc: svc})
}

type exportsFileStore interface {
	PutGenerated(ctx context.Context, scope contracts.Scope, in files.GeneratedInput) (conversation.FileRef, error)
}

type exportsGateway struct {
	svc *exports.Service
}

func (g exportsGateway) Create(ctx context.Context, scope contracts.Scope, sessionID string, req exports.Request) (conversation.FileRef, error) {
	return g.svc.Create(ctx, scope, sessionID, req)
}
