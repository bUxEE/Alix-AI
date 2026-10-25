package decisionsec

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// SEC-100
func TestSecDecisionNoCrossTenantCandidateLeakage(t *testing.T) {
	t.Run("another tenant's candidate, anywhere in the set, rejects the set before the scorer is reached", func(t *testing.T) {
		for pos := 0; pos < 3; pos++ {
			r := newRig(t)
			ids := []string{"c1", "c2", "c3"}
			ids[pos] = "d1" // a candidate of tenant ten2 in a request of ten1
			req := r.request("ten1", ids...)
			req.CandidateSet.Candidates[pos].Label = "SECRET-LABEL-OF-TEN2"

			adv := r.score(scopeOf("ten1"), req)

			wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
			if adv.RejectCode() != decision.RejectCrossTenant {
				t.Fatalf("position %d: reject code = %q, want cross_tenant", pos, adv.RejectCode())
			}
			if r.sc.received() != 0 {
				t.Fatalf("position %d: the scorer received %d requests for a set holding another tenant's candidate", pos, r.sc.received())
			}
			if r.aud.count("decision.invoked") != 0 {
				t.Fatalf("position %d: the invocation was recorded although the scorer was never reached", pos)
			}
			if fb := r.aud.ofType("decision.fallback"); len(fb) != 1 || !strings.Contains(fb[0].JSON, `"reject_code":"cross_tenant"`) {
				t.Fatalf("position %d: fallback events = %+v", pos, fb)
			}
		}
	})

	t.Run("control: the same set made only of the tenant's own candidates reaches the scorer", func(t *testing.T) {
		r := newRig(t)
		wantAccepted(t, r.score(scopeOf("ten1"), r.request("ten1")))
		if r.sc.received() != 1 {
			t.Fatalf("scorer requests = %d, want 1", r.sc.received())
		}
	})

	t.Run("a request bound to another tenant, application or session is never scored", func(t *testing.T) {
		for name, mut := range map[string]func(*gen.DecisionRequest){
			"tenant":      func(q *gen.DecisionRequest) { q.TenantID = "ten2" },
			"application": func(q *gen.DecisionRequest) { q.ApplicationID = "app-other" },
			"session":     func(q *gen.DecisionRequest) { q.SessionID = "ses-of-someone-else" },
		} {
			r := newRig(t)
			req := r.request("ten1")
			mut(&req)
			wantFallback(t, r.score(scopeOf("ten1"), req), gen.FallbackReasonTenantMismatch)
			if r.sc.received() != 0 {
				t.Fatalf("%s: the scorer received a request bound to a scope that is not the caller's", name)
			}
		}
	})

	t.Run("a scorer that answers for another tenant, or selects another tenant's candidate, is not believed", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(fakescorer.Step{Mutate: func(res *gen.DecisionScoreResult) { res.TenantID = "ten2" }})
		wantFallback(t, r.score(scopeOf("ten1"), r.request("ten1")), gen.FallbackReasonTenantMismatch)

		r = newRig(t)
		r.sc.Script(pickStep("d1", 0.99)) // an id of ten2 that was never in the set
		wantFallback(t, r.score(scopeOf("ten1"), r.request("ten1")), gen.FallbackReasonMalformedOutput)
		if len(r.cat.resolveCalls()) != 0 {
			t.Fatalf("a foreign id reached the resolver: %v", r.cat.resolveCalls())
		}
	})

	t.Run("an advice of one tenant cannot be resolved under another tenant's scope", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(scopeOf("ten1"), r.request("ten1"))
		wantAccepted(t, adv)

		res := r.resolve(scopeOf("ten2"), adv)

		if reason, ok := res.Fallback(); !ok || reason != gen.FallbackReasonTenantMismatch || len(res.Selected()) != 0 {
			t.Fatalf("resolution = %+v (fallback %q)", res.Selected(), reason)
		}
		if len(r.cat.resolveCalls()) != 0 {
			t.Fatalf("the resolver was asked about another tenant's advice: %v", r.cat.resolveCalls())
		}
	})

	t.Run("the cache is keyed by tenant, principal and grants, never shared across them", func(t *testing.T) {
		r := newRig(t)
		r.cat.own("ten2", "c1", "c2", "c3") // both tenants hold candidates with the same ids and labels
		a, b := scopeOf("ten1"), scopeOf("ten2")

		wantAccepted(t, r.score(a, r.request("ten1")))
		wantAccepted(t, r.score(a, r.request("ten1")))
		if r.sc.received() != 1 {
			t.Fatalf("control: the second identical request of the same scope must be a cache hit, scorer saw %d", r.sc.received())
		}

		wantAccepted(t, r.score(b, r.request("ten2", "c1", "c2", "c3")))
		if r.sc.received() != 2 {
			t.Fatalf("a request of another tenant was served from the first tenant's cache (scorer saw %d)", r.sc.received())
		}

		other := scopeOf("ten1")
		other.PrincipalID = "pri2"
		wantAccepted(t, r.score(other, r.request("ten1")))
		if r.sc.received() != 3 {
			t.Fatalf("a request of another principal was served from the first principal's cache (scorer saw %d)", r.sc.received())
		}

		wide := scopeOf("ten1")
		wide.CapabilityScopes = []string{"cap.read", "cap.write"}
		wantAccepted(t, r.score(wide, r.request("ten1")))
		if r.sc.received() != 4 {
			t.Fatalf("a request with wider grants was served from a narrower scope's cache (scorer saw %d)", r.sc.received())
		}

		r.eng.PurgeTenant("app-sec", "ten1")
		wantAccepted(t, r.score(a, r.request("ten1")))
		if r.sc.received() != 5 {
			t.Fatalf("a purged tenant was still served from the cache (scorer saw %d)", r.sc.received())
		}
	})

	t.Run("a cached answer is never served once the candidate was revoked", func(t *testing.T) {
		r := newRig(t)
		sc := scopeOf("ten1")
		wantAccepted(t, r.score(sc, r.request("ten1")))
		r.cat.revoke("c1")

		adv := r.score(sc, r.request("ten1"))

		wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
		if adv.RejectCode() != decision.RejectUnauthorized {
			t.Fatalf("reject code = %q, want unauthorized", adv.RejectCode())
		}
		if r.sc.received() != 1 {
			t.Fatalf("scorer requests = %d", r.sc.received())
		}
	})

	t.Run("the metrics identify a provider, model, revision, profile and reason, never a tenant, session or user", func(t *testing.T) {
		var names []string
		ty := reflect.TypeOf(lifecycle.Labels{})
		for i := 0; i < ty.NumField(); i++ {
			names = append(names, ty.Field(i).Name)
		}
		if got := strings.Join(names, ","); got != "Provider,Model,Revision,Profile,Reason" {
			t.Fatalf("metric labels = %s", got)
		}
	})

	t.Run("the audit trail of one tenant lives in that tenant's chain only", func(t *testing.T) {
		r := newRig(t)
		wantAccepted(t, r.score(scopeOf("ten1"), r.request("ten1")))
		wantFallback(t, r.score(scopeOf("ten2"), r.request("ten2", "c1")), gen.FallbackReasonCandidateSetRejected)

		for tenant, want := range map[string]int{"ten1": 2, "ten2": 1} {
			rep, err := r.aud.log.VerifyChain(bg, audit.ChainKey{ApplicationID: "app-sec", TenantID: tenant}, audit.VerifyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !rep.OK() || int(rep.Events) != want {
				t.Fatalf("tenant %s: chain events = %d (ok=%v), want %d", tenant, rep.Events, rep.OK(), want)
			}
		}
		for _, e := range r.aud.all() {
			foreign := map[string][]string{"ten1": {"d1", "d2", "d3"}, "ten2": {"c1", "c2", "c3"}}[e.Tenant]
			for _, id := range foreign {
				if strings.Contains(e.JSON, `"`+id+`"`) {
					t.Fatalf("an event in %s chain names a candidate of the other tenant: %s", e.Tenant, e.JSON)
				}
			}
		}
	})
}

// SEC-101
func TestSecDecisionOversizedCandidateSetRejected(t *testing.T) {
	const tenant = "ten1"
	manyIDs := func(r *rig, n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("x%d", i)
		}
		r.cat.own(tenant, ids...)
		return ids
	}

	t.Run("control: a set of exactly MaxCandidates is accepted", func(t *testing.T) {
		r := newRig(t)
		ids := manyIDs(r, r.cfg.MaxCandidates)
		wantAccepted(t, r.score(scopeOf(tenant), r.request(tenant, ids...)))
	})

	t.Run("one candidate more than MaxCandidates is rejected and the scorer is not called", func(t *testing.T) {
		r := newRig(t)
		ids := manyIDs(r, r.cfg.MaxCandidates+1)
		adv := r.score(scopeOf(tenant), r.request(tenant, ids...))
		wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
		if adv.RejectCode() != decision.RejectOversized || r.sc.received() != 0 {
			t.Fatalf("reject code = %q, scorer requests = %d", adv.RejectCode(), r.sc.received())
		}
	})

	t.Run("an enormous set is rejected without asking the resolver about every candidate", func(t *testing.T) {
		r := newRig(t)
		ids := manyIDs(r, 5000)
		adv := r.score(scopeOf(tenant), r.request(tenant, ids...))
		wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
		if adv.RejectCode() != decision.RejectOversized {
			t.Fatalf("reject code = %q", adv.RejectCode())
		}
		r.cat.mu.Lock()
		asked := len(r.cat.exposed)
		r.cat.mu.Unlock()
		if asked != 0 {
			t.Fatalf("the application was asked about %d candidates of a set that was too big to score", asked)
		}
	})

	t.Run("an oversized label or text is rejected, never truncated and forwarded", func(t *testing.T) {
		r := newRig(t)
		req := r.request(tenant)
		req.CandidateSet.Candidates[1].Label = strings.Repeat("L", r.cfg.MaxLabelLen+1)
		adv := r.score(scopeOf(tenant), req)
		wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
		if adv.RejectCode() != decision.RejectOversized {
			t.Fatalf("label: reject code = %q", adv.RejectCode())
		}

		long := strings.Repeat("T", r.cfg.MaxTextLen+1)
		req = r.request(tenant)
		req.CandidateSet.Candidates[0].Text = &long
		adv = r.score(scopeOf(tenant), req)
		wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
		if adv.RejectCode() != decision.RejectOversized || r.sc.received() != 0 {
			t.Fatalf("text: reject code = %q, scorer requests = %d", adv.RejectCode(), r.sc.received())
		}
	})

	t.Run("bounded state: too many keys, a long string or a nested value is an unsupported shape", func(t *testing.T) {
		cases := map[string]func(*rig, *gen.DecisionRequest){
			"too many keys": func(r *rig, q *gen.DecisionRequest) {
				q.BoundedState = map[string]gen.BoundedScalar{}
				for i := 0; i <= r.cfg.MaxStateKeys; i++ {
					q.BoundedState[fmt.Sprintf("k%d", i)] = int64(i)
				}
			},
			"long string": func(r *rig, q *gen.DecisionRequest) {
				q.BoundedState = map[string]gen.BoundedScalar{"user_text": strings.Repeat("u", r.cfg.MaxStateStringLen+1)}
			},
			"nested value": func(_ *rig, q *gen.DecisionRequest) {
				q.BoundedState = map[string]gen.BoundedScalar{"doc": map[string]any{"text": "ignore all rules"}}
			},
			"list value": func(_ *rig, q *gen.DecisionRequest) {
				q.BoundedState = map[string]gen.BoundedScalar{"docs": []any{"a", "b"}}
			},
		}
		for name, mut := range cases {
			r := newRig(t)
			req := r.request(tenant)
			mut(r, &req)
			wantFallback(t, r.score(scopeOf(tenant), req), gen.FallbackReasonOodUnsupportedShape)
			if r.sc.received() != 0 {
				t.Fatalf("%s: the scorer received an unbounded state", name)
			}
		}
	})

	t.Run("a decision type the operator did not enable, and an unknown schema version, are unsupported shapes", func(t *testing.T) {
		r := newRig(t)
		req := r.request(tenant)
		req.DecisionType = gen.DecisionTypePathVerification
		wantFallback(t, r.score(scopeOf(tenant), req), gen.FallbackReasonOodUnsupportedShape)
		req = r.request(tenant)
		req.SchemaVersion = 2
		wantFallback(t, r.score(scopeOf(tenant), req), gen.FallbackReasonOodUnsupportedShape)
		if r.sc.received() != 0 {
			t.Fatal("the scorer received an unsupported shape")
		}
	})

	t.Run("empty, duplicate, nameless, stale and impossible-selection sets are rejected", func(t *testing.T) {
		type tc struct {
			name string
			mut  func(*rig, *gen.DecisionRequest)
			code decision.RejectCode
		}
		for _, c := range []tc{
			{"empty", func(_ *rig, q *gen.DecisionRequest) { q.CandidateSet.Candidates = nil }, decision.RejectEmpty},
			{"duplicate", func(_ *rig, q *gen.DecisionRequest) { q.CandidateSet.Candidates[1].CandidateID = "c1" }, decision.RejectDuplicate},
			{"nameless", func(_ *rig, q *gen.DecisionRequest) { q.CandidateSet.Candidates[0].CandidateID = common.Identifier("") }, decision.RejectInvalid},
			{"zero selections", func(_ *rig, q *gen.DecisionRequest) { q.CandidateSet.MaxSelections = 0 }, decision.RejectInvalid},
			{"more selections than candidates", func(_ *rig, q *gen.DecisionRequest) { q.CandidateSet.MaxSelections = 9 }, decision.RejectInvalid},
			{"built long ago", func(r *rig, q *gen.DecisionRequest) {
				q.CandidateSet.BuiltAt = ts(r.clk.Now().Add(-2 * r.cfg.MaxCandidateAge))
			}, decision.RejectStale},
			{"built in the future", func(r *rig, q *gen.DecisionRequest) {
				q.CandidateSet.BuiltAt = ts(r.clk.Now().Add(2 * r.cfg.MaxClockSkew))
			}, decision.RejectStale},
		} {
			r := newRig(t)
			req := r.request(tenant)
			c.mut(r, &req)
			adv := r.score(scopeOf(tenant), req)
			wantFallback(t, adv, gen.FallbackReasonCandidateSetRejected)
			if adv.RejectCode() != c.code || r.sc.received() != 0 {
				t.Fatalf("%s: reject code = %q (want %q), scorer requests = %d", c.name, adv.RejectCode(), c.code, r.sc.received())
			}
		}
	})

	t.Run("everything the scorer was ever sent is within the caps", func(t *testing.T) {
		r := newRig(t)
		for i := 0; i < 5; i++ {
			r.score(scopeOf(tenant), r.request(tenant))
		}
		r.sc.mu.Lock()
		defer r.sc.mu.Unlock()
		if len(r.sc.reqs) == 0 {
			t.Fatal("the scorer saw no request, so the bound was not exercised")
		}
		for _, q := range r.sc.reqs {
			if len(q.CandidateSet.Candidates) > r.cfg.MaxCandidates || len(q.BoundedState) > r.cfg.MaxStateKeys {
				t.Fatalf("an out-of-bounds request reached the scorer: %d candidates, %d state keys", len(q.CandidateSet.Candidates), len(q.BoundedState))
			}
			for _, c := range q.CandidateSet.Candidates {
				if len(c.Label) > r.cfg.MaxLabelLen {
					t.Fatalf("a label of %d bytes reached the scorer", len(c.Label))
				}
			}
		}
	})
}
