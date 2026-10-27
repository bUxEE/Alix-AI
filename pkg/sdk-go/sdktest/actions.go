package sdktest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

const preparationTTL = 10 * time.Minute

// roundTrip builds a generated type from a plain document, so a document the schema would refuse (an enum
// value that does not exist, a malformed member) fails in the fake instead of reaching a test.
func roundTrip[T any](doc map[string]any) T {
	data, err := json.Marshal(doc)
	if err != nil {
		panic("sdktest: " + err.Error())
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		panic("sdktest: the fake built an invalid document: " + err.Error())
	}
	return v
}

// prepare registers a preparation at risk level 2 with its confirmation secret.
func (f *Fake) prepare(claims Claims, in PrepareInput) *prep {
	now := f.now()
	summary := in.Summary
	if summary == "" {
		summary = "Do " + in.CapabilityID
	}
	action := roundTrip[actions.PreparedAction](map[string]any{
		"preparation_id": f.typedID("prep"),
		"capability_id":  in.CapabilityID,
		"state":          "prepared",
		"summary":        summary,
		"effects": []any{map[string]any{
			"operation": "other", "entity_type": "record", "description": summary, "changes": []any{}, "external": false,
		}},
		"risk_level":              2,
		"data_versions":           []any{map[string]any{"entity_type": "record", "entity_id": "record-1", "version": "v1"}},
		"prepared_at":             f.stamp(now),
		"expires_at":              f.stamp(now.Add(preparationTTL)),
		"invalidation_conditions": []any{"data_version_changed", "session_ended"},
		"confirmation": map[string]any{
			"mode": "explicit", "bindings": []any{"preparation_id", "principal", "session", "data_versions"},
		},
		"idempotency_key": randomSegment(24),
	})
	p := &prep{claims: claims, action: action, expires: now.Add(preparationTTL), secret: "cfm_" + randomSegment(32), commit: in.Commit}
	f.preps[string(action.PreparationID)] = p
	return p
}

// confirmCheck is the coordinator's Confirm: it uses the secret up when it is the right one. A zero code is
// success; otherwise the typed failure and its HTTP status.
func (f *Fake) confirmCheck(p *prep, token string) (contracts.ErrCode, string, int) {
	if p.used {
		return contracts.ErrReplayed, "the confirmation was already used", http.StatusConflict
	}
	switch f.effective(p).State {
	case actions.PreparedActionStatePrepared:
	case actions.PreparedActionStateExpired:
		return contracts.ErrExpired, "the preparation has expired; prepare the action again", http.StatusConflict
	case actions.PreparedActionStateInvalidated:
		return contracts.ErrStale, "the data the preparation relied on changed; prepare the action again", http.StatusConflict
	default:
		return contracts.ErrConflict, "the preparation is " + string(p.action.State), http.StatusConflict
	}
	if token != p.secret {
		return contracts.ErrPermissionDenied, "the confirmation does not belong to this preparation", http.StatusForbidden
	}
	p.used = true
	return "", "", 0
}

// commit commits a confirmed preparation: it records the receipt and moves the state.
func (f *Fake) commit(p *prep) (actions.ActionReceipt, *contracts.Error) {
	plan := p.commit
	status := plan.Status
	if status == "" {
		status = "committed"
	}
	now := f.stamp(f.now())
	doc := map[string]any{
		"receipt_id":     f.typedID("rcpt"),
		"preparation_id": string(p.action.PreparationID),
		"capability_id":  string(p.action.CapabilityID),
		"status":         status,
		"records":        []any{map[string]any{"entity_type": "record", "entity_id": "record-1"}},
		"facts":          []any{},
		"errors":         []any{},
		"audit_ref":      f.typedID("aud"),
		"issued_at":      now,
	}
	var failure *contracts.Error
	switch status {
	case "committed":
		p.action.State = actions.PreparedActionStateCommitted
	case "queued":
		p.action.State = actions.PreparedActionStateCommitted
		doc["follow_up"] = map[string]any{"reference": f.typedID("job"), "state": "queued", "as_of": now}
	default:
		code := plan.ErrorCode
		if code == "" {
			code = map[string]contracts.ErrCode{"partial": contracts.ErrPartialFailure, "uncertain": contracts.ErrUncertainOutcome}[status]
		}
		if code == "" {
			code = contracts.ErrConflict
		}
		msg := plan.ErrorMessage
		if msg == "" {
			msg = "the commit ended " + status
		}
		doc["errors"] = []any{map[string]any{"code": string(code), "message": msg}}
		failure = contracts.New(code, msg)
		if status == "rejected" {
			doc["records"] = []any{}
			p.action.State = actions.PreparedActionStateRejected
		} else {
			p.action.State = actions.PreparedActionStateUncertain
		}
	}
	receipt := roundTrip[actions.ActionReceipt](doc)
	p.receipt = &receipt
	return receipt, failure
}

// prep returns the preparation the path names, which must be the caller's.
func (c *call) prep() (*prep, bool) {
	p := c.f.preps[c.param("preparation_id")]
	if p == nil || !sameScope(p.claims, c.claims) {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "no such preparation in this scope")
		return nil, false
	}
	return p, true
}

func (c *call) getAction() {
	if p, ok := c.prep(); ok {
		c.json(http.StatusOK, c.f.effective(p))
	}
}

func (c *call) confirmAction() {
	p, ok := c.prep()
	if !ok {
		return
	}
	raw, ok := c.readBody()
	if !ok {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !c.closedObject(raw, &body) {
		return
	}
	if !confirmationToken.MatchString(body.Token) {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the confirmation token is malformed")
		return
	}
	if code, msg, status := c.f.confirmCheck(p, body.Token); code != "" {
		c.fail(status, code, msg)
		return
	}
	receipt, failure := c.f.commit(p)
	doc := map[string]any{"preparation": p.action, "receipt": receipt}
	switch {
	case failure == nil:
		c.json(http.StatusOK, doc)
	default:
		doc["error"] = errorBody(failure.Code, failure.Message, nil)
		if receipt.Status == "rejected" {
			c.json(http.StatusConflict, doc)
		} else {
			c.json(http.StatusInternalServerError, doc)
		}
	}
}

func (c *call) rejectAction() {
	p, ok := c.prep()
	if !ok {
		return
	}
	switch c.f.effective(p).State {
	case actions.PreparedActionStatePrepared:
		p.action.State = actions.PreparedActionStateRejected
	case actions.PreparedActionStateRejected:
	default:
		c.fail(http.StatusConflict, contracts.ErrConflict, "the preparation is "+string(c.f.effective(p).State))
		return
	}
	c.json(http.StatusOK, p.action)
}

func (c *call) getReceipt() {
	p, ok := c.prep()
	if !ok {
		return
	}
	if p.receipt == nil {
		c.fail(http.StatusConflict, contracts.ErrConflict, "the preparation has no outcome yet")
		return
	}
	c.json(http.StatusOK, p.receipt)
}

func (c *call) reportStepUpOutcome() {
	p, ok := c.prep()
	if !ok {
		return
	}
	raw, ok := c.readBody()
	if !ok {
		return
	}
	var body struct {
		Outcome   string `json:"outcome"`
		Reference string `json:"reference"`
	}
	if !c.closedObject(raw, &body) {
		return
	}
	switch body.Outcome {
	case "completed", "declined", "failed":
	default:
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "outcome must be completed, declined or failed")
		return
	}
	if p.receipt != nil {
		status := http.StatusOK
		if p.receipt.Status == "rejected" {
			status = http.StatusConflict
		}
		c.json(status, p.receipt)
		return
	}
	switch c.f.effective(p).State {
	case actions.PreparedActionStatePrepared:
	default:
		c.fail(http.StatusConflict, contracts.ErrConflict, "the preparation is "+string(c.f.effective(p).State))
		return
	}
	switch body.Outcome {
	case "completed":
		receipt, failure := c.f.commit(p)
		if failure != nil {
			c.json(http.StatusConflict, receipt)
			return
		}
		c.json(http.StatusOK, receipt)
	default:
		p.commit = CommitPlan{Status: "rejected", ErrorCode: contracts.ErrConflict, ErrorMessage: "step-up " + body.Outcome + " in the host interface"}
		receipt, _ := c.f.commit(p)
		c.json(http.StatusConflict, receipt)
	}
}

// ---- applications, capabilities, probes ----

type pkg struct {
	hash      string
	version   string
	installed time.Time
}

const seedHash = "0000000000000000000000000000000000000000000000000000000000000000"

func (c *call) packageDocs() []any {
	f := c.f
	out := []any{}
	if f.appState == "not_installed" {
		return out
	}
	doc := func(hash, version, state string, at time.Time) map[string]any {
		return map[string]any{
			"manifest_hash": hash, "package_version": version, "state": state, "warnings": 0,
			"installed_at": f.stamp(at), "status_changed_at": f.stamp(at),
		}
	}
	out = append(out, doc(seedHash, "1.0.0", f.appState, f.now().Add(-time.Hour)))
	for _, h := range f.pkgOrder {
		out = append(out, doc(h, f.packages[h].version, "quarantined", f.packages[h].installed))
	}
	return out
}

func (c *call) registerApplication() {
	if !contains(c.claims.CapabilityScopes, "applications.register") {
		c.fail(http.StatusForbidden, contracts.ErrPermissionDenied, "the token lacks the applications.register scope")
		return
	}
	if !strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/gzip") {
		c.fail(http.StatusUnsupportedMediaType, contracts.ErrValidationFailed, "a package is sent as application/gzip")
		return
	}
	raw, ok := c.readBody()
	if !ok {
		return
	}
	if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the body is not a gzip archive")
		return
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	status := http.StatusOK
	created := false
	if c.f.packages[hash] == nil {
		c.f.packages[hash] = &pkg{hash: hash, version: "1.0.0", installed: c.f.now()}
		c.f.pkgOrder = append(c.f.pkgOrder, hash)
		status, created = http.StatusCreated, true
	}
	p := c.f.packages[hash]
	c.json(status, map[string]any{
		"application_id": c.claims.ApplicationID,
		"package": map[string]any{
			"manifest_hash": hash, "package_version": p.version, "state": "quarantined", "warnings": 0,
			"installed_at": c.f.stamp(p.installed), "status_changed_at": c.f.stamp(p.installed),
		},
		"created": created, "findings": []any{},
	})
}

func (c *call) getApplication() {
	if c.param("application_id") != c.claims.ApplicationID {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the path names another application than the token's")
		return
	}
	doc := map[string]any{"application_id": c.claims.ApplicationID, "status": c.f.appState, "packages": c.packageDocs()}
	if c.f.appState == "active" {
		doc["health"] = map[string]any{"status": "ok", "application_version": "1.0.0"}
	}
	c.json(http.StatusOK, doc)
}

func (c *call) listCapabilities() {
	all := []map[string]any{
		{"id": "demo.lookup", "title": "Look up a booking", "risk_level": 0, "requires_confirmation": false, "requires_step_up": false, "fields": []any{"booking_ref"}},
		{"id": "demo.book", "title": "Book a slot", "description": "Books a slot for the caller.", "risk_level": 2, "requires_confirmation": true, "requires_step_up": false, "fields": []any{"slot_date", "slot_start"}},
		{"id": "demo.close_account", "title": "Close the account", "risk_level": 3, "requires_confirmation": true, "requires_step_up": true, "fields": []any{}},
	}
	out := []any{}
	for _, entry := range all {
		if contains(c.claims.CapabilityScopes, entry["id"].(string)) {
			out = append(out, entry)
		}
	}
	c.json(http.StatusOK, map[string]any{"capabilities": out})
}

func (c *call) readinessDoc() map[string]any {
	status, ready := "ok", true
	check := map[string]any{"name": "store", "ok": true}
	role := map[string]any{"role": "controller", "configured": true, "status": "ok"}
	if !c.f.ready {
		status, ready = "unavailable", false
		check = map[string]any{"name": "store", "ok": false, "reason": "the fake is set not ready"}
		role = map[string]any{"role": "controller", "configured": true, "status": "unavailable", "reason": "the fake is set not ready"}
	}
	return map[string]any{"status": status, "ready": ready, "roles": []any{role}, "checks": []any{check}}
}

func (c *call) liveness() { c.json(http.StatusOK, map[string]any{"status": "ok"}) }

func (c *call) readiness() { c.report(c.readinessDoc()) }

func (c *call) health() {
	doc := c.readinessDoc()
	doc["application"] = map[string]any{"application_id": c.claims.ApplicationID, "status": c.f.appState}
	c.report(doc)
}

// report answers a readiness report: 200 when ready, 503 when not (the same document).
func (c *call) report(doc map[string]any) {
	if doc["ready"] == false {
		c.w.Header().Set("Retry-After", "1")
		c.json(http.StatusServiceUnavailable, doc)
		return
	}
	c.json(http.StatusOK, doc)
}
