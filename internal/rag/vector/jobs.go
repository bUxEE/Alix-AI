package vector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// EmbedJobType is the job type embedding work is queued under (master plan 3
// rule 13, 17.1: embeddings are ingestion work, priority class 5).
const EmbedJobType = "rag.vector.embed"

// EmbedJobSpec is the registration of the embedding job type. The class is
// fixed here, by the type, so a producer cannot promote its own work: embedding
// is ClassIngest and yields to interactive work like the rest of ingestion.
func EmbedJobSpec() jobs.TypeSpec {
	return jobs.TypeSpec{Name: EmbedJobType, Class: jobs.ClassIngest}
}

// EnqueueEmbed queues one embedding job for the chunks of req. The payload
// carries ids only. Repeating the call for the same generation and chunks
// returns the first job (the idempotency key is their digest), so a producer
// that retries after a crash does not queue the work twice.
func EnqueueEmbed(ctx context.Context, q *jobs.Queue, scope contracts.Scope, req EmbedRequest) (jobs.EnqueueResult, error) {
	if err := req.Validate(); err != nil {
		return jobs.EnqueueResult{}, err
	}
	// Canonical form: the order of the ids carries no meaning, and the queue
	// treats the same key with a different payload as a conflict.
	req.ChunkIDs = Dedupe(req.ChunkIDs)
	sort.Strings(req.ChunkIDs)
	payload, err := json.Marshal(req)
	if err != nil {
		return jobs.EnqueueResult{}, contracts.Wrap(contracts.ErrInternal, err, "vector: embed payload cannot be encoded")
	}
	return q.Enqueue(ctx, scope, jobs.EnqueueRequest{
		Type:           EmbedJobType,
		Payload:        payload,
		IdempotencyKey: embedKey(req),
	})
}

// embedKey digests the generation, source and the (canonical) chunk ids into an
// idempotency key.
func embedKey(req EmbedRequest) string {
	h := sha256.New()
	for _, f := range append([]string{req.Generation, req.SourceID}, req.ChunkIDs...) {
		var n [8]byte
		l := uint64(len(f))
		for i := range n {
			n[i] = byte(l >> (8 * i))
		}
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return "embed:" + hex.EncodeToString(h.Sum(nil))
}

// Handler is the jobs.Handler of embedding jobs. It runs under the job's own
// scope (the Pool re-validated it just before calling), decodes the payload
// strictly and runs Embed. A malformed payload fails the job permanently: no
// retry can fix it. Every other failure keeps the code the embedder, the
// source or the index gave it, so a model that is down retries with backoff and
// a configuration conflict does not.
func (p *Pipeline) Handler() jobs.Handler {
	return func(ctx context.Context, job jobs.Job) error {
		var req EmbedRequest
		dec := json.NewDecoder(bytes.NewReader(job.Payload))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return jobs.Permanent(contracts.Wrap(contracts.ErrValidationFailed, err, "vector: embed job payload is malformed"))
		}
		if err := req.Validate(); err != nil {
			return jobs.Permanent(err)
		}
		st, err := p.Embed(ctx, job.Scope, req)
		if err != nil {
			return err
		}
		p.log.Info("embedding job done", "job", job.ID, "generation", req.Generation,
			"n_reused", st.Reused, "n_embedded", st.Embedded, "n_missing", st.Missing, "tok_in", st.InputToks)
		return nil
	}
}

// Register registers the embedding job type on the queue and its handler on the
// pool. Call it once while wiring the process that runs ingestion.
func (p *Pipeline) Register(q *jobs.Queue, pool *jobs.Pool) error {
	if err := q.Register(EmbedJobSpec()); err != nil {
		return err
	}
	return pool.Handle(EmbedJobType, p.Handler())
}
