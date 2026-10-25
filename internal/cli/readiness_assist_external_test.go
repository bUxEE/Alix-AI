//go:build !noexternal

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/readiness/assist"
	"github.com/buxee/alix-ai/internal/readiness/scan"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// rdyExtGuard is a fake data-egress guard. It records every candidate and builds a realistic result: an envelope with the text of
// the parts it keeps, and one outcome per part.
type rdyExtGuard struct {
	mu    sync.Mutex
	cands []dataegress.Candidate
	err   error
	// drop removes a part (message index, part index, the part).
	drop func(mi, pi int, p dataegress.Part) bool
	// rewrite changes the text of a kept part, as the scrubber does.
	rewrite func(string) string
	// instructions, when set, replaces the instructions in the envelope.
	instructions *string
	noEnvelope   bool
	// corrupt makes the outcomes point at a position the envelope does not have.
	corrupt bool
}

func (g *rdyExtGuard) Evaluate(_ context.Context, c dataegress.Candidate) (dataegress.Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cands = append(g.cands, c)
	if g.err != nil {
		return dataegress.Result{}, g.err
	}
	if g.noEnvelope {
		return dataegress.Result{}, nil
	}
	env := &pv.RequestEnvelope{Payload: pv.RequestEnvelopePayload{Instructions: c.Instructions}}
	if g.instructions != nil {
		env.Payload.Instructions = *g.instructions
	}
	var res dataegress.Result
	for mi, m := range c.Messages {
		var parts []pv.RequestEnvelopePart
		for pi, p := range m.Parts {
			oc := dataegress.PartOutcome{Message: mi, Part: pi, EnvelopeMessage: -1, EnvelopePart: -1}
			if g.drop == nil || !g.drop(mi, pi, p) {
				text := p.Text
				if g.rewrite != nil {
					text = g.rewrite(text)
				}
				oc.Kept, oc.EnvelopeMessage, oc.EnvelopePart = true, len(env.Payload.Messages), len(parts)
				if g.corrupt {
					oc.EnvelopeMessage = 99
				}
				parts = append(parts, pv.RequestEnvelopePart{Value: pv.RequestEnvelopeTextPart{Type: "text", Text: text}})
			}
			res.Parts = append(res.Parts, oc)
		}
		if len(parts) > 0 {
			env.Payload.Messages = append(env.Payload.Messages, pv.RequestEnvelopeMessage{Role: m.Role, Parts: parts})
		}
	}
	res.Envelope = env
	return res, nil
}

func (g *rdyExtGuard) calls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.cands)
}

// rdyExtFake is a fake rdyExtStack.
type rdyExtFake struct {
	guard      *rdyExtGuard
	gen        providers.MultimodalGenerator
	deployment string
	resolveErr error
	admitErr   error
	authErr    error
	admitted   []string
	resolved   []string
	authorized []string
	profile    pv.ProviderProfile
}

func rdyExtNewFake(gen providers.MultimodalGenerator) *rdyExtFake {
	return &rdyExtFake{
		guard: &rdyExtGuard{}, gen: gen, deployment: "dep_readiness_test",
		profile: pv.ProviderProfile{ProfileID: "prf_readiness_test", ProviderID: "openai", ModelID: "hosted-model-0", ModelVersion: "v1"},
	}
}

func (f *rdyExtFake) Deployment() string { return f.deployment }

func (f *rdyExtFake) Guard() dataegress.Guard {
	if f.guard == nil {
		return nil
	}
	return f.guard
}

func (f *rdyExtFake) Resolve(_ context.Context, _ contracts.Scope, modelID string) (pv.ProviderProfile, error) {
	f.resolved = append(f.resolved, modelID)
	return f.profile, f.resolveErr
}

func (f *rdyExtFake) Authorize(_ context.Context, scope contracts.Scope) error {
	f.authorized = append(f.authorized, scope.PrincipalID+"@"+scope.TenantID)
	return f.authErr
}

func (f *rdyExtFake) Admit(_ context.Context, _ contracts.Scope, _ pv.ProviderProfile, class string) error {
	f.admitted = append(f.admitted, class)
	return f.admitErr
}

func (f *rdyExtFake) Send(ctx context.Context, scope contracts.Scope, _ pv.ProviderProfile, _ string, _ dataegress.Candidate, req providers.ChatRequest) (providers.ChatResult, error) {
	res, err := f.gen.Generate(ctx, scope, providers.FromChat(req))
	if err != nil {
		return providers.ChatResult{}, err
	}
	return providers.ChatResult{Text: res.Text(), FinishReason: res.FinishReason, Model: res.Model}, nil
}

// rdyExtEnable turns external mode on for one test and installs the fake stack; the opener counts its calls.
func rdyExtEnable(t *testing.T, st rdyExtStack) *int {
	t.Helper()
	oldSec, oldOpen := externalSection, rdyExtOpen
	externalSection.Enabled = true
	externalSection.DeploymentOperators = []string{"demo-app/" + rdyExtTestOperator}
	opened := 0
	rdyExtOpen = func(context.Context, *Env) (rdyExtStack, error) {
		opened++
		if st == nil {
			return nil, errors.New("the stack cannot be built")
		}
		return st, nil
	}
	t.Cleanup(func() { externalSection, rdyExtOpen = oldSec, oldOpen })
	return &opened
}

// rdyExtTestOperator is a principal of the test configuration's deployment_operators (as demo-app/usr_ops).
const rdyExtTestOperator = "usr_ops"

func rdyExtEnv(class string) *Env {
	return rdyExtEnvAs(class, rdyExtTestOperator)
}

func rdyExtEnvAs(class, operator string) *Env {
	return (&Env{Config: config.NewRegistry(), LookupEnv: func(k string) (string, bool) {
		switch {
		case k == rdyExtClassEnv && class != "":
			return class, true
		case k == rdyExtOperatorEnv && operator != "":
			return operator, true
		}
		return "", false
	}}).withDefaults()
}

func rdyExtTestGate(st rdyExtStack) rdyExtGate {
	return rdyExtGate{leg: newRdyExtLeg(st, "hosted-model-0", "internal", rdyExtTestOperator, rdyExtDefaultTenant)}
}

func TestReadinessExternalHookIsFilledByTheExternalBuild(t *testing.T) {
	if rdyCLIExternal == nil {
		t.Fatal("the !noexternal build must fill the external hook")
	}
}

func TestReadinessExternalHookDoesNothingWhileExternalModeIsOff(t *testing.T) {
	opened := rdyExtEnable(t, rdyExtNewFake(fake.New()))
	externalSection.Enabled = false // the default

	pick, err := rdyCLIExternal(context.Background(), rdyExtEnv(""), "demo-app", "hosted-model-0")
	if pick != nil || err != nil {
		t.Fatalf("pick %+v, err %v: want nothing", pick, err)
	}
	if *opened != 0 {
		t.Fatal("a stack was built while external mode is off")
	}
	// the whole path of a run: no external model, the local rules decide
	setup, err := rdyCLIBuildModel(context.Background(), rdyExtEnv(""), "demo-app", "")
	if err != nil || setup.Config.Model != nil || setup.Config.AllowExternal || *opened != 0 {
		t.Fatalf("setup %+v err %v opened %d", setup, err, *opened)
	}
}

func TestReadinessExternalHookLeavesTheLocalModelAlone(t *testing.T) {
	opened := rdyExtEnable(t, rdyExtNewFake(fake.New()))
	old := inferenceSection
	inferenceSection.Models = []string{"local-model-0"}
	t.Cleanup(func() { inferenceSection = old })

	for _, id := range []string{"", "local-model-0"} {
		if pick, err := rdyCLIExternal(context.Background(), rdyExtEnv("internal"), "demo-app", id); pick != nil || err != nil {
			t.Errorf("model %q: pick %+v err %v: the local model must be left alone", id, pick, err)
		}
	}
	if *opened != 0 {
		t.Fatal("a stack was built for a local run")
	}
}

func TestReadinessExternalHookClassesTheProjectLocalOnlyUnlessTheOperatorSaysOtherwise(t *testing.T) {
	st := rdyExtNewFake(fake.New())
	rdyExtEnable(t, st)
	for _, tc := range []struct{ env, want string }{
		{"", "local_only"}, {"local_only", "local_only"}, {"internal", "internal"}, {" Confidential ", "confidential"}, {"public", "public"},
	} {
		pick, err := rdyCLIExternal(context.Background(), rdyExtEnv(tc.env), "demo-app", "hosted-model-0")
		if err != nil || pick == nil {
			t.Fatalf("env %q: pick %v err %v", tc.env, pick, err)
		}
		if pick.ProjectClass != tc.want || pick.Model.Local || pick.Gate == nil || pick.Model.ModelID != "hosted-model-0" || pick.Model.Qualified {
			t.Errorf("env %q: %+v", tc.env, pick)
		}
	}
	if _, err := rdyCLIExternal(context.Background(), rdyExtEnv("secretish"), "demo-app", "hosted-model-0"); err == nil || !errors.Is(err, ErrUsage) {
		t.Fatalf("an unknown class must be a usage error, got %v", err)
	}
}

func TestReadinessExternalHookNeverFallsBackToTheLocalModelWhenTheStackCannotBeBuilt(t *testing.T) {
	rdyExtEnable(t, nil)
	pick, err := rdyCLIExternal(context.Background(), rdyExtEnv("internal"), "demo-app", "hosted-model-0")
	if err == nil || pick != nil {
		t.Fatalf("pick %+v err %v: an error is the only answer", pick, err)
	}
	if _, err := rdyCLIBuildModel(context.Background(), rdyExtEnv("internal"), "demo-app", "hosted-model-0"); err == nil {
		t.Fatal("the run went on with another model")
	}
}

func rdyExtItems() []assist.AssistEgressItem {
	return []assist.AssistEgressItem{
		{Role: providers.MsgSystem, Trusted: true, Content: "SYSTEM-FRAME", Kind: "frame", Class: "public"},
		{Role: providers.MsgUser, Trusted: true, Content: "TARGETS-FRAME", Kind: "frame", Class: "public"},
		{Role: providers.MsgUser, Content: "EXCERPT-ONE", Kind: "excerpt", SourceID: "docs/a.md", Class: "internal"},
		{Role: providers.MsgUser, Content: "EXCERPT-TWO", Kind: "excerpt", SourceID: "docs/b.md", Class: "confidential"},
		{Role: providers.MsgUser, Trusted: true, Content: "FINAL-FRAME", Kind: "frame", Class: "public"},
	}
}

func rdyExtRequest(items []assist.AssistEgressItem) assist.AssistEgressRequest {
	return assist.AssistEgressRequest{Scope: rdyCLIScope("demo-app"), Provider: "external", ModelID: "hosted-model-0", ProjectClass: "internal", Items: items}
}

func rdyExtContents(items []assist.AssistEgressItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Content
	}
	return out
}

func TestReadinessExternalGateMapsItemsToTheGuardAndBackFromTheEnvelope(t *testing.T) {
	st := rdyExtNewFake(fake.New())
	g := rdyExtTestGate(st)

	t.Run("what goes to the guard", func(t *testing.T) {
		st.guard.cands = nil
		items := rdyExtItems()
		items[3].LocalOnly = true
		items = append(items, assist.AssistEgressItem{Role: providers.MsgUser, Content: "EXCERPT-LOCAL", Kind: "excerpt", Class: "local_only", LocalOnly: true})
		got, err := g.Release(context.Background(), rdyExtRequest(items))
		if err != nil {
			t.Fatal(err)
		}
		if st.guard.calls() != 1 {
			t.Fatalf("the guard was asked %d times", st.guard.calls())
		}
		c := st.guard.cands[0]
		if c.Instructions != "SYSTEM-FRAME" || c.Scope.DeploymentID != "dep_readiness_test" || c.Scope.ApplicationID != "demo-app" || c.Profile.ModelID != "hosted-model-0" {
			t.Errorf("candidate = %+v", c)
		}
		if len(c.Messages) != 5 {
			t.Fatalf("%d messages, want one per item except the instructions", len(c.Messages))
		}
		want := []struct {
			text, cat, class string
			local            bool
		}{
			{"TARGETS-FRAME", "dialogue_directive", "public", false},
			{"EXCERPT-ONE", "user_message", "internal", false},
			{"EXCERPT-TWO", "user_message", "confidential", true},
			{"FINAL-FRAME", "dialogue_directive", "public", false},
			{"EXCERPT-LOCAL", "user_message", "restricted", true}, // local_only is the most restrictive class, and removed
		}
		for i, w := range want {
			p := c.Messages[i].Parts
			if len(p) != 1 || p[0].Text != w.text || p[0].Category != w.cat || p[0].Class != w.class || p[0].LocalOnly != w.local {
				t.Errorf("message %d = %+v, want %+v", i, p, w)
			}
		}
		// the instructions come back first, as the frame they were
		if len(got) == 0 || got[0].Role != providers.MsgSystem || got[0].Content != "SYSTEM-FRAME" {
			t.Fatalf("released = %v", rdyExtContents(got))
		}
	})

	t.Run("the text comes from the envelope, not from the items", func(t *testing.T) {
		st.guard = &rdyExtGuard{rewrite: func(s string) string { return strings.ReplaceAll(s, "EXCERPT", "[scrubbed]") }}
		got, err := g.Release(context.Background(), rdyExtRequest(rdyExtItems()))
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(rdyExtContents(got), "|")
		if strings.Contains(all, "EXCERPT") || !strings.Contains(all, "[scrubbed]-ONE") || !strings.Contains(all, "[scrubbed]-TWO") {
			t.Fatalf("the released text is not the guard's: %v", rdyExtContents(got))
		}
	})

	t.Run("an excerpt the guard removes is not released", func(t *testing.T) {
		st.guard = &rdyExtGuard{drop: func(_, _ int, p dataegress.Part) bool { return p.Text == "EXCERPT-TWO" }}
		got, err := g.Release(context.Background(), rdyExtRequest(rdyExtItems()))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range rdyExtContents(got) {
			if c == "EXCERPT-TWO" {
				t.Fatalf("a removed excerpt came back: %v", rdyExtContents(got))
			}
		}
		if len(got) != 4 {
			t.Fatalf("released %v", rdyExtContents(got))
		}
	})

	t.Run("a prompt with a frame missing is refused whole", func(t *testing.T) {
		st.guard = &rdyExtGuard{drop: func(_, _ int, p dataegress.Part) bool { return p.Text == "TARGETS-FRAME" }}
		_, err := g.Release(context.Background(), rdyExtRequest(rdyExtItems()))
		if !contracts.HasCode(err, contracts.ErrPermissionDenied) {
			t.Fatalf("err = %v, want permission_denied", err)
		}
	})
}

func TestReadinessExternalGateFailuresArePermissionDenied(t *testing.T) {
	deny := contracts.New(contracts.ErrPermissionDenied, "egress denied: kill switch")
	for name, mk := range map[string]func() rdyExtGate{
		"the guard says no": func() rdyExtGate { st := rdyExtNewFake(fake.New()); st.guard.err = deny; return rdyExtTestGate(st) },
		"the guard fails": func() rdyExtGate {
			st := rdyExtNewFake(fake.New())
			st.guard.err = errors.New("db is down")
			return rdyExtTestGate(st)
		},
		"the guard is unavailable": func() rdyExtGate {
			st := rdyExtNewFake(fake.New())
			st.guard.err = contracts.New(contracts.ErrUnavailable, "x")
			return rdyExtTestGate(st)
		},
		"the guard releases nothing": func() rdyExtGate {
			st := rdyExtNewFake(fake.New())
			st.guard.noEnvelope = true
			return rdyExtTestGate(st)
		},
		"the envelope is unreadable": func() rdyExtGate {
			st := rdyExtNewFake(fake.New())
			st.guard.corrupt = true
			return rdyExtTestGate(st)
		},
		"no profile": func() rdyExtGate {
			st := rdyExtNewFake(fake.New())
			st.resolveErr = errors.New("none")
			return rdyExtTestGate(st)
		},
		"no guard at all": func() rdyExtGate { st := rdyExtNewFake(fake.New()); st.guard = nil; return rdyExtTestGate(st) },
		"no stack":        func() rdyExtGate { return rdyExtGate{} },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := mk().Release(context.Background(), rdyExtRequest(rdyExtItems()))
			if !contracts.HasCode(err, contracts.ErrPermissionDenied) || len(got) != 0 {
				t.Fatalf("got %v, err %v: want permission_denied and nothing", rdyExtContents(got), err)
			}
		})
	}
}

// The whole leg: the real assist engine, the hook, the fake stack. The project class decides first; the guard decides next; a failure of
// either ends the leg without trying anything else.
func TestReadinessExternalLegThroughTheAssistEngine(t *testing.T) {
	res, err := scan.ScanDir(rdyCLIClinic, scan.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, class string, st *rdyExtFake) (*assist.AssistOutcome, *fake.Provider) {
		t.Helper()
		gen := st.gen.(*fake.Provider)
		rdyExtEnable(t, st)
		setup, err := rdyCLIBuildModel(context.Background(), rdyExtEnv(class), "demo-app", "hosted-model-0")
		if err != nil {
			t.Fatal(err)
		}
		if setup.Config.Model == nil || setup.Config.Model.Local || !setup.Config.AllowExternal || setup.Config.Gate == nil {
			t.Fatalf("setup = %+v", setup.Config)
		}
		out, err := assist.AssistRun(context.Background(), rdyCLIScope("demo-app"), rdyCLIClinic, res, setup.Config)
		if err != nil {
			t.Fatal(err)
		}
		return out, gen
	}
	newFake := func() *rdyExtFake {
		gen := fake.New()
		gen.On(fake.Any()).Reply(`{"proposals":[]}`)
		return rdyExtNewFake(gen)
	}

	t.Run("local_only: nothing is sent, the guard is not even asked", func(t *testing.T) {
		st := newFake()
		out, gen := run(t, "", st)
		if out.Leg.Status != "refused" || !contracts.HasCode(out.Cause, contracts.ErrPermissionDenied) {
			t.Fatalf("leg %+v cause %v", out.Leg, out.Cause)
		}
		if gen.Calls() != 0 || st.guard.calls() != 0 {
			t.Fatalf("model called %d times, guard %d times", gen.Calls(), st.guard.calls())
		}
	})
	t.Run("internal: the guard releases and the model is called", func(t *testing.T) {
		st := newFake()
		out, gen := run(t, "internal", st)
		if out.Leg.Status != "ran" || gen.Calls() == 0 || st.guard.calls() == 0 {
			t.Fatalf("leg %+v cause %v, model %d guard %d", out.Leg, out.Cause, gen.Calls(), st.guard.calls())
		}
		for _, c := range st.guard.cands {
			if c.Instructions == "" || c.Scope.DeploymentID != "dep_readiness_test" {
				t.Fatalf("candidate = %+v", c)
			}
			for _, m := range c.Messages {
				for _, p := range m.Parts {
					if p.Class == "restricted" || p.LocalOnly {
						t.Fatalf("a part that must not leave was offered as sendable: %+v", p)
					}
				}
			}
		}
		if len(st.admitted) == 0 || st.admitted[0] != "internal" {
			t.Fatalf("the registry gates ran for class %v", st.admitted)
		}
	})
	t.Run("the guard refuses: refused, nothing sent, no other model", func(t *testing.T) {
		st := newFake()
		st.guard.err = contracts.New(contracts.ErrPermissionDenied, "egress denied")
		out, gen := run(t, "internal", st)
		if out.Leg.Status != "refused" || !contracts.HasCode(out.Cause, contracts.ErrPermissionDenied) || gen.Calls() != 0 {
			t.Fatalf("leg %+v cause %v model calls %d", out.Leg, out.Cause, gen.Calls())
		}
	})
	t.Run("the registry refuses the credential: the leg ends, no other model", func(t *testing.T) {
		st := newFake()
		st.admitErr = contracts.New(contracts.ErrUnauthenticated, "the credential is not active")
		out, gen := run(t, "internal", st)
		if out.Leg.Status == "ran" || gen.Calls() != 0 || out.Cause == nil {
			t.Fatalf("leg %+v cause %v model calls %d", out.Leg, out.Cause, gen.Calls())
		}
	})
}

func TestReadinessExternalGateRefusesWhenTheGuardWithholdsTheInstructions(t *testing.T) {
	st := rdyExtNewFake(fake.New())
	empty := ""
	st.guard.instructions = &empty
	got, err := (rdyExtTestGate(st)).Release(context.Background(), rdyExtRequest(rdyExtItems()))
	if !contracts.HasCode(err, contracts.ErrPermissionDenied) || len(got) != 0 {
		t.Fatalf("got %v, err %v: want permission_denied and nothing", rdyExtContents(got), err)
	}
}

// The live stack follows the deployment mode like the rest of external mode: with Local (the default) nothing leaves, and an
// external deployment with no qualified profile for the model still sends nothing.
func TestReadinessExternalLiveStackHonoursTheDeploymentMode(t *testing.T) {
	a := externalTestAdmin(t, filepath.Join(t.TempDir(), "external"), nil)
	live := &rdyExtLive{admin: a, sender: &extWireSender{admin: a, reg: extFinalProviderRegistry(), cache: map[string]ExternalProvider{}}}
	scope := newRdyExtLeg(live, "hosted-model-0", "internal", "usr_ops", rdyExtDefaultTenant).scope(rdyCLIScope("demo-app"))
	if live.Deployment() != externalTestDeployment || live.Guard() == nil {
		t.Fatalf("deployment %q guard %v", live.Deployment(), live.Guard())
	}

	_, err := live.Resolve(context.Background(), scope, "hosted-model-0")
	if !contracts.HasCode(err, contracts.ErrPermissionDenied) || !strings.Contains(err.Error(), "local") {
		t.Fatalf("Local mode must refuse: %v", err)
	}
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} { // the most restrictive level decides
		if err := a.modes.set(lv, scope, modes.ModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	_, err = live.Resolve(context.Background(), scope, "hosted-model-0")
	if !contracts.HasCode(err, contracts.ErrPermissionDenied) || !strings.Contains(err.Error(), "no qualified provider profile") {
		t.Fatalf("an external deployment without a profile must refuse: %v", err)
	}
}

// ---- the live stack, end to end, against loopback fake providers (no network, no real provider) ----
//
// The tests below drive the real providerreg, providerbudget, dataegress, credentials and modes packages (through the stack
// `alixai readiness` builds), the real assist engine and the real OpenAI adapter. The only fake is the provider: an httptest server on
// 127.0.0.1 that counts what reaches it.

// rdyExtStub is a loopback provider. It counts the requests it receives by the model they name.
type rdyExtStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	hits   map[string]int
	bodies []string
	// reply answers a request for a model: the status and the body. Nil answers a valid reply that names the pinned snapshot.
	reply func(model string) (int, string)
	// onHit runs when a request arrives, before it is answered.
	onHit func()
}

const (
	rdyExtModelA, rdyExtVersionA = "gpt-5-mini", "2025-08-07"
	rdyExtModelB, rdyExtVersionB = "claude-haiku-4-5", "2025-10-01"
)

// rdyExtReplyBody is a Responses API answer that carries an empty proposal list, the closed schema the assist engine wants.
func rdyExtReplyBody(model string) string {
	return fmt.Sprintf(`{"id":"resp_0123456789abcdef","object":"response","status":"completed","model":%q,`+
		`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"{\"proposals\":[]}","annotations":[]}]}],`+
		`"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":0},"output_tokens":500,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":1500}}`, model)
}

func newRdyExtStub(t *testing.T) *rdyExtStub {
	t.Helper()
	s := &rdyExtStub{hits: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &in)
		s.mu.Lock()
		s.hits[in.Model]++
		s.bodies = append(s.bodies, string(body))
		reply, onHit := s.reply, s.onHit
		s.mu.Unlock()
		if onHit != nil {
			onHit()
		}
		status, out := http.StatusOK, rdyExtReplyBody(in.Model+"-"+rdyExtVersionA)
		if reply != nil {
			status, out = reply(in.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rdyExtStub) count(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[model]
}

func (s *rdyExtStub) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.hits {
		n += c
	}
	return n
}

// rdyExtKillAfterReserve engages the real kill switch right after the real budget service reserved: the moment between the Reserve and
// the send that SEC-129 closes. Release, Settle and States are the service's own.
type rdyExtKillAfterReserve struct {
	*providerbudget.Service
	engage func()
}

func (k rdyExtKillAfterReserve) Reserve(ctx context.Context, scope contracts.Scope, c providerbudget.Call) (providerbudget.Reservation, error) {
	r, err := k.Service.Reserve(ctx, scope, c)
	if err == nil {
		k.engage()
	}
	return r, err
}

type rdyExtRigOpts struct {
	noBudget bool
	// fallback registers a second qualified profile (another provider, its own loopback server) and names it as the fallback of the first.
	fallback bool
}

// rdyExtRig is the live stack of the leg over a real store, with one qualified profile (two with the fallback option).
type rdyExtRig struct {
	t      *testing.T
	clk    *clock.Fake
	admin  *externalAdmin
	live   *rdyExtLive
	stub   *rdyExtStub // the provider of profile A (OpenAI adapter)
	stubB  *rdyExtStub // the provider of profile B (Anthropic adapter)
	scope  contracts.Scope
	budget pv.Budget
	a, b   pv.ProviderProfile
}

func rdyExtProviderBudget(provider string, daily, monthly int64) pv.Budget {
	id := pv.ProviderID(provider)
	return pv.Budget{
		Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindProvider, ProviderID: &id},
		Channel: pv.ChannelInteractive, Currency: "USD", WarnAtPercent: 80,
		Limits: pv.BudgetLimits{DailyMicroUnits: pv.MicroUnits(daily), MonthlyMicroUnits: pv.MicroUnits(monthly)},
	}
}

func newRdyExtRig(t *testing.T, o rdyExtRigOpts) *rdyExtRig {
	t.Helper()
	ctx := context.Background()
	r := &rdyExtRig{t: t, clk: clock.NewFake(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)), stub: newRdyExtStub(t), stubB: newRdyExtStub(t)}
	db := storetest.Open(t, nil)
	alg, err := audit.New(db, audit.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sec := externalConfig{
		Enabled: true, DeploymentOperators: []string{"demo-app/" + rdyExtTestOperator},
		OpenAIBaseURL: r.stub.srv.URL, AnthropicBaseURL: r.stubB.srv.URL,
	}
	r.admin, err = newExternalAdmin(externalAdminDeps{
		DB: db, Audit: alg, Clock: r.clk, DeploymentID: externalTestDeployment, Dir: filepath.Join(t.TempDir(), "external"),
		Validator: externalAcceptKeys{}, Section: sec,
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := &extWireSender{admin: r.admin, sec: sec, reg: extFinalProviderRegistry(), cache: map[string]ExternalProvider{}}
	r.live = &rdyExtLive{admin: r.admin, sender: sender}
	r.scope = contracts.Scope{
		DeploymentID: externalTestDeployment, ApplicationID: "demo-app", TenantID: rdyExtDefaultTenant, WorkspaceID: "operator-cli",
		PrincipalID: rdyExtTestOperator, Roles: []string{"operator"}, CapabilityScopes: []string{"providers.manage"}, SessionID: "ses_operator_cli",
	}

	// External mode at every level; the data-egress policy lets public and internal data go to these two providers.
	for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
		if err := r.admin.modes.set(lv, r.scope, modes.ModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	r.egressPolicy(ctx)

	var fb *pv.ProfileID
	if o.fallback {
		r.b = r.qualify(ctx, rdyExtModelB, rdyExtVersionB, "anthropic", nil)
		id := r.b.ProfileID
		fb = &id
	}
	r.a = r.qualify(ctx, rdyExtModelA, rdyExtVersionA, "openai", fb)

	if !o.noBudget {
		b, err := r.admin.PutBudget(ctx, r.scope, rdyExtProviderBudget("openai", 1_000_000, 10_000_000))
		if err != nil {
			t.Fatal(err)
		}
		r.budget = b
	}
	return r
}

func (r *rdyExtRig) egressPolicy(ctx context.Context) {
	t := r.t
	admin := r.scope.Clone()
	admin.PrincipalID, admin.SessionID = "usr_egress_admin", "ses_egress_admin"
	admin.CapabilityScopes = []string{dataegress.CapabilityRead, dataegress.CapabilityApprove}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		c, _ := dataegress.WriteCapability(lv)
		admin.CapabilityScopes = append(admin.CapabilityScopes, c)
	}
	base := func(level pv.EgressLevel) dataegress.PolicyInput {
		return dataegress.PolicyInput{
			Level: level, Enabled: true, SourceMode: pv.EgressModeAllowExcerpt,
			AllowedProviders: []pv.ProviderID{"openai", "anthropic"},
			AllowedModels:    []pv.ModelRef{{ProviderID: "openai", ModelID: rdyExtModelA}, {ProviderID: "anthropic", ModelID: rdyExtModelB}},
			AllowedClasses:   []pv.EgressEligibleClass{pv.EgressEligibleClassPublic, pv.EgressEligibleClassInternal},
			AllowedRegions:   []pv.RegionCode{"us", "eu"},
		}
	}
	if _, err := r.admin.egress.PutDeploymentPolicy(ctx, dataegress.Operator{DeploymentID: externalTestDeployment, PrincipalID: "opr_root"}, base(pv.EgressLevelDeployment)); err != nil {
		t.Fatal(err)
	}
	for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
		if _, err := r.admin.egress.PutPolicy(ctx, admin, base(lv)); err != nil {
			t.Fatal(err)
		}
	}
}

// qualify takes a text-only profile of the provider through registration, credential, acknowledgement, conformance and qualification.
func (r *rdyExtRig) qualify(ctx context.Context, model, version, provider string, fallback *pv.ProfileID) pv.ProviderProfile {
	t := r.t
	t.Helper()
	raw, err := os.ReadFile("../../schemas/providers/examples/provider-profile/valid/qualified-text-only.json")
	if err != nil {
		t.Fatal(err)
	}
	var p pv.ProviderProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	p.ProfileID, p.CredRef, p.BillingNoticeAck, p.LastConformance = "", nil, nil, nil
	p.Lifecycle, p.QuarantineReason, p.CreatedAt, p.UpdatedAt = "", nil, "", ""
	p.ProviderID, p.ModelID, p.ModelVersion = pv.ProviderID(provider), pv.ModelID(model), pv.ModelVersion(version)
	p.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: "demo-app"}}
	day := r.clk.Now().Format("2006-01-02")
	p.Terms.RecordedOn, p.LastPolicyReview = day, day
	if fallback != nil {
		p.Fallback = pv.ProviderProfileFallback{ProfileID: fallback, On: []pv.ProviderProfileFallbackOnItem{"unavailable", "timeout", "rate_limited"}}
	}
	p, err = r.admin.reg.Register(ctx, r.scope, p)
	if err != nil {
		t.Fatal(err)
	}
	var in gateway.ProvidersCredentialInput
	if err := json.Unmarshal([]byte(`{"key":"fixture-credential-value-0001"}`), &in); err != nil { // TEST-ONLY placeholder, not shaped like a key
		t.Fatal(err)
	}
	if st, err := r.admin.SubmitCredential(ctx, r.scope, pv.ProviderID(provider), in); err != nil || st.State != pv.ConnectionStatusStateConnected {
		t.Fatalf("SubmitCredential = %+v, %v", st, err)
	}
	if p, err = r.admin.reg.Get(ctx, r.scope, p.ProfileID); err != nil {
		t.Fatal(err)
	}
	if p, err = r.admin.reg.AcknowledgeBillingNotice(ctx, r.scope, p.ProfileID, providerreg.Acknowledgement{
		NoticeVersion: p.BillingNotice.Version, ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true,
	}); err != nil {
		t.Fatal(err)
	}
	var rep pv.ConformanceReport
	if err := json.Unmarshal(extWireTestReport(t, p, r.clk.Now().Add(-time.Hour)), &rep); err != nil {
		t.Fatal(err)
	}
	if _, err := r.admin.reg.RecordConformance(ctx, r.scope, p.ProfileID, rep); err != nil {
		t.Fatal(err)
	}
	if p, err = r.admin.reg.Qualify(ctx, r.scope, p.ProfileID); err != nil || p.Lifecycle != pv.ProfileLifecycleQualified {
		t.Fatalf("Qualify = %+v, %v", p.Lifecycle, err)
	}
	return p
}

// run is `alixai readiness -model gpt-5-mini` over the clinic fixture, through the hook, the gate and the engine.
func (r *rdyExtRig) run(class, operator string) (*assist.AssistOutcome, error) {
	r.t.Helper()
	res, err := scan.ScanDir(rdyCLIClinic, scan.ScanOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	rdyExtEnable(r.t, r.live)
	setup, err := rdyCLIBuildModel(context.Background(), rdyExtEnvAs(class, operator), "demo-app", rdyExtModelA)
	if err != nil {
		return nil, err
	}
	return assist.AssistRun(context.Background(), rdyCLIScope("demo-app"), rdyCLIClinic, res, setup.Config)
}

// state is the daily window of the provider budget.
func (r *rdyExtRig) state() pv.BudgetStateWindow {
	r.t.Helper()
	st, err := r.admin.budget.State(context.Background(), r.scope, r.budget.BudgetID)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, w := range st.Windows {
		if w.Window == pv.BudgetStateWindowWindowDaily {
			return w
		}
	}
	r.t.Fatal("no daily window")
	return pv.BudgetStateWindow{}
}

func (r *rdyExtRig) usage() providerbudget.UsageExport {
	r.t.Helper()
	exp, err := r.admin.budget.ExportUsage(context.Background(), r.scope, providerbudget.UsageFilter{ApplicationWide: true})
	if err != nil {
		r.t.Fatal(err)
	}
	return exp
}

// wantNothingSent is what every refusal must leave behind: no request at any provider, no money held or spent, no usage record.
func (r *rdyExtRig) wantNothingSent(what string) {
	r.t.Helper()
	if n := r.stub.total() + r.stubB.total(); n != 0 {
		r.t.Errorf("%s: %d request(s) reached a provider", what, n)
	}
	if r.budget.BudgetID != "" {
		if w := r.state(); w.ReservedMicroUnits != 0 || w.SpentMicroUnits != 0 {
			r.t.Errorf("%s: the budget holds %d and has spent %d", what, w.ReservedMicroUnits, w.SpentMicroUnits)
		}
	}
	if n := len(r.usage().Rows); n != 0 {
		r.t.Errorf("%s: %d usage record(s) were written for a call that was never made", what, n)
	}
}

// A call reserves the provider budget before anything is sent and settles it after: while the provider is answering, the budget holds
// money; afterwards it holds none, has spent what the provider reported, and one succeeded usage record exists per call.
func TestExternalReadinessReservesAndSettlesTheBudget(t *testing.T) {
	r := newRdyExtRig(t, rdyExtRigOpts{})
	// At every request the budget already holds the reservation of that very call, and has spent only what earlier calls settled.
	var unreserved, overspent int
	var spentBefore pv.MicroUnits
	r.stub.onHit = func() {
		w := r.state()
		if w.ReservedMicroUnits == 0 {
			unreserved++
		}
		if w.SpentMicroUnits != spentBefore {
			overspent++
		}
		spentBefore = w.SpentMicroUnits + 450 // what the fake provider reports: 1000 in, 500 out at the profile's prices
	}

	out, err := r.run("internal", rdyExtTestOperator)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Leg.Status) != "ran" || out.Cause != nil || r.stub.total() == 0 {
		t.Fatalf("leg %+v cause %v, provider requests %d", out.Leg, out.Cause, r.stub.total())
	}
	if r.stub.count(rdyExtModelA) != r.stub.total() || r.stubB.total() != 0 {
		t.Fatalf("requests went to %v and %v", r.stub.hits, r.stubB.hits)
	}
	if unreserved != 0 || overspent != 0 {
		t.Fatalf("%d request(s) arrived with nothing reserved, %d with spend not matching earlier settlements: the reservation must come first", unreserved, overspent)
	}
	w := r.state()
	if w.ReservedMicroUnits != 0 || w.SpentMicroUnits == 0 {
		t.Fatalf("after the calls the budget holds %d and has spent %d: every reservation is settled", w.ReservedMicroUnits, w.SpentMicroUnits)
	}
	exp := r.usage()
	if len(exp.Rows) != r.stub.total() {
		t.Fatalf("%d usage records for %d calls", len(exp.Rows), r.stub.total())
	}
	var charged pv.MicroUnits
	for _, row := range exp.Rows {
		if row.Record.Outcome != pv.UsageRecordOutcomeSucceeded || row.Record.Metering.ToksIn != 1000 || row.Record.Metering.ToksOut != 500 {
			t.Fatalf("usage record %+v: want succeeded with the provider's figures", row.Record)
		}
		charged += row.ChargedMicroUnits
	}
	if charged != w.SpentMicroUnits {
		t.Fatalf("charged %d, spent %d", charged, w.SpentMicroUnits)
	}
	// What left is the filtered prompt and the pinned model, never a credential.
	for _, b := range r.stub.bodies {
		if !strings.Contains(b, `"model":"`+rdyExtModelA+`"`) || strings.Contains(b, "fixture-credential-value") {
			t.Fatalf("request body %.200s", b)
		}
	}
}

// Every refusal below happens before a byte leaves: no request reaches the provider, no money stays held or is spent, no usage record
// exists, and the leg ends refused with the deterministic scan alone (the hook errors for an operator who is not listed).
func TestExternalReadinessRefusesWithoutBudgetOperatorOrWhenTheKillSwitchIsEngaged(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, out *assist.AssistOutcome, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if string(out.Leg.Status) != "refused" || !contracts.HasCode(out.Cause, contracts.ErrPermissionDenied) {
			t.Fatalf("leg %+v cause %v: want refused and permission_denied", out.Leg, out.Cause)
		}
	}

	t.Run("no budget on the interactive channel", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{noBudget: true})
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		var ce *contracts.Error
		if !errors.As(out.Cause, &ce) || ce.Details["reason"] != providerbudget.ReasonNoBudget {
			t.Fatalf("cause %v: want the reason no_budget", out.Cause)
		}
		r.wantNothingSent("no budget")
	})

	t.Run("a budget of another provider is no budget", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{noBudget: true})
		if _, err := r.admin.PutBudget(ctx, r.scope, rdyExtProviderBudget("anthropic", 1_000_000, 10_000_000)); err != nil {
			t.Fatal(err)
		}
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		r.wantNothingSent("budget of another provider")
	})

	t.Run("an exhausted budget", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{noBudget: true})
		b, err := r.admin.PutBudget(ctx, r.scope, rdyExtProviderBudget("openai", 1, 1)) // below the estimate of any call
		if err != nil {
			t.Fatal(err)
		}
		r.budget = b
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		r.wantNothingSent("exhausted budget")
	})

	t.Run("an operator who is not listed in the configuration", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{})
		opened := rdyExtEnable(t, r.live)
		_, err := rdyCLIBuildModel(ctx, rdyExtEnvAs("internal", "usr_stranger"), "demo-app", rdyExtModelA)
		if !contracts.HasCode(err, contracts.ErrPermissionDenied) || *opened != 0 {
			t.Fatalf("err %v, stacks opened %d: want permission_denied before anything is opened", err, *opened)
		}
		if _, err := rdyCLIBuildModel(ctx, rdyExtEnvAs("internal", ""), "demo-app", rdyExtModelA); !contracts.HasCode(err, contracts.ErrPermissionDenied) {
			t.Fatalf("no operator named: err %v", err)
		}
		r.wantNothingSent("operator not listed")
	})

	t.Run("an operator the service of the deployment does not know", func(t *testing.T) {
		// The configuration the process loaded lists usr_other, the operator service built over the deployment does not: X-13 is
		// applied by the service on every request, not only by the hook.
		r := newRdyExtRig(t, rdyExtRigOpts{})
		res, err := scan.ScanDir(rdyCLIClinic, scan.ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		rdyExtEnable(t, r.live)
		externalSection.DeploymentOperators = []string{"demo-app/usr_other"}
		setup, err := rdyCLIBuildModel(ctx, rdyExtEnvAs("internal", "usr_other"), "demo-app", rdyExtModelA)
		if err != nil {
			t.Fatal(err)
		}
		out, err := assist.AssistRun(ctx, rdyCLIScope("demo-app"), rdyCLIClinic, res, setup.Config)
		refused(t, out, err)
		r.wantNothingSent("operator unknown to the service")
	})

	t.Run("the kill switch is engaged before the run", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{})
		if _, err := r.admin.SetKillSwitch(ctx, r.scope, true, "test"); err != nil {
			t.Fatal(err)
		}
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		r.wantNothingSent("kill switch before the run")
	})

	t.Run("the kill switch is engaged between the reservation and the send", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{})
		engaged := 0
		r.live.budgets = rdyExtKillAfterReserve{Service: r.admin.budget, engage: func() {
			engaged++
			if err := r.admin.egress.EngageKillSwitch(ctx, dataegress.Operator{DeploymentID: externalTestDeployment, PrincipalID: rdyExtTestOperator}, "emergency"); err != nil {
				t.Error(err)
			}
		}}
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		if engaged != 1 {
			t.Fatalf("the budget reserved %d times, want once", engaged)
		}
		r.wantNothingSent("kill switch after the reservation") // the reservation was released: nothing held, nothing spent, no record
	})

	t.Run("the project class is local_only", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{})
		out, err := r.run("", rdyExtTestOperator)
		refused(t, out, err)
		r.wantNothingSent("local_only project")
	})

	t.Run("the terms of the profile are stale", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{})
		r.clk.Advance(31 * 24 * time.Hour) // past the review interval of 30 days
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		p, gerr := r.admin.reg.Get(ctx, r.scope, r.a.ProfileID)
		if gerr != nil || p.Lifecycle != pv.ProfileLifecycleQuarantined {
			t.Fatalf("profile %v %v: a stale review quarantines it", p.Lifecycle, gerr)
		}
		r.wantNothingSent("stale terms")
	})

	t.Run("the deployment mode is local", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{})
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			if err := r.admin.modes.set(lv, r.scope, modes.ModeLocal); err != nil {
				t.Fatal(err)
			}
		}
		out, err := r.run("internal", rdyExtTestOperator)
		refused(t, out, err)
		r.wantNothingSent("local mode")
	})
}

// A refusal or a failure ends the leg. The profile names a fallback and the other provider is qualified, funded and answering, and it
// is never asked; the local runtime is never asked either.
func TestExternalReadinessNeverFallsBackToAnotherProvider(t *testing.T) {
	ctx := context.Background()
	// the second provider has a budget of its own, so only the fallback rule can keep it silent
	fund := func(r *rdyExtRig) {
		if _, err := r.admin.PutBudget(ctx, r.scope, rdyExtProviderBudget("anthropic", 1_000_000, 10_000_000)); err != nil {
			t.Fatal(err)
		}
	}
	silent := func(t *testing.T, r *rdyExtRig, out *assist.AssistOutcome, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if string(out.Leg.Status) == "ran" || out.Cause == nil {
			t.Fatalf("leg %+v cause %v: the leg must end", out.Leg, out.Cause)
		}
		if r.stubB.total() != 0 {
			t.Fatalf("the fallback provider received %d request(s)", r.stubB.total())
		}
		for _, row := range r.usage().Rows {
			if row.Record.Provider.ProviderID != "openai" {
				t.Fatalf("a call was recorded for %s", row.Record.Provider.ProviderID)
			}
		}
	}

	t.Run("the provider is unavailable", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{fallback: true})
		fund(r)
		r.stub.reply = func(string) (int, string) { return http.StatusServiceUnavailable, `{"error":{"message":"down"}}` }
		out, err := r.run("internal", rdyExtTestOperator)
		silent(t, r, out, err)
		if string(out.Leg.Status) != "unavailable" || r.stub.total() == 0 {
			t.Fatalf("leg %+v, requests to the first provider %d", out.Leg, r.stub.total())
		}
		if w := r.state(); w.ReservedMicroUnits != 0 {
			t.Fatalf("the failed call left %d held", w.ReservedMicroUnits)
		}
	})

	t.Run("the provider rejects the credential", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{fallback: true})
		fund(r)
		r.stub.reply = func(string) (int, string) { return http.StatusUnauthorized, `{"error":{"message":"bad key"}}` }
		out, err := r.run("internal", rdyExtTestOperator)
		silent(t, r, out, err)
		if string(out.Leg.Status) != "refused" {
			t.Fatalf("leg %+v", out.Leg)
		}
	})

	t.Run("the reply names another model", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{fallback: true})
		fund(r)
		r.stub.reply = func(string) (int, string) { return http.StatusOK, rdyExtReplyBody("gpt-5-nano-2025-08-07") }
		out, err := r.run("internal", rdyExtTestOperator)
		silent(t, r, out, err)
		p, gerr := r.admin.reg.Get(ctx, r.scope, r.a.ProfileID)
		if gerr != nil || p.Lifecycle != pv.ProfileLifecycleQuarantined {
			t.Fatalf("profile %v %v: a reply from another model quarantines it", p.Lifecycle, gerr)
		}
		for _, row := range r.usage().Rows {
			if row.Record.Outcome == pv.UsageRecordOutcomeSucceeded {
				t.Fatalf("a reply from another model was settled as a success")
			}
		}
	})

	t.Run("the terms are stale", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{fallback: true})
		fund(r)
		r.clk.Advance(31 * 24 * time.Hour)
		out, err := r.run("internal", rdyExtTestOperator)
		silent(t, r, out, err)
		if r.stub.total() != 0 || string(out.Leg.Status) != "refused" {
			t.Fatalf("leg %+v, requests to the first provider %d", out.Leg, r.stub.total())
		}
	})

	t.Run("the budget is gone", func(t *testing.T) {
		r := newRdyExtRig(t, rdyExtRigOpts{fallback: true, noBudget: true})
		fund(r)
		out, err := r.run("internal", rdyExtTestOperator)
		silent(t, r, out, err)
		if r.stub.total() != 0 || string(out.Leg.Status) != "refused" {
			t.Fatalf("leg %+v, requests to the first provider %d", out.Leg, r.stub.total())
		}
	})
}
