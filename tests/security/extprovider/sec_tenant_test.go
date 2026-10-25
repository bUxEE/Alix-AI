package extprovidersec

import (
	"context"
	"encoding/json"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/dialogue/extmode"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// SEC-116
func TestSecExtCrossTenantRequestMixingImpossible(t *testing.T) {
	a, b := receptionist(), tenantTwo()

	t.Run("two tenants interleaved: every provider call carries one tenant's text and its own scope", func(t *testing.T) {
		w := newWorld(t, withTenantTwo())
		for i := 0; i < 3; i++ {
			w.propose("availability_list", `{"date":"2026-10-14"}`)
			w.propose("availability_list", `{"date":"2026-10-14"}`)
			w.say(a, sayFree+" TENANTA-MARK")
			w.say(b, sayFree+" TENANTB-MARK")
		}
		seenA, seenB := 0, 0
		for _, p := range w.ext.Prompts() {
			hasA, hasB := p.Contains("TENANTA-MARK"), p.Contains("TENANTB-MARK")
			if hasA && hasB {
				t.Fatal("one prompt holds two tenants' text")
			}
			if hasA {
				seenA++
			}
			if hasB {
				seenB++
			}
			if hasA && p.Scope.TenantID != a.TenantID || hasB && p.Scope.TenantID != b.TenantID {
				t.Fatalf("a prompt of tenant %s carries the other tenant's text", p.Scope.TenantID)
			}
		}
		if seenA == 0 || seenB == 0 {
			t.Fatalf("control: tenant A marks %d tenant B marks %d: both must reach their own provider call", seenA, seenB)
		}
		for _, s := range w.snd.all() {
			if s.Scope.TenantID == "" || s.Req.Call.Envelope == nil {
				t.Fatalf("a send without a scope or an envelope: %+v", s.Scope)
			}
		}

		// Every send left an egress decision in its own tenant's audit chain, with metadata and no content.
		rows, err := w.db.QueryContext(ctxb(), `SELECT tenant_id, data FROM audit_events WHERE event_type = 'dataegress.decision'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		byDecision := map[string]string{}
		for rows.Next() {
			var tenant, data string
			if err := rows.Scan(&tenant, &data); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(data, "TENANTA-MARK") || strings.Contains(data, "TENANTB-MARK") || strings.Contains(data, "Which appointments") {
				t.Fatalf("an egress decision holds content: %s", data)
			}
			var d struct {
				DecisionID string `json:"decision_id"`
				Outcome    string `json:"outcome"`
			}
			if err := json.Unmarshal([]byte(data), &d); err != nil || d.Outcome != "allow" {
				t.Fatalf("decision %s: %v", data, err)
			}
			byDecision[d.DecisionID] = tenant
		}
		for _, s := range w.snd.all() {
			if got := byDecision[string(s.Req.Call.Decision.DecisionID)]; got != s.Scope.TenantID {
				t.Fatalf("the decision of a send for tenant %s is in the chain of %q", s.Scope.TenantID, got)
			}
		}
	})

	t.Run("a request released for one tenant cannot be sent under another, and cannot be sent twice", func(t *testing.T) {
		w := newWorld(t, withTenantTwo())
		in := dialogue.GuardInput{
			Scope: a, Provider: dialogue.ProviderInfo{Name: extmode.ExternalName},
			Request: dialogue.PromptRequest{
				Role: "controller",
				Items: []dialogue.ContextItem{
					{Role: "system", Trusted: true, Content: "You are the controller.", Meta: dialogue.ItemMeta{Kind: dialogue.ItemFrame, SourceID: "core", Class: dialogue.ClassInternal}},
					{Role: "user", Content: "hello TENANTA-1", Meta: dialogue.ItemMeta{Kind: dialogue.ItemUserMessage, SourceID: "user", Class: dialogue.ClassConfidential}},
				},
				MaxTokens: 64,
			},
		}
		released, err := w.rt.Guard(ctxb(), in)
		if err != nil {
			t.Fatal(err)
		}
		gen, err := w.rt.Select(ctxb(), a, providers.RoleController)
		if err != nil || gen.Local {
			t.Fatalf("Select: local=%v err=%v, want the external route", gen.Local, err)
		}
		chat := released.ChatRequest()
		if _, err := gen.Generator.Chat(ctxb(), b, chat); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Fatalf("another tenant sent a request released for tenant A: %v", err)
		}
		if w.ext.Calls() != 0 || w.snd.sends() != 0 {
			t.Fatal("the provider was called for a request presented by the wrong tenant")
		}
		w.ext.On(anyPrompt()).Reply("ok").Times(1)
		if _, err := gen.Generator.Chat(ctxb(), a, chat); err != nil {
			t.Fatalf("control: the owner could not send its own released request: %v", err)
		}
		if _, err := gen.Generator.Chat(ctxb(), a, chat); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Fatalf("a released request was sent twice: %v", err)
		}
	})

	t.Run("a provider profile permitted for one tenant is refused for the other, with nothing sent", func(t *testing.T) {
		w := newWorld(t, withTenantTwo(), withProfilePermittedForTenantOnly())
		healthy(t, w) // control: the permitted tenant is served by the same profile
		sends := w.snd.sends()
		w.propose("availability_list", `{"date":"2026-10-14"}`)
		res := w.say(b, sayFree)
		if !res.Failed() {
			t.Fatalf("act %s: tenant B was served by a profile that is not permitted for it", res.Directive.Act)
		}
		if w.snd.sends() != sends {
			t.Fatal("a request of tenant B was sent")
		}
		if _, err := w.reg.Registry.Check(ctxb(), b, w.profA, requirementFor()); err == nil {
			t.Fatal("the registry admitted the profile for tenant B")
		}
	})

	t.Run("evidence retrieved for another tenant is refused, and another tenant's source policy releases nothing to this one", func(t *testing.T) {
		w := newWorld(t, withTenantTwo())
		w.enableKnowledge()
		srcA := w.newSource(a, pv.EgressModeAllowExcerpt, pv.DataClassificationInternal)
		pkgB := pkgFor(b, sayQuestion, evItem{source: "Other tenant", text: "TENANTB-SECRET text", id: srcA, egress: rag.SourceEgressAllowExcerpt, class: rag.SourceClassificationInternal})
		if _, err := w.ask(a, pkgB); contracts.CodeOf(err) != contracts.ErrScopeViolation {
			t.Fatalf("err = %v, want scope_violation for a package built for tenant B", err)
		}
		if w.ext.Calls() != 0 || w.promptsContain("TENANTB-SECRET") {
			t.Fatal("another tenant's text reached the provider")
		}

		// Tenant B's own source policy is invisible to tenant A: a package of A that names B's source is local_only.
		srcB := w.newSource(b, pv.EgressModeAllowExcerpt, pv.DataClassificationInternal)
		pkgA := pkgFor(a, sayQuestion, evItem{source: "B source", text: "The clinic building opens at nine in the morning. TENANTB-ONLY", id: srcB, egress: rag.SourceEgressAllowExcerpt, class: rag.SourceClassificationInternal})
		res, err := w.ask(a, pkgA)
		if err != nil {
			t.Fatal(err)
		}
		evidenceRestricted(w, res)
		if w.promptsContain("TENANTB-ONLY") {
			t.Fatal("a source policy of tenant B released its text to tenant A's request")
		}
		// Control: tenant B asking about its own source is answered.
		pkgOwn := pkgFor(b, sayQuestion, evItem{source: "B source", text: "The clinic building opens at nine in the morning. TENANTB-ONLY", id: srcB, egress: rag.SourceEgressAllowExcerpt, class: rag.SourceClassificationInternal})
		w.answerWith(claimJSON{Text: "The clinic building opens at nine in the morning.", IDs: []string{"e1"}})
		if own, err := w.ask(b, pkgOwn); err != nil {
			t.Fatal(err)
		} else if _, ok := own.Value.(rag.GroundedAnswer); !ok {
			t.Fatalf("control: tenant B own source gave %T %+v", own.Value, own.Value)
		}
	})

	t.Run("a reservation made for one tenant cannot be settled or released by another", func(t *testing.T) {
		w := newWorld(t, withTenantTwo())
		var otherErr, otherRelease error
		o, err := w.rt.Router().Execute(ctxb(), modes.Request{
			Scope: a, Role: modes.RoleController, Candidate: w.baseCandidate(), Requirement: requirementFor(),
			Estimate: func(pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
				return providerbudget.Units{ToksIn: 10, ToksOut: 5}, callCost, nil
			},
		}, modes.Executors{
			Local: func(context.Context) error { return nil },
			External: func(_ context.Context, call modes.ExternalCall) (modes.ExternalResult, error) {
				_, otherErr = w.budget.Settle(ctxb(), b, call.Reservation.ID, providerActual())
				otherRelease = w.budget.Release(ctxb(), b, call.Reservation.ID)
				return modes.ExternalResult{Metering: providerbudget.Units{ToksIn: 8, ToksOut: 4}}, nil
			},
		})
		if err != nil || !o.Served() {
			t.Fatalf("control: the owner's call: %+v %v", o, err)
		}
		if otherErr == nil || otherRelease == nil {
			t.Fatalf("tenant B settled (%v) or released (%v) a reservation of tenant A", otherErr, otherRelease)
		}
		if got, ok := w.budget.lastSettled(); !ok || got.Outcome != pv.UsageRecordOutcomeSucceeded {
			t.Fatalf("settlement %+v: the owner's own settlement must stand", got)
		}
	})

	t.Run("the request that goes to the provider adapter never carries the identity of the user, the tenant or the session", func(t *testing.T) {
		w := newWorld(t, withTenantTwo())
		w.propose("availability_list", `{"date":"2026-10-14"}`)
		w.say(b, sayFree)
		for _, s := range w.snd.all() {
			wireBytes, _ := json.Marshal(s.Req.Rich)
			wire := string(wireBytes)
			// The application name is in the instructions by design; no other identifier is.
			for _, id := range []string{b.TenantID, b.PrincipalID, b.SessionID, b.WorkspaceID, b.DeploymentID} {
				if id != "" && strings.Contains(wire, id) {
					t.Fatalf("the request sent to the provider carries %q", id)
				}
			}
		}
		if w.snd.sends() == 0 {
			t.Fatal("control: nothing was sent")
		}
	})
}
