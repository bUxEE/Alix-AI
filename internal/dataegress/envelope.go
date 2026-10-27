package dataegress

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Bounds of schemas/providers/request-envelope.
const (
	maxInstructionChars = 32768
	maxMessages         = 64
	maxPartsPerMessage  = 32
	maxTextChars        = 65536
	maxTools            = 32
	maxToolDescChars    = 1024
	maxOpenMembers      = 128
	maxPayloadBytes     = 1 << 20
	maxOutputToks       = 262144
	maxTimeoutMS        = 600000
)

// Severity of a classification failure. The worst one names the denial.
const (
	failClassNotAllowed = 1
	failUnclassified    = 2
	failRestricted      = 3
)

type keptPart struct {
	value providers.RequestEnvelopePart
	cat   providers.SendCategory
}

// builder implements steps 4 to 6 of additive plan section 6.2 for one request:
// it removes secrets and local-only fields, drops what policy keeps out, attaches
// the source and classification metadata and builds the bounded envelope.
type builder struct {
	r *run
	s *Service
	c Candidate

	stripped map[providers.BlockedCategory]bool
	withheld map[providers.SendCategory]bool
	sent     map[providers.SendCategory]bool

	classFail   int
	classReason providers.EgressDecisionReason
	classCode   string
	srcFail     *denial
	envFail     string

	// files is set when the request carries an excerpt of an uploaded file, and filesFail
	// is the reason code of the first one the files setting keeps out.
	files     bool
	filesFail string
	// audioSeen and audioFail are the same for raw audio and the audio setting.
	audioSeen bool
	audioFail string

	evidence int
	outcomes []PartOutcome
	msgs     []providers.RequestEnvelopeMessage
}

func newBuilder(r *run) *builder {
	return &builder{
		r: r, s: r.s, c: r.c,
		stripped: map[providers.BlockedCategory]bool{}, withheld: map[providers.SendCategory]bool{}, sent: map[providers.SendCategory]bool{},
	}
}

func (b *builder) failEnvelope(code string) {
	if b.envFail == "" {
		b.envFail = code
	}
}

func (b *builder) failClass(severity int, reason providers.EgressDecisionReason, code string) {
	if severity > b.classFail {
		b.classFail, b.classReason, b.classCode = severity, reason, code
	}
}

func (b *builder) note(found []providers.BlockedCategory) bool {
	for _, c := range found {
		b.stripped[c] = true
	}
	return len(found) > 0
}

// processParts walks every candidate part once. It never stops early: a request
// that is going to be denied still reports everything found, so the decision
// lists every category that was stripped or withheld.
func (b *builder) processParts() {
	for mi, m := range b.c.Messages {
		if !m.Role.Valid() {
			b.failEnvelope("bad_message_role")
		}
		var parts []providers.RequestEnvelopePart
		for pi, p := range m.Parts {
			oc := PartOutcome{Message: mi, Part: pi, EnvelopeMessage: -1, EnvelopePart: -1}
			kept, why := b.one(p)
			if kept != nil {
				oc.Kept, oc.EnvelopeMessage, oc.EnvelopePart = true, len(b.msgs), len(parts)
				parts = append(parts, kept.value)
				b.sent[kept.cat] = true
			}
			oc.Why = why
			b.outcomes = append(b.outcomes, oc)
		}
		if len(parts) > maxPartsPerMessage {
			b.failEnvelope("too_many_parts")
		}
		if len(parts) > 0 {
			b.msgs = append(b.msgs, providers.RequestEnvelopeMessage{Role: m.Role, Parts: parts})
		}
	}
}

// one decides what becomes of a part. A part is removed (never sent) when its
// category is a blocked one, when it is marked local-only, or when policy keeps it out;
// it makes the whole request fail when it is restricted, unclassified or of a class that
// is not allowed to leave.
func (b *builder) one(p Part) (*keptPart, string) {
	o := b.r.scope
	if p.Origin != nil && *p.Origin != (Origin{ApplicationID: o.ApplicationID, TenantID: o.TenantID, WorkspaceID: o.WorkspaceID}) {
		b.failEnvelope("part_scope_mismatch")
		return nil, "part_scope_mismatch"
	}
	if bc := providers.BlockedCategory(p.Category); bc.Valid() {
		b.stripped[bc] = true
		return nil, "blocked_category"
	}
	if p.LocalOnly {
		b.stripped[providers.BlockedCategoryLocalOnlyField] = true
		return nil, "local_only_field"
	}
	cat := providers.SendCategory(p.Category)
	if !cat.Valid() {
		b.failClass(failUnclassified, providers.EgressDecisionReasonUnclassifiedData, "unknown_category")
		return nil, "unknown_category"
	}
	if cat == providers.SendCategoryRAGExcerpt || cat == providers.SendCategoryRAGMetadata {
		return b.rag(p, cat)
	}
	cls, ok := parseClass(p.Class)
	switch {
	case !ok:
		b.failClass(failUnclassified, providers.EgressDecisionReasonUnclassifiedData, "unknown_class")
		return nil, "unknown_class"
	case cls == providers.DataClassificationRestricted:
		b.failClass(failRestricted, providers.EgressDecisionReasonRestrictedData, "restricted_part")
		return nil, "restricted_data"
	}
	el := providers.EgressEligibleClass(cls)
	if p.Kind == PartText && cat == providers.SendCategoryFileContent {
		return b.fileExcerpt(p, cat, el)
	}
	if p.Kind == PartAudio && cat == providers.SendCategoryAudioContent {
		return b.audio(p, cat, el)
	}
	if !b.r.classAllowed(el, nil) {
		b.failClass(failClassNotAllowed, providers.EgressDecisionReasonClassNotAllowed, "class_not_listed")
		return nil, "class_not_allowed"
	}
	switch p.Kind {
	case PartText:
		switch cat {
		case providers.SendCategoryUserMessage, providers.SendCategoryConversationHistory, providers.SendCategoryStructuredState,
			providers.SendCategoryDeterministicFact, providers.SendCategoryDialogueDirective, providers.SendCategoryActionReceipt,
			providers.SendCategoryWebQuery:
			return b.text(p.Text, cat, el, nil, nil)
		}
		b.failEnvelope("category_kind_mismatch")
		return nil, "category_kind_mismatch"
	case PartImage, PartFile, PartAudio:
		return b.media(p, cat, el)
	}
	b.failEnvelope("bad_part_kind")
	return nil, "bad_part_kind"
}

// text scrubs and places one text part.
func (b *builder) text(content string, cat providers.SendCategory, cls providers.EgressEligibleClass, src *providers.SourceID, mode *providers.EgressMode) (*keptPart, string) {
	s, found := scrub(content, true)
	why := ""
	if b.note(found) {
		why = "scrubbed"
	}
	if strings.TrimSpace(s) == "" {
		return nil, "empty_content"
	}
	if utf8.RuneCountInString(s) > maxTextChars {
		b.failEnvelope("part_too_large")
		return nil, "part_too_large"
	}
	meta := providers.RequestEnvelopePartMeta{Category: cat, Classification: cls, SourceID: src, SourceMode: mode, SizeBytes: providers.Counter(len(s))}
	return &keptPart{value: providers.RequestEnvelopePart{Value: providers.RequestEnvelopeTextPart{Type: "text", Text: s, Internal: meta}}, cat: cat}, why
}

// fileExcerpt places an excerpt of a file a user uploaded (file_content sent as text). An upload has no RAG
// source policy, so the files setting of the chain decides whether it may leave (run.filesAllowed); one that may
// not denies the request at the files check. Its class was already checked: restricted and unknown never get here.
func (b *builder) fileExcerpt(p Part, cat providers.SendCategory, cls providers.EgressEligibleClass) (*keptPart, string) {
	b.files = true
	if code := b.r.filesAllowed(cls); code != "" {
		if b.filesFail == "" {
			b.filesFail = code
		}
		return nil, "files_not_allowed"
	}
	k, why := b.text(p.Text, cat, cls, nil, nil)
	if k != nil {
		b.r.contentClasses = append(b.r.contentClasses, cls)
	}
	return k, why
}

// audio places raw audio (audio_content, for an external speech recogniser). Like an upload, audio has no RAG
// source policy: the audio setting of the chain decides whether it may leave (run.audioAllowed), and a policy that
// lets text of the same class leave does not. Audio that may not leave denies the request at the audio check; audio
// that may is placed as any media part, by reference and within the profile's limits.
func (b *builder) audio(p Part, cat providers.SendCategory, cls providers.EgressEligibleClass) (*keptPart, string) {
	b.audioSeen = true
	if code := b.r.audioAllowed(cls); code != "" {
		if b.audioFail == "" {
			b.audioFail = code
		}
		return nil, "audio_not_allowed"
	}
	k, why := b.media(p, cat, cls)
	if k != nil {
		b.r.contentClasses = append(b.r.contentClasses, cls)
	}
	return k, why
}

// rag is the evidence filter: what a RAG source may contribute is decided by its
// effective mode (additive plan section 8). local_only and deny keep the source
// out entirely; allow_metadata lets a title, id and status through and never the
// content; allow_excerpt lets a bounded excerpt through. A source whose class,
// provider, model or region policy does not allow this provider is kept out as well.
func (b *builder) rag(p Part, cat providers.SendCategory) (*keptPart, string) {
	if p.Kind != PartText {
		b.failEnvelope("bad_part_kind")
		return nil, "bad_part_kind"
	}
	if !sourceIDRE.MatchString(p.SourceID) {
		b.failEnvelope("rag_part_without_source")
		return nil, "rag_part_without_source"
	}
	id := providers.SourceID(p.SourceID)
	info := b.r.sources[id]
	if info == nil { // resolveSources saw every well-formed source id
		b.failEnvelope("source_unresolved")
		return nil, "source_unresolved"
	}
	exclude := func(reason providers.EgressDecisionReason, code, why string) (*keptPart, string) {
		b.withheld[cat] = true
		if p.Required && b.srcFail == nil {
			b.srcFail = &denial{check: providers.EgressDecisionCheckNameSourceMode, reason: reason, code: code}
		}
		return nil, why
	}
	switch info.mode {
	case providers.EgressModeAllowExcerpt, providers.EgressModeAllowMetadata:
	case providers.EgressModeLocalOnly:
		why := info.why
		if why == "" {
			why = "source_local_only"
		}
		return exclude(providers.EgressDecisionReasonSourceLocalOnly, "local_only_source_required", why)
	default: // deny, and anything that is not a mode
		return exclude(providers.EgressDecisionReasonSourceDenied, "denied_source_required", "source_denied")
	}

	cls, ok := parseClass(p.Class)
	if !ok || info.policy == nil || info.policy.SourceClass == nil || !info.policy.SourceClass.Valid() {
		b.failClass(failUnclassified, providers.EgressDecisionReasonUnclassifiedData, "unknown_class")
		return nil, "unknown_class"
	}
	if r := classRank(*info.policy.SourceClass); r > classRank(cls) {
		cls = *info.policy.SourceClass // a part can not be less restricted than its source
	}
	if cls == providers.DataClassificationRestricted {
		b.failClass(failRestricted, providers.EgressDecisionReasonRestrictedData, "restricted_part")
		return nil, "restricted_data"
	}
	el := providers.EgressEligibleClass(cls)
	if !b.r.classAllowed(el, info.policy) {
		b.withheld[cat] = true
		if p.Required {
			b.failClass(failClassNotAllowed, providers.EgressDecisionReasonClassNotAllowed, "class_not_listed")
		}
		return nil, "source_class_not_allowed"
	}

	outCat, content, why := cat, p.Text, ""
	switch {
	case cat == providers.SendCategoryRAGExcerpt && info.mode == providers.EgressModeAllowMetadata:
		// Titles, ids and status only. The content never leaves.
		b.withheld[providers.SendCategoryRAGExcerpt] = true
		outCat, content, why = providers.SendCategoryRAGMetadata, p.Label, "metadata_only"
		if strings.TrimSpace(content) == "" {
			return nil, "metadata_only"
		}
	case cat == providers.SendCategoryRAGMetadata:
		if content = p.Label; strings.TrimSpace(content) == "" {
			content = p.Text
		}
	}
	if outCat == providers.SendCategoryRAGExcerpt || outCat == providers.SendCategoryRAGMetadata {
		scrubbed, found := scrub(content, true)
		b.note(found)
		if cut, truncated := cutUTF8(scrubbed, b.s.limits.MaxExcerptBytes); truncated {
			scrubbed, why = cut, "truncated"
		}
		if outCat == providers.SendCategoryRAGExcerpt && b.evidence+len(scrubbed) > b.s.limits.MaxEvidenceBytes {
			b.withheld[outCat] = true
			if p.Required {
				b.failEnvelope("required_evidence_too_large")
			}
			return nil, "evidence_budget_exceeded"
		}
		content = scrubbed
	}
	k, w := b.text(content, outCat, el, &id, &info.mode)
	if k == nil {
		return nil, w
	}
	if outCat == providers.SendCategoryRAGExcerpt {
		b.evidence += len(content)
		info.outcome = providers.EgressDecisionSourceDecisionOutcomeExcerptIncluded
	} else if info.outcome != providers.EgressDecisionSourceDecisionOutcomeExcerptIncluded {
		info.outcome = providers.EgressDecisionSourceDecisionOutcomeMetadataIncluded
	}
	if why == "" {
		why = w
	}
	return k, why
}

// media places an image, file or audio part: a reference to a scoped file the
// adapter resolves, never bytes and never a path.
func (b *builder) media(p Part, cat providers.SendCategory, cls providers.EgressEligibleClass) (*keptPart, string) {
	prof := b.r.prof
	var want providers.SendCategory
	var modality providers.Modality
	var handling *providers.ProviderProfileMediaHandling
	if prof.Media == nil {
		prof.Media = &providers.ProviderProfileMedia{}
	}
	switch p.Kind {
	case PartImage:
		want, modality, handling = providers.SendCategoryImageContent, providers.ModalityImage, prof.Media.Images
	case PartFile:
		want, modality, handling = providers.SendCategoryFileContent, providers.ModalityFile, prof.Media.Files
	default:
		want, modality, handling = providers.SendCategoryAudioContent, providers.ModalityAudio, prof.Media.Audio
	}
	switch {
	case cat != want:
		b.failEnvelope("category_kind_mismatch")
		return nil, "category_kind_mismatch"
	case !fileIDRE.MatchString(p.FileID):
		b.failEnvelope("bad_file_id")
		return nil, "bad_file_id"
	case handling == nil || !containsModality(prof.Modalities.Input, modality):
		b.failEnvelope("unsupported_modality")
		return nil, "unsupported_modality"
	case p.SizeBytes < 0 || p.SizeBytes > handling.MaxBytes:
		b.failEnvelope("media_too_large")
		return nil, "media_too_large"
	}
	meta := providers.RequestEnvelopePartMeta{Category: cat, Classification: cls, SizeBytes: providers.Counter(p.SizeBytes)}
	fid := providers.FileID(p.FileID)
	var v providers.RequestEnvelopePart
	switch p.Kind {
	case PartImage:
		v.Value = providers.RequestEnvelopeImagePart{Type: "image", FileID: fid, Internal: meta}
	case PartFile:
		v.Value = providers.RequestEnvelopeFilePart{Type: "file", FileID: fid, Internal: meta}
	default:
		v.Value = providers.RequestEnvelopeAudioPart{Type: "audio", FileID: fid, Internal: meta}
	}
	return &keptPart{value: v, cat: cat}, ""
}

// assemble builds and bounds the envelope (step 6). It returns a fixed code when
// the request cannot be expressed within the bounds of the schema and of the profile.
func (b *builder) assemble() (*providers.RequestEnvelope, string) {
	if b.envFail != "" {
		return nil, b.envFail
	}
	c, prof := b.c, b.r.prof

	instr, found := scrub(c.Instructions, true)
	b.note(found)
	if n := utf8.RuneCountInString(instr); strings.TrimSpace(instr) == "" || n > maxInstructionChars {
		return nil, "bad_instructions"
	}
	if len(b.msgs) == 0 {
		return nil, "no_content"
	}
	if len(b.msgs) > maxMessages {
		return nil, "too_many_messages"
	}

	tools := make([]providers.RequestEnvelopeToolSpec, 0, len(c.Tools))
	if len(c.Tools) > 0 {
		if !prof.ToolCalling.Supported {
			return nil, "tool_calling_unsupported"
		}
		if len(c.Tools) > maxTools || (prof.ToolCalling.MaxTools != nil && int64(len(c.Tools)) > *prof.ToolCalling.MaxTools) {
			return nil, "too_many_tools"
		}
	}
	for _, t := range c.Tools {
		desc, found := scrub(t.Description, true)
		b.note(found)
		if !toolNameRE.MatchString(t.Name) || utf8.RuneCountInString(desc) < 1 || utf8.RuneCountInString(desc) > maxToolDescChars || len(t.Parameters) > maxOpenMembers {
			return nil, "bad_tool"
		}
		params := map[string]any{}
		if t.Parameters != nil {
			v, found := scrubValue(t.Parameters)
			b.note(found)
			params, _ = v.(map[string]any)
		}
		tools = append(tools, providers.RequestEnvelopeToolSpec{Name: t.Name, Description: desc, Parameters: params})
	}

	rf := providers.RequestEnvelopeResponseFormat{Kind: c.ResponseFormat.Kind}
	switch rf.Kind {
	case providers.RequestEnvelopeResponseFormatKindText, providers.RequestEnvelopeResponseFormatKindJSONObject:
		if c.ResponseFormat.Schema != nil {
			return nil, "bad_response_format"
		}
	case providers.RequestEnvelopeResponseFormatKindJSONSchema:
		if c.ResponseFormat.Schema == nil || len(c.ResponseFormat.Schema) > maxOpenMembers {
			return nil, "bad_response_format"
		}
		v, found := scrubValue(c.ResponseFormat.Schema)
		b.note(found)
		rf.Schema, _ = v.(map[string]any)
		// Strict is a request to the provider where its profile guarantees one.
		rf.Strict = c.ResponseFormat.Strict && prof.StructuredOutput == providers.ProviderProfileStructuredOutputStrictSchema
	default:
		return nil, "bad_response_format"
	}

	if c.MaxOutputToks < 1 || c.MaxOutputToks > maxOutputToks || c.MaxOutputToks > prof.Limits.MaxOutputToks || c.TimeoutMS < 1 || c.TimeoutMS > maxTimeoutMS {
		return nil, "bad_limits"
	}

	payload := providers.RequestEnvelopePayload{
		Instructions: instr, Messages: b.msgs, Tools: tools, ResponseFormat: rf,
		Limits: providers.RequestEnvelopePayloadLimits{MaxOutputToks: c.MaxOutputToks, TimeoutMS: c.TimeoutMS},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, "unencodable"
	}
	size := int64(len(raw))
	if size > maxPayloadBytes || size > prof.Limits.MaxRequestBytes {
		return nil, "payload_too_large"
	}
	var reservation *providers.ReservationID
	if c.ReservationID != "" {
		id := providers.ReservationID(c.ReservationID)
		reservation = &id
	}
	est := b.s.estimate(size)
	if est < 0 {
		est = 0
	}
	return &providers.RequestEnvelope{
		EnvelopeID: b.r.d.EnvelopeID, CreatedAt: timestamp(b.r.now), Provider: providerRef(prof), Role: c.Role, Payload: payload,
		Internal: providers.RequestEnvelopeInternal{
			Subject: subjectOf(b.r.scope), TraceID: common.TraceID(c.TraceID), Channel: c.Channel, ReservationID: reservation,
			PayloadBytes: size, EstimatedToksIn: providers.Counter(est),
		},
	}, ""
}

// ---- classification ----

func parseClass(s string) (providers.DataClassification, bool) {
	c := providers.DataClassification(s)
	return c, c.Valid()
}

// classRank orders the classifications; a value that is not one ranks above restricted.
func classRank(c providers.DataClassification) int {
	switch c {
	case providers.DataClassificationPublic:
		return 0
	case providers.DataClassificationInternal:
		return 1
	case providers.DataClassificationConfidential:
		return 2
	case providers.DataClassificationRestricted:
		return 3
	}
	return 4
}

// classAllowed reports whether data of class cls may leave towards the provider.
// Public and internal data need every level of the chain to list the class (internal
// only with the approval recorded on that level) and the provider profile to allow it;
// confidential data is a per-source decision: it needs a source policy that lists it
// with an approval, and the profile to allow it. Anything else is not allowed.
// ADR 0045: self_hosted profiles may send public, internal and confidential when the
// profile lists them; internal does not require chain approval (restricted never leaves).
func (r *run) classAllowed(cls providers.EgressEligibleClass, src *providers.EgressPolicy) bool {
	if !cls.Valid() || !containsClass(r.prof.AllowedDataClasses, cls) {
		return false
	}
	if IsSelfHosted(r.prof) {
		switch cls {
		case providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential:
			return true
		}
		return false
	}
	if cls != providers.EgressEligibleClassConfidential {
		for _, p := range r.chain.all() {
			if !containsClass(p.AllowedClasses, cls) || (cls == providers.EgressEligibleClassInternal && p.Approval == nil) {
				return false
			}
		}
	} else if src == nil {
		return false
	}
	if src != nil {
		if !containsClass(src.AllowedClasses, cls) || (cls != providers.EgressEligibleClassPublic && src.Approval == nil) {
			return false
		}
	}
	return true
}

// filesAllowed returns why an excerpt of an uploaded file of class cls may not leave towards the provider, or ""
// when it may (see contentAllowed).
func (r *run) filesAllowed(cls providers.EgressEligibleClass) string {
	return r.contentAllowed(cls, "files", func(p *providers.EgressPolicy) *providers.EgressPolicyContentEgress { return p.Files })
}

// audioAllowed returns why raw audio of class cls may not leave towards the provider, or "" when it may (see
// contentAllowed).
func (r *run) audioAllowed(cls providers.EgressEligibleClass) string {
	return r.contentAllowed(cls, "audio", func(p *providers.EgressPolicy) *providers.EgressPolicyContentEgress { return p.Audio })
}

// contentAllowed decides content that no RAG source policy covers (an excerpt of an uploaded file, raw audio) by
// the setting of that kind on every level. Every level must enable it with a max_class at or above cls and, for
// internal and confidential, carry the approval; public and internal must also be listed in allowed_classes at
// every level, as for any part of that class (confidential cannot be listed above source level: the setting is the
// decision that stands in for a source policy); and the profile must allow the class. It returns a fixed reason
// code, or "".
func (r *run) contentAllowed(cls providers.EgressEligibleClass, kind string, setting func(*providers.EgressPolicy) *providers.EgressPolicyContentEgress) string {
	for i, p := range r.chain.all() {
		lv := string(levelNames[i])
		s := setting(p)
		switch {
		case s == nil || !s.Enabled || !s.MaxClass.Valid():
			return kind + "_off_" + lv
		case classRank(providers.DataClassification(cls)) > classRank(providers.DataClassification(s.MaxClass)):
			return kind + "_class_above_max_" + lv
		case cls != providers.EgressEligibleClassPublic && p.Approval == nil:
			return kind + "_not_approved_" + lv
		case cls != providers.EgressEligibleClassConfidential && !containsClass(p.AllowedClasses, cls):
			return "class_not_listed_" + lv
		}
	}
	if !cls.Valid() || !containsClass(r.prof.AllowedDataClasses, cls) {
		return "class_not_in_profile"
	}
	return ""
}

func containsModality(list []providers.Modality, m providers.Modality) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

// cutUTF8 cuts s to at most max bytes at a character boundary.
func cutUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
