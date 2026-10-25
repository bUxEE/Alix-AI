package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The job types of the pipeline (master plan 3 rule 13, 12.3, 17.1). Each stage
// of 12.3 is its own type, so a failure retries on its own and progress can be
// read from the queue.
//
// Ingestion proper is priority class 5. A rebuild because a component version
// changed is reindexing, class 6: while an active generation still serves the
// source, rebuilding it must yield to everything else, ingestion included.
// The class belongs to the type and is not the producer's to choose.
const (
	// JobSync discovers a source, applies the mass-deletion guard, records the
	// immutable source versions and the deletions, and queues the parse stage.
	JobSync = "rag.ingest.sync"
	// JobParse reads one source version and parses it in the sandboxed worker.
	JobParse = "rag.ingest.parse"
	// JobIndex chunks one parsed version and writes it to the full-text index.
	JobIndex = "rag.ingest.index"
	// JobFinalize validates and activates a generation whose build is complete.
	JobFinalize = "rag.ingest.finalize"
	// JobReindexParse, JobReindexIndex and JobReindexFinalize are the same three
	// stages for a generation built because a component version changed.
	JobReindexParse    = "rag.reindex.parse"
	JobReindexIndex    = "rag.reindex.index"
	JobReindexFinalize = "rag.reindex.finalize"
	// JobMaintain compacts tombstoned data, retires old generations and drops
	// the vectors of retired ones. It is class 6.
	JobMaintain = "rag.ingest.maintain"
)

// Retry limits of the stage jobs. A document-caused failure is never retried
// (see parseFailure); these bound the failures the environment causes.
const (
	maxSyncAttempts  = 5
	maxParseAttempts = 3
	maxIndexAttempts = 4
	maxFinalAttempts = 4
	maxMaintAttempts = 3
	// maxRequeues is how often the reconciler queues a stage again after its
	// job ended failed, before the version itself is marked failed.
	maxRequeues = 3
)

// JobSpecs lists the job types this package registers on the queue, in the
// order they are registered. The embedding job (vector.EmbedJobType) is
// registered as well when embedding is configured; it is not listed here.
func JobSpecs() []jobs.TypeSpec {
	return []jobs.TypeSpec{
		{Name: JobSync, Class: jobs.ClassIngest, MaxAttempts: maxSyncAttempts},
		{Name: JobParse, Class: jobs.ClassIngest, MaxAttempts: maxParseAttempts},
		{Name: JobIndex, Class: jobs.ClassIngest, MaxAttempts: maxIndexAttempts},
		{Name: JobFinalize, Class: jobs.ClassIngest, MaxAttempts: maxFinalAttempts},
		{Name: JobReindexParse, Class: jobs.ClassMaintenance, MaxAttempts: maxParseAttempts},
		{Name: JobReindexIndex, Class: jobs.ClassMaintenance, MaxAttempts: maxIndexAttempts},
		{Name: JobReindexFinalize, Class: jobs.ClassMaintenance, MaxAttempts: maxFinalAttempts},
		{Name: JobMaintain, Class: jobs.ClassMaintenance, MaxAttempts: maxMaintAttempts},
	}
}

// PausedJobTypes lists the job types that disk pressure holds back: every
// stage that writes derived data, and the embedding job. Maintenance only
// removes data and is not held.
func PausedJobTypes() []string {
	var out []string
	for _, sp := range JobSpecs() {
		if sp.Name != JobMaintain {
			out = append(out, sp.Name)
		}
	}
	return append(out, vector.EmbedJobType)
}

// lane says which job types a generation's stages run as.
type lane string

const (
	laneIngest  lane = "ingest"
	laneReindex lane = "reindex"
)

func (l lane) valid() bool { return l == laneIngest || l == laneReindex }

func (l lane) parseType() string {
	if l == laneReindex {
		return JobReindexParse
	}
	return JobParse
}

func (l lane) indexType() string {
	if l == laneReindex {
		return JobReindexIndex
	}
	return JobIndex
}

func (l lane) finalizeType() string {
	if l == laneReindex {
		return JobReindexFinalize
	}
	return JobFinalize
}

// The payloads carry references only: job payloads are stored in clear text.
type (
	syncPayload struct {
		SourceID string `json:"source_id"`
	}
	stagePayload struct {
		SourceID  string `json:"source_id"`
		VersionID string `json:"version_id"`
	}
	finalizePayload struct {
		SourceID     string `json:"source_id"`
		GenerationID string `json:"generation_id"`
	}
	maintainPayload struct{}
)

// decodePayload reads a job payload strictly: an unknown field or a type
// mismatch is a permanent failure, because no retry can fix it.
func decodePayload(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return jobs.Permanent(contracts.Wrap(contracts.ErrValidationFailed, err, "ingest: the job payload is malformed"))
	}
	return nil
}

// enqueue queues one job under the initiating scope and service identity.
func (s *Service) enqueue(ctx context.Context, scope contracts.Scope, serviceIdentity, typ string, payload any, key string, runAt time.Time) (jobs.EnqueueResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return jobs.EnqueueResult{}, contracts.Wrap(contracts.ErrInternal, err, "ingest: a job payload cannot be encoded")
	}
	return s.q.Enqueue(ctx, scope, jobs.EnqueueRequest{
		Type: typ, Payload: raw, IdempotencyKey: key, RunAt: runAt, ServiceIdentity: serviceIdentity,
	})
}

// enqueueFresh queues a job under a key that names the work, not the attempt:
// while a job of that key is waiting or running the request coalesces into it,
// and when the job of that key has already ended (failed, cancelled, or
// succeeded without changing what the key names) the work is queued again under
// the next suffix. A plain idempotency key would keep returning the finished job
// and swallow every later request for the same work, for as long as the queue
// retains it.
func (s *Service) enqueueFresh(ctx context.Context, scope contracts.Scope, serviceIdentity, typ string, payload any, baseKey string, runAt time.Time) (jobs.EnqueueResult, error) {
	for n := 0; n < maxFreshKeys; n++ {
		key := baseKey
		if n > 0 {
			key = fmt.Sprintf("%s:n%d", baseKey, n)
		}
		res, err := s.enqueue(ctx, scope, serviceIdentity, typ, payload, key, runAt)
		if err != nil || !res.Duplicate || !res.Job.State.Terminal() {
			return res, err
		}
	}
	return jobs.EnqueueResult{}, contracts.New(contracts.ErrResourceExhausted, "ingest: the same work has been queued and ended too many times; wait for the retained jobs to be pruned")
}

// maxFreshKeys bounds the suffixes enqueueFresh tries.
const maxFreshKeys = 16

// requeueKey is the idempotency key of a stage job; a later attempt at the same
// version gets a new key, because a finished job's key keeps returning that job.
func requeueKey(prefix, versionID string, n int) string {
	if n == 0 {
		return prefix + ":" + versionID
	}
	return fmt.Sprintf("%s:%s:r%d", prefix, versionID, n)
}

func embedKey(versionID string, batch, requeues int) string {
	if requeues == 0 {
		return fmt.Sprintf("ie:%s:%d", versionID, batch)
	}
	return fmt.Sprintf("ie:%s:%d:r%d", versionID, batch, requeues)
}

// Handle attaches the handlers of this package's job types (and of the
// embedding job, when embedding is configured) to the pool. The embedding job
// type is registered by New; the composition root must not also call
// vector.Pipeline.Register for the same queue.
func (s *Service) Handle(pool *jobs.Pool) error {
	for typ, h := range map[string]jobs.Handler{
		JobSync:            s.syncHandler,
		JobParse:           s.stageHandler(laneIngest, s.parseStage),
		JobIndex:           s.stageHandler(laneIngest, s.indexStage),
		JobFinalize:        s.finalizeHandler(laneIngest),
		JobReindexParse:    s.stageHandler(laneReindex, s.parseStage),
		JobReindexIndex:    s.stageHandler(laneReindex, s.indexStage),
		JobReindexFinalize: s.finalizeHandler(laneReindex),
		JobMaintain:        s.maintainHandler,
	} {
		if err := pool.Handle(typ, h); err != nil {
			return err
		}
	}
	if s.pipe != nil {
		return pool.Handle(vector.EmbedJobType, s.embedHandler)
	}
	return nil
}

// stageHandler adapts a stage function to a job handler: it decodes the payload
// and ends the job quietly when the source or the version is gone.
func (s *Service) stageHandler(l lane, stage func(ctx context.Context, job jobs.Job, l lane, p stagePayload) error) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p stagePayload
		if err := decodePayload(job.Payload, &p); err != nil {
			return err
		}
		if p.SourceID == "" || p.VersionID == "" {
			return jobs.Permanent(contracts.New(contracts.ErrValidationFailed, "ingest: the stage job names no source or version"))
		}
		err := stage(ctx, job, l, p)
		if isGone(err) {
			return nil
		}
		return err
	}
}

// pausedError is what a stage returns when disk pressure arrived between the
// lease and the start: retryable, so the job is tried again after its backoff.
func pausedError(v Verdict) error {
	return contracts.New(contracts.ErrDegraded, "ingestion is paused: "+v.Reason).WithDetails("reason", v.Reason)
}

// checkPause returns pausedError while ingestion is paused.
func (s *Service) checkPause(ctx context.Context) error {
	if v := s.pause.Verdict(ctx); v.Paused {
		return pausedError(v)
	}
	return nil
}
