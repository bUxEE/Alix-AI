package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/buxee/alix-ai/pkg/bridge"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// The prepare / validate / commit / status protocol every consequential
// capability shares. A capability supplies what is particular to it (actionDef);
// this file supplies what must never differ between them:
//
//   - the host reauthorises every call from the identity it receives;
//   - validate and prepare run the same checks and report the same snapshot
//     version on unchanged data;
//   - commit applies exactly once per idempotency key, answers a repeat with the
//     original reply, refuses a preparation whose data moved (stale) or whose time
//     ran out (expired), and re-checks every rule before it writes;
//   - status says what was recorded for a key, and "nothing" in the one shape that
//     proves nothing was applied.

// plan is what a capability computed from its fields and the current data.
type plan struct {
	versions []actions.DataVersion
	values   []actions.Fact
	summary  string
	effects  []actions.Effect
	next     []NextAction
	// gate is a commit-time precondition beyond the data: the step-up approval of
	// a level 3 action. nil means none.
	gate func() *refusal
	// apply writes. It runs under the lock, after every check passed.
	apply func() applied
}

// applied is what a write produced.
type applied struct {
	ids      []actions.RecordRef
	values   []actions.Fact
	versions []actions.DataVersion
	next     []NextAction
}

type actionDef struct {
	id         string
	permission string
	// fieldNames are the capability's fields. The first effect of a prepare lists
	// every given field under its own name, which is how commit re-derives what
	// the user confirmed.
	fieldNames []string
	risk       int
	validFor   int
	load       func(l *Ledger, c *caller, f fields) (*plan, *refusal)
}

// IdempotencyWindow is how long a recorded commit is remembered.
const IdempotencyWindow = 24 * time.Hour

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (l *Ledger) evaluate(def *actionDef, id bridge.Identity, raw map[string]any) (*caller, fields, *plan, *refusal) {
	c, bad := l.resolveCaller(id, def.permission)
	if bad != nil {
		return nil, nil, nil, bad
	}
	f, bad := parseFields(raw, def.fieldNames...)
	if bad != nil {
		return nil, nil, nil, bad
	}
	p, bad := def.load(l, c, f)
	if bad != nil {
		return nil, nil, nil, bad
	}
	return c, f, p, nil
}

func (l *Ledger) validateAction(def *actionDef, id bridge.Identity, raw map[string]any) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _, p, bad := l.evaluate(def, id, raw)
	if bad != nil {
		return bad.result()
	}
	r := okResult(l.now(), p.versions)
	r.Values = p.values
	return r
}

func (l *Ledger) prepareAction(def *actionDef, id bridge.Identity, raw map[string]any) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _, p, bad := l.evaluate(def, id, raw)
	if bad != nil {
		return bad.result()
	}
	r := okResult(l.now(), p.versions)
	r.Values = p.values
	r.AllowedNextActions = p.next
	r.Preparation = &Preparation{
		Summary: p.summary, Effects: p.effects, DataVersions: p.versions,
		DeclaredRiskLevel: def.risk, ValidForSeconds: def.validFor,
	}
	return r
}

// factScalar reads the value of an effect's change back as a field value. A money
// value is never read back: the amount travels as its integer minor units.
func factScalar(f actions.Fact) any {
	switch v := f.Value.Value.(type) {
	case actions.TextValue:
		return v.Value
	case actions.IdentifierValue:
		return string(v.Value)
	case actions.DateValue:
		return v.Value
	case actions.IntegerValue:
		return v.Value
	case actions.BooleanValue:
		return v.Value
	}
	return nil
}

// fieldsFromEffects re-derives the candidate fields from the first effect of the
// prepared action. Everything in the prepared action is untrusted input: the
// fields are checked again against the current data, and the data versions it
// recorded are compared with the ones the check reads now.
func (def *actionDef) fieldsFromEffects(p *actions.PreparedAction) (fields, *refusal) {
	if p == nil || len(p.Effects) == 0 {
		return nil, refuse("validation_failed", "prepared", "the prepared action is incomplete")
	}
	f := fields{}
	for _, ch := range p.Effects[0].Changes {
		if slices.Contains(def.fieldNames, string(ch.Field)) {
			f[string(ch.Field)] = factScalar(ch)
		}
	}
	return f, nil
}

func fingerprint(capability, principalID string, f fields) string {
	b, _ := json.Marshal(map[string]any{"capability": capability, "principal": principalID, "fields": f})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (l *Ledger) commitAction(def *actionDef, id bridge.Identity, key string, prepared *actions.PreparedAction) Result {
	l.mu.Lock()
	res := func() Result {
		c, bad := l.resolveCaller(id, def.permission)
		if bad != nil {
			return bad.result()
		}
		if !keyPattern.MatchString(key) {
			return refuse("validation_failed", "idempotency_key", "the idempotency key must be 16 to 128 URL-safe characters").result()
		}
		l.sweepReceipts(c.tenant)
		if prepared == nil {
			return refuse("validation_failed", "prepared", "the prepared action is missing").result()
		}
		if string(prepared.CapabilityID) != def.id {
			return refuse("validation_failed", "prepared", "the prepared action is for another capability").result()
		}
		if prepared.IdempotencyKey != "" && string(prepared.IdempotencyKey) != key {
			return refuse("validation_failed", "idempotency_key", "the idempotency key is not the prepared action's").result()
		}
		f, bad := def.fieldsFromEffects(prepared)
		if bad != nil {
			return bad.result()
		}
		fp := fingerprint(def.id, c.principal.id, f)

		// A repeat answers with the original reply and changes nothing. The same key
		// for anything else is a conflict, never a second effect.
		if prev := c.tenant.receipts[key]; prev != nil {
			if prev.capability != def.id || prev.principal != c.principal.id || prev.fingerprint != fp {
				return refuse("conflict", "idempotency_key", "the idempotency key was already used for a different action").result()
			}
			return prev.result
		}

		now := l.now()
		if exp, err := time.Parse(time.RFC3339, string(prepared.ExpiresAt)); err != nil || !now.Before(exp) {
			return refuse("expired", "prepared", "the prepared action expired before it was committed").result()
		}

		p, bad := def.load(l, c, f)
		if bad != nil {
			return bad.result()
		}
		// First the data the user confirmed, then the rules: a record that moved is
		// stale, whatever else changed with it.
		for _, cur := range p.versions {
			seen, found := actions.DataVersion{}, false
			for _, v := range prepared.DataVersions {
				if v.EntityType == cur.EntityType && v.EntityID == cur.EntityID {
					seen, found = v, true
				}
			}
			if !found {
				return refuse("validation_failed", "prepared", "the prepared action does not record the data it relied on").result()
			}
			if seen.Version != cur.Version {
				r := refuse("stale", "", "a record changed after the action was prepared")
				r.fresh = freshnessOf(now, p.versions)
				return r.result()
			}
		}
		if p.gate != nil {
			if bad := p.gate(); bad != nil {
				return bad.result()
			}
		}
		ap := p.apply()
		out := okResult(now, ap.versions)
		out.Identifiers, out.Values, out.AllowedNextActions = ap.ids, ap.values, ap.next
		c.tenant.receipts[key] = &receipt{capability: def.id, principal: c.principal.id, fingerprint: fp, result: out, at: now}
		return out
	}()
	l.mu.Unlock()
	// Delivery is best effort, outside the lock and off the reply path: the commit is
	// done, the caller must not wait for AlixAI's intake (a slow one would turn a
	// success into an uncertain outcome), and the event about a record must not reach
	// AlixAI before the reply to the commit that changed it. An event that cannot be
	// sent stays in the outbox; flushes are serialised, so events leave in order.
	if l.cfg.Events != nil {
		go func() { _ = l.flushOutbox(context.Background()) }()
	}
	return res
}

func (l *Ledger) sweepReceipts(t *tenant) {
	cutoff := l.now().Add(-IdempotencyWindow)
	for k, r := range t.receipts {
		if r.at.Before(cutoff) {
			delete(t.receipts, k)
		}
	}
}

// statusAction says what became of the commit sent with a key. Exactly one answer
// proves that nothing was applied: rejected, one validation_failed on
// idempotency_key, no identifiers. Every other answer is a recorded outcome or a
// refusal, and a refusal says nothing about whether the commit happened.
func (l *Ledger) statusAction(def *actionDef, id bridge.Identity, key string) Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, bad := l.resolveCaller(id, def.permission)
	if bad != nil {
		return bad.result()
	}
	if !keyPattern.MatchString(key) {
		return refuse("validation_failed", "idempotency_key", "the idempotency key must be 16 to 128 URL-safe characters").result()
	}
	l.sweepReceipts(c.tenant)
	r := c.tenant.receipts[key]
	if r == nil {
		return refuse("validation_failed", "idempotency_key", "the application has no record of this idempotency key").result()
	}
	if r.principal != c.principal.id {
		return refuse("permission_denied", "", "the idempotency key belongs to another principal").result()
	}
	if r.capability != def.id {
		return refuse("conflict", "idempotency_key", "the idempotency key was used for another capability").result()
	}
	return r.result
}

// ---- shared pieces of the capabilities --------------------------------------

func nextConfirm() []NextAction {
	return []NextAction{{Action: "confirm"}, {Action: "cancel"}}
}

func nextCancel() []NextAction { return []NextAction{{Action: "cancel"}} }

func sendEffect(entity, description string) actions.Effect {
	return actions.Effect{
		Operation: actions.EffectOperationSend, EntityType: "notification",
		Description: description, Changes: []actions.Fact{textFact("recipient", entity)}, External: true,
	}
}

// visibleCustomer returns a customer the caller may reach, or scope_violation: a
// record of another team or organisation is refused exactly like one that does not
// exist.
func (c *caller) customer(id string, tenantWide bool) (*customer, *refusal) {
	cu := c.tenant.customers[id]
	if cu == nil || !c.canSeeTeam(cu.team, tenantWide) {
		return nil, refuse("scope_violation", "customer", "that customer is not available")
	}
	return cu, nil
}

func (c *caller) quote(id string) (*quote, *refusal) {
	q := c.tenant.quotes[id]
	if q == nil || !c.canSeeTeam(q.team, false) {
		return nil, refuse("scope_violation", "quote", "that quote is not available")
	}
	return q, nil
}

func (c *caller) invoice(id string, tenantWide bool) (*invoice, *refusal) {
	inv := c.tenant.invoices[id]
	if inv == nil || !c.canSeeTeam(inv.team, tenantWide) {
		return nil, refuse("scope_violation", "invoice", "that invoice is not available")
	}
	return inv, nil
}
