package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/analyst/compute"
	"github.com/buxee/alix-ai/internal/analyst/domain"
	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/analyst/settings"
	"github.com/buxee/alix-ai/internal/analyst/web"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// ADR 0047 analyst companion wiring for `alixai serve`. Defaults stay off: with
// dialogue.analyst.enabled false the runner is not attached and Local / 1.4
// behaviour is unchanged. AnalystService is still provided so GET /v1/me/analyst
// answers with the effective (all-off) set.

const analystProvideName = "analyst"

func init() {
	registerServeProvider(analystProvideName, analystProvide)
	serveProfileNewEngine = analystEngine(serveProfileNewEngine)
}

// analystWire holds process-wide seams built once in analystProvide and read when
// the dialogue engine is constructed.
var analystWire struct {
	mu             sync.Mutex
	store          *settings.Store
	service        *settings.Service
	sources        *analystSources
	runner         *loop.Runner
	capable        func(context.Context, contracts.Scope) bool
	secrets        web.SecretResolver
	webQueryAllows func(context.Context, contracts.Scope) bool
	provider       providers.TextGenerator // optional; set when engine deps are known
}

// analystWebQueryAllows is the web_query gate. External build replaces it with the
// dataegress.WebQuery probe; noexternal leaves the default (always false).
var analystWebQueryAllows = func(context.Context, contracts.Scope) bool { return false }

// analystNativeCaller is the provider-native search seam (nil under noexternal).
var analystNativeCaller web.NativeCaller

type nativeCallerFunc func(ctx context.Context, scope contracts.Scope, query string, allowDomains []string) (web.NativeCallResult, error)

func (f nativeCallerFunc) SearchWeb(ctx context.Context, scope contracts.Scope, query string, allowDomains []string) (web.NativeCallResult, error) {
	return f(ctx, scope, query, allowDomains)
}

// dialogueAnalystWebProvider reads dialogue.analyst.web.provider (tests may replace).
var dialogueAnalystWebProvider = func() string {
	return dialogue.Loaded().Analyst.Web.Provider
}

// dialogueAnalystWebFallback reads dialogue.analyst.web.fallback.
var dialogueAnalystWebFallback = func() string {
	return dialogue.Loaded().Analyst.Web.Fallback
}

func buildWebSearchChain(webCfg *an.ConfigWebConfig, turns *webTurnBag, allows func(context.Context, contracts.Scope) bool) (loop.WebSearcher, error) {
	secrets := analystDynamicSecrets{}
	primary := *webCfg.Provider
	chain := &web.ChainSearcher{
		AllowDomains: func(ctx context.Context, scope contracts.Scope) []string {
			return turns.policyFor(ctx, scope).Allow
		},
		HostFilter: func(ctx context.Context, scope contracts.Scope) web.HostPolicy {
			return turns.policyFor(ctx, scope)
		},
	}
	if primary == an.ConfigWebConfigProviderProviderNative {
		chain.Native = analystNativeCaller
		chain.NativeOK = func(ctx context.Context, scope contracts.Scope) bool {
			return allows != nil && allows(ctx, scope) &&
				analystWebQueryProviderOK(ctx, scope, pv.EgressPolicyWebQueryProviderProviderNative) &&
				analystNativeProfileOK(ctx, scope)
		}
		fb := an.ConfigWebConfigFallbackEmpty
		if webCfg.Fallback != nil {
			fb = *webCfg.Fallback
		}
		if fb != an.ConfigWebConfigFallbackEmpty {
			fp, err := web.NewFallbackProvider(fb, webCfg, secrets)
			if err != nil {
				return nil, err
			}
			chain.Fallback = fp
			chain.FallbackName = string(fb)
			prov := pv.EgressPolicyWebQueryProvider(fb)
			chain.FallbackOK = func(ctx context.Context, scope contracts.Scope) bool {
				return allows != nil && allows(ctx, scope) && analystWebQueryProviderOK(ctx, scope, prov)
			}
		}
		return webSearcherChainAdapt{inner: chain, turns: turns, allows: allows}, nil
	}
	prov, err := web.NewProvider(webCfg, secrets)
	if err != nil {
		return nil, err
	}
	chain.Fallback = prov
	chain.FallbackName = string(primary)
	wp := analystConfiguredWebProvider()
	chain.FallbackOK = func(ctx context.Context, scope contracts.Scope) bool {
		return allows != nil && allows(ctx, scope) && analystWebQueryProviderOK(ctx, scope, wp)
	}
	return webSearcherChainAdapt{inner: chain, turns: turns, allows: allows}, nil
}

// analystWebQueryProviderOK is set by the external build; default refuses.
var analystWebQueryProviderOK = func(context.Context, contracts.Scope, pv.EgressPolicyWebQueryProvider) bool {
	return false
}

// analystNativeProfileOK is set by the external build; true when the routed
// controller profile has native_web_search.
var analystNativeProfileOK = func(context.Context, contracts.Scope) bool { return false }

func analystConfiguredWebProvider() pv.EgressPolicyWebQueryProvider {
	switch dialogueAnalystWebProvider() {
	case "provider_native":
		return pv.EgressPolicyWebQueryProviderProviderNative
	case "brave":
		return pv.EgressPolicyWebQueryProviderBrave
	case "tavily":
		return pv.EgressPolicyWebQueryProviderTavily
	case "searxng":
		return pv.EgressPolicyWebQueryProviderSearxng
	default:
		return pv.EgressPolicyWebQueryProviderSearxng
	}
}

// newAnalystFetcher builds the SSRF-safe fetcher. External tests may replace it
// so a TLS fake can serve allow-listed pages under the dial policy.
var newAnalystFetcher = func() *web.Fetcher {
	return web.NewFetcher(nil, nil, ids.NewGenerator(nil))
}

// Optional External-mode budget hooks (nil under noexternal → turn ceiling only).
var (
	analystExternalReserve func(ctx context.Context, scope contracts.Scope, estToks int) (loop.BudgetReservation, error)
	analystExternalSettle  func(ctx context.Context, scope contracts.Scope, r loop.BudgetReservation, a loop.BudgetActual) error
)

// analystCapableLive is set by the External build to read the routed controller
// profile at call time. analystProvide closes over serveDeps before External is
// attached; using that stale copy would keep AnalystCapable false forever.
var analystCapableLive func(context.Context, contracts.Scope) bool

func analystProvide(_ context.Context, d serveDeps) error {
	store := settings.NewStore(d.DB, d.Clock, d.Audit)
	src := &analystSources{
		apps: func() (gateway.ApplicationService, bool) {
			return gateway.Lookup[gateway.ApplicationService](d.Services)
		},
		capable: analystCapableLookup(d),
	}
	svc := &settings.Service{Store: store, Auth: settings.DefaultAuthorizer{}, Source: src}
	if err := gateway.Provide[gateway.AnalystService](d.Services, svc); err != nil {
		return err
	}
	analystWire.mu.Lock()
	analystWire.store = store
	analystWire.service = svc
	analystWire.sources = src
	analystWire.capable = src.capable
	analystWire.mu.Unlock()

	cfg := dialogue.Loaded().Analyst
	if !cfg.Enabled {
		if d.Log != nil {
			d.Log.Info("analyst: dialogue.analyst.enabled is false; single-capability path only")
		}
		return nil
	}
	return nil
}

func analystEngine(next func(dialogue.Deps) (*dialogue.Engine, error)) func(dialogue.Deps) (*dialogue.Engine, error) {
	return func(d dialogue.Deps) (*dialogue.Engine, error) {
		cfg := dialogue.Loaded().Analyst
		if cfg.Enabled && d.Actions != nil && d.Providers != nil {
			runner, capable, err := buildAnalystRunner(d, cfg)
			if err != nil {
				return nil, err
			}
			d.Analyst = dialogue.AnalystDeps{Runner: runner, AnalystCapable: capable}
			analystWire.mu.Lock()
			analystWire.runner = runner
			analystWire.capable = capable
			if analystWire.sources != nil {
				analystWire.sources.capable = capable
			}
			analystWire.mu.Unlock()
		}
		return next(d)
	}
}

func buildAnalystRunner(d dialogue.Deps, cfg dialogue.AnalystConfig) (*loop.Runner, func(context.Context, contracts.Scope) bool, error) {
	capable := analystCapableLive
	if capable == nil {
		capable = analystWire.capable
	}
	if capable == nil {
		capable = func(context.Context, contracts.Scope) bool { return false }
	}
	eng := compute.New(compute.Options{Forecasts: cfg.Abilities.Forecasts})
	var searcher loop.WebSearcher
	var fetcher loop.WebFetcher
	var turns *webTurnBag
	webCfg := mapWebConfig(cfg.Web)
	if webCfg != nil && webCfg.Provider != nil && *webCfg.Provider != an.ConfigWebConfigProviderEmpty {
		if !ExternalProvidersBuilt() {
			return nil, nil, fmt.Errorf("dialogue.analyst.web: web search requires External mode")
		}
		wf := newAnalystFetcher()
		turns = &webTurnBag{
			deploy: web.HostPolicy{Allow: cfg.Web.AllowHosts, Deny: cfg.Web.DenyHosts},
			budget: web.Budget{MaxSearches: cfg.Web.MaxSearches, MaxFetches: cfg.Web.MaxFetches},
		}
		allows := analystWire.webQueryAllows
		if allows == nil {
			allows = analystWebQueryAllows
		}
		chain, err := buildWebSearchChain(webCfg, turns, allows)
		if err != nil {
			return nil, nil, fmt.Errorf("dialogue.analyst.web: %w", err)
		}
		searcher = chain
		fetcher = webFetcherAdapt{inner: wf, turns: turns}
	}
	var domainCls loop.DomainClassifier
	if d.Packages != nil {
		domainCls = &domainAdapt{
			packages: d.Packages,
			sel:      d.Providers,
			guard:    d.Guard,
		}
	}
	opt := dialogue.AnalystRunnerOptions{
		// Resolve store/sources at call time: the dialogue engine is built before
		// analystProvide fills analystWire (cmd_serve.go), so capture-at-build is nil.
		Abilities: &abilityAdapt{},
		Domain:    domainCls,
		Computer:  computeAdapt{eng: eng},
		Verifier:  analystVerifier{},
		Searcher:  searcher,
		Fetcher:   fetcher,
		IDs:       idsAdapt{},
		Packages:  d.Packages,
		Turns:     turns,
		Budget: &loopBudgetAdapt{
			sel: d.Providers, prompt: d.Config.Profile.Budget(),
			maxTurnToks: cfg.MaxTurnTokens,
		},
		OnProgress: func(ctx context.Context, scope contracts.Scope, turnID string, stage conversation.ProgressEventStage) {
			publishAnalystProgress(ctx, scope, turnID, stage)
		},
	}
	// Analyst steps carry CompactSchema; the dialogue profile prompt ceiling (e.g. lite
	// 2048) is too tight. Turn token budget is enforced by loopBudgetAdapt instead.
	// MaxOutputTokens still comes from the profile so modes EstimateRichCost admits the call.
	caller := &stepCallerAdapt{sel: d.Providers, guard: d.Guard, budget: d.Config.Profile.Budget()}
	runner, err := dialogue.NewAnalystRunnerFromConfig(cfg, d.Actions, func() contracts.Scope { return contracts.Scope{} }, caller, opt)
	if err != nil {
		return nil, nil, err
	}
	return runner, capable, nil
}

func publishAnalystProgress(_ context.Context, scope contracts.Scope, turnID string, stage conversation.ProgressEventStage) {
	hub := streamVerifiedHub.Load()
	if hub == nil || turnID == "" {
		return
	}
	turn, err := hub.Lookup(scope, turnID)
	if err != nil {
		turn, err = hub.Begin(scope, turnID)
		if err != nil {
			return
		}
	}
	if caps, ok := dialogue.ClientCapabilitiesForTurn(turnID); ok {
		turn.SetClientCapabilities(caps)
	}
	_ = turn.Publish(conversation.TurnEvent{Value: conversation.ProgressEvent{
		TurnID: act.TurnID(turnID), Stage: stage, State: conversation.ProgressEventStateStarted,
	}})
}

func mapWebConfig(w dialogue.AnalystWebConfig) *an.ConfigWebConfig {
	if strings.TrimSpace(w.Provider) == "" {
		return nil
	}
	p := an.ConfigWebConfigProvider(w.Provider)
	out := &an.ConfigWebConfig{Provider: &p, AllowHosts: w.AllowHosts, DenyHosts: w.DenyHosts}
	if w.CredRef != "" {
		c := w.CredRef
		out.CredRef = &c
	}
	if w.Origin != "" {
		o := w.Origin
		out.Origin = &o
	}
	if w.Fallback != "" {
		f := an.ConfigWebConfigFallback(w.Fallback)
		out.Fallback = &f
	}
	if w.MaxSearches > 0 {
		v := int64(w.MaxSearches)
		out.MaxSearches = &v
	}
	if w.MaxFetches > 0 {
		v := int64(w.MaxFetches)
		out.MaxFetches = &v
	}
	return out
}

func analystCapableLookup(d serveDeps) func(context.Context, contracts.Scope) bool {
	return func(ctx context.Context, scope contracts.Scope) bool {
		if d.External == nil {
			return false
		}
		type cataloguer interface {
			ControllerAnalystCapable(ctx context.Context, scope contracts.Scope) (bool, error)
		}
		if c, ok := d.External.(cataloguer); ok {
			ok, err := c.ControllerAnalystCapable(ctx, scope)
			return err == nil && ok
		}
		type profiler interface {
			ResolvedControllerProfile(ctx context.Context, scope contracts.Scope) (pv.ProviderProfile, bool)
		}
		if p, ok := d.External.(profiler); ok {
			prof, found := p.ResolvedControllerProfile(ctx, scope)
			if !found || prof.AnalystCapable == nil {
				return false
			}
			return *prof.AnalystCapable
		}
		return false
	}
}

// ---- settings Sources ----

type analystSources struct {
	apps    func() (gateway.ApplicationService, bool)
	capable func(context.Context, contracts.Scope) bool
}

func (s *analystSources) DeploymentConfig(context.Context, contracts.Scope) (an.Config, error) {
	return fileAnalystToConfig(dialogue.Loaded().Analyst), nil
}

func (s *analystSources) ApplicationAssistant(ctx context.Context, scope contracts.Scope) (*ap.ManifestAssistant, error) {
	apps, ok := s.apps()
	if !ok {
		return nil, nil
	}
	pkg, err := apps.Package(ctx, scope.ApplicationID)
	if err != nil {
		return nil, err
	}
	type holder interface{ Assistant() *ap.ManifestAssistant }
	if h, ok := pkg.(holder); ok {
		return h.Assistant(), nil
	}
	return nil, nil
}

func (s *analystSources) ControllerAnalystCapable(ctx context.Context, scope contracts.Scope) (bool, error) {
	if s.capable == nil {
		return false, nil
	}
	return s.capable(ctx, scope), nil
}

func (s *analystSources) WebConfigured(context.Context, contracts.Scope) (bool, error) {
	w := dialogue.Loaded().Analyst.Web
	return strings.TrimSpace(w.Provider) != "", nil
}

func fileAnalystToConfig(c dialogue.AnalystConfig) an.Config {
	en := c.Enabled
	out := an.Config{Enabled: &en, Abilities: &an.ConfigAbilityFlags{
		Loop: boolPtr(c.Abilities.Loop), Compute: boolPtr(c.Abilities.Compute),
		Forecasts: boolPtr(c.Abilities.Forecasts), Presentation: boolPtr(c.Abilities.Presentation),
		Drafts: boolPtr(c.Abilities.Drafts), Knowledge: boolPtr(c.Abilities.Knowledge),
		WebSearch: boolPtr(c.Abilities.WebSearch), WebFetch: boolPtr(c.Abilities.WebFetch),
	}}
	if c.MaxSteps > 0 {
		v := int64(c.MaxSteps)
		out.MaxSteps = &v
	}
	out.Web = mapWebConfig(c.Web)
	return out
}

func boolPtr(b bool) *bool { return &b }

// ---- loop adapters ----

type abilityAdapt struct{}

func (a *abilityAdapt) Resolve(ctx context.Context, scope contracts.Scope, capable bool) (loop.Effective, error) {
	if a == nil {
		return loop.Effective{}, nil
	}
	analystWire.mu.Lock()
	src, store := analystWire.sources, analystWire.store
	analystWire.mu.Unlock()
	if src == nil {
		return loop.Effective{}, nil
	}
	dep, err := src.DeploymentConfig(ctx, scope)
	if err != nil {
		return loop.Effective{}, err
	}
	app, err := src.ApplicationAssistant(ctx, scope)
	if err != nil {
		return loop.Effective{}, err
	}
	ten := an.TenantSettings{}
	if store != nil {
		ten, err = store.GetTenant(ctx, scope, scope.TenantID)
		if err != nil {
			return loop.Effective{}, err
		}
	}
	webOK, _ := src.WebConfigured(ctx, scope)
	domainOK := app != nil && app.Domain != nil && app.Domain.Description != "" && len(app.Domain.InScope) > 0
	got := settings.AbilityResolver{}.Resolve(settings.ResolveInput{
		Deployment: dep, Application: app, Tenant: ten,
		AnalystCapable: capable, WebConfigured: webOK, DomainDeclared: domainOK,
	})
	flags := loop.AbilityFlags{
		Loop: got.Abilities.Loop, Compute: got.Abilities.Compute, Forecasts: got.Abilities.Forecasts,
		Presentation: got.Abilities.Presentation, Drafts: got.Abilities.Drafts,
		Knowledge: got.Abilities.Knowledge, WebSearch: got.Abilities.WebSearch, WebFetch: got.Abilities.WebFetch,
	}
	loopOn := dep.Abilities != nil && dep.Abilities.Loop != nil && *dep.Abilities.Loop
	wanted := boolOr(dep.Enabled, false) && loopOn && !capable
	eff := loop.Effective{Enabled: got.Enabled, AnalystCapable: capable, Abilities: flags, Wanted: wanted}
	if wanted {
		eff.WantedReason = contracts.ErrModelNotAnalystCapable
	}
	return eff, nil
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

type computeAdapt struct{ eng *compute.Engine }

func (c computeAdapt) Run(_ context.Context, _ contracts.Scope, op an.ComputeOp, datasets map[an.DatasetID]an.Dataset) (loop.ComputeResult, error) {
	inputs := make([]*an.Dataset, 0, len(op.Inputs))
	for _, id := range op.Inputs {
		ds, ok := datasets[id]
		if !ok {
			return loop.ComputeResult{}, contracts.New(contracts.ErrValidationFailed, "compute: unknown input dataset")
		}
		cp := ds
		inputs = append(inputs, &cp)
	}
	res, err := c.eng.Run(op, inputs)
	if err != nil {
		return loop.ComputeResult{}, err
	}
	out := loop.ComputeResult{Facts: res.Facts}
	ds := res.Dataset
	out.Dataset = &ds
	return out, nil
}

type analystVerifier struct{}

func (analystVerifier) VerifyAnswer(_ context.Context, in loop.VerifyAnswerInput) (loop.VerifyAnswerResult, error) {
	facts := verify.AnalystFactSet{Derived: in.Facts.Facts}
	for _, ds := range in.Facts.Datasets {
		facts.Datasets = append(facts.Datasets, ds)
	}
	for _, c := range in.Facts.Citations {
		facts.Citations = append(facts.Citations, verify.AnalystCitation{Meta: c, FetchedText: in.Facts.Fetched[c.ID]})
	}
	lang := verify.LangEN
	if in.Language != "" {
		if l, err := verify.ParseLanguage(in.Language); err == nil {
			lang = l
		}
	}
	ans := verify.AnalystAnswer{Text: in.TextPlan, SourceSpans: in.SourceSpans, Blocks: in.Blocks}
	out, err := verify.VerifyAnalyst(ans, facts, verify.AnalystOptions{Language: lang})
	if err != nil {
		return loop.VerifyAnswerResult{}, err
	}
	res := loop.VerifyAnswerResult{
		Text: out.Text, Verified: out.Source == verify.SourceModel,
		Blocks: out.Blocks, CoreMaterialised: out.CoreMaterialised,
	}
	if out.Source != verify.SourceModel {
		res.Segments = []loop.VerifiedSegment{{Text: out.Text, Replace: true, Reason: string(out.Reason)}}
	}
	return res, nil
}

type domainAdapt struct {
	packages dialogue.PackageSource
	sel      dialogue.ProviderSelector
	guard    dialogue.PromptGuard
}

func (d *domainAdapt) Classify(ctx context.Context, scope contracts.Scope, utterance string) (an.DomainClassification, error) {
	pkg, err := d.packages.Package(ctx, scope.ApplicationID)
	if err != nil {
		return "", err
	}
	type holder interface{ Assistant() *ap.ManifestAssistant }
	var dom an.Domain
	if h, ok := pkg.(holder); ok && h.Assistant() != nil && h.Assistant().Domain != nil {
		dom = *h.Assistant().Domain
	}
	// Scope must be the turn's: empty Scope breaks External Select / egress Guard.
	call := analystDomainModel{sel: d.sel, guard: d.guard, scope: scope}
	res, err := domain.New(call).Classify(ctx, dom, utterance)
	if err != nil {
		return "", err
	}
	return res.Classification, nil
}

type analystDomainModel struct {
	sel   dialogue.ProviderSelector
	guard dialogue.PromptGuard
	scope contracts.Scope
}

func (m analystDomainModel) Complete(ctx context.Context, prompt string) ([]byte, error) {
	if m.sel == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "analyst: no provider for domain classification")
	}
	prov, err := m.sel.Select(ctx, m.scope, providers.RoleController)
	if err != nil {
		return nil, err
	}
	// Egress: system → Instructions; user ItemUserMessage → classified content (ItemFrame is unclassified).
	req := dialogue.PromptRequest{
		Role: providers.RoleController,
		Items: []dialogue.ContextItem{
			{
				Role: providers.MsgSystem, Trusted: true,
				Content: "Classify the user utterance against the application domain. Reply with ONLY JSON.",
				Meta:    dialogue.ItemMeta{Kind: dialogue.ItemFrame, Class: dialogue.ClassInternal},
			},
			{
				Role: providers.MsgUser, Trusted: true, Content: prompt,
				Meta: dialogue.ItemMeta{Kind: dialogue.ItemUserMessage, Class: dialogue.ClassInternal},
			},
		},
		Output: providers.SchemaConstraint(json.RawMessage(`{
			"type":"object","additionalProperties":false,
			"required":["classification","reason"],
			"properties":{
				"classification":{"type":"string","enum":["in_domain","out_of_domain"]},
				"reason":{"type":"string","maxLength":512}
			}
		}`)),
		MaxTokens: 128, Temperature: 0,
	}
	guarded := req
	if m.guard != nil {
		guarded, err = m.guard.Guard(ctx, dialogue.GuardInput{
			Scope: m.scope, Provider: dialogue.ProviderInfo{Name: prov.Name, Local: prov.Local}, Request: req,
		})
		if err != nil {
			return nil, err
		}
	}
	res, err := prov.Generator.Chat(ctx, m.scope, guarded.ChatRequest())
	if err != nil {
		return nil, err
	}
	if len(res.JSON) > 0 {
		return res.JSON, nil
	}
	return []byte(res.Text), nil
}

type stepCallerAdapt struct {
	sel    dialogue.ProviderSelector
	guard  dialogue.PromptGuard
	budget providers.Budget
}

func (s *stepCallerAdapt) Call(ctx context.Context, in loop.StepCallInput) (loop.StepCallResult, error) {
	prov, err := s.sel.Select(ctx, in.Scope, providers.RoleController)
	if err != nil {
		return loop.StepCallResult{}, err
	}
	items := make([]dialogue.ContextItem, 0, len(in.Items))
	for _, it := range in.Items {
		role := providers.MsgUser
		kind := dialogue.ItemUserMessage
		switch it.Role {
		case "system":
			role = providers.MsgSystem
			kind = dialogue.ItemFrame
		case "assistant":
			role = providers.MsgAssistant
			kind = dialogue.ItemTurn
		}
		switch it.SourceID {
		case "abilities", "datasets", "prior_steps":
			kind = dialogue.ItemState
		case "user_message":
			kind = dialogue.ItemUserMessage
		}
		// Cloud egress cannot list confidential on chain policies (source-only).
		// Mirror extmode UserMessage relaxation: confidential → internal for the wire.
		class := dialogue.ClassInternal
		switch it.DataClass {
		case an.DataClassPublic:
			class = dialogue.ClassPublic
		case an.DataClassRestricted:
			class = dialogue.ClassRestricted
		}
		items = append(items, dialogue.ContextItem{
			Role: role, Trusted: it.Trusted, Content: it.Content,
			Meta: dialogue.ItemMeta{Kind: kind, SourceID: it.SourceID, Class: class, LocalOnly: it.LocalOnly},
		})
	}
	maxOut := 512
	if s.budget.MaxOutputTokens > 0 && s.budget.MaxOutputTokens < maxOut {
		maxOut = s.budget.MaxOutputTokens
	}
	// Do not SchemaConstraint here: extmode treats JSON Schema as commit and attaches
	// capability tools, which breaks the analyst step grammar. Schema is in the prompt;
	// ParseStep validates the model JSON.
	if len(in.Schema) > 0 {
		items = append(items, dialogue.ContextItem{
			Role: providers.MsgSystem, Trusted: true,
			Content: "Reply with ONLY JSON matching this schema:\n" + string(in.Schema),
			Meta:    dialogue.ItemMeta{Kind: dialogue.ItemFrame, Class: dialogue.ClassInternal},
		})
	}
	req := dialogue.PromptRequest{
		Role: providers.RoleController, Items: items, MaxTokens: maxOut, Temperature: 0,
	}
	guarded := req
	if s.guard != nil {
		guarded, err = s.guard.Guard(ctx, dialogue.GuardInput{
			Scope: in.Scope, Provider: dialogue.ProviderInfo{Name: prov.Name, Local: prov.Local}, Request: req,
		})
		if err != nil {
			return loop.StepCallResult{}, err
		}
	}
	chat := guarded.ChatRequest()
	if s.budget.MaxPromptTokens > 0 {
		chat, err = s.budget.CheckChat(chat)
		if err != nil {
			return loop.StepCallResult{}, err
		}
	}
	res, err := prov.Generator.Chat(ctx, in.Scope, chat)
	if err != nil {
		return loop.StepCallResult{}, err
	}
	raw := res.JSON
	if len(raw) == 0 {
		raw = []byte(res.Text)
	}
	raw = unwrapSingletonJSONArray(raw)
	return loop.StepCallResult{Raw: raw, TokIn: res.Usage.InputTokens, TokOut: res.Usage.OutputTokens}, nil
}

// unwrapSingletonJSONArray peels a one-element JSON array some providers emit for
// structured text (ParseStep expects a single object).
func unwrapSingletonJSONArray(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' {
		return raw
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 1 {
		return raw
	}
	return bytes.TrimSpace(arr[0])
}

type idsAdapt struct{}

func (idsAdapt) New(prefix string) string {
	switch prefix {
	case "ds":
		// Analyst dataset ids are lowercase-only (schemas/analyst/shared).
		return strings.ToLower(ids.New("ds"))
	case "wcit":
		return ids.New("wcit")
	case "trc":
		return ids.New("trc")
	default:
		return ids.New("trc")
	}
}

// analystDynamicSecrets resolves web cred_ref through the credentials Resolver
// bound after external_providers starts (same service other cred_ref users get).
type analystDynamicSecrets struct{}

func (analystDynamicSecrets) Resolve(ctx context.Context, ref string) (string, error) {
	analystWire.mu.Lock()
	s := analystWire.secrets
	analystWire.mu.Unlock()
	if s == nil {
		return "", contracts.New(contracts.ErrUnavailable, "analyst: credentials resolver is not wired for web search")
	}
	return s.Resolve(ctx, ref)
}

// ---- web turn bag (keyed by turn id; dropped on EndTurn) ----

type webTurnBag struct {
	mu     sync.Mutex
	deploy web.HostPolicy
	budget web.Budget
	byTurn map[string]*web.Turn
}

func (b *webTurnBag) EndTurn(turnID string) {
	if b == nil || turnID == "" {
		return
	}
	b.mu.Lock()
	delete(b.byTurn, turnID)
	b.mu.Unlock()
}

// analystPackagePreferredHosts reads assistant.domain.preferred_web_hosts (empty when absent).
// Tests may replace it.
var analystPackagePreferredHosts = func(ctx context.Context, scope contracts.Scope) []string {
	analystWire.mu.Lock()
	src := analystWire.sources
	analystWire.mu.Unlock()
	if src == nil {
		return nil
	}
	asst, err := src.ApplicationAssistant(ctx, scope)
	if err != nil || asst == nil || asst.Domain == nil {
		return nil
	}
	return asst.Domain.PreferredWebHosts
}

func (b *webTurnBag) policyFor(ctx context.Context, scope contracts.Scope) web.HostPolicy {
	ten := web.HostPolicy{}
	analystWire.mu.Lock()
	store := analystWire.store
	analystWire.mu.Unlock()
	if store != nil {
		if ts, err := store.GetTenant(ctx, scope, scope.TenantID); err == nil && ts.Web != nil {
			ten = web.HostPolicy{Allow: ts.Web.AllowHosts, Deny: ts.Web.DenyHosts}
		}
	}
	pkgHosts := analystPackagePreferredHosts(ctx, scope)
	// Empty/absent package preferred_web_hosts does not narrow (most restrictive wins only when set).
	parts := []web.HostPolicy{b.deploy, ten}
	if len(pkgHosts) > 0 {
		parts = append(parts, web.HostPolicy{Allow: pkgHosts})
	}
	return web.MergeHostPolicy(parts...)
}

func (b *webTurnBag) turn(ctx context.Context, scope contracts.Scope, turnID string) *web.Turn {
	if turnID == "" {
		turnID = scope.SessionID + ":" + scope.PrincipalID // last resort; prefer real turn id
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.byTurn == nil {
		b.byTurn = map[string]*web.Turn{}
	}
	if t := b.byTurn[turnID]; t != nil {
		return t
	}
	t := web.NewTurn(b.policyFor(ctx, scope), b.budget)
	b.byTurn[turnID] = t
	return t
}

func (b *webTurnBag) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.byTurn)
}

type webSearcherChainAdapt struct {
	inner  *web.ChainSearcher
	turns  *webTurnBag
	allows func(context.Context, contracts.Scope) bool
}

func (a webSearcherChainAdapt) Search(ctx context.Context, scope contracts.Scope, in loop.SearchInput) (loop.SearchResult, error) {
	policyOK := false
	if a.allows != nil {
		policyOK = a.allows(ctx, scope)
	}
	t := a.turns.turn(ctx, scope, in.TurnID)
	res, err := a.inner.Search(ctx, scope, t, in.Query, in.Datasets, policyOK)
	if err != nil {
		return loop.SearchResult{}, err
	}
	out := loop.SearchResult{Hits: make([]loop.SearchHit, len(res.Hits))}
	for i, h := range res.Hits {
		out.Hits[i] = loop.SearchHit{URL: h.URL, Title: h.Title, Snippet: h.Snippet}
	}
	return out, nil
}

// webSearcherAdapt wraps a single Provider-backed Searcher (unit tests).
type webSearcherAdapt struct {
	inner  *web.Searcher
	turns  *webTurnBag
	allows func(context.Context, contracts.Scope) bool
}

func (a webSearcherAdapt) Search(ctx context.Context, scope contracts.Scope, in loop.SearchInput) (loop.SearchResult, error) {
	policyOK := false
	if a.allows != nil {
		policyOK = a.allows(ctx, scope)
	}
	t := a.turns.turn(ctx, scope, in.TurnID)
	res, err := a.inner.Search(ctx, t, in.Query, in.Datasets, policyOK)
	if err != nil {
		return loop.SearchResult{}, err
	}
	out := loop.SearchResult{Hits: make([]loop.SearchHit, len(res.Hits))}
	for i, h := range res.Hits {
		out.Hits[i] = loop.SearchHit{URL: h.URL, Title: h.Title, Snippet: h.Snippet}
	}
	return out, nil
}

type webFetcherAdapt struct {
	inner *web.Fetcher
	turns *webTurnBag
}

func (a webFetcherAdapt) Fetch(ctx context.Context, scope contracts.Scope, in loop.FetchInput) (loop.FetchResult, error) {
	t := a.turns.turn(ctx, scope, in.TurnID)
	res, err := a.inner.Fetch(ctx, t, in.URL)
	if err != nil {
		return loop.FetchResult{}, err
	}
	return loop.FetchResult{Citation: res.Citation, Text: res.Text}, nil
}

// ---- loop Budget → providerbudget / turn token ceiling ----

type loopBudgetAdapt struct {
	sel         dialogue.ProviderSelector
	prompt      providers.Budget
	maxTurnToks int
	mu          sync.Mutex
	usedToks    int
}

func (b *loopBudgetAdapt) Reserve(ctx context.Context, scope contracts.Scope, estToks int) (loop.BudgetReservation, error) {
	if b == nil {
		return loop.BudgetReservation{ID: "rsv_noop"}, nil
	}
	b.mu.Lock()
	if b.maxTurnToks > 0 && b.usedToks+estToks > b.maxTurnToks {
		b.mu.Unlock()
		return loop.BudgetReservation{}, contracts.New(contracts.ErrResourceExhausted, "analyst: turn token budget exhausted")
	}
	b.usedToks += estToks
	b.mu.Unlock()

	if analystExternalReserve != nil {
		return analystExternalReserve(ctx, scope, estToks)
	}
	return loop.BudgetReservation{ID: ids.New("rsv")}, nil
}

func (b *loopBudgetAdapt) Settle(ctx context.Context, scope contracts.Scope, r loop.BudgetReservation, a loop.BudgetActual) error {
	if b == nil || analystExternalSettle == nil {
		return nil
	}
	return analystExternalSettle(ctx, scope, r, a)
}

func analystMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}
