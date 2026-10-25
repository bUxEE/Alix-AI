package dataegress

import (
	"context"
	"embed"
	"regexp"
	"sync"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("dataegress", migrations) }

// ID prefixes. They are constants because the repository invariants parse them.
const (
	prefixPolicy   = "epol"
	prefixDecision = "edec"
	prefixEnvelope = "envl"
)

// Capability scopes of the policy API. A context token carries them in
// Scope.CapabilityScopes; there are no wildcards and each write capability is
// bound to one level, so a token that may edit a workspace policy cannot edit the
// application's. Deployment-level policy and the kill switch are operator paths
// (see Operator) and have no capability scope: a request token can never reach them.
const (
	// CapabilityRead lets a principal read the policies of its own scope.
	CapabilityRead = "dataegress.read"
	// CapabilityApprove lets a principal approve a policy that lets internal or
	// confidential data leave (the "authorised operator" of the egress-policy schema).
	CapabilityApprove = "dataegress.approve"

	capabilityApplicationWrite = "dataegress.application.write"
	capabilityTenantWrite      = "dataegress.tenant.write"
	capabilityWorkspaceWrite   = "dataegress.workspace.write"
	capabilitySourceWrite      = "dataegress.source.write"
)

// WriteCapability returns the capability scope that lets a principal write the
// policy of level, or false for deployment (an operator path) and unknown levels.
func WriteCapability(level providers.EgressLevel) (string, bool) {
	switch level {
	case providers.EgressLevelApplication:
		return capabilityApplicationWrite, true
	case providers.EgressLevelTenant:
		return capabilityTenantWrite, true
	case providers.EgressLevelWorkspace:
		return capabilityWorkspaceWrite, true
	case providers.EgressLevelSource:
		return capabilitySourceWrite, true
	}
	return "", false
}

// Default bounds on what evidence may leave (additive plan section 8, "bounded
// authorised excerpts").
const (
	DefaultMaxExcerptBytes  = 2048
	DefaultMaxEvidenceBytes = 16 * 1024
)

// Limits bounds the RAG evidence of one external request. A zero value means
// the default.
type Limits struct {
	// MaxExcerptBytes caps one excerpt (or one metadata line). Longer text is cut
	// at a character boundary.
	MaxExcerptBytes int
	// MaxEvidenceBytes caps all evidence of one request. An excerpt that would
	// exceed it is withheld whole.
	MaxEvidenceBytes int
}

// Deps are the collaborators of a Service. DB, Clock and Audit are required:
// without an audit trail there is no egress (no best-effort mode).
type Deps struct {
	DB    *store.DB
	Clock clock.Clock
	Audit audit.Appender
	// IDs mints decision, envelope and policy IDs. Nil means a generator on Clock.
	IDs    *ids.Generator
	Limits Limits
	// EstimateToksIn turns the byte size of a payload into the upper-bound toks_in
	// estimate stored in the envelope. Nil means one tok per byte, which is an upper
	// bound for any tokenizer; the budget service (X-09) may pass a tighter bound.
	EstimateToksIn func(payloadBytes int64) int64
}

// Service is the data-egress policy engine: the policy store, the kill switch
// and the Guard in front of every external request. It is safe for concurrent
// use. It decides what DATA may leave; internal/egress decides which network
// destinations may be dialled, and the two are never merged.
type Service struct {
	db       *store.DB
	clk      clock.Clock
	audit    audit.Appender
	ids      *ids.Generator
	limits   Limits
	estimate func(int64) int64

	// latched holds the deployments whose kill switch this process has engaged: a
	// process that was told to stop sending stops at once, even when the database
	// write that persists the switch has not (yet) succeeded.
	latched sync.Map
}

// New returns a Service. It fails closed on a missing dependency.
func New(d Deps) (*Service, error) {
	switch {
	case d.DB == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "dataegress: a store is required")
	case d.Audit == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "dataegress: an audit log is required (no egress without an audit trail)")
	case d.Limits.MaxExcerptBytes < 0 || d.Limits.MaxEvidenceBytes < 0:
		return nil, contracts.New(contracts.ErrValidationFailed, "dataegress: limits cannot be negative")
	}
	s := &Service{db: d.DB, clk: d.Clock, audit: d.Audit, ids: d.IDs, limits: d.Limits, estimate: d.EstimateToksIn}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	if s.ids == nil {
		s.ids = ids.NewGenerator(s.clk)
	}
	if s.limits.MaxExcerptBytes == 0 {
		s.limits.MaxExcerptBytes = DefaultMaxExcerptBytes
	}
	if s.limits.MaxEvidenceBytes == 0 {
		s.limits.MaxEvidenceBytes = DefaultMaxEvidenceBytes
	}
	if s.estimate == nil {
		s.estimate = func(b int64) int64 { return b }
	}
	return s, nil
}

// Guard is the method set the dialogue PromptGuard adapter calls (X-12). It is
// defined here, not imported from internal/dialogue: dialogue sits above this
// package and its own tests forbid it to import it. The adapter maps a dialogue
// GuardInput onto a Candidate field by field (item role, content, class,
// local-only flag and source id) and maps Result.Parts back onto the items it
// sends.
type Guard interface {
	// Evaluate decides whether the candidate may be sent to the provider of its
	// profile. It returns the filtered envelope or, on a denial, an error with code
	// permission_denied whose Details name the rule and never carry content.
	Evaluate(ctx context.Context, c Candidate) (Result, error)
}

var _ Guard = (*Service)(nil)

var (
	reasonCodeRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	providerIDRE    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	modelIDRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)
	regionCodeRE    = regexp.MustCompile(`^(global|[a-z]{2}(-[a-z0-9]{1,16}){0,3})$`)
	sourceIDRE      = regexp.MustCompile(`^src_[0-9A-HJKMNP-TV-Z]{26}$`)
	toolNameRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	identifierMax   = contracts.MaxScopeValueLen
	profileIDRE     = regexp.MustCompile(`^pprf_[0-9A-HJKMNP-TV-Z]{26}$`)
	modelVersionRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,63}$`)
	reservationIDRE = regexp.MustCompile(`^rsv_[0-9A-HJKMNP-TV-Z]{26}$`)
	fileIDRE        = regexp.MustCompile(`^fil_[0-9A-HJKMNP-TV-Z]{26}$`)
)
