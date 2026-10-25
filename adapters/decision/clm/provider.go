package clm

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ErrUnsupportedRuntime is wrapped by an error when the service does not provide the
// scoring route or model listing. ScoreDecision reports the same condition as a
// result with the unsupported_runtime fallback reason.
var ErrUnsupportedRuntime = wire.ErrUnsupportedRuntime

// ErrModelMismatch is wrapped by an error when the service does not list the
// configured model, does not report the pinned revision, or answers as another model.
var ErrModelMismatch = errors.New("clm: the service's model or revision does not match the configuration")

const (
	pathRank      = "/v1/rank"
	pathSystemOne = "/v1/systemone"
	pathModels    = "/v1/models"
	pathHealth    = "/health"
)

// routes is the complete allowlist of this package; nothing else is callable.
// The sidecar of deploy/container/clm requires its service key (Authorization:
// Bearer) on the scoring and model routes; /health needs none and is called without
// it. The vLLM encoder behind the sidecar has no key and is never called from here.
var routes = []wire.Route{
	{Method: http.MethodPost, Path: pathRank, Auth: true},
	{Method: http.MethodPost, Path: pathSystemOne, Auth: true},
	{Method: http.MethodGet, Path: pathModels, Auth: true},
	{Method: http.MethodGet, Path: pathHealth, Auth: false},
}

// AllowedEndpoints lists the routes the adapter may call, as "METHOD /path".
func AllowedEndpoints() []string { return wire.Describe(routes) }

// Provider scores decisions through a CLM service. It implements
// internal/decision.DecisionModelProvider (asserted in tests and in the composition
// root) and is safe for concurrent use. Constructing it opens no connection.
type Provider struct {
	cfg      Config
	manifest gen.ScorerManifest
	client   *wire.Client

	mu        sync.Mutex
	checkedAt time.Time
	now       func() time.Time
}

// Option adjusts a Provider under construction.
type Option func(*options)

type options struct {
	log *slog.Logger
	now func() time.Time
	key *wire.Key
}

// WithAPIKey supplies the service key directly, for a composition root that reads it
// from an OS secret store; it takes precedence over Config.APIKeyRef.
func WithAPIKey(key string) Option {
	return func(o *options) { k := wire.NewKey(key); o.key = &k }
}

// WithLogger sets the logger that receives debug records (method, path, status,
// duration). Request bodies, responses, state and candidates are never logged.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithNow injects the time source of the model-check cache.
func WithNow(f func() time.Time) Option { return func(o *options) { o.now = f } }

// New validates cfg and builds the provider. A plaintext non-loopback endpoint or an
// invalid TLS configuration fails here, not at the first request.
func New(cfg Config, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "clm: invalid configuration")
	}
	cfg = cfg.withDefaults()
	ep, err := wire.ParseEndpoint("clm", cfg.Endpoint)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "clm: invalid configuration")
	}
	var tlsCfg *tls.Config
	if ep.Scheme == "https" {
		if tlsCfg, err = wire.BuildTLS("clm", cfg.TLS); err != nil {
			return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "clm: invalid configuration")
		}
	}
	var clmKey wire.Key
	if o.key != nil {
		clmKey = *o.key
	} else if clmKey, err = wire.ResolveKeyRef(cfg.APIKeyRef); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "clm: invalid configuration")
	}
	rev := cfg.ModelRevision
	if rev == "" {
		rev = UnverifiedRevision
	}
	p := &Provider{
		cfg: cfg,
		// The encoder revision is the revision of the CLM model; the frozen encoder is
		// named by the documentation but its revision is not reported by the service.
		manifest: wire.Manifest(ProviderID, cfg.Model, rev, EncoderID, "unspecified",
			gen.ScorerManifestLicence{SpdxID: "Apache-2.0", Redistribution: gen.ScorerManifestLicenceRedistributionAllowed,
				// Vendor statement (model card, read 2026-10-07); the licence review is D-H01.
				URL: ptr("https://huggingface.co/Contrastive-LM/CLM-v0.1-8B")}),
		client: wire.NewClient(wire.Options{
			Who: "clm", Endpoint: ep, TLS: tlsCfg, Key: clmKey, Routes: routes,
			Timeout: cfg.RequestTimeout, MaxResponse: cfg.MaxResponseBytes, Logger: o.log,
		}),
		now: o.now,
	}
	if p.now == nil {
		p.now = time.Now
	}
	return p, nil
}

func ptr[T any](v T) *T { return &v }

// String describes the provider.
func (p *Provider) String() string { return "decision/clm.Provider{" + p.cfg.Endpoint + "}" }

// Manifest identifies the provider, the configured model and the pinned revision.
func (p *Provider) Manifest() gen.ScorerManifest { return p.manifest }

// Health reports whether the service answers GET /health.
func (p *Provider) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.HealthTimeout)
	defer cancel()
	resp, err := p.client.Do(ctx, http.MethodGet, pathHealth, nil)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return wire.StatusError("clm", resp)
	}
	return nil
}

// Ready is Health plus a fresh model and revision check: it returns nil only when
// the service is up and lists the configured model at the pinned revision. A
// mismatch wraps ErrModelMismatch; a service without the model listing wraps
// ErrUnsupportedRuntime.
func (p *Provider) Ready(ctx context.Context) error {
	if err := p.Health(ctx); err != nil {
		return err
	}
	return p.verifyModel(ctx, true)
}

// modelEntry is one entry of GET /v1/models. The documentation names "names,
// descriptions, and release dates" and no field names, so id or name and revision or
// version are all accepted.
type modelEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Revision string `json:"revision"`
	Version  string `json:"version"`
}

func (e modelEntry) name() string {
	for _, s := range []string{e.ID, e.Name, e.Model} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (e modelEntry) revision() string {
	if e.Revision != "" {
		return e.Revision
	}
	return e.Version
}

// decodeModels reads the listing as {"data": [...]}, {"models": [...]} or a bare
// array.
func decodeModels(body []byte) ([]modelEntry, error) {
	var wrapped struct {
		Data   []modelEntry `json:"data"`
		Models []modelEntry `json:"models"`
	}
	if err := wire.Decode("clm", body, &wrapped); err == nil && (wrapped.Data != nil || wrapped.Models != nil) {
		return append(wrapped.Data, wrapped.Models...), nil
	}
	var bare []modelEntry
	if err := wire.Decode("clm", body, &bare); err != nil {
		return nil, err
	}
	return bare, nil
}

// verifyModel checks the configured model and pinned revision against GET
// /v1/models. A passed check is reused for ModelCheckTTL unless force is set; a
// failed one never is.
func (p *Provider) verifyModel(ctx context.Context, force bool) error {
	p.mu.Lock()
	fresh := !force && !p.checkedAt.IsZero() && p.now().Sub(p.checkedAt) < p.cfg.ModelCheckTTL
	p.mu.Unlock()
	if fresh {
		return nil
	}
	resp, err := p.client.Do(ctx, http.MethodGet, pathModels, nil)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return wire.StatusError("clm", resp)
	}
	entries, err := decodeModels(resp.Body)
	if err != nil {
		return err
	}
	mismatch := func() error {
		p.mu.Lock()
		p.checkedAt = time.Time{}
		p.mu.Unlock()
		return contracts.Wrap(contracts.ErrUnavailable, ErrModelMismatch, "clm: the service does not serve the configured model or revision")
	}
	for _, e := range entries {
		if e.name() != p.cfg.Model {
			continue
		}
		if p.cfg.ModelRevision != "" && e.revision() != p.cfg.ModelRevision {
			return mismatch()
		}
		p.mu.Lock()
		p.checkedAt = p.now()
		p.mu.Unlock()
		return nil
	}
	return mismatch()
}

// ScoreDecision scores req.CandidateSet. Failures are typed: an unsupported shape (a
// state over the encoder limit, too few or too many candidates, indistinguishable
// candidates, a body over the cap), a missing route and a model or revision
// mismatch come back as a declined result (a fallback disposition with the reason);
// every other failure as a *contracts.Error the engine maps. No partial output is
// ever returned as a success.
func (p *Provider) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	if err := scope.Validate(); err != nil {
		return gen.DecisionScoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return gen.DecisionScoreResult{}, contracts.Wrap(contracts.ErrTimeout, err, "clm: deadline exceeded")
		}
		return gen.DecisionScoreResult{}, contracts.Wrap(contracts.ErrUnavailable, err, "clm: request canceled")
	}
	decline := func(r gen.FallbackReason) (gen.DecisionScoreResult, error) {
		return wire.Declined(req, p.manifest, r), nil
	}
	call, err := p.build(req)
	if err != nil {
		if errors.Is(err, errShape) {
			return decline(gen.FallbackReasonOodUnsupportedShape)
		}
		return gen.DecisionScoreResult{}, err
	}
	started := time.Now()
	// The model check runs before any state leaves, so a service that serves something
	// else never sees it.
	if err := p.verifyModel(ctx, false); err != nil {
		return p.failed(req, err)
	}
	ranked, err := p.ask(ctx, call)
	if err != nil {
		return p.failed(req, err)
	}
	sel := wire.Selections(ranked, int(req.CandidateSet.MaxSelections), p.cfg.SecondaryFloor)
	return wire.Accepted(req, p.manifest, sel, time.Since(started).Milliseconds()), nil
}

// failed turns an error into a declined result when its reason is a verdict about
// the runtime or the shape, and passes every other typed error through.
func (p *Provider) failed(req gen.DecisionRequest, err error) (gen.DecisionScoreResult, error) {
	switch {
	case errors.Is(err, ErrUnsupportedRuntime):
		return wire.Declined(req, p.manifest, gen.FallbackReasonUnsupportedRuntime), nil
	case errors.Is(err, ErrModelMismatch):
		return wire.Declined(req, p.manifest, gen.FallbackReasonModelMismatch), nil
	}
	return gen.DecisionScoreResult{}, err
}

// Probe sends the configured mode's route a fixed two-candidate question (no
// application data) to find out whether the service provides it. It returns nil when
// it does, an error wrapping ErrUnsupportedRuntime when the route or backend is
// absent, ErrModelMismatch on a model mismatch, and another typed error when the
// service could not be reached or answered something else.
func (p *Provider) Probe(ctx context.Context) error {
	probe := gen.DecisionRequest{
		DecisionType: gen.DecisionTypeItemSelection, SchemaVersion: 1, Locale: "en",
		BoundedState: map[string]gen.BoundedScalar{"probe": true},
		CandidateSet: gen.CandidateSet{MaxSelections: 1, Candidates: []gen.CandidateSetCandidate{
			{CandidateID: "a", Label: "first"}, {CandidateID: "b", Label: "second"},
		}},
	}
	c, err := p.build(probe)
	if err != nil {
		return contracts.New(contracts.ErrInternal, "clm: cannot build the probe")
	}
	if err := p.verifyModel(ctx, false); err != nil {
		return err
	}
	_, err = p.ask(ctx, c)
	return err
}
