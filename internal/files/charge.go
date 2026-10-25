package files

import (
	"context"
	"log/slog"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ChargeKind names the volume a Charger accounts a write to.
type ChargeKind string

const (
	// ChargeStorage is the bytes a tenant holds at rest (the storage gauge).
	ChargeStorage ChargeKind = "storage"
	// ChargeIngest is the bytes handed to the RAG ingestion (the daily counter).
	// Only a rag_source file is charged to it.
	ChargeIngest ChargeKind = "ingest"
)

// Charger is the quota hook of the files module (M9-01, ADR 0018). The module
// defines it and does not know who fills it: the composition root adapts
// internal/quotas, so this package keeps to the imports the module map allows.
//
// ChargeTx runs INSIDE the transaction that stores the file's metadata, before
// the blob is placed. A refusal (resource_exhausted, naming the dimension and
// the limit) is returned from Put unchanged and aborts the transaction: no row,
// no blob, no charge. A later failure of the same transaction takes the charge
// with it, because the charge is a write of that transaction and of no other.
//
// Refund gives a charge back OUTSIDE a transaction, for the one case where the
// data has to be withdrawn after it committed (the RAG ingestion refused the
// file). It is never a quota check and never fails a request.
type Charger interface {
	ChargeTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, kind ChargeKind, bytes int64) error
	Refund(ctx context.Context, scope contracts.Scope, kind ChargeKind, bytes int64) error
}

// ChargesInTransaction reports whether Put charges the tenant's volumes in the
// transaction that stores the file. The composition root's HTTP wrapper asks:
// a service that does is not also charged by a reservation at the boundary
// (that would count every upload twice), it is only checked early there.
func (s *Service) ChargesInTransaction() bool { return s.charge != nil }

// chargeTx charges a write's bytes inside tx: storage always, ingestion too for
// a file that becomes a RAG source. With no Charger it does nothing.
func (s *Service) chargeTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, treatment Treatment, bytes int64) error {
	if s.charge == nil {
		return nil
	}
	if err := s.charge.ChargeTx(ctx, tx, scope, ChargeStorage, bytes); err != nil {
		return err
	}
	if treatment == TreatmentRAGSource {
		return s.charge.ChargeTx(ctx, tx, scope, ChargeIngest, bytes)
	}
	return nil
}

// refundCharge gives back what chargeTx charged for a file that was withdrawn
// after its transaction committed.
func (s *Service) refundCharge(ctx context.Context, scope contracts.Scope, treatment Treatment, bytes int64) {
	if s.charge == nil {
		return
	}
	for _, k := range chargeKinds(treatment) {
		if err := s.charge.Refund(ctx, scope, k, bytes); err != nil {
			s.log.Warn("files: a charge could not be given back", slog.String("code", string(contracts.CodeOf(err))))
		}
	}
}

func chargeKinds(treatment Treatment) []ChargeKind {
	if treatment == TreatmentRAGSource {
		return []ChargeKind{ChargeStorage, ChargeIngest}
	}
	return []ChargeKind{ChargeStorage}
}
