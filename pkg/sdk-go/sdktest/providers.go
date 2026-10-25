package sdktest

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// The providers and OAuth areas of the fake (External Provider Mode, milestone X): provider connections, the mode,
// the data-egress policy, the egress disclosure, budgets, usage, the kill switch and the browser OAuth flow, so a
// host can test its external-mode flows without a running AlixAI.
//
// The area is OFF until a test calls ConfigureProviders, exactly like a deployment without external mode: every
// route then answers `unavailable` (503), never an empty result. Every route needs an OPERATOR, decided from the
// configuration a test gives (ProvidersConfig.TenantAdmins and DeploymentOperators, by principal id), never from a
// role or capability scope a token asserts; with nobody configured nobody passes. The decision is made before the
// body is read.
//
// The fake keeps no secret: a manual credential is looked at (length, a prefix) and dropped, never stored, echoed or
// logged, and the request log holds no body. Money is integer micro-units.

// ProviderSeed is one provider profile the fake offers.
type ProviderSeed struct {
	// ID is the provider id (for example "openai").
	ID string
	// Model and ModelVersion are the pinned model of the profile.
	Model, ModelVersion string
	// NoticeVersion is the current billing notice version of the profile.
	NoticeVersion string
	// OAuth says whether the provider offers browser OAuth for API access. Without it StartOAuth answers
	// `validation_failed` with details.reason `manual_fallback`.
	OAuth bool
}

// ProvidersConfig switches the providers area on.
type ProvidersConfig struct {
	// TenantAdmins are the principals that administer their own tenant.
	TenantAdmins []string
	// DeploymentOperators are the principals that also manage what concerns more than one tenant: the kill switch,
	// the application and deployment mode and egress policy, provider-wide budgets, the whole application's usage.
	DeploymentOperators []string
	// Providers are the profiles on offer.
	Providers []ProviderSeed
}

// DefaultProvidersConfig is a deployment whose only operator is the default principal ("principal-1"), offering an
// OAuth-capable provider "openai" and a manual-key provider "acme".
func DefaultProvidersConfig() ProvidersConfig {
	return ProvidersConfig{
		DeploymentOperators: []string{"principal-1"},
		Providers: []ProviderSeed{
			{ID: "openai", Model: "gpt-4.1-mini", ModelVersion: "2025-04-14", NoticeVersion: "2026-10-02.1", OAuth: true},
			{ID: "acme", Model: "acme-small", ModelVersion: "2026-01-01", NoticeVersion: "2026-10-02.1"},
		},
	}
}

type providerConn struct {
	state       string
	kind        string
	account     map[string]any
	connectedAt string
	expiresAt   string
	warnings    []string
}

type oauthFlow struct {
	provider, tenant, principal, redirect string
	expires                               time.Time
	used                                  bool
}

type providersState struct {
	on          bool
	unavailable bool
	cfg         ProvidersConfig
	conns       map[string]*providerConn // tenant/provider
	acks        map[string]bool          // tenant/provider
	modes       map[string]string        // level -> mode
	kill        map[string]any
	policies    map[string]map[string]any
	budgets     []map[string]any
	spent       map[string][2]int64 // budget id -> daily, monthly
	flows       map[string]*oauthFlow
	usage       map[string]any
	stateSeq    int
}

// ConfigureProviders switches the providers area on with this configuration (and resets its state).
func (f *Fake) ConfigureProviders(cfg ProvidersConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers = providersState{
		on: true, cfg: cfg,
		conns: map[string]*providerConn{}, acks: map[string]bool{}, modes: map[string]string{},
		kill: map[string]any{"engaged": false}, policies: map[string]map[string]any{},
		spent: map[string][2]int64{}, flows: map[string]*oauthFlow{},
		usage: map[string]any{"records": []any{}, "totals": []any{}},
	}
}

// SetProvidersUnavailable makes every providers and OAuth route answer `unavailable` (503), as a server without
// the providers service does, or restores them.
func (f *Fake) SetProvidersUnavailable(unavailable bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers.unavailable = unavailable
}

// SetProviderBudgetSpend sets the settled spend of a budget's daily and monthly windows (micro-units), so a test
// can see a warning or an exhausted budget.
func (f *Fake) SetProviderBudgetSpend(budgetID string, dailyMicroUnits, monthlyMicroUnits int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers.spent[budgetID] = [2]int64{dailyMicroUnits, monthlyMicroUnits}
}

// SetProviderUsage sets the document the usage export answers with (records and totals).
func (f *Fake) SetProviderUsage(doc map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.providers.usage = doc
}

const (
	profileULID = "01J0000000000000000000000P"
	fakeDeploy  = "dep_fake"
)

func (ps *providersState) seed(id string) (ProviderSeed, bool) {
	for _, s := range ps.cfg.Providers {
		if s.ID == id {
			return s, true
		}
	}
	return ProviderSeed{}, false
}

func providerRef(s ProviderSeed) map[string]any {
	return map[string]any{
		"profile_id":    "pprf_" + profileULID,
		"provider_id":   s.ID,
		"model_id":      s.Model,
		"model_version": s.ModelVersion,
	}
}

// providersBegin is the common head of every providers route: the service is there, the caller is an operator of
// at least the given level, the query holds only the listed names. It runs before any body is read.
func (c *call) providersBegin(deployment bool, allowedQuery ...string) bool {
	ps := &c.f.providers
	if !ps.on || ps.unavailable {
		c.fail(http.StatusServiceUnavailable, contracts.ErrUnavailable, "the server has no providers service")
		return false
	}
	isOp := slices.Contains(ps.cfg.DeploymentOperators, c.claims.PrincipalID)
	isAdmin := isOp || slices.Contains(ps.cfg.TenantAdmins, c.claims.PrincipalID)
	if (deployment && !isOp) || !isAdmin {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the caller is not an operator")
		return false
	}
	for name := range c.r.URL.Query() {
		if !slices.Contains(allowedQuery, name) {
			c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "unknown query parameter")
			return false
		}
	}
	return true
}

func (c *call) isDeploymentOperator() bool {
	return slices.Contains(c.f.providers.cfg.DeploymentOperators, c.claims.PrincipalID)
}

// providersProvider validates the path's provider and finds its seed.
func (c *call) providersProvider() (ProviderSeed, bool) {
	id := c.param("provider")
	seed, ok := c.f.providers.seed(id)
	if !ok {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "no such provider", "reason", "not_found")
		return ProviderSeed{}, false
	}
	return seed, true
}

func (c *call) providersDecode(v any) bool {
	body, ok := c.readBody()
	if !ok {
		return false
	}
	if !strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/json") {
		c.fail(http.StatusUnsupportedMediaType, contracts.ErrValidationFailed, "Content-Type must be application/json")
		return false
	}
	if err := decodeStrict(body, v); err != nil {
		// Never forward the decoder's message: on the credential route it can quote the caller's bytes.
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the request body is not valid for this route")
		return false
	}
	return true
}

func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (f *Fake) connKey(c *call, provider string) string { return c.claims.TenantID + "/" + provider }

// connectionStatus is the ConnectionStatus of one provider for the caller's tenant.
func (c *call) connectionStatus(s ProviderSeed) map[string]any {
	ps := &c.f.providers
	conn := ps.conns[c.f.connKey(c, s.ID)]
	st := map[string]any{
		"as_of": c.f.stamp(c.f.now()), "provider": providerRef(s), "lifecycle": "qualified",
		"state": "not_connected", "kind": nil,
		"account":      map[string]any{"account_id": nil, "organization_id": nil, "project_id": nil},
		"connected_at": nil, "expires_at": nil, "warnings": []string{}, "health": "unknown",
		"last_error_kind": nil, "budget_state": c.budgetStatusFor(s.ID),
		"billing_notice_acknowledged": ps.acks[c.f.connKey(c, s.ID)],
	}
	if conn != nil {
		st["state"], st["kind"], st["account"] = conn.state, conn.kind, conn.account
		st["connected_at"] = conn.connectedAt
		if conn.expiresAt != "" {
			st["expires_at"] = conn.expiresAt
		}
		st["warnings"] = append([]string{}, conn.warnings...)
		if conn.state == "connected" {
			st["health"] = "healthy"
		}
	}
	return st
}

func (c *call) providersList() {
	if !c.providersBegin(false) {
		return
	}
	out := []map[string]any{}
	for _, s := range c.f.providers.cfg.Providers {
		out = append(out, c.connectionStatus(s))
	}
	c.json(http.StatusOK, map[string]any{"providers": out})
}

func (c *call) providersCredential() {
	if !c.providersBegin(false) {
		return
	}
	seed, ok := c.providersProvider()
	if !ok {
		return
	}
	var in struct {
		Key       string   `json:"key"`
		Label     string   `json:"label"`
		ExpiresAt string   `json:"expires_at"`
		Scopes    []string `json:"scopes"`
	}
	if !c.providersDecode(&in) {
		return
	}
	// The key is looked at and dropped. Nothing below keeps, logs or echoes it, and every failure has a fixed message.
	if in.Key == "" || len(in.Key) > 4096 {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the credential was not accepted")
		return
	}
	if strings.HasPrefix(in.Key, "bad") {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the credential was not accepted")
		return
	}
	conn := &providerConn{
		state: "connected", kind: "api_key", connectedAt: c.f.stamp(c.f.now()), expiresAt: in.ExpiresAt,
		account: map[string]any{"account_id": nil, "organization_id": "org-fake", "project_id": "proj-fake"},
	}
	if strings.HasPrefix(in.Key, "broad") {
		// The provider reports a key broader than needed: stored quarantined, with the warning.
		conn.state, conn.warnings = "quarantined", []string{"excessive_permissions_reported"}
	} else {
		conn.warnings = []string{"broader_than_oauth_scopes"}
	}
	c.f.providers.conns[c.f.connKey(c, seed.ID)] = conn
	c.json(http.StatusOK, c.connectionStatus(seed))
}

func (c *call) providersDisconnect() {
	if !c.providersBegin(false) {
		return
	}
	seed, ok := c.providersProvider()
	if !ok {
		return
	}
	delete(c.f.providers.conns, c.f.connKey(c, seed.ID))
	c.json(http.StatusOK, c.connectionStatus(seed))
}

func (c *call) providersAck() {
	if !c.providersBegin(false) {
		return
	}
	seed, ok := c.providersProvider()
	if !ok {
		return
	}
	var in struct {
		NoticeVersion string `json:"notice_version"`
		NotAPIAccess  bool   `json:"consumer_subscription_is_not_api_access"`
		Separate      bool   `json:"separate_api_billing_understood"`
	}
	if !c.providersDecode(&in) {
		return
	}
	if in.NoticeVersion == "" || !in.NotAPIAccess || !in.Separate {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the notice version and both confirmations are required")
		return
	}
	if in.NoticeVersion != seed.NoticeVersion {
		c.fail(http.StatusConflict, contracts.ErrConflict, "the notice version is not the current one", "reason", "stale_notice")
		return
	}
	c.f.providers.acks[c.f.connKey(c, seed.ID)] = true
	c.json(http.StatusOK, map[string]any{
		"ack_id": c.f.typedID("ack"), "profile_id": "pprf_" + profileULID, "provider_id": seed.ID,
		"notice_version":  seed.NoticeVersion,
		"notice_digest":   "sha256:" + strings.Repeat("c", 64),
		"scope":           map[string]any{"deployment_id": fakeDeploy, "application_id": c.claims.ApplicationID, "tenant_id": c.claims.TenantID},
		"confirmations":   map[string]any{"consumer_subscription_is_not_api_access": true, "separate_api_billing_understood": true},
		"acknowledged_by": c.claims.PrincipalID, "acknowledged_at": c.f.stamp(c.f.now()),
	})
}

var fakeRoles = []string{"conversational_controller", "language_realization", "vision", "image_generation", "embedding", "stt", "tts"}

func (c *call) providersDisclosure() {
	if !c.providersBegin(false, "role", "source", "estimated_toks_in", "max_output_toks") {
		return
	}
	seed, ok := c.providersProvider()
	if !ok {
		return
	}
	q := c.r.URL.Query()
	role := "conversational_controller"
	if v := q.Get("role"); v != "" {
		if !slices.Contains(fakeRoles, v) {
			c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "role is not a known role")
			return
		}
		role = v
	}
	for _, name := range []string{"estimated_toks_in", "max_output_toks"} {
		if v := q.Get(name); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
				c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, name+" is not a valid count")
				return
			}
		}
	}
	ps := &c.f.providers
	conn := ps.conns[c.f.connKey(c, seed.ID)]
	effective, _ := c.modeView()["effective"].(string)
	enabled := effective != "local" && conn != nil && conn.state == "connected" && ps.acks[c.f.connKey(c, seed.ID)]
	classes, categories, excerpts := []string{}, []string{}, false
	if enabled {
		classes = []string{"public"}
		categories = []string{"user_message", "structured_state", "deterministic_fact"}
		if len(q["source"]) > 0 {
			categories, excerpts = append(categories, "rag_excerpt"), true
		}
	}
	budget := c.budgetStatusFor(seed.ID)
	if budget == nil {
		budget = "ok"
	}
	c.json(http.StatusOK, map[string]any{
		"generated_at": c.f.stamp(c.f.now()), "provider": providerRef(seed), "role": role,
		"external_enabled": enabled, "data_classes": classes, "data_categories": categories, "rag_excerpts_included": excerpts,
		"regions":        []string{"us"},
		"terms":          map[string]any{"retention": "limited", "retention_days": 30, "training_use": "never", "residency": "selectable"},
		"cost":           map[string]any{"currency": "EUR", "estimate_upper_micro_units": 4200, "budget_state": budget},
		"on_unavailable": map[string]any{"behavior": "fail", "fallback_profile_id": nil},
	})
}

// ---- mode ----

var modeRank = map[string]int{"local": 0, "hybrid": 1, "external": 2}

// effectiveMode is the most restrictive configured level (local when none), and what decided it.
func (c *call) effectiveMode() (mode, decidedBy string) {
	ps := &c.f.providers
	mode, decidedBy = "local", "default"
	first := true
	for _, level := range []string{"deployment", "application", "tenant"} {
		m, ok := ps.modes[c.modeKey(level)]
		if !ok {
			continue
		}
		if first || modeRank[m] < modeRank[mode] {
			mode, decidedBy = m, level
		}
		first = false
	}
	return mode, decidedBy
}

func (c *call) modeKey(level string) string {
	switch level {
	case "application":
		return "application/" + c.claims.ApplicationID
	case "tenant":
		return "tenant/" + c.claims.ApplicationID + "/" + c.claims.TenantID
	}
	return "deployment"
}

func (c *call) modeView() map[string]any {
	ps := &c.f.providers
	levels := map[string]any{}
	for _, level := range []string{"deployment", "application", "tenant"} {
		if m, ok := ps.modes[c.modeKey(level)]; ok {
			levels[level] = m
		}
	}
	configured, decided := c.effectiveMode()
	killed, _ := ps.kill["engaged"].(bool)
	effective := configured
	if killed {
		effective, decided = "local", "kill_switch"
	}
	return map[string]any{"effective": effective, "configured": configured, "decided_by": decided, "kill_switch": killed, "levels": levels}
}

func (c *call) providersGetMode() {
	if !c.providersBegin(false) {
		return
	}
	c.json(http.StatusOK, c.modeView())
}

func (c *call) providersPutMode() {
	if !c.providersBegin(false) {
		return
	}
	var in struct {
		Level string `json:"level"`
		Mode  string `json:"mode"`
	}
	if !c.providersDecode(&in) {
		return
	}
	if !slices.Contains([]string{"deployment", "application", "tenant"}, in.Level) {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "level is deployment, application or tenant")
		return
	}
	if _, ok := modeRank[in.Mode]; !ok {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "mode is local, hybrid or external")
		return
	}
	if in.Level != "tenant" && !c.isDeploymentOperator() {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "this level needs a deployment operator")
		return
	}
	c.f.providers.modes[c.modeKey(in.Level)] = in.Mode
	c.json(http.StatusOK, c.modeView())
}

// ---- egress policy ----

func (c *call) policyKey(level, source string) string {
	switch level {
	case "deployment":
		return "deployment"
	case "application":
		return "application/" + c.claims.ApplicationID
	case "tenant":
		return "tenant/" + c.claims.ApplicationID + "/" + c.claims.TenantID
	case "workspace":
		return "workspace/" + c.claims.ApplicationID + "/" + c.claims.TenantID + "/" + c.claims.WorkspaceID
	}
	return "source/" + c.claims.ApplicationID + "/" + c.claims.TenantID + "/" + c.claims.WorkspaceID + "/" + source
}

var egressLevels = []string{"deployment", "application", "tenant", "workspace", "source"}

var fakeSourceID = regexp.MustCompile(`^src_[0-9A-HJKMNP-TV-Z]{26}$`)

func (c *call) providersGetPolicy() {
	if !c.providersBegin(false, "level", "source_id") {
		return
	}
	q := c.r.URL.Query()
	level, source := q.Get("level"), q.Get("source_id")
	if !slices.Contains(egressLevels, level) || (level == "source") != (source != "") || (source != "" && !fakeSourceID.MatchString(source)) {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "level is required; source_id is for the source level and only for it")
		return
	}
	if p, ok := c.f.providers.policies[c.policyKey(level, source)]; ok {
		c.json(http.StatusOK, map[string]any{"policy": p})
		return
	}
	c.json(http.StatusOK, map[string]any{"policy": nil})
}

// contentEgress is the files or audio setting of a policy update. Enabled is a pointer so that a missing member is
// told apart from false.
type contentEgress struct {
	Enabled  *bool  `json:"enabled"`
	MaxClass string `json:"max_class"`
}

func (c *call) providersPutPolicy() {
	if !c.providersBegin(false) {
		return
	}
	var in struct {
		Level            string           `json:"level"`
		SourceID         string           `json:"source_id"`
		ExpectedRevision int64            `json:"expected_revision"`
		Enabled          bool             `json:"enabled"`
		SourceMode       string           `json:"source_mode"`
		SourceClass      string           `json:"source_class"`
		AllowedProviders []string         `json:"allowed_providers"`
		AllowedModels    []map[string]any `json:"allowed_models"`
		AllowedClasses   []string         `json:"allowed_classes"`
		AllowedRegions   []string         `json:"allowed_regions"`
		Files            *contentEgress   `json:"files"`
		Audio            *contentEgress   `json:"audio"`
	}
	if !c.providersDecode(&in) {
		return
	}
	if !slices.Contains(egressLevels, in.Level) || (in.Level == "source") != (in.SourceID != "") || (in.SourceID != "" && !fakeSourceID.MatchString(in.SourceID)) ||
		!slices.Contains([]string{"local_only", "allow_excerpt", "allow_metadata", "deny"}, in.SourceMode) || in.ExpectedRevision < 0 ||
		(in.Level == "source") != (in.SourceClass != "") {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the policy is not valid")
		return
	}
	// The files and audio settings: {enabled, max_class}, never restricted, and not at source level (uploads and raw
	// audio have no source).
	content := map[string]any{}
	abovePublic := false
	for name, s := range map[string]*contentEgress{"files": in.Files, "audio": in.Audio} {
		if s == nil {
			continue
		}
		if in.Level == "source" || s.Enabled == nil || !slices.Contains([]string{"public", "internal", "confidential"}, s.MaxClass) {
			c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the "+name+" setting is not valid")
			return
		}
		content[name] = map[string]any{"enabled": *s.Enabled, "max_class": s.MaxClass}
		abovePublic = abovePublic || (*s.Enabled && s.MaxClass != "public")
	}
	if (in.Level == "deployment" || in.Level == "application") && !c.isDeploymentOperator() {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "this level needs a deployment operator")
		return
	}
	key := c.policyKey(in.Level, in.SourceID)
	cur, exists := c.f.providers.policies[key]
	revision := int64(0)
	if exists {
		revision, _ = cur["revision"].(int64)
	}
	if in.ExpectedRevision != revision {
		c.fail(http.StatusConflict, contracts.ErrConflict, "the policy changed since it was read", "revision", revision)
		return
	}
	now := c.f.stamp(c.f.now())
	target := map[string]any{"deployment_id": fakeDeploy}
	if in.Level != "deployment" {
		target["application_id"] = c.claims.ApplicationID
	}
	if in.Level == "tenant" || in.Level == "workspace" || in.Level == "source" {
		target["tenant_id"] = c.claims.TenantID
	}
	if in.Level == "workspace" || in.Level == "source" {
		target["workspace_id"] = c.claims.WorkspaceID
	}
	if in.Level == "source" {
		target["source_id"] = in.SourceID
	}
	nonNil := func(v []string) []string {
		if v == nil {
			return []string{}
		}
		return v
	}
	models := in.AllowedModels
	if models == nil {
		models = []map[string]any{}
	}
	policy := map[string]any{
		"policy_id": c.f.typedID("epol"), "revision": revision + 1, "level": in.Level, "target": target, "enabled": in.Enabled,
		"source_mode": in.SourceMode, "allowed_providers": nonNil(in.AllowedProviders), "allowed_models": models,
		"allowed_classes": nonNil(in.AllowedClasses), "allowed_regions": nonNil(in.AllowedRegions),
		"updated_at": now, "updated_by": c.claims.PrincipalID,
	}
	if exists {
		policy["policy_id"] = cur["policy_id"]
	}
	if in.SourceClass != "" {
		policy["source_class"] = in.SourceClass
	}
	maps.Copy(policy, content)
	if slices.Contains(in.AllowedClasses, "internal") || slices.Contains(in.AllowedClasses, "confidential") || abovePublic {
		policy["approval"] = map[string]any{"approved_by": c.claims.PrincipalID, "approved_at": now}
	}
	c.f.providers.policies[key] = policy
	c.json(http.StatusOK, policy)
}

// ---- budgets, usage ----

func (c *call) budgetStatusFor(provider string) any {
	worst := ""
	for _, b := range c.f.providers.budgets {
		subject, _ := b["subject"].(map[string]any)
		if pid, ok := subject["provider_id"].(string); ok && pid != provider {
			continue
		}
		state := c.budgetState(b)["state"].(string)
		if state == "exhausted" || (state == "warning" && worst != "exhausted") || worst == "" {
			worst = state
		}
	}
	if worst == "" {
		return nil
	}
	return worst
}

func (c *call) budgetState(b map[string]any) map[string]any {
	ps := &c.f.providers
	id, _ := b["budget_id"].(string)
	limits, _ := b["limits"].(map[string]any)
	warn, _ := b["warn_at_percent"].(int64)
	spent := ps.spent[id]
	now := c.f.now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	window := func(name string, from, to time.Time, limit, spentNow int64) map[string]any {
		remaining := limit - spentNow
		state := "ok"
		switch {
		case remaining <= 0:
			remaining, state = 0, "exhausted"
		case spentNow*100 >= limit*warn:
			state = "warning"
		}
		return map[string]any{
			"window": name, "starts_at": c.f.stamp(from), "ends_at": c.f.stamp(to), "limit_micro_units": limit,
			"spent_micro_units": spentNow, "reserved_micro_units": 0, "remaining_micro_units": remaining, "state": state,
		}
	}
	daily := window("daily", day, day.AddDate(0, 0, 1), limits["daily_micro_units"].(int64), spent[0])
	monthly := window("monthly", month, month.AddDate(0, 1, 0), limits["monthly_micro_units"].(int64), spent[1])
	worst := "ok"
	for _, w := range []map[string]any{daily, monthly} {
		switch w["state"] {
		case "exhausted":
			worst = "exhausted"
		case "warning":
			if worst != "exhausted" {
				worst = "warning"
			}
		}
	}
	return map[string]any{
		"budget_id": id, "subject": b["subject"], "channel": b["channel"], "currency": b["currency"], "as_of": c.f.stamp(c.f.now()),
		"state": worst, "windows": []any{daily, monthly}, "local_fallback_available": false,
	}
}

func (c *call) providersGetBudgets() {
	if !c.providersBegin(false) {
		return
	}
	budgets, states := []any{}, []any{}
	for _, b := range c.f.providers.budgets {
		budgets = append(budgets, b)
		states = append(states, c.budgetState(b))
	}
	c.json(http.StatusOK, map[string]any{"budgets": budgets, "states": states})
}

func (c *call) providersPutBudget() {
	if !c.providersBegin(false) {
		return
	}
	var in struct {
		BudgetID string         `json:"budget_id"`
		Revision int64          `json:"revision"`
		Subject  map[string]any `json:"subject"`
		Channel  string         `json:"channel"`
		Currency string         `json:"currency"`
		Limits   struct {
			Daily   int64 `json:"daily_micro_units"`
			Monthly int64 `json:"monthly_micro_units"`
		} `json:"limits"`
		WarnAtPercent int64          `json:"warn_at_percent"`
		Quotas        map[string]any `json:"quotas"`
		RateLimits    map[string]any `json:"rate_limits"`
		UpdatedAt     string         `json:"updated_at"`
		UpdatedBy     string         `json:"updated_by"`
	}
	if !c.providersDecode(&in) {
		return
	}
	kind, _ := in.Subject["kind"].(string)
	if !slices.Contains([]string{"provider", "tenant", "workspace", "principal"}, kind) ||
		!slices.Contains([]string{"interactive", "background"}, in.Channel) || in.Currency == "" ||
		in.WarnAtPercent < 1 || in.WarnAtPercent > 99 || in.Limits.Daily < 0 || in.Limits.Monthly < 0 {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the budget is not valid")
		return
	}
	if kind == "provider" && !c.isDeploymentOperator() {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "a provider-wide budget needs a deployment operator")
		return
	}
	doc := map[string]any{
		"subject": in.Subject, "channel": in.Channel, "currency": in.Currency,
		"limits":          map[string]any{"daily_micro_units": in.Limits.Daily, "monthly_micro_units": in.Limits.Monthly},
		"warn_at_percent": in.WarnAtPercent, "updated_at": c.f.stamp(c.f.now()), "updated_by": c.claims.PrincipalID,
	}
	if in.Quotas != nil {
		doc["quotas"] = in.Quotas
	}
	if in.RateLimits != nil {
		doc["rate_limits"] = in.RateLimits
	}
	ps := &c.f.providers
	if in.BudgetID == "" {
		doc["budget_id"], doc["revision"] = c.f.typedID("bud"), int64(1)
		ps.budgets = append(ps.budgets, doc)
		c.json(http.StatusOK, doc)
		return
	}
	for i, b := range ps.budgets {
		if b["budget_id"] != in.BudgetID {
			continue
		}
		if revision, _ := b["revision"].(int64); revision != in.Revision {
			c.fail(http.StatusConflict, contracts.ErrConflict, "the budget changed since it was read", "revision", revision)
			return
		}
		old, _ := json.Marshal(b["subject"])
		cur, _ := json.Marshal(in.Subject)
		if string(old) != string(cur) || b["channel"] != in.Channel || b["currency"] != in.Currency {
			c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the subject, channel and currency of a budget cannot change")
			return
		}
		doc["budget_id"], doc["revision"] = in.BudgetID, in.Revision+1
		ps.budgets[i] = doc
		c.json(http.StatusOK, doc)
		return
	}
	c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "no such budget", "reason", "unknown_budget")
}

func (c *call) providersUsage() {
	if !c.providersBegin(false, "from", "to", "provider", "channel", "scope", "format") {
		return
	}
	q := c.r.URL.Query()
	for _, name := range []string{"from", "to"} {
		if v := q.Get(name); v != "" {
			if _, err := time.Parse(time.RFC3339, v); err != nil {
				c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, name+" is not an RFC 3339 time")
				return
			}
		}
	}
	if v := q.Get("channel"); v != "" && v != "interactive" && v != "background" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "channel is not a known channel")
		return
	}
	if v := q.Get("format"); v != "" && v != "json" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "this fake renders json")
		return
	}
	switch q.Get("scope") {
	case "", "tenant":
	case "application":
		if !c.isDeploymentOperator() {
			c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the whole application needs a deployment operator")
			return
		}
	default:
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "scope is tenant or application")
		return
	}
	c.json(http.StatusOK, c.f.providers.usage)
}

// ---- kill switch ----

func (c *call) providersGetKill() {
	if !c.providersBegin(true) {
		return
	}
	c.json(http.StatusOK, c.f.providers.kill)
}

func (c *call) providersPutKill() {
	if !c.providersBegin(true) {
		return
	}
	var in struct {
		Engaged *bool  `json:"engaged"`
		Reason  string `json:"reason"`
	}
	if !c.providersDecode(&in) {
		return
	}
	if in.Engaged == nil || (in.Reason != "" && !validReasonCode(in.Reason)) {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "engaged is required; reason is a fixed code")
		return
	}
	doc := map[string]any{"engaged": *in.Engaged, "changed_by": c.claims.PrincipalID, "changed_at": c.f.stamp(c.f.now())}
	if in.Reason != "" {
		doc["reason"] = in.Reason
	}
	c.f.providers.kill = doc
	c.json(http.StatusOK, doc)
}

func validReasonCode(s string) bool {
	if s == "" || len(s) > 64 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return false
		}
	}
	return true
}

// ---- OAuth ----

func (c *call) oauthStart() {
	if !c.providersBegin(false) {
		return
	}
	seed, ok := c.providersProvider()
	if !ok {
		return
	}
	var in struct {
		Capabilities  []string `json:"capabilities"`
		RedirectURI   string   `json:"redirect_uri"`
		Reauthorize   string   `json:"reauthorize"`
		SwitchAccount bool     `json:"switch_account"`
		Label         string   `json:"label"`
	}
	if !c.providersDecode(&in) {
		return
	}
	if len(in.Capabilities) == 0 || len(in.Capabilities) > 64 || in.RedirectURI == "" || len(in.RedirectURI) > 512 || len(in.Label) > 128 {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "capabilities and redirect_uri are required and bounded")
		return
	}
	if !seed.OAuth {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "this provider has no OAuth for API access: use a manual credential", "reason", "manual_fallback")
		return
	}
	ps := &c.f.providers
	ps.stateSeq++
	state := "st" + randomSegment(16) + strconv.Itoa(ps.stateSeq)
	expires := c.f.now().Add(10 * time.Minute)
	ps.flows[state] = &oauthFlow{provider: seed.ID, tenant: c.claims.TenantID, principal: c.claims.PrincipalID, redirect: in.RedirectURI, expires: expires}
	authURL := "https://auth.fake.test/authorize?" + url.Values{
		"response_type": {"code"}, "state": {state}, "code_challenge": {"ch" + randomSegment(16)}, "code_challenge_method": {"S256"},
		"nonce": {"no" + randomSegment(8)}, "redirect_uri": {in.RedirectURI},
	}.Encode()
	c.json(http.StatusOK, map[string]any{
		"authorization_url": authURL, "redirect_uri": in.RedirectURI, "expires_at": c.f.stamp(expires),
		"consent": map[string]any{
			"provider_name": seed.ID, "account": map[string]any{"account_id": nil, "organization_id": nil, "project_id": nil},
			"scopes":          []any{map[string]any{"name": "model.request", "description": "Send model requests", "data_access": "the permitted request content"}},
			"data_access":     []string{"the permitted request content"},
			"reauthorization": in.Reauthorize != "", "switch_account": in.SwitchAccount,
		},
	})
}

func (c *call) oauthCallback() {
	if !c.providersBegin(false) {
		return
	}
	var in struct {
		RedirectURI string `json:"redirect_uri"`
		Query       string `json:"query"`
	}
	if !c.providersDecode(&in) {
		return
	}
	if in.RedirectURI == "" || len(in.RedirectURI) > 512 || in.Query == "" || len(in.Query) > 4096 {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "redirect_uri and query are required and bounded")
		return
	}
	q, err := url.ParseQuery(strings.TrimPrefix(in.Query, "?"))
	if err != nil || q.Get("state") == "" {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the callback query has no usable state")
		return
	}
	ps := &c.f.providers
	flow := ps.flows[q.Get("state")]
	// The state is single use: unknown, replayed, expired or issued to another scope is refused, and spent. The message
	// is fixed and holds neither the state nor the code.
	refuse := func() {
		c.fail(http.StatusConflict, contracts.ErrConflict, "the authorization could not be completed")
	}
	if flow == nil {
		refuse()
		return
	}
	delete(ps.flows, q.Get("state"))
	if flow.used || c.f.now().After(flow.expires) || flow.tenant != c.claims.TenantID || flow.principal != c.claims.PrincipalID ||
		flow.redirect != in.RedirectURI || q.Get("error") != "" || q.Get("code") == "" {
		refuse()
		return
	}
	seed, _ := ps.seed(flow.provider)
	ps.conns[c.f.connKey(c, seed.ID)] = &providerConn{
		state: "connected", kind: "oauth", connectedAt: c.f.stamp(c.f.now()), expiresAt: c.f.stamp(c.f.now().Add(time.Hour)),
		account: map[string]any{"account_id": "sub-fake", "organization_id": nil, "project_id": nil},
	}
	c.json(http.StatusOK, c.connectionStatus(seed))
}
