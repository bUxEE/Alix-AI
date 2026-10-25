package cli

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/readiness/assist"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The model leg of `alixai readiness` (master plan 9.5, ADR 0037). This file is the composition root of the leg and
// nothing else: it picks the model, wraps it in the request budget and hands it to internal/readiness/assist, which
// does everything that matters (excerpts, closed schema, proposal checks). The model only proposes.
//
//   - Local first. The model is the configured LocalAI controller (localai and inference sections), the way
//     `alixai serve` builds it. Nothing is configured: no model, and the report says so (not_configured). Something is
//     configured and cannot be built or reached: the leg is "unavailable" and the cause is printed; it is never
//     silently turned into "not configured", and no other model is tried.
//   - The label. A model is "unqualified, advisory" unless the model registry shows a qualified status for it. The Lite
//     controller candidates are not qualified (M0-17, M0-21, M5-09).
//   - External is a hook. rdyCLIExternal is nil. Only a file built without the noexternal tag (providers_external.go
//     or a sibling) may set it, because only that build may import the X packages (internal/dataegress and the rest).
//     This file imports none of them, so `go build -tags noexternal ./cmd/alixai` holds no X package. A hook that is
//     set must return a gate for any model that is not local; the engine refuses one without it. The hook
//     (readiness_assist_external.go) also applies the operator allow-list of `alixai providers`, reserves and settles
//     the provider budget around every send and reads the kill switch again before it (M9-03).

// rdyCLIExternalPick is what the external hook returns when external mode is enabled and the egress guard and router
// allow a provider for this use. A hook that returns nil with no error means "external mode is off".
type rdyCLIExternalPick struct {
	Model *assist.AssistModel
	// Gate is the data-egress guard adapter. An external model needs it.
	Gate assist.AssistEgressGate
	// ProjectClass is the data class the operator gave the project: local_only unless said otherwise.
	ProjectClass string
}

// rdyCLIExternal is nil by default. See the note above.
//
// app is the -app of the run ("" for the default of rdyCLIScope): the operator allow-list is checked for the application
// the run's scope carries.
var rdyCLIExternal func(ctx context.Context, env *Env, app, modelID string) (*rdyCLIExternalPick, error)

// rdyCLIModelSource builds the model for one run. Tests replace it. A nil model with a nil error means no model is
// configured.
var rdyCLIModelSource = rdyCLIBuildModel

// rdyCLIBudget bounds one model call of the leg. Its caps are above the engine's own defaults (6000 prompt and 1200
// output tokens): a smaller budget would make every call fail validation.
var rdyCLIBudget = providers.Budget{
	MaxPromptTokens: 8192,
	MaxOutputTokens: 2048,
	MaxBatchItems:   16,
	Timeout:         2 * time.Minute,
}

// rdyCLIAssistSetup is the model, gate and class a run passes to the assist engine.
type rdyCLIAssistSetup struct {
	Config assist.AssistConfig
}

// rdyCLIBuildModel is the default model source: the external hook first (it answers only when external mode is on),
// then the local controller.
func rdyCLIBuildModel(ctx context.Context, env *Env, app, modelID string) (rdyCLIAssistSetup, error) {
	if rdyCLIExternal != nil {
		pick, err := rdyCLIExternal(ctx, env, app, modelID)
		if err != nil {
			return rdyCLIAssistSetup{}, err
		}
		if pick != nil && pick.Model != nil {
			return rdyCLIAssistSetup{Config: assist.AssistConfig{
				Model: pick.Model, Gate: pick.Gate, AllowExternal: !pick.Model.Local, ProjectClass: pick.ProjectClass,
			}}, nil
		}
	}
	m, err := rdyCLILocalModel(ctx, env, modelID)
	if err != nil {
		return rdyCLIAssistSetup{}, err
	}
	return rdyCLIAssistSetup{Config: assist.AssistConfig{Model: m, ProjectClass: assist.AssistClassLocalOnly}}, nil
}

// rdyCLIUnavailable is the generator of a model that is configured and cannot be used. Every call fails closed, so
// the leg reports "unavailable" and carries the cause.
type rdyCLIUnavailable struct{ cause error }

func (u rdyCLIUnavailable) Chat(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatResult, error) {
	return providers.ChatResult{}, u.err()
}

func (u rdyCLIUnavailable) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, u.err()
}

func (u rdyCLIUnavailable) err() error {
	return contracts.Wrap(contracts.ErrUnavailable, u.cause, "the configured model cannot be used")
}

// rdyCLIConfigured reports whether the deployment configuration carries a model to use: both sections registered on
// the registry in use (a registry that does not know them cannot have configured anything) and at least one verified
// model listed.
func rdyCLIConfigured(env *Env) bool {
	have := map[string]bool{}
	for _, n := range env.Config.Sections() {
		have[n] = true
	}
	return have[localaiSectionName] && have[inferenceSectionName] && len(inferenceSection.Models) > 0
}

// rdyCLIQualified is true only for a registry entry that shows a qualified status (qualified-emulated or certified) for
// the controller use this leg makes of it. A candidate, a rejected or a retired entry, an entry qualified for another
// role only (an embedder, say) and a model the registry does not know are not qualified: the report then says
// "unqualified, advisory".
func rdyCLIQualified(e models.Entry) bool {
	if e.Status != models.StatusQualifiedEmulated && e.Status != models.StatusCertified {
		return false
	}
	return slices.Contains(e.Roles, models.RoleConversationalController) || slices.Contains(e.Roles, models.RoleTypedRouting)
}

// rdyCLILocalModel builds the model from the localai and inference sections, as `alixai serve` does for the controller
// role. modelID, when given, must be one of inference.models (never free text).
func rdyCLILocalModel(ctx context.Context, env *Env, modelID string) (*assist.AssistModel, error) {
	if !rdyCLIConfigured(env) {
		if modelID != "" {
			return nil, Usagef("-model %q: no model is configured (inference.models is empty)", modelID)
		}
		return nil, nil
	}
	inf, lcfg := inferenceSection, localaiSection
	if modelID != "" && !slices.Contains(inf.Models, modelID) {
		return nil, Usagef("-model %q is not one of inference.models", modelID)
	}
	label := modelID
	if label == "" {
		label = lcfg.Models.Controller
	}
	if label == "" {
		label = inf.Models[0]
	}
	out := &assist.AssistModel{Name: "local", ModelID: label, Local: true}
	fail := func(err error) (*assist.AssistModel, error) {
		out.Gen = rdyCLIUnavailable{cause: err}
		return out, nil
	}

	mr, err := loadModelRegistry()
	if err != nil {
		return fail(err)
	}
	versions, err := verifyModels(ctx, mr, inf.ModelsDir, inf.Models)
	if err != nil {
		return fail(err)
	}
	name, err := serveReadinessControllerModel(mr, inf.Models, rdyCLIFirst(modelID, lcfg.Models.Controller))
	if err != nil {
		return fail(err)
	}
	out.ModelID = name
	lcfg.Models.Controller = name
	if len(lcfg.QualifiedVersions) > 0 {
		versions = intersectVersions(versions, lcfg.QualifiedVersions)
	}
	if len(versions) == 0 {
		return fail(errors.New("no LocalAI version is qualified for the configured models"))
	}
	lcfg.QualifiedVersions = versions
	client, err := localai.New(lcfg, localai.WithClock(clock.Real{}))
	if err != nil {
		return fail(err)
	}
	gen, err := providers.WithBudget(client, rdyCLIBudget)
	if err != nil {
		return fail(err)
	}
	out.Gen = gen
	if e, ok := mr.Get(name); ok {
		out.Qualified = rdyCLIQualified(e)
	}
	return out, nil
}

func rdyCLIFirst(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
