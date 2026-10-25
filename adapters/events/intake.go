package events

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Defaults of Config.
const (
	// DefaultMaxBodyBytes bounds a delivery.
	DefaultMaxBodyBytes = 64 << 10
	// DefaultMaxValidity bounds how long one signature may be valid. Event ids are
	// remembered until the signature that carried them expires, so this is also how
	// long the intake remembers.
	DefaultMaxValidity = 15 * time.Minute
)

// Sessions is the part of the session layer the intake needs. *sessions.Store
// implements it.
type Sessions interface {
	ApplyEvent(ctx context.Context, service contracts.Scope, ev sessions.ApplicationEvent, ivr sessions.Invalidator, n sessions.Notifier) (sessions.EventResult, error)
}

var _ Sessions = (*sessions.Store)(nil)

// Config configures an Intake.
type Config struct {
	// DeploymentID is this AlixAI installation, for the service scope. Required.
	DeploymentID string
	// ApplicationID is the application this intake serves. Every event must be
	// for it and signed by one of its keys. Required. The platform's own
	// application ID is refused: its keys sign AlixAI's own artifacts.
	ApplicationID string
	// Verifier checks signatures against the trust store. Required.
	Verifier *trust.Verifier
	// Replay remembers event ids. Required. Production wiring passes the durable
	// identity.SQLiteReplayCache, so a restart does not reopen the window.
	Replay identity.ReplayCache
	// Sessions applies verified events. Required.
	Sessions Sessions
	// Invalidator is the coordinator's Invalidate, the one effectful thing an
	// event can reach. Required.
	Invalidator sessions.Invalidator
	// Notifier tells the sessions an event changed. Optional.
	Notifier sessions.Notifier
	// AllowScope narrows the tenants and workspaces of the application the intake
	// accepts events for; return an error to refuse. Optional: without it every
	// tenant and workspace of the application a key is registered for is accepted,
	// because the application's key is its authority over its own tenants.
	AllowScope func(ctx context.Context, tenantID, workspaceID string) error
	// MaxValidity is the longest a signature may be valid. Default DefaultMaxValidity.
	MaxValidity time.Duration
	// MaxBodyBytes bounds a delivery. Default DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// Logger receives operational records: identifiers and counts, never a body.
	// Default: discard.
	Logger *slog.Logger
}

// Status is what became of a delivery.
type Status string

const (
	// StatusApplied: the event was verified and applied.
	StatusApplied Status = "applied"
	// StatusDuplicate: an event with this id was already applied; nothing was done.
	StatusDuplicate Status = "duplicate"
)

// Receipt is the outcome of an accepted delivery. A refused delivery has no
// receipt, only an error.
type Receipt struct {
	EventID string `json:"event_id"`
	Status  Status `json:"status"`
	// Sessions is how many sessions the event changed; Notified and NotifyFailed
	// how many of them were told and how many could not be.
	Sessions     int `json:"sessions"`
	Notified     int `json:"notified"`
	NotifyFailed int `json:"notify_failed"`
}

// Intake receives signed application events for one application.
type Intake struct {
	cfg Config
}

// New returns an Intake. A configuration that leaves anything it depends on
// unset is an error, never a default that accepts events.
func New(cfg Config) (*Intake, error) {
	bad := func(msg string) (*Intake, error) {
		return nil, contracts.New(contracts.ErrValidationFailed, "events: "+msg)
	}
	switch {
	case cfg.DeploymentID == "":
		return bad("an intake needs the deployment id")
	case cfg.ApplicationID == "":
		return bad("an intake is bound to one application")
	case cfg.ApplicationID == trust.PlatformApplicationID:
		return bad("an intake cannot be bound to the platform application")
	case cfg.Verifier == nil:
		return bad("an intake needs a verifier")
	case cfg.Replay == nil:
		return bad("an intake needs a replay cache")
	case cfg.Sessions == nil:
		return bad("an intake needs the session store")
	case cfg.Invalidator == nil:
		return bad("an intake needs the coordinator's invalidator")
	}
	if cfg.MaxValidity <= 0 {
		cfg.MaxValidity = DefaultMaxValidity
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	// The intake's scope is validated once, so a deployment or application id the
	// scope grammar refuses fails at start-up and not on the first event.
	if err := serviceScope(cfg.DeploymentID, cfg.ApplicationID, "tenant", "workspace").Validate(); err != nil {
		return nil, err
	}
	return &Intake{cfg: cfg}, nil
}

// serviceScope is the identity an event is applied under. Its tenant and
// workspace come from the signed event, nothing else.
func serviceScope(deployment, application, tenant, workspace string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: deployment, ApplicationID: application, TenantID: tenant, WorkspaceID: workspace,
		PrincipalID: ServicePrincipal, Roles: []string{ServiceRole}, CapabilityScopes: []string{sessions.EventCapability},
		SessionID: ServiceSession,
	}
}

// replayKey is the id an event is remembered under. The prefix keeps it apart
// from the nonces of context tokens in the same cache.
func replayKey(eventID string) string { return "evt:" + eventID }

// DeliverNotification handles an MCP notification whose params are a delivery.
// Anything but MCPNotificationMethod is not an event and is refused. A
// notification that arrives over an authenticated MCP connection is still
// unsigned until its signature verifies: the connection says which application
// it came from (the intake is bound to one), not that the event is genuine.
func (i *Intake) DeliverNotification(ctx context.Context, method string, params json.RawMessage) (Receipt, error) {
	if method != MCPNotificationMethod {
		return Receipt{}, invalid(contracts.ErrValidationFailed, "not an event notification")
	}
	return i.Deliver(ctx, params)
}

// Deliver verifies and applies one delivery. See the package documentation for
// the order of the checks. An error means the event was not applied (or, if
// recording it failed after it was, that it may be delivered again safely); a
// duplicate is not an error.
func (i *Intake) Deliver(ctx context.Context, raw []byte) (Receipt, error) {
	cfg := i.cfg
	if int64(len(raw)) > cfg.MaxBodyBytes {
		return Receipt{}, i.refused(invalid(contracts.ErrSchemaInvalid, "the delivery is too large"), "")
	}
	env, err := decodeEnvelope(raw)
	if err != nil {
		return Receipt{}, i.refused(err, "")
	}

	// A delivery carries one signature. With several, which one verifies would
	// depend on the clock (the first acceptable in array order), and the id is
	// remembered only until the one that did expire: a replay after it would pass
	// on the next signature and apply the event again. Events are short lived and
	// need no key-rotation overlap: the host signs each delivery with its current
	// key. No signature at all is refused below, as unauthenticated.
	if len(env.Signatures) > 1 {
		return Receipt{}, i.refused(invalid(contracts.ErrSchemaInvalid, "a delivery carries one signature"), "")
	}

	// Authenticity first: nothing about the event is believed before a signature
	// by a key registered for this application verified over exactly its bytes.
	acc, err := cfg.Verifier.VerifyPayload(ctx, env.Event, env.Signatures, trust.PayloadOptions{ApplicationID: cfg.ApplicationID})
	if err != nil {
		return Receipt{}, i.refused(err, "")
	}
	if acc.ExpiresAt.Sub(acc.SignedAt) > cfg.MaxValidity {
		return Receipt{}, i.refused(contracts.New(contracts.ErrUnauthenticated, "events: the signature is valid for longer than the intake accepts"), "")
	}

	ev, err := decodeEvent(env.Event)
	if err != nil {
		return Receipt{}, i.refused(err, "")
	}
	if ev.ApplicationID != cfg.ApplicationID {
		return Receipt{}, i.refused(contracts.New(contracts.ErrScopeViolation, "events: the event is for another application"), ev.ID)
	}
	scope := serviceScope(cfg.DeploymentID, ev.ApplicationID, ev.TenantID, ev.WorkspaceID)
	if err := scope.Validate(); err != nil {
		return Receipt{}, i.refused(err, ev.ID)
	}
	if cfg.AllowScope != nil {
		if err := cfg.AllowScope(ctx, ev.TenantID, ev.WorkspaceID); err != nil {
			code := contracts.CodeOf(err)
			if code == "" || code == contracts.ErrInternal {
				code = contracts.ErrScopeViolation
			}
			return Receipt{}, i.refused(contracts.Wrap(code, err, "events: the event is for a scope the intake does not accept"), ev.ID)
		}
	}
	sev := ev.session()
	if err := sev.Validate(); err != nil {
		return Receipt{}, i.refused(err, ev.ID)
	}

	// Replay: the event was already applied for this scope, whatever signature
	// this delivery carries.
	seen, err := i.seen(ctx, scope, ev.ID)
	if err != nil {
		return Receipt{}, i.refused(err, ev.ID)
	}
	if seen {
		cfg.Logger.Info("event ignored: already applied", "event_id", ev.ID, "application_id", ev.ApplicationID)
		return Receipt{EventID: ev.ID, Status: StatusDuplicate}, nil
	}

	res, err := cfg.Sessions.ApplyEvent(ctx, scope, sev, cfg.Invalidator, cfg.Notifier)
	if err != nil {
		// Not recorded as seen: the sender redelivers and the event is applied again.
		cfg.Logger.Warn("event not applied", "event_id", ev.ID, "application_id", ev.ApplicationID, "code", string(contracts.CodeOf(err)))
		return Receipt{}, err
	}
	rc := Receipt{EventID: ev.ID, Status: StatusApplied, Sessions: res.Sessions, Notified: res.Notified, NotifyFailed: res.NotifyFailed}

	if err := cfg.Replay.Consume(ctx, scope, replayKey(ev.ID), acc.ExpiresAt); err != nil && contracts.CodeOf(err) != contracts.ErrReplayed {
		// Applied but not remembered: a replay inside the signature's validity would
		// be applied again, so say it did not complete and let it be redelivered
		// (applying is idempotent).
		cfg.Logger.Warn("event applied but not recorded", "event_id", ev.ID, "application_id", ev.ApplicationID)
		return rc, contracts.Wrap(contracts.ErrUnavailable, err, "events: the event was applied but could not be recorded; deliver it again")
	}
	if rc.NotifyFailed > 0 {
		cfg.Logger.Warn("sessions could not be notified", "event_id", ev.ID, "failed", rc.NotifyFailed, "notified", rc.Notified)
	}
	cfg.Logger.Info("event applied", "event_id", ev.ID, "event_type", ev.Type, "application_id", ev.ApplicationID,
		"tenant_id", ev.TenantID, "workspace_id", ev.WorkspaceID, "sessions", rc.Sessions)
	return rc, nil
}

// seen reports whether the id is already recorded and live, without recording or
// burning it: it asks the cache to record the id with an expiry that has already
// passed, which by the cache's contract is not a presentation (and is removed by
// the next Consume of the id) and which fails with replayed when the id is live.
// A cache that cannot decide fails the delivery (fail closed).
func (i *Intake) seen(ctx context.Context, scope contracts.Scope, eventID string) (bool, error) {
	err := i.cfg.Replay.Consume(ctx, scope, replayKey(eventID), time.Unix(0, 0))
	switch {
	case err == nil:
		return false, nil
	case contracts.CodeOf(err) == contracts.ErrReplayed:
		return true, nil
	default:
		return false, contracts.Wrap(contracts.ErrUnavailable, err, "events: the replay cache could not decide")
	}
}

// refused logs a refusal (the reason, never the body) and returns the error.
func (i *Intake) refused(err error, eventID string) error {
	if len(eventID) > contracts.MaxScopeValueLen {
		eventID = eventID[:contracts.MaxScopeValueLen]
	}
	i.cfg.Logger.Warn("event refused", "event_id", eventID, "code", string(contracts.CodeOf(err)), "reason", reasonOf(err))
	return err
}

// reasonOf names why a signature was refused, from trust's closed vocabulary, for
// the log and for the sender's debugging. It carries no key material.
func reasonOf(err error) string {
	var ve *trust.VerifyError
	switch {
	case errors.As(err, &ve) && len(ve.Failures) > 0:
		return string(ve.Failures[0].Reason)
	case errors.Is(err, trust.ErrUnsigned):
		return "unsigned"
	}
	return ""
}
