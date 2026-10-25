package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The actions tables carry the scope columns (master plan 10.2).
func TestActionsTablesAreScoped(t *testing.T) {
	reg := store.NewRegistry()
	if err := reg.Register("actions", migrations); err != nil {
		t.Fatal(err)
	}
	storetest.AssertScopedTables(t, storetest.Open(t, reg))
}

// A receipt is immutable once issued.
func TestReceiptsAreImmutable(t *testing.T) {
	e := newEnv(t)
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Alpha")
	if _, err := e.co.Commit(ctxb(), sc, id(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(ctxb(), `UPDATE action_receipts SET status = 'rejected'`); err == nil {
		t.Error("a receipt could be edited")
	}
}

func TestConfirmationTokenNeverPrintsItsSecret(t *testing.T) {
	tok, err := newConfirmationToken()
	if err != nil {
		t.Fatal(err)
	}
	secret := tok.Reveal()
	if !tokenRE.MatchString(secret) {
		t.Fatalf("token %q has the wrong shape", secret)
	}
	b, _ := json.Marshal(map[string]any{"a": tok, "b": &tok, "c": []ConfirmationToken{tok}})
	var buf strings.Builder
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "token", tok, "other", tok)
	for what, got := range map[string]string{
		"json":    string(b),
		"%v":      fmt.Sprintf("%v", tok),
		"%+v":     fmt.Sprintf("%+v", struct{ T ConfirmationToken }{tok}),
		"%#v":     fmt.Sprintf("%#v", tok),
		"%s":      fmt.Sprintf("%s", tok),
		"slog":    buf.String(),
		"error":   fmt.Errorf("wrapped: %v", tok).Error(),
		"details": fmt.Sprint(contracts.New(contracts.ErrInternal, "x").WithDetails("t", tok).Details),
	} {
		if strings.Contains(got, secret) {
			t.Errorf("%s leaks the secret: %s", what, got)
		}
	}
	other, _ := newConfirmationToken()
	if other.Reveal() == secret {
		t.Error("two tokens are equal")
	}
	if _, err := ParseConfirmationToken(secret); err != nil {
		t.Errorf("a token does not parse back: %v", err)
	}
	for _, bad := range []string{"", "cfm_", "cfm_short", strings.Repeat("A", 47), "cfm_" + strings.Repeat("!", 43)} {
		if _, err := ParseConfirmationToken(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestConfirmationMACBindsAllFour(t *testing.T) {
	tok, _ := newConfirmationToken()
	other, _ := newConfirmationToken()
	base := confirmMAC(tok, "prep_1", "user-1", "ses-1", "digest")
	for name, got := range map[string]string{
		"token":       confirmMAC(other, "prep_1", "user-1", "ses-1", "digest"),
		"preparation": confirmMAC(tok, "prep_2", "user-1", "ses-1", "digest"),
		"principal":   confirmMAC(tok, "prep_1", "user-2", "ses-1", "digest"),
		"session":     confirmMAC(tok, "prep_1", "user-1", "ses-2", "digest"),
		"versions":    confirmMAC(tok, "prep_1", "user-1", "ses-1", "other"),
		// Field boundaries are part of the input: moving a character between two
		// fields is a different binding.
		"boundary": confirmMAC(tok, "prep_1u", "ser-1", "ses-1", "digest"),
	} {
		if got == base {
			t.Errorf("changing the %s does not change the binding", name)
		}
	}
	if confirmMAC(tok, "prep_1", "user-1", "ses-1", "digest") != base {
		t.Error("the binding is not deterministic")
	}
}

func TestDataDigestIgnoresOrderNotContent(t *testing.T) {
	a := act.DataVersion{EntityType: "a", EntityID: "1", Version: "v1"}
	b := act.DataVersion{EntityType: "b", EntityID: "2", Version: "v2"}
	if dataDigest([]act.DataVersion{a, b}) != dataDigest([]act.DataVersion{b, a}) {
		t.Error("order changes the digest")
	}
	b2 := b
	b2.Version = "v3"
	if dataDigest([]act.DataVersion{a, b}) == dataDigest([]act.DataVersion{a, b2}) {
		t.Error("a changed version does not change the digest")
	}
}

func TestNewRequiresEveryDependency(t *testing.T) {
	e := newEnv(t)
	good := Deps{DB: e.db, Policy: e.gw, Audit: e.alog, Packages: e.co.packages, Adapters: e.co.adapters}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Deps){
		"db":       func(d *Deps) { d.DB = nil },
		"policy":   func(d *Deps) { d.Policy = nil },
		"audit":    func(d *Deps) { d.Audit = nil },
		"packages": func(d *Deps) { d.Packages = nil },
		"adapters": func(d *Deps) { d.Adapters = nil },
	} {
		d := good
		mut(&d)
		if _, err := New(d); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("without %s: %v", name, err)
		}
	}
	for name, cfg := range map[string]Config{
		"ttl too short":  {MaxPreparationTTL: time.Millisecond},
		"ttl too long":   {MaxPreparationTTL: 48 * time.Hour},
		"negative ttl":   {MaxPreparationTTL: -time.Minute},
		"settle too low": {SettleTimeout: time.Millisecond},
		"stale too low":  {CommitStaleAfter: time.Millisecond},
	} {
		d := good
		d.Config = cfg
		if _, err := New(d); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRequestNeedsLiveContextAndScope(t *testing.T) {
	e := newEnv(t)
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Alpha")
	if _, err := e.co.Get(nil, sc, id(p)); contracts.CodeOf(err) != contracts.ErrInternal { //nolint:staticcheck // a nil context is the case under test
		t.Errorf("nil context: %v", err)
	}
	if _, err := e.co.Get(ctxb(), contracts.Scope{}, id(p)); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Errorf("zero scope: %v", err)
	}
	wild := sc
	wild.TenantID = "*"
	if _, err := e.co.Get(ctxb(), wild, id(p)); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Errorf("wildcard tenant: %v", err)
	}
	if _, err := e.co.Get(ctxb(), sc, "prep_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D"); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Errorf("unknown preparation: %v", err)
	}
	// A preparation that exists in another tenant answers exactly like one that does not exist.
	other := sc
	other.TenantID = tenantB
	_, errOther := e.co.Get(ctxb(), other, id(p))
	_, errNone := e.co.Get(ctxb(), other, "prep_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D")
	if errOther == nil || errNone == nil || errOther.Error() != errNone.Error() {
		t.Errorf("another tenant's preparation is distinguishable: %v vs %v", errOther, errNone)
	}
	// Both say not_found, which the session layer tells from a continuity refusal:
	// a preparation retention removed is no longer there to void.
	var ce *contracts.Error
	if !errors.As(errNone, &ce) || ce.Details["reason"] != "not_found" || !errors.As(errOther, &ce) || ce.Details["reason"] != "not_found" {
		t.Errorf("unknown preparation details: %v vs %v", errOther, errNone)
	}
	_, err := e.co.Invalidate(ctxb(), scopeOf("user-2", "ses-1"), id(p), act.PreparedActionInvalidationConditionsItemGoalChanged)
	if !errors.As(err, &ce) || ce.Code != contracts.ErrScopeViolation || ce.Details["reason"] == "not_found" {
		t.Errorf("another principal's preparation = %v (%v), want scope_violation that is not not_found", err, ce)
	}
}

func TestCheckReceipt(t *testing.T) {
	good := func() contracts.ActionReceipt {
		return contracts.ActionReceipt{
			ReceiptID:     "rcpt_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D",
			PreparationID: "prep_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D",
			CapabilityID:  "items.create",
			Status:        act.ActionReceiptStatusCommitted,
			AuditRef:      "aud_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D",
			IssuedAt:      stamp(t0),
		}
	}
	issue := []act.Issue{{Code: common.ErrorCodeConflict, Message: "x"}}
	follow := &act.FollowUp{Reference: "q1", State: act.FollowUpStateQueued}
	for name, tc := range map[string]struct {
		mut func(*contracts.ActionReceipt)
		ok  bool
	}{
		"committed":                 {func(*contracts.ActionReceipt) {}, true},
		"committed with an error":   {func(r *contracts.ActionReceipt) { r.Errors = issue }, false},
		"rejected without an error": {func(r *contracts.ActionReceipt) { r.Status = act.ActionReceiptStatusRejected }, false},
		"rejected with an error":    {func(r *contracts.ActionReceipt) { r.Status, r.Errors = act.ActionReceiptStatusRejected, issue }, true},
		"rejected with records": {func(r *contracts.ActionReceipt) {
			r.Status, r.Errors, r.Records = act.ActionReceiptStatusRejected, issue, []act.RecordRef{{EntityType: "a", EntityID: "b"}}
		}, false},
		"partial without an error":  {func(r *contracts.ActionReceipt) { r.Status = act.ActionReceiptStatusPartial }, false},
		"partial with an error":     {func(r *contracts.ActionReceipt) { r.Status, r.Errors = act.ActionReceiptStatusPartial, issue }, true},
		"uncertain with an error":   {func(r *contracts.ActionReceipt) { r.Status, r.Errors = act.ActionReceiptStatusUncertain, issue }, true},
		"queued without follow-up":  {func(r *contracts.ActionReceipt) { r.Status = act.ActionReceiptStatusQueued }, false},
		"queued with follow-up":     {func(r *contracts.ActionReceipt) { r.Status, r.FollowUp = act.ActionReceiptStatusQueued, follow }, true},
		"unknown status":            {func(r *contracts.ActionReceipt) { r.Status = "done" }, false},
		"malformed receipt id":      {func(r *contracts.ActionReceipt) { r.ReceiptID = "rcpt_1" }, false},
		"audit ref of another kind": {func(r *contracts.ActionReceipt) { r.AuditRef = "prep_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D" }, false},
		"too many errors for the schema": {func(r *contracts.ActionReceipt) {
			r.Status, r.Errors = act.ActionReceiptStatusRejected, make([]act.Issue, 65)
		}, false},
		"too many records for the schema": {func(r *contracts.ActionReceipt) { r.Records = make([]act.RecordRef, 257) }, false},
	} {
		r := good()
		tc.mut(&r)
		if err := checkReceipt(r); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}
}

// What a failed Commit means, in one table (SEC-041, SEC-047).
func TestClassifyCommit(t *testing.T) {
	r := row{capabilityID: capCreate, key: "idem_01J9ZQ3K8V4W5X6Y7Z8A9B0C1D", prepared: contracts.PreparedAction{
		Effects: []act.Effect{{Operation: act.EffectOperationCreate, EntityType: "item"}},
	}}
	result := func(status act.DeterministicResultStatus, mut func(*contracts.DeterministicResult)) contracts.DeterministicResult {
		res := contracts.DeterministicResult{
			Operation: act.DeterministicResultOperationCommit, CapabilityID: capCreate, Status: status,
			Identifiers: []act.RecordRef{{EntityType: "item", EntityID: "item_1"}}, Values: []act.Fact{}, Errors: []act.Issue{},
			AllowedNextActions: []act.AllowedNextAction{}, Freshness: &act.Freshness{AsOf: stamp(t0), Version: "v1"},
		}
		if status != act.DeterministicResultStatusOk && status != act.DeterministicResultStatusQueued {
			res.Errors = []act.Issue{{Code: common.ErrorCodeConflict, Message: "x"}}
		}
		if status == act.DeterministicResultStatusQueued {
			res.FollowUp = &act.FollowUp{Reference: "q1", State: act.FollowUpStateQueued}
		}
		if mut != nil {
			mut(&res)
		}
		return res
	}
	typed := func(c contracts.ErrCode) error { return contracts.New(c, "x") }
	for name, tc := range map[string]struct {
		res  contracts.DeterministicResult
		err  error
		want verdict
	}{
		"ok":                                {res: result(act.DeterministicResultStatusOk, nil), want: verdictRecorded},
		"queued":                            {res: result(act.DeterministicResultStatusQueued, nil), want: verdictRecorded},
		"partial":                           {res: result(act.DeterministicResultStatusPartial, nil), want: verdictRecorded},
		"ok about another record":           {res: result(act.DeterministicResultStatusOk, func(r *contracts.DeterministicResult) { r.Identifiers[0].EntityType = "invoice" }), want: verdictUnknown},
		"rejected":                          {res: result(act.DeterministicResultStatusRejected, nil), want: verdictRejected},
		"the host cannot tell":              {res: result(act.DeterministicResultStatusUncertain, nil), want: verdictUnknown},
		"a dependency of the host is down":  {res: result(act.DeterministicResultStatusUnavailable, nil), want: verdictUnknown},
		"a result for another capability":   {res: result(act.DeterministicResultStatusOk, func(r *contracts.DeterministicResult) { r.CapabilityID = "items.other" }), want: verdictUnknown},
		"a result for another operation":    {res: result(act.DeterministicResultStatusOk, func(r *contracts.DeterministicResult) { r.Operation = act.DeterministicResultOperationRead }), want: verdictUnknown},
		"unavailable":                       {err: typed(contracts.ErrUnavailable), want: verdictNotSent},
		"resource_exhausted":                {err: typed(contracts.ErrResourceExhausted), want: verdictNotSent},
		"rate_limited":                      {err: typed(contracts.ErrRateLimited), want: verdictNotSent},
		"scope_violation":                   {err: typed(contracts.ErrScopeViolation), want: verdictRefused},
		"validation_failed":                 {err: typed(contracts.ErrValidationFailed), want: verdictRefused},
		"permission_denied":                 {err: typed(contracts.ErrPermissionDenied), want: verdictRefused},
		"uncertain_outcome":                 {err: adapters.UncertainOutcome(errors.New("reset")), want: verdictUnknown},
		"uncertain_outcome wrapping a code": {err: contracts.Wrap(contracts.ErrUncertainOutcome, typed(contracts.ErrUnavailable), "x"), want: verdictUnknown},
		"timeout":                           {err: typed(contracts.ErrTimeout), want: verdictUnknown},
		"schema_invalid":                    {err: typed(contracts.ErrSchemaInvalid), want: verdictUnknown},
		"partial_failure as an error":       {err: typed(contracts.ErrPartialFailure), want: verdictUnknown},
		"conflict as an error":              {err: typed(contracts.ErrConflict), want: verdictUnknown},
		"internal":                          {err: typed(contracts.ErrInternal), want: verdictUnknown},
		"an untyped error":                  {err: errors.New("boom"), want: verdictUnknown},
		"a canceled context":                {err: fmt.Errorf("adapters: commit canceled: %w", context.Canceled), want: verdictUnknown},
	} {
		got, _, _ := classifyCommit(r, tc.res, tc.err)
		if got != tc.want {
			t.Errorf("%s: verdict %d, want %d", name, got, tc.want)
		}
	}
}

// Outcomes other than a plain commit are receipts that say what they are.
func TestRecordedOutcomesKeepTheirStatus(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")
	forge := func(status act.DeterministicResultStatus) func(*scripted) {
		return func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err != nil {
					return res, err
				}
				res.Status = status
				switch status {
				case act.DeterministicResultStatusQueued:
					res.Errors = []act.Issue{}
					res.FollowUp = &act.FollowUp{Reference: "mail_1", State: act.FollowUpStateQueued}
				case act.DeterministicResultStatusPartial:
					res.Errors = []act.Issue{{Code: common.ErrorCodePartialFailure, Message: "the notification was not sent"}}
				}
				return res, nil
			}
		}
	}
	t.Run("queued", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Alpha")
		e.sc.set(forge(act.DeterministicResultStatusQueued))
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusQueued || rc.FollowUp == nil || rc.FollowUp.Reference != "mail_1" || len(rc.Errors) != 0 {
			t.Fatalf("receipt = %+v", rc)
		}
		validateAgainst(t, schemaReceipt, rc)
		if e.state(id(p)) != act.PreparedActionStateCommitted {
			t.Errorf("state = %s", e.state(id(p)))
		}
	})
	t.Run("partial is an error with a receipt", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Beta")
		e.sc.set(forge(act.DeterministicResultStatusPartial))
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrPartialFailure)
		if rc.Status != act.ActionReceiptStatusPartial || len(rc.Errors) != 1 || len(rc.Records) != 1 {
			t.Fatalf("receipt = %+v", rc)
		}
		validateAgainst(t, schemaReceipt, rc)
		again, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrPartialFailure)
		if diff := cmp.Diff(rc, again); diff != "" {
			t.Errorf("receipt changed:\n%s", diff)
		}
		if e.hostCalls(adapters.OpCommit) != 1 {
			t.Error("a partial outcome was retried")
		}
	})
	t.Run("a refusal before sending is not applied", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Gamma")
		e.sc.set(func(s *scripted) {
			s.onCommit = func(adapters.CommitRequest, func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				return act.DeterministicResult{}, contracts.New(contracts.ErrScopeViolation, "no")
			}
		})
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrScopeViolation)
		if rc.Status != act.ActionReceiptStatusRejected || e.hostCalls(adapters.OpStatus) != 0 || e.state(id(p)) != act.PreparedActionStateRejected {
			t.Errorf("receipt %s ops %v state %s", rc.Status, e.hostOps(), e.state(id(p)))
		}
		validateAgainst(t, schemaReceipt, rc)
	})
}

// The requirement stored with a preparation binds at commit even when the
// policy, which knows only the declaration, would allow it.
func TestStoredRequirementBindsBesideThePolicy(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")
	e := newEnv(t, withLevel(level2))
	e.sc.set(func(s *scripted) {
		s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
			res, err := next()
			if err == nil && res.Preparation != nil {
				res.Preparation.DeclaredRiskLevel = act.RiskLevel3 // the host: this one is level 3
			}
			return res, err
		}
	})
	p := e.prepare(sc, "Alpha")
	// Pretend the confirmation happened: the policy (level 2, confirmed) allows,
	// the stored step-up requirement does not.
	e.exec(`UPDATE action_preparations SET state = 'confirmed', confirmed_at = 1 WHERE id = ?`, id(p))
	_, err := e.co.Commit(ctxb(), sc, id(p))
	wantCode(t, err, contracts.ErrStepUpRequired)
	if rec := e.recs.last(); rec.Outcome != "allow" {
		t.Errorf("the policy was expected to allow this one, and the stored mode to refuse: %+v", rec)
	}
	if e.hostCalls(adapters.OpCommit) != 0 || e.hostCalls(adapters.OpValidate) != 0 {
		t.Errorf("reached the host: %v", e.hostOps())
	}
}
