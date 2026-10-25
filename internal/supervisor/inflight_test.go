package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

func receipt(prep string, st act.ActionReceiptStatus) contracts.ActionReceipt {
	return contracts.ActionReceipt{ReceiptID: act.ReceiptID("rcpt_" + prep), PreparationID: act.PreparationID(prep), Status: st}
}

// Only a receipt for that preparation whose status says the host recorded the
// commit makes an action committed; everything else is uncommitted.
func TestLedgerSettlesOnlyOnAProvingReceipt(t *testing.T) {
	tests := []struct {
		name          string
		src           ReceiptSource
		wantCommitted bool
		wantReconcile bool
		wantReason    string
	}{
		{"committed", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("p", act.ActionReceiptStatusCommitted)}}, true, false, ReasonReceiptProvesCommit},
		{"queued", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("p", act.ActionReceiptStatusQueued)}}, true, false, ReasonReceiptProvesCommit},
		{"partial", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("p", act.ActionReceiptStatusPartial)}}, true, false, ReasonReceiptProvesCommit},
		{"rejected", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("p", act.ActionReceiptStatusRejected)}}, false, false, ReasonReceiptRejected},
		{"uncertain", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("p", act.ActionReceiptStatusUncertain)}}, false, true, ReasonReceiptUncertain},
		{"another preparation", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("q", act.ActionReceiptStatusCommitted)}}, false, true, ReasonReceiptMismatch},
		{"unknown status", mapSource{receipts: map[string]contracts.ActionReceipt{"p": receipt("p", act.ActionReceiptStatus("done"))}}, false, true, ReasonReceiptInvalid},
		{"no receipt", mapSource{}, false, true, ReasonNoReceipt},
		{"lookup error", mapSource{fail: map[string]error{"p": errors.New("down")}}, false, true, ReasonLookupFailed},
		{"no source at all", nil, false, true, ReasonNoReceipt},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk := fakeClock()
			l := NewLedger(clk, tt.src)
			if err := l.Begin("p"); err != nil {
				t.Fatal(err)
			}
			clk.Advance(time.Second)
			out := l.Settle(context.Background())
			if len(out) != 1 {
				t.Fatalf("settled %d actions, want 1", len(out))
			}
			s := out[0]
			if s.Committed != tt.wantCommitted || s.NeedsReconcile != tt.wantReconcile || s.Reason != tt.wantReason {
				t.Errorf("settlement = %+v; want committed=%v reconcile=%v reason=%s", s, tt.wantCommitted, tt.wantReconcile, tt.wantReason)
			}
			if (s.ReceiptID != "") != tt.wantCommitted {
				t.Errorf("receipt id %q with committed=%v", s.ReceiptID, s.Committed)
			}
			if s.Settled.Sub(s.Began) != time.Second {
				t.Errorf("began %s settled %s", s.Began, s.Settled)
			}
			if l.Len() != 0 {
				t.Error("the ledger still tracks the action")
			}
		})
	}
}

func TestLedgerBeginEndAndBounds(t *testing.T) {
	l := NewLedger(clock.Real{}, nil)
	if err := l.Begin(""); !contracts.HasCode(err, contracts.ErrValidationFailed) {
		t.Errorf("empty id = %v, want validation_failed", err)
	}
	if err := l.Begin("a"); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin("a"); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Errorf("duplicate = %v, want conflict", err)
	}
	if err := l.Begin("b"); err != nil {
		t.Fatal(err)
	}
	l.End("a")
	l.End("unknown") // no-op
	out := l.Settle(context.Background())
	if len(out) != 1 || out[0].PreparationID != "b" {
		t.Errorf("settled %+v, want only b: a ended normally", out)
	}

	full := NewLedger(clock.Real{}, nil)
	for i := range maxInFlight {
		if err := full.Begin(string(rune('a'+i%26)) + time.Duration(i).String()); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
	}
	if err := full.Begin("one-too-many"); !contracts.HasCode(err, contracts.ErrResourceExhausted) {
		t.Errorf("past the bound = %v, want resource_exhausted", err)
	}
}

// Settlement keeps the order in which the actions began.
func TestLedgerSettlesInBeginOrder(t *testing.T) {
	l := NewLedger(clock.Real{}, nil)
	for _, id := range []string{"c", "a", "b"} {
		if err := l.Begin(id); err != nil {
			t.Fatal(err)
		}
	}
	var got string
	for _, s := range l.Settle(context.Background()) {
		got += s.PreparationID
	}
	if got != "cab" {
		t.Errorf("order = %q, want cab", got)
	}
}
