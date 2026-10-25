package extmode

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Retriever is the retrieval pipeline (query.Pipeline implements it).
type Retriever interface {
	Query(ctx context.Context, scope contracts.Scope, req query.Request) (query.Result, error)
}

// Knowledge is the dialogue.Knowledge of an engine that may use an external provider. In Local mode
// it is exactly retrieval and the local answerer. For an external route the excerpts are first
// filtered by the egress guard, source by source, and only then does the external answerer see them.
//
// The answerer for the external route is built by the composition root over ExternalGenerator, and
// must share its citation Resolver with the local one so that every reference resolves in one place.
type Knowledge struct {
	rt       *Runtime
	retr     Retriever
	local    *answer.Answerer
	external *answer.Answerer
}

var _ dialogue.Knowledge = (*Knowledge)(nil)

// NewKnowledge returns the Knowledge. The retriever and the external answerer are required; the local
// answerer may be nil when no local model can answer.
func (r *Runtime) NewKnowledge(retr Retriever, local, external *answer.Answerer) (*Knowledge, error) {
	if retr == nil || external == nil {
		return nil, contracts.New(contracts.ErrInternal, "extmode: knowledge needs a retriever and an external answerer")
	}
	return &Knowledge{rt: r, retr: retr, local: local, external: external}, nil
}

// ExternalGenerator is the text generator the external answerer is built over. Every call it makes
// goes through the router, whose egress guard evaluates the excerpts again.
func (r *Runtime) ExternalGenerator() providers.TextGenerator { return ragGen{r} }

// evidenceInfo is what the generator needs to class the evidence of one answer.
type evidenceInfo struct {
	sourceID string
	class    string
	label    string
	origin   dataegress.Origin
}

type ctxKey struct{}

// Answer implements dialogue.Knowledge.
func (k *Knowledge) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	if err := scope.Validate(); err != nil {
		return rag.AnswerResult{}, err
	}
	q := query.Request{Text: string(req.Question)}
	for _, id := range req.SourceIDs {
		q.SourceIDs = append(q.SourceIDs, string(id))
	}
	res, err := k.retr.Query(ctx, scope, q)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	pkg := res.Package
	ps := pkg.Scope
	if string(ps.ApplicationID) != scope.ApplicationID || string(ps.TenantID) != scope.TenantID ||
		string(ps.WorkspaceID) != scope.WorkspaceID || string(ps.PrincipalID) != scope.PrincipalID {
		return rag.AnswerResult{}, contracts.New(contracts.ErrScopeViolation, "the evidence package was built for another scope")
	}

	mode, err := k.rt.router.Resolver().Resolve(ctx, scope)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	rp, perr := k.rt.cfg.Policy.RolePolicy(ctx, scope, modes.RoleController)
	if mode.Mode == modes.ModeLocal || perr != nil || len(rp.Profiles) == 0 {
		return k.answerLocal(ctx, scope, pkg)
	}

	kept, restricted, err := k.filter(ctx, scope, pkg)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	if restricted {
		// The best evidence is a source this provider may not receive. A local model that can serve the
		// request (Hybrid) answers it; otherwise the typed result says so, and nothing was sent.
		if mode.Mode == modes.ModeHybrid && k.local != nil && k.rt.cfg.LocalProviders != nil && k.rt.cfg.LocalProviders.Usable(modes.RoleController) {
			return k.answerLocal(ctx, scope, pkg)
		}
		return restrictedResult(pkg), nil
	}
	ev := map[string]evidenceInfo{}
	origin := dataegress.Origin{ApplicationID: scope.ApplicationID, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID}
	for _, it := range kept.Items {
		ev[string(it.EvidenceID)] = evidenceInfo{sourceID: string(it.SourceID), class: string(*it.SourceClassification), label: it.DisplayName, origin: origin}
	}
	out, err := k.external.Answer(context.WithValue(ctx, ctxKey{}, ev), scope, kept)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	return out.Answer, nil
}

func (k *Knowledge) answerLocal(ctx context.Context, scope contracts.Scope, pkg rag.EvidencePackage) (rag.AnswerResult, error) {
	if k.local == nil {
		return rag.AnswerResult{}, contracts.New(contracts.ErrUnavailable, "extmode: no local answerer")
	}
	out, err := k.local.Answer(ctx, scope, pkg)
	if err != nil {
		return rag.AnswerResult{}, err
	}
	return out.Answer, nil
}

// restrictedResult is the typed result "the selected provider cannot access the required evidence".
func restrictedResult(pkg rag.EvidencePackage) rag.AnswerResult {
	return rag.AnswerResult{Value: rag.NoSupportedEvidence{
		Outcome: "no_supported_evidence", Reason: rag.NoSupportedEvidenceReasonEgressRestricted,
		Notices: append(rag.RetrievalNotices{}, pkg.Retrieval.Notices...),
	}}
}

// eligible reports whether a source's own declaration lets its content be considered for a provider:
// the egress mode allows content or metadata and the classification is known and below restricted.
// The egress guard decides again, with the operator's policy, and the more restrictive wins.
func eligible(it rag.EvidenceItem) bool {
	switch rag.EffectiveSourceEgress(it.SourceEgress) {
	case rag.SourceEgressAllowExcerpt, rag.SourceEgressAllowMetadata:
	default:
		return false
	}
	if it.SourceClassification == nil {
		return false
	}
	switch *it.SourceClassification {
	case rag.SourceClassificationPublic, rag.SourceClassificationInternal, rag.SourceClassificationConfidential:
		return true
	}
	return false
}

// filter evaluates the excerpts of a package against the egress guard and returns the package with
// only what may be sent (bounded excerpts, or metadata). restricted is true when the best-ranked
// evidence, the evidence the answer needs, cannot go to the provider; nothing is then evaluated or sent.
func (k *Knowledge) filter(ctx context.Context, scope contracts.Scope, pkg rag.EvidencePackage) (rag.EvidencePackage, bool, error) {
	live := make([]rag.EvidenceItem, 0, len(pkg.Items))
	for _, it := range pkg.Items {
		if it.Availability != "removed" && strings.TrimSpace(it.Text) != "" {
			live = append(live, it)
		}
	}
	out := pkg
	out.Items = nil
	if len(live) == 0 {
		return out, false, nil
	}
	best := 0
	for i, it := range live {
		if it.Rank < live[best].Rank {
			best = i
		}
	}
	if !eligible(live[best]) {
		return out, true, nil
	}
	origin := &dataegress.Origin{ApplicationID: scope.ApplicationID, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID}
	parts := []dataegress.Part{{
		Kind: dataegress.PartText, Text: string(pkg.Query), Category: string(pv.SendCategoryUserMessage), Class: string(pv.DataClassificationInternal),
	}}
	var idx []int // idx[i] is the live item of part i+1
	maxClass := pv.DataClassificationInternal
	for i, it := range live {
		if !eligible(it) {
			continue // withheld: its mode or class keeps it out
		}
		cls := pv.DataClassification(*it.SourceClassification)
		if sensitive(cls) {
			maxClass = cls
		}
		parts = append(parts, dataegress.Part{
			Kind: dataegress.PartText, Text: it.Text, Label: it.DisplayName, Category: string(pv.SendCategoryRAGExcerpt),
			Class: string(cls), SourceID: string(it.SourceID), Required: i == best, Origin: origin,
		})
		idx = append(idx, i)
	}
	req := providerreg.Requirement{Role: pv.RoleConversationalController, DataClass: maxClass, OutputToks: int64(k.rt.cfg.Budget.MaxOutputTokens)}
	prof, err := k.rt.profileFor(ctx, scope, modes.RoleController, req)
	if err != nil {
		return out, false, err
	}
	cand := dataegress.Candidate{
		Scope: scope, Profile: prof, Role: pv.RoleConversationalController, Channel: pv.ChannelInteractive, TraceID: "trc_extmode",
		Instructions:   "Answer the question using only the evidence passages given.",
		Messages:       []dataegress.Message{{Role: pv.RequestEnvelopeMessageRoleUser, Parts: parts}},
		ResponseFormat: pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText},
		MaxOutputToks:  int64(max(k.rt.cfg.Budget.MaxOutputTokens, 1)), TimeoutMS: max(k.rt.cfg.Budget.Timeout.Milliseconds(), 1000),
	}
	eg, err := k.rt.cfg.Egress.Evaluate(ctx, cand)
	if err != nil {
		switch detailRule(err) {
		case string(pv.EgressDecisionReasonSourceLocalOnly), string(pv.EgressDecisionReasonSourceDenied), string(pv.EgressDecisionReasonClassNotAllowed):
			return out, true, nil
		}
		return out, false, asDenied(err)
	}
	if eg.Envelope == nil {
		return out, false, deny("guard_failure")
	}
	for _, po := range eg.Parts {
		if !po.Kept || po.Message != 0 || po.Part < 1 || po.Part > len(idx) {
			continue
		}
		text, ok := envelopeText(eg.Envelope, po.EnvelopeMessage, po.EnvelopePart)
		if !ok {
			return out, false, deny("envelope_unreadable")
		}
		it := live[idx[po.Part-1]]
		it.Text = text
		out.Items = append(out.Items, it)
	}
	if len(out.Items) == 0 {
		return out, true, nil
	}
	return out, false, nil
}

func detailRule(err error) string {
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce == nil {
		return ""
	}
	s, _ := ce.Details["rule"].(string)
	return s
}

// ---- the generator of the external answerer ----

// ragGen is the generator the external answerer calls. It rebuilds the egress candidate from the
// request: the system message is the core's, each evidence block is a RAG excerpt of its source, and
// the rest is the question. A block whose handle the filter did not release has no source and is denied.
type ragGen struct{ r *Runtime }

var evidenceBlock = regexp.MustCompile(`(?s)<evidence id="([^"]*)">\n(.*?)\n</evidence>\n`)

func (g ragGen) Chat(ctx context.Context, scope contracts.Scope, req providers.ChatRequest) (providers.ChatResult, error) {
	if err := scope.Validate(); err != nil {
		return providers.ChatResult{}, err
	}
	ev, ok := ctx.Value(ctxKey{}).(map[string]evidenceInfo)
	if !ok {
		return providers.ChatResult{}, deny("evidence_not_released")
	}
	c := dataegress.Candidate{Scope: scope, Channel: pv.ChannelInteractive, TraceID: "trc_extmode"}
	var instr []string
	maxClass := pv.DataClassificationInternal
	for _, m := range req.Messages {
		if m.Role == providers.MsgSystem {
			instr = append(instr, m.Content)
			continue
		}
		var parts []dataegress.Part
		text, last := m.Content, 0
		add := func(s string) {
			if s != "" {
				parts = append(parts, dataegress.Part{
					Kind: dataegress.PartText, Text: s, Category: string(pv.SendCategoryUserMessage), Class: string(pv.DataClassificationInternal),
				})
			}
		}
		for _, loc := range evidenceBlock.FindAllStringSubmatchIndex(text, -1) {
			add(text[last:loc[4]])
			handle := text[loc[2]:loc[3]]
			p := dataegress.Part{Kind: dataegress.PartText, Text: text[loc[4]:loc[5]], Category: string(pv.SendCategoryRAGExcerpt)}
			if info, known := ev[handle]; known {
				o := info.origin
				p.SourceID, p.Class, p.Label, p.Origin = info.sourceID, info.class, info.label, &o
				if cls := pv.DataClassification(info.class); sensitive(cls) {
					maxClass = cls
				}
			}
			parts = append(parts, p)
			last = loc[5]
		}
		add(text[last:])
		c.Messages = append(c.Messages, dataegress.Message{Role: envRole(m.Role), Parts: parts})
	}
	c.Instructions = strings.Join(instr, "\n\n")
	c.MaxOutputToks, c.TimeoutMS = int64(max(req.MaxTokens, 1)), max(g.r.cfg.Budget.Timeout.Milliseconds(), 1000)
	c.ResponseFormat = pv.RequestEnvelopeResponseFormat{Kind: pv.RequestEnvelopeResponseFormatKindText}
	st := &staged{original: req, filtered: req, cand: c, role: modes.RoleController, schemaOut: req.Output.Kind() == providers.ConstraintJSONSchema}
	if st.schemaOut {
		c.ResponseFormat = responseFormat(st, c, req.Output)
		st.cand = c
	}
	st.req = providerreg.Requirement{
		Role: pv.RoleConversationalController, DataClass: maxClass, ContextToks: int64(g.r.cfg.Budget.EstimateChat(req)),
		OutputToks: int64(max(req.MaxTokens, 1)), StructuredOutput: structuredNeed(st, c),
	}
	return g.r.run(ctx, scope, st)
}

func (ragGen) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, contracts.New(contracts.ErrUnavailable, "extmode: the external route does not stream")
}
