package supervisor

import (
	"context"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// maxInFlight bounds the ledger: a leak in a caller must not grow memory
// without limit, and refusing the 4097th concurrent model-driven action is
// safe (fail closed).
const maxInFlight = 4096

// Reasons a Settlement carries. They are fixed strings: nothing from a
// receipt's free text or from the runtime reaches them.
const (
	ReasonReceiptProvesCommit = "receipt_proves_commit"
	ReasonReceiptRejected     = "receipt_rejected"
	ReasonReceiptUncertain    = "receipt_uncertain"
	ReasonReceiptMismatch     = "receipt_mismatch"
	ReasonReceiptInvalid      = "receipt_invalid"
	ReasonNoReceipt           = "no_receipt"
	ReasonLookupFailed        = "receipt_lookup_failed"
)

// ReceiptSource finds the receipt the core wrote for a preparation. It is the
// only proof the ledger accepts: it must read the action coordinator's store,
// never anything the model produced. found is false when no receipt exists.
type ReceiptSource interface {
	Receipt(ctx context.Context, preparationID string) (r contracts.ActionReceipt, found bool, err error)
}

// Settlement is what became of one in-flight action when the runtime was lost.
type Settlement struct {
	PreparationID string
	// Committed is true only when a receipt proves the host recorded the
	// commit. Every other outcome is uncommitted: the action was not applied,
	// or nothing proves that it was.
	Committed bool
	// ReceiptID names the proving receipt when Committed.
	ReceiptID string
	// Reason is one of the Reason constants.
	Reason string
	// NeedsReconcile is true when the outcome is unproven: the caller must ask
	// the host (the coordinator's Recover or Reconcile, through Status and the
	// idempotency key) before any retry. A rejected receipt proves nothing was
	// applied and needs none.
	NeedsReconcile bool
	Began          time.Time
	Settled        time.Time
}

// Ledger tracks the preparations in flight against the runtime. It records
// facts and settles them; it never commits, retries or resumes anything.
type Ledger struct {
	mu    sync.Mutex
	clk   clock.Clock
	src   ReceiptSource
	order []string
	began map[string]time.Time
}

// NewLedger returns a ledger. src may be nil: with no way to prove a commit
// every settlement is uncommitted.
func NewLedger(clk clock.Clock, src ReceiptSource) *Ledger {
	if clk == nil {
		clk = clock.Real{}
	}
	return &Ledger{clk: clk, src: src, began: map[string]time.Time{}}
}

// Begin records that preparationID is now in flight against the runtime.
func (l *Ledger) Begin(preparationID string) error {
	if preparationID == "" {
		return contracts.New(contracts.ErrValidationFailed, "supervisor: an in-flight action needs a preparation id")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, dup := l.began[preparationID]; dup {
		return contracts.New(contracts.ErrConflict, "supervisor: the action is already in flight")
	}
	if len(l.began) >= maxInFlight {
		return contracts.New(contracts.ErrResourceExhausted, "supervisor: too many actions in flight")
	}
	l.began[preparationID] = l.clk.Now()
	l.order = append(l.order, preparationID)
	return nil
}

// End records that the action's outcome was settled the normal way (the
// coordinator wrote its receipt or ended the preparation). It is a no-op for an
// unknown id.
func (l *Ledger) End(preparationID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.began[preparationID]; !ok {
		return
	}
	delete(l.began, preparationID)
	for i, id := range l.order {
		if id == preparationID {
			l.order = append(l.order[:i], l.order[i+1:]...)
			break
		}
	}
}

// Len is the number of actions in flight.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.began)
}

// Settle ends every in-flight action, in the order it began, and says what
// became of each. It is called when the runtime is lost, before any restart. An
// action is Committed only when the receipt source returns a receipt for that
// very preparation whose status says the host recorded the commit; every other
// case is uncommitted.
func (l *Ledger) Settle(ctx context.Context) []Settlement {
	l.mu.Lock()
	ids := l.order
	began := l.began
	l.order = nil
	l.began = map[string]time.Time{}
	l.mu.Unlock()

	out := make([]Settlement, 0, len(ids))
	for _, id := range ids {
		s := l.settleOne(ctx, id)
		s.Began = began[id]
		s.Settled = l.clk.Now()
		out = append(out, s)
	}
	return out
}

func (l *Ledger) settleOne(ctx context.Context, id string) Settlement {
	s := Settlement{PreparationID: id, NeedsReconcile: true}
	if l.src == nil {
		s.Reason = ReasonNoReceipt
		return s
	}
	r, found, err := l.src.Receipt(ctx, id)
	switch {
	case err != nil:
		s.Reason = ReasonLookupFailed
	case !found:
		s.Reason = ReasonNoReceipt
	case string(r.PreparationID) != id:
		s.Reason = ReasonReceiptMismatch
	case !r.Status.Valid():
		s.Reason = ReasonReceiptInvalid
	default:
		switch r.Status {
		case act.ActionReceiptStatusCommitted, act.ActionReceiptStatusQueued, act.ActionReceiptStatusPartial:
			s.Committed = true
			s.ReceiptID = string(r.ReceiptID)
			s.Reason = ReasonReceiptProvesCommit
			s.NeedsReconcile = false
		case act.ActionReceiptStatusRejected:
			s.Reason = ReasonReceiptRejected
			s.NeedsReconcile = false
		default: // uncertain
			s.Reason = ReasonReceiptUncertain
		}
	}
	return s
}
