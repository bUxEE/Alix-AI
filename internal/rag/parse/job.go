package parse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// JobType is the job type under which parsing is queued (master plan section 3
// rule 13: parsing is bounded, durable background work).
const JobType = "rag.parse"

// JobSpec registers parsing as ingest-class work (section 17.1: it yields to
// interactive work). MaxAttempts is 2: one retry for a failure that the
// environment caused (the sandbox could not be started, the machine was busy),
// never a third run. Document-caused failures are not retried at all, see
// NewJobHandler.
func JobSpec() jobs.TypeSpec {
	return jobs.TypeSpec{Name: JobType, Class: jobs.ClassIngest, MaxAttempts: 2}
}

// RegisterJob registers JobSpec on q.
func RegisterJob(q *jobs.Queue) error { return q.Register(JobSpec()) }

// JobPayload is a parse job's payload: references, never bytes or credentials
// (payloads are stored in clear text).
type JobPayload struct {
	// SourceID and VersionID identify the immutable source version to parse.
	SourceID  string      `json:"source_id"`
	VersionID string      `json:"version_id"`
	Format    wire.Format `json:"format"`
}

// Marshal is the JSON to enqueue.
func (p JobPayload) Marshal() (json.RawMessage, error) {
	if p.SourceID == "" || p.VersionID == "" || !p.Format.Valid() {
		return nil, contracts.New(contracts.ErrValidationFailed, "parse job: source, version and a supported format are required")
	}
	return json.Marshal(p)
}

// Loader fetches the bytes of a source version for a job. The ingestion pipeline
// supplies it, built on the source provider (which does the authorisation and
// the canonical-path checks); this package never touches a file or the network.
type Loader func(ctx context.Context, scope contracts.Scope, p JobPayload) ([]byte, error)

// Sink receives a finished parse. The ingestion pipeline supplies it: it
// chunks the result (Result.Document) and stores what it needs.
type Sink func(ctx context.Context, scope contracts.Scope, p JobPayload, res *Result) error

// NewJobHandler returns the handler of a parse job: it loads the document under
// the job's scope, parses it in a sandboxed worker, and hands the result to
// sink. Delivery is at least once, so load and sink must be idempotent.
//
// Retry policy, decided here and not in the Runner (which reports what
// happened): a failure caused by the document and the limits (the document is
// malformed, too large, a bomb, or hangs, spins or exhausts memory under the
// configured limits) is deterministic, so it fails the job at once as
// validation_failed with the reason kept; retrying hostile input only repeats
// the cost. A failure caused by the environment (the sandbox could not be
// installed or the worker could not start in time) keeps its retryable code and
// is retried once.
func NewJobHandler(r *Runner, load Loader, sink Sink) jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var p JobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil || p.SourceID == "" || p.VersionID == "" || !p.Format.Valid() {
			return contracts.New(contracts.ErrValidationFailed, "parse job: the payload is not a valid parse job")
		}
		doc, err := load(ctx, job.Scope, p)
		if err != nil {
			return err
		}
		res, err := r.Parse(ctx, p.Format, doc)
		if err != nil {
			return jobError(err)
		}
		return sink(ctx, job.Scope, p, res)
	}
}

// jobError maps a Runner error to what the queue should do about it.
func jobError(err error) error {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return err
	}
	switch reason, _ := ce.Details["reason"].(string); reason {
	case "wall_timeout", "cpu_time", "memory_limit", "killed_by_os":
		out := contracts.Wrap(contracts.ErrValidationFailed, err, fmt.Sprintf("parse: the document exceeds the parsing limits (%s)", reason))
		out.WithDetails("reason", "parse_limit").WithDetails("limit", reason)
		return out
	}
	return err
}
