package actions

import (
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// SEC-050. A DeterministicResult has no members that echo the request: it does
// not say which record it is about or which data version it saw, and a hostile
// or broken host can name any record. The shape check in internal/adapters
// (Normalise, run again here) proves a reply is a well-formed result for this
// operation, capability and idempotency key. What it cannot prove is that the
// result is about THIS action. The coordinator knows what it asked for (the
// exact effects and targets of the PreparedAction it holds), so it binds the
// result to that:
//
//   - a Commit or Status result may name only records the prepared action
//     targets: an identifier's entity type must be the type of one of its
//     effects and, where that effect names a target record (every effect but a
//     create), the identifier must be that record. A host cannot report the
//     commit as applied to a record the user never confirmed;
//   - the data versions the coordinator compares at commit are the ones it
//     stored at prepare, never ones a later reply claims (the freshness check
//     in commit.go compares the Validate result against the stored snapshot
//     version, not against anything in the Commit reply);
//   - a result that fails these checks is not used for a decision: after a
//     Commit was sent it is treated as an unknown outcome, which is resolved by
//     Status and, if that cannot decide, ends uncertain, never committed.

func mismatch(msg string) error {
	return contracts.New(contracts.ErrSchemaInvalid, "the application's result does not match the prepared action: "+msg)
}

// bindPrepare checks that the host's Prepare result is consistent with itself:
// it must carry a preparation and a freshness version, and must not give two
// versions for one record. Without a single version per record, "the data
// changed" would depend on which entry the comparison read.
func bindPrepare(res contracts.DeterministicResult) error {
	p := res.Preparation
	if p == nil {
		return mismatch("a prepare result carries no preparation")
	}
	if res.Freshness == nil || res.Freshness.Version == "" {
		return mismatch("a prepare result carries no freshness version")
	}
	type key struct{ typ, id string }
	seen := make(map[key]bool, len(p.DataVersions))
	for _, dv := range p.DataVersions {
		k := key{string(dv.EntityType), string(dv.EntityID)}
		if seen[k] {
			return mismatch("one record is given more than one data version")
		}
		seen[k] = true
	}
	return nil
}

// effectCovers reports whether an effect may produce the record ref.
func effectCovers(e act.Effect, ref act.RecordRef) bool {
	if e.EntityType != ref.EntityType {
		return false
	}
	return e.EntityID == nil || *e.EntityID == ref.EntityID
}

// bindCommit checks that a Commit or Status result concerns the prepared
// action it answers: every record it names is one the action targets.
func bindCommit(p contracts.PreparedAction, res contracts.DeterministicResult) error {
	for _, ref := range res.Identifiers {
		covered := false
		for _, e := range p.Effects {
			if effectCovers(e, ref) {
				covered = true
				break
			}
		}
		if !covered {
			return mismatch("it names a record the prepared action does not target")
		}
	}
	return nil
}

// hasExternalEffect reports whether any effect leaves the host application.
func hasExternalEffect(effects []act.Effect) bool {
	for _, e := range effects {
		if e.External {
			return true
		}
	}
	return false
}
