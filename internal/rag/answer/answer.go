package answer

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// Deps are the collaborators of an Answerer. Generator and Catalog are required:
// without a generator there is nothing to ground, and without a catalog the final
// authorisation recheck, and the permission check of every reference, could not be
// made (an Answerer fails closed rather than skip them).
type Deps struct {
	// Generator is the text model that proposes the claims: the controller role
	// of internal/providers (registry.TextGenerator(providers.RoleController)),
	// wrapped by the composition in its budget. Its output is untrusted.
	Generator providers.TextGenerator
	// Catalog is the deterministic authority on sources (the same one the query
	// pipeline uses): it is asked again, just before citations are built, which
	// sources the principal may still read and in what state they are.
	Catalog query.SourceCatalog
	// Resolver issues and resolves citation references. Nil means a Resolver
	// over Catalog with the Answerer's configuration; pass one to share it with
	// the API that resolves references.
	Resolver *Resolver
	// Clock is the injected clock; nil means clock.Real.
	Clock clock.Clock
}

// Answerer turns an EvidencePackage into a grounded answer with citations, or into
// no_supported_evidence (master plan 12.4 "grounded generation", "claim/source
// verification", "answer with original-source references"; 12.5). It is safe for
// concurrent use and keeps no per-question state.
type Answerer struct {
	gen providers.TextGenerator
	cat query.SourceCatalog
	res *Resolver
	clk clock.Clock
	cfg Config
}

// New returns an Answerer. The configuration is validated: nothing is clamped.
func New(d Deps, cfg Config) (*Answerer, error) {
	if d.Generator == nil {
		return nil, contracts.New(contracts.ErrInternal, "answer: no text generator")
	}
	if d.Catalog == nil {
		return nil, contracts.New(contracts.ErrInternal, "answer: no source catalog: authorisation cannot be established")
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "answer: invalid configuration")
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	res := d.Resolver
	if res == nil {
		var err error
		if res, err = NewResolver(d.Catalog, clk, cfg); err != nil {
			return nil, err
		}
	}
	return &Answerer{gen: d.Generator, cat: d.Catalog, res: res, clk: clk, cfg: cfg}, nil
}

// Config returns the configuration the Answerer runs with.
func (a *Answerer) Config() Config { return a.cfg }

// Resolver returns the resolver of the references this Answerer issues.
func (a *Answerer) Resolver() *Resolver { return a.res }

// Result is the outcome of one question.
type Result struct {
	// Answer is a GroundedAnswer, or a NoSupportedEvidence result. There is no
	// third shape: no uncited answer, no empty answer.
	Answer rag.AnswerResult
	// Citations are the internal citation records, parallel to the answer's
	// citations (empty for NoSupportedEvidence). INTERNAL: they carry each source's
	// data-egress mode and classification and are never serialised to a client; the
	// answer's own citations are their client projection.
	Citations []rag.EvidenceCitation
	// Diagnostics describes how the answer was reached, for logs and metrics. It
	// holds no evidence text, no claim text and no model output.
	Diagnostics Diagnostics
}

// Diagnostics describes one answer.
type Diagnostics struct {
	// Items is the package's items; Eligible those shown to the model (a removed
	// source's item is not).
	Items, Eligible int
	// Generated is true when the model was called. An empty package and a package
	// of removed sources never reach it.
	Generated bool
	// Reason is why there is no answer, when there is none.
	Reason rag.NoSupportedEvidenceReason
	// Proposed is the claims the model wrote; Kept those that survived the claim
	// check and the final recheck.
	Proposed, Kept int
	// Dropped, by reason. None carries claim text.
	DroppedInvalid         int // empty, too long, hidden characters, uncited, or too thin to check
	DroppedUnknownEvidence int // cited a handle the package does not hold
	DroppedInstruction     int // reads as an instruction to the assistant
	DroppedUnsupported     int // no cited passage supports it
	DroppedIrrelevant      int // supported, but none of the claims has anything in common with the question
	DroppedDuplicate       int // the same claim twice
	DroppedOverCap         int // beyond the claim, citation or per-citation caps
	DroppedRevoked         int // every supporting source was revoked or removed at the final recheck
	// InstructionLikeItems counts evidence items whose text carries an injected
	// instruction. They are data like any other; the count is for the logs.
	InstructionLikeItems int
	// Citations is the citations of the answer.
	Citations int
	// TokIn and TokOut are the model's reported usage.
	TokIn, TokOut int
	// Durations of the stages. A stage that did not run is zero.
	Total, Generate, Verify, Recheck time.Duration
}

// checkPackage refuses a package that is not the caller's, or not what the query
// pipeline builds. A package is bound to the scope and principal it was authorised
// for and is never reused for another (schemas/rag/evidence-package): answering
// from one would put another tenant's text in this caller's prompt.
func checkPackage(scope contracts.Scope, pkg rag.EvidencePackage) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	ps := pkg.Scope
	if string(ps.ApplicationID) != scope.ApplicationID || string(ps.TenantID) != scope.TenantID ||
		string(ps.WorkspaceID) != scope.WorkspaceID || string(ps.PrincipalID) != scope.PrincipalID {
		return contracts.New(contracts.ErrScopeViolation, "answer: the evidence package was built for another scope")
	}
	if strings.TrimSpace(string(pkg.Query)) == "" {
		return contracts.New(contracts.ErrValidationFailed, "answer: the evidence package has no question")
	}
	for i, it := range pkg.Items {
		if string(it.EvidenceID) != "e"+strconv.Itoa(i+1) || it.Rank != int64(i+1) {
			return contracts.New(contracts.ErrValidationFailed, "answer: the evidence package is malformed: handles must run e1, e2, ... in rank order")
		}
	}
	return nil
}

// Answer answers the package's question from the package's evidence, and from
// nothing else.
//
//  1. The package must be the caller's (checkPackage). An empty package, or one
//     whose sources are all removed, ends here as no_supported_evidence: the model is
//     not asked, so it cannot answer from what it knows.
//  2. The model is asked for claims, each citing the handles of the passages that
//     state it, under a constrained schema. Passages and question are fenced data
//     (prompt.go). A failed or unreadable generation is an error, never an answer
//     and never "no evidence".
//  3. Each claim is checked, by code, against the passages it cites (support.go):
//     a handle the package does not hold, an instruction, a claim with no support
//     in any cited passage is dropped. A claim survives with the citations that do
//     support it, and only those.
//     When none of the surviving claims has a term, number or identifier in common
//     with the question, the outcome is no_supported_evidence too (relevance.go).
//  4. The source catalog is read again. A claim whose every supporting source has
//     been revoked or removed since retrieval is dropped; each remaining citation
//     carries the source's availability and staleness as the more cautious of the
//     package's and the catalog's.
//  5. Citations are built by the core, with a permission-checked, opaque, expiring
//     open/preview reference each (resolver.go). With no claim left the result is
//     no_supported_evidence, with the reason.
//
// The scope is the caller's and is passed to the model call and to every catalog
// read. Retrieval notices of the package (reduced_semantic_recall, ...) are carried
// into both outcomes.
func (a *Answerer) Answer(ctx context.Context, scope contracts.Scope, pkg rag.EvidencePackage) (Result, error) {
	started := a.clk.Now()
	if err := checkPackage(scope, pkg); err != nil {
		return Result{}, err
	}
	var diag Diagnostics
	diag.Items = len(pkg.Items)
	notices := append(rag.RetrievalNotices{}, pkg.Retrieval.Notices...)
	finish := func(res Result) Result {
		res.Diagnostics = diag
		res.Diagnostics.Total = a.clk.Now().Sub(started)
		return res
	}
	none := func(reason rag.NoSupportedEvidenceReason) Result {
		diag.Reason = reason
		return finish(Result{Answer: rag.AnswerResult{Value: rag.NoSupportedEvidence{
			Outcome: "no_supported_evidence", Reason: reason, Notices: notices,
		}}})
	}

	// 1. What the model may see.
	var items []rag.EvidenceItem
	var texts []string
	handles := map[string]int{}
	for _, it := range pkg.Items {
		if it.Availability == conversation.CitationAvailabilityRemoved {
			continue // a removed source is never shown, never cited
		}
		text := sanitize(it.Text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if looksLikeInstruction(text) {
			diag.InstructionLikeItems++
		}
		handles[string(it.EvidenceID)] = len(items)
		items = append(items, it)
		texts = append(texts, text)
	}
	diag.Eligible = len(items)
	if len(pkg.Items) == 0 {
		return none(rag.NoSupportedEvidenceReasonNoMatch), nil
	}
	if len(items) == 0 {
		return none(rag.NoSupportedEvidenceReasonEvidenceUnavailable), nil
	}

	// 2. Grounded generation.
	question := clipRunes(string(pkg.Query), 2000)
	req := buildPrompt(question, items, texts, a.cfg)
	gctx, cancel := context.WithTimeout(ctx, a.cfg.GenerateTimeout)
	defer cancel()
	genStart := a.clk.Now()
	out, err := a.gen.Chat(gctx, scope, req)
	diag.Generated = true
	diag.Generate = a.clk.Now().Sub(genStart)
	if err != nil {
		return Result{}, generationError(err)
	}
	diag.TokIn, diag.TokOut = out.Usage.InputTokens, out.Usage.OutputTokens
	if out.FinishReason == providers.FinishLength {
		return Result{}, contracts.New(contracts.ErrSchemaInvalid, "answer: model output was cut at the token limit")
	}
	raw := []byte(out.Text)
	if len(out.JSON) > 0 {
		raw = out.JSON
	}
	proposed, err := parseOutput(raw)
	if err != nil {
		return Result{}, err
	}
	diag.Proposed = len(proposed)
	if len(proposed) == 0 {
		return none(rag.NoSupportedEvidenceReasonNoMatch), nil // the model found nothing to say
	}

	// 3. Claim/source verification, by code.
	verifyStart := a.clk.Now()
	kept := a.verifyClaims(proposed, texts, handles, &diag)
	diag.Verify = a.clk.Now().Sub(verifyStart)
	if len(kept) == 0 {
		return none(rag.NoSupportedEvidenceReasonClaimsUnsupported), nil
	}
	// Supported is not answered: claims that say nothing about what was asked (the
	// renewals passage, retrieved for "capital of France") are not an answer.
	if !answersQuestion(question, kept) {
		diag.DroppedIrrelevant = len(kept)
		return none(rag.NoSupportedEvidenceReasonClaimsUnsupported), nil
	}

	// 4. Final recheck of the sources behind the claims.
	recheckStart := a.clk.Now()
	states, err := a.recheck(ctx, scope, items, kept)
	diag.Recheck = a.clk.Now().Sub(recheckStart)
	if err != nil {
		return Result{}, err
	}
	var live []keptClaim
	for _, k := range kept {
		var sup []support
		for _, s := range k.support {
			if st := states[string(items[s.item].SourceID)]; st.usable {
				sup = append(sup, s)
			}
		}
		if len(sup) == 0 {
			diag.DroppedRevoked++
			continue
		}
		k.support = sup
		live = append(live, k)
	}
	if len(live) == 0 {
		return none(rag.NoSupportedEvidenceReasonEvidenceUnavailable), nil
	}

	// 5. Citations, written by the core.
	b := newBuilder(a.cfg, items, states)
	for _, k := range live {
		if !b.add(k) {
			diag.DroppedOverCap++
		}
	}
	if len(b.claims) == 0 {
		return none(rag.NoSupportedEvidenceReasonClaimsUnsupported), nil
	}
	internal := make([]rag.EvidenceCitation, len(b.cites))
	clients := make([]conversation.Citation, len(b.cites))
	for i, c := range b.cites {
		ref, _, err := a.res.Issue(scope, RefSpec{
			SourceID: c.item.SourceID, Version: c.item.Version, Locator: c.item.Locator, Preview: c.preview,
		})
		if err != nil {
			return Result{}, err
		}
		internal[i] = rag.EvidenceCitation{
			SourceID: c.item.SourceID, DisplayName: c.name, Version: c.item.Version,
			Locator: cloneLocator(c.item.Locator), Availability: c.availability, Staleness: c.staleness,
			OpenRef: ref, Claims: c.claims,
			SourceEgress: c.item.SourceEgress, SourceClassification: c.item.SourceClassification,
		}
		clients[i] = internal[i].ClientCitation()
	}
	ga := rag.GroundedAnswer{Outcome: "answered", Claims: b.claims, Citations: clients, Notices: notices}
	if err := checkAnswer(ga); err != nil {
		return Result{}, err // a bug in this package, never a malformed answer on the wire
	}
	diag.Kept = len(ga.Claims)
	diag.Citations = len(ga.Citations)
	return finish(Result{Answer: rag.AnswerResult{Value: ga}, Citations: internal}), nil
}

// generationError types a failed model call. A deadline is a timeout, the
// caller's cancellation stays a cancellation, a typed error keeps its code and
// anything else is unavailable; none of them is an answer.
func generationError(err error) error {
	err = providers.ContextError(err)
	var ce *contracts.Error
	switch {
	case errors.As(err, &ce):
		return contracts.Wrap(ce.Code, err, "answer: grounded generation failed")
	case errors.Is(err, context.Canceled):
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "answer: grounded generation failed")
}

// support is one cited passage that supports a claim: the item (an index into the
// items shown to the model) and the window of its sentences that does.
type support struct {
	item int
	span string
}

// keptClaim is a claim that passed verification and what supports it.
type keptClaim struct {
	text    string
	terms   analysis
	support []support
}

// verifyClaims is step 3: it returns the claims that survive, in the model's order.
func (a *Answerer) verifyClaims(proposed []modelClaim, texts []string, handles map[string]int, diag *Diagnostics) []keptClaim {
	passages := make([]*passage, len(texts))
	passageOf := func(i int) *passage {
		if passages[i] == nil {
			passages[i] = newPassage(texts[i])
		}
		return passages[i]
	}
	seen := map[string]struct{}{}
	var kept []keptClaim
	for n, mc := range proposed {
		if n >= a.cfg.MaxClaims {
			diag.DroppedOverCap += len(proposed) - n
			break
		}
		text, ok := cleanClaim(mc.Text)
		if !ok || utf8.RuneCountInString(text) > MaxClaimRunes || len(mc.EvidenceIDs) == 0 || len(mc.EvidenceIDs) > maxEvidencePerClaim {
			diag.DroppedInvalid++
			continue
		}
		if looksLikeInstruction(text) {
			diag.DroppedInstruction++
			continue
		}
		key := strings.Join(strings.Fields(fold(text)), " ")
		if _, dup := seen[key]; dup {
			diag.DroppedDuplicate++
			continue
		}
		// Every cited handle must be one the package gave the model: a claim that
		// names another is tainted, not trimmed.
		var cited []int
		unknown := false
		for _, h := range mc.EvidenceIDs {
			i, ok := handles[h]
			if !ok {
				unknown = true
				break
			}
			if !containsInt(cited, i) {
				cited = append(cited, i)
			}
		}
		if unknown {
			diag.DroppedUnknownEvidence++
			continue
		}
		cc, ok := newClaimCheck(text, a.cfg)
		if !ok {
			diag.DroppedInvalid++
			continue
		}
		var sup []support
		for _, i := range cited {
			if span, ok := passageOf(i).support(cc, a.cfg.WindowSentences); ok {
				sup = append(sup, support{item: i, span: span})
			}
		}
		if len(sup) == 0 {
			diag.DroppedUnsupported++
			continue
		}
		seen[key] = struct{}{}
		kept = append(kept, keptClaim{text: text, terms: cc.a, support: sup})
	}
	return kept
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// sourceState is the state of one source at the final recheck, merged with the
// package's.
type sourceState struct {
	usable       bool
	name         string
	availability conversation.CitationAvailability
	staleness    conversation.CitationStaleness
}

// recheck is step 4: it reads the catalog again for the sources behind the kept
// claims. A source the catalog does not return, or does not authorise, or marks
// removed, is not usable. The state of a usable one is the more cautious of the
// package's and the catalog's: removed over unavailable over available, stale over
// unknown over current.
func (a *Answerer) recheck(ctx context.Context, scope contracts.Scope, items []rag.EvidenceItem, kept []keptClaim) (map[string]sourceState, error) {
	var ids []string
	seen := map[string]struct{}{}
	for _, k := range kept {
		for _, s := range k.support {
			id := string(items[s.item].SourceID)
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	fresh, err := a.cat.Sources(ctx, scope, ids)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "answer: source catalog unavailable: the final authorisation recheck was not made")
	}
	out := make(map[string]sourceState, len(ids))
	for _, k := range kept {
		for _, s := range k.support {
			it := items[s.item]
			id := string(it.SourceID)
			if _, done := out[id]; done {
				continue
			}
			st, ok := fresh[id]
			if !ok || !st.Authorised || st.Availability == conversation.CitationAvailabilityRemoved {
				out[id] = sourceState{}
				continue
			}
			name := st.DisplayName
			if name == "" {
				name = it.DisplayName
			}
			out[id] = sourceState{
				usable:       true,
				name:         name,
				availability: worseAvailability(availabilityOf(it.Availability), availabilityOf(st.Availability)),
				staleness:    worseStaleness(stalenessOf(it.Staleness), stalenessOf(st.Staleness)),
			}
		}
	}
	return out, nil
}

func worseAvailability(a, b conversation.CitationAvailability) conversation.CitationAvailability {
	rank := func(v conversation.CitationAvailability) int {
		switch v {
		case conversation.CitationAvailabilityAvailable:
			return 0
		case conversation.CitationAvailabilityUnavailable:
			return 1
		}
		return 2
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

func worseStaleness(a, b conversation.CitationStaleness) conversation.CitationStaleness {
	rank := func(v conversation.CitationStaleness) int {
		switch v {
		case conversation.CitationStalenessCurrent:
			return 0
		case conversation.CitationStalenessUnknown:
			return 1
		}
		return 2
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// citation is a citation under construction: one per distinct (source, version,
// locator).
type citation struct {
	item         rag.EvidenceItem
	name         string
	availability conversation.CitationAvailability
	staleness    conversation.CitationStaleness
	claims       []string
	preview      string
}

// builder assembles claims and citations within the schema's caps.
type builder struct {
	cfg    Config
	items  []rag.EvidenceItem
	states map[string]sourceState

	cites  []*citation
	byKey  map[string]int
	claims []rag.GroundedAnswerClaim
}

func newBuilder(cfg Config, items []rag.EvidenceItem, states map[string]sourceState) *builder {
	return &builder{cfg: cfg, items: items, states: states, byKey: map[string]int{}}
}

// citeKey identifies what a citation points at: the same source, version and place
// is one citation, however many claims it supports.
func citeKey(it rag.EvidenceItem) string {
	k := string(it.SourceID) + "\x00" + string(it.Version)
	if l := it.Locator; l != nil {
		if l.Page != nil {
			k += "\x00p" + strconv.FormatInt(*l.Page, 10)
		}
		if l.Section != nil {
			k += "\x00s" + *l.Section
		}
		if l.Record != nil {
			k += "\x00r" + string(*l.Record)
		}
		if l.Span != nil {
			k += "\x00x" + strconv.FormatInt(l.Span.Start, 10) + "-" + strconv.FormatInt(l.Span.End, 10)
		}
	}
	return k
}

// add appends the claim with its citations, or reports false, changing nothing,
// when it would break a cap: the claims of the answer, the citations of the answer,
// or the claims one citation may list.
func (b *builder) add(k keptClaim) bool {
	if len(b.claims) >= b.cfg.MaxClaims {
		return false
	}
	type target struct {
		key  string
		item int
		span string
	}
	var targets []target
	have := map[string]struct{}{}
	fresh := 0
	for _, s := range k.support {
		key := citeKey(b.items[s.item])
		if _, dup := have[key]; dup {
			continue
		}
		have[key] = struct{}{}
		targets = append(targets, target{key: key, item: s.item, span: s.span})
		if idx, ok := b.byKey[key]; ok {
			if len(b.cites[idx].claims) >= MaxCitationClaims {
				return false
			}
		} else {
			fresh++
		}
	}
	if len(b.cites)+fresh > b.cfg.MaxCitations || len(targets) > MaxClaimCitations {
		return false
	}
	indexes := make([]int64, 0, len(targets))
	for _, t := range targets {
		idx, ok := b.byKey[t.key]
		if !ok {
			it := b.items[t.item]
			st := b.states[string(it.SourceID)]
			name := clipRunes(st.name, 256)
			if strings.TrimSpace(name) == "" {
				name = string(it.SourceID)
			}
			idx = len(b.cites)
			b.cites = append(b.cites, &citation{
				item: it, name: name, availability: st.availability, staleness: st.staleness, preview: t.span,
			})
			b.byKey[t.key] = idx
		}
		b.cites[idx].claims = append(b.cites[idx].claims, k.text)
		indexes = append(indexes, int64(idx))
	}
	b.claims = append(b.claims, rag.GroundedAnswerClaim{Text: k.text, CitationIndexes: indexes})
	return true
}

// checkAnswer verifies what the schema cannot express and the answer's provenance
// promise: every claim names at least one existing citation, no index repeats, and
// every claim's text is among the claims of every citation it names; every
// citation lists at least one claim and has a valid state and a reference. It is
// the last gate before an answer leaves the package.
func checkAnswer(ga rag.GroundedAnswer) error {
	bad := func(msg string) error {
		return contracts.New(contracts.ErrInternal, "answer: built an answer that breaks its own invariants: "+msg)
	}
	if len(ga.Claims) == 0 || len(ga.Claims) > MaxClaimsCeiling {
		return bad("claim count")
	}
	if len(ga.Citations) == 0 || len(ga.Citations) > MaxCitationsCeiling {
		return bad("citation count")
	}
	for _, c := range ga.Citations {
		if len(c.Claims) == 0 || len(c.Claims) > MaxCitationClaims {
			return bad("citation claim count")
		}
		if !c.Availability.Valid() || !c.Staleness.Valid() || c.OpenRef.Value == nil || c.SourceID == "" || c.Version == "" {
			return bad("citation fields")
		}
	}
	for _, cl := range ga.Claims {
		if len(cl.CitationIndexes) == 0 || len(cl.CitationIndexes) > MaxClaimCitations {
			return bad("claim without citation")
		}
		seen := map[int64]struct{}{}
		for _, ix := range cl.CitationIndexes {
			if ix < 0 || ix >= int64(len(ga.Citations)) {
				return bad("citation index")
			}
			if _, dup := seen[ix]; dup {
				return bad("repeated citation index")
			}
			seen[ix] = struct{}{}
			found := false
			for _, t := range ga.Citations[ix].Claims {
				if t == cl.Text {
					found = true
					break
				}
			}
			if !found {
				return bad("claim not listed by its citation")
			}
		}
	}
	return nil
}
