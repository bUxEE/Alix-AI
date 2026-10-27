package dataegress

import (
	"context"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// denial is the first failing check of an evaluation.
type denial struct {
	check  providers.EgressDecisionCheckName
	reason providers.EgressDecisionReason
	// code is the fixed reason code recorded on the check, never free text.
	code string
}

// preflight is the part of an evaluation that does not depend on the content of
// the request: the kill switch, the policy chain, the profile and the allowlists
// (steps 1 to 7 of the check order). Evaluate and Disclose share it, so the
// disclosure a host shows can never disagree with the decision the guard takes.
type preflight struct {
	checks []providers.EgressDecisionCheck
	levels []providers.EgressDecisionLevelResult
	chain  chain
	denial *denial
}

func (pf *preflight) pass(name providers.EgressDecisionCheckName) {
	pf.checks = append(pf.checks, providers.EgressDecisionCheck{Name: name, Passed: true, Reason: "passed"})
}

func (pf *preflight) fail(name providers.EgressDecisionCheckName, reason providers.EgressDecisionReason, code string) {
	pf.checks = append(pf.checks, providers.EgressDecisionCheck{Name: name, Passed: false, Reason: providers.ReasonCode(code)})
	pf.denial = &denial{check: name, reason: reason, code: code}
}

// preflight evaluates the checks in order and stops at the first failure. A returned
// error is a failure of the guard itself (the store could not be read): the
// caller treats it as a denial.
func (s *Service) preflight(ctx context.Context, scope contracts.Scope, prof providers.ProviderProfile, role providers.Role) (preflight, error) {
	var pf preflight

	// 1. The global kill switch, read from the store on every call. Nothing else
	// is consulted when it is engaged, and a store that cannot be read counts as engaged.
	on, err := s.killSwitchOn(ctx, scope.DeploymentID)
	if err != nil {
		return pf, err
	}
	if on {
		pf.fail(providers.EgressDecisionCheckNameKillSwitch, providers.EgressDecisionReasonKillSwitch, "global_external_disable_on")
		return pf, nil
	}
	pf.pass(providers.EgressDecisionCheckNameKillSwitch)

	// 2. External processing is enabled at every level; a level with no policy is disabled.
	ch, err := s.loadChain(ctx, scope)
	if err != nil {
		return pf, err
	}
	pf.chain = ch
	for i, p := range ch.all() {
		lr := providers.EgressDecisionLevelResult{Level: levelNames[i]}
		if p != nil {
			id, mode := p.PolicyID, p.SourceMode
			lr.PolicyID, lr.Enabled, lr.SourceMode = &id, p.Enabled, &mode
		}
		pf.levels = append(pf.levels, lr)
	}
	for i, p := range ch.all() {
		switch {
		case p == nil:
			pf.fail(providers.EgressDecisionCheckNameExternalEnabled, providers.EgressDecisionReasonExternalDisabled, "no_policy_"+string(levelNames[i]))
			return pf, nil
		case !p.Enabled:
			pf.fail(providers.EgressDecisionCheckNameExternalEnabled, providers.EgressDecisionReasonExternalDisabled, "policy_disabled_"+string(levelNames[i]))
			return pf, nil
		}
	}
	pf.pass(providers.EgressDecisionCheckNameExternalEnabled)

	// 3. The profile is qualified: a candidate, quarantined or retired profile
	// is never sent to.
	if prof.Lifecycle != providers.ProfileLifecycleQualified {
		pf.fail(providers.EgressDecisionCheckNameProfileActive, providers.EgressDecisionReasonProfileNotActive, "profile_not_qualified")
		return pf, nil
	}
	pf.pass(providers.EgressDecisionCheckNameProfileActive)

	// 4. The profile may serve this role and this application, tenant and workspace.
	switch {
	case !containsRole(prof.Roles, role):
		pf.fail(providers.EgressDecisionCheckNameProfilePermitted, providers.EgressDecisionReasonProfileNotPermitted, "role_not_permitted")
		return pf, nil
	case !scopePermitted(prof.PermittedScopes, scope):
		pf.fail(providers.EgressDecisionCheckNameProfilePermitted, providers.EgressDecisionReasonProfileNotPermitted, "scope_not_permitted")
		return pf, nil
	}
	pf.pass(providers.EgressDecisionCheckNameProfilePermitted)

	// 5 and 6. Every level allows the provider, and the provider and model pair. The most
	// restrictive level wins; an empty list allows nothing.
	for i, p := range ch.all() {
		if !containsProvider(p.AllowedProviders, prof.ProviderID) {
			pf.fail(providers.EgressDecisionCheckNameProviderAllowlist, providers.EgressDecisionReasonProviderNotAllowed, "provider_not_listed_"+string(levelNames[i]))
			return pf, nil
		}
	}
	pf.pass(providers.EgressDecisionCheckNameProviderAllowlist)
	for i, p := range ch.all() {
		if !containsModel(p.AllowedModels, providers.ModelRef{ProviderID: prof.ProviderID, ModelID: prof.ModelID}) {
			pf.fail(providers.EgressDecisionCheckNameModelAllowlist, providers.EgressDecisionReasonModelNotAllowed, "model_not_listed_"+string(levelNames[i]))
			return pf, nil
		}
	}
	pf.pass(providers.EgressDecisionCheckNameModelAllowlist)

	// 7. Region and residency: an unknown residency is denied, and every region the
	// provider processes in must be listed at every level.
	for i, p := range ch.all() {
		switch regionVerdict(prof.Region, p.AllowedRegions) {
		case regionUnknown:
			pf.fail(providers.EgressDecisionCheckNameRegion, providers.EgressDecisionReasonRegionUnknown, "residency_unknown")
			return pf, nil
		case regionDenied:
			pf.fail(providers.EgressDecisionCheckNameRegion, providers.EgressDecisionReasonRegionNotAllowed, "region_not_listed_"+string(levelNames[i]))
			return pf, nil
		}
	}
	pf.pass(providers.EgressDecisionCheckNameRegion)
	return pf, nil
}

// Evaluate is the deterministic gate in front of every external request (additive
// plan section 6.2): it checks the kill switch, the policy chain, the profile, the
// allowlists, the region, the classification of every part and the source modes,
// removes secrets, local-only fields, filesystem paths, database handles and MCP URLs,
// builds the bounded envelope, records the decision in the audit log and only then
// returns the envelope (step 8: send only after the egress policy passes).
//
// A denial is an error with code permission_denied whose Details name the rule
// ("rule", "check", "reason_code", "decision_id") and never carry content. The Result
// then still holds the Decision and a local-only Disclosure, and no envelope. A
// failure of the guard itself (the store or the audit log) is a denial too: nothing
// leaves unless the whole decision is recorded. No model takes part, and nothing in the
// candidate's text can change an outcome.
func (s *Service) Evaluate(ctx context.Context, c Candidate) (Result, error) {
	if err := validateCandidate(c); err != nil {
		return Result{}, err
	}
	now := s.clk.Now().UTC()
	r := &run{s: s, c: c, scope: c.Scope, prof: c.Profile, now: now, sources: map[providers.SourceID]*srcInfo{}}
	r.d = providers.EgressDecision{
		DecisionID: providers.EgressDecisionID(s.ids.New(prefixDecision)),
		DecidedAt:  timestamp(now),
		Subject:    subjectOf(c.Scope),
		Provider:   providerRef(c.Profile),
		Role:       c.Role,
		EnvelopeID: providers.EnvelopeID(s.ids.New(prefixEnvelope)),
		Outcome:    providers.EgressDecisionOutcomeDeny, // the zero decision is a deny
		Reason:     providers.EgressDecisionReasonExternalDisabled,
		Levels:     []providers.EgressDecisionLevelResult{},
		Checks:     []providers.EgressDecisionCheck{},
		Categories: providers.EgressDecisionCategories{Sent: []providers.SendCategory{}, Withheld: []providers.SendCategory{}, Stripped: []providers.BlockedCategory{}},
		Sources:    []providers.EgressDecisionSourceDecision{},
	}

	den, env, err := r.execute(ctx)
	if err != nil {
		return Result{}, guardFailure(err)
	}
	if den == nil {
		r.d.Outcome, r.d.Reason = providers.EgressDecisionOutcomeAllow, providers.EgressDecisionReasonAllowed
	} else {
		code := string(contracts.ErrPermissionDenied)
		r.d.Outcome, r.d.Reason, r.d.Code = providers.EgressDecisionOutcomeDeny, den.reason, &code
		r.d.Categories.Sent = []providers.SendCategory{}
		r.d.RAGExcerptsIncluded = false
		env = nil
	}

	// Step 7: record the decision before anything is handed back.
	var auditErr error
	if loc, err := audit.ForScope(c.Scope); err != nil {
		auditErr = err
	} else if _, err := EventDecision.Append(ctx, s.audit, loc, r.d); err != nil {
		auditErr = err
	}
	if den == nil && auditErr != nil {
		return Result{}, guardFailure(contracts.Wrap(contracts.ErrUnavailable, auditErr, "the egress decision could not be recorded"))
	}

	res := Result{Decision: r.d, Envelope: env, Parts: r.parts}
	if den != nil {
		res.Disclosure = s.disclosureStaysLocal(c.Profile, c.Role)
		e := contracts.New(contracts.ErrPermissionDenied, "egress denied: "+string(den.reason)).
			WithDetails("rule", string(den.reason)).WithDetails("check", string(den.check)).
			WithDetails("reason_code", den.code).WithDetails("decision_id", string(r.d.DecisionID)).
			WithDetails("audit_recorded", auditErr == nil)
		return res, e
	}
	res.Disclosure = r.disclosure(env)
	return res, nil
}

// guardFailure turns a failure of the guard into a denial that wraps its cause.
// It is permission_denied, not unavailable: an egress that could not be decided is a
// denied egress, and an "unavailable" would invite a caller to try a named fallback.
func guardFailure(cause error) error {
	return contracts.Wrap(contracts.ErrPermissionDenied, cause, "egress denied: the egress guard could not complete").
		WithDetails("rule", "guard_failure")
}

// run is one evaluation.
type run struct {
	s     *Service
	c     Candidate
	scope contracts.Scope
	prof  providers.ProviderProfile
	now   time.Time
	d     providers.EgressDecision

	chain   chain
	sources map[providers.SourceID]*srcInfo
	order   []providers.SourceID
	parts   []PartOutcome
	// contentClasses are the classes of the kept parts that no source policy covers
	// and a content setting of the chain let through (file excerpts, audio), for the disclosure.
	contentClasses []providers.EgressEligibleClass
}

// srcInfo is what the guard knows of one RAG source of the request.
type srcInfo struct {
	id     providers.SourceID
	policy *providers.EgressPolicy
	// mode is the effective mode: the most restrictive along the chain and the source policy.
	mode    providers.EgressMode
	why     string
	outcome providers.EgressDecisionSourceDecisionOutcome
}

func (r *run) execute(ctx context.Context) (*denial, *providers.RequestEnvelope, error) {
	pf, err := r.s.preflight(ctx, r.scope, r.prof, r.c.Role)
	if err != nil {
		return nil, nil, err
	}
	r.d.Levels, r.d.Checks, r.chain = pf.levels, pf.checks, pf.chain
	if pf.denial != nil {
		return pf.denial, nil, nil
	}
	fail := func(name providers.EgressDecisionCheckName, reason providers.EgressDecisionReason, code string) (*denial, *providers.RequestEnvelope, error) {
		r.d.Checks = append(r.d.Checks, providers.EgressDecisionCheck{Name: name, Passed: false, Reason: providers.ReasonCode(code)})
		return &denial{check: name, reason: reason, code: code}, nil, nil
	}

	if err := r.resolveSourceIDs(ctx, collectSourceIDs(r.c)); err != nil {
		return nil, nil, err
	}
	b := newBuilder(r)
	b.processParts()
	b.finishRecord(r)

	// 8. Classification of every part.
	if b.classFail != 0 {
		return fail(providers.EgressDecisionCheckNameClassification, b.classReason, b.classCode)
	}
	r.d.Checks = append(r.d.Checks, providers.EgressDecisionCheck{Name: providers.EgressDecisionCheckNameClassification, Passed: true, Reason: "passed"})
	// 8a. An excerpt of an uploaded file leaves only under the files setting of every level. The check is
	// recorded only for a request that carries one, so every other decision is unchanged.
	if b.files {
		if b.filesFail != "" {
			return fail(providers.EgressDecisionCheckNameFiles, providers.EgressDecisionReasonClassNotAllowed, b.filesFail)
		}
		r.d.Checks = append(r.d.Checks, providers.EgressDecisionCheck{Name: providers.EgressDecisionCheckNameFiles, Passed: true, Reason: "passed"})
	}
	// 8b. Raw audio leaves only under the audio setting of every level, recorded only for a request that carries audio.
	if b.audioSeen {
		if b.audioFail != "" {
			return fail(providers.EgressDecisionCheckNameAudio, providers.EgressDecisionReasonClassNotAllowed, b.audioFail)
		}
		r.d.Checks = append(r.d.Checks, providers.EgressDecisionCheck{Name: providers.EgressDecisionCheckNameAudio, Passed: true, Reason: "passed"})
	}
	// 9. A source the request requires may not be kept out by its mode.
	if b.srcFail != nil {
		return fail(providers.EgressDecisionCheckNameSourceMode, b.srcFail.reason, b.srcFail.code)
	}
	r.d.Checks = append(r.d.Checks, providers.EgressDecisionCheck{Name: providers.EgressDecisionCheckNameSourceMode, Passed: true, Reason: "passed"})
	// 10. The envelope is bounded and well formed.
	env, code := b.assemble()
	b.finishRecord(r)
	if code != "" {
		return fail(providers.EgressDecisionCheckNameEnvelope, providers.EgressDecisionReasonEnvelopeInvalid, code)
	}
	r.d.Checks = append(r.d.Checks, providers.EgressDecisionCheck{Name: providers.EgressDecisionCheckNameEnvelope, Passed: true, Reason: "passed"})
	return nil, env, nil
}

// collectSourceIDs lists the distinct, well-formed RAG source ids a candidate names,
// in order of first appearance. A malformed id is not listed; the part that carries it
// makes the envelope invalid.
func collectSourceIDs(c Candidate) []providers.SourceID {
	var out []providers.SourceID
	seen := map[providers.SourceID]bool{}
	for _, m := range c.Messages {
		for _, p := range m.Parts {
			id := providers.SourceID(p.SourceID)
			if sourceIDRE.MatchString(p.SourceID) && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// resolveSourceIDs loads the source policy of every listed RAG source and computes its
// effective mode: the most restrictive of the ceilings along the policy chain and the
// source's own policy. A source with no policy is local_only (the default of every
// source), and so is a source whose own policy is disabled or does not allow this
// provider, model and region.
func (r *run) resolveSourceIDs(ctx context.Context, ids []providers.SourceID) error {
	for _, id := range ids {
		if r.sources[id] != nil {
			continue
		}
		pol, err := r.s.loadSource(ctx, r.scope, id)
		if err != nil {
			return err
		}
		mode := r.chainCeiling()
		info := &srcInfo{id: id, policy: pol, outcome: providers.EgressDecisionSourceDecisionOutcomeExcluded}
		if pol == nil {
			mode = moreRestrictiveMode(mode, providers.EgressModeLocalOnly)
			info.why = "source_local_only"
		} else {
			mode = moreRestrictiveMode(mode, pol.SourceMode)
			if !pol.Enabled {
				mode = moreRestrictiveMode(mode, providers.EgressModeLocalOnly)
			}
			if !containsProvider(pol.AllowedProviders, r.prof.ProviderID) ||
				!containsModel(pol.AllowedModels, providers.ModelRef{ProviderID: r.prof.ProviderID, ModelID: r.prof.ModelID}) ||
				regionVerdict(r.prof.Region, pol.AllowedRegions) != regionOK {
				mode = moreRestrictiveMode(mode, providers.EgressModeLocalOnly)
				info.why = "source_provider_not_allowed"
			}
		}
		info.mode = mode
		r.sources[id] = info
		r.order = append(r.order, id)
	}
	return nil
}

// chainCeiling is the most permissive mode the four request levels allow a source:
// the most restrictive of their source modes.
func (r *run) chainCeiling() providers.EgressMode {
	mode := providers.EgressModeAllowExcerpt
	for _, p := range r.chain.all() {
		mode = moreRestrictiveMode(mode, p.SourceMode)
	}
	return mode
}

// finishRecord copies what the builder found into the decision. It may be called
// again after assemble.
func (b *builder) finishRecord(r *run) {
	r.d.Categories.Sent = sortedSend(b.sent)
	r.d.Categories.Withheld = sortedSend(b.withheld)
	r.d.Categories.Stripped = sortedBlocked(b.stripped)
	r.d.RAGExcerptsIncluded = b.sent[providers.SendCategoryRAGExcerpt]
	r.d.Sources = r.d.Sources[:0]
	for _, id := range r.order {
		info := r.sources[id]
		r.d.Sources = append(r.d.Sources, providers.EgressDecisionSourceDecision{SourceID: id, EffectiveMode: info.mode, Outcome: info.outcome})
	}
	r.parts = b.outcomes
}

// ---- candidate validation ----

func validateCandidate(c Candidate) error {
	bad := func(field string) error {
		return contracts.New(contracts.ErrValidationFailed, "dataegress: the candidate request is malformed").WithDetails("field", field)
	}
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	p := c.Profile
	if !profileIDRE.MatchString(string(p.ProfileID)) || !providerIDRE.MatchString(string(p.ProviderID)) ||
		!modelIDRE.MatchString(string(p.ModelID)) || !modelVersionRE.MatchString(string(p.ModelVersion)) {
		return bad("profile")
	}
	if !c.Role.Valid() {
		return bad("role")
	}
	if !c.Channel.Valid() {
		return bad("channel")
	}
	if !validIdentifier(c.TraceID) {
		return bad("trace_id")
	}
	if c.ReservationID != "" && !reservationIDRE.MatchString(c.ReservationID) {
		return bad("reservation_id")
	}
	return nil
}

func validIdentifier(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > identifierMax {
		return false
	}
	for _, r := range s {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func subjectOf(s contracts.Scope) providers.Subject {
	return providers.Subject{
		ApplicationID: common.Identifier(s.ApplicationID), TenantID: common.Identifier(s.TenantID), WorkspaceID: common.Identifier(s.WorkspaceID),
		PrincipalID: common.Identifier(s.PrincipalID), SessionID: common.Identifier(s.SessionID),
	}
}

func providerRef(p providers.ProviderProfile) providers.ProviderRef {
	return providers.ProviderRef{ProfileID: p.ProfileID, ProviderID: p.ProviderID, ModelID: p.ModelID, ModelVersion: p.ModelVersion}
}

// ---- list helpers ----

func containsRole(list []providers.Role, r providers.Role) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

func containsProvider(list []providers.ProviderID, p providers.ProviderID) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

func containsModel(list []providers.ModelRef, m providers.ModelRef) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

func containsRegion(list []providers.RegionCode, r providers.RegionCode) bool {
	for _, x := range list {
		if x == r {
			return true
		}
	}
	return false
}

// scopePermitted reports whether a grant of the profile covers the scope. A grant
// that omits a narrower level covers every value inside the named parent; there is
// no wildcard, and an empty list permits nobody. A grant that names a workspace
// without a tenant is malformed and covers nothing.
func scopePermitted(grants []providers.ProviderProfileScopeGrant, s contracts.Scope) bool {
	for _, g := range grants {
		if string(g.ApplicationID) != s.ApplicationID {
			continue
		}
		if g.TenantID == nil {
			if g.WorkspaceID != nil {
				continue
			}
			return true
		}
		if string(*g.TenantID) != s.TenantID {
			continue
		}
		if g.WorkspaceID == nil || string(*g.WorkspaceID) == s.WorkspaceID {
			return true
		}
	}
	return false
}

type regionResult int

const (
	regionOK regionResult = iota
	regionDenied
	regionUnknown
)

// regionVerdict checks a provider's residency against one allowlist of regions.
// An unknown residency is never allowed; a global residency is allowed only when
// "global" is listed; and every region the provider processes in must be listed.
func regionVerdict(reg providers.ProviderProfileRegion, allowed []providers.RegionCode) regionResult {
	switch reg.Residency {
	case providers.ResidencyPinned, providers.ResidencySelectable, providers.ResidencyGlobal:
	default: // unknown, empty or not a residency
		return regionUnknown
	}
	if len(reg.ProcessingRegions) == 0 {
		return regionUnknown
	}
	if reg.Residency == providers.ResidencyGlobal && !containsRegion(allowed, "global") {
		return regionDenied
	}
	for _, r := range reg.ProcessingRegions {
		if !containsRegion(allowed, r) {
			return regionDenied
		}
	}
	return regionOK
}

func modeRank(m providers.EgressMode) int {
	switch m {
	case providers.EgressModeAllowExcerpt:
		return 3
	case providers.EgressModeAllowMetadata:
		return 2
	case providers.EgressModeLocalOnly:
		return 1
	}
	return 0 // deny, and any value that is not a mode
}

func moreRestrictiveMode(a, b providers.EgressMode) providers.EgressMode {
	m := a
	if modeRank(b) < modeRank(a) {
		m = b
	}
	if modeRank(m) == 0 {
		return providers.EgressModeDeny
	}
	return m
}

var sendOrder = []providers.SendCategory{
	providers.SendCategoryUserMessage, providers.SendCategoryConversationHistory, providers.SendCategoryStructuredState,
	providers.SendCategoryDeterministicFact, providers.SendCategoryDialogueDirective, providers.SendCategoryActionReceipt,
	providers.SendCategoryRAGExcerpt, providers.SendCategoryRAGMetadata, providers.SendCategoryFileContent,
	providers.SendCategoryImageContent, providers.SendCategoryAudioContent, providers.SendCategoryWebQuery,
}

var blockedOrder = []providers.BlockedCategory{
	providers.BlockedCategoryCredential, providers.BlockedCategoryRawDatabaseContent, providers.BlockedCategoryFilesystemPath,
	providers.BlockedCategoryDatabaseHandle, providers.BlockedCategoryMCPURL, providers.BlockedCategoryLocalOnlyField,
}

func sortedSend(set map[providers.SendCategory]bool) []providers.SendCategory {
	out := []providers.SendCategory{}
	for _, c := range sendOrder {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}

func sortedBlocked(set map[providers.BlockedCategory]bool) []providers.BlockedCategory {
	out := []providers.BlockedCategory{}
	for _, c := range blockedOrder {
		if set[c] {
			out = append(out, c)
		}
	}
	return out
}
