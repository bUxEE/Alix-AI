package extmodal

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Approval is the host's explicit decision about one file for one provider. The zero Approval allows
// nothing.
type Approval struct {
	// Approved lets the file (or an excerpt of it) go to the provider.
	Approved bool
	// PrivateEgress is the explicit egress permission an application-private file needs on top.
	PrivateEgress bool
	// ExcerptOnly lets only a transformed excerpt leave, never the file itself.
	ExcerptOnly bool
}

// FileApprovals returns the approval of a file for a provider. An error denies.
type FileApprovals interface {
	FileApproval(ctx context.Context, scope contracts.Scope, fileID, providerID string) (Approval, error)
}

// MemoryApprovals is an in-memory FileApprovals. Nothing granted means nothing approved.
type MemoryApprovals struct {
	mu sync.Mutex
	m  map[string]Approval
}

// NewMemoryApprovals returns an empty set.
func NewMemoryApprovals() *MemoryApprovals { return &MemoryApprovals{m: map[string]Approval{}} }

func approvalKey(s contracts.Scope, fileID, providerID string) string {
	return strings.Join([]string{s.DeploymentID, s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, fileID, providerID}, "\x00")
}

// Grant records the approval of a file for a provider in the scope.
func (a *MemoryApprovals) Grant(scope contracts.Scope, fileID, providerID string, ap Approval) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[approvalKey(scope, fileID, providerID)] = ap
}

// FileApproval implements FileApprovals.
func (a *MemoryApprovals) FileApproval(_ context.Context, scope contracts.Scope, fileID, providerID string) (Approval, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.m[approvalKey(scope, fileID, providerID)], nil
}

// FileUse names one file a request wants to use, by its AlixAI reference.
type FileUse struct {
	FileID string
	// Excerpt, when set, is a transformed excerpt that is sent as text INSTEAD of the file.
	Excerpt string
}

// VisionRequest is a question about approved images or files.
type VisionRequest struct {
	Prompt  string
	Files   []FileUse
	MaxToks int
}

const (
	maxFilesPerRequest = 8
	maxExcerptBytes    = 4096
	maxPromptBytes     = 16 << 10
)

// isPrivate reports whether a file belongs to the application and not to the user: such a file needs
// the explicit egress permission.
func isPrivate(ref conversation.FileRef) bool {
	if ref.Origin == conversation.FileRefOriginApplication {
		return true
	}
	return ref.Treatment != nil && *ref.Treatment == conversation.FileRefTreatmentApplicationObject
}

// preparedFile is a file that passed every check.
type preparedFile struct {
	part  providers.ContentPart // zero when an excerpt is sent instead
	text  string                // the excerpt
	bytes int64
}

// prepareFile resolves and checks one file. The order is fixed and stops at the first failure.
func (s *Service) prepareFile(ctx context.Context, scope contracts.Scope, t Target, u FileUse) (preparedFile, Modality, error) {
	m := ModFileInput
	if s.d.Files == nil || s.d.Approvals == nil {
		return preparedFile{}, m, refuse(m, ReasonFileUnresolved, "no file service or approval source is configured")
	}
	// A reference is resolved from scratch every time: scope and expiry are the file service's.
	ref, err := s.d.Files.Stat(ctx, scope, u.FileID)
	if err != nil {
		return preparedFile{}, m, contracts.Wrap(contracts.CodeOf(err), err, "extmodal: the file reference does not resolve").
			WithDetails("reason", string(ReasonFileUnresolved)).WithDetails("modality", string(m))
	}
	if strings.HasPrefix(ref.MimeType, "image/") {
		m = ModVision
	}
	if exp, perr := parseTime(string(ref.ExpiresAt)); perr != nil || !exp.After(s.clk.Now()) {
		return preparedFile{}, m, refuse(m, ReasonFileExpired, "the file reference has expired")
	}
	ap, err := s.d.Approvals.FileApproval(ctx, scope, u.FileID, string(t.Profile.ProviderID))
	if err != nil || !ap.Approved {
		return preparedFile{}, m, refuse(m, ReasonFileNotApproved, "only files approved for this provider may leave")
	}
	if isPrivate(ref) && !ap.PrivateEgress {
		return preparedFile{}, m, refuse(m, ReasonPrivateFile, "an application-private file needs explicit egress permission")
	}
	if u.Excerpt != "" {
		if len(u.Excerpt) > maxExcerptBytes {
			return preparedFile{}, m, refuse(m, ReasonTooLarge, "the excerpt is larger than the limit")
		}
		return preparedFile{text: u.Excerpt, bytes: int64(len(u.Excerpt))}, m, nil
	}
	if ap.ExcerptOnly {
		return preparedFile{}, m, refuse(m, ReasonExcerptOnly, "this file may leave only as a transformed excerpt")
	}
	kind := providers.ModFile
	if m == ModVision {
		kind = providers.ModImage
	}
	if !providers.MediaTypeAllowed(kind, ref.MimeType) {
		return preparedFile{}, m, refuse(m, ReasonMediaType, "this media type may not be sent")
	}
	if h := t.mediaHandling(m); h != nil {
		if len(h.MimeTypes) > 0 && !slices.Contains(h.MimeTypes, ref.MimeType) {
			return preparedFile{}, m, refuse(m, ReasonMediaType, "the provider does not accept this media type")
		}
		if h.MaxBytes > 0 && ref.SizeBytes > h.MaxBytes {
			return preparedFile{}, m, refuse(m, ReasonTooLarge, "the file is larger than the provider accepts")
		}
	}
	part := providers.ImageRef(u.FileID, ref.MimeType)
	if m == ModFileInput {
		part = providers.FileRef(u.FileID, ref.MimeType)
	}
	return preparedFile{part: part, bytes: ref.SizeBytes}, m, nil
}

// Describe sends a question about approved images or files to a vision-capable provider and returns
// the validated answer. Nothing is sent unless every modality it needs is admitted and every file
// passed: policy, capability, reference, expiry, approval, private-file permission, media type and
// size, and the egress guard for the prompt and excerpts.
func (s *Service) Describe(ctx context.Context, scope contracts.Scope, t Target, gen providers.MultimodalGenerator, req VisionRequest, opts IngestOptions) (Output, error) {
	if gen == nil {
		return Output{}, contracts.New(contracts.ErrUnavailable, "extmodal: no external generator is installed")
	}
	if strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > maxPromptBytes {
		return Output{}, invalid(ModVision, "the prompt is empty or too long")
	}
	if len(req.Files) == 0 || len(req.Files) > maxFilesPerRequest {
		return Output{}, invalid(ModVision, "a vision request names between one and eight files")
	}
	var (
		parts     = []providers.ContentPart{providers.TextPart(req.Prompt)}
		sent      = int64(len(req.Prompt))
		admitted  = map[Modality]Disclosure{}
		excerpts  []string
		seen      = map[string]bool{}
		disclose  []Disclosure
		needAdmit []Modality
	)
	for _, u := range req.Files {
		if seen[u.FileID] {
			return Output{}, invalid(ModVision, "a file is named twice")
		}
		seen[u.FileID] = true
		pf, m, err := s.prepareFile(ctx, scope, t, u)
		if err != nil {
			s.observe(Event{Modality: m, Provider: string(t.Profile.ProviderID), Model: string(t.Profile.ModelID), Reason: ReasonOf(err)})
			return Output{}, err
		}
		if pf.text != "" {
			excerpts = append(excerpts, pf.text)
		} else if _, ok := admitted[m]; !ok {
			admitted[m] = Disclosure{}
			needAdmit = append(needAdmit, m)
		}
		if pf.part.Kind != "" {
			parts = append(parts, pf.part)
		}
		sent += pf.bytes
	}
	if len(needAdmit) == 0 {
		needAdmit = append(needAdmit, ModVision) // a question is still a vision-role call
	}
	for _, m := range needAdmit {
		adm, err := s.Admit(ctx, scope, t, m)
		if err != nil {
			return Output{}, err
		}
		disclose = append(disclose, adm.Disclosure)
	}
	decision, err := s.allowText(ctx, scope, t, ModVision, PurposeVisionAsk, req.Prompt)
	if err != nil {
		return Output{}, err
	}
	for _, x := range excerpts {
		if _, err := s.allowText(ctx, scope, t, ModVision, PurposeExcerpt, x); err != nil {
			return Output{}, err
		}
		parts = append(parts, providers.TextPart(x))
	}
	rr := providers.RichRequest{
		Messages: []providers.RichMessage{{Role: providers.MsgUser, Parts: parts}},
		MaxToks:  req.MaxToks,
	}
	if rr.MaxToks == 0 {
		rr.MaxToks = t.outputCap() // the reply is asked for no more than what is reserved for it
	}
	if err := rr.Validate(); err != nil {
		return Output{}, err
	}
	if err := t.Caps.Check(rr); err != nil {
		return Output{}, err
	}
	if rr.MaxToks <= 0 {
		return Output{}, notEstimable(ModVision, "the reply has no output bound")
	}
	h, err := s.reserve(ctx, scope, t, ModVision, decision, richUnits(rr))
	if err != nil {
		return Output{}, err
	}
	start := s.clk.Now()
	res, err := gen.Generate(ctx, scope, rr)
	if err != nil {
		h.failed(ctx, err)
		return Output{}, err
	}
	h.succeeded(ctx, res.Usage)
	out, err := s.Ingest(ctx, scope, t, res, opts)
	if err != nil {
		return Output{}, err
	}
	out.Disclosures = append(out.Disclosures, disclose...)
	s.observe(Event{
		Modality: ModVision, Provider: string(t.Profile.ProviderID), Model: string(t.Profile.ModelID), Allowed: true, Route: "external",
		BytesSent: sent, BytesReceived: int64(len(res.Text())), Latency: s.clk.Now().Sub(start),
	})
	return out, nil
}
