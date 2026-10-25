package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// maxMessage bounds an error message stored with a version.
const maxMessage = 512

func clip(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	s = s[:maxMessage]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// stagedParse is the parse result kept between the parse job and the index job.
// It carries the parser identity, so the index job can refuse output that does
// not belong to the generation's recorded parser.
type stagedParse struct {
	Parser     rag.ComponentVersion `json:"parser"`
	Format     string               `json:"format"`
	Text       string               `json:"text"`
	PageStarts []int                `json:"page_starts,omitempty"`
	Blocks     []wire.Block         `json:"blocks"`
	Warnings   []string             `json:"warnings,omitempty"`
}

func encodeStage(res *parse.Result) ([]byte, error) {
	return json.Marshal(stagedParse{
		Parser: res.Parser, Format: string(res.Format), Text: res.Text, PageStarts: res.PageStarts, Blocks: res.Blocks, Warnings: res.Warnings,
	})
}

func decodeStage(b []byte) (stagedParse, error) {
	var st stagedParse
	if err := json.Unmarshal(b, &st); err != nil {
		return stagedParse{}, contracts.Wrap(contracts.ErrInternal, err, "ingest: a staged parse result is unreadable")
	}
	return st, nil
}

// ---------------------------------------------------------------- parse stage

// parseStage is sandboxed parsing (12.3): it reads one immutable source version,
// proves that the bytes are the version's, parses them in a disposable worker
// process and stages the result for the index stage.
func (s *Service) parseStage(ctx context.Context, job jobs.Job, l lane, p stagePayload) error {
	scope := job.Scope
	v, err := getVersion(ctx, s.db, scope, p.VersionID)
	if err != nil {
		return err
	}
	if v.SourceID != p.SourceID {
		return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "ingest: the version belongs to another source"))
	}
	switch v.State {
	case vPending:
	case vParsed:
		// A redelivery after the parse finished: make sure the next stage exists.
		return s.tolerateExhausted(ctx, scope, v.SourceID, s.enqueueStage(ctx, scope, job.ServiceIdentity, v, l))
	default:
		return nil
	}
	if src, ok, err := loadSource(ctx, s.db, scope, v.SourceID); err != nil {
		return err
	} else if !ok || src.Availability == availRemoved {
		return nil
	}
	if err := s.checkPause(ctx); err != nil {
		return err
	}
	if err := s.parserReady(ctx); err != nil {
		return err
	}
	if stale, err := s.versionStale(ctx, job, v, true); err != nil || stale {
		return err
	}

	res, err := s.reg.Resolve(ctx, scope, v.SourceID)
	if err != nil {
		return err
	}
	def, prov := res.Def, res.Provider
	if err := def.CheckScope(scope); err != nil {
		return jobs.Permanent(err)
	}
	data, hash, err := readItem(ctx, prov, scope, def, v.Key)
	switch {
	case errors.Is(err, source.ErrNotFound):
		// The item left the source after it was discovered; the next scan
		// tombstones it. This version will never be indexed.
		return s.retireVersion(ctx, job, v, "missing", true)
	case contracts.CodeOf(err) == contracts.ErrResourceExhausted:
		return s.failVersionPermanent(ctx, job, l, v, err, "too_large")
	case err != nil:
		return err
	}
	if hash != v.Hash {
		// What is there now is not the version that was recorded: never index
		// content under another content's hash.
		return s.retireVersion(ctx, job, v, "changed", true)
	}
	format := wire.Format(v.Format)
	if !format.Valid() {
		return s.failVersionPermanent(ctx, job, l, v, contracts.New(contracts.ErrValidationFailed, "ingest: the version has no parseable format"), "unsupported_format")
	}

	pres, err := s.parser.Parse(ctx, format, data)
	if err != nil {
		permanent, code, reason := parseFailure(err)
		if !permanent {
			return err
		}
		return s.failVersion(ctx, job, l, v, code, reason, err, true)
	}
	body, err := encodeStage(pres)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "ingest: the parse result cannot be staged")
	}
	advanced := false
	if err := s.db.Tx(ctx, func(tx *store.Tx) error {
		advanced = false
		if row, ok, err := loadSource(ctx, tx, scope, v.SourceID); err != nil {
			return err
		} else if !ok || row.Availability == availRemoved {
			return ErrSourceGone
		}
		ok, err := setVersion(ctx, tx, scope, v.ID, `state = 'parsed', job_id = '', updated_ns = ?`, []any{nsOf(s.clk.Now())}, vPending)
		if err != nil || !ok {
			return err
		}
		advanced = true
		return putStage(ctx, tx, scope, v.ID, body, s.clk.Now())
	}); err != nil {
		return err
	}
	if !advanced {
		return nil
	}
	v.State, v.JobID = vParsed, ""
	return s.tolerateExhausted(ctx, scope, v.SourceID, s.enqueueStage(ctx, scope, job.ServiceIdentity, v, l))
}

// tolerateExhausted decides what a full queue means for a stage whose own work
// is done and whose follow-up could not be queued. While other versions of the
// source have jobs in flight, each of them tops the window up when it finishes
// (settled), so the follow-up is simply late and the stage's job succeeds: it
// must not burn its attempts, or fail, over a queue that is merely busy. When
// nothing of the source is in flight nobody would come back for it, so the
// error is returned and the job is retried with backoff.
func (s *Service) tolerateExhausted(ctx context.Context, scope contracts.Scope, sourceID string, err error) error {
	if err == nil || !isExhausted(err) {
		return err
	}
	n, cerr := countVersions(ctx, s.db, scope, `source_id = ? AND state IN ('pending', 'parsed') AND job_id <> ''`, sourceID)
	if cerr == nil && n > 0 {
		s.log.Debug("the queue is full; the follow-up waits for a finished version to top the window up", "source", sourceID)
		return nil
	}
	return err
}

// readItem reads one item through the provider, which does the authorisation
// and the canonical-path checks, bounded by the declared file limit, and
// returns the content address of exactly the bytes it read.
func readItem(ctx context.Context, prov source.Provider, scope contracts.Scope, def source.Definition, key string) ([]byte, string, error) {
	c, err := prov.Open(ctx, scope, def, key)
	if err != nil {
		return nil, "", err
	}
	defer c.Close()
	limit := def.Limits.MaxFileBytes
	data, err := io.ReadAll(io.LimitReader(c, limit+1))
	if err != nil {
		return nil, "", contracts.Wrap(contracts.ErrUnavailable, err, "ingest: the item could not be read")
	}
	if int64(len(data)) > limit {
		return nil, "", contracts.New(contracts.ErrResourceExhausted, "ingest: the item is larger than the source's file limit")
	}
	sum := sha256.Sum256(data)
	return data, source.HashPrefix + hex.EncodeToString(sum[:]), nil
}

// parseFailure classifies what the parser reported. A failure caused by the
// document and the limits (malformed, encrypted, a bomb, needs OCR, over the
// time or memory limit) is deterministic: retrying hostile input only repeats
// its cost, so it ends the version as failed. A failure caused by the
// environment (the sandbox could not start, the worker was slow to start, it
// crashed) is retried, a bounded number of times.
func parseFailure(err error) (permanent bool, code contracts.ErrCode, reason string) {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return false, contracts.CodeOf(err), ""
	}
	reason, _ = ce.Details["reason"].(string)
	switch ce.Code {
	case contracts.ErrValidationFailed:
		return true, ce.Code, reason
	case contracts.ErrTimeout:
		if reason == "startup_timeout" {
			return false, ce.Code, reason
		}
		return true, contracts.ErrValidationFailed, "parse_limit"
	case contracts.ErrResourceExhausted:
		return true, contracts.ErrValidationFailed, "parse_limit"
	}
	return false, ce.Code, reason
}

// ---------------------------------------------------------------- index stage

// indexStage chunks a parsed version and writes it to the full-text index. From
// here on the document is searchable: embeddings are queued behind it and never
// hold it back (12.3: FTS before embeddings).
func (s *Service) indexStage(ctx context.Context, job jobs.Job, l lane, p stagePayload) error {
	scope := job.Scope
	v, err := getVersion(ctx, s.db, scope, p.VersionID)
	if err != nil {
		return err
	}
	if v.SourceID != p.SourceID {
		return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "ingest: the version belongs to another source"))
	}
	switch v.State {
	case vParsed:
	case vIndexed:
		return s.afterIndexed(ctx, job, l, v)
	default:
		return nil
	}
	if src, ok, err := loadSource(ctx, s.db, scope, v.SourceID); err != nil {
		return err
	} else if !ok || src.Availability == availRemoved {
		return nil
	}
	if err := s.checkPause(ctx); err != nil {
		return err
	}
	if stale, err := s.versionStale(ctx, job, v, true); err != nil || stale {
		return err
	}
	body, ok, err := getStage(ctx, s.db, scope, v.ID)
	if err != nil {
		return err
	}
	if !ok {
		// The staged result is gone (the version was replaced, or the row was
		// lost): parse it again.
		if _, err := setVersion(ctx, s.db, scope, v.ID, `state = 'pending', job_id = '', updated_ns = ?`, []any{nsOf(s.clk.Now())}, vParsed); err != nil {
			return err
		}
		v.State, v.JobID = vPending, ""
		return s.enqueueStage(ctx, scope, job.ServiceIdentity, v, l)
	}
	st, err := decodeStage(body)
	if err != nil {
		return jobs.Permanent(err)
	}
	ig, err := s.ix.GetGeneration(ctx, scope, v.GenerationID)
	if err != nil {
		return err
	}
	if st.Parser != ig.Components.Parser {
		return s.retireVersion(ctx, job, v, "components_changed", true)
	}
	pres := &parse.Result{Parser: st.Parser, Format: wire.Format(st.Format), Text: st.Text, PageStarts: st.PageStarts, Blocks: st.Blocks, Warnings: st.Warnings}
	cres, err := s.chunker.Chunk(ctx, pres.Document(v.DocumentID))
	if err != nil {
		if code := contracts.CodeOf(err); code == contracts.ErrValidationFailed || code == contracts.ErrResourceExhausted {
			return s.failVersionPermanent(ctx, job, l, v, err, "chunk_failed")
		}
		return err
	}
	return s.putAndRecord(ctx, job, l, v, cres)
}

// putAndRecord writes the chunks and records the version as indexed. The
// check that the version is still the current one, the write and the record
// run under the source's lock, so a scan that supersedes or deletes the item
// cannot interleave with the write and leave a deleted document retrievable.
func (s *Service) putAndRecord(ctx context.Context, job jobs.Job, l lane, v versionRow, cres chunk.Result) error {
	scope := job.Scope
	unlock := s.lock(scope, v.SourceID)
	cur, err := getVersion(ctx, s.db, scope, v.ID)
	if err != nil || cur.State != vParsed {
		unlock()
		return err
	}
	if src, ok, err := loadSource(ctx, s.db, scope, v.SourceID); err != nil || !ok || src.Availability == availRemoved {
		unlock()
		return err
	}
	_, err = s.ix.PutDocument(ctx, scope, v.GenerationID, index.Document{ID: v.DocumentID, Version: v.Hash, Chunks: cres})
	if err != nil {
		unlock()
		switch contracts.CodeOf(err) {
		case contracts.ErrConflict:
			// The generation stopped taking writes (activated, failed or
			// withdrawn) while this version waited.
			if stale, serr := s.versionStale(ctx, job, v, true); serr != nil || stale {
				return serr
			}
			return err
		case contracts.ErrResourceExhausted, contracts.ErrValidationFailed:
			return s.failVersionPermanent(ctx, job, l, v, err, "index_failed")
		}
		return err
	}
	childIDs := make([]string, len(cres.Children))
	for i, c := range cres.Children {
		childIDs[i] = c.ID
	}
	embed := s.pipe != nil && len(childIDs) > 0
	now := s.clk.Now()
	err = s.db.Tx(ctx, func(tx *store.Tx) error {
		state := embNone
		if embed {
			state = embPending
		}
		ok, err := setVersion(ctx, tx, scope, v.ID,
			`state = 'indexed', chunks = ?, chunk_ids = ?, embed_state = ?, job_id = '', updated_ns = ?`,
			[]any{len(childIDs), encodeIDs(childIDs), state, nsOf(now)}, vParsed)
		if err != nil || !ok {
			return err
		}
		if err := deleteStage(ctx, tx, scope, v.ID); err != nil {
			return err
		}
		// What the replaced versions of this document held, and this one no
		// longer does, is owed a removal from the vector index.
		if _, err := tx.ExecContext(ctx, `UPDATE ragingest_version SET vec_owed = 1
 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND generation_id = ? AND item_key = ?
   AND version_id <> ? AND state = 'superseded' AND chunk_ids <> '[]'`,
			scope.ApplicationID, scope.TenantID, scope.WorkspaceID, v.GenerationID, v.Key, v.ID); err != nil {
			return err
		}
		if !embed {
			return nil
		}
		for b := 0; b*vectorBatch < len(childIDs); b++ {
			n := min(vectorBatch, len(childIDs)-b*vectorBatch)
			if _, err := store.InsertScoped(ctx, tx, scope, "ragingest_embed",
				[]string{"version_id", "batch_no", "chunk_count"}, v.ID, b, n); err != nil {
				return err
			}
		}
		return nil
	})
	unlock()
	if err != nil {
		return err
	}
	v.State, v.ChunkIDs = vIndexed, childIDs
	if err := s.cleanupVectors(ctx, scope, v.SourceID); err != nil {
		return err
	}
	return s.afterIndexed(ctx, job, l, v)
}

// afterIndexed is everything that follows an indexed version, written so that
// a redelivered job completes whatever an earlier delivery did not: the
// embedding jobs, the window of further versions, the activation check.
func (s *Service) afterIndexed(ctx context.Context, job jobs.Job, l lane, v versionRow) error {
	scope := job.Scope
	if s.pipe != nil {
		if err := s.pumpEmbeds(ctx, scope, job.ServiceIdentity, v.SourceID, v.ID, false); err != nil && !isExhausted(err) {
			return err
		}
	}
	return s.settled(ctx, scope, job.ServiceIdentity, v.SourceID, v.GenerationID)
}

// settled runs after any version reaches a final state: it tops up the window
// of queued work and queues the generation's activation when nothing is left.
func (s *Service) settled(ctx context.Context, scope contracts.Scope, serviceIdentity, sourceID, genID string) error {
	exhausted, err := s.pump(ctx, scope, serviceIdentity, sourceID, s.cfg.EnqueueBatch, false)
	if err != nil {
		return err
	}
	if exhausted {
		err := contracts.New(contracts.ErrResourceExhausted, "ingest: the queue is full; waiting versions are queued as it drains")
		if err := s.tolerateExhausted(ctx, scope, sourceID, err); err != nil {
			return err
		}
	}
	if s.pipe != nil {
		// Embedding batches that could not be queued earlier (a full queue) are
		// picked up as versions finish.
		if err := s.pumpEmbeds(ctx, scope, serviceIdentity, sourceID, "", false); err != nil && !isExhausted(err) {
			return err
		}
	}
	return s.tolerateExhausted(ctx, scope, sourceID, s.maybeFinalize(ctx, scope, serviceIdentity, sourceID, genID))
}

func isExhausted(err error) bool {
	c := contracts.CodeOf(err)
	return c == contracts.ErrResourceExhausted || c == contracts.ErrRateLimited
}

// ---------------------------------------------------------------- stale and failed versions

// versionStale reports whether a version should no longer be worked on, and
// settles it if so: its generation stopped taking writes, or was built with
// other component versions than this build of the pipeline has. Output of a
// different parser or chunker is never mixed into a generation that records
// another one; the next sync builds a generation with the current components.
func (s *Service) versionStale(ctx context.Context, job jobs.Job, v versionRow, resync bool) (bool, error) {
	ig, err := s.ix.GetGeneration(ctx, job.Scope, v.GenerationID)
	if err != nil {
		if isNotFound(err) {
			return true, s.retireVersion(ctx, job, v, "generation_gone", false)
		}
		return false, err
	}
	switch ig.State {
	case rag.IndexGenerationStateBuilding, rag.IndexGenerationStateActive:
	default:
		return true, s.retireVersion(ctx, job, v, "generation_closed", false)
	}
	if !sameComponents(ig.Components, s.components()) {
		return true, s.retireVersion(ctx, job, v, "components_changed", resync)
	}
	return false, nil
}

func isNotFound(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return false
	}
	r, _ := ce.Details["reason"].(string)
	return ce.Code == contracts.ErrValidationFailed && r == "not_found"
}

// retireVersion supersedes a version that cannot be indexed as it is (the item
// changed or vanished, its generation closed) and, when asked, queues a sync so
// the source is looked at again.
func (s *Service) retireVersion(ctx context.Context, job jobs.Job, v versionRow, reason string, resync bool) error {
	if err := s.db.Tx(ctx, func(tx *store.Tx) error {
		return supersedeVersions(ctx, tx, job.Scope, `version_id = ?`, []any{v.ID}, reason, s.clk.Now())
	}); err != nil {
		return err
	}
	if resync {
		if _, err := s.RequestSync(ctx, job.Scope, v.SourceID, SyncOptions{
			Key: "sync:" + v.SourceID + ":stale:" + v.ID, ServiceIdentity: job.ServiceIdentity,
		}); err != nil && !isExhausted(err) {
			return err
		}
	}
	return s.maybeFinalize(ctx, job.Scope, job.ServiceIdentity, v.SourceID, v.GenerationID)
}

// failVersionPermanent records a failure and ends the job with it, permanently.
func (s *Service) failVersionPermanent(ctx context.Context, job jobs.Job, l lane, v versionRow, cause error, reason string) error {
	return s.failVersion(ctx, job, l, v, contracts.CodeOf(cause), reason, cause, true)
}

// failVersion marks a version failed: a recorded outcome that stays visible in
// the source status ("never half-indexed"). The version was not indexed, so a
// previous version of the document, if one is indexed, keeps serving.
func (s *Service) failVersion(ctx context.Context, job jobs.Job, l lane, v versionRow, code contracts.ErrCode, reason string, cause error, permanent bool) error {
	msg := ""
	var ce *contracts.Error
	if errors.As(cause, &ce) {
		msg = ce.Message
	}
	if err := s.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := setVersion(ctx, tx, job.Scope, v.ID,
			`state = 'failed', error_code = ?, error_reason = ?, error_message = ?, job_id = '', updated_ns = ?`,
			[]any{string(code), reason, clip(msg), nsOf(s.clk.Now())}, vPending, vParsed); err != nil {
			return err
		}
		return deleteStage(ctx, tx, job.Scope, v.ID)
	}); err != nil {
		return err
	}
	if err := s.settled(ctx, job.Scope, job.ServiceIdentity, v.SourceID, v.GenerationID); err != nil {
		return err
	}
	if permanent {
		return jobs.Permanent(cause)
	}
	return cause
}

// ---------------------------------------------------------------- queueing the stages

// enqueueStage queues the stage that follows the version's state: parse for a
// pending version, index for a parsed one. The idempotency key is per version
// (and per requeue), so repeating the call returns the same job.
func (s *Service) enqueueStage(ctx context.Context, scope contracts.Scope, serviceIdentity string, v versionRow, l lane) error {
	var typ, prefix string
	switch v.State {
	case vPending:
		typ, prefix = l.parseType(), "parse"
	case vParsed:
		typ, prefix = l.indexType(), "index"
	default:
		return nil
	}
	res, err := s.enqueue(ctx, scope, serviceIdentity, typ, stagePayload{SourceID: v.SourceID, VersionID: v.ID},
		requeueKey(prefix, v.ID, v.Requeues), time.Time{})
	if err != nil {
		return err
	}
	if res.Job.ID == v.JobID {
		return nil
	}
	_, err = setVersion(ctx, s.db, scope, v.ID, `job_id = ?`, []any{res.Job.ID}, v.State)
	return err
}

// pump keeps the window of queued stage jobs of one source filled, and no
// fuller: a source with a hundred thousand documents never puts a hundred
// thousand jobs in the queue. window is the most versions that may have a stage
// job queued at once; every version that finishes calls pump and takes the
// place of the one that left. With dead set it first looks at the jobs queued
// earlier and queues a stage again when its job ended failed or vanished (at
// most maxRequeues times, then the version fails): that is how a pipeline
// stalled by an outage, a full queue or a crash drains. exhausted reports that
// the queue (or an enqueue guard) refused a job, so waiting work remains and
// the caller should come back later.
func (s *Service) pump(ctx context.Context, scope contracts.Scope, serviceIdentity, sourceID string, window int, dead bool) (exhausted bool, err error) {
	lanes, err := s.laneMap(ctx, scope, sourceID)
	if err != nil {
		return false, err
	}
	laneOfVersion := func(v versionRow) lane {
		if l := lanes[v.GenerationID]; l.valid() {
			return l
		}
		return laneIngest
	}
	if dead {
		vs, err := queryVersions(ctx, s.db, scope, `source_id = ? AND state IN ('pending', 'parsed') AND job_id <> ''`, []any{sourceID},
			fmt.Sprintf(`ORDER BY created_ns, version_id LIMIT %d`, 2*window+64))
		if err != nil {
			return false, err
		}
		for _, v := range vs {
			alive, jerr := s.jobAlive(ctx, scope, v.JobID)
			if jerr != nil {
				return false, jerr
			}
			if alive {
				continue
			}
			if v.Requeues >= maxRequeues {
				if err := s.failExhausted(ctx, scope, v); err != nil {
					return false, err
				}
				continue
			}
			v.Requeues++
			if _, err := setVersion(ctx, s.db, scope, v.ID, `requeues = ?, job_id = '', updated_ns = ?`, []any{v.Requeues, nsOf(s.clk.Now())}, v.State); err != nil {
				return false, err
			}
			v.JobID = ""
			if err := s.enqueueStage(ctx, scope, serviceIdentity, v, laneOfVersion(v)); err != nil {
				if isExhausted(err) {
					return true, nil
				}
				return false, err
			}
		}
		if s.pipe != nil {
			if err := s.pumpEmbeds(ctx, scope, serviceIdentity, sourceID, "", true); err != nil {
				if isExhausted(err) {
					return true, nil
				}
				return false, err
			}
		}
	}
	inflight, err := countVersions(ctx, s.db, scope, `source_id = ? AND state IN ('pending', 'parsed') AND job_id <> ''`, sourceID)
	if err != nil {
		return false, err
	}
	room := window - inflight
	if room <= 0 {
		return false, nil
	}
	vs, err := queryVersions(ctx, s.db, scope, `source_id = ? AND state IN ('pending', 'parsed') AND job_id = ''`, []any{sourceID},
		fmt.Sprintf(`ORDER BY created_ns, version_id LIMIT %d`, room))
	if err != nil {
		return false, err
	}
	for _, v := range vs {
		if err := s.enqueueStage(ctx, scope, serviceIdentity, v, laneOfVersion(v)); err != nil {
			if isExhausted(err) {
				return true, nil
			}
			return false, err
		}
	}
	return false, nil
}

// failExhausted ends a version whose stage jobs kept failing.
func (s *Service) failExhausted(ctx context.Context, scope contracts.Scope, v versionRow) error {
	return s.db.Tx(ctx, func(tx *store.Tx) error {
		if _, err := setVersion(ctx, tx, scope, v.ID,
			`state = 'failed', error_code = ?, error_reason = 'job_failed', error_message = ?, job_id = '', updated_ns = ?`,
			[]any{string(contracts.ErrUnavailable), "the stage job failed " + fmt.Sprint(maxRequeues+1) + " times", nsOf(s.clk.Now())}, vPending, vParsed); err != nil {
			return err
		}
		return deleteStage(ctx, tx, scope, v.ID)
	})
}

// jobAlive reports whether a queued job can still run.
func (s *Service) jobAlive(ctx context.Context, scope contracts.Scope, jobID string) (bool, error) {
	j, err := s.q.Get(ctx, scope, jobID)
	if err != nil {
		if contracts.CodeOf(err) == contracts.ErrScopeViolation {
			return false, nil // pruned, or never there
		}
		return false, err
	}
	return j.State == jobs.StatePending || j.State == jobs.StateRunning, nil
}

func (s *Service) laneMap(ctx context.Context, scope contracts.Scope, sourceID string) (map[string]lane, error) {
	gens, err := listGens(ctx, s.db, scope, sourceID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]lane, len(gens))
	for _, g := range gens {
		out[g.ID] = g.Lane
	}
	return out, nil
}
