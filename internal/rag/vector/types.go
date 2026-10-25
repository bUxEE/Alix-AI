package vector

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Bounds every provider enforces, so a caller cannot make one request
// allocate or scan without limit and every implementation fails alike.
const (
	// MaxK is the most hits one search returns.
	MaxK = 500
	// MaxBatch is the most items in one Upsert, refs in one Reuse and chunk ids
	// in one DeleteChunks.
	MaxBatch = 1024
	// MaxGenerations is the most generations one search may name.
	MaxGenerations = 64
	// MaxDim is the largest vector dimension accepted.
	MaxDim = 4096
	// MaxIDLen bounds chunk, generation, source and model identifiers, in bytes.
	MaxIDLen = 256
)

// Info describes an implementation, so a caller (and the efficiency review)
// can tell an exact scan from an approximate index.
type Info struct {
	// Name is the stable implementation name ("flat", "sqlitevec").
	Name string
	// Exact is true when a search examines every authorised vector of the
	// requested generations, so recall against brute force is limited only by
	// quantisation. An approximate index sets it false.
	Exact bool
	// Metric is "cosine": scores are cosine similarities in [-1, 1], higher is
	// closer.
	Metric string
	// Storage names the stored vector format ("int8").
	Storage string
	// Reuses is true when Reuse can copy stored vectors. A provider that
	// cannot returns an empty ReuseResult, which is always correct; the
	// contract suite only requires a positive reuse from providers that set it.
	Reuses bool
	// StableTies is true when equal scores are ordered by chunk id ascending,
	// so a search is reproducible down to ties (duplicate chunks have identical
	// vectors). An index that ranks through an internal heap and cannot promise
	// it (sqlite-vec) sets false: the hits are still ordered best first, but
	// which of several equal scores fills the last places is its own business.
	StableTies bool
}

// Item is one vector to store: a chunk of the generation being built.
type Item struct {
	// ChunkID is the chunk's ID (internal/rag/chunk).
	ChunkID string
	// Key is the reuse key of the exact input the vector was computed from
	// (NewInputKey). It is what Reuse looks vectors up by.
	Key InputKey
	// Vector is the model's output. Providers normalise it; a zero or
	// non-finite vector is refused.
	Vector []float32
}

// UpsertBatch stores vectors in one index generation of one source. The
// generation is created by its first batch and then fixed to the source, model
// and dimension it was created with: a later batch that disagrees is refused
// with conflict, never mixed in.
type UpsertBatch struct {
	// Generation is the index generation ID (internal/rag/index owns the
	// lifecycle; the vector index only stores under it).
	Generation string
	// SourceID is the source the generation indexes.
	SourceID string
	// Model is the embedding model's component identifier. Vectors of different
	// models are never comparable, so it is part of every generation.
	Model string
	Items []Item
}

// UpsertResult says what an Upsert did.
type UpsertResult struct {
	// Inserted counts new chunks; Replaced counts chunks whose vector was
	// overwritten.
	Inserted int
	Replaced int
}

// ReuseRef names a chunk whose embedding input has this key.
type ReuseRef struct {
	ChunkID string
	Key     InputKey
}

// ReuseRequest asks the index to fill a generation from vectors it already
// holds for the same scope, model and exact input (master plan 12.3: unchanged
// chunks reuse embeddings). The vectors never leave the index. A provider may
// look only under the SAME chunk id in the scope's other live generations, as
// Flat does (an unchanged chunk keeps its id, and no index on a hash is needed),
// or by key alone; in both cases the key must match, so nothing of another scope
// or model is ever found.
type ReuseRequest struct {
	Generation string
	SourceID   string
	Model      string
	Refs       []ReuseRef
}

// ReuseResult lists the chunks that are now present in the generation with the
// requested key, whether they were copied now or already there. Everything
// else must be embedded. A provider may reuse nothing; that is always correct.
type ReuseResult struct {
	Reused []string
}

// Query is a similarity search.
type Query struct {
	// Vector is the query embedding, produced by Model.
	Vector []float32
	// Model must equal the model of every generation searched: a query vector
	// from one model is never scored against another model's vectors.
	Model string
	// K is how many hits to return, 1 to MaxK.
	K int
	// Generations are the index generations to search: the active generations
	// of the sources the caller is authorised for. Nothing else is read. An
	// empty list returns no hits (fail closed), never "everything".
	Generations []string
}

// Hit is one result, best first. Score is the cosine similarity of the query
// and the stored (quantised) vector.
type Hit struct {
	ChunkID      string
	GenerationID string
	SourceID     string
	Score        float32
}

// SkipReason says why a requested generation contributed nothing.
type SkipReason string

const (
	// SkipUnknownGeneration: the scope holds no such generation (not built yet,
	// already dropped, or not the caller's).
	SkipUnknownGeneration SkipReason = "unknown_generation"
	// SkipModelMismatch: the generation was built with another model.
	SkipModelMismatch SkipReason = "model_mismatch"
	// SkipDimMismatch: the query's dimension differs from the generation's.
	SkipDimMismatch SkipReason = "dim_mismatch"
)

// Skipped is a requested generation that was not searched. A caller labels
// the result reduced_semantic_recall when it is not empty.
type Skipped struct {
	Generation string
	Reason     SkipReason
}

// SearchResult holds the hits, best first, and what was not searched.
type SearchResult struct {
	Hits    []Hit
	Skipped []Skipped
	// Scanned is how many stored vectors were scored.
	Scanned int
}

// GenerationStats describes one generation of the caller's scope.
type GenerationStats struct {
	Generation string
	SourceID   string
	Model      string
	Dim        int
	Vectors    int
}

// Stats describes the caller's own scope only. It never reports another
// scope's size: a count is a side channel (SEC-023).
type Stats struct {
	Info
	// Vectors is the total over Generations.
	Vectors     int
	Generations []GenerationStats
}

// VectorIndexProvider is the replaceable vector index (master plan 6, ADR
// 0011). Every method takes the caller's Scope and refuses to run without a
// valid one (scope_missing / scope_violation); an implementation applies the
// scope as a predicate on what it READS, before any similarity is computed,
// never as a filter over a global top-k (ADR 0011 R1, SEC-021).
//
// Implementations must be safe for concurrent use and honour ctx. They return
// *contracts.Error values and never map a failure to a success-looking result.
// RunProviderSuite in package vectortest is the contract: a new
// implementation proves equivalence by passing it.
type VectorIndexProvider interface {
	// Info describes the implementation.
	Info() Info
	// Upsert stores or replaces vectors. It is atomic per batch and idempotent.
	// It refuses with resource_exhausted past the configured ceiling.
	Upsert(ctx context.Context, scope contracts.Scope, b UpsertBatch) (UpsertResult, error)
	// Reuse copies vectors the scope already holds for the same model and
	// input key into a generation. It never crosses scopes (SEC-023).
	Reuse(ctx context.Context, scope contracts.Scope, r ReuseRequest) (ReuseResult, error)
	// Search returns the K nearest vectors among the requested generations of
	// the caller's scope.
	Search(ctx context.Context, scope contracts.Scope, q Query) (SearchResult, error)
	// DeleteChunks removes chunks from one generation and reports how many
	// were present. A deleted vector is gone from every later search.
	DeleteChunks(ctx context.Context, scope contracts.Scope, generation string, chunkIDs []string) (int, error)
	// DeleteGeneration removes a whole generation and reports how many vectors
	// it held.
	DeleteGeneration(ctx context.Context, scope contracts.Scope, generation string) (int, error)
	// DeleteSource removes every generation of a source (source deletion).
	DeleteSource(ctx context.Context, scope contracts.Scope, sourceID string) (int, error)
	// DeleteScope removes everything the workspace holds (workspace or tenant
	// deletion; a tenant is purged workspace by workspace).
	DeleteScope(ctx context.Context, scope contracts.Scope) (int, error)
	// Stats describes the caller's scope.
	Stats(ctx context.Context, scope contracts.Scope) (Stats, error)
}

// InputKey is the reuse key of one embedding input: the SHA-256 of the
// application, tenant and workspace, the embedding model and the exact text
// embedded. It is deliberately NOT the chunk's ContentHash: that covers the
// text alone and has no scope, so reusing by it would let one tenant learn
// that another tenant holds identical content (SEC-023), and would also hit
// when the embedding input adds a header or a section path the hash never saw.
type InputKey [sha256.Size]byte

// String is the lower-case hex form.
func (k InputKey) String() string { return hex.EncodeToString(k[:]) }

// IsZero reports whether the key was never set.
func (k InputKey) IsZero() bool { return k == InputKey{} }

const inputKeyDomain = "alixai/vector/input-key/v1"

// NewInputKey derives the reuse key. Every field is length-prefixed so
// ("a", "bc") and ("ab", "c") cannot collide. The scope must be valid; its
// application, tenant and workspace are part of the key, so identical text in
// another scope gets an unrelated key and can never be found by this one.
func NewInputKey(scope contracts.Scope, model, input string) (InputKey, error) {
	if err := scope.Validate(); err != nil {
		return InputKey{}, err
	}
	if err := CheckID("model", model); err != nil {
		return InputKey{}, err
	}
	h := sha256.New()
	var lenBuf [binary.MaxVarintLen64]byte
	for _, field := range []string{inputKeyDomain, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, model, input} {
		n := binary.PutUvarint(lenBuf[:], uint64(len(field)))
		h.Write(lenBuf[:n])
		h.Write([]byte(field))
	}
	var k InputKey
	h.Sum(k[:0])
	return k, nil
}

// invalid builds a validation_failed error.
func invalid(format string, args ...any) error {
	return contracts.Newf(contracts.ErrValidationFailed, "vector: "+format, args...)
}

// CheckID validates an identifier the caller supplies (chunk, generation,
// source, model): non-empty, bounded, valid UTF-8, no control characters. The
// identifiers are bound as SQL arguments and never interpolated, so this is
// hygiene and log safety, not injection defence.
func CheckID(what, v string) error {
	switch {
	case v == "":
		return invalid("%s is empty", what)
	case len(v) > MaxIDLen:
		return invalid("%s is longer than %d bytes", what, MaxIDLen)
	case !utf8.ValidString(v):
		return invalid("%s is not valid UTF-8", what)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return invalid("%s contains a control character", what)
		}
	}
	return nil
}

// checkVector refuses an empty, oversized, non-finite or all-zero vector: a
// NaN poisons every score it touches and a zero vector has no direction.
func checkVector(what string, v []float32) error {
	if len(v) == 0 {
		return invalid("%s is empty", what)
	}
	if len(v) > MaxDim {
		return invalid("%s has %d dimensions, over the limit of %d", what, len(v), MaxDim)
	}
	var nonZero bool
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return invalid("%s holds a non-finite value", what)
		}
		if x != 0 {
			nonZero = true
		}
	}
	if !nonZero {
		return invalid("%s is all zeros", what)
	}
	return nil
}

// Validate checks the batch's shape. Scope is checked by the provider.
func (b UpsertBatch) Validate() error {
	for _, f := range []struct{ what, v string }{{"generation", b.Generation}, {"source id", b.SourceID}, {"model", b.Model}} {
		if err := CheckID(f.what, f.v); err != nil {
			return err
		}
	}
	if len(b.Items) == 0 {
		return invalid("upsert has no items")
	}
	if len(b.Items) > MaxBatch {
		return invalid("upsert has %d items, over the limit of %d", len(b.Items), MaxBatch)
	}
	seen := make(map[string]struct{}, len(b.Items))
	dim := len(b.Items[0].Vector)
	for i, it := range b.Items {
		if err := CheckID("chunk id", it.ChunkID); err != nil {
			return err
		}
		if _, dup := seen[it.ChunkID]; dup {
			return invalid("chunk id %q appears twice in one batch", it.ChunkID)
		}
		seen[it.ChunkID] = struct{}{}
		if it.Key.IsZero() {
			return invalid("item %d has no input key", i)
		}
		if err := checkVector("vector", it.Vector); err != nil {
			return err
		}
		if len(it.Vector) != dim {
			return invalid("item %d has %d dimensions, the batch has %d", i, len(it.Vector), dim)
		}
	}
	return nil
}

// Validate checks the request's shape.
func (r ReuseRequest) Validate() error {
	for _, f := range []struct{ what, v string }{{"generation", r.Generation}, {"source id", r.SourceID}, {"model", r.Model}} {
		if err := CheckID(f.what, f.v); err != nil {
			return err
		}
	}
	if len(r.Refs) == 0 {
		return invalid("reuse has no refs")
	}
	if len(r.Refs) > MaxBatch {
		return invalid("reuse has %d refs, over the limit of %d", len(r.Refs), MaxBatch)
	}
	seen := make(map[string]struct{}, len(r.Refs))
	for _, ref := range r.Refs {
		if err := CheckID("chunk id", ref.ChunkID); err != nil {
			return err
		}
		if _, dup := seen[ref.ChunkID]; dup {
			return invalid("chunk id %q appears twice in one reuse request", ref.ChunkID)
		}
		seen[ref.ChunkID] = struct{}{}
		if ref.Key.IsZero() {
			return invalid("chunk %q has no input key", ref.ChunkID)
		}
	}
	return nil
}

// Validate checks the query's shape.
func (q Query) Validate() error {
	if err := checkVector("query vector", q.Vector); err != nil {
		return err
	}
	if err := CheckID("model", q.Model); err != nil {
		return err
	}
	if q.K < 1 || q.K > MaxK {
		return invalid("k %d is outside 1 to %d", q.K, MaxK)
	}
	if len(q.Generations) > MaxGenerations {
		return invalid("%d generations is over the limit of %d", len(q.Generations), MaxGenerations)
	}
	for _, g := range q.Generations {
		if err := CheckID("generation", g); err != nil {
			return err
		}
	}
	return nil
}

// CheckChunkIDs validates the ids of a DeleteChunks call.
func CheckChunkIDs(generation string, ids []string) error {
	if err := CheckID("generation", generation); err != nil {
		return err
	}
	if len(ids) == 0 {
		return invalid("delete has no chunk ids")
	}
	if len(ids) > MaxBatch {
		return invalid("delete has %d chunk ids, over the limit of %d", len(ids), MaxBatch)
	}
	for _, id := range ids {
		if err := CheckID("chunk id", id); err != nil {
			return err
		}
	}
	return nil
}

// Dedupe returns s without repeated entries, order kept.
func Dedupe(s []string) []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for _, v := range s {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// joinPath renders a heading breadcrumb.
func joinPath(path []string) string { return strings.Join(path, " > ") }
