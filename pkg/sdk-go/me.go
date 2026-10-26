package sdkgo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// MeClient is the per-principal LLM account surface (ADR 0041, 1.2.0).
type MeClient struct{ c *Client }

// Me returns the principal LLM client.
func (c *Client) Me() *MeClient { return &MeClient{c: c} }

// PrincipalCredentialStatus is the safe status of the caller's own provider credential.
type PrincipalCredentialStatus struct {
	ProviderID string         `json:"provider_id"`
	Status     string         `json:"status"`
	Account    map[string]any `json:"account,omitempty"`
}

// PrincipalBillingAck is the body of the principal billing-notice ack.
type PrincipalBillingAck struct {
	NoticeVersion                      string `json:"notice_version"`
	ConsumerSubscriptionIsNotAPIAccess bool   `json:"consumer_subscription_is_not_api_access"`
	SeparateAPIBillingUnderstood       bool   `json:"separate_api_billing_understood"`
}

// PrincipalBillingAckResult is the ack response.
type PrincipalBillingAckResult struct {
	ProviderID     string    `json:"provider_id"`
	NoticeVersion  string    `json:"notice_version"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// PrincipalOAuthStart is the body of POST /v1/me/oauth/{provider}/start.
type PrincipalOAuthStart struct {
	Capabilities []string `json:"capabilities"`
	RedirectURI  string   `json:"redirect_uri"`
	Presentation string   `json:"presentation,omitempty"`
	Callback     string   `json:"callback,omitempty"`
	OpenerOrigin string   `json:"opener_origin,omitempty"`
	Label        string   `json:"label,omitempty"`
}

// PrincipalOAuthStarted is the start response.
type PrincipalOAuthStarted struct {
	AuthorizationURL string         `json:"authorization_url"`
	RedirectURI      string         `json:"redirect_uri"`
	ExpiresAt        time.Time      `json:"expires_at"`
	FlowID           string         `json:"flow_id,omitempty"`
	PollIntervalMS   int            `json:"poll_interval_ms,omitempty"`
	Consent          map[string]any `json:"consent,omitempty"`
}

func (m *MeClient) Catalog(ctx context.Context, options ...CallOption) (*pv.ProviderCatalog, error) {
	req := request{op: opGetProviderCatalog, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkProviderCatalog)
}

func (m *MeClient) LLM(ctx context.Context, options ...CallOption) (*pv.PrincipalLlm, error) {
	req := request{op: opGetPrincipalLlm, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkPrincipalLlm)
}

func (m *MeClient) PutRole(ctx context.Context, role string, body pv.PrincipalLlmBindingUpdate, options ...CallOption) (*pv.PrincipalLlm, error) {
	if err := pathID("role", role); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opPutPrincipalLlmRole, params: map[string]string{"role": role}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkPrincipalLlm)
}

func (m *MeClient) DeleteRole(ctx context.Context, role string, options ...CallOption) (*pv.PrincipalLlm, error) {
	if err := pathID("role", role); err != nil {
		return nil, err
	}
	req := request{op: opDeletePrincipalLlmRole, params: map[string]string{"role": role}, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkPrincipalLlm)
}

func (m *MeClient) SubmitCredential(ctx context.Context, provider, key string, options ...CallOption) (*PrincipalCredentialStatus, error) {
	if err := pathID("provider", provider); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, invalidArgument("key", "key is required")
	}
	raw, _ := json.Marshal(map[string]string{"key": key})
	req := request{op: opPutPrincipalProviderCredential, params: map[string]string{"provider": provider}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkPrincipalCredentialStatus)
}

func (m *MeClient) DeleteCredential(ctx context.Context, provider string, options ...CallOption) (*PrincipalCredentialStatus, error) {
	if err := pathID("provider", provider); err != nil {
		return nil, err
	}
	req := request{op: opDeletePrincipalProviderCredential, params: map[string]string{"provider": provider}, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkPrincipalCredentialStatus)
}

func (m *MeClient) AcknowledgeBillingNotice(ctx context.Context, provider string, body PrincipalBillingAck, options ...CallOption) (*PrincipalBillingAckResult, error) {
	if err := pathID("provider", provider); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opAckPrincipalProviderBillingNotice, params: map[string]string{"provider": provider}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), func(*PrincipalBillingAckResult) error { return nil })
}

func (m *MeClient) StartOAuth(ctx context.Context, provider string, body PrincipalOAuthStart, options ...CallOption) (*PrincipalOAuthStarted, error) {
	if err := pathID("provider", provider); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req := request{op: opStartPrincipalOAuth, params: map[string]string{"provider": provider}, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), func(*PrincipalOAuthStarted) error { return nil })
}

// PrincipalOAuthCallback is the body of POST /v1/me/oauth/callback.
type PrincipalOAuthCallback struct {
	RedirectURI string `json:"redirect_uri"`
	Query       string `json:"query"`
}

// PrincipalOAuthFlowStatus is the body of GET /v1/me/oauth/flows/{flow_id}.
type PrincipalOAuthFlowStatus struct {
	FlowID    string    `json:"flow_id"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CompleteOAuth relays the host callback for a flow the caller started (1.3.0).
func (m *MeClient) CompleteOAuth(ctx context.Context, body PrincipalOAuthCallback, options ...CallOption) (*PrincipalCredentialStatus, error) {
	if body.RedirectURI == "" || len(body.RedirectURI) > 512 {
		return nil, invalidArgument("RedirectURI", "it is required, at most 512 characters")
	}
	if body.Query == "" || len(body.Query) > 4096 {
		return nil, invalidArgument("Query", "it is required, at most 4096 characters")
	}
	raw, err := json.Marshal(map[string]string{"redirect_uri": body.RedirectURI, "query": body.Query})
	if err != nil {
		return nil, err
	}
	req := request{op: opCompletePrincipalOAuth, accept: "application/json", contentType: "application/json", body: raw}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkPrincipalCredentialStatus)
}

// OAuthFlowStatus reads the caller's own popup OAuth flow (1.3.0).
func (m *MeClient) OAuthFlowStatus(ctx context.Context, flowID string, options ...CallOption) (*PrincipalOAuthFlowStatus, error) {
	if flowID == "" {
		return nil, invalidArgument("flowID", "it is required")
	}
	req := request{op: opGetPrincipalOAuthFlow, params: map[string]string{"flow_id": flowID}, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), func(*PrincipalOAuthFlowStatus) error { return nil })
}

// CancelOAuthFlow cancels the caller's own pending popup OAuth flow (1.3.0).
func (m *MeClient) CancelOAuthFlow(ctx context.Context, flowID string, options ...CallOption) error {
	if flowID == "" {
		return invalidArgument("flowID", "it is required")
	}
	req := request{op: opCancelPrincipalOAuthFlow, params: map[string]string{"flow_id": flowID}}
	return doEmpty(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout))
}

// MeUsageQuery selects the caller's usage page.
type MeUsageQuery struct {
	From, To time.Time
	TenantID string
	Limit    int
	Cursor   string
}

// Usage returns the caller's own usage across tenants (GET /v1/me/usage).
func (m *MeClient) Usage(ctx context.Context, query MeUsageQuery, options ...CallOption) (*pv.MeUsage, error) {
	q := url.Values{}
	if !query.From.IsZero() {
		q.Set("from", query.From.UTC().Format(time.RFC3339))
	}
	if !query.To.IsZero() {
		q.Set("to", query.To.UTC().Format(time.RFC3339))
	}
	if query.TenantID != "" {
		q.Set("tenant_id", query.TenantID)
	}
	if query.Cursor != "" {
		q.Set("cursor", query.Cursor)
	}
	if query.Limit != 0 {
		q.Set("limit", fmt.Sprintf("%d", query.Limit))
	}
	req := request{op: opGetMeUsage, accept: "application/json", query: q}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMeUsage)
}

// Budget returns the caller's own principal budgets and states (GET /v1/me/budget).
func (m *MeClient) Budget(ctx context.Context, options ...CallOption) (*pv.MeBudget, error) {
	req := request{op: opGetMeBudget, accept: "application/json"}
	return doJSON(ctx, m.c.t, req, applyOptions(options).deadline(m.c.timeout), checkMeBudget)
}

func checkMeUsage(u *pv.MeUsage) error {
	if u.Groups == nil {
		return fmt.Errorf("me usage: groups must be present")
	}
	return nil
}

func checkMeBudget(b *pv.MeBudget) error {
	if b.Budgets == nil || b.States == nil {
		return fmt.Errorf("me budget: budgets and states must be present")
	}
	return nil
}

func checkProviderCatalog(c *pv.ProviderCatalog) error {
	if c.Entries == nil {
		return fmt.Errorf("provider catalog: entries must be present")
	}
	return nil
}

func checkPrincipalLlm(a *pv.PrincipalLlm) error {
	if a.PrincipalID == "" || a.ApplicationID == "" {
		return fmt.Errorf("principal llm: principal_id and application_id are required")
	}
	return nil
}

func checkPrincipalCredentialStatus(s *PrincipalCredentialStatus) error {
	if s.ProviderID == "" || s.Status == "" {
		return fmt.Errorf("principal credential status: provider_id and status are required")
	}
	return nil
}
