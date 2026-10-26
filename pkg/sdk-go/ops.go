package sdkgo

import (
	"context"
	"encoding/json"
	"time"
)

// OpsClient is the operator operations surface (erasure, 1.3.0).
type OpsClient struct{ c *Client }

// Ops returns the operator client.
func (c *Client) Ops() *OpsClient { return &OpsClient{c: c} }

// ErasureRequest is the body of POST /v1/ops/erasure.
type ErasureRequest struct {
	Level       string `json:"level"`
	Scope       string `json:"scope,omitempty"` // tenant (default) or application; only with level principal
	TenantID    string `json:"tenant_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// ErasureJobAccepted is the 202 body.
type ErasureJobAccepted struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

// ErasureJobStatus is the body of GET /v1/ops/erasure/{job_id}.
type ErasureJobStatus struct {
	JobID       string         `json:"job_id"`
	Status      string         `json:"status"`
	Level       string         `json:"level"`
	Scope       string         `json:"scope,omitempty"`
	TenantID    string         `json:"tenant_id"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	PrincipalID string         `json:"principal_id,omitempty"`
	Counts      map[string]any `json:"counts,omitempty"`
	Error       string         `json:"error,omitempty"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
}

// StartErasure starts a GDPR erasure job (deployment operator or tenant admin).
func (o *OpsClient) StartErasure(ctx context.Context, body ErasureRequest, options ...CallOption) (*ErasureJobAccepted, error) {
	if body.Level != "principal" && body.Level != "tenant" && body.Level != "workspace" {
		return nil, invalidArgument("Level", "it must be principal, tenant or workspace")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opStartErasure, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, o.c.t, req, applyOptions(options).deadline(o.c.timeout), func(a *ErasureJobAccepted) error {
		if a.JobID == "" || a.Status == "" {
			return invalidArgument("response", "job_id and status are required")
		}
		return nil
	})
}

// ErasureStatus reads an erasure job.
func (o *OpsClient) ErasureStatus(ctx context.Context, jobID string, options ...CallOption) (*ErasureJobStatus, error) {
	if jobID == "" {
		return nil, invalidArgument("jobID", "it is required")
	}
	req := request{op: opGetErasureJob, params: map[string]string{"job_id": jobID}, accept: "application/json"}
	return doJSON(ctx, o.c.t, req, applyOptions(options).deadline(o.c.timeout), func(*ErasureJobStatus) error { return nil })
}
