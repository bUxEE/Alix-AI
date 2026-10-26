package retention

import (
	"context"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// JobStatus is the status of an erasure job.
type JobStatus string

// Job statuses.
const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
)

const jobIDPrefix = "erj"

// Job is one asynchronous erasure.
type Job struct {
	JobID       string         `json:"job_id"`
	Status      JobStatus      `json:"status"`
	Level       Level          `json:"level"`
	Scope       PrincipalScope `json:"scope,omitempty"`
	Application string         `json:"application_id"`
	TenantID    string         `json:"tenant_id"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	PrincipalID string         `json:"principal_id,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	Counts      Counts         `json:"counts,omitempty"`
	Error       string         `json:"error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
	targetKey   string
}

// Jobs tracks in-flight and recent erasure jobs (idempotent per target).
type Jobs struct {
	mu       sync.Mutex
	byID     map[string]*Job
	byTarget map[string]string // target key → job id while queued/running
	eng      *Engine
}

// NewJobs builds a job tracker over eng.
func NewJobs(eng *Engine) *Jobs {
	return &Jobs{byID: map[string]*Job{}, byTarget: map[string]string{}, eng: eng}
}

// Start queues or returns the running job for the same target. Returns the job and whether it was newly created.
func (j *Jobs) Start(ctx context.Context, req Request) (*Job, bool, error) {
	if err := req.Validate(); err != nil {
		return nil, false, err
	}
	if j == nil || j.eng == nil {
		return nil, false, contracts.New(contracts.ErrUnavailable, "erasure is not available on this server")
	}
	key := req.TargetKey()
	j.mu.Lock()
	if id, ok := j.byTarget[key]; ok {
		job := j.byID[id]
		j.mu.Unlock()
		return cloneJob(job), false, nil
	}
	now := j.eng.Now()
	job := &Job{
		JobID: ids.New(jobIDPrefix), Status: JobQueued, Level: req.Level, Scope: req.EffectiveScope(),
		Application: req.ApplicationID, TenantID: req.TenantID, WorkspaceID: req.WorkspaceID,
		PrincipalID: req.PrincipalID, Reason: req.Reason, CreatedAt: now, targetKey: key,
	}
	j.byID[job.JobID] = job
	j.byTarget[key] = job.JobID
	j.mu.Unlock()

	go j.run(context.WithoutCancel(ctx), job.JobID, req)
	return cloneJob(job), true, nil
}

// Get returns a job by id. Unknown → not_found.
func (j *Jobs) Get(_ context.Context, jobID string) (*Job, error) {
	if j == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "erasure is not available on this server")
	}
	if !ids.Valid(jobID, jobIDPrefix) {
		return nil, contracts.New(contracts.ErrValidationFailed, "the erasure job id is malformed")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[jobID]
	if !ok {
		return nil, contracts.New(contracts.ErrValidationFailed, "unknown erasure job").WithDetails("reason", "not_found")
	}
	return cloneJob(job), nil
}

func (j *Jobs) run(ctx context.Context, jobID string, req Request) {
	j.mu.Lock()
	job := j.byID[jobID]
	if job == nil {
		j.mu.Unlock()
		return
	}
	job.Status = JobRunning
	j.mu.Unlock()

	counts, err := j.eng.Erase(ctx, req)

	j.mu.Lock()
	defer j.mu.Unlock()
	job = j.byID[jobID]
	if job == nil {
		return
	}
	now := j.eng.Now()
	job.FinishedAt = &now
	job.Counts = counts
	delete(j.byTarget, job.targetKey)
	if err != nil {
		job.Status = JobFailed
		job.Error = err.Error()
		return
	}
	job.Status = JobCompleted
}

func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	out := *j
	if j.FinishedAt != nil {
		t := *j.FinishedAt
		out.FinishedAt = &t
	}
	return &out
}
