//go:build !noexternal

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/readiness/assist"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The external model hook of `alixai readiness` (master plan 9.5, ADR 0037; additive plan, milestone X).
//
// readiness_assist.go leaves rdyCLIExternal nil and imports no X package, so that `go build -tags noexternal ./cmd/alixai` holds none.
// This file is built only without that tag, and fills the hook. Nothing here runs unless the operator turns it on:
//
//   - external mode must be enabled in the configuration (external_providers.enabled). With it off the hook answers "no external
//     model" before it builds anything: no store is opened, no directory is created, no connection is made, and the run is exactly the
//     Local one.
//   - an external model is used only when the operator names it with -model and it is not one of inference.models. With no -model, the
//     local controller runs, as ever. There is no automatic choice of a hosted model, and none is ever tried after a refusal.
//   - the operator is named in ALIXAI_READINESS_OPERATOR and "<application>/<operator>" (the application of the run, -app) must be
//     listed in external_providers.deployment_operators, the rule of every `alixai providers` command (X-13). A name that is missing or not listed ends the leg before the store is opened. The same
//     check is made again, through the operator service of the deployment, before the guard is asked and before the send.
//   - the project is classed local_only (nothing may leave) unless the operator sets another class in
//     ALIXAI_READINESS_PROJECT_CLASS (public, internal, confidential or restricted). The engine refuses an external model while the
//     class is local_only, restricted content never leaves, and the data-egress guard decides on every request what part of it may.
//   - the call runs through the Router of internal/modes, the one every external call of the product goes through: the deployment mode,
//     the registry gates (qualified, fresh terms, data class, credential), the guard, a budget Reserve with the estimate floor before
//     anything leaves, the kill switch read again between the Reserve and the send (the reservation is then released), the send, the
//     reply model check against the pinned model, and a Settle. A call with no applicable budget on the interactive channel is refused
//     (no_budget). The Router is given no local runtime and a registry that never names a fallback profile, so nothing else is tried.
//   - one profile is pinned for the whole run (the first qualified profile of the model) and the Router is told that profile only.
//   - every refusal made before anything is sent is permission_denied. A refusal, an invalid or stale credential, a model that is not
//     qualified or a data class its profile may not receive ends the leg; the report says "refused" and the deterministic scan stands
//     alone.

// The environment variables of the external leg.
const (
	// rdyExtClassEnv names the data class the operator gives the project's content.
	rdyExtClassEnv = "ALIXAI_READINESS_PROJECT_CLASS"
	// rdyExtOperatorEnv names the operator who runs the leg ("<application>/<operator>" is in external_providers.deployment_operators).
	rdyExtOperatorEnv = "ALIXAI_READINESS_OPERATOR"
	// rdyExtTenantEnv names the tenant whose credential and budgets the leg uses; the default is the one of `alixai providers`.
	rdyExtTenantEnv = "ALIXAI_READINESS_TENANT"
	// rdyExtDefaultTenant is the tenant `alixai providers -tenant` defaults to.
	rdyExtDefaultTenant = "operator-cli"
)

func init() { rdyCLIExternal = rdyExtHook }

// rdyExtStack is what the hook needs from the X services. The live implementation is rdyExtLive; tests use fakes.
type rdyExtStack interface {
	// Deployment is the id of the deployment the guard and the registry decide for.
	Deployment() string
	// Authorize refuses unless the principal of the scope is a deployment operator of this deployment (X-13).
	Authorize(ctx context.Context, scope contracts.Scope) error
	// Resolve returns the qualified profile of the model in the application of the scope.
	Resolve(ctx context.Context, scope contracts.Scope, modelID string) (pv.ProviderProfile, error)
	// Admit runs the registry gates (qualified, fresh, data class, credential) for a request of this class, before the guard is asked.
	Admit(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile, class string) error
	// Guard is the data-egress guard.
	Guard() dataegress.Guard
	// Send routes one released prompt: every gate again, the budget, the kill switch, the send and the settlement. cand is the candidate
	// the guard released the prompt from.
	Send(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile, class string, cand dataegress.Candidate, req providers.ChatRequest) (providers.ChatResult, error)
}

// rdyExtOpen builds the stack. Tests replace it. It is called only when external mode is enabled and an external model was named.
var rdyExtOpen = rdyExtOpenLive

// rdyExtHook is the value of rdyCLIExternal.
func rdyExtHook(ctx context.Context, env *Env, app, modelID string) (*rdyCLIExternalPick, error) {
	if !externalSection.Enabled {
		return nil, nil
	}
	if modelID == "" || slices.Contains(inferenceSection.Models, modelID) {
		return nil, nil // the local controller, or a local model the operator named
	}
	class, err := rdyExtProjectClass(env)
	if err != nil {
		return nil, err
	}
	operator, tenant, err := rdyExtPrincipal(env, rdyCLIScope(app).ApplicationID)
	if err != nil {
		return nil, err
	}
	st, err := rdyExtOpen(ctx, env)
	if err != nil {
		// External mode was asked for and could not be set up: an error, never a quiet fall back to the local model.
		return nil, fmt.Errorf("readiness: the external model %q cannot be used: %w", modelID, err)
	}
	leg := newRdyExtLeg(st, modelID, class, operator, tenant)
	gen, err := providers.WithBudget(rdyExtText{leg: leg}, rdyCLIBudget)
	if err != nil {
		return nil, err
	}
	return &rdyCLIExternalPick{
		Model:        &assist.AssistModel{Name: "external", ModelID: modelID, Local: false, Gen: gen},
		Gate:         rdyExtGate{leg: leg},
		ProjectClass: class,
	}, nil
}

// rdyExtProjectClass reads the class the operator gave the project: local_only unless said otherwise.
func rdyExtProjectClass(env *Env) (string, error) {
	v, _ := env.LookupEnv(rdyExtClassEnv)
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "":
		return assist.AssistClassLocalOnly, nil
	case assist.AssistClassLocalOnly, "public", "internal", "confidential", "restricted":
		return v, nil
	}
	return "", Usagef("%s=%q: want local_only, public, internal, confidential or restricted", rdyExtClassEnv, v)
}

// rdyExtPrincipal reads the operator who runs the leg and the tenant it works in, and applies the allow-list of the configuration to the
// operator of the run's application before anything is opened: a name that is missing, or whose "<application>/<operator>" is not in
// external_providers.deployment_operators, is permission_denied. The operator service checks the same entry again on every request.
func rdyExtPrincipal(env *Env, application string) (operator, tenant string, err error) {
	operator, _ = env.LookupEnv(rdyExtOperatorEnv)
	operator = strings.TrimSpace(operator)
	tenant, _ = env.LookupEnv(rdyExtTenantEnv)
	if tenant = strings.TrimSpace(tenant); tenant == "" {
		tenant = rdyExtDefaultTenant
	}
	probe := contracts.Scope{
		DeploymentID: "dep", ApplicationID: application, TenantID: tenant, WorkspaceID: "wsp", PrincipalID: operator, SessionID: "ses",
		Roles: []string{"operator"}, CapabilityScopes: []string{"readiness.scan"},
	}
	switch {
	case operator == "":
		return "", "", rdyExtDeny(nil, "name the operator in "+rdyExtOperatorEnv+" (listed as <application>/<operator> in external_providers.deployment_operators)")
	case probe.Validate() != nil:
		return "", "", Usagef("%s or %s is not an identifier", rdyExtOperatorEnv, rdyExtTenantEnv)
	case !externalSection.isDeploymentOperator(application, operator):
		return "", "", rdyExtDeny(nil, "the operator of this application is not listed in external_providers.deployment_operators")
	}
	return operator, tenant, nil
}

// rdyExtDeny makes any failure of the gate a permission_denied, whatever it was.
func rdyExtDeny(err error, msg string) error {
	if err == nil {
		return contracts.New(contracts.ErrPermissionDenied, "readiness external: "+msg)
	}
	return contracts.Wrap(contracts.ErrPermissionDenied, err, "readiness external: "+msg)
}

// ---- one run of the leg ----

// rdyExtLeg is what the gate and the generator of one run share: the stack, who runs the leg and in which tenant, the profile pinned
// for the run, and the candidates the guard released prompts from. The generator sends nothing the gate did not release.
type rdyExtLeg struct {
	stack    rdyExtStack
	modelID  string
	class    string
	operator string
	tenant   string

	mu      sync.Mutex
	profile *pv.ProviderProfile
	staged  map[string]dataegress.Candidate
}

func newRdyExtLeg(st rdyExtStack, modelID, class, operator, tenant string) *rdyExtLeg {
	return &rdyExtLeg{stack: st, modelID: modelID, class: class, operator: operator, tenant: tenant, staged: map[string]dataegress.Candidate{}}
}

// scope is the scope the guard, the registry and the budget decide for: the deployment's own id (so its kill switch, policies and
// profiles apply), the operator as principal (the budget matches a reservation to the principal that made it) and the tenant of the leg.
func (l *rdyExtLeg) scope(s contracts.Scope) contracts.Scope {
	if d := l.stack.Deployment(); d != "" {
		s.DeploymentID = d
	}
	s.PrincipalID, s.TenantID = l.operator, l.tenant
	return s
}

// authorize is the operator rule of X-13, applied on every request of the leg.
func (l *rdyExtLeg) authorize(ctx context.Context, scope contracts.Scope) error {
	if err := l.stack.Authorize(ctx, scope); err != nil {
		return rdyExtDeny(err, "the operator may not use an external provider")
	}
	return nil
}

// pin returns the profile of the run, resolving it once. Later requests use the same profile or are refused: a profile that stopped being
// qualified is never replaced by another one.
func (l *rdyExtLeg) pin(ctx context.Context, scope contracts.Scope) (pv.ProviderProfile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.profile != nil {
		return *l.profile, nil
	}
	p, err := l.stack.Resolve(ctx, scope, l.modelID)
	if err != nil {
		return pv.ProviderProfile{}, rdyExtDeny(err, "no provider profile can serve this model")
	}
	l.profile = &p
	return p, nil
}

func (l *rdyExtLeg) pinned() (pv.ProviderProfile, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.profile == nil {
		return pv.ProviderProfile{}, false
	}
	return *l.profile, true
}

// rdyExtKey identifies a prompt by the text of its messages, in order.
func rdyExtKey(contents []string) string {
	h := sha256.New()
	for _, c := range contents {
		h.Write([]byte(strconv.Itoa(len(c)) + ":"))
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (l *rdyExtLeg) stage(contents []string, c dataegress.Candidate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.staged[rdyExtKey(contents)] = c
}

// take returns the candidate a prompt was released from, once.
func (l *rdyExtLeg) take(contents []string) (dataegress.Candidate, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := rdyExtKey(contents)
	c, ok := l.staged[k]
	delete(l.staged, k)
	return c, ok
}

// ---- the gate: dataegress.Guard to assist.AssistEgressGate ----

// rdyExtGate adapts the data-egress guard to the gate of the assist engine. The guard decides; the adapter only maps items to parts
// and parts back to items, and takes the text it returns from the guard's envelope, never from the items it was given. Before the guard
// is asked it applies the operator rule and the registry gates, so a refused operator or a stale profile costs no guard decision.
type rdyExtGate struct{ leg *rdyExtLeg }

var _ assist.AssistEgressGate = rdyExtGate{}

// rdyExtMap says which candidate message came from which item.
type rdyExtMap struct {
	msgItem []int // candidate message index -> item index
}

// rdyExtDataClass maps the class of an item to a class the guard knows. local_only and anything unknown are restricted, the most
// restrictive, and the part is also marked local-only so the guard removes it.
func rdyExtDataClass(c string) (class string, localOnly bool) {
	switch c {
	case "public", "internal", "confidential", "restricted":
		return c, false
	}
	return string(pv.DataClassificationRestricted), true
}

func rdyExtCandidate(scope contracts.Scope, p pv.ProviderProfile, req assist.AssistEgressRequest) (dataegress.Candidate, rdyExtMap, error) {
	c := dataegress.Candidate{
		Scope: scope, Profile: p, Role: pv.RoleConversationalController, Channel: pv.ChannelInteractive, TraceID: "trc_readiness",
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  int64(rdyCLIBudget.MaxOutputTokens), TimeoutMS: max(rdyCLIBudget.Timeout.Milliseconds(), 1000),
	}
	var m rdyExtMap
	var instr []string
	for i, it := range req.Items {
		if it.Role == providers.MsgSystem && it.Kind == "frame" && it.Trusted {
			instr = append(instr, it.Content) // text AlixAI wrote from a fixed template
			continue
		}
		part := dataegress.Part{Kind: dataegress.PartText, Text: it.Content}
		switch it.Kind {
		case "frame":
			// Text AlixAI wrote (the requirement list): a directive, public. A text part can only carry a text category, and a
			// project excerpt is content the operator chose to show the model, so it travels as a user message with the
			// project's class.
			part.Category, part.Class = string(pv.SendCategoryDialogueDirective), string(pv.DataClassificationPublic)
		case "excerpt":
			part.Category = string(pv.SendCategoryUserMessage)
			part.Class, part.LocalOnly = rdyExtDataClass(it.Class)
			part.LocalOnly = part.LocalOnly || it.LocalOnly
		default:
			return c, m, rdyExtDeny(nil, "an item of an unknown kind was refused")
		}
		role := pv.RequestEnvelopeMessageRoleUser
		if it.Role == providers.MsgAssistant {
			role = pv.RequestEnvelopeMessageRoleAssistant
		}
		c.Messages = append(c.Messages, dataegress.Message{Role: role, Parts: []dataegress.Part{part}})
		m.msgItem = append(m.msgItem, i)
	}
	c.Instructions = strings.Join(instr, "\n\n")
	return c, m, nil
}

// rdyExtEnvText reads the text of one part of the guard envelope.
func rdyExtEnvText(env *pv.RequestEnvelope, mi, pi int) (string, bool) {
	if env == nil || mi < 0 || mi >= len(env.Payload.Messages) {
		return "", false
	}
	parts := env.Payload.Messages[mi].Parts
	if pi < 0 || pi >= len(parts) {
		return "", false
	}
	switch v := parts[pi].Value.(type) {
	case pv.RequestEnvelopeTextPart:
		return v.Text, true
	case *pv.RequestEnvelopeTextPart:
		if v != nil {
			return v.Text, true
		}
	}
	return "", false
}

// Release implements assist.AssistEgressGate.
func (g rdyExtGate) Release(ctx context.Context, req assist.AssistEgressRequest) ([]assist.AssistEgressItem, error) {
	leg := g.leg
	if leg == nil || leg.stack == nil || leg.stack.Guard() == nil {
		return nil, rdyExtDeny(nil, "the data-egress guard is not available")
	}
	scope := leg.scope(req.Scope)
	if err := leg.authorize(ctx, scope); err != nil {
		return nil, err
	}
	p, err := leg.pin(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err := leg.stack.Admit(ctx, scope, p, leg.class); err != nil {
		return nil, rdyExtDeny(err, "the provider profile cannot receive this project")
	}
	cand, m, err := rdyExtCandidate(scope, p, req)
	if err != nil {
		return nil, err
	}
	res, err := leg.stack.Guard().Evaluate(ctx, cand)
	if err != nil {
		return nil, rdyExtDeny(err, "the data-egress guard refused the request")
	}
	if res.Envelope == nil {
		return nil, rdyExtDeny(nil, "the data-egress guard released nothing")
	}
	env := res.Envelope

	var out []assist.AssistEgressItem
	released := cand
	released.Messages = nil
	if ins := env.Payload.Instructions; strings.TrimSpace(ins) == "" && cand.Instructions != "" {
		return nil, rdyExtDeny(nil, "the data-egress guard withheld the instructions")
	} else if strings.TrimSpace(ins) != "" {
		out = append(out, assist.AssistEgressItem{Role: providers.MsgSystem, Trusted: true, Content: ins, Kind: "frame", Class: string(pv.DataClassificationPublic)})
		released.Instructions = ins
	}
	kept := map[int]bool{}
	for _, po := range res.Parts {
		if !po.Kept {
			continue
		}
		if po.Message < 0 || po.Message >= len(m.msgItem) || po.Part < 0 || po.Part >= len(cand.Messages[po.Message].Parts) {
			return nil, rdyExtDeny(nil, "the data-egress guard answered for a message that was not sent")
		}
		text, ok := rdyExtEnvText(env, po.EnvelopeMessage, po.EnvelopePart)
		if !ok {
			return nil, rdyExtDeny(nil, "the data-egress envelope cannot be read")
		}
		it := req.Items[m.msgItem[po.Message]]
		it.Content, it.LocalOnly = text, false
		out = append(out, it)
		kept[m.msgItem[po.Message]] = true
		// The candidate of the send is what was released: the scrubbed text, with the class and category it was offered under.
		orig := cand.Messages[po.Message]
		part := orig.Parts[po.Part]
		part.Text = text
		released.Messages = append(released.Messages, dataegress.Message{Role: orig.Role, Parts: []dataegress.Part{part}})
	}
	// A prompt with one of its own frames missing is not the prompt the engine built: refuse it whole.
	for i, it := range req.Items {
		if it.Kind == "frame" && it.Role != providers.MsgSystem && !kept[i] {
			return nil, rdyExtDeny(nil, "the data-egress guard withheld part of the instructions")
		}
	}
	if len(out) == 0 {
		return nil, rdyExtDeny(nil, "the data-egress guard released nothing")
	}
	contents := make([]string, len(out))
	for i, it := range out {
		contents[i] = it.Content
	}
	leg.stage(contents, released)
	return out, nil
}

// ---- the generator ----

// rdyExtText is the text generator of the external model. It sends only a prompt the gate released: it finds the candidate the guard
// released the prompt from and hands it, with the pinned profile, to the stack, which runs every gate again around the send. Any failure
// is returned as it is; no other provider is tried.
type rdyExtText struct{ leg *rdyExtLeg }

func (g rdyExtText) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	leg := g.leg
	if leg == nil || leg.stack == nil {
		return providers.ChatResult{}, rdyExtDeny(nil, "no external stack")
	}
	scope = leg.scope(scope)
	if err := leg.authorize(ctx, scope); err != nil {
		return providers.ChatResult{}, err
	}
	p, ok := leg.pinned()
	if !ok {
		return providers.ChatResult{}, rdyExtDeny(nil, "no provider profile was pinned: the prompt was not released by the data-egress guard")
	}
	contents := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		contents[i] = m.Content
	}
	cand, ok := leg.take(contents)
	if !ok {
		return providers.ChatResult{}, rdyExtDeny(nil, "the prompt was not released by the data-egress guard")
	}
	return leg.stack.Send(ctx, scope, p, leg.class, cand, req)
}

func (rdyExtText) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "readiness external: the leg does not stream")
}

// ---- the router around the send ----

// rdyExtNoLocal is the local runtime the Router is given: none. A request the Router cannot serve externally is never served locally.
type rdyExtNoLocal struct{}

func (rdyExtNoLocal) Usable(modes.Role) bool { return false }

// rdyExtProfiles is the profile registry the Router reads. It never names another profile: the registry's own fallback to a profile it
// names is not used by the leg, which is pinned to one.
type rdyExtProfiles struct{ *providerreg.Registry }

func (rdyExtProfiles) Fallback(context.Context, contracts.Scope, pv.ProfileID, providerreg.FallbackCondition, providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrPermissionDenied, "readiness external: the leg never falls back to another profile").
		WithDetails("reason", string(providerreg.ReasonFallbackNotNamed))
}

// rdyExtEstimate prices the worst case of the prompt for the profile of the attempt: the estimated input, the output cap and one
// request, with the estimate floor the budget never reserves below.
func rdyExtEstimate(req providers.ChatRequest) modes.Estimator {
	return func(p pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error) {
		units := providerbudget.Units{
			ToksIn: pv.Counter(providers.Budget{}.EstimateChat(req)), ToksOut: pv.Counter(max(req.MaxTokens, 1)), Requests: 1,
		}
		floor, err := providerbudget.Estimate(p.Pricing, units)
		return units, floor, err
	}
}

// rdyExtRich builds the request for the provider from the filtered envelope and only from it; the closed output schema, the cap and the
// temperature are the engine's own and come from its request.
func rdyExtRich(env *pv.RequestEnvelope, req providers.ChatRequest) (providers.RichRequest, error) {
	if env == nil {
		return providers.RichRequest{}, rdyExtDeny(nil, "no envelope")
	}
	out := providers.RichRequest{Role: providers.RoleController, Output: req.Output, MaxToks: req.MaxTokens, Temperature: req.Temperature, Stop: req.Stop}
	if out.MaxToks <= 0 {
		out.MaxToks = int(env.Payload.Limits.MaxOutputToks)
	}
	if ins := strings.TrimSpace(env.Payload.Instructions); ins != "" {
		out.Messages = append(out.Messages, providers.RichMessage{Role: providers.MsgSystem, Trusted: true, Parts: []providers.ContentPart{providers.TextPart(env.Payload.Instructions)}})
	}
	for mi, m := range env.Payload.Messages {
		role := providers.MsgUser
		if m.Role == pv.RequestEnvelopeMessageRoleAssistant {
			role = providers.MsgAssistant
		}
		rm := providers.RichMessage{Role: role}
		for pi := range m.Parts {
			text, ok := rdyExtEnvText(env, mi, pi)
			if !ok {
				return providers.RichRequest{}, rdyExtDeny(nil, "the envelope holds a part that is not text")
			}
			rm.Parts = append(rm.Parts, providers.TextPart(text))
		}
		out.Messages = append(out.Messages, rm)
	}
	return out, nil
}

func rdyExtMetering(u providers.CostUsage) providerbudget.Units {
	return providerbudget.Units{ToksIn: pv.Counter(u.ToksIn), ToksOut: pv.Counter(u.ToksOut), Requests: 1}
}

// rdyExtRouteErr turns a request the Router did not serve into the error of the leg. A request refused before anything was sent
// (egress, budget, credential, stale profile, mode, kill switch) is permission_denied with the Router's reason, so the report says
// "refused". One that failed after the send keeps the typed error of the Router: unavailable, timeout, uncertain_outcome, malformed.
func rdyExtRouteErr(o modes.Outcome, err error) error {
	if o.DataSent {
		return err
	}
	e := contracts.Wrap(contracts.ErrPermissionDenied, err, "readiness external: the call was refused and nothing was sent")
	if o.Reason != "" {
		e = e.WithDetails("reason", o.Reason)
	}
	if o.Kind != "" {
		e = e.WithDetails("outcome", string(o.Kind))
	}
	return e.WithDetails("alternate_provider", "none")
}

// ---- the live stack ----

// rdyExtLive is the stack over the X services of the deployment, built the way `alixai providers` builds them. The store it opens is
// closed when the process ends.
type rdyExtLive struct {
	admin  *externalAdmin
	sender *extWireSender
	// budgets replaces the budget service the Router uses (tests wrap the real one); nil is the service of the deployment.
	budgets modes.Budgets
}

func rdyExtOpenLive(ctx context.Context, env *Env) (rdyExtStack, error) {
	deployment := identity.CurrentConfig().DeploymentID
	if deployment == "" {
		return nil, errors.New("identity.deployment_id is not configured")
	}
	core := env.Config.Core()
	opts := store.FromConfig()
	opts.Path = filepath.Join(core.DataDir.String(), store.DefaultFileName)
	opts.Logger = slog.New(slog.DiscardHandler)
	db, err := store.Open(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("open the store: %w", err)
	}
	clk, log := clock.Clock(clock.Real{}), slog.New(slog.DiscardHandler)
	fail := func(err error) (rdyExtStack, error) {
		_ = db.Close()
		return nil, err
	}
	alg, err := audit.New(db, audit.Options{Clock: clk, Registry: audit.DefaultRegistry, Logger: log})
	if err != nil {
		return fail(err)
	}
	admin, err := newExternalAdmin(externalAdminDeps{
		DB: db, Audit: alg, Clock: clk, Log: log, DeploymentID: deployment, Dir: filepath.Join(core.DataDir.String(), "external"), Section: externalSection,
	})
	if err != nil {
		return fail(err)
	}
	sender := &extWireSender{admin: admin, sec: externalSection, reg: extFinalProviderRegistry(), log: log, cache: map[string]ExternalProvider{}}
	return &rdyExtLive{admin: admin, sender: sender}, nil
}

func (l *rdyExtLive) Deployment() string { return l.admin.deployment }

func (l *rdyExtLive) Guard() dataegress.Guard { return l.admin.egress }

// Authorize is the operator rule of X-13: the operator service of the deployment, at the deployment level (the leg sends project content
// out of the deployment and draws on its provider budgets).
func (l *rdyExtLive) Authorize(ctx context.Context, scope contracts.Scope) error {
	return l.admin.AuthorizeOperator(ctx, scope, gateway.ProvidersOperatorDeployment)
}

func (l *rdyExtLive) Resolve(ctx context.Context, scope contracts.Scope, modelID string) (pv.ProviderProfile, error) {
	// The deployment mode decides first: with Local mode (the default) or the kill switch nothing leaves, whatever the profiles say.
	mode, err := l.admin.resolver.Resolve(ctx, scope)
	if err != nil {
		return pv.ProviderProfile{}, err
	}
	if mode.KillSwitch {
		return pv.ProviderProfile{}, contracts.New(contracts.ErrPermissionDenied, "the external kill switch is engaged: no external model is used").WithDetails("reason", modes.DecidedByKillSwitch)
	}
	if mode.Mode == modes.ModeLocal {
		return pv.ProviderProfile{}, contracts.New(contracts.ErrPermissionDenied, "the deployment mode for this application is local: no external model is used")
	}
	all, err := l.admin.reg.List(ctx, scope)
	if err != nil {
		return pv.ProviderProfile{}, err
	}
	var match []pv.ProviderProfile
	for _, p := range all {
		if string(p.ModelID) == modelID && p.Lifecycle == pv.ProfileLifecycleQualified {
			match = append(match, p)
		}
	}
	if len(match) == 0 {
		return pv.ProviderProfile{}, contracts.New(contracts.ErrPermissionDenied, "no qualified provider profile serves this model in this application")
	}
	sort.Slice(match, func(i, j int) bool { return match[i].ProfileID < match[j].ProfileID })
	return match[0], nil
}

func (l *rdyExtLive) Admit(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile, class string) error {
	dc, _ := rdyExtDataClass(class)
	_, err := l.admin.reg.Check(ctx, scope, p.ProfileID, providerreg.Requirement{
		Role: pv.RoleConversationalController, DataClass: pv.DataClassification(dc),
	})
	return err
}

// router builds the Router of one send, over the services of the deployment, for the one profile of the run.
func (l *rdyExtLive) router(id pv.ProfileID) (*modes.Router, error) {
	var budgets modes.Budgets = l.admin.budget
	if l.budgets != nil {
		budgets = l.budgets
	}
	return modes.New(modes.Config{
		Modes: l.admin.modes, KillSwitch: l.admin.egress,
		Policy: modes.StaticPolicy{modes.RoleController: {Profiles: []pv.ProfileID{id}}}, // no local fallback is named
		Local:  rdyExtNoLocal{}, Profiles: rdyExtProfiles{l.admin.reg}, Guard: l.admin.egress, Budgets: budgets, Clock: l.admin.clk,
	})
}

func (l *rdyExtLive) Send(ctx context.Context, scope contracts.Scope, p pv.ProviderProfile, class string, cand dataegress.Candidate, req providers.ChatRequest) (providers.ChatResult, error) {
	rt, err := l.router(p.ProfileID)
	if err != nil {
		return providers.ChatResult{}, err
	}
	size := 0
	for _, m := range req.Messages {
		size += len(m.Content)
	}
	dc, _ := rdyExtDataClass(class)
	var reply providers.ChatResult
	o, err := rt.Execute(ctx, modes.Request{
		Scope: scope, Role: modes.RoleController, Channel: pv.ChannelInteractive,
		Requirement: providerreg.Requirement{
			DataClass: pv.DataClassification(dc), ContextToks: int64(providers.Budget{}.EstimateChat(req)),
			OutputToks: int64(max(req.MaxTokens, 1)), RequestBytes: int64(size),
		},
		Candidate: cand, Estimate: rdyExtEstimate(req),
	}, modes.Executors{
		// No Local executor: nothing is served locally when the external route fails.
		External: func(ctx context.Context, call modes.ExternalCall) (modes.ExternalResult, error) {
			rich, err := rdyExtRich(call.Envelope, req)
			if err != nil {
				return modes.ExternalResult{}, &modes.CallError{Kind: pv.ProviderErrorKindBadRequest, Sent: false, Err: err}
			}
			gen, err := l.sender.generatorFor(call.Admission.Profile)
			if err != nil {
				ce := modes.CallErrorOf(err)
				ce.Sent = false // no adapter or no credential: nothing left
				return modes.ExternalResult{}, ce
			}
			res, err := gen.Generate(ctx, scope, rich)
			if err != nil {
				return modes.ExternalResult{}, modes.CallErrorOf(err)
			}
			metering := rdyExtMetering(res.Usage)
			bad := func() error {
				return &modes.CallError{Kind: pv.ProviderErrorKindMalformedResponse, Sent: true, Metering: &metering}
			}
			// A reply that does not say which model wrote it cannot be compared with the pinned model: refused like a malformed one. A
			// reply that names another model is refused by the Router, which also quarantines the profile.
			if res.Model == "" {
				return modes.ExternalResult{}, bad()
			}
			checked, err := rdyCLIBudget.CheckRichResult(rich, res)
			if err != nil || res.FinishReason == providers.FinishFiltered || strings.TrimSpace(checked.Text()) == "" {
				return modes.ExternalResult{}, bad()
			}
			reply = providers.ChatResult{
				Text: checked.Text(), FinishReason: checked.FinishReason, TraceID: checked.TraceID, Model: checked.Model, Timings: checked.Timings,
				Usage: providers.Usage{InputTokens: checked.Usage.ToksIn, OutputTokens: checked.Usage.ToksOut},
			}
			ur := modes.ExternalResult{Metering: metering, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported, ReplyModel: res.Model}
			if res.Usage.Estimated {
				ur.MeteringBasis = pv.UsageRecordMeteringBasisEstimated
			} else if res.Usage.CostMicros > 0 {
				m := pv.MicroUnits(res.Usage.CostMicros)
				ur.ReportedMicroUnits = &m
			}
			return ur, nil
		},
	})
	if err != nil {
		return providers.ChatResult{}, rdyExtRouteErr(o, err)
	}
	return reply, nil
}
