package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// maybeFinalize queues the activation of a generation under construction when
// every one of its versions has reached a final state and the build has been
// seeded. It is called after each version settles, so the last one to finish
// queues it; the key carries the number of versions, so a later completion is
// not mistaken for the earlier request.
func (s *Service) maybeFinalize(ctx context.Context, scope contracts.Scope, serviceIdentity, sourceID, genID string) error {
	g, ok, err := loadGen(ctx, s.db, scope, genID)
	if err != nil || !ok || !g.Seeded {
		return err
	}
	ig, err := s.ix.GetGeneration(ctx, scope, genID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	if ig.State != rag.IndexGenerationStateBuilding {
		return nil
	}
	open, err := countVersions(ctx, s.db, scope, `generation_id = ? AND state IN ('pending', 'parsed')`, genID)
	if err != nil || open > 0 {
		return err
	}
	total, err := countVersions(ctx, s.db, scope, `generation_id = ?`, genID)
	if err != nil {
		return err
	}
	return s.enqueueFinalize(ctx, scope, serviceIdentity, sourceID, genID, g.Lane, fmt.Sprint(total))
}

func (s *Service) enqueueFinalize(ctx context.Context, scope contracts.Scope, serviceIdentity, sourceID, genID string, l lane, tag string) error {
	if !l.valid() {
		l = laneIngest
	}
	_, err := s.enqueueFresh(ctx, scope, serviceIdentity, l.finalizeType(), finalizePayload{SourceID: sourceID, GenerationID: genID},
		"fin:"+genID+":"+tag, time.Time{})
	return err
}

func (s *Service) finalizeHandler(_ lane) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p finalizePayload
		if err := decodePayload(job.Payload, &p); err != nil {
			return err
		}
		if p.SourceID == "" || p.GenerationID == "" {
			return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "ingest: the finalize job names no source or generation"))
		}
		err := s.finalize(ctx, job, p)
		if isGone(err) {
			return nil
		}
		return err
	}
}

// finalize is generation activation (12.3): the build is validated against what
// the pipeline knows it wrote, and only then does it replace the generation that
// serves the source, in one transaction of the index. Embeddings are not
// waited for: the generation serves full-text results the moment it is valid,
// and its vectors arrive as the embedding jobs finish.
func (s *Service) finalize(ctx context.Context, job jobs.Job, p finalizePayload) error {
	scope := job.Scope
	if err := s.checkPause(ctx); err != nil {
		return err
	}
	unlock := s.lock(scope, p.SourceID)
	defer unlock()
	if src, ok, err := loadSource(ctx, s.db, scope, p.SourceID); err != nil {
		return err
	} else if !ok || src.Availability == availRemoved {
		return nil
	}
	g, ok, err := loadGen(ctx, s.db, scope, p.GenerationID)
	if err != nil || !ok {
		return err
	}
	ig, err := s.ix.GetGeneration(ctx, scope, p.GenerationID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	switch ig.State {
	case rag.IndexGenerationStateValidating:
		// Validated by an earlier delivery that stopped before the switch.
		return s.activate(ctx, scope, p.SourceID, p.GenerationID)
	case rag.IndexGenerationStateBuilding:
	default:
		return nil
	}
	if !g.Seeded {
		return nil
	}
	open, err := countVersions(ctx, s.db, scope, `generation_id = ? AND state IN ('pending', 'parsed')`, p.GenerationID)
	if err != nil || open > 0 {
		return err
	}
	indexed, err := countVersions(ctx, s.db, scope, `generation_id = ? AND state = 'indexed'`, p.GenerationID)
	if err != nil {
		return err
	}
	kept, err := s.keptDocuments(ctx, scope, p.SourceID, p.GenerationID)
	if err != nil {
		return err
	}
	held := indexed + kept
	if held == 0 {
		// Do not replace a generation that serves documents with one that holds
		// none: that is every document failing, not a source that emptied (a
		// deletion has already taken its documents out of the old generation).
		if old, ok, err := s.ix.ActiveGeneration(ctx, scope, p.SourceID); err != nil {
			return err
		} else if ok && old.Counts.Documents > 0 {
			cause := contracts.New(contracts.ErrValidationFailed, "ingest: no document could be indexed into the new generation; the serving one is kept")
			if err := s.retireGeneration(ctx, scope, p.SourceID, p.GenerationID, "no_documents"); err != nil {
				return err
			}
			return jobs.Permanent(cause)
		}
	}
	if _, err := s.ix.ValidateGeneration(ctx, scope, p.GenerationID, index.Expectation{Documents: &held}); err != nil {
		// ValidateGeneration marked it failed; its versions and vectors go.
		if rerr := s.retireGeneration(ctx, scope, p.SourceID, p.GenerationID, "validation_failed"); rerr != nil {
			return rerr
		}
		return jobs.Permanent(err)
	}
	return s.activate(ctx, scope, p.SourceID, p.GenerationID)
}

// keptDocuments counts the live items of a build whose newest version in it was
// not indexed (it failed, or was withdrawn) after an earlier one was: the build
// holds that earlier copy, which keeps serving the item exactly as it would in
// a serving generation (failVersion), so validation must expect it.
func (s *Service) keptDocuments(ctx context.Context, scope contracts.Scope, sourceID, genID string) (int, error) {
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT DISTINCT v.document_id FROM ragingest_version AS v`, Alias: "v",
		Where: `v.generation_id = ? AND v.state = 'superseded'
 AND NOT EXISTS (SELECT 1 FROM ragingest_version AS n WHERE n.application_id = v.application_id AND n.tenant_id = v.tenant_id
   AND n.workspace_id = v.workspace_id AND n.generation_id = v.generation_id AND n.item_key = v.item_key AND n.state = 'indexed')
 AND EXISTS (SELECT 1 FROM ragingest_item AS i WHERE i.application_id = v.application_id AND i.tenant_id = v.tenant_id
   AND i.workspace_id = v.workspace_id AND i.source_id = ? AND i.item_key = v.item_key)`,
		WhereArgs: []any{genID, sourceID},
	})
	if err != nil {
		return 0, err
	}
	var docs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return 0, err
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	return s.ix.CountDocuments(ctx, scope, genID, docs)
}

func (s *Service) activate(ctx context.Context, scope contracts.Scope, sourceID, genID string) error {
	if err := s.ix.ActivateGeneration(ctx, scope, genID); err != nil {
		return err
	}
	s.log.Info("index generation activated", "source", sourceID, "generation", genID)
	if err := s.refreshEmbedded(ctx, scope, genID); err != nil {
		return err
	}
	return s.dropRetiredVectors(ctx, scope, sourceID)
}
