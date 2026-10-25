package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Job is one row of the queue as callers see it.
type Job struct {
	ID string `json:"id"`
	// Scope is the initiating scope, kept for the life of the job. The
	// ScopeChecker re-validates it before every run.
	Scope contracts.Scope `json:"scope"`
	Type  string          `json:"type"`
	Class Class           `json:"class"`
	State State           `json:"state"`
	// Payload is the JSON the producer supplied. It is stored in clear text
	// like every other row: put references in it, never credentials.
	Payload         json.RawMessage `json:"payload"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	ServiceIdentity string          `json:"service_identity,omitempty"`

	// Attempts counts leases so far; a handler sees the current attempt (1
	// for the first run).
	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`

	// RunAt is the earliest time the job may run; a retry sets it to the end
	// of its backoff. Deadline is zero for no deadline.
	RunAt    time.Time `json:"run_at"`
	Deadline time.Time `json:"deadline,omitzero"`

	// CancelRequested is set when a running job was asked to stop.
	CancelRequested bool `json:"cancel_requested,omitempty"`

	// ErrorCode and ErrorMessage describe the last failure. They stay set on a
	// job that is waiting to retry, and are what a failed job ends with.
	ErrorCode    contracts.ErrCode `json:"error_code,omitempty"`
	ErrorMessage string            `json:"error_message,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// TypeSpec registers a job type. The class belongs to the type: producers
// cannot choose it, so nothing can promote its own work into an interactive
// class (SEC-078).
type TypeSpec struct {
	// Name is lower case words joined by dots or underscores, e.g.
	// "rag.parse" or "index_rebuild".
	Name  string
	Class Class
	// MaxAttempts is the most times a job of this type is leased; zero takes
	// Config.MaxAttempts.
	MaxAttempts int
	// BackoffBase and BackoffMax override the config's retry delay for this
	// type; zero takes the config values.
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

var typeNameRE = regexp.MustCompile(`^[a-z][a-z0-9]*([._][a-z0-9]+)*$`)

const (
	maxTypeNameLen    = 64
	maxIdempotencyLen = 255
	maxServiceIDLen   = 128
	maxMessageLen     = 512
)

// EnqueueRequest is what a producer supplies. The scope is a separate
// argument of Enqueue.
type EnqueueRequest struct {
	// Type must have been registered with Queue.Register.
	Type string
	// Payload must be valid JSON of at most Config.MaxPayloadBytes; empty
	// means null.
	Payload json.RawMessage
	// IdempotencyKey, when set, makes the enqueue safe to repeat: a second
	// Enqueue with the same key in the same scope returns the first job. The
	// key is held for as long as the job is retained (Config.RetainFinished).
	IdempotencyKey string
	// RunAt delays the job; zero means now.
	RunAt time.Time
	// Deadline is when the job stops being worth running. A job still waiting
	// at its deadline fails with timeout instead of running, and a running one
	// has its context cancelled at the deadline. Zero means none.
	Deadline time.Time
	// ServiceIdentity declares that the job runs under a service identity
	// instead of the initiating principal's live scope. The declaration alone
	// grants nothing: the Pool's ServiceAuthorizer must accept it (fail
	// closed when there is none).
	ServiceIdentity string
}

// EnqueueResult is what Enqueue returns.
type EnqueueResult struct {
	Job Job
	// Duplicate is true when the idempotency key matched an existing job and
	// nothing was enqueued; Job is that existing job.
	Duplicate bool
}

// LeaseRequest asks for work.
type LeaseRequest struct {
	// Classes are the classes to serve; the most urgent class with ready work
	// is served first. Required.
	Classes []Class
	// Types restricts the job types; empty means every type. A Pool passes
	// the types it has handlers for.
	Types []string
	// Max is how many jobs to lease at most; zero means one.
	Max int
	// TTL is the lease length; zero means Config.LeaseTTL.
	TTL time.Duration
}

// Leased is a job a worker holds. Pass it back to Heartbeat, Complete, Fail
// or Release; the token proves the lease is still the worker's.
type Leased struct {
	Job
	Token string
	Until time.Time
}

// Outcome is where Fail left the job.
type Outcome struct {
	State State
	// RunAt is when a retry becomes eligible (State pending); zero otherwise.
	RunAt time.Time
	// Code is the error code recorded on the job; empty for a cancelled job.
	Code contracts.ErrCode
}

// Reaped counts what a maintenance pass did.
type Reaped struct {
	// Requeued is expired leases returned to pending.
	Requeued int
	// TimedOut is jobs failed with timeout because their deadline passed.
	TimedOut int
	// Exhausted is jobs whose lease expired on their last attempt; they fail
	// with uncertain_outcome because the handler may have done its work.
	Exhausted int
	// Cancelled is expired leases of jobs that had been asked to cancel.
	Cancelled int
}

// Total is the number of jobs the pass touched.
func (r Reaped) Total() int { return r.Requeued + r.TimedOut + r.Exhausted + r.Cancelled }

// Handler runs one job. ctx is cancelled when the job is cancelled, when its
// lease is lost, at its deadline, and when the Pool shuts down; a handler
// must stop promptly then. Returning nil completes the job.
//
// A non-nil error is classified by its contracts code: a retryable code
// (unavailable, degraded, timeout, rate_limited, resource_exhausted) is
// retried with backoff until the type's attempt limit; anything else, a plain
// error included, fails the job at once. Unknown failures are not retried on
// purpose: a handler that is not idempotent must not run twice by accident,
// and one that is can say so by returning a retryable code. Handlers must be
// idempotent anyway: a crash or an expired lease runs a job again.
type Handler func(ctx context.Context, job Job) error

// ScopeChecker re-validates a job's initiating scope just before it runs
// (master plan section 10.2). Implementations answer from authoritative
// identity state (principal revoked, session expired, tenant disabled).
// nil means the scope is still valid. Any other error stops the job; see
// Pool for how its code is read.
type ScopeChecker interface {
	CheckScope(ctx context.Context, scope contracts.Scope) error
}

// ScopeCheckerFunc adapts a function to ScopeChecker.
type ScopeCheckerFunc func(ctx context.Context, scope contracts.Scope) error

// CheckScope calls f.
func (f ScopeCheckerFunc) CheckScope(ctx context.Context, scope contracts.Scope) error {
	return f(ctx, scope)
}

// ServiceAuthorizer decides whether a declared service identity may run a
// job without the initiating principal's live scope. nil means authorised.
// The authority is this deterministic hook, never the job row: a job that
// merely names a service identity is not exempt from the scope check.
type ServiceAuthorizer interface {
	AuthorizeService(ctx context.Context, identity string, job Job) error
}

// ServiceAuthorizerFunc adapts a function to ServiceAuthorizer.
type ServiceAuthorizerFunc func(ctx context.Context, identity string, job Job) error

// AuthorizeService calls f.
func (f ServiceAuthorizerFunc) AuthorizeService(ctx context.Context, identity string, job Job) error {
	return f(ctx, identity, job)
}

// GuardRequest is what an EnqueueGuard sees: the job about to be inserted and
// the live counts, read inside the same transaction that will insert it.
type GuardRequest struct {
	Scope contracts.Scope
	Type  string
	Class Class
	Now   time.Time
	// ClassPending is the number of jobs waiting in the class, and
	// ClassMaxDepth its limit (the queue checks this itself; it is here so a
	// guard can shed load early).
	ClassPending  int
	ClassMaxDepth int
	// TenantPending and TenantRunning are the jobs this application and
	// tenant already have waiting and running in the class, across all of
	// the tenant's workspaces. A per-tenant cap compares their sum.
	TenantPending int
	TenantRunning int
}

// EnqueueGuard is the admission hook ADR 0018 asks for: it runs before the
// insert and may reject with rate_limited or resource_exhausted. A nil error
// admits the job; an error is returned from Enqueue and nothing is inserted
// (a contracts error keeps its code, any other error becomes internal).
//
// The guard runs inside the enqueue's write transaction, so its counts are
// exact, and two rules follow. It must be fast and must not call Queue methods
// that write, nor write to the store at all: the transaction holds the only
// writer connection and such a call would wait for itself forever. And the
// transaction is retried when the database is busy, so the guard can run more
// than once for one Enqueue: it must not consume quota irreversibly. Charge a
// token bucket or a counter after Enqueue returns a new job, not in the guard.
type EnqueueGuard interface {
	AllowEnqueue(ctx context.Context, req GuardRequest) error
}

// EnqueueGuardFunc adapts a function to EnqueueGuard.
type EnqueueGuardFunc func(ctx context.Context, req GuardRequest) error

// AllowEnqueue calls f.
func (f EnqueueGuardFunc) AllowEnqueue(ctx context.Context, req GuardRequest) error {
	return f(ctx, req)
}

// Candidate is a ready job a DequeuePolicy chooses among.
type Candidate struct {
	ID            string
	ApplicationID string
	TenantID      string
	WorkspaceID   string
	Type          string
	Class         Class
	// Seq grows with enqueue order; the default policy takes the lowest.
	Seq        int64
	EnqueuedAt time.Time
	Attempts   int
}

// DequeuePolicy chooses which ready job of a class a worker leases next. The
// default (nil policy) is FIFO inside the class. A policy only orders work
// inside a class: the queue picks the class first (most urgent with ready
// work, after the yield rule), so no policy can reorder priority classes.
//
// ready holds the oldest ready job of each application and tenant in the
// class, oldest first, at most Config.PolicyWindow of them. That is the shape
// a fair policy needs: one candidate per tenant however many each has queued.
// Pick returns the index of the job to lease, or a negative index (or one out
// of range) to lease nothing from this class now. It runs inside the lease
// transaction, so it must be quick and must not call the Queue. A policy
// keeps its own state (for example, the tenant it served last).
type DequeuePolicy interface {
	Pick(ready []Candidate) int
}

// DequeuePolicyFunc adapts a function to DequeuePolicy.
type DequeuePolicyFunc func(ready []Candidate) int

// Pick calls f.
func (f DequeuePolicyFunc) Pick(ready []Candidate) int { return f(ready) }

// Permanent marks err as not worth retrying, whatever its code says. A nil
// err stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

const leaseLostMessage = "job lease lost: the lease expired, the job was cancelled, or it was leased again"

// IsLeaseLost reports whether err says the worker no longer holds the job.
// The worker must stop and not write a result.
func IsLeaseLost(err error) bool {
	return errors.Is(err, contracts.New(contracts.ErrConflict, leaseLostMessage))
}

func leaseLost() error { return contracts.New(contracts.ErrConflict, leaseLostMessage) }
