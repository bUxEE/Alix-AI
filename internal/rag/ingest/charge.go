package ingest

import (
	"context"
	"sync"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Charger is the quota hook of the ingestion (M9-01, ADR 0018). The package defines it and does not know who
// fills it: the composition root adapts internal/quotas, so this package keeps to the imports the module map
// allows.
//
// ChargeIngestTx runs INSIDE the transaction that writes the versions a scan created, before any of its rows
// are written, with the bytes of the documents that become new versions (what the sync is about to parse and
// index; a document whose content did not change costs nothing, and neither does a rebuild of a generation
// beside the active one). A refusal (resource_exhausted, naming the dimension, the limit and when the day
// resets) aborts that transaction: none of its rows and none of its charge are written. A failure after the
// charge takes the charge back with the rows, because the charge is a write of the same transaction.
type Charger interface {
	ChargeIngestTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, bytes int64) error
}

// ChargerFunc adapts a function to Charger.
type ChargerFunc func(ctx context.Context, tx *store.Tx, scope contracts.Scope, bytes int64) error

// ChargeIngestTx calls f.
func (f ChargerFunc) ChargeIngestTx(ctx context.Context, tx *store.Tx, scope contracts.Scope, bytes int64) error {
	return f(ctx, tx, scope, bytes)
}

// boundChargers holds the charger of a store for the services built over it that were given none in Deps. The
// serve composition builds the ingestion and the quotas in separate places over the one store; this is how the
// quotas reach a service built before them, without either importing the other. The lookup is made when a
// charge is due, not when the service is built, so the order of the two does not matter.
var boundChargers sync.Map // *store.DB -> *boundCharger

// boundCharger is a pointer so that unbinding can compare entries, whatever the charger's own type is.
type boundCharger struct{ c Charger }

// BindCharger makes c the charger of every service over db that has no Charge of its own, and returns the
// function that undoes it. Binding another charger to the same store replaces the first.
func BindCharger(db *store.DB, c Charger) (unbind func()) {
	if db == nil || c == nil {
		return func() {}
	}
	e := &boundCharger{c: c}
	boundChargers.Store(db, e)
	return func() { boundChargers.CompareAndDelete(db, e) }
}

// ChargerOf is the charger bound to db, or nil: for the composition root's tests and diagnostics, which ask
// whether the quotas reached the ingestion.
func ChargerOf(db *store.DB) Charger {
	if v, ok := boundChargers.Load(db); ok {
		return v.(*boundCharger).c
	}
	return nil
}

// charger is the Charger in force: the one the service was given, else the one bound to its store, else none.
func (s *Service) charger() Charger {
	if s.charge != nil {
		return s.charge
	}
	if v, ok := boundChargers.Load(s.db); ok {
		return v.(*boundCharger).c
	}
	return nil
}

// chargeBatch charges the bytes of the new versions of a batch in tx. total is zero when nothing new is read.
func (s *Service) chargeBatch(ctx context.Context, tx *store.Tx, scope contracts.Scope, total int64) error {
	if total <= 0 {
		return nil
	}
	c := s.charger()
	if c == nil {
		return nil
	}
	return c.ChargeIngestTx(ctx, tx, scope, total)
}
