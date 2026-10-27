package loop

import (
	"context"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Deps wires sibling implementations. Required for a live loop: Abilities,
// Reader, Caller. Others may be nil when their ability is off / in tests.
type Deps struct {
	Abilities AbilityResolver
	Domain    DomainClassifier
	Computer  Computer
	Verifier  AnalystVerifier
	Searcher  WebSearcher
	Fetcher   WebFetcher
	Reader    CapabilityReader
	Caller    StepCaller
	Budget    Budget
	Progress  Progress
	IDs       IDSource
	Caps      Caps
	Clock     func() time.Time
	// Turns is optional; when set, EndTurn runs when the loop returns.
	Turns TurnLifecycle
}

// Runner is the bounded analyst step loop.
type Runner struct {
	d Deps
}

// New builds a Runner. Abilities, Reader and Caller are required.
func New(d Deps) (*Runner, error) {
	if d.Abilities == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "analyst/loop: AbilityResolver is required")
	}
	if d.Reader == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "analyst/loop: CapabilityReader is required")
	}
	if d.Caller == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "analyst/loop: StepCaller is required")
	}
	d.Caps = d.Caps.Normalized()
	if d.IDs == nil {
		d.IDs = defaultIDs()
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	return &Runner{d: d}, nil
}

// Run enters the step loop. When the effective set lacks loop, returns
// OutcomeFallback (dialogue keeps the single-capability path). When the path was
// wanted but the model is not analyst_capable, Code is model_not_analyst_capable
// and Kind is OutcomeFallback.
func (r *Runner) Run(ctx context.Context, scope contracts.Scope, in Input) (Outcome, error) {
	if err := scope.Validate(); err != nil {
		return Outcome{}, err
	}
	if in.HasClientCaps {
		RememberClientCapabilities(in.TurnID, in.ClientCaps)
	}

	eff, err := r.d.Abilities.Resolve(ctx, scope, in.AnalystCapable)
	if err != nil {
		return Outcome{}, err
	}
	if !eff.Enabled || !eff.Abilities.Loop {
		out := Outcome{Kind: OutcomeFallback}
		if eff.Wanted {
			out.Code = eff.WantedReason
			if out.Code == "" {
				out.Code = contracts.ErrModelNotAnalystCapable
			}
		}
		return out, nil
	}
	if !eff.AnalystCapable {
		return Outcome{Kind: OutcomeFallback, Code: contracts.ErrModelNotAnalystCapable}, nil
	}

	if r.d.Domain != nil {
		cls, cerr := r.d.Domain.Classify(ctx, scope, in.Utterance)
		if cerr != nil {
			return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrDomainClassificationFailed}, nil
		}
		if cls == an.DomainClassificationOutOfDomain {
			return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrOutOfDomain}, nil
		}
	}

	st := &state{
		r: r, scope: scope, in: in, abilities: eff.Abilities,
		datasets: map[an.DatasetID]an.Dataset{},
		fetched:  map[string]string{},
		deadline: r.d.Clock().Add(time.Duration(r.d.Caps.MaxTurnWallMS) * time.Millisecond),
	}
	out, err := st.loop(ctx)
	if r.d.Turns != nil && in.TurnID != "" {
		r.d.Turns.EndTurn(in.TurnID)
	}
	return out, err
}

type state struct {
	r         *Runner
	scope     contracts.Scope
	in        Input
	abilities AbilityFlags
	datasets  map[an.DatasetID]an.Dataset
	facts     []an.DerivedFact
	citations []an.WebCitation
	fetched   map[string]string
	search    []SearchHit
	prior     []string
	reads     int
	searches  int
	fetches   int
	tokens    int
	cost      int64
	deadline  time.Time
	reserved  []BudgetReservation
}

func (s *state) loop(ctx context.Context) (Outcome, error) {
	defer s.settleAll(ctx, false)

	for step := 0; step < s.r.d.Caps.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			s.settleAll(ctx, true)
			if out, ok := s.tryAnswerFromSoFar(ctx); ok {
				out.Steps = step
				return out, nil
			}
			return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrResourceExhausted, Steps: step, Prior: s.prior}, nil
		}
		if hit, kind := s.capCheck(); hit {
			if out, ok := s.tryAnswerFromSoFar(ctx); ok {
				out.Steps = step
				out.CapHit = kind
				return out, nil
			}
			code := contracts.ErrAnalystCapExceeded
			if kind == CapTokens || kind == CapCost || kind == CapWall {
				code = contracts.ErrResourceExhausted
			}
			return Outcome{Kind: OutcomeRefuse, Code: code, CapHit: kind, Steps: step, Prior: s.prior}, nil
		}

		schema, err := CompactSchema(s.abilities)
		if err != nil {
			return Outcome{}, err
		}
		items := BuildStepPrompt(s.in.Utterance, s.abilities, s.datasets, s.prior, s.r.d.Caps)
		traceID := s.r.d.IDs.New("trc")

		if s.r.d.Budget != nil {
			rsv, berr := s.r.d.Budget.Reserve(ctx, s.scope, estimatePromptToks(items)+512)
			if berr != nil {
				if out, ok := s.tryAnswerFromSoFar(ctx); ok {
					out.Steps = step
					out.CapHit = CapCost
					return out, nil
				}
				return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrResourceExhausted, CapHit: CapCost, Steps: step, Prior: s.prior}, nil
			}
			s.reserved = append(s.reserved, rsv)
		}

		call, cerr := s.r.d.Caller.Call(ctx, StepCallInput{
			Scope: s.scope, Items: items, Schema: schema, TraceID: traceID,
		})
		if cerr != nil {
			if ctx.Err() != nil {
				s.settleAll(ctx, true)
				if out, ok := s.tryAnswerFromSoFar(ctx); ok {
					out.Steps = step
					return out, nil
				}
				return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrResourceExhausted, Steps: step, Prior: s.prior}, nil
			}
			return Outcome{Kind: OutcomeRefuse, Code: contracts.CodeOf(cerr), Steps: step, Prior: s.prior}, nil
		}
		s.tokens += call.TokIn + call.TokOut
		if len(s.reserved) > 0 {
			_ = s.r.d.Budget.Settle(ctx, s.scope, s.reserved[len(s.reserved)-1], BudgetActual{TokIn: call.TokIn, TokOut: call.TokOut})
			s.reserved = s.reserved[:len(s.reserved)-1]
		}

		sp, perr := ParseStep(call.Raw, s.abilities, traceID)
		if perr != nil {
			s.prior = append(s.prior, "step refused: "+string(contracts.CodeOf(perr)))
			if contracts.CodeOf(perr) == contracts.ErrAbilityDisabled {
				return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrAbilityDisabled, Steps: step + 1, Prior: s.prior}, nil
			}
			continue
		}

		out, done, xerr := s.execute(ctx, sp)
		if xerr != nil {
			code := contracts.CodeOf(xerr)
			s.prior = append(s.prior, fmt.Sprintf("%s error: %s", sp.Kind, code))
			if code == contracts.ErrAbilityDisabled || code == contracts.ErrWebQueryRefused ||
				code == contracts.ErrScopeViolation || code == contracts.ErrPermissionDenied {
				return Outcome{
					Kind: OutcomeRefuse, Code: code, Details: contracts.DetailsOf(xerr),
					Steps: step + 1, Prior: s.prior, Facts: s.factSet(),
				}, nil
			}
			continue
		}
		if done {
			out.Steps = step + 1
			out.Prior = s.prior
			out.Facts = s.factSet()
			out.WritesAttempted = false
			return out, nil
		}
	}

	if out, ok := s.tryAnswerFromSoFar(ctx); ok {
		out.CapHit = CapSteps
		out.Steps = s.r.d.Caps.MaxSteps
		return out, nil
	}
	return Outcome{Kind: OutcomeRefuse, Code: contracts.ErrAnalystCapExceeded, CapHit: CapSteps, Steps: s.r.d.Caps.MaxSteps, Prior: s.prior, Facts: s.factSet()}, nil
}

func (s *state) execute(ctx context.Context, sp an.StepProposal) (Outcome, bool, error) {
	switch sp.Kind {
	case an.StepProposalKindRead:
		s.publish(ctx, conversation.ProgressEventStageAnalystRead)
		return s.doRead(ctx, sp)
	case an.StepProposalKindCompute:
		s.publish(ctx, conversation.ProgressEventStageAnalystCompute)
		return s.doCompute(ctx, sp)
	case an.StepProposalKindSearch:
		s.publish(ctx, conversation.ProgressEventStageAnalystSearch)
		return s.doSearch(ctx, sp)
	case an.StepProposalKindFetch:
		s.publish(ctx, conversation.ProgressEventStageAnalystFetch)
		return s.doFetch(ctx, sp)
	case an.StepProposalKindAnswer:
		s.publish(ctx, conversation.ProgressEventStageAnalystAnswer)
		return s.doAnswer(ctx, sp)
	case an.StepProposalKindClarify:
		s.prior = append(s.prior, "clarify")
		return Outcome{Kind: OutcomeClarify, Clarify: sp.Clarify}, true, nil
	case an.StepProposalKindAct:
		// Hand off only — never commit here.
		s.prior = append(s.prior, "act handoff "+string(sp.Act.CapabilityID))
		return Outcome{Kind: OutcomeAct, Act: sp.Act, WritesAttempted: false}, true, nil
	}
	return Outcome{}, false, contracts.New(contracts.ErrSchemaInvalid, "unknown step kind")
}

func (s *state) doRead(ctx context.Context, sp an.StepProposal) (Outcome, bool, error) {
	if s.reads >= s.r.d.Caps.MaxReads {
		return Outcome{}, false, contracts.New(contracts.ErrAnalystCapExceeded, "max reads exceeded").
			WithDetails("cap", string(CapReads))
	}
	res, err := s.r.d.Reader.Read(ctx, s.scope, ReadRequest{
		CapabilityID: string(sp.Read.CapabilityID),
		Arguments:    sp.Read.Arguments,
	})
	if err != nil {
		return Outcome{}, false, err
	}
	ds := res.Dataset
	if ds.ID == "" {
		ds.ID = an.DatasetID(s.r.d.IDs.New("ds"))
	}
	if int(ds.RecordCount) > s.r.d.Caps.MaxRecordsPerDataset || len(ds.Rows) > s.r.d.Caps.MaxRecordsPerDataset {
		ds.Rows = ds.Rows[:s.r.d.Caps.MaxRecordsPerDataset]
		ds.RecordCount = int64(len(ds.Rows))
	}
	if s.totalRecords()+int(ds.RecordCount) > s.r.d.Caps.MaxTotalRecords {
		return Outcome{}, false, contracts.New(contracts.ErrAnalystCapExceeded, "max total records exceeded").
			WithDetails("cap", string(CapRecords))
	}
	s.datasets[ds.ID] = ds
	s.reads++
	s.prior = append(s.prior, fmt.Sprintf("read %s → %s (%d rows)", sp.Read.CapabilityID, ds.ID, ds.RecordCount))
	return Outcome{}, false, nil
}

func (s *state) doCompute(ctx context.Context, sp an.StepProposal) (Outcome, bool, error) {
	if s.r.d.Computer == nil {
		return Outcome{}, false, contracts.New(contracts.ErrAbilityDisabled, "compute engine not wired")
	}
	if ForecastOp(sp.Compute.Op) && !s.abilities.Forecasts {
		return Outcome{}, false, contracts.New(contracts.ErrAbilityDisabled, "forecasts ability off")
	}
	res, err := s.r.d.Computer.Run(ctx, s.scope, *sp.Compute, s.datasets)
	if err != nil {
		return Outcome{}, false, err
	}
	if res.Dataset != nil {
		ds := *res.Dataset
		if ds.ID == "" {
			ds.ID = an.DatasetID(s.r.d.IDs.New("ds"))
		}
		s.datasets[ds.ID] = ds
		s.prior = append(s.prior, fmt.Sprintf("compute %s → %s", sp.Compute.Op, ds.ID))
	} else {
		s.prior = append(s.prior, fmt.Sprintf("compute %s → facts", sp.Compute.Op))
	}
	s.facts = append(s.facts, res.Facts...)
	return Outcome{}, false, nil
}

func (s *state) doSearch(ctx context.Context, sp an.StepProposal) (Outcome, bool, error) {
	if s.r.d.Searcher == nil {
		return Outcome{}, false, contracts.New(contracts.ErrAbilityDisabled, "web search not wired")
	}
	if s.searches >= s.r.d.Caps.MaxSearches {
		return Outcome{}, false, contracts.New(contracts.ErrAnalystCapExceeded, "max searches exceeded").
			WithDetails("cap", string(CapSearches))
	}
	if QueryLeaksRestricted(sp.Search.Query, s.datasets) {
		return Outcome{}, false, contracts.New(contracts.ErrWebQueryRefused, "web query embeds confidential or restricted values").
			WithDetails("reason", "confidential_value")
	}
	dsList := make([]an.Dataset, 0, len(s.datasets))
	for _, ds := range s.datasets {
		dsList = append(dsList, ds)
	}
	res, err := s.r.d.Searcher.Search(ctx, s.scope, SearchInput{
		Query: sp.Search.Query, TurnID: s.in.TurnID, Datasets: dsList,
	})
	if err != nil {
		return Outcome{}, false, err
	}
	s.search = res.Hits
	s.searches++
	s.prior = append(s.prior, fmt.Sprintf("search → %d hits", len(res.Hits)))
	return Outcome{}, false, nil
}

func (s *state) doFetch(ctx context.Context, sp an.StepProposal) (Outcome, bool, error) {
	if s.r.d.Fetcher == nil {
		return Outcome{}, false, contracts.New(contracts.ErrAbilityDisabled, "web fetch not wired")
	}
	if s.fetches >= s.r.d.Caps.MaxFetches {
		return Outcome{}, false, contracts.New(contracts.ErrAnalystCapExceeded, "max fetches exceeded").
			WithDetails("cap", string(CapFetches))
	}
	idx := int(sp.Fetch.SearchResultIndex)
	if idx < 0 || idx >= len(s.search) {
		return Outcome{}, false, contracts.New(contracts.ErrValidationFailed, "fetch index not from a prior search hit")
	}
	if s.search[idx].URL != sp.Fetch.URL {
		return Outcome{}, false, contracts.New(contracts.ErrValidationFailed, "fetch url does not match the search hit")
	}
	res, err := s.r.d.Fetcher.Fetch(ctx, s.scope, FetchInput{
		URL: sp.Fetch.URL, Index: idx, TurnID: s.in.TurnID,
	})
	if err != nil {
		return Outcome{}, false, err
	}
	cit := res.Citation
	if cit.ID == "" {
		cit.ID = s.r.d.IDs.New("wcit")
	}
	cit.SourceLabel = "web"
	s.citations = append(s.citations, cit)
	s.fetched[cit.ID] = res.Text
	s.fetches++
	s.prior = append(s.prior, "fetch "+cit.ID)
	return Outcome{}, false, nil
}

func (s *state) doAnswer(ctx context.Context, sp an.StepProposal) (Outcome, bool, error) {
	s.prior = append(s.prior, "answer")
	if s.r.d.Verifier == nil {
		// Without a verifier the loop still ends; dialogue may template.
		return Outcome{
			Kind:  OutcomeAnswer,
			Text:  sp.Answer.TextPlan,
			Spans: sp.Answer.SourceSpans,
		}, true, nil
	}
	facts := s.factSet()
	vres, err := s.r.d.Verifier.VerifyAnswer(ctx, VerifyAnswerInput{
		Scope: s.scope, TextPlan: sp.Answer.TextPlan, Blocks: sp.Answer.Blocks,
		SourceSpans: sp.Answer.SourceSpans, Facts: facts, Stream: s.in.StreamVerified,
		Language: s.in.Language,
	})
	if err != nil {
		return Outcome{}, false, err
	}
	text, modelText := realiseRefs(vres, facts, s.in.Language)
	return Outcome{
		Kind: OutcomeAnswer, Text: text, ModelText: modelText, Blocks: vres.Blocks,
		CoreMaterialised: vres.CoreMaterialised, Spans: sp.Answer.SourceSpans,
	}, true, nil
}

func (s *state) tryAnswerFromSoFar(ctx context.Context) (Outcome, bool) {
	if len(s.datasets) == 0 && len(s.facts) == 0 {
		return Outcome{}, false
	}
	if s.r.d.Verifier == nil {
		return Outcome{}, false
	}
	plan := "Summary of verified data gathered so far."
	facts := s.factSet()
	vres, err := s.r.d.Verifier.VerifyAnswer(ctx, VerifyAnswerInput{
		Scope: s.scope, TextPlan: plan, Facts: facts, Language: s.in.Language,
	})
	if err != nil || !vres.Verified {
		return Outcome{}, false
	}
	text, modelText := realiseRefs(vres, facts, s.in.Language)
	return Outcome{
		Kind: OutcomeAnswer, Text: text, ModelText: modelText, Blocks: vres.Blocks,
		CoreMaterialised: vres.CoreMaterialised, Facts: facts, Prior: s.prior,
	}, true
}

// realiseRefs resolves {{ref:…}} for the client while keeping the token form for
// the model window (ADR 0049). Template / non-verified answers pass through.
func realiseRefs(vres VerifyAnswerResult, facts FactSet, language string) (text, modelText string) {
	text = vres.Text
	modelText = vres.ModelText
	if modelText == "" && len(verify.ParseRefTokens(text)) > 0 {
		modelText = text
	}
	if modelText == "" || !vres.Verified {
		return text, modelText
	}
	lang := verify.LangEN
	if language != "" {
		if l, err := verify.ParseLanguage(language); err == nil {
			lang = l
		}
	}
	af := verify.AnalystFactSet{Derived: facts.Facts}
	for _, ds := range facts.Datasets {
		af.Datasets = append(af.Datasets, ds)
	}
	for _, c := range facts.Citations {
		af.Citations = append(af.Citations, verify.AnalystCitation{Meta: c, FetchedText: facts.Fetched[c.ID]})
	}
	resolved, err := verify.ResolveAnalystRefs(modelText, af, lang)
	if err != nil {
		return text, modelText
	}
	return resolved, modelText
}

func (s *state) factSet() FactSet {
	return FactSet{
		Datasets:  s.datasets,
		Facts:     append([]an.DerivedFact(nil), s.facts...),
		Citations: append([]an.WebCitation(nil), s.citations...),
		Fetched:   s.fetched,
	}
}

func (s *state) totalRecords() int {
	n := 0
	for _, ds := range s.datasets {
		n += int(ds.RecordCount)
	}
	return n
}

func (s *state) capCheck() (bool, CapKind) {
	now := s.r.d.Clock()
	if !s.deadline.IsZero() && !now.Before(s.deadline) {
		return true, CapWall
	}
	if s.tokens >= s.r.d.Caps.MaxTurnTokens {
		return true, CapTokens
	}
	if s.r.d.Caps.MaxTurnCost != nil && s.cost >= *s.r.d.Caps.MaxTurnCost {
		return true, CapCost
	}
	return false, ""
}

func (s *state) publish(ctx context.Context, stage conversation.ProgressEventStage) {
	if s.r.d.Progress == nil {
		return
	}
	s.r.d.Progress.Publish(ctx, s.scope, s.in.TurnID, stage)
}

func (s *state) settleAll(ctx context.Context, cancel bool) {
	if s.r.d.Budget == nil {
		return
	}
	for _, rsv := range s.reserved {
		a := BudgetActual{Cancelled: cancel}
		_ = s.r.d.Budget.Settle(context.WithoutCancel(ctx), s.scope, rsv, a)
	}
	s.reserved = nil
}

func estimatePromptToks(items []PromptItem) int {
	n := 0
	for _, it := range items {
		n += (len(it.Content) + 2) / 3
	}
	return n
}

// Ensure common.TraceID import used when building act outcomes in tests.
var _ = common.TraceID("")
