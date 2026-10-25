package dataegress

import (
	"context"
	"math/big"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// maxMicroUnits is the largest amount a MicroUnits value holds (2^53-1).
const maxMicroUnits = 1<<53 - 1

// DisclosureInput describes the request a host is about to show a disclosure for
// (additive plan section 6.3). Everything is optional except the role.
type DisclosureInput struct {
	Role providers.Role
	// Sources are the RAG sources the request would draw on.
	Sources []providers.SourceID
	// EstimatedToksIn and MaxOutputToks feed the upper-bound cost estimate. A zero
	// MaxOutputToks means the profile's limit.
	EstimatedToksIn int64
	MaxOutputToks   int64
	// Budget is the state of the budget the call would be charged to, as the budget
	// service reports it. Empty is taken as ok: the guard never decides on a budget, the
	// budget service refuses an exhausted one with its own typed error.
	Budget providers.BudgetStatus
}

// Disclose computes what a host application may show a user before a request
// leaves the deployment (additive plan 6.3): which provider and model would process
// it, which data classes and categories may leave, whether RAG excerpts are included,
// the provider's stated retention and training terms, an upper-bound cost and what
// happens when the provider is unavailable. It runs the same request-independent
// checks as Evaluate (kill switch, policy chain, profile, allowlists, region), so the
// disclosure cannot promise what the guard would then refuse; when any of them fails
// nothing may leave and nothing is listed as leaving. File excerpts and raw audio are
// listed only where the files and audio settings of the chain let them leave. It
// records nothing: it is not a decision about a request.
func (s *Service) Disclose(ctx context.Context, scope contracts.Scope, prof providers.ProviderProfile, in DisclosureInput) (providers.EgressDisclosure, error) {
	if err := validateCandidate(Candidate{Scope: scope, Profile: prof, Role: in.Role, Channel: providers.ChannelInteractive, TraceID: "disclosure"}); err != nil {
		return providers.EgressDisclosure{}, err
	}
	if in.Budget != "" && !in.Budget.Valid() {
		return providers.EgressDisclosure{}, contracts.New(contracts.ErrValidationFailed, "dataegress: the budget state is not a known state").WithDetails("field", "budget")
	}
	budget := in.Budget
	if budget == "" {
		budget = providers.BudgetStatusOk
	}
	pf, err := s.preflight(ctx, scope, prof, in.Role)
	if err != nil {
		return providers.EgressDisclosure{}, wrapStore(err, "dataegress: the disclosure could not be computed")
	}
	if pf.denial != nil {
		d := s.disclosureStaysLocal(prof, in.Role)
		d.Cost.BudgetState = budget
		return d, nil
	}

	r := &run{s: s, scope: scope, prof: prof, chain: pf.chain, sources: map[providers.SourceID]*srcInfo{}}
	ids := make([]providers.SourceID, 0, len(in.Sources))
	for _, id := range in.Sources {
		if sourceIDRE.MatchString(string(id)) {
			ids = append(ids, id)
		}
	}
	if err := r.resolveSourceIDs(ctx, ids); err != nil {
		return providers.EgressDisclosure{}, wrapStore(err, "dataegress: the disclosure could not be computed")
	}

	classes := effectiveClasses(pf.chain, prof)
	excerpts, metadata := false, false
	for _, id := range r.order {
		info := r.sources[id]
		if info.policy == nil || info.policy.SourceClass == nil || !info.policy.SourceClass.Valid() || *info.policy.SourceClass == providers.DataClassificationRestricted {
			continue
		}
		el := providers.EgressEligibleClass(*info.policy.SourceClass)
		if !r.classAllowed(el, info.policy) {
			continue
		}
		switch info.mode {
		case providers.EgressModeAllowExcerpt:
			excerpts, metadata = true, true
		case providers.EgressModeAllowMetadata:
			metadata = true
		default:
			continue
		}
		if !containsClass(classes, el) {
			classes = append(classes, el)
		}
	}
	classes = dedupeClasses(classes)
	// Content no RAG source covers, an excerpt of an uploaded file (text, so any profile) and raw audio (a profile
	// that takes it), leaves only under its own setting along the chain: the disclosure lists it, with its classes,
	// only where the guard's rule (run.contentAllowed) lets it leave.
	excerptClasses := allowedContentClasses(r.filesAllowed)
	var audioClasses []providers.EgressEligibleClass
	if prof.Media != nil && prof.Media.Audio != nil && containsModality(prof.Modalities.Input, providers.ModalityAudio) {
		audioClasses = allowedContentClasses(r.audioAllowed)
	}
	if len(classes) == 0 && len(excerptClasses) == 0 && len(audioClasses) == 0 {
		d := s.disclosureStaysLocal(prof, in.Role)
		d.Cost.BudgetState = budget
		return d, nil
	}

	var cats []providers.SendCategory
	if len(classes) > 0 {
		cats = append(cats, providers.SendCategoryUserMessage, providers.SendCategoryConversationHistory, providers.SendCategoryStructuredState,
			providers.SendCategoryDeterministicFact, providers.SendCategoryDialogueDirective, providers.SendCategoryActionReceipt)
		if excerpts {
			cats = append(cats, providers.SendCategoryRAGExcerpt)
		}
		if metadata {
			cats = append(cats, providers.SendCategoryRAGMetadata)
		}
		if prof.Media != nil {
			if prof.Media.Files != nil && containsModality(prof.Modalities.Input, providers.ModalityFile) {
				cats = append(cats, providers.SendCategoryFileContent)
			}
			if prof.Media.Images != nil && containsModality(prof.Modalities.Input, providers.ModalityImage) {
				cats = append(cats, providers.SendCategoryImageContent)
			}
		}
	}
	if len(excerptClasses) > 0 {
		cats = append(cats, providers.SendCategoryFileContent)
	}
	if len(audioClasses) > 0 {
		cats = append(cats, providers.SendCategoryAudioContent)
	}
	classes = dedupeClasses(append(append(classes, excerptClasses...), audioClasses...))
	out := prof.Limits.MaxOutputToks
	if in.MaxOutputToks > 0 && in.MaxOutputToks < out {
		out = in.MaxOutputToks
	}
	d := baseDisclosure(prof, in.Role)
	d.GeneratedAt = timestamp(s.clk.Now())
	d.ExternalEnabled, d.DataClasses, d.DataCategories, d.RAGExcerptsIncluded = true, classes, sortedSend(setOf(cats)), excerpts
	d.Cost = providers.EgressDisclosureCost{Currency: prof.Pricing.Currency, EstimateUpperMicroUnits: costUpperBound(prof.Pricing, in.EstimatedToksIn, out, false), BudgetState: budget}
	return d, nil
}

// allowedContentClasses are the classes of content no RAG source covers that allowed (run.filesAllowed or
// run.audioAllowed) lets leave.
func allowedContentClasses(allowed func(providers.EgressEligibleClass) string) []providers.EgressEligibleClass {
	var out []providers.EgressEligibleClass
	for _, cls := range []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential} {
		if allowed(cls) == "" {
			out = append(out, cls)
		}
	}
	return out
}

func setOf(list []providers.SendCategory) map[providers.SendCategory]bool {
	m := make(map[providers.SendCategory]bool, len(list))
	for _, c := range list {
		m[c] = true
	}
	return m
}

// effectiveClasses are the classes (public, internal) every level of the chain
// lists and the profile allows. Internal counts only where the level carries the
// operator approval. Confidential is a per-source decision and never appears here.
func effectiveClasses(ch chain, prof providers.ProviderProfile) []providers.EgressEligibleClass {
	var out []providers.EgressEligibleClass
	for _, cls := range []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal} {
		ok := containsClass(prof.AllowedDataClasses, cls)
		for _, p := range ch.all() {
			if p == nil || !containsClass(p.AllowedClasses, cls) || (cls == providers.EgressEligibleClassInternal && p.Approval == nil) {
				ok = false
			}
		}
		if ok {
			out = append(out, cls)
		}
	}
	return out
}

// baseDisclosure is the part of a disclosure that comes from the profile alone.
func baseDisclosure(prof providers.ProviderProfile, role providers.Role) providers.EgressDisclosure {
	regions := []providers.RegionCode{}
	if prof.Region.Residency != providers.ResidencyUnknown {
		regions = append(regions, prof.Region.ProcessingRegions...)
	}
	on := providers.EgressDisclosureOnUnavailable{Behavior: providers.EgressDisclosureOnUnavailableBehaviorFail}
	if prof.Fallback.ProfileID != nil && len(prof.Fallback.On) > 0 {
		id := *prof.Fallback.ProfileID
		on = providers.EgressDisclosureOnUnavailable{Behavior: providers.EgressDisclosureOnUnavailableBehaviorFallback, FallbackProfileID: &id}
	}
	return providers.EgressDisclosure{
		Provider: providerRef(prof), Role: role,
		DataClasses: []providers.EgressEligibleClass{}, DataCategories: []providers.SendCategory{}, Regions: regions,
		Terms: providers.EgressDisclosureTerms{
			Retention: prof.Terms.Retention, RetentionDays: prof.Terms.RetentionDays, TrainingUse: prof.Terms.TrainingUse, Residency: prof.Region.Residency,
		},
		Cost:          providers.EgressDisclosureCost{Currency: prof.Pricing.Currency, BudgetState: providers.BudgetStatusOk},
		OnUnavailable: on,
	}
}

// disclosureStaysLocal is the disclosure of a request that may not leave: nothing
// is listed as leaving.
func (s *Service) disclosureStaysLocal(prof providers.ProviderProfile, role providers.Role) providers.EgressDisclosure {
	d := baseDisclosure(prof, role)
	d.GeneratedAt = timestamp(s.clk.Now())
	return d
}

// disclosure is the disclosure of an allowed request, from what was actually sent.
func (r *run) disclosure(env *providers.RequestEnvelope) providers.EgressDisclosure {
	d := baseDisclosure(r.prof, r.c.Role)
	d.GeneratedAt = timestamp(r.now)
	classes := effectiveClasses(r.chain, r.prof)
	for _, id := range r.order {
		info := r.sources[id]
		if info.outcome != providers.EgressDecisionSourceDecisionOutcomeExcluded && info.policy != nil && info.policy.SourceClass != nil {
			if el := providers.EgressEligibleClass(*info.policy.SourceClass); el.Valid() && !containsClass(classes, el) {
				classes = append(classes, el)
			}
		}
	}
	for _, el := range r.contentClasses {
		if !containsClass(classes, el) {
			classes = append(classes, el)
		}
	}
	d.ExternalEnabled, d.DataClasses = true, dedupeClasses(classes)
	d.DataCategories = append([]providers.SendCategory{}, r.d.Categories.Sent...)
	d.RAGExcerptsIncluded = r.d.RAGExcerptsIncluded
	hasMedia := false
	for _, m := range env.Payload.Messages {
		for _, p := range m.Parts {
			if _, text := p.Value.(providers.RequestEnvelopeTextPart); !text {
				hasMedia = true
			}
		}
	}
	d.Cost.EstimateUpperMicroUnits = costUpperBound(r.prof.Pricing, int64(env.Internal.EstimatedToksIn), env.Payload.Limits.MaxOutputToks, hasMedia)
	return d
}

// costUpperBound is the upper-bound price of a request in integer micro-units:
// the sum over the metered units of ceil(units * micro_units / per_units), with the
// upper bounds toksIn and toksOut. It is nil when a unit the request uses has no price
// (such a request is not estimated) or the amount cannot be represented. A request with
// media is not estimated here; the budget service (X-09) prices media.
func costUpperBound(pt providers.PriceTable, toksIn, toksOut int64, hasMedia bool) *providers.MicroUnits {
	if hasMedia || pt.Prices.ToksIn == nil || pt.Prices.ToksOut == nil {
		return nil
	}
	total := new(big.Int)
	add := func(units int64, p *providers.PriceTablePrice) bool {
		if p == nil || p.PerUnits <= 0 || units < 0 {
			return false
		}
		n := new(big.Int).Mul(big.NewInt(units), big.NewInt(int64(p.MicroUnits)))
		d := big.NewInt(p.PerUnits)
		n.Add(n, new(big.Int).Sub(d, big.NewInt(1))) // ceil
		total.Add(total, n.Quo(n, d))
		return true
	}
	if !add(toksIn, pt.Prices.ToksIn) || !add(toksOut, pt.Prices.ToksOut) {
		return nil
	}
	if pt.Prices.Requests != nil && !add(1, pt.Prices.Requests) {
		return nil
	}
	if !total.IsInt64() || total.Int64() > maxMicroUnits {
		return nil
	}
	v := providers.MicroUnits(total.Int64())
	return &v
}
