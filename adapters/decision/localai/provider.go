package localai

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/buxee/alix-ai/adapters/decision/internal/wire"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// ErrUnsupportedRuntime is wrapped by Probe's error when the runtime does not
// provide the Decisions API or does not host a decisions model. ScoreDecision
// reports the same condition as a result with the unsupported_runtime fallback
// reason.
var ErrUnsupportedRuntime = wire.ErrUnsupportedRuntime

const (
	pathSystemOne = "/v1/systemone"
	pathReady     = "/readyz"
)

// routes is the complete allowlist of this package, separate from the five-route
// table of adapters/localai and from the voice one: neither can widen another.
// /v1/systemone/permute and /v1/systemone/separate, model listing, install,
// backends, agents and every other route are absent and unreachable. The
// readiness route is anonymous upstream and is called without the key.
var routes = []wire.Route{
	{Method: http.MethodPost, Path: pathSystemOne, Auth: true},
	{Method: http.MethodGet, Path: pathReady, Auth: false},
}

// AllowedEndpoints lists the routes the adapter may call, as "METHOD /path".
func AllowedEndpoints() []string { return wire.Describe(routes) }

// Provider scores decisions through LocalAI's Decisions API. It implements
// internal/decision.DecisionModelProvider (asserted in tests and in the composition
// root) and is safe for concurrent use. Constructing it opens no connection.
type Provider struct {
	cfg      Config
	manifest gen.ScorerManifest
	client   *wire.Client
}

// Option adjusts a Provider under construction.
type Option func(*options)

type options struct {
	log *slog.Logger
	key *wire.Key
}

// WithLogger sets the logger that receives debug records (method, path, status,
// duration). Request bodies, responses, state, candidates and the key are never
// logged.
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.log = l } }

// WithAPIKey supplies the key directly, for a composition root that reads it from an
// OS secret store; it takes precedence over Config.APIKeyRef.
func WithAPIKey(key string) Option {
	return func(o *options) { k := wire.NewKey(key); o.key = &k }
}

// New validates cfg, resolves the key reference and builds the provider. A refused
// endpoint (plaintext to a non-loopback host), an unreadable key reference or an
// invalid TLS configuration fails here, not at the first request.
func New(cfg Config, opts ...Option) (*Provider, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if err := cfg.Validate(); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai decisions: invalid configuration")
	}
	cfg = cfg.withDefaults()
	ep, err := wire.ParseEndpoint("localai decisions", cfg.Endpoint)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai decisions: invalid configuration")
	}
	var key wire.Key
	if o.key != nil {
		key = *o.key
	} else if key, err = wire.ResolveKeyRef(cfg.APIKeyRef); err != nil {
		return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai decisions: invalid configuration")
	}
	var tlsCfg *tls.Config
	if ep.Scheme == "https" {
		if tlsCfg, err = wire.BuildTLS("localai decisions", cfg.TLS); err != nil {
			return nil, contracts.Wrap(contracts.ErrValidationFailed, err, "localai decisions: invalid configuration")
		}
	}
	return &Provider{
		cfg: cfg,
		manifest: wire.Manifest(ProviderID, cfg.Model, cfg.ModelRevision, "unspecified", "unspecified",
			// The weights belong to whatever the operator installed; AlixAI cannot read a
			// licence off a runtime, so none is claimed and redistribution is restricted.
			gen.ScorerManifestLicence{SpdxID: "unknown", Redistribution: gen.ScorerManifestLicenceRedistributionRestricted}),
		client: wire.NewClient(wire.Options{
			Who: "localai decisions", Endpoint: ep, TLS: tlsCfg, Key: key, Routes: routes,
			Timeout: cfg.RequestTimeout, MaxResponse: cfg.MaxResponseBytes, Logger: o.log,
		}),
	}, nil
}

// String describes the provider without any credential.
func (p *Provider) String() string { return "decision/localai.Provider{" + p.cfg.Endpoint + "}" }

// Manifest identifies the provider, the configured model and the pinned revision.
func (p *Provider) Manifest() gen.ScorerManifest { return p.manifest }

// Health reports whether the runtime answers its readiness route. It says nothing
// about the Decisions API; Probe does.
func (p *Provider) Health(ctx context.Context) error {
	resp, err := p.client.Do(ctx, http.MethodGet, pathReady, nil)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return contracts.Newf(contracts.ErrUnavailable, "localai decisions: the runtime is not ready (HTTP %d)", resp.Status)
	}
	return nil
}

// Probe asks the runtime a fixed two-option question (no application data) to find
// out whether it provides the Decisions API for the configured model. It returns nil
// when it does, an error wrapping ErrUnsupportedRuntime when it does not, and another
// typed error when the runtime could not be reached or answered something else.
func (p *Provider) Probe(ctx context.Context) error {
	qs := map[string]Question{choiceID: {Type: TypeChoice, Instructions: "Choose one option.", Criteria: map[string]string{"a": "first", "b": "second"}}}
	body, err := encodeSystemOne(p.cfg.Model, "probe", qs, p.cfg.MaxOptions)
	if err != nil {
		return contracts.New(contracts.ErrInternal, "localai decisions: cannot build the probe")
	}
	_, err = p.ask(ctx, body, map[string]struct{}{"a": {}, "b": {}})
	var ae *answerError
	if errors.As(err, &ae) && ae.mismatch {
		return contracts.New(contracts.ErrUnavailable, "localai decisions: the runtime serves another model than the configured one")
	}
	return err
}

// answerError marks an answer that names another model or revision than the
// configured one.
type answerError struct{ mismatch bool }

func (*answerError) Error() string { return "localai decisions: model or revision mismatch" }

// ScoreDecision scores req.CandidateSet with one typed choice question. Failures are
// typed: an unsupported shape, a missing runtime route or backend and a model or
// revision mismatch come back as a declined result (a fallback disposition with the
// reason), every other failure as a *contracts.Error the engine maps. No partial
// output is ever returned as a success.
func (p *Provider) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	if err := scope.Validate(); err != nil {
		return gen.DecisionScoreResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return gen.DecisionScoreResult{}, wireCtxError(err)
	}
	decline := func(r gen.FallbackReason) (gen.DecisionScoreResult, error) {
		return wire.Declined(req, p.manifest, r), nil
	}
	state, err := wire.StateString(req)
	if err != nil {
		return gen.DecisionScoreResult{}, err
	}
	qs, err := decisionQuestions(req)
	if err != nil {
		return decline(gen.FallbackReasonOodUnsupportedShape)
	}
	body, err := encodeSystemOne(p.cfg.Model, state, qs, p.cfg.MaxOptions)
	if err != nil {
		return decline(gen.FallbackReasonOodUnsupportedShape)
	}
	ids := make(map[string]struct{}, len(req.CandidateSet.Candidates))
	for _, c := range req.CandidateSet.Candidates {
		ids[string(c.CandidateID)] = struct{}{}
	}
	started := time.Now()
	ranked, err := p.ask(ctx, body, ids)
	if err != nil {
		var ae *answerError
		switch {
		case errors.Is(err, ErrUnsupportedRuntime):
			return decline(gen.FallbackReasonUnsupportedRuntime)
		case errors.As(err, &ae) && ae.mismatch:
			return decline(gen.FallbackReasonModelMismatch)
		}
		return gen.DecisionScoreResult{}, err
	}
	sel := wire.Selections(ranked, int(req.CandidateSet.MaxSelections), p.cfg.SecondaryFloor)
	return wire.Accepted(req, p.manifest, sel, time.Since(started).Milliseconds()), nil
}

func wireCtxError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return contracts.Wrap(contracts.ErrTimeout, err, "localai decisions: deadline exceeded")
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "localai decisions: request canceled")
}

// ask sends one encoded request and returns the scored options, best first. ids are
// the options that were offered: an answer outside them is malformed.
func (p *Provider) ask(ctx context.Context, body []byte, ids map[string]struct{}) ([]wire.Scored, error) {
	resp, err := p.client.Do(ctx, http.MethodPost, pathSystemOne, body)
	if err != nil {
		return nil, err
	}
	if resp.Status != http.StatusOK {
		return nil, wire.StatusError("localai decisions", resp)
	}
	var out systemOneResponse
	if err := wire.Decode("localai decisions", resp.Body, &out); err != nil {
		return nil, err
	}
	if (out.Model != "" && out.Model != p.cfg.Model) || (out.Revision != "" && out.Revision != p.cfg.ModelRevision) {
		return nil, &answerError{mismatch: true}
	}
	bad := func() error {
		return contracts.New(contracts.ErrSchemaInvalid, "localai decisions: the answer is not the documented choice answer")
	}
	a, ok := out.Answers[choiceID]
	if !ok || (a.Type != "" && a.Type != TypeChoice) || a.Choice == nil {
		return nil, bad()
	}
	if _, offered := ids[*a.Choice]; !offered {
		return nil, bad()
	}
	for id, pr := range a.Probabilities {
		if _, offered := ids[id]; !offered || !wire.Unit(pr) {
			return nil, bad()
		}
	}
	if a.Confidence != nil && !wire.Unit(*a.Confidence) {
		return nil, bad()
	}
	pick := wire.Scored{ID: *a.Choice}
	prob, hasProb := a.Probabilities[*a.Choice]
	switch {
	case hasProb && a.Confidence != nil:
		pick.Prob, pick.Confidence = prob, *a.Confidence
	case hasProb:
		pick.Prob, pick.Confidence = prob, prob
	case a.Confidence != nil:
		pick.Prob, pick.Confidence = *a.Confidence, *a.Confidence
	default:
		return nil, bad()
	}
	ranked := []wire.Scored{pick}
	for id, pr := range a.Probabilities {
		if id != pick.ID {
			ranked = append(ranked, wire.Scored{ID: id, Prob: pr, Confidence: pr})
		}
	}
	return ranked, nil
}
