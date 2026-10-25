package extprovidersec

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// transport are the only conditions on which a fallback may ever be named for a provider that is up and trusted.
var transportConditions = []modes.Condition{modes.CondUnavailable, modes.CondTimeout, modes.CondRateLimited, modes.CondUnsupportedModality, modes.CondMalformedOutput}

var transportOn = []pv.ProviderProfileFallbackOnItem{
	pv.ProviderProfileFallbackOnItemUnavailable, pv.ProviderProfileFallbackOnItemTimeout,
	pv.ProviderProfileFallbackOnItemRateLimited, pv.ProviderProfileFallbackOnItemUnsupportedModality,
}

// SEC-121
func TestSecExtNoHiddenFallback(t *testing.T) {
	t.Run("control: a transport failure takes the fallback the profile names, and the hop is on record", func(t *testing.T) {
		w := newWorld(t, withNamedFallback(transportOn...))
		w.ext.FailNext(pfake.FailUnavailable)
		w.proposeBook()
		res := w.say(receptionist(), sayBook)

		wantAct(t, res, "request_confirmation")
		if got := w.snd.sentTo(); len(got) != 2 || got[0] != w.profA || got[1] != w.profB {
			t.Fatalf("sent to %v, want profile A then its named fallback B", got)
		}
		o := w.lastOutcome()
		if !o.Served() || len(o.Hops) != 1 || o.Hops[0].To != string(w.profB) || o.Hops[0].Condition != modes.CondUnavailable {
			t.Fatalf("outcome %+v: a fallback must be recorded", o)
		}
	})

	t.Run("control: a local fallback is taken for the condition policy names, and recorded", func(t *testing.T) {
		w := newWorld(t, withLocalFallback(modes.CondInvalidCredential))
		w.creds.set(credRef(0), pv.CredentialRefStatusInvalid)
		o, _, sent, localRuns := w.routeDirect(receptionist(), false)

		if !o.Served() || o.Target != modes.TargetLocal || localRuns != 1 || sent != 0 {
			t.Fatalf("outcome %+v sent %d local runs %d", o, sent, localRuns)
		}
		if len(o.Hops) != 1 || o.Hops[0].To != modes.LocalHop || o.Hops[0].Condition != modes.CondInvalidCredential {
			t.Fatalf("hops %+v: a fallback must be recorded", o.Hops)
		}
	})

	// Every fallback policy could name for a provider that is up is named: none may apply to a failure of trust,
	// money or policy. A second profile is registered, named as the primary's fallback, and routed.
	failures := []struct {
		name  string
		opts  []opt
		setup func(w *world)
		// reason is what the registry itself says about profile A after the setup (empty: not a registry matter).
		reason providerreg.Reason
	}{
		{"invalid credential", nil, func(w *world) { w.creds.set(credRef(0), pv.CredentialRefStatusInvalid) }, providerreg.ReasonCredentialInvalid},
		{"credential service unreachable", nil, func(w *world) { w.creds.mu.Lock(); w.creds.err = errBoom; w.creds.mu.Unlock() }, providerreg.ReasonCredentialInvalid},
		{"terms changed", nil, func(w *world) {
			h := pv.SHA256("sha256:" + strings.Repeat("b", 64))
			if _, err := w.reg.Observe(ctxb(), receptionist(), w.profA, providerreg.Observation{TermsHash: &h}); err != nil {
				t.Fatal(err)
			}
		}, providerreg.ReasonTermsChanged},
		{"review stale", nil, func(w *world) { w.clk.Advance(45 * 24 * time.Hour) }, providerreg.ReasonStaleReview},
		{"operator hold", nil, func(w *world) {
			if _, err := w.reg.Hold(ctxb(), receptionist(), w.profA); err != nil {
				t.Fatal(err)
			}
		}, providerreg.ReasonOperatorHold},
		{"egress denied", []opt{withoutEgressPolicy()}, nil, ""},
		{"budget exhausted", []opt{withBudget(1)}, nil, ""},
	}
	for _, tc := range failures {
		t.Run(tc.name+": no alternate profile, no local run, no hop", func(t *testing.T) {
			opts := append([]opt{withNamedFallback(transportOn...), withLocalFallback(transportConditions...)}, tc.opts...)
			w := newWorld(t, opts...)
			if tc.setup != nil {
				tc.setup(w)
			}
			if tc.reason != "" {
				_, err := w.reg.Registry.Check(ctxb(), receptionist(), w.profA, providerreg.Requirement{Role: pv.RoleConversationalController, DataClass: pv.DataClassificationInternal})
				if providerreg.ReasonOf(err) != tc.reason {
					t.Fatalf("the registry says %q (%v), want %q: the setup did not create the failure under test", providerreg.ReasonOf(err), err, tc.reason)
				}
			}
			w.proposeBook()
			w.localProposeBook()
			res := w.say(receptionist(), sayBook)

			if !res.Failed() {
				t.Fatalf("act %s: the failure was papered over", res.Directive.Act)
			}
			w.nothingExecuted(res)
			if got := w.snd.sentTo(); slices.Contains(got, w.profB) {
				t.Fatalf("sent to %v: a request reached the fallback profile", got)
			}
			if w.reg.fallbackCount() != 0 {
				t.Fatalf("the registry was asked for a fallback %d times on a failure of trust, money or policy", w.reg.fallbackCount())
			}
			if w.local.Calls() != 0 {
				t.Fatalf("the local runtime was called %d times although no fallback is named for this failure", w.local.Calls())
			}
			if w.routed() > 0 {
				if o := w.lastOutcome(); len(o.Hops) != 0 || o.Target != modes.TargetNone {
					t.Fatalf("outcome %+v: no hop may be taken", o)
				}
			}
			if w.snd.sends() != 0 {
				t.Fatalf("%d requests were sent although the gate must have stopped them", w.snd.sends())
			}
		})
	}

	t.Run("the registry refuses to name an alternate for those conditions, whatever the caller says", func(t *testing.T) {
		w := newWorld(t, withNamedFallback(transportOn...))
		for _, c := range []providerreg.FallbackCondition{
			providerreg.FallbackOnInvalidCredential, providerreg.FallbackOnStaleTerms,
			providerreg.FallbackOnEgressDenied, providerreg.FallbackOnBudgetExhausted,
		} {
			_, _, err := w.reg.Registry.Fallback(ctxb(), receptionist(), w.profA, c, providerreg.Requirement{})
			if err == nil {
				t.Errorf("condition %s selected an alternate provider", c)
			}
		}
	})

	t.Run("a fallback is not taken for a transport condition the profile did not name", func(t *testing.T) {
		w := newWorld(t, withNamedFallback(pv.ProviderProfileFallbackOnItemRateLimited))
		w.ext.FailNext(pfake.FailUnavailable)
		w.proposeBook()
		res := w.say(receptionist(), sayBook)
		if !res.Failed() || slices.Contains(w.snd.sentTo(), w.profB) {
			t.Fatalf("failed %v sent to %v: the profile named the fallback for rate limits only", res.Failed(), w.snd.sentTo())
		}
	})
}
