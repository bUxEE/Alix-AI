package ingest

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Hold is a scan the mass-deletion guard refused and that waits for the source
// to be remounted or for an operator to confirm it. It is what an operator
// reviews before calling ConfirmMassDeletion with its Digest.
type Hold struct {
	// Digest identifies exactly the deletion set that was refused.
	Digest string
	// Deleted of Previous items would have been deleted.
	Deleted  int
	Previous int
	// Why is WhyEmptyRoot, WhyMaxDeleted or WhyMaxFraction.
	Why string
	// Since is when the hold was recorded.
	Since time.Time
	// Confirmed is true while an operator's confirmation of this deletion is in
	// force; ConfirmedBy and ConfirmedUntil say by whom and until when.
	Confirmed      bool
	ConfirmedBy    string
	ConfirmedUntil time.Time
}

// Counts are the figures of a source, shaped like the counts of the source
// status (schema rag/source-status).
type Counts struct {
	// Discovered is what the last accepted scan found inside the root and the
	// include rules.
	Discovered int
	// Indexed is the items in the serving generation.
	Indexed int
	// Skipped is the items not accepted: by the provider (size, format, link,
	// name) or because the parser has no reader for their type.
	Skipped int
	// Failed is the items whose parse failed or timed out, never half-indexed.
	Failed int
	// Tombstoned is the deletion tombstones the source holds.
	Tombstoned int
	// Waiting is the versions still on their way (parse, index) and
	// EmbeddingPending those indexed whose embeddings are still queued.
	Waiting          int
	EmbeddingPending int
}

// SourceInfo is the operator-visible state of one source: availability and why,
// the figures, the serving generation and, when the guard refused a scan, the
// hold.
type SourceInfo struct {
	SourceID string
	// State is the source status lifecycle (schema rag/source-status).
	State rag.SourceStatusState
	// Available is false when the source cannot be trusted now: its root is
	// unreachable, a scan is held by the guard, a declared limit was reached,
	// or it was removed.
	Available bool
	// Reason is ReasonRootUnreachable, ReasonMassDeletion, ReasonLimitExceeded,
	// ReasonRemoved or ReasonTenantDeleted; "" when available.
	Reason    string
	ErrorCode string
	Message   string
	// LastAttempt is when the last scan started and LastSuccess when the last one
	// completed; LastSuccess never advances for a scan that could not reach the
	// root (master plan 18). Zero when none did.
	LastAttempt time.Time
	LastSuccess time.Time
	Counts      Counts
	// ActiveGenerationID is the generation that serves the source, "" when none.
	ActiveGenerationID string
	Hold               *Hold
}

// Status reads the state of a source of the caller's scope. A source of another
// scope is reported exactly like one that does not exist.
func (s *Service) Status(ctx context.Context, scope contracts.Scope, sourceID string) (SourceInfo, error) {
	if err := scope.Validate(); err != nil {
		return SourceInfo{}, err
	}
	if !ids.Valid(sourceID, source.IDPrefix) {
		return SourceInfo{}, contracts.New(contracts.ErrValidationFailed, "ingest: not a source id")
	}
	row, ok, err := loadSource(ctx, s.db, scope, sourceID)
	if err != nil {
		return SourceInfo{}, err
	}
	if !ok {
		return SourceInfo{}, errNoSuchSource()
	}
	info := SourceInfo{
		SourceID: sourceID, Available: row.Availability == availAvailable, Reason: row.Reason, ErrorCode: row.ErrorCode, Message: row.Message,
		LastAttempt: row.LastScan, LastSuccess: row.LastSuccess,
	}
	if row.HoldDigest != "" {
		now := s.clk.Now()
		info.Hold = &Hold{
			Digest: row.HoldDigest, Deleted: row.HoldDeleted, Previous: row.HoldPrevious, Why: row.HoldWhy, Since: row.HoldAt,
			Confirmed: row.ConfirmDigest == row.HoldDigest && now.Before(row.ConfirmUntil), ConfirmedBy: row.ConfirmBy, ConfirmedUntil: row.ConfirmUntil,
		}
	}
	if row.Availability == availRemoved {
		info.State = rag.SourceStatusStateRemoving
		if row.DeletionMode == modeDisable {
			info.State = rag.SourceStatusStateDisabled
		}
		return info, nil
	}
	active, hasActive, err := s.ix.ActiveGeneration(ctx, scope, sourceID)
	if err != nil {
		return SourceInfo{}, err
	}
	if hasActive {
		info.ActiveGenerationID = string(active.GenerationID)
	}
	c := Counts{Discovered: row.Discovered, Skipped: row.Skipped}
	count := func(where string, args ...any) (int, error) {
		return countVersions(ctx, s.db, scope, `source_id = ? AND `+where, append([]any{sourceID}, args...)...)
	}
	if hasActive {
		if c.Indexed, err = count(`generation_id = ? AND state = 'indexed'`, info.ActiveGenerationID); err != nil {
			return SourceInfo{}, err
		}
	}
	building, err := s.buildingGeneration(ctx, scope, sourceID)
	if err != nil {
		return SourceInfo{}, err
	}
	// Failed and skipped versions of the generation being looked at: the serving
	// one, or the one under construction when nothing serves yet.
	view := info.ActiveGenerationID
	if building != "" {
		view = building
	}
	if view != "" {
		var f, sk int
		if f, err = count(`generation_id = ? AND state = 'failed'`, view); err != nil {
			return SourceInfo{}, err
		}
		if sk, err = count(`generation_id = ? AND state = 'skipped'`, view); err != nil {
			return SourceInfo{}, err
		}
		c.Failed, c.Skipped = f, c.Skipped+sk
	}
	if c.Waiting, err = count(`state IN ('pending', 'parsed')`); err != nil {
		return SourceInfo{}, err
	}
	if c.EmbeddingPending, err = count(`state = 'indexed' AND embed_state = 'pending'`); err != nil {
		return SourceInfo{}, err
	}
	q, err := store.QueryRowScoped(ctx, s.db, scope, store.ScopedStmt{
		SQL: `SELECT count(*) FROM ragingest_tombstone`, Where: `source_id = ?`, WhereArgs: []any{sourceID},
	})
	if err != nil {
		return SourceInfo{}, err
	}
	if err := q.Scan(&c.Tombstoned); err != nil {
		return SourceInfo{}, err
	}
	info.Counts = c

	switch {
	case row.Reason == ReasonLimitExceeded:
		info.State = rag.SourceStatusStatePaused
	case row.LastScan.IsZero():
		info.State = rag.SourceStatusStateRegistered
	case c.Waiting > 0 || building != "":
		info.State = rag.SourceStatusStateIndexing
	case hasActive:
		info.State = rag.SourceStatusStateReady
	default:
		info.State = rag.SourceStatusStateRegistered
	}
	return info, nil
}

// buildingGeneration returns the generation under construction, or "".
func (s *Service) buildingGeneration(ctx context.Context, scope contracts.Scope, sourceID string) (string, error) {
	gens, err := s.ix.ListGenerations(ctx, scope, sourceID)
	if err != nil {
		return "", err
	}
	for _, g := range gens {
		if g.State == rag.IndexGenerationStateBuilding || g.State == rag.IndexGenerationStateValidating {
			return string(g.GenerationID), nil
		}
	}
	return "", nil
}
