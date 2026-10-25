package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// clinicPackageDir is the real, signed Application Integration Package of the clinic
// reference application. Every test serves its conversations from it: the capabilities,
// field definitions, ambiguity rules, terminology, conversation contracts and host
// policy are the package's own, never a copy.
const clinicPackageDir = "../../examples/reference-apps/clinic-booking/package"

// clinicNow is "today" of every conversation: a Monday morning. The clinic's slots start
// two to four days later, inside the booking window of 60 minutes to 90 days.
var clinicNow = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

func ctxb() context.Context { return context.Background() }

// ---- the package ----

// activePackage is the clinic package as the registry, the policy gateway, the action
// coordinator and the dialogue engine see it once the integration audit has activated it.
// A package fresh from the loader is quarantined and nothing in apppkg can activate it
// (only the audit does); the audit is not what these tests are about, so the one thing
// this wrapper changes is the activation state it reports. Everything else is the loaded
// package, byte for byte.
type activePackage struct{ capabilities.Package }

func (activePackage) Status() policy.PackageStatus { return policy.StatusActive }

func loadPackage(t testing.TB, dir string) capabilities.Package {
	t.Helper()
	res, err := apppkg.Load(ctxb(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatalf("load %s: %v", dir, err)
	}
	if res.Package == nil {
		t.Fatalf("package %s did not load: %v", dir, res.Findings)
	}
	return activePackage{capabilities.FromApppkg(res.Package)}
}

// copyTree copies a package directory so a test can change it and sign it again.
func copyTree(t testing.TB, from string) string {
	t.Helper()
	to := t.TempDir()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return to
}

// stepUpClinicDir is the clinic package under a deployment policy that requires a
// step-up to change a patient's contact details (the phone and the email that booking
// confirmations are sent to are an account-takeover route). Only the host policy file
// changes, by the one thing a policy may do to a risk level, which is raise it; the
// package is then signed again with the TEST-ONLY publisher key, like any edited package.
func stepUpClinicDir(t testing.TB) string {
	t.Helper()
	dir := copyTree(t, clinicPackageDir)
	p := filepath.Join(dir, "policies", "default-access.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const from = "risk_overrides: []"
	if !strings.Contains(string(b), from) {
		t.Fatalf("%s has no empty risk_overrides to raise", p)
	}
	const to = `risk_overrides:
  - capability_id: patients.update_contact
    minimum_risk_level: 3
    reason: The phone number and the email address are where booking confirmations are sent, so changing them is an account-takeover route and must be done in the clinic application's own authenticated interface.`
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = trust.SignPackageDir(dir, trust.SignOptions{
		PackageIdentity: trust.PackageIdentity{ApplicationID: clinicApp, PackageVersion: "1.0.0"},
		Keys:            []trust.SigningKey{devkeys.SigningKey()},
		Now:             time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2036, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("sign %s: %v", dir, err)
	}
	return dir
}

// ---- scopes ----

func receptionScope() contracts.Scope { return RoleScope("receptionist") }
func doctorScope() contracts.Scope    { return RoleScope("doctor") }
func patientScope() contracts.Scope   { return RoleScope("patient") }

// ---- the world ----

// policyRecorder keeps every decision the policy gateway made.
type policyRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
}

func (r *policyRecorder) Emit(_ context.Context, rec policy.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

func (r *policyRecorder) denied(capabilityID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, rec := range r.recs {
		if rec.CapabilityID == capabilityID && rec.Outcome == policy.OutcomeDeny {
			n++
		}
	}
	return n
}

type teeSink []policy.Sink

func (t teeSink) Emit(ctx context.Context, r policy.Record) error {
	for _, s := range t {
		if err := s.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// world is everything a conversation runs against, wired exactly as a composition root
// wires it: a throwaway store, the real audit log and policy gateway, the capability
// registry over the real clinic package, the clinic stand-in behind adapters.Wrap, the
// action coordinator, the session store, the scripted model behind the local selector and
// the dialogue engine.
type world struct {
	t     testing.TB
	db    *store.DB
	clk   *clock.Fake
	alog  *audit.Log
	gw    *policy.Gateway
	recs  *policyRecorder
	host  *ClinicHost
	guard adapters.AppAdapter
	co    *actions.Coordinator
	reg   *capabilities.Registry
	store *sessions.Store
	model *pfake.Provider
	eng   *dialogue.Engine
	pkg   capabilities.Package
}

type worldOpts struct {
	packageDir string
	// modelResponses lets the model phrase the responses of every act (the default is the
	// deterministic template for every act, so a test sees exactly what the core decided).
	modelResponses bool
	cfg            dialogue.Config
}

type worldOpt func(*worldOpts)

func withPackageDir(dir string) worldOpt { return func(o *worldOpts) { o.packageDir = dir } }
func withModelResponses() worldOpt       { return func(o *worldOpts) { o.modelResponses = true } }

// allActs lists every dialogue act, for Config.TemplateOnly.
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

func newWorld(t testing.TB, opts ...worldOpt) *world {
	t.Helper()
	o := worldOpts{packageDir: clinicPackageDir}
	for _, f := range opts {
		f(&o)
	}
	if !o.modelResponses {
		o.cfg.TemplateOnly = allActs()
	}
	w := &world{t: t, clk: clock.NewFake(clinicNow), recs: &policyRecorder{}}
	w.db = storetest.Open(t, nil)
	var err error
	if w.alog, err = audit.New(w.db, audit.Options{Clock: w.clk}); err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](w.alog, audit.EventPolicyDecision, func(r policy.Record) (audit.Locator, error) {
		s := r.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	w.gw = policy.NewGateway(w.clk, policy.WithSink(teeSink{sink, w.recs}))
	w.host = NewClinicHost(w.clk)
	if w.guard, err = adapters.Wrap(clinicAdapter{w.host}, adapters.Options{ApplicationID: clinicApp}); err != nil {
		t.Fatal(err)
	}
	w.pkg = loadPackage(t, o.packageDir)
	w.reg = capabilities.NewRegistry(nil, capabilities.WithClock(w.clk))
	if err := w.reg.Activate(w.pkg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	w.co, err = actions.New(actions.Deps{
		DB: w.db, Policy: w.gw, Audit: w.alog,
		Packages: actions.PackageSourceFunc(func(context.Context, string) (policy.ActivePackage, error) { return w.pkg, nil }),
		Adapters: actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return w.guard, nil }),
		Clock:    w.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.store, err = sessions.New(w.db, w.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	w.model = pfake.New(pfake.WithClock(w.clk))
	budget := providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute}
	sel, err := dialogue.LocalSelector(w.model, budget)
	if err != nil {
		t.Fatal(err)
	}
	w.eng, err = dialogue.New(dialogue.Deps{
		Sessions: w.store, Registry: w.reg,
		Packages: dialogue.PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return w.pkg, nil }),
		Policy:   w.gw, Actions: w.co,
		Adapters:  actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return w.guard, nil }),
		Providers: sel, Audit: w.alog, Clock: w.clk, Config: o.cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// ---- scripting the model ----

// proposalJSON builds a model output for a TaskProposal: the capability, the intent, the
// field values the model read out of the words, and the mentions of entities it found
// (each optionally with an ID it believes is right, which the host only ever treats as a
// hint).
func proposalJSON(capability, intent string, fields map[string]any, refs ...map[string]any) string {
	m := map[string]any{
		"capability_id": nil, "fields": map[string]any{}, "entity_refs": []any{}, "intent": intent,
		"uncertainty": map[string]any{"overall": 0.9},
	}
	if capability != "" {
		m["capability_id"] = capability
	}
	if fields != nil {
		m["fields"] = fields
	}
	if len(refs) > 0 {
		list := make([]any, len(refs))
		for i, r := range refs {
			list[i] = r
		}
		m["entity_refs"] = list
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// propose scripts the model's next TaskProposal (model call 1, a JSON-schema constrained
// request) and nothing more: it steps aside after one use, so a turn that makes a second
// proposal call (or one the test did not expect) has no script and fails.
func (w *world) propose(capability, intent string, fields map[string]any, refs ...map[string]any) {
	w.t.Helper()
	w.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(capability, intent, fields, refs...)).Times(1)
}

// phrase scripts the next response text (model call 2, an unconstrained request).
func (w *world) phrase(text string) {
	w.t.Helper()
	w.model.On(pfake.Constrained(providers.ConstraintNone)).Reply(text).Times(1)
}

// proposals counts the model calls that were a task proposal, and responses the calls
// that phrased a response.
func (w *world) proposals() int { return w.callsOf(providers.ConstraintJSONSchema) }
func (w *world) responses() int { return w.callsOf(providers.ConstraintNone) }

func (w *world) callsOf(kind providers.ConstraintKind) int {
	n := 0
	for _, p := range w.model.Prompts() {
		if p.Chat.Output.Kind() == kind {
			n++
		}
	}
	return n
}

// promptText concatenates everything every call sent to the model.
func (w *world) promptText() string {
	var sb strings.Builder
	for _, p := range w.model.Prompts() {
		for _, s := range p.Texts() {
			sb.WriteString(s)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// ---- running turns ----

// turn runs one turn and fails the test on an error: an error means the turn could not
// be run at all, and every refusal or failure is a Result with a typed code.
func (w *world) turn(sc contracts.Scope, in dialogue.TurnInput) dialogue.Result {
	w.t.Helper()
	res, err := w.eng.Turn(ctxb(), sc, in)
	if err != nil {
		w.t.Fatalf("Turn: %v", err)
	}
	if strings.TrimSpace(res.Response.Text) == "" {
		w.t.Fatalf("empty response for act %s", res.Directive.Act)
	}
	// A confirmation secret goes to the client and nowhere else: not into the marshalled
	// result, which is what a gateway would log.
	if b, err := json.Marshal(res); err == nil && strings.Contains(string(b), "cfm_") {
		w.t.Fatalf("a confirmation token leaked into the marshalled result: %s", b)
	}
	return res
}

func (w *world) say(sc contracts.Scope, lang, text string) dialogue.Result {
	w.t.Helper()
	return w.turn(sc, dialogue.TurnInput{Text: text, Language: lang})
}

func (w *world) confirm(sc contracts.Scope, prev dialogue.Result) dialogue.Result {
	w.t.Helper()
	if prev.Prepared == nil || prev.Confirmation.IsZero() {
		w.t.Fatalf("nothing to confirm: prepared = %v, token zero = %v", prev.Prepared, prev.Confirmation.IsZero())
	}
	return w.turn(sc, dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(prev.Prepared.PreparationID), Token: prev.Confirmation}})
}

func (w *world) choose(sc contracts.Scope, choiceID string) dialogue.Result {
	w.t.Helper()
	return w.turn(sc, dialogue.TurnInput{ChoiceID: choiceID})
}

func (w *world) session(sc contracts.Scope) sessions.Session {
	w.t.Helper()
	s, err := w.store.Open(ctxb(), sc)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

func (w *world) goal(sc contracts.Scope) *sessions.Goal { return w.session(sc).State.Goal }

// prepStates lists the state of every preparation the coordinator holds, oldest first.
func (w *world) prepStates() []string {
	w.t.Helper()
	rows, err := w.db.QueryContext(ctxb(), `SELECT state FROM action_preparations ORDER BY prepared_at, rowid`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// ---- assertions ----

func wantAct(t testing.TB, res dialogue.Result, a string) {
	t.Helper()
	if string(res.Directive.Act) != a {
		t.Fatalf("act = %s (code %q, response %q), want %s", res.Directive.Act, res.Code, res.Response.Text, a)
	}
}

func wantCode(t testing.TB, res dialogue.Result, code contracts.ErrCode) {
	t.Helper()
	if res.Code != code {
		t.Fatalf("code = %q, want %q (act %s, response %q)", res.Code, code, res.Directive.Act, res.Response.Text)
	}
}

func mustContain(t testing.TB, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s does not contain %q:\n%s", what, needle, haystack)
	}
}

func mustNotContain(t testing.TB, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s contains %q:\n%s", what, needle, haystack)
	}
}

// noWrites asserts the application saw no Prepare and no Commit and applied no effect:
// nothing consequential was done.
func (w *world) noWrites() {
	w.t.Helper()
	if n := w.host.count(adapters.OpPrepare); n != 0 {
		w.t.Fatalf("the host saw %d Prepare calls, want 0", n)
	}
	w.noCommits()
}

// noCommits asserts nothing was committed or applied (a preparation may have been staged).
func (w *world) noCommits() {
	w.t.Helper()
	if n := w.host.count(adapters.OpCommit); n != 0 {
		w.t.Fatalf("the host saw %d Commit calls, want 0", n)
	}
	if n := w.host.writes(); n != 0 {
		w.t.Fatalf("the host applied %d effects, want 0", n)
	}
}

func hasTransition(res dialogue.Result, name string) bool {
	for _, n := range res.Trace.Transitions {
		if n == name {
			return true
		}
	}
	return false
}

// factOf returns the display (or canonical) text of the first fact named field.
func factValue(res dialogue.Result, field string) (string, bool) {
	for _, f := range res.Directive.Facts {
		if string(f.Field) == field {
			b, _ := json.Marshal(f)
			var m struct {
				Value struct {
					Value any `json:"value"`
				} `json:"value"`
				Display string `json:"display"`
			}
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.UseNumber()
			_ = dec.Decode(&m)
			if m.Display != "" {
				return m.Display, true
			}
			return fmt.Sprint(m.Value.Value), true
		}
	}
	return "", false
}
