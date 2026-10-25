package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// continueAfter is how long a sync that found the queue full waits before it
// looks again.
const continueAfter = 30 * time.Second

// batchRows is how many snapshot records one transaction writes, so the store's
// single writer is never held for long (master plan 3 rule 12).
const batchRows = 1000

// SyncOptions shapes a sync request.
type SyncOptions struct {
	// Key makes the request idempotent: repeating it returns the first job,
	// even one that has ended. Empty derives one from the source's state, so
	// requests that arrive while a sync is waiting or running coalesce into it,
	// and a request after one that ended without scanning (it failed, or was
	// refused) queues a new sync.
	Key string
	// ServiceIdentity runs the sync, and every stage job it queues, under a
	// declared service identity (jobs.EnqueueRequest.ServiceIdentity) instead
	// of the initiating principal's live scope. The Pool's ServiceAuthorizer
	// decides; a declaration alone grants nothing.
	ServiceIdentity string
}

// RequestSync queues a sync of one source (class 5). The scope must be the one
// the source serves; it is re-validated by the pool just before the job runs,
// so a principal whose access was revoked in the meantime gets a failed job and
// no scan.
func (s *Service) RequestSync(ctx context.Context, scope contracts.Scope, sourceID string, opts SyncOptions) (jobs.EnqueueResult, error) {
	if err := scope.Validate(); err != nil {
		return jobs.EnqueueResult{}, err
	}
	if !ids.Valid(sourceID, source.IDPrefix) {
		return jobs.EnqueueResult{}, contracts.New(contracts.ErrValidationFailed, "ingest: not a source id")
	}
	if opts.Key != "" {
		return s.enqueue(ctx, scope, opts.ServiceIdentity, JobSync, syncPayload{SourceID: sourceID}, opts.Key, time.Time{})
	}
	row, _, err := loadSource(ctx, s.db, scope, sourceID)
	if err != nil {
		return jobs.EnqueueResult{}, err
	}
	return s.enqueueFresh(ctx, scope, opts.ServiceIdentity, JobSync, syncPayload{SourceID: sourceID},
		fmt.Sprintf("sync:%s:%d:%d", sourceID, row.SnapshotSeq, nsOf(row.LastScan)), time.Time{})
}

// CheckDefinition compares the source's registered definition with the one its
// last accepted scan used, and queues a sync when they differ or the source was
// never scanned. It is what the registration layer calls after a package is
// re-audited: a source whose declaration changed (root, include or exclude
// rules, formats, limits, deletion behaviour) is scanned again under the new
// rules, which tombstones what is no longer admitted and picks up what now is.
func (s *Service) CheckDefinition(ctx context.Context, scope contracts.Scope, sourceID string, opts SyncOptions) (bool, jobs.EnqueueResult, error) {
	res, err := s.reg.Resolve(ctx, scope, sourceID)
	if err != nil {
		return false, jobs.EnqueueResult{}, err
	}
	fp := fingerprintOf(res.Def)
	row, found, err := loadSource(ctx, s.db, scope, sourceID)
	if err != nil {
		return false, jobs.EnqueueResult{}, err
	}
	if found && row.Fingerprint == fp {
		return false, jobs.EnqueueResult{}, nil
	}
	if opts.Key == "" {
		opts.Key = "sync:" + sourceID + ":def:" + fp[:16]
	}
	r, err := s.RequestSync(ctx, scope, sourceID, opts)
	return err == nil, r, err
}

// fingerprintOf digests what decides which items a source admits: its type,
// root, rules, formats, limits, scope binding and deletion behaviour. A changed
// digest is a changed definition. The title, the sync schedule and the package
// version are not part of it: a re-audit that leaves the declaration alone
// needs no rescan.
func fingerprintOf(def source.Definition) string {
	h := sha256.New()
	put := func(fields ...string) {
		for _, f := range fields {
			var n [binary.MaxVarintLen64]byte
			h.Write(n[:binary.PutUvarint(n[:], uint64(len(f)))])
			h.Write([]byte(f))
		}
	}
	list := func(label string, l []string) {
		put(label, fmt.Sprint(len(l)))
		put(l...)
	}
	put("alixai/ingest/definition/v1", string(def.Type), def.ApplicationID,
		string(def.Tenant.Mode), def.Tenant.ID, string(def.Workspace.Mode), def.Workspace.ID,
		string(def.Root.Kind), def.Root.Path, def.Root.MountID, def.Root.SubPath)
	list("include", def.Include)
	list("exclude", def.Exclude)
	formats := append([]string(nil), def.Formats...)
	sort.Strings(formats)
	list("formats", formats)
	put("limits", fmt.Sprint(def.Limits.MaxFileBytes), fmt.Sprint(def.Limits.MaxTotalBytes), fmt.Sprint(def.Limits.MaxFiles))
	put("deletion", string(def.Deletion.OnSourceRemoved), fmt.Sprint(int64(def.Deletion.PurgeAfter)))
	return hex.EncodeToString(h.Sum(nil))
}

func deletionMode(def source.Definition) string {
	if def.Deletion.OnSourceRemoved == ap.RAGSourceDeletionOnSourceRemovedPurge {
		return modePurge
	}
	return modeDisable
}

// documentID is the stable identity of one item inside its source: a bounded
// digest of the source and the item key, never the path itself. It is the
// value the chunker hashes into every chunk id (chunk.Document.SourceID, at
// most 128 bytes) and the document id of the index, so unchanged content keeps
// its chunk ids across versions and generations, and two items never share one.
func documentID(sourceID, key string) string {
	sum := sha256.Sum256([]byte("alixai/ingest/document/v1\x00" + sourceID + "\x00" + key))
	return "doc_" + hex.EncodeToString(sum[:16])
}

// formatFor maps a sniffed type to the parser worker's formats.
func formatFor(mime string) (wire.Format, bool) {
	switch mime {
	case source.MIMEText:
		return wire.FormatTXT, true
	case source.MIMEMarkdown:
		return wire.FormatMD, true
	case source.MIMEHTML:
		return wire.FormatHTML, true
	case source.MIMECSV:
		return wire.FormatCSV, true
	case source.MIMEPDF:
		return wire.FormatPDF, true
	case source.MIMEDOCX:
		return wire.FormatDOCX, true
	}
	return "", false
}

func sameRecord(a, b source.Record) bool {
	return a.Key == b.Key && a.Size == b.Size && a.ModTime.Equal(b.ModTime) && a.Hash == b.Hash && a.MIME == b.MIME && a.SeenAt.Equal(b.SeenAt)
}

// syncHandler is the handler of JobSync.
func (s *Service) syncHandler(ctx context.Context, job jobs.Job) error {
	var p syncPayload
	if err := decodePayload(job.Payload, &p); err != nil {
		return err
	}
	if !ids.Valid(p.SourceID, source.IDPrefix) {
		return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "ingest: the sync job names no source"))
	}
	err := s.sync(ctx, job, p.SourceID)
	if isGone(err) {
		return nil
	}
	return err
}

// sync is one pass of the front half of 12.3: discovery, scope check, MIME and
// size validation (the provider does both), the mass-deletion guard, immutable
// source versions with their hashes, the tombstones, and the queue of the parse
// stage.
func (s *Service) sync(ctx context.Context, job jobs.Job, sourceID string) error {
	scope := job.Scope
	if err := s.checkPause(ctx); err != nil {
		return err
	}
	unlock := s.syncLock(scope, sourceID)
	defer unlock()

	res, err := s.reg.Resolve(ctx, scope, sourceID)
	if err != nil {
		return err
	}
	def, prov := res.Def, res.Provider
	if prov == nil || def.SourceID != sourceID {
		return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "ingest: the registry resolved another source than the one asked for"))
	}
	// The authorisation and scope check of 12.3: the verified scope of the job
	// must be the one the definition serves, before anything is read.
	if err := def.CheckScope(scope); err != nil {
		return jobs.Permanent(err)
	}
	src, found, err := loadSource(ctx, s.db, scope, sourceID)
	if err != nil {
		return err
	}
	if found {
		// Finish the deletions an earlier pass wrote down and did not apply; a
		// removed source has nothing else to do.
		if err := s.applyOutbox(ctx, scope, sourceID); err != nil {
			return err
		}
		if src.Availability == availRemoved {
			return nil
		}
	}

	prevRecs, err := s.loadSnapshot(ctx, s.db, scope, sourceID)
	if err != nil {
		return err
	}
	prev := make(map[string]source.Record, len(prevRecs))
	for _, r := range prevRecs {
		prev[r.Key] = r
	}
	fp := fingerprintOf(def)
	now := s.clk.Now()

	cs, derr := prov.Discover(ctx, scope, def, prevRecs)
	if derr != nil {
		return s.scanFailed(ctx, scope, def, derr, now)
	}

	verdict := evaluateGuard(s.cfg.Guard, prevRecs, cs)
	if verdict.Hold {
		confirmed := found && src.ConfirmDigest == verdict.Digest && src.ConfirmDigest != "" && now.Before(src.ConfirmUntil)
		if !confirmed {
			return s.holdScan(ctx, scope, def, verdict, now)
		}
	}

	target, err := s.applyScan(ctx, job, def, fp, prev, cs, src, found, now, verdict)
	if err != nil {
		return err
	}
	exhausted, err := s.pump(ctx, scope, job.ServiceIdentity, sourceID, s.cfg.EnqueueBatch, true)
	if err != nil {
		return err
	}
	if exhausted {
		// The queue refused work: what is waiting is picked up by a follow-up
		// sync a little later (finished versions top the window up as well).
		if _, err := s.enqueue(ctx, scope, job.ServiceIdentity, JobSync, syncPayload{SourceID: sourceID},
			fmt.Sprintf("sync:%s:more:%d", sourceID, now.UnixNano()), now.Add(continueAfter)); err != nil {
			if err := s.tolerateExhausted(ctx, scope, sourceID, err); err != nil {
				return err
			}
		}
	}
	if err := s.tolerateExhausted(ctx, scope, sourceID, s.maybeFinalize(ctx, scope, job.ServiceIdentity, sourceID, target.ID)); err != nil {
		return err
	}
	// A definition that was re-audited while this scan ran was scanned under the
	// old rules; scan again under the new ones.
	if again, err := s.reg.Resolve(ctx, scope, sourceID); err == nil {
		if now := fingerprintOf(again.Def); now != fp {
			_, err := s.RequestSync(ctx, scope, sourceID, SyncOptions{Key: "sync:" + sourceID + ":def:" + now[:16], ServiceIdentity: job.ServiceIdentity})
			return err
		}
	}
	return nil
}

// applyScan writes an accepted scan and applies its deletions, under the
// source's write lock: an index stage cannot write a document between the
// moment the item is recorded as deleted and the moment it is hidden.
func (s *Service) applyScan(ctx context.Context, job jobs.Job, def source.Definition, fp string, prev map[string]source.Record,
	cs source.ChangeSet, src sourceRow, found bool, now time.Time, verdict guardVerdict) (genTarget, error) {
	scope, sourceID := job.Scope, def.SourceID
	unlock := s.lock(scope, sourceID)
	defer unlock()
	target, err := s.planGeneration(ctx, scope, job.ServiceIdentity, sourceID)
	if err != nil {
		return genTarget{}, err
	}
	a := &scanApply{
		scope: scope, def: def, fp: fp, prev: prev, cs: cs, src: src, found: found, target: target, now: now, verdict: verdict,
	}
	if err := s.persistScan(ctx, a); err != nil {
		return genTarget{}, err
	}
	if err := s.applyOutboxLocked(ctx, scope, sourceID); err != nil {
		return genTarget{}, err
	}
	return target, s.cleanupVectors(ctx, scope, sourceID)
}

// scanFailed records why a scan did not complete and returns the error to the
// queue. Nothing from the scan is applied: the provider returns no changes with
// an error, the snapshot is untouched and last_success does not advance (master
// plan 18: a source that could not be checked is not reported as checked).
func (s *Service) scanFailed(ctx context.Context, scope contracts.Scope, def source.Definition, derr error, now time.Time) error {
	code := contracts.CodeOf(derr)
	reason := ""
	switch code {
	case contracts.ErrUnavailable:
		reason = ReasonRootUnreachable
	case contracts.ErrResourceExhausted:
		reason = ReasonLimitExceeded
	}
	if reason != "" {
		var msg string
		var ce *contracts.Error
		if errors.As(derr, &ce) {
			msg = ce.Message
		}
		if _, err := s.updateSource(ctx, scope, def.SourceID, true, func(_ *store.Tx, r *sourceRow) (bool, error) {
			if r.Availability == availRemoved {
				return false, ErrSourceGone
			}
			if r.Availability != availUnavailable || r.Reason != reason {
				r.Since = now
			}
			r.Availability, r.Reason, r.ErrorCode, r.Message = availUnavailable, reason, string(code), msg
			r.LastScan = now
			r.DeletionMode, r.PurgeAfter = deletionMode(def), def.Deletion.PurgeAfter
			return true, nil
		}); err != nil && !isGone(err) {
			return err
		}
	}
	if code == contracts.ErrResourceExhausted {
		// The same scan finds the same excess; only an operator or a smaller
		// source changes the answer, so it is not retried.
		return jobs.Permanent(derr)
	}
	return derr
}

// ---------------------------------------------------------------- generations

// genTarget is the generation a scan feeds.
type genTarget struct {
	ID string
	// Lane is the stage job family of the generation's builds.
	Lane lane
	// Incremental is true when the target is the active generation: the scan's
	// changes are written into it as they are indexed. Otherwise the target is
	// a generation under construction that is seeded with every live item and
	// activated when it is complete.
	Incremental bool
}

func errGenerationBusy() error {
	return contracts.New(contracts.ErrUnavailable, "ingest: the generation of this source is being activated; try again")
}

// planGeneration decides which generation the scan feeds (12.3: a changed
// component creates a new index generation that becomes active only after
// validation). The components of this build of the pipeline are compared with
// those of the active generation: equal means the changes go into it; different
// (or no generation yet) means a generation is built beside it.
func (s *Service) planGeneration(ctx context.Context, scope contracts.Scope, serviceIdentity, sourceID string) (genTarget, error) {
	comps := s.components()
	gens, err := s.ix.ListGenerations(ctx, scope, sourceID)
	if err != nil {
		return genTarget{}, err
	}
	var active, building *index.Generation
	for i := range gens {
		switch gens[i].State {
		case rag.IndexGenerationStateActive:
			active = &gens[i]
		case rag.IndexGenerationStateBuilding, rag.IndexGenerationStateValidating:
			building = &gens[i]
		}
	}
	ensure := func(id string, ln lane, seeded bool) error {
		return s.db.Tx(ctx, func(tx *store.Tx) error { return s.ensureGen(ctx, tx, scope, sourceID, id, ln, seeded) })
	}
	if active != nil && sameComponents(active.Components, comps) {
		if building != nil {
			if err := s.retireGeneration(ctx, scope, sourceID, string(building.GenerationID), "components_reverted"); err != nil {
				return genTarget{}, err
			}
		}
		id := string(active.GenerationID)
		if err := ensure(id, laneIngest, true); err != nil {
			return genTarget{}, err
		}
		return genTarget{ID: id, Lane: laneIngest, Incremental: true}, nil
	}
	if building != nil {
		id := string(building.GenerationID)
		if sameComponents(building.Components, comps) {
			ln := laneOf(active)
			if g, ok, err := loadGen(ctx, s.db, scope, id); err != nil {
				return genTarget{}, err
			} else if ok {
				ln = g.Lane
			}
			if building.State == rag.IndexGenerationStateValidating {
				// A build that is being activated takes no more writes.
				if err := s.enqueueFinalize(ctx, scope, serviceIdentity, sourceID, id, ln, "validating"); err != nil {
					return genTarget{}, err
				}
				return genTarget{}, errGenerationBusy()
			}
			if err := ensure(id, ln, false); err != nil {
				return genTarget{}, err
			}
			return genTarget{ID: id, Lane: ln}, nil
		}
		// The components changed again while a build was running: that build
		// would activate a generation that is already out of date.
		if err := s.retireGeneration(ctx, scope, sourceID, id, "components_changed"); err != nil {
			return genTarget{}, err
		}
	}
	g, err := s.ix.BeginGeneration(ctx, scope, sourceID, comps)
	if err != nil {
		return genTarget{}, err
	}
	id := string(g.GenerationID)
	ln := laneOf(active)
	if err := ensure(id, ln, false); err != nil {
		return genTarget{}, err
	}
	return genTarget{ID: id, Lane: ln}, nil
}

// laneOf is the lane a new build runs in: a build beside an active generation
// is reindexing (class 6), the first build of a source is ingestion.
func laneOf(active *index.Generation) lane {
	if active != nil {
		return laneReindex
	}
	return laneIngest
}

// retireGeneration abandons a generation that is being built: it is failed in
// the index, its versions are superseded and its vectors removed.
func (s *Service) retireGeneration(ctx context.Context, scope contracts.Scope, sourceID, genID, reason string) error {
	cause := contracts.New(contracts.ErrConflict, "ingest: build abandoned: "+reason)
	if err := s.ix.FailGeneration(ctx, scope, genID, cause); err != nil && contracts.CodeOf(err) != contracts.ErrConflict {
		return err
	}
	now := s.clk.Now()
	if err := s.db.Tx(ctx, func(tx *store.Tx) error {
		return supersedeVersions(ctx, tx, scope, `generation_id = ?`, []any{genID}, reason, now)
	}); err != nil {
		return err
	}
	return s.dropGenerationVectors(ctx, scope, genID)
}

// dropGenerationVectors removes the embeddings of a generation that no longer
// serves, once, and remembers it.
func (s *Service) dropGenerationVectors(ctx context.Context, scope contracts.Scope, genID string) error {
	if s.vec != nil {
		if _, err := s.vec.DeleteGeneration(ctx, scope, genID); err != nil {
			return err
		}
	}
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
			SQL: `UPDATE ragingest_generation SET vectors_dropped = 1`, Where: `generation_id = ?`, WhereArgs: []any{genID},
		})
		return err
	})
}

// ---------------------------------------------------------------- persisting a scan

// scanApply is one accepted scan, ready to be written.
type scanApply struct {
	scope   contracts.Scope
	def     source.Definition
	fp      string
	prev    map[string]source.Record
	cs      source.ChangeSet
	src     sourceRow
	found   bool
	target  genTarget
	now     time.Time
	verdict guardVerdict
}

type batchItem struct {
	rec     source.Record
	write   bool // the snapshot row changes
	version bool // an immutable source version is created
	charge  bool // the content is new to the source (added or modified), so its bytes are charged to ingestion
}

// persistScan writes an accepted scan: the changed snapshot rows together with
// the source versions they create (in batches, each its own transaction, a
// version never without its snapshot row), then, in one last transaction, the
// deletions (snapshot rows, the outbox and the superseded versions), the
// generation's state and the source row. A crash between batches leaves rows
// that are consistent with each other; the next scan finds them unchanged and
// repeats the rest.
func (s *Service) persistScan(ctx context.Context, a *scanApply) error {
	sourceID := a.def.SourceID
	existing, err := s.currentVersions(ctx, a.scope, a.target.ID)
	if err != nil {
		return err
	}
	needVersion := map[string]bool{}
	if a.target.Incremental {
		for _, it := range a.cs.Added {
			needVersion[it.Key] = true
		}
		for _, it := range a.cs.Modified {
			needVersion[it.Key] = true
		}
	}
	fresh := map[string]bool{}
	for _, it := range a.cs.Added {
		fresh[it.Key] = true
	}
	for _, it := range a.cs.Modified {
		fresh[it.Key] = true
	}
	var work []batchItem
	for _, r := range a.cs.Next {
		it := batchItem{rec: r, charge: fresh[r.Key]}
		if p, ok := a.prev[r.Key]; !ok || !sameRecord(p, r) {
			it.write = true
		}
		if a.target.Incremental {
			it.version = needVersion[r.Key] && existing[r.Key] != r.Hash
		} else {
			it.version = existing[r.Key] != r.Hash
		}
		if it.write || it.version {
			work = append(work, it)
		}
	}
	for start := 0; start < len(work); start += batchRows {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+batchRows, len(work))
		if err := s.persistBatch(ctx, a, work[start:end]); err != nil {
			if contracts.CodeOf(err) == contracts.ErrResourceExhausted {
				// The day's ingestion is used up: the same scan meets the same refusal until the window resets,
				// so the job ends here (the batches before it are whole, the next scan repeats the rest).
				s.log.Warn("ingest: a sync stopped at the tenant's ingestion quota", slog.String("source_id", sourceID))
				return jobs.Permanent(err)
			}
			return err
		}
	}
	return s.persistFinal(ctx, a, sourceID)
}

// currentVersions maps the item key to the hash of its current (not superseded)
// version in a generation.
func (s *Service) currentVersions(ctx context.Context, scope contracts.Scope, genID string) (map[string]string, error) {
	rows, err := store.QueryScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT item_key, hash FROM ragingest_version`, Where: `generation_id = ? AND state <> 'superseded'`, WhereArgs: []any{genID},
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, h string
		if err := rows.Scan(&k, &h); err != nil {
			return nil, err
		}
		out[k] = h
	}
	return out, rows.Err()
}

func (s *Service) persistBatch(ctx context.Context, a *scanApply, batch []batchItem) error {
	scope, sourceID := a.scope, a.def.SourceID
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		if row, ok, err := loadSource(ctx, tx, scope, sourceID); err != nil {
			return err
		} else if ok && row.Availability == availRemoved {
			return ErrSourceGone
		}
		// The ingestion quota is charged by this transaction, before its rows: a refusal writes none of them.
		var bytes int64
		for _, it := range batch {
			if _, readable := formatFor(it.rec.MIME); it.version && it.charge && readable {
				bytes += it.rec.Size
			}
		}
		if err := s.chargeBatch(ctx, tx, scope, bytes); err != nil {
			return err
		}
		upsert, err := tx.PrepareContext(ctx, `INSERT INTO ragingest_item
 (application_id, tenant_id, workspace_id, source_id, item_key, size, mod_ns, hash, mime, seen_ns)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (application_id, tenant_id, workspace_id, source_id, item_key)
 DO UPDATE SET size = excluded.size, mod_ns = excluded.mod_ns, hash = excluded.hash, mime = excluded.mime, seen_ns = excluded.seen_ns`)
		if err != nil {
			return err
		}
		defer upsert.Close()
		now := s.clk.Now()
		for _, it := range batch {
			r := it.rec
			if it.write {
				if _, err := upsert.ExecContext(ctx, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, sourceID, r.Key,
					r.Size, nsOf(r.ModTime), r.Hash, r.MIME, nsOf(r.SeenAt)); err != nil {
					return err
				}
			}
			if !it.version {
				continue
			}
			// The old version of this document in this generation is replaced; its
			// chunks keep serving until the new version is indexed.
			if err := supersedeVersions(ctx, tx, scope, `generation_id = ? AND item_key = ? AND hash <> ?`,
				[]any{a.target.ID, r.Key, r.Hash}, "replaced", now); err != nil {
				return err
			}
			v := versionRow{
				ID: s.ids.New("ver"), SourceID: sourceID, GenerationID: a.target.ID, Key: r.Key, DocumentID: documentID(sourceID, r.Key),
				Hash: r.Hash, Size: r.Size, ModNS: nsOf(r.ModTime), MIME: r.MIME, State: vPending, EmbedState: embNone, Created: now, Updated: now,
			}
			if f, ok := formatFor(r.MIME); ok {
				v.Format = string(f)
			} else {
				// The source admits the type, but the parser worker has no reader
				// for it: a recorded outcome, not a silent gap.
				v.State, v.ErrReason, v.ErrMessage = vSkipped, "unsupported_format", "the parser has no reader for "+r.MIME
			}
			if err := insertVersion(ctx, tx, scope, v); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) persistFinal(ctx context.Context, a *scanApply, sourceID string) error {
	scope := a.scope
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		now := s.clk.Now()
		row, ok, err := loadSource(ctx, tx, scope, sourceID)
		if err != nil {
			return err
		}
		if ok && row.Availability == availRemoved {
			return ErrSourceGone
		}
		if ok != a.found || (ok && row.SnapshotSeq != a.src.SnapshotSeq) {
			return contracts.New(contracts.ErrConflict, "ingest: the source was scanned by another pass in the meantime")
		}
		if !ok {
			row = sourceRow{ID: sourceID, Created: now, Since: now}
		}

		for _, ts := range a.cs.Deleted {
			if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL: `DELETE FROM ragingest_item`, Where: `source_id = ? AND item_key = ?`, WhereArgs: []any{sourceID, ts.Key},
			}); err != nil {
				return err
			}
			if _, err := store.InsertScoped(ctx, tx, scope, "ragingest_tombstone",
				[]string{"source_id", "item_key", "document_id", "reason", "last_hash", "at_ns", "purge_after_ns", "applied_ns"},
				sourceID, ts.Key, documentID(sourceID, ts.Key), string(ts.Reason), ts.Last.Hash, nsOf(ts.At), nsOf(ts.PurgeAfter), 0); err != nil {
				return err
			}
			if err := supersedeVersions(ctx, tx, scope, `source_id = ? AND item_key = ?`, []any{sourceID, ts.Key}, "deleted", now); err != nil {
				return err
			}
			if err := markOwed(ctx, tx, scope, `source_id = ? AND item_key = ?`, []any{sourceID, ts.Key}); err != nil {
				return err
			}
		}
		if !a.target.Incremental {
			// Every version of the build exists now: "nothing left to do" can be
			// trusted from here on.
			if _, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
				SQL: `UPDATE ragingest_generation SET seeded = 1`, Where: `generation_id = ?`, WhereArgs: []any{a.target.ID},
			}); err != nil {
				return err
			}
		}

		wasHeld := row.HoldDigest != ""
		if row.Availability != availAvailable {
			row.Since = now
		}
		row.Fingerprint = a.fp
		row.Availability, row.Reason, row.ErrorCode, row.Message = availAvailable, "", "", ""
		row.SnapshotSeq++
		row.LiveItems = len(a.cs.Next)
		row.LastScan, row.LastSuccess = now, now
		row.Discovered, row.Skipped, row.Failed, row.Tombstoned = a.cs.Counts.Discovered, len(a.cs.Skipped), len(a.cs.Failed), len(a.cs.Deleted)
		row.DeletionMode, row.PurgeAfter = deletionMode(a.def), a.def.Deletion.PurgeAfter
		row.HoldDigest, row.HoldDeleted, row.HoldPrevious, row.HoldWhy, row.HoldAt = "", 0, 0, "", time.Time{}
		row.ConfirmDigest, row.ConfirmBy, row.ConfirmUntil = "", "", time.Time{}
		row.Updated = now
		if err := writeSource(ctx, tx, scope, row, !ok); err != nil {
			return err
		}
		if a.verdict.Hold && wasHeld {
			// An operator confirmed this deletion and it was applied: record it.
			return s.auditTx(ctx, tx, scope, EventMassDeletionApplied, massDeletionEvent{
				SourceID: sourceID, Digest: a.verdict.Digest, Deleted: a.verdict.Deleted, Previous: a.verdict.Previous, Why: a.verdict.Why,
			})
		}
		return nil
	})
}

// supersedeVersions marks the not-yet-superseded versions matching the clause
// superseded, with the reason; their staged parse and embedding batches are
// dropped. It does not touch the vector index's side of the books: a replaced
// version keeps serving (and keeps its embeddings) until its successor is
// indexed, and only then does it owe the vector index a cleanup (see
// markOwed and putAndRecord).
func supersedeVersions(ctx context.Context, tx *store.Tx, scope contracts.Scope, where string, args []any, reason string, now time.Time) error {
	vs, err := queryVersions(ctx, tx, scope, `(`+where+`) AND state <> 'superseded'`, args, "")
	if err != nil {
		return err
	}
	for _, v := range vs {
		if _, err := setVersion(ctx, tx, scope, v.ID, `state = 'superseded', error_reason = ?, updated_ns = ?`, []any{reason, nsOf(now)}); err != nil {
			return err
		}
		if err := deleteStage(ctx, tx, scope, v.ID); err != nil {
			return err
		}
		if err := deleteEmbedRows(ctx, tx, scope, v.ID); err != nil {
			return err
		}
	}
	return nil
}

// markOwed records that the matching versions that hold indexed chunks owe the
// vector index a removal of their embeddings (cleanupVectors does it): their
// item was deleted. Versions in every state are marked, because a version that
// an earlier replacement left behind still holds embeddings.
func markOwed(ctx context.Context, tx *store.Tx, scope contracts.Scope, where string, args []any) error {
	_, err := store.ExecScoped(ctx, tx, scope, store.ScopedStmt{
		SQL: `UPDATE ragingest_version SET vec_owed = 1`, Where: `(` + where + `) AND chunk_ids <> '[]'`, WhereArgs: args,
	})
	return err
}
