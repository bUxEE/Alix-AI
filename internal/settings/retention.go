package settings

import (
	"context"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// History retention is enforced by internal/history, which holds the one stored
// policy (per application and per tenant) and applies the shorter of the two. When
// the service is built with a RetentionStore the retention a tenant or deployment
// document states is therefore not a setting this module keeps:
//
//   - a Put sends history.retention_days of a tenant document to the tenant's
//     policy and of a deployment document to the application's policy, and stores
//     the document without it. Omitting it removes the policy (a Put replaces the
//     whole document), exactly as it removes any other member;
//   - a Get gives the stored document back with the retention the store holds, so
//     reading a document and writing it back changes nothing;
//   - Effective takes the retention of the application and the tenant from the
//     store's own effective figure instead of applying a second rule of its own, and
//     then narrows it by the user's preference and the operator's ceiling.
//
// The user level has no equivalent in the store (history keeps no per-user policy):
// a user's retention stays a stored preference, is reported in the effective
// settings, and is not enforced by history.
//
// A retention change is audited by the store (history's settings.changed event,
// written in the same transaction as the policy), not by this module's
// settings.document_written event, and it does not bump the document's version:
// the stored document did not change. The version of a document still guards every
// other member. The policy is written inside the Put's own transaction, so a Put
// that is refused (a stale version, an event that cannot be recorded) leaves the
// policy as it was, and a refusal of the store refuses the whole Put.

// Retention is the retention policy in force for one application and tenant, as
// the RetentionStore reports it. A zero field means "not set".
type Retention struct {
	// Application is the application-wide policy in days (the deployment level) and
	// Tenant the tenant's own (the tenant level).
	Application int
	Tenant      int
	// EffectiveDays is the shorter of the two that are set, or 0 when neither is:
	// the store applies that rule, this module only reports it.
	EffectiveDays int
	// Source is "application" or "tenant" (the one EffectiveDays comes from), or
	// "none".
	Source string
}

// RetentionStore is where the history retention policy lives (internal/history). The
// composition root supplies it; the caller's scope is the one the settings service has
// already authorised for the level that is written, so the store must not ask the
// caller for a second capability, and the application-wide policy is written only
// because the token carries settings.deployment.write.
type RetentionStore interface {
	// Retention reports the policy in force for scope's application and tenant.
	Retention(ctx context.Context, scope contracts.Scope) (Retention, error)
	// SetRetention sets the policy of a level to days, or removes it when days is 0,
	// inside tx (the transaction of the settings document, on the same store), and
	// writes nothing when the level already holds days. LevelDeployment is the
	// application-wide policy and LevelTenant the tenant's own. A tenant cannot keep
	// history longer than its application's policy allows.
	SetRetention(ctx context.Context, tx *store.Tx, scope contracts.Scope, level Level, days int) error
}

// delegatesRetention reports whether a level's retention lives in the store.
func (s *Service) delegatesRetention(level Level) bool {
	return s.retention != nil && (level == LevelDeployment || level == LevelTenant)
}

// retentionOf is the days the store holds for a level of scope's application and tenant.
func retentionOf(level Level, r Retention) int {
	if level == LevelDeployment {
		return r.Application
	}
	return r.Tenant
}

// splitRetention returns the canonical document without its history.retention_days
// and the days it stated (0 when it stated none).
func splitRetention(level Level, canonical []byte) (stored []byte, days int, err error) {
	doc, err := decodeDocument(level, canonical)
	if err != nil {
		return nil, 0, contracts.Wrap(contracts.ErrInternal, err, "settings: a checked document can not be decoded")
	}
	if doc.policy != nil && doc.policy.History != nil && doc.policy.History.RetentionDays != nil {
		days = int(*doc.policy.History.RetentionDays)
	}
	if stored, err = withRetention(doc, 0); err != nil {
		return nil, 0, err
	}
	return stored, days, nil
}

// mergeRetention returns the stored document with retention days in it (the
// document as the API shows it), or without any when days is 0. A retention an older
// stored document still carries is never shown: the store is the only source.
func mergeRetention(level Level, stored []byte, days int) ([]byte, error) {
	doc, err := decodeDocument(level, stored)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, err, "settings: a stored document can not be decoded")
	}
	return withRetention(doc, days)
}

// withRetention is the canonical form of a tenant or deployment document with its
// history.retention_days set to days (removed when 0). An emptied history member is
// dropped, so a document that said nothing else stores no trace of history.
func withRetention(doc document, days int) ([]byte, error) {
	p := *doc.policy
	var h gen.PolicyHistory
	if p.History != nil {
		h = *p.History
	}
	h.RetentionDays = nil
	if days > 0 {
		d := gen.RetentionDays(days)
		h.RetentionDays = &d
	}
	p.History = &h
	if h == (gen.PolicyHistory{}) {
		p.History = nil
	}
	return document{level: doc.level, policy: &p}.canonical()
}

// applyRetention makes the store's policy of a level days, and does nothing when it
// already is. It runs in the document's transaction, before the document is
// written, so a refusal of the store (a tenant asking for more than its application
// allows, a write that is not permitted) leaves nothing stored, and a document that
// is not stored leaves the policy as it was.
func (s *Service) applyRetention(ctx context.Context, tx *store.Tx, scope contracts.Scope, level Level, days int) error {
	if err := s.retention.SetRetention(ctx, tx, scope, level, days); err != nil {
		return wrapStore(err, "settings: the history retention could not be written")
	}
	return nil
}

// layerRetention adds what the store holds to the layers of a caller: its effective
// figure, and the documents without any retention of their own (what an older
// document still says is not the policy).
func (s *Service) layerRetention(ctx context.Context, scope contracts.Scope, ly *layers) error {
	if s.retention == nil {
		return nil
	}
	r, err := s.retention.Retention(ctx, scope)
	if err != nil {
		return wrapStore(err, "settings: the history retention could not be read")
	}
	ly.retention = &r
	ly.deployment, ly.tenant = withoutRetention(ly.deployment), withoutRetention(ly.tenant)
	return nil
}

func withoutRetention(p *policyDoc) *policyDoc {
	if p == nil || p.History == nil || p.History.RetentionDays == nil {
		return p
	}
	q := *p
	h := *p.History
	h.RetentionDays = nil
	q.History = &h
	return &q
}

// RetentionDelegated reports whether the service sends the history retention of tenant and
// deployment documents to a RetentionStore. `alixai serve` always builds it that way; the
// composition root's tests assert it.
func (s *Service) RetentionDelegated() bool { return s.retention != nil }
