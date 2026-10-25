package aguigold

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// The rig is the service the golden streams are recorded from: the real session store, dialogue engine, policy gateway,
// action coordinator, files service and stream hub behind the real gateway handlers, with identity verification of
// single-use context tokens, the in-memory host application (internal/adapters/fake) behind adapters.Wrap, and a
// scripted model provider. Only the model is scripted, and the clock is fixed, so what the engine decides, and the
// bytes it sends, are the same on every run; the identifiers it mints are random and are canonicalised in the recorded
// files (canon.go).

const (
	rigApp     = identitytest.DefaultApplicationID
	rigTenant  = identitytest.DefaultTenant
	rigWsp     = identitytest.DefaultWorkspace
	rigSession = identitytest.DefaultSession

	capRead   = fake.ReadCapability   // items.get
	capCreate = fake.ActionCapability // items.create
)

var rigNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

var rigCaps = []string{capRead, capCreate}

type rig struct {
	t      *testing.T
	clk    *clock.Fake
	db     *store.DB
	host   *fake.App
	model  *pfake.Provider
	eng    *dialogue.Engine
	hub    *stream.Hub
	runs   *gateway.AGUIRuns
	srv    *gateway.Server
	issuer *identitytest.Issuer
}

// rigPackage is the application package: items.get (a read) and items.create (an action with a free-text field, a
// number and a field with a few allowed values, so every kind of question can be asked). It satisfies
// capabilities.Package without a signed archive.
type rigPackage struct {
	*policy.Package
	specs     map[string]ap.Capability
	contracts map[[2]string]ap.ConversationContract
	terms     []ap.TerminologyTerm
}

func (p *rigPackage) ContentHash() string               { return strings.Repeat("0", 61) + "abc" }
func (p *rigPackage) SupportedLanguages() []string      { return []string{"en", "it"} }
func (p *rigPackage) Terminology() []ap.TerminologyTerm { return p.terms }
func (p *rigPackage) Spec(id string) (ap.Capability, bool) {
	s, ok := p.specs[id]
	return s, ok
}
func (p *rigPackage) ContractFor(c, l string) (ap.ConversationContract, bool) {
	x, ok := p.contracts[[2]string{c, l}]
	return x, ok
}

var _ capabilities.Package = (*rigPackage)(nil)

func rigI64(n int64) *int64     { return &n }
func rigF64(n float64) *float64 { return &n }

func buildPackage(t testing.TB) *rigPackage {
	t.Helper()
	slug := func(s string) ap.CapabilityID { return ap.CapabilityID(s) }
	specs := map[string]ap.Capability{
		capRead: {
			ID: slug(capRead), Audience: ap.CapabilityAudienceUser, Title: "Get an item",
			Purpose: "Look up one item by its ID and report its name. Use it when the user asks about an item.",
			Examples: []ap.CapabilityExample{
				{ID: "get-en", Language: "en", TestOnly: true, Utterance: "Show item item-1"},
				{ID: "get-it", Language: "it", TestOnly: true, Utterance: "Mostra l'articolo item-1"},
			},
			Fields:      []ap.CapabilityField{{Name: "item_id", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item to look up."}},
			EntityTypes: map[string]ap.CapabilityEntityType{},
		},
		capCreate: {
			ID: slug(capCreate), Audience: ap.CapabilityAudienceUser, Title: "Create an item",
			Purpose: "Create a new item with a name, a quantity and a size. The item is prepared first and created only after the user confirms.",
			Examples: []ap.CapabilityExample{
				{ID: "create-en", Language: "en", TestOnly: true, Utterance: "Create an item named widget, quantity 3, small"},
			},
			Fields: []ap.CapabilityField{
				{Name: "name", Type: ap.CapabilityFieldTypeString, Required: true, Description: "The item name.", MinLength: rigI64(1), MaxLength: rigI64(64)},
				{Name: "quantity", Type: ap.CapabilityFieldTypeInteger, Required: true, Description: "How many.", Minimum: rigF64(1), Maximum: rigF64(99)},
				{Name: "size", Type: ap.CapabilityFieldTypeEnum, Required: true, Description: "The size.", Values: []string{"small", "medium", "large"}},
			},
			EntityTypes: map[string]ap.CapabilityEntityType{},
			EffectClass: ap.CapabilityEffectClass(policy.EffectExternal), RiskLevel: ap.RiskLevel(contracts.RiskConsequential),
			Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeExplicit},
		},
	}
	decls := []policy.CapabilityDecl{
		{ID: capRead, Audience: policy.AudienceUser, EffectClass: policy.EffectNone, RiskLevel: contracts.RiskRead,
			Confirmation: policy.ConfirmNone, Fields: []policy.FieldDecl{{Name: "item_id", Required: true}}},
		{ID: capCreate, Audience: policy.AudienceUser, EffectClass: policy.EffectExternal, RiskLevel: contracts.RiskConsequential,
			Confirmation: policy.ConfirmExplicit, Fields: []policy.FieldDecl{{Name: "name", Required: true}, {Name: "quantity", Required: true}, {Name: "size", Required: true}}},
	}
	p := &rigPackage{specs: specs, contracts: map[[2]string]ap.ConversationContract{}}
	p.terms = []ap.TerminologyTerm{
		{ID: "create", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "create", Synonyms: []ap.Label{"add", "new"}}, "it": {Preferred: "crea", Synonyms: []ap.Label{"aggiungi", "nuovo"}}}},
		{ID: "item", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "item", Synonyms: []ap.Label{"thing", "article"}}, "it": {Preferred: "articolo", Synonyms: []ap.Label{"oggetto"}}}},
		{ID: "lookup", Labels: map[string]ap.TerminologyLabel{
			"en": {Preferred: "get", Synonyms: []ap.Label{"find", "show", "lookup"}}, "it": {Preferred: "trova", Synonyms: []ap.Label{"mostra", "cerca"}}}},
		{ID: "name", Labels: map[string]ap.TerminologyLabel{"en": {Preferred: "name"}, "it": {Preferred: "nome"}}},
		{ID: "quantity", Labels: map[string]ap.TerminologyLabel{"en": {Preferred: "quantity"}, "it": {Preferred: "quantità"}}},
		{ID: "size", Labels: map[string]ap.TerminologyLabel{"en": {Preferred: "size"}, "it": {Preferred: "taglia"}}},
	}
	sort.Slice(p.terms, func(i, j int) bool { return p.terms[i].ID < p.terms[j].ID })
	prompts := func(items ...[2]string) []ap.ConversationContractFieldPrompt {
		var out []ap.ConversationContractFieldPrompt
		for _, it := range items {
			out = append(out, ap.ConversationContractFieldPrompt{Field: common.FieldName(it[0]), Label: ap.Label(it[1]), Prompt: ap.Template("What is the " + it[1] + "?")})
		}
		return out
	}
	for _, l := range []string{"en", "it"} {
		p.contracts[[2]string{capRead, l}] = ap.ConversationContract{CapabilityID: slug(capRead), Language: ap.LanguageTag(l),
			Terminology: []ap.Slug{"lookup", "item"}, FieldPrompts: prompts([2]string{"item_id", "Item code"})}
		p.contracts[[2]string{capCreate, l}] = ap.ConversationContract{CapabilityID: slug(capCreate), Language: ap.LanguageTag(l),
			Terminology:  []ap.Slug{"create", "item", "quantity", "name", "size"},
			FieldPrompts: prompts([2]string{"name", "Item name"}, [2]string{"quantity", "Quantity"}, [2]string{"size", "Size"})}
	}
	hp, err := policy.ParseHostPolicy([]byte(`{
  "policy_id": "default-access",
  "role_grants": [
    { "role": "member", "capabilities": ["items.get", "items.create"] }
  ],
  "risk_overrides": [],
  "field_projection": []
}`))
	if err != nil {
		t.Fatal(err)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: rigApp, Version: "1.0.0", Status: policy.StatusActive,
		Roles: []string{"member"}, Capabilities: decls, Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		t.Fatalf("NewPackage: %v", err)
	}
	p.Package = pp
	return p
}

// rigApps is the gateway.ApplicationService of the rig: one in-memory package.
type rigApps struct{ pkg *rigPackage }

func (a *rigApps) Package(_ context.Context, applicationID string) (capabilities.Package, error) {
	if a.pkg.ApplicationID() != applicationID {
		return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	return a.pkg, nil
}

func (a *rigApps) Records(_ context.Context, applicationID string) ([]apppkg.Record, error) {
	return []apppkg.Record{{ApplicationID: applicationID, ManifestHash: a.pkg.ContentHash(), PackageVersion: "1.0.0", State: apppkg.StateActive,
		InstalledAt: rigNow, StatusChangedAt: rigNow}}, nil
}

func (a *rigApps) Register(context.Context, contracts.Scope, []byte) (*apppkg.Installed, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "the in-memory application does not register packages")
}

var _ gateway.ApplicationService = (*rigApps)(nil)

// newRig builds the service. uiTools are the names of the UI tools the host registers for the application (the engine's
// allow-list, dialogue.Deps.UITools); with none, no UI tool is ever offered, which is the default of a deployment.
func newRig(t *testing.T, uiTools ...string) *rig {
	t.Helper()
	r := &rig{t: t, clk: clock.NewFake(rigNow)}
	r.db = storetest.Open(t, nil)
	alog, err := audit.New(r.db, audit.Options{Clock: r.clk})
	if err != nil {
		t.Fatal(err)
	}
	sink := audit.NewRecordSink[policy.Record](alog, audit.EventPolicyDecision, func(rec policy.Record) (audit.Locator, error) {
		s := rec.Subject
		return audit.ForIDs(s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID)
	})
	gw := policy.NewGateway(r.clk, policy.WithSink(sink))
	r.host = fake.NewApp(fake.WithClock(r.clk))
	guard, err := adapters.Wrap(fake.NewAdapter(r.host), adapters.Options{ApplicationID: rigApp})
	if err != nil {
		t.Fatal(err)
	}
	source := actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return guard, nil })

	pkg := buildPackage(t)
	apps := &rigApps{pkg: pkg}
	registry := capabilities.NewRegistry(nil, capabilities.WithClock(r.clk))
	if err := registry.Activate(pkg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	co, err := actions.New(actions.Deps{
		DB: r.db, Policy: gw, Audit: alog, Clock: r.clk, Adapters: source,
		Packages: actions.PackageSourceFunc(func(ctx context.Context, app string) (policy.ActivePackage, error) {
			p, err := apps.Package(ctx, app)
			if err != nil {
				return nil, err
			}
			return p, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(r.db, r.clk, sessions.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r.model = pfake.New(pfake.WithClock(r.clk))
	sel, err := dialogue.LocalSelector(r.model, providers.Budget{MaxPromptTokens: 4096, MaxOutputTokens: 1024, MaxBatchItems: 4, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var uiRegistry dialogue.UIToolRegistry
	if len(uiTools) > 0 {
		uiRegistry = dialogue.StaticUITools(map[string][]string{rigApp: uiTools})
	}
	r.eng, err = dialogue.New(dialogue.Deps{
		Sessions: sess, Registry: registry, Packages: apps, Policy: gw, Actions: co, Adapters: source,
		Providers: sel, Audit: alog, Clock: r.clk,
		Decision: dialogue.NoopDecisionStage{}, Guard: dialogue.LocalOnlyGuard{}, UITools: uiRegistry,
	})
	if err != nil {
		t.Fatal(err)
	}
	filesSvc, err := files.New(files.Options{DB: r.db, Clock: r.clk, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	t.Cleanup(func() { _ = filesSvc.Close() })
	historySvc, err := history.New(history.Deps{DB: r.db, Transcripts: sess, Audit: alog, Clock: r.clk})
	if err != nil {
		t.Fatalf("history.New: %v", err)
	}
	r.hub = stream.NewHub(stream.Config{Clock: r.clk})
	r.runs = gateway.NewAGUIRuns(r.clk)

	services := gateway.NewServices()
	if uiRegistry != nil {
		// The composition root gives the gateway the allow-list the engine was built with.
		if err := gateway.Provide(services, uiRegistry); err != nil {
			t.Fatalf("provide the UI tool allow-list: %v", err)
		}
	}
	for _, perr := range []error{
		gateway.Provide(services, sess),
		gateway.Provide(services, r.eng),
		gateway.Provide[gateway.ActionService](services, co),
		gateway.Provide[gateway.EventsIntakes](services, rigEventsIntakes{}),
		gateway.Provide(services, gateway.NewEventsFeed(gateway.EventsFeedConfig{Clock: r.clk})),
		gateway.Provide[gateway.ApplicationService](services, apps),
		gateway.Provide(services, gw),
		gateway.Provide[actions.AdapterSource](services, source),
		gateway.Provide[audit.Appender](services, alog),
		gateway.Provide[gateway.FilesService](services, filesSvc),
		gateway.Provide[gateway.AttachModelFiles](services, filesSvc),
		gateway.Provide(services, r.hub),
		gateway.Provide(services, r.runs),
		// The areas that share a service with the ones the rig uses are provided too: a server refuses to start with an area half wired.
		gateway.Provide(services, historySvc),
		gateway.Provide[gateway.RAGSources](services, rigRAGSources{}),
	} {
		if perr != nil {
			t.Fatalf("provide a service: %v", perr)
		}
	}
	keys := identity.NewKeyRegistry(nil)
	r.issuer = identitytest.NewIssuer(t, identitytest.IssuerConfig{ApplicationID: rigApp, Clock: r.clk})
	if err := keys.Register(r.issuer.Key()); err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.Options{
		DeploymentID: identitytest.DefaultDeployment, Keys: keys, Replay: identity.NewSQLiteReplayCache(r.db, r.clk, nil), Clock: r.clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.NewRegistry(r.clk)
	if err := reg.Register(providers.RoleController, r.model); err != nil {
		t.Fatal(err)
	}
	r.srv, err = gateway.NewServer(gateway.Options{
		Clock: r.clk, Logger: slog.New(slog.DiscardHandler), Providers: reg,
		V1Auth: identity.Middleware(verifier), Services: services,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only the areas the recordings use must be mounted (the settings area has services the rig does not run).
	for _, area := range []string{"v1_agui", "v1_sessions", "v1_turnevents", "v1_actions", "v1_apps", "v1_files"} {
		if un := r.srv.Unmounted()[area]; len(un) != 0 {
			t.Fatalf("the area %s is not mounted: it needs %v", area, un)
		}
	}
	return r
}

// rigRAGSources stands in for the RAG source registry the rig does not run: every call answers unavailable.
type rigRAGSources struct{}

var _ gateway.RAGSources = rigRAGSources{}

func rigRAGUnavailable() error { return contracts.New(contracts.ErrUnavailable, "the rig runs no RAG") }

func (rigRAGSources) Register(context.Context, contracts.Scope, gateway.RAGSourcesRegistration) (gateway.RAGSourcesRecord, error) {
	return gateway.RAGSourcesRecord{}, rigRAGUnavailable()
}

func (rigRAGSources) Reaudit(context.Context, contracts.Scope) (gateway.RAGSourcesReaudit, error) {
	return gateway.RAGSourcesReaudit{}, rigRAGUnavailable()
}

func (rigRAGSources) Remove(context.Context, contracts.Scope, string) error {
	return rigRAGUnavailable()
}

func (rigRAGSources) Status(context.Context, contracts.Scope, string) ([]gateway.RAGSourcesRecord, error) {
	return nil, rigRAGUnavailable()
}

// ---- requests ----

var rigTokenMu sync.Mutex

// token mints a fresh single-use context token for the default principal and session.
func (r *rig) token() string {
	rigTokenMu.Lock()
	defer rigTokenMu.Unlock()
	return r.issuer.Token(func(d *identitytest.Draft) { d.Set(identity.ClaimCapabilityScopes, rigCaps) })
}

func (r *rig) call(method, path, contentType string, body []byte, headers ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+r.token())
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// post sends a run (a RunAgentInput as JSON) to POST /v1/agui.
func (r *rig) post(body map[string]any, headers ...string) *httptest.ResponseRecorder {
	r.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.call(http.MethodPost, "/v1/agui", "application/json", raw, headers...)
}

// ---- scripting the model ----

func rigProposal(capability, intent string, fields map[string]any) string {
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
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (r *rig) onProposal(capability, intent string, fields map[string]any) {
	r.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(rigProposal(capability, intent, fields))
}

func (r *rig) onResponse(text string) {
	r.model.On(pfake.Constrained(providers.ConstraintNone)).Reply(text)
}

// seedItem gives the host application the item that the read scenarios look up.
func (r *rig) seedItem() { r.host.Seed(rigTenant, rigWsp, "item-1", "gadget") }

// ---- run inputs ----

func runInput(run string, extra map[string]any, messages ...map[string]any) map[string]any {
	if messages == nil {
		messages = []map[string]any{}
	}
	body := map[string]any{"threadId": rigSession, "runId": run, "state": map[string]any{}, "messages": messages, "tools": []any{}, "context": []any{}, "forwardedProps": map[string]any{}}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func userMessage(id, text string) map[string]any {
	return map[string]any{"id": id, "role": "user", "content": text}
}

func resumeInput(prep string, approved bool) map[string]any {
	return map[string]any{"resume": []any{map[string]any{"interruptId": prep, "status": "resolved", "payload": map[string]any{"approved": approved}}}}
}

// ---- files ----

func rigPNG() []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

// upload sends one file to POST /v1/files and returns its id.
func (r *rig) upload(name, contentType string, content []byte) string {
	r.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, name))
	h.Set("Content-Type", contentType)
	w, err := mw.CreatePart(h)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader(content)); err != nil {
		r.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		r.t.Fatal(err)
	}
	rec := r.call(http.MethodPost, "/v1/files", mw.FormDataContentType(), b.Bytes())
	if rec.Code != http.StatusCreated {
		r.t.Fatalf("upload = %d: %s", rec.Code, rec.Body.String())
	}
	var ref struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ref); err != nil || ref.FileID == "" {
		r.t.Fatalf("upload response: %v: %s", err, rec.Body.String())
	}
	return ref.FileID
}
