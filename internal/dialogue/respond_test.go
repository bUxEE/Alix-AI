package dialogue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// respondRealiserSelector is a provider selector that also implements ResponseRealiser: it is what a
// composition root with external realisation gives as Deps.Providers.
type respondRealiserSelector struct {
	ProviderSelector
	calls  atomic.Int32
	handle bool
	out    verify.Outcome
	err    error
	seen   ResponseInput
}

func (r *respondRealiserSelector) RealiseResponse(_ context.Context, in ResponseInput) (verify.Outcome, bool, error) {
	r.calls.Add(1)
	r.seen = in
	return r.out, r.handle, r.err
}

func respondRealiserEnv(t *testing.T, r *respondRealiserSelector, cfg Config) *env {
	t.Helper()
	e := newEnv(t, withConfig(cfg), withDeps(func(d *Deps) {
		r.ProviderSelector = d.Providers
		d.Providers = r
	}))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
	e.onResponse("Sure. How many would you like?")
	return e
}

// Without a realiser, or with one that declines, the response step is what it always was; a realiser that
// takes the turn writes the response (its model text is verified again here), an unverifiable text or an
// error is the deterministic template, and an act a model has nothing to add to never reaches it.
func TestResponseRealiserSeamDefaultsToUnchangedBehaviour(t *testing.T) {
	const external = "Certainly. How many would you like?"

	t.Run("no realiser: the local model writes the response", func(t *testing.T) {
		e := newEnv(t)
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		e.onResponse("Sure. How many would you like?")
		res := e.say(memberScope(), sayCreate)
		wantAct(t, res, "ask_missing")
		if res.Response.Source != verify.SourceModel || res.Response.Text != "Sure. How many would you like?" || len(e.prompts()) != 2 {
			t.Fatalf("response %+v, %d model calls", res.Response, len(e.prompts()))
		}
		if _, ok := e.eng.providers.(ResponseRealiser); ok {
			t.Fatal("the default selector implements the realiser")
		}
	})

	t.Run("a realiser that declines changes nothing", func(t *testing.T) {
		r := &respondRealiserSelector{}
		e := respondRealiserEnv(t, r, Config{})
		res := e.say(memberScope(), sayCreate)
		if r.calls.Load() != 1 {
			t.Fatalf("the realiser was asked %d times, want once", r.calls.Load())
		}
		if res.Response.Source != verify.SourceModel || res.Response.Text != "Sure. How many would you like?" || len(e.prompts()) != 2 {
			t.Fatalf("response %+v, %d model calls", res.Response, len(e.prompts()))
		}
		if r.seen.Directive == nil || string(r.seen.Directive.Act) != "ask_missing" || r.seen.Scope.TenantID != memberScope().TenantID {
			t.Fatalf("the realiser was given %+v", r.seen)
		}
	})

	t.Run("a realiser that takes the turn writes the response and the local model is not asked for it", func(t *testing.T) {
		r := &respondRealiserSelector{handle: true, out: verify.Outcome{Text: external, Source: verify.SourceModel, Reason: verify.ReasonVerified}}
		e := respondRealiserEnv(t, r, Config{})
		res := e.say(memberScope(), sayCreate)
		if res.Response.Source != verify.SourceModel || res.Response.Text != external {
			t.Fatalf("response %+v", res.Response)
		}
		if n := len(e.prompts()); n != 1 {
			t.Fatalf("%d model calls, want the proposal only", n)
		}
		last := res.Trace.ModelCalls[len(res.Trace.ModelCalls)-1]
		if last.Purpose != "response" || last.Provider != "realiser" || last.Err != "" {
			t.Fatalf("trace %+v", last)
		}
	})

	t.Run("unverified model text from a realiser is never shown", func(t *testing.T) {
		r := &respondRealiserSelector{handle: true, out: verify.Outcome{Text: "Your appointment is booked for 9999.", Source: verify.SourceModel, Reason: verify.ReasonVerified}}
		e := respondRealiserEnv(t, r, Config{})
		res := e.say(memberScope(), sayCreate)
		if res.Response.Source != verify.SourceTemplate || res.Response.Reason != verify.ReasonVerificationFailed {
			t.Fatalf("response %+v", res.Response)
		}
	})

	t.Run("a realiser error is the template, not another answer", func(t *testing.T) {
		r := &respondRealiserSelector{handle: true, err: errors.New("provider down")}
		e := respondRealiserEnv(t, r, Config{})
		res := e.say(memberScope(), sayCreate)
		if res.Response.Source != verify.SourceTemplate || res.Response.Text == "" {
			t.Fatalf("response %+v", res.Response)
		}
		if len(e.prompts()) != 1 {
			t.Fatalf("%d model calls: the local model answered after the realiser failed", len(e.prompts()))
		}
	})

	t.Run("a template-only act never reaches the realiser", func(t *testing.T) {
		r := &respondRealiserSelector{handle: true, out: verify.Outcome{Text: external, Source: verify.SourceModel, Reason: verify.ReasonVerified}}
		e := respondRealiserEnv(t, r, Config{TemplateOnly: []conversation.DialogueDirectiveAct{conversation.DialogueDirectiveActAskMissing}})
		res := e.say(memberScope(), sayCreate)
		if r.calls.Load() != 0 || res.Response.Source != verify.SourceTemplate {
			t.Fatalf("realiser asked %d times, response %+v", r.calls.Load(), res.Response)
		}
	})
}

// replyHookRecorder is a ReplyHook that records what it was shown.
type replyHookRecorder struct {
	mu    sync.Mutex
	calls []replyHookCall
}

type replyHookCall struct {
	scope contracts.Scope
	out   verify.Outcome
}

func (r *replyHookRecorder) hook(_ context.Context, scope contracts.Scope, out verify.Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, replyHookCall{scope: scope, out: out})
}

func (r *replyHookRecorder) seen() []replyHookCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]replyHookCall(nil), r.calls...)
}

// wantHookSawReply checks that the hook was called exactly once, with the scope of the turn and exactly the
// outcome the caller of the turn received.
func wantHookSawReply(t *testing.T, rec *replyHookRecorder, res Result) {
	t.Helper()
	got := rec.seen()
	if len(got) != 1 {
		t.Fatalf("the hook was called %d times, want once", len(got))
	}
	if diff := cmp.Diff(res.Response, got[0].out); diff != "" {
		t.Fatalf("the hook saw a different reply than the caller (-caller +hook):\n%s", diff)
	}
	if got[0].scope.TenantID != memberScope().TenantID || got[0].scope.SessionID != memberScope().SessionID {
		t.Fatalf("the hook saw scope %+v", got[0].scope)
	}
	if strings.TrimSpace(got[0].out.Text) == "" {
		t.Fatal("the hook saw an empty reply")
	}
}

// The hook after verify.Realise sees the final reply text of every kind of turn: the local model's verified text, a
// text a ResponseRealiser took, the template that replaced a text that failed the verifier, the template of a turn
// the model could not serve and the template of an act a model has nothing to add to. It never sees a reply of a
// turn that was not delivered.
func TestRealisedTextReachesTheAttestHookForEveryTurn(t *testing.T) {
	const external = "Certainly. How many would you like?"
	withRealiser := func(rec *replyHookRecorder, r *respondRealiserSelector) envOpt {
		return withDeps(func(d *Deps) {
			r.ProviderSelector = d.Providers
			d.Providers = r
			d.OnReply = rec.hook
		})
	}

	t.Run("the local model's verified text", func(t *testing.T) {
		rec := &replyHookRecorder{}
		e := newEnv(t, withDeps(func(d *Deps) { d.OnReply = rec.hook }))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		e.onResponse("Sure. How many would you like?")
		res := e.say(memberScope(), sayCreate)
		if res.Response.Source != verify.SourceModel {
			t.Fatalf("response %+v", res.Response)
		}
		wantHookSawReply(t, rec, res)
	})

	t.Run("a text a realiser took", func(t *testing.T) {
		rec := &replyHookRecorder{}
		r := &respondRealiserSelector{handle: true, out: verify.Outcome{Text: external, Source: verify.SourceModel, Reason: verify.ReasonVerified}}
		e := newEnv(t, withRealiser(rec, r))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		res := e.say(memberScope(), sayCreate)
		if res.Response.Text != external {
			t.Fatalf("response %+v", res.Response)
		}
		wantHookSawReply(t, rec, res)
	})

	t.Run("a realiser that declines: the local text is announced", func(t *testing.T) {
		rec := &replyHookRecorder{}
		e := newEnv(t, withRealiser(rec, &respondRealiserSelector{}))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		e.onResponse("Sure. How many would you like?")
		wantHookSawReply(t, rec, e.say(memberScope(), sayCreate))
	})

	t.Run("a realiser text that fails the verifier: the hook sees the template, not the text", func(t *testing.T) {
		const invented = "Your appointment is booked for 9999."
		rec := &replyHookRecorder{}
		r := &respondRealiserSelector{handle: true, out: verify.Outcome{Text: invented, Source: verify.SourceModel, Reason: verify.ReasonVerified}}
		e := newEnv(t, withRealiser(rec, r))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		res := e.say(memberScope(), sayCreate)
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response %+v", res.Response)
		}
		wantHookSawReply(t, rec, res)
		if strings.Contains(rec.seen()[0].out.Text, invented) {
			t.Fatal("unverified model text reached the hook")
		}
	})

	t.Run("a model that is down: the template", func(t *testing.T) {
		rec := &replyHookRecorder{}
		e := newEnv(t, withDeps(func(d *Deps) { d.OnReply = rec.hook }))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		e.model.On(pfake.Constrained(providers.ConstraintNone)).Fail(pfake.FailUnavailable)
		res := e.say(memberScope(), sayCreate)
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response %+v", res.Response)
		}
		wantHookSawReply(t, rec, res)
	})

	t.Run("a template-only act: a refusal", func(t *testing.T) {
		rec := &replyHookRecorder{}
		e := newEnv(t, withDeps(func(d *Deps) { d.OnReply = rec.hook }))
		e.onProposal(proposalJSON("items.destroy_all", "task", map[string]any{"name": "widget"}))
		res := e.say(memberScope(), sayCreate)
		wantAct(t, res, "refuse")
		wantHookSawReply(t, rec, res)
	})

	t.Run("a turn that returns an error announces nothing", func(t *testing.T) {
		rec := &replyHookRecorder{}
		e := newEnv(t, withDeps(func(d *Deps) { d.OnReply = rec.hook }))
		if _, err := e.eng.Turn(ctxb(), contracts.Scope{}, TurnInput{Text: sayCreate, Language: "en"}); err == nil {
			t.Fatal("a turn without a scope succeeded")
		}
		if n := len(rec.seen()); n != 0 {
			t.Fatalf("the hook was called %d times for a turn that failed", n)
		}
	})
}

// With the hook nil the engine is what it was; a hook cannot change a reply, and one that panics does not fail the
// turn.
func TestNilAttestHookChangesNothing(t *testing.T) {
	run := func(hook ReplyHook) (Result, int) {
		e := newEnv(t, withDeps(func(d *Deps) { d.OnReply = hook }))
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
		e.onResponse("Sure. How many would you like?")
		res := e.say(memberScope(), sayCreate)
		return res, len(e.prompts())
	}
	plain, plainCalls := run(nil)
	if plain.Response.Source != verify.SourceModel || plain.Response.Text == "" {
		t.Fatalf("response %+v", plain.Response)
	}

	var seen verify.Outcome
	hooked, hookedCalls := run(func(_ context.Context, _ contracts.Scope, out verify.Outcome) {
		seen = out
		out.Text = "tampered" // a copy: the hook cannot change the reply
	})
	panicked, panickedCalls := run(func(context.Context, contracts.Scope, verify.Outcome) { panic("hook bug") })

	for name, got := range map[string]Result{"a hook": hooked, "a panicking hook": panicked} {
		if got.Response.Text != plain.Response.Text || got.Response.Source != plain.Response.Source ||
			got.Code != plain.Code || got.Directive.Act != plain.Directive.Act {
			t.Fatalf("%s changed the turn: %+v vs %+v", name, got.Response, plain.Response)
		}
	}
	if seen.Text != plain.Response.Text || seen.Source != plain.Response.Source || seen.Reason != plain.Response.Reason {
		t.Fatalf("the hook saw %+v, want %+v", seen, plain.Response)
	}
	if plainCalls != hookedCalls || plainCalls != panickedCalls {
		t.Fatalf("model calls %d / %d / %d: a hook changed the work done", plainCalls, hookedCalls, panickedCalls)
	}
	if e := newEnv(t); e.eng.onReply != nil {
		t.Fatal("an engine built without the hook has one")
	}
}
