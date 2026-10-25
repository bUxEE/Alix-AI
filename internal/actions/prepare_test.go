package actions

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

var keyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// A prepared action is created from the host's Prepare result and the core's
// own decisions, stored in state prepared, announced with an audit event, and
// does not touch the application's data.
func TestPrepareCreatesPreparedAction(t *testing.T) {
	t.Run("level 1: no confirmation, host effects, core-minted members", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Alpha")
		a := p.Action

		if !ids.Valid(string(a.PreparationID), "prep") {
			t.Errorf("preparation id %q is not a prep_ ULID", a.PreparationID)
		}
		if !keyRE.MatchString(string(a.IdempotencyKey)) {
			t.Errorf("idempotency key %q is not a valid key", a.IdempotencyKey)
		}
		if a.State != act.PreparedActionStatePrepared || a.CapabilityID != capCreate {
			t.Errorf("state %s capability %s", a.State, a.CapabilityID)
		}
		if a.RiskLevel != act.RiskLevel1 {
			t.Errorf("risk = %d, want 1 (platform floor of a reversible capability, host declared 1)", a.RiskLevel)
		}
		if a.Confirmation.Mode != act.ConfirmationRequirementModeNone || len(a.Confirmation.Bindings) != 0 || !p.Confirmation.IsZero() {
			t.Errorf("a level 1 action needs no confirmation: %+v token=%v", a.Confirmation, !p.Confirmation.IsZero())
		}
		// From the host: the exact effect, its data version and its canonical summary.
		if len(a.Effects) != 1 || a.Effects[0].Operation != act.EffectOperationCreate || a.Effects[0].EntityType != fake.ItemEntity {
			t.Fatalf("effects = %+v", a.Effects)
		}
		if a.Summary != `Create an item named "Alpha"` {
			t.Errorf("summary = %q", a.Summary)
		}
		if len(a.DataVersions) != 1 || a.DataVersions[0].Version != "v0" {
			t.Errorf("data versions = %+v", a.DataVersions)
		}
		// From the core's clock: the host honours the preparation for 300 s, the
		// platform for ten minutes, and the earlier wins.
		if want := stamp(t0); a.PreparedAt != want {
			t.Errorf("prepared_at = %s, want %s", a.PreparedAt, want)
		}
		if want := stamp(t0.Add(300 * time.Second)); a.ExpiresAt != want {
			t.Errorf("expires_at = %s, want %s", a.ExpiresAt, want)
		}
		if !hasAll(a.InvalidationConditions, invalidationConditions()) {
			t.Errorf("invalidation conditions = %v", a.InvalidationConditions)
		}
		validateAgainst(t, schemaPrepared, a)

		// Stored in state prepared, scoped, with its audit event; the host's data is untouched.
		got, err := e.co.Get(ctxb(), sc, id(p))
		noErr(t, err)
		if diff := cmp.Diff(a, got); diff != "" {
			t.Errorf("stored preparation differs (-announced +stored):\n%s", diff)
		}
		if e.state(id(p)) != act.PreparedActionStatePrepared {
			t.Errorf("stored state = %s", e.state(id(p)))
		}
		if ev := e.events(); len(ev) != 1 || ev[0] != "action.prepared" {
			t.Errorf("audit events = %v, want [action.prepared]", ev)
		}
		if ph := e.recs.phases(); len(ph) != 1 || ph[0] != policy.PhasePrepare {
			t.Errorf("policy phases = %v, want [prepare]", ph)
		}
		if e.app.Effects() != 0 || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("preparing changed the application: effects=%d ops=%v", e.app.Effects(), e.hostOps())
		}
	})

	t.Run("level 2: explicit confirmation bound to four things, with a secret", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(scopeOf("user-1", "ses-1"), "Beta")
		a := p.Action
		if a.RiskLevel != act.RiskLevel2 || a.Confirmation.Mode != act.ConfirmationRequirementModeExplicit {
			t.Fatalf("risk %d mode %s", a.RiskLevel, a.Confirmation.Mode)
		}
		want := []act.ConfirmationRequirementBindingsItem{
			act.ConfirmationRequirementBindingsItemPreparationID, act.ConfirmationRequirementBindingsItemPrincipal,
			act.ConfirmationRequirementBindingsItemSession, act.ConfirmationRequirementBindingsItemDataVersions,
		}
		if diff := cmp.Diff(want, a.Confirmation.Bindings); diff != "" {
			t.Errorf("bindings (-want +got):\n%s", diff)
		}
		if p.Confirmation.IsZero() {
			t.Fatal("no confirmation token was issued")
		}
		validateAgainst(t, schemaPrepared, a)
		// The secret is in the token only: not in the announced action, not in the
		// database, not in an audit event.
		secret := p.Confirmation.Reveal()
		b, _ := json.Marshal(p)
		if strings.Contains(string(b), secret) {
			t.Error("the token is in the marshalled result")
		}
		for _, q := range []string{
			`SELECT COUNT(*) FROM action_preparations WHERE instr(confirmation_mac || prepared_json || fields_json || scope_json, ?) > 0`,
			`SELECT COUNT(*) FROM audit_events WHERE instr(data, ?) > 0`,
		} {
			if n := e.count(q, secret); n != 0 {
				t.Errorf("the confirmation secret is stored (%d rows): %s", n, q)
			}
		}
	})

	t.Run("the platform limit shortens a longer host promise, never lengthens it", func(t *testing.T) {
		e := newEnv(t, withConfig(Config{MaxPreparationTTL: time.Minute}))
		a := e.prepare(scopeOf("user-1", "ses-1"), "Gamma").Action
		if want := stamp(t0.Add(time.Minute)); a.ExpiresAt != want {
			t.Errorf("expires_at = %s, want %s (platform limit below the host's 300 s)", a.ExpiresAt, want)
		}
		e2 := newEnv(t, withConfig(Config{MaxPreparationTTL: time.Hour}))
		a = e2.prepare(scopeOf("user-1", "ses-1"), "Gamma").Action
		if want := stamp(t0.Add(300 * time.Second)); a.ExpiresAt != want {
			t.Errorf("expires_at = %s, want %s (the host's 300 s)", a.ExpiresAt, want)
		}
		// A promise too large to count in nanoseconds is the platform limit, not an overflow into the past.
		e3 := newEnv(t)
		e3.sc.set(func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Preparation != nil {
					huge := common.TTLSeconds(1 << 50)
					res.Preparation.ValidForSeconds = &huge
				}
				return res, err
			}
		})
		a = e3.prepare(scopeOf("user-1", "ses-1"), "Gamma").Action
		if want := stamp(t0.Add(DefaultMaxPreparationTTL)); a.ExpiresAt != want {
			t.Errorf("expires_at = %s, want %s (the platform limit)", a.ExpiresAt, want)
		}
	})

	t.Run("a missing required field is reported without calling the host", func(t *testing.T) {
		e := newEnv(t)
		sc := scopeOf("user-1", "ses-1")
		_, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{}})
		wantCode(t, err, contracts.ErrMissingFields)
		if n := len(e.app.Calls()); n != 0 {
			t.Errorf("the host was called %d times", n)
		}
		if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
			t.Errorf("%d preparations stored", n)
		}
	})

	t.Run("a host refusal is returned with its typed issues and stores nothing", func(t *testing.T) {
		e := newEnv(t)
		sc := scopeOf("user-1", "ses-1")
		p, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": strings.Repeat("x", 65)}})
		wantCode(t, err, contracts.ErrValidationFailed)
		if p.Result.Status != act.DeterministicResultStatusRejected || len(p.Result.Errors) != 1 || p.Result.Errors[0].Field == nil || *p.Result.Errors[0].Field != "name" {
			t.Errorf("result = %+v", p.Result)
		}
		if p.Action.PreparationID != "" || !p.Confirmation.IsZero() {
			t.Error("a refused preparation produced an action or a token")
		}
		if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
			t.Errorf("%d preparations stored", n)
		}
	})

	t.Run("policy denies before the host is asked", func(t *testing.T) {
		e := newEnv(t)
		visitor := scopeOf("user-1", "ses-1")
		visitor.Roles = []string{"visitor"} // declared, but not granted items.create
		_, err := e.co.Prepare(ctxb(), visitor, PrepareInput{Session: visitor, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrPermissionDenied)
		noToken := scopeOf("user-1", "ses-1")
		noToken.CapabilityScopes = []string{capRead}
		_, err = e.co.Prepare(ctxb(), noToken, PrepareInput{Session: noToken, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrScopeViolation)
		sc := scopeOf("user-1", "ses-1")
		_, err = e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: "items.delete", Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrUnknownCapability)
		if n := len(e.app.Calls()); n != 0 {
			t.Errorf("the host was called %d times: %v", n, e.hostOps())
		}
		// Every denial is itself in the audit log.
		if n := e.eventCount("policy.decision"); n != 3 {
			t.Errorf("%d policy decisions recorded, want 3", n)
		}
	})

	t.Run("the host cannot lower the platform minimum, and can raise the level", func(t *testing.T) {
		// A host that declares level 0 for an external effect is still level 2.
		e := newEnv(t, withLevel(level1))
		e.sc.set(func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Preparation != nil {
					res.Preparation.DeclaredRiskLevel = act.RiskLevel0
					res.Preparation.Effects[0].External = true
				}
				return res, err
			}
		})
		a := e.prepare(scopeOf("user-1", "ses-1"), "Delta").Action
		if a.RiskLevel != act.RiskLevel2 || a.Confirmation.Mode != act.ConfirmationRequirementModeExplicit {
			t.Errorf("an external effect must be level 2 with explicit confirmation, got %d %s", a.RiskLevel, a.Confirmation.Mode)
		}
		validateAgainst(t, schemaPrepared, a)
		// A host that declares level 3 makes the action level 3 with a step-up.
		e2 := newEnv(t, withLevel(level1))
		e2.sc.set(func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Preparation != nil {
					res.Preparation.DeclaredRiskLevel = act.RiskLevel3
				}
				return res, err
			}
		})
		a = e2.prepare(scopeOf("user-1", "ses-1"), "Delta").Action
		if a.RiskLevel != act.RiskLevel3 || a.Confirmation.Mode != act.ConfirmationRequirementModeStepUp {
			t.Errorf("host-declared level 3 must need a step-up, got %d %s", a.RiskLevel, a.Confirmation.Mode)
		}
		validateAgainst(t, schemaPrepared, a)
	})
}

func hasAll[T comparable](got, want []T) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
