package m1

import (
	"context"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// rigRAGSources stands in for the RAG source registry the M1 rig does not run. The gateway refuses to start with a
// route area half wired (v1_rag needs RAGSources next to the ApplicationService the rig provides), and M1 does not
// exercise RAG: every call answers unavailable, which is what a deployment with no RAG answers.
type rigRAGSources struct{}

var _ gateway.RAGSources = rigRAGSources{}

func rigRAGUnavailable() error {
	return contracts.New(contracts.ErrUnavailable, "the M1 rig runs no RAG")
}

func (rigRAGSources) Register(context.Context, contracts.Scope, gateway.RAGSourcesRegistration) (gateway.RAGSourcesRecord, error) {
	return gateway.RAGSourcesRecord{}, rigRAGUnavailable()
}

func (rigRAGSources) Reaudit(context.Context, contracts.Scope) (gateway.RAGSourcesReaudit, error) {
	return gateway.RAGSourcesReaudit{}, rigRAGUnavailable()
}

func (rigRAGSources) Remove(context.Context, contracts.Scope, string) error {
	return rigRAGUnavailable()
}

func (rigRAGSources) Status(context.Context, contracts.Scope, string) ([]gateway.RAGSourcesRecord, error) {
	return nil, rigRAGUnavailable()
}
