package dialogue

import (
	"context"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// NewAnalystRunnerFromConfig builds a loop.Runner from dialogue.analyst config and
// sibling seams. The integrator passes real Computer / Verifier / Web / Domain /
// AbilityResolver implementations; nil optional seams disable those steps.
// Caps and the deployment AbilityResolver come from cfg (Loaded().Analyst).
func NewAnalystRunnerFromConfig(cfg AnalystConfig, actionsCoord *actions.Coordinator, sessionScope func() contracts.Scope, caller loop.StepCaller, opt AnalystRunnerOptions) (*loop.Runner, error) {
	if actionsCoord == nil || caller == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "dialogue: analyst runner needs actions and a step caller")
	}
	abilities := opt.Abilities
	if abilities == nil {
		abilities = deploymentAbilityResolver{cfg: cfg}
	}
	caps := loop.Caps{
		MaxSteps: cfg.MaxSteps, MaxReads: cfg.MaxReads,
		MaxRecordsPerDataset: cfg.MaxRecordsPerDataset, MaxTotalRecords: cfg.MaxTotalRecords,
		MaxTurnTokens: cfg.MaxTurnTokens, MaxTurnWallMS: cfg.MaxTurnWallMS,
		MaxTurnCost: cfg.MaxTurnCost,
		MaxSearches: cfg.Web.MaxSearches, MaxFetches: cfg.Web.MaxFetches,
	}
	var progress loop.Progress
	if opt.OnProgress != nil {
		progress = progressFunc(opt.OnProgress)
	}
	return loop.New(loop.Deps{
		Abilities: abilities,
		Domain:    opt.Domain,
		Computer:  opt.Computer,
		Verifier:  opt.Verifier,
		Searcher:  opt.Searcher,
		Fetcher:   opt.Fetcher,
		Reader:    coordinatorReader{actions: actionsCoord, session: sessionScope, packages: opt.Packages},
		Caller:    caller,
		Budget:    opt.Budget,
		Progress:  progress,
		IDs:       opt.IDs,
		Caps:      caps,
		Turns:     opt.Turns,
	})
}

// AnalystRunnerOptions holds optional sibling seams for NewAnalystRunnerFromConfig.
type AnalystRunnerOptions struct {
	Abilities  loop.AbilityResolver
	Domain     loop.DomainClassifier
	Computer   loop.Computer
	Verifier   loop.AnalystVerifier
	Searcher   loop.WebSearcher
	Fetcher    loop.WebFetcher
	Budget     loop.Budget
	IDs        loop.IDSource
	OnProgress ProgressHook
	Packages   PackageSource
	Turns      loop.TurnLifecycle
}

// ProgressHook sees one progress stage during a turn (analyst_* stages).
type ProgressHook func(ctx context.Context, scope contracts.Scope, turnID string, stage conversation.ProgressEventStage)

type progressFunc ProgressHook

func (f progressFunc) Publish(ctx context.Context, scope contracts.Scope, turnID string, stage conversation.ProgressEventStage) {
	if f != nil {
		f(ctx, scope, turnID, stage)
	}
}
