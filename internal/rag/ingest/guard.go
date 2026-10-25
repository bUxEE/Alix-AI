package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The mass-deletion guard (master plan 18, 19).
//
// An unmounted NAS share or removable drive leaves a reachable, empty directory
// at its mount point, and a scan of that directory reports every item the
// source held as deleted. The provider cannot tell the two apart and says so
// (providers/fs: "Known limitation"); this is the guard it asks its caller to
// apply. A scan whose deletions look like that is refused as a whole: nothing
// from it is applied (no tombstone, no new version, no snapshot change), the
// source is marked unavailable with the reason mass_deletion, the refusal is
// audited, and the job fails with a typed error. A genuine mass deletion needs
// an explicit operator action (Service.ConfirmMassDeletion) bound to the
// digest of exactly the deletion set that was refused; the confirmation is
// single use and expires. There is no switch that turns the guard off.

// Why a scan was held.
const (
	// WhyEmptyRoot: the scan found nothing at all where the previous one found
	// items. It applies whatever the size of the source.
	WhyEmptyRoot = "empty_root"
	// WhyMaxDeleted: more items would be deleted than GuardConfig.MaxDeleted.
	WhyMaxDeleted = "max_deleted"
	// WhyMaxFraction: more than GuardConfig.MaxFraction of the items of a source
	// of at least GuardConfig.MinPrevious would be deleted.
	WhyMaxFraction = "max_fraction"
)

// guardVerdict is the guard's decision on one scan.
type guardVerdict struct {
	Hold     bool
	Digest   string
	Deleted  int
	Previous int
	Why      string
}

// evaluateGuard decides whether a scan's deletions are applied. Only items
// that left a reachable root count (ReasonDeleted): an item that is still
// there but no longer admitted (excluded by a changed declaration, too large,
// a link now) is a per-item decision of the rules, and is exactly what an
// unmounted share can never produce.
func evaluateGuard(cfg GuardConfig, prev []source.Record, cs source.ChangeSet) guardVerdict {
	var gone []source.Tombstone
	for _, t := range cs.Deleted {
		if t.Reason == source.ReasonDeleted {
			gone = append(gone, t)
		}
	}
	v := guardVerdict{Deleted: len(gone), Previous: len(prev)}
	if len(gone) == 0 {
		return v
	}
	emptyRoot := len(cs.Next) == 0 && cs.Counts.Discovered == 0 && cs.Counts.Skipped == 0 && len(cs.Failed) == 0
	switch {
	case emptyRoot:
		v.Why = WhyEmptyRoot
	case len(gone) > cfg.MaxDeleted:
		v.Why = WhyMaxDeleted
	case len(prev) >= cfg.MinPrevious && float64(len(gone)) > cfg.MaxFraction*float64(len(prev)):
		v.Why = WhyMaxFraction
	default:
		return v
	}
	v.Hold = true
	v.Digest = guardDigest(len(prev), gone)
	return v
}

// guardDigest binds a confirmation to one deletion set: the items that would
// go (by key and last version) and the size of the snapshot they are measured
// against. A different set, even a smaller one, has a different digest.
func guardDigest(previous int, gone []source.Tombstone) string {
	sort.Slice(gone, func(i, j int) bool { return gone[i].Key < gone[j].Key })
	h := sha256.New()
	put := func(f string) {
		var n [binary.MaxVarintLen64]byte
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(f)))])
		h.Write([]byte(f))
	}
	put("alixai/ingest/mass-deletion/v1")
	put(fmt.Sprint(previous))
	for _, t := range gone {
		put(t.Key)
		put(t.Last.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// massDeletionError is the typed error of a held scan. It is unavailable (the
// source cannot be trusted until it is remounted or confirmed) and carries the
// digest an operator confirms.
func massDeletionError(v guardVerdict) error {
	return contracts.New(contracts.ErrUnavailable,
		fmt.Sprintf("ingest: the scan would delete %d of %d items of the source and was held; remount the source or confirm the deletion", v.Deleted, v.Previous)).
		WithDetails("reason", ReasonMassDeletion).
		WithDetails("why", v.Why).
		WithDetails("deleted", v.Deleted).
		WithDetails("previous", v.Previous).
		WithDetails("digest", v.Digest)
}

// IsMassDeletion reports whether err is the refusal of a scan that would delete
// too much of a source.
func IsMassDeletion(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Code != contracts.ErrUnavailable {
		return false
	}
	r, _ := ce.Details["reason"].(string)
	return r == ReasonMassDeletion
}

// The audit events of the guard (tenant chain).
type massDeletionEvent struct {
	SourceID    string `json:"source_id"`
	Digest      string `json:"digest"`
	Deleted     int    `json:"deleted"`
	Previous    int    `json:"previous"`
	Why         string `json:"why,omitempty"`
	ConfirmedBy string `json:"confirmed_by,omitempty"`
}

var (
	// EventMassDeletionHeld records a scan refused by the guard.
	EventMassDeletionHeld = audit.MustRegister[massDeletionEvent](audit.TypeSpec{
		Name: "ingest.mass_deletion_held", Description: "a scan that would delete too much of a source was held for an operator",
		Levels: audit.LevelTenant, Required: []string{"source_id", "digest"},
	})
	// EventMassDeletionConfirmed records an operator's confirmation of a held deletion.
	EventMassDeletionConfirmed = audit.MustRegister[massDeletionEvent](audit.TypeSpec{
		Name: "ingest.mass_deletion_confirmed", Description: "an operator confirmed a held mass deletion",
		Levels: audit.LevelTenant, Required: []string{"source_id", "digest", "confirmed_by"},
	})
	// EventMassDeletionApplied records the confirmed deletion being applied.
	EventMassDeletionApplied = audit.MustRegister[massDeletionEvent](audit.TypeSpec{
		Name: "ingest.mass_deletion_applied", Description: "a confirmed mass deletion was applied",
		Levels: audit.LevelTenant, Required: []string{"source_id", "digest"},
	})
)

// auditTx appends an event on the scope's tenant chain inside the caller's
// transaction, so it becomes durable with the caller's other writes or with
// none of them. An error means "do not proceed".
func (s *Service) auditTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, t audit.Type[massDeletionEvent], payload massDeletionEvent) error {
	loc, err := audit.ForScope(scope)
	if err != nil {
		return err
	}
	if _, err := t.AppendTx(ctx, s.aud, tx, loc, payload); err != nil {
		return err
	}
	return nil
}

// holdScan records a refused scan: the source becomes unavailable (reason
// mass_deletion), the hold is stored with the digest an operator confirms, and
// the refusal is audited, all in one transaction. If any of it fails the scan is
// still not applied: the guard fails closed. The returned error is permanent: a
// rescan of the same tree gives the same answer, and the next scheduled sync
// (after a remount) is the retry.
func (s *Service) holdScan(ctx context.Context, scope contracts.Scope, def source.Definition, v guardVerdict, now time.Time) error {
	if _, err := s.updateSource(ctx, scope, def.SourceID, true, func(tx *store.Tx, r *sourceRow) (bool, error) {
		if r.Availability == availRemoved {
			return false, ErrSourceGone
		}
		newHold := r.HoldDigest != v.Digest
		if r.Availability != availUnavailable || r.Reason != ReasonMassDeletion {
			r.Since = now
		}
		r.Availability, r.Reason, r.ErrorCode = availUnavailable, ReasonMassDeletion, string(contracts.ErrUnavailable)
		r.Message = fmt.Sprintf("a scan would delete %d of %d items (%s) and is held until the source is remounted or an operator confirms it", v.Deleted, v.Previous, v.Why)
		r.LastScan = now
		r.DeletionMode, r.PurgeAfter = deletionMode(def), def.Deletion.PurgeAfter
		if newHold {
			r.HoldDigest, r.HoldDeleted, r.HoldPrevious, r.HoldWhy, r.HoldAt = v.Digest, v.Deleted, v.Previous, v.Why, now
			if r.ConfirmDigest != v.Digest {
				r.ConfirmDigest, r.ConfirmBy, r.ConfirmUntil = "", "", time.Time{}
			}
			if err := s.auditTx(ctx, tx, scope, EventMassDeletionHeld, massDeletionEvent{
				SourceID: def.SourceID, Digest: v.Digest, Deleted: v.Deleted, Previous: v.Previous, Why: v.Why,
			}); err != nil {
				return false, err
			}
		}
		return true, nil
	}); err != nil {
		return err
	}
	s.log.Warn("scan held by the mass-deletion guard", "source", def.SourceID, "n_deleted", v.Deleted, "n_previous", v.Previous, "why", v.Why)
	return jobs.Permanent(massDeletionError(v))
}

// ConfirmMassDeletion is the explicit operator action that lets a held mass
// deletion through. The digest is the one the operator saw (SourceInfo.Hold,
// or the details of the typed error); it must be that of the hold in force, so
// a deletion set that changed since it was reviewed needs a new confirmation.
// The confirmation is audited, valid for Config.ConfirmTTL and used once: the
// next scan that produces exactly this deletion applies it. A sync is queued.
//
// The caller's scope must be the source's, and the OperatorAuthorizer must
// accept ActionConfirmMassDeletion; with none configured, or on any doubt, it
// is denied.
func (s *Service) ConfirmMassDeletion(ctx context.Context, scope contracts.Scope, sourceID, digest string) (jobs.EnqueueResult, error) {
	if err := scope.Validate(); err != nil {
		return jobs.EnqueueResult{}, err
	}
	if !ids.Valid(sourceID, source.IDPrefix) || digest == "" {
		return jobs.EnqueueResult{}, contracts.New(contracts.ErrValidationFailed, "ingest: a source id and the digest of the held deletion are required")
	}
	if s.operator == nil {
		return jobs.EnqueueResult{}, contracts.New(contracts.ErrPermissionDenied, "ingest: no operator authorisation is configured; a held mass deletion cannot be confirmed")
	}
	if err := s.operator.AuthorizeOperator(ctx, scope, ActionConfirmMassDeletion); err != nil {
		if contracts.CodeOf(err) == contracts.ErrPermissionDenied || contracts.CodeOf(err) == contracts.ErrScopeViolation {
			return jobs.EnqueueResult{}, err
		}
		return jobs.EnqueueResult{}, contracts.Wrap(contracts.ErrPermissionDenied, err, "ingest: the operator action is not authorised")
	}
	now := s.clk.Now()
	if _, err := s.updateSource(ctx, scope, sourceID, false, func(tx *store.Tx, r *sourceRow) (bool, error) {
		switch {
		case r.Availability == availRemoved:
			return false, ErrSourceGone
		case r.HoldDigest == "":
			return false, contracts.New(contracts.ErrConflict, "ingest: the source has no held deletion")
		case r.HoldDigest != digest:
			return false, contracts.New(contracts.ErrConflict, "ingest: the held deletion is not the one that was reviewed; review it again")
		}
		r.ConfirmDigest, r.ConfirmBy, r.ConfirmUntil = digest, scope.PrincipalID, now.Add(s.cfg.ConfirmTTL)
		return true, s.auditTx(ctx, tx, scope, EventMassDeletionConfirmed, massDeletionEvent{
			SourceID: sourceID, Digest: digest, Deleted: r.HoldDeleted, Previous: r.HoldPrevious, Why: r.HoldWhy, ConfirmedBy: scope.PrincipalID,
		})
	}); err != nil {
		if isGone(err) {
			return jobs.EnqueueResult{}, errNoSuchSource()
		}
		return jobs.EnqueueResult{}, err
	}
	// The default key names the state of the source, so this coalesces with a
	// sync that is already waiting (it will find the confirmation) and is a new
	// request after a scan has completed.
	return s.RequestSync(ctx, scope, sourceID, SyncOptions{})
}
