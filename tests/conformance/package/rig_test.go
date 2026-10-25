package conformancepkg

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	clinic "github.com/buxee/alix-ai/tests/conversation"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// clinicPackage is the clinic reference application's signed package, the one package every
// application of the suite is made from.
const clinicPackage = "../../../examples/reference-apps/clinic-booking/package"

// hostApplication is the one application id the clinic stand-in answers to: it is the
// product's own name for itself, and what the shim below shows it, whatever AlixAI calls
// the registered application.
const hostApplication = "clinic-booking"

// worldNow is "today" of every conversation: a Monday morning, as in tests/conversation.
var worldNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// ---- the host side of an application ------------------------------------------------------

// rebrand shows the host the one application id it knows, whatever the registered
// application is called, so two applications can run over two hosts of the same product.
// It sits beneath the counting adapter, so what is counted is what AlixAI sent.
type rebrand struct{ adapters.AppAdapter }

func (r rebrand) Read(ctx context.Context, sc contracts.Scope, q adapters.ReadRequest) (act.DeterministicResult, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Read(ctx, sc, q)
}

func (r rebrand) Validate(ctx context.Context, sc contracts.Scope, q adapters.ValidateRequest) (act.DeterministicResult, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Validate(ctx, sc, q)
}

func (r rebrand) Prepare(ctx context.Context, sc contracts.Scope, q adapters.PrepareRequest) (act.DeterministicResult, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Prepare(ctx, sc, q)
}

func (r rebrand) Commit(ctx context.Context, sc contracts.Scope, q adapters.CommitRequest) (act.DeterministicResult, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Commit(ctx, sc, q)
}

func (r rebrand) Status(ctx context.Context, sc contracts.Scope, q adapters.StatusRequest) (act.DeterministicResult, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Status(ctx, sc, q)
}

func (r rebrand) Cancel(ctx context.Context, sc contracts.Scope, q adapters.CancelRequest) (act.DeterministicResult, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Cancel(ctx, sc, q)
}

func (r rebrand) Inspect(ctx context.Context, sc contracts.Scope, q adapters.InspectRequest) (adapters.Inspection, error) {
	sc.ApplicationID = hostApplication
	return r.AppAdapter.Inspect(ctx, sc, q)
}

// counting is a tripwire: it records what the host of one application was asked, by
// operation and by the application id of the scope AlixAI sent.
type counting struct {
	adapters.AppAdapter
	mu   sync.Mutex
	ops  map[adapters.Operation]int
	apps map[string]int
}

func newCounting(inner adapters.AppAdapter) *counting {
	return &counting{AppAdapter: inner, ops: map[adapters.Operation]int{}, apps: map[string]int{}}
}

func (c *counting) note(op adapters.Operation, sc contracts.Scope) {
	c.mu.Lock()
	c.ops[op]++
	c.apps[sc.ApplicationID]++
	c.mu.Unlock()
}

func (c *counting) count(op adapters.Operation) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ops[op]
}

func (c *counting) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, k := range c.ops {
		n += k
	}
	return n
}

func (c *counting) applications() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.apps {
		out[k] = v
	}
	return out
}

func (c *counting) Read(ctx context.Context, sc contracts.Scope, q adapters.ReadRequest) (act.DeterministicResult, error) {
	c.note(adapters.OpRead, sc)
	return c.AppAdapter.Read(ctx, sc, q)
}

func (c *counting) Validate(ctx context.Context, sc contracts.Scope, q adapters.ValidateRequest) (act.DeterministicResult, error) {
	c.note(adapters.OpValidate, sc)
	return c.AppAdapter.Validate(ctx, sc, q)
}

func (c *counting) Prepare(ctx context.Context, sc contracts.Scope, q adapters.PrepareRequest) (act.DeterministicResult, error) {
	c.note(adapters.OpPrepare, sc)
	return c.AppAdapter.Prepare(ctx, sc, q)
}

func (c *counting) Commit(ctx context.Context, sc contracts.Scope, q adapters.CommitRequest) (act.DeterministicResult, error) {
	c.note(adapters.OpCommit, sc)
	return c.AppAdapter.Commit(ctx, sc, q)
}

func (c *counting) Status(ctx context.Context, sc contracts.Scope, q adapters.StatusRequest) (act.DeterministicResult, error) {
	c.note(adapters.OpStatus, sc)
	return c.AppAdapter.Status(ctx, sc, q)
}

func (c *counting) Cancel(ctx context.Context, sc contracts.Scope, q adapters.CancelRequest) (act.DeterministicResult, error) {
	c.note(adapters.OpCancel, sc)
	return c.AppAdapter.Cancel(ctx, sc, q)
}

// ---- the packages ---------------------------------------------------------------------------

// activePackage is the package as the registry, the policy gateway, the action coordinator
// and the dialogue engine see it once the integration audit has activated it. The audit is
// not what this suite is about (the conformance CLI runs it), so the one thing this changes
// is the activation state it reports.
type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

// signedCopy writes a copy of the clinic package under the application id id, signed with the
// TEST-ONLY publisher key, and returns its directory. The id appears twice in the package: the
// manifest's application id and the application the RAG source is scoped to.
func signedCopy(t testing.TB, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	err := filepath.WalkDir(clinicPackage, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(clinicPackage, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, rel)
		if e.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(rel, from, to string) {
		p := filepath.Join(dir, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(b), from) != 1 {
			t.Fatalf("%s has no single %q to re-identify", rel, from)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), from, to, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	edit("alixai.yaml", "application:\n  id: "+hostApplication+"\n", "application:\n  id: "+id+"\n")
	edit("rag-sources/clinic-policies.yaml", "application_id: "+hostApplication+"\n", "application_id: "+id+"\n")
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, err := trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: id, PackageVersion: "1.0.0"},
		Keys:            []trust.SigningKey{devkeys.SigningKey()}, Now: now, ExpiresAt: now.AddDate(10, 0, 0),
	}); err != nil {
		t.Fatalf("sign %s: %v", id, err)
	}
	return dir
}

// ---- the service -----------------------------------------------------------------------------

// tenantApp is one registered application of the rig.
type tenantApp struct {
	id    string
	pkg   capabilities.Package
	host  *clinic.ClinicHost
	calls *counting
	guard adapters.AppAdapter
}

// scope is the verified scope of a role of the clinic's directory under this application.
// Every level other than the application is the same for every application of the rig.
func (a *tenantApp) scope(role, session string) contracts.Scope {
	s := clinic.RoleScope(role)
	s.ApplicationID = a.id
	s.SessionID = session
	return s
}

// rig is one set of services serving any number of applications.
type rig struct {
	t     testing.TB
	clk   *clock.Fake
	db    *store.DB
	alog  *audit.Log
	gw    *policy.Gateway
	reg   *capabilities.Registry
	co    *actions.Coordinator
	store *sessions.Store
	model *pfake.Provider
	eng   *dialogue.Engine

	mu   sync.Mutex
	apps map[string]*tenantApp
}

func allActs() []conversation.DialogueDirectiveAct {
	return []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActRequestConfirmation,
		conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure,
		conversation.DialogueDirectiveActRefuse, conversation.DialogueDirectiveActOfferHelp,
		conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
	}
}

func newRig(t testing.TB) *rig {
	t.Helper()
	r := &rig{t: t, clk: clock.NewFake(worldNow), apps: map[string]*tenantApp{}}
	r.db = storetest.Open(t, nil)
	var err error
	if r.alog, err = audit.New(r.db, audit.Options{Clock: r.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](r.alog, audit.EventPolicyDecision, func(rec policy.Record) (audit.Locator, error) {
		s := rec.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	r.gw = policy.NewGateway(r.clk, policy.WithSink(sink))
	r.reg = capabilities.NewRegistry(nil, capabilities.WithClock(r.clk))

	adapterFor := actions.AdapterSourceFunc(func(_ context.Context, app string) (adapters.AppAdapter, error) {
		a, err := r.app(app)
		if err != nil {
			return nil, err
		}
		return a.guard, nil
	})
	r.co, err = actions.New(actions.Deps{
		DB: r.db, Policy: r.gw, Audit: r.alog, Clock: r.clk,
		Packages: actions.PackageSourceFunc(func(_ context.Context, app string) (policy.ActivePackage, error) {
			a, err := r.app(app)
			if err != nil {
				return nil, err
			}
			return a.pkg, nil
		}),
		Adapters: adapterFor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.store, err = sessions.New(r.db, r.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	r.model = pfake.New()
	sel, err := dialogue.LocalSelector(r.model, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	r.eng, err = dialogue.New(dialogue.Deps{
		Sessions: r.store, Registry: r.reg,
		Packages: dialogue.PackageSourceFunc(func(_ context.Context, app string) (capabilities.Package, error) {
			a, err := r.app(app)
			if err != nil {
				return nil, err
			}
			return a.pkg, nil
		}),
		Policy: r.gw, Actions: r.co, Adapters: adapterFor, Providers: sel, Audit: r.alog, Clock: r.clk,
		Config:   dialogue.Config{TemplateOnly: allActs()},
		Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) app(id string) (*tenantApp, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.apps[id]
	if !ok {
		return nil, contracts.Newf(contracts.ErrUnavailable, "application %s is not registered", id)
	}
	return a, nil
}

// register adds the application whose signed package is in dir: it loads the package,
// activates it in the registry and gives it a host behind the scope-checking wrapper. It is
// what registering one more application costs the process, and what the memory test measures.
func (r *rig) register(dir string) *tenantApp {
	r.t.Helper()
	src, err := apppkg.FromPath(dir)
	if err != nil {
		r.t.Fatal(err)
	}
	res, err := apppkg.Load(context.Background(), src, apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		r.t.Fatalf("load %s: %v %v", dir, err, res)
	}
	pkg := activePackage{capabilities.FromApppkg(res.Package)}
	id := string(res.Package.Manifest.Application.ID)
	host := clinic.NewClinicHost(r.clk)
	calls := newCounting(rebrand{host.Adapter()})
	guard, err := adapters.Wrap(calls, adapters.Options{ApplicationID: id})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.Activate(pkg); err != nil {
		r.t.Fatalf("activate %s: %v", id, err)
	}
	a := &tenantApp{id: id, pkg: pkg, host: host, calls: calls, guard: guard}
	r.mu.Lock()
	r.apps[id] = a
	r.mu.Unlock()
	return a
}

// ---- scripting and running turns -----------------------------------------------------------

// propose scripts the model's next TaskProposal and nothing more: it steps aside after one
// use, so a turn that makes a call nobody scripted fails.
func (r *rig) propose(capability string, fields map[string]any) {
	r.t.Helper()
	b, err := json.Marshal(map[string]any{
		"capability_id": capability, "fields": fields, "entity_refs": []any{}, "intent": "task",
		"uncertainty": map[string]any{"overall": 0.9},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.model.Reset()
	r.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(string(b)).Times(1)
}

func (r *rig) say(sc contracts.Scope, lang, text string) dialogue.Result {
	r.t.Helper()
	res, err := r.eng.Turn(context.Background(), sc, dialogue.TurnInput{Text: text, Language: lang})
	if err != nil {
		r.t.Fatalf("Turn: %v", err)
	}
	return res
}

// the utterances and the records of the clinic stand-in's world (tests/conversation)
const (
	sayBook = "Book Luca Ferri for 2026-10-14 10:30"
	sayFree = "Which appointments are free on 2026-10-14?"
)

var bookLuca = map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"}

// auditText exports the audit events of the scope's own application, tenant and workspace.
func (r *rig) auditText(sc contracts.Scope) (string, int) {
	r.t.Helper()
	sc.CapabilityScopes = append(append([]string(nil), sc.CapabilityScopes...), audit.CapabilityExport)
	var sb strings.Builder
	res, err := r.alog.Export(context.Background(), sc, io.Writer(&sb), audit.ExportOptions{})
	if err != nil {
		r.t.Fatalf("audit export: %v", err)
	}
	return sb.String(), res.Events
}

func describeErr(err error) string {
	if err == nil {
		return "no error"
	}
	return fmt.Sprintf("%s (%v)", contracts.CodeOf(err), err)
}
