package cli

import (
	"context"
	"log/slog"

	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
)

// The files area of the public API (M3-06, M3-20): the attachment store behind /v1/files and
// behind the attachments of a turn. It is built from what every provider is given (the store,
// the clock, the data directory) and provided to the gateway as the tenant-facing interfaces
// only (gateway.FilesService for the files routes, gateway.AttachModelFiles for the turn);
// the tenant deletion and the compaction sweeps stay here, in the composition root.
//
// The seams of the files module are filled in cmd_files_seams.go: the attachment policy (the
// "files" section, with the size held to the deployment's settings), the RAG ingestion, the
// vision gate and the compaction, which is a service job of serve, not a goroutine of this
// function.
func init() { registerServeProvider("files", provideFiles) }

func provideFiles(ctx context.Context, d serveDeps) error {
	svc, err := files.New(files.Options{
		DB: d.DB, Clock: d.Clock, DataDir: d.DataDir, Log: d.Log,
		Policy: attachFilesPolicy(d),
		RAG:    attachRAGSources{},
		Vision: attachVisionGate{reg: d.Providers},
		// The tenant's storage and ingestion are charged in the transaction that stores the file (M9-01).
		Charge: quotaTxChargerFor(d.DB),
	})
	if err != nil {
		return err
	}
	// Residue of a crash first, once, before a request is served.
	if rep, err := svc.Recover(ctx); err != nil {
		d.Log.Warn("files: recovery failed", slog.Any("err", err))
	} else if rep.Temp+rep.Orphans > 0 {
		d.Log.Info("files: crash residue removed", slog.Int("temp", rep.Temp), slog.Int("orphans", rep.Orphans))
	}
	// Then the periodic compaction, as a job of serve: it runs while serve runs and is stopped
	// before the store is closed.
	if err := attachFilesRegisterCompaction(d, svc); err != nil {
		return err
	}
	if err := gateway.Provide[gateway.AttachModelFiles](d.Services, svc); err != nil {
		return err
	}
	return gateway.Provide[gateway.FilesService](d.Services, svc)
}
