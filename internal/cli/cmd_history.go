package cli

import (
	"context"
	"log/slog"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The history area of the public API (M3-08). Scheduled retention runs here too (M3-21,
// cmd_history_retention.go): the area is built after the RAG stack, because the retention job runs
// on the stack's queue and pool, and before the settings area, which stores its retention preference
// in this service.
func init() { registerServeProvider(historyRetentionProviderName, historyProvide) }

func historyProvide(_ context.Context, d serveDeps) error {
	sess, ok := gateway.Lookup[*sessions.Store](d.Services)
	if !ok {
		return contracts.New(contracts.ErrInternal, "cli: the history service needs the session store, which was not provided")
	}
	deps := history.Deps{
		DB: d.DB, Transcripts: sess, Audit: d.Audit, Clock: d.Clock, Logger: d.Log,
		Operator: historyRetentionOperator, Config: historyRetentionConfig,
		// A search index the upgrade isolated answers "unavailable", never an empty page.
		Quarantine: quarantineHistoryOf(quarantineOf(d.DataDir, d.Clock)),
	}
	// The job queue is the RAG stack's, and the job carries the deployment's id. Without either the
	// retention job is not registered and is not scheduled, which is said, never silent.
	st, haveStack := gateway.Lookup[*ragStack](d.Services)
	dep := identity.CurrentConfig().DeploymentID
	switch {
	case haveStack && dep != "":
		deps.Queue, deps.DeploymentID = st.queue, dep
	case d.Log != nil:
		d.Log.Warn("history: retention is applied only when the retention job is scheduled, and it is not: serve has no job queue or no identity.deployment_id",
			slog.Bool("job_queue", haveStack), slog.Bool("deployment_id", dep != ""))
	}
	svc, err := history.New(deps)
	if err != nil {
		return err
	}
	if deps.Queue != nil {
		if err := historyRetentionShare(d, st, svc, historyRetentionEvery(deps.Config)); err != nil {
			return err
		}
	}
	if historyRetentionOnCompose != nil {
		historyRetentionOnCompose(svc)
	}
	if err := gateway.Provide(d.Services, svc); err != nil {
		return err
	}
	return gateway.Provide[gateway.TurnBlocksArchive](d.Services, svc)
}
