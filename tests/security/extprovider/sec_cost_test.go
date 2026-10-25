package extprovidersec

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// callCost is the micro-units every routed call below reports and is reserved at (the floor of the estimate).
const callCost = 100

// routeCost routes one controller call that settles at the cost the provider reports. hold, when set, keeps the
// call open inside the send.
func (w *world) routeCost(sc contracts.Scope, reported pv.MicroUnits, sentN *atomic.Int32, hold func()) (modes.Outcome, error) {
	req := modes.Request{
		Scope: sc, Role: modes.RoleController, Candidate: w.baseCandidate(), Requirement: providerreg.Requirement{DataClass: pv.DataClassificationInternal},
		Estimate: func(pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
			return providerbudget.Units{ToksIn: 10, ToksOut: 5}, callCost, nil
		},
	}
	return w.rt.Router().Execute(ctxb(), req, modes.Executors{
		Local: func(context.Context) error { return nil },
		External: func(context.Context, modes.ExternalCall) (modes.ExternalResult, error) {
			sentN.Add(1)
			if hold != nil {
				hold()
			}
			return modes.ExternalResult{Metering: providerbudget.Units{ToksIn: 8, ToksOut: 4}, ReportedMicroUnits: &reported}, nil
		},
	})
}

func i64p(n int64) *int64 { return &n }

// SEC-122
func TestSecExtCostExhaustionStopsExternalCalls(t *testing.T) {
	t.Run("control: a budget that covers the calls lets every one of them through", func(t *testing.T) {
		w := newWorld(t, withBudget(10*callCost))
		var sent atomic.Int32
		for i := 0; i < 5; i++ {
			if o, err := w.routeCost(receptionist(), callCost, &sent, nil); err != nil || !o.Served() {
				t.Fatalf("call %d: outcome %+v err %v", i, o, err)
			}
		}
		if sent.Load() != 5 {
			t.Fatalf("sent %d, want 5", sent.Load())
		}
	})

	t.Run("a burst of concurrent calls is bounded by the budget, never by the number of callers", func(t *testing.T) {
		w := newWorld(t, withBudget(5*callCost))
		var sent atomic.Int32
		var wg sync.WaitGroup
		var mu sync.Mutex
		var refused []modes.Outcome
		var codes []contracts.ErrCode
		const callers = 16
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o, err := w.routeCost(receptionist(), callCost, &sent, nil)
				if err != nil {
					mu.Lock()
					refused = append(refused, o)
					codes = append(codes, contracts.CodeOf(err))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if sent.Load() > 5 {
			t.Fatalf("%d calls reached the provider on a budget for 5", sent.Load())
		}
		if sent.Load() != 5 || len(refused) != callers-5 {
			t.Fatalf("sent %d refused %d: the budget must be spent exactly, not under-used", sent.Load(), len(refused))
		}
		for i, o := range refused {
			if o.Kind != modes.KindBudgetExhausted || o.DataSent || codes[i] != contracts.ErrResourceExhausted {
				t.Fatalf("refused outcome %+v code %s: a refused call is typed and sent nothing", o, codes[i])
			}
		}
		// After exhaustion every further call is refused too.
		if o, err := w.routeCost(receptionist(), callCost, &sent, nil); err == nil || o.Kind != modes.KindBudgetExhausted || sent.Load() != 5 {
			t.Fatalf("a call after exhaustion: outcome %+v err %v sent %d", o, err, sent.Load())
		}
		if w.local.Calls() != 0 {
			t.Fatal("exhaustion moved the request to the local runtime unasked")
		}
	})

	t.Run("a reported overspend is charged in full, so the next call is refused", func(t *testing.T) {
		w := newWorld(t, withBudget(5*callCost))
		var sent atomic.Int32
		if o, err := w.routeCost(receptionist(), 5*callCost, &sent, nil); err != nil || !o.Served() {
			t.Fatalf("first call: %+v %v", o, err)
		}
		o, err := w.routeCost(receptionist(), callCost, &sent, nil)
		if err == nil || o.Kind != modes.KindBudgetExhausted || sent.Load() != 1 {
			t.Fatalf("second call: outcome %+v err %v sent %d: the spend of the first was not counted", o, err, sent.Load())
		}
	})

	t.Run("a request rate limit refuses the burst and says when to retry", func(t *testing.T) {
		w := newWorld(t, withoutBudget())
		w.putLimits(1000*callCost, i64p(2), nil)
		var sent atomic.Int32
		for i := 0; i < 2; i++ {
			if o, err := w.routeCost(receptionist(), callCost, &sent, nil); err != nil || !o.Served() {
				t.Fatalf("call %d: %+v %v", i, o, err)
			}
		}
		o, err := w.routeCost(receptionist(), callCost, &sent, nil)
		if contracts.CodeOf(err) != contracts.ErrRateLimited || o.Kind != modes.KindRateLimited || o.RetryAfter <= 0 || sent.Load() != 2 {
			t.Fatalf("outcome %+v err %v sent %d", o, err, sent.Load())
		}
	})

	t.Run("a concurrency limit refuses a second call while the first is in flight", func(t *testing.T) {
		w := newWorld(t, withoutBudget())
		w.putLimits(1000*callCost, nil, i64p(1))
		var sent atomic.Int32
		inFlight, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			_, err := w.routeCost(receptionist(), callCost, &sent, func() { close(inFlight); <-release })
			done <- err
		}()
		<-inFlight
		o, err := w.routeCost(receptionist(), callCost, &sent, nil)
		if err == nil || o.Kind != modes.KindBudgetExhausted || o.DataSent || sent.Load() != 1 {
			t.Fatalf("overlapping call: outcome %+v err %v sent %d", o, err, sent.Load())
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("the first call: %v", err)
		}
		if o, err := w.routeCost(receptionist(), callCost, &sent, nil); err != nil || !o.Served() {
			t.Fatalf("after the first settled: %+v %v", o, err)
		}
	})

	t.Run("no budget configured means no external call", func(t *testing.T) {
		w := newWorld(t, withoutBudget())
		var sent atomic.Int32
		o, err := w.routeCost(receptionist(), callCost, &sent, nil)
		if err == nil || o.Kind != modes.KindBudgetExhausted || sent.Load() != 0 || o.DataSent {
			t.Fatalf("outcome %+v err %v sent %d", o, err, sent.Load())
		}
	})

	t.Run("a whole conversation stops the same way: every turn refused, nothing prepared, nothing sent", func(t *testing.T) {
		w := newWorld(t, withBudget(1)) // one micro-unit a day: below any call's upper bound
		for i := 0; i < 3; i++ {
			w.proposeBook()
			res := w.say(receptionist(), sayBook)
			w.failedWith(res, contracts.ErrResourceExhausted)
			w.nothingExecuted(res)
		}
		if w.snd.sends() != 0 || w.ext.Calls() != 0 || w.local.Calls() != 0 {
			t.Fatalf("sends %d provider calls %d local calls %d", w.snd.sends(), w.ext.Calls(), w.local.Calls())
		}
		states, err := w.budget.States(ctxb(), receptionist(), egProvider, pv.ChannelInteractive)
		if err != nil || len(states) == 0 {
			t.Fatalf("states %v err %v: the budget state must be readable by the operator", states, err)
		}
	})
}
