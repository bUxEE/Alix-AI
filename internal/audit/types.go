package audit

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// EventType names a kind of audit event: two to four lower-case segments
// separated by dots, such as "action.committing". The first segment names the
// owning area.
type EventType string

// ChainLevels says which kinds of chain an event type may be appended to.
type ChainLevels uint8

// Chain levels. A type that sets none is a tenant-chain type.
const (
	// LevelTenant: the event belongs to one tenant and is appended to the
	// chain of (application, tenant).
	LevelTenant ChainLevels = 1 << iota
	// LevelApplication: the event belongs to an application as a whole (or to
	// the platform) and is appended to the application chain.
	LevelApplication
)

// MaxTypeNameLen bounds an event type name.
const MaxTypeNameLen = 64

var typeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*){1,3}$`)

// ValidTypeName reports whether s is a well-formed event type name.
func ValidTypeName(s string) bool { return len(s) <= MaxTypeNameLen && typeNameRE.MatchString(s) }

// TypeSpec describes an event type for registration.
type TypeSpec struct {
	// Name is the event type, e.g. "dataegress.decision".
	Name EventType
	// Description says in a sentence what the event records.
	Description string
	// Levels is where the event may be appended; zero means LevelTenant.
	Levels ChainLevels
	// Required lists top-level data keys that must be present.
	Required []string
	// Validate, when not nil, checks the (not yet redacted) data object. It
	// must not retain it. A failure refuses the append.
	Validate func(data map[string]any) error
}

func (s TypeSpec) levels() ChainLevels {
	if s.Levels == 0 {
		return LevelTenant
	}
	return s.Levels
}

func (s TypeSpec) validate() error {
	if !ValidTypeName(string(s.Name)) {
		return contracts.Newf(contracts.ErrValidationFailed,
			"audit: event type %q is not two to four lower-case dot-separated segments (at most %d characters)", clipName(string(s.Name)), MaxTypeNameLen)
	}
	if s.Levels&^(LevelTenant|LevelApplication) != 0 {
		return contracts.Newf(contracts.ErrValidationFailed, "audit: event type %s names an unknown chain level", s.Name)
	}
	for _, k := range s.Required {
		if k == "" {
			return contracts.Newf(contracts.ErrValidationFailed, "audit: event type %s requires an empty key", s.Name)
		}
	}
	return nil
}

func clipName(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// Registry holds the event types that may be appended. It is safe for
// concurrent use. Registering is meant for init(); the registry is never
// consulted while verifying a chain.
type Registry struct {
	mu    sync.RWMutex
	types map[EventType]TypeSpec
}

// NewRegistry returns an empty registry. Most code uses DefaultRegistry;
// tests that must not leak types into it build their own, usually from
// NewCoreRegistry.
func NewRegistry() *Registry { return &Registry{types: map[EventType]TypeSpec{}} }

// NewCoreRegistry returns a registry holding exactly the core event types of
// this package.
func NewCoreRegistry() *Registry {
	r := NewRegistry()
	for _, s := range coreTypes() {
		if err := r.Register(s); err != nil {
			panic("audit: core event type: " + err.Error())
		}
	}
	return r
}

// DefaultRegistry is the registry used by a Log that is given none: the core
// types plus whatever other packages register from init().
var DefaultRegistry = NewCoreRegistry()

// Register adds spec. A name that is already registered is a conflict, even
// when the specs are identical: two owners for one name is a bug.
func (r *Registry) Register(spec TypeSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.types[spec.Name]; dup {
		return contracts.Newf(contracts.ErrConflict, "audit: event type %s is already registered", spec.Name)
	}
	spec.Required = append([]string(nil), spec.Required...)
	r.types[spec.Name] = spec
	return nil
}

// Lookup returns the spec registered under name.
func (r *Registry) Lookup(name EventType) (TypeSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.types[name]
	return s, ok
}

// Types lists the registered specs sorted by name.
func (r *Registry) Types() []TypeSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]TypeSpec, 0, len(r.types))
	for _, s := range r.types {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Type is a registered event type whose data is a Go value of type T. It is
// what Register returns: the owning package keeps it in a variable and appends
// through it, so call sites cannot misspell a type name or pass another
// event's payload.
type Type[T any] struct{ name EventType }

// Name returns the event type.
func (t Type[T]) Name() EventType { return t.name }

// Event builds the NewEvent for payload.
func (t Type[T]) Event(payload T) NewEvent { return NewEvent{Type: t.name, Data: payload} }

// Append appends payload to the chain loc names, in its own transaction.
func (t Type[T]) Append(ctx context.Context, a Appender, loc Locator, payload T) (Event, error) {
	return a.AppendAt(ctx, loc, t.Event(payload))
}

// AppendTx appends payload inside tx, the caller's transaction.
func (t Type[T]) AppendTx(ctx context.Context, a Appender, tx *store.Tx, loc Locator, payload T) (Event, error) {
	return a.AppendTx(ctx, tx, loc, t.Event(payload))
}

// RegisterIn registers spec in r and returns its typed handle.
func RegisterIn[T any](r *Registry, spec TypeSpec) (Type[T], error) {
	if r == nil {
		return Type[T]{}, contracts.New(contracts.ErrInternal, "audit: nil registry")
	}
	if err := r.Register(spec); err != nil {
		return Type[T]{}, err
	}
	return Type[T]{name: spec.Name}, nil
}

// Register registers spec in DefaultRegistry and returns its typed handle.
func Register[T any](spec TypeSpec) (Type[T], error) { return RegisterIn[T](DefaultRegistry, spec) }

// MustRegister is Register for init(): it panics on an error, which is always
// a programming error (bad name, duplicate owner), like config.Register.
func MustRegister[T any](spec TypeSpec) Type[T] {
	t, err := Register[T](spec)
	if err != nil {
		panic(fmt.Sprintf("audit: MustRegister(%s): %v", spec.Name, err))
	}
	return t
}

// Core event types. The data of each is a JSON object; the keys listed as
// required are the only ones this package demands, the owning package decides
// the rest.
const (
	// EventPolicyDecision is one policy gateway decision, allow or deny: the
	// policy.Record of schemas/policy/decision-record.schema.json as it is.
	EventPolicyDecision EventType = "policy.decision"

	// The action lifecycle (master plan 11.4, ADR 0006): prepared, confirmed,
	// committing (appended BEFORE the adapter's Commit is called, in the same
	// transaction as the state change; SEC-087), then one outcome.
	EventActionPrepared    EventType = "action.prepared"
	EventActionConfirmed   EventType = "action.confirmed"
	EventActionCommitting  EventType = "action.committing"
	EventActionCommitted   EventType = "action.committed"
	EventActionRejected    EventType = "action.rejected"
	EventActionExpired     EventType = "action.expired"
	EventActionInvalidated EventType = "action.invalidated"
	EventActionUncertain   EventType = "action.uncertain"

	// Package lifecycle, on the application chain.
	EventPackageInstalled   EventType = "package.installed"
	EventPackageQuarantined EventType = "package.quarantined"
	EventPackageActivated   EventType = "package.activated"
	EventPackageDeactivated EventType = "package.deactivated"
	EventPackageRemoved     EventType = "package.removed"

	// Integration-audit runs (master plan 4.5), on the application chain.
	EventAuditRunStarted         EventType = "audit_run.started"
	EventAuditRunCompleted       EventType = "audit_run.completed"
	EventAuditRunWarningAccepted EventType = "audit_run.warning_accepted"

	// EventSettingsChanged is a change to a setting, on the application chain
	// or a tenant chain.
	EventSettingsChanged EventType = "settings.changed"

	// Deletions. Tombstones: the events of what was deleted stay.
	EventDeletionTenant         EventType = "deletion.tenant"
	EventDeletionSource         EventType = "deletion.source"
	EventDeletionHistoryCleared EventType = "deletion.history_cleared"
	// EventDeletionReceipts records that finished action preparations and their
	// receipts were removed (a tenant or principal erasure, or retention).
	EventDeletionReceipts EventType = "deletion.receipts"

	// EventAuditErased records that the payloads of audit events were replaced
	// by signed tombstones (ADR-0036). It is appended in the same transaction as
	// the tombstones and names no subject.
	EventAuditErased EventType = "audit.erased"

	// EventAuditExported records an export of the audit log (who, which
	// filter), appended before the data leaves.
	EventAuditExported EventType = "audit.exported"
)

func coreTypes() []TypeSpec {
	tenant := LevelTenant
	app := LevelApplication
	both := LevelTenant | LevelApplication
	action := func(name EventType, what string) TypeSpec {
		return TypeSpec{Name: name, Description: what, Levels: tenant, Required: []string{"preparation_id"}}
	}
	return []TypeSpec{
		{Name: EventPolicyDecision, Description: "a policy gateway decision (allow or deny) with its full record", Levels: both,
			Required: []string{"decision_id", "phase", "outcome"}},

		action(EventActionPrepared, "an action was prepared and awaits confirmation"),
		action(EventActionConfirmed, "a preparation was confirmed"),
		action(EventActionCommitting, "the commit is about to be sent to the application (write-ahead)"),
		action(EventActionCommitted, "the application recorded the commit"),
		action(EventActionRejected, "a preparation was rejected"),
		action(EventActionExpired, "a preparation expired"),
		action(EventActionInvalidated, "a preparation was invalidated"),
		action(EventActionUncertain, "the outcome of a commit is unknown"),

		{Name: EventPackageInstalled, Description: "an application package was installed (into quarantine)", Levels: app},
		{Name: EventPackageQuarantined, Description: "an application package was quarantined", Levels: app},
		{Name: EventPackageActivated, Description: "an application package was activated", Levels: app},
		{Name: EventPackageDeactivated, Description: "an application package was deactivated", Levels: app},
		{Name: EventPackageRemoved, Description: "an application package was removed", Levels: app},

		{Name: EventAuditRunStarted, Description: "an integration audit started", Levels: app, Required: []string{"run_id"}},
		{Name: EventAuditRunCompleted, Description: "an integration audit finished", Levels: app, Required: []string{"run_id"}},
		{Name: EventAuditRunWarningAccepted, Description: "an operator accepted an integration-audit warning", Levels: app, Required: []string{"run_id"}},

		{Name: EventSettingsChanged, Description: "a setting changed", Levels: both},

		{Name: EventDeletionTenant, Description: "a tenant was deleted (tombstone; its chain stays)", Levels: tenant},
		{Name: EventDeletionSource, Description: "a source was deleted (tombstone)", Levels: tenant, Required: []string{"source_id"}},
		{Name: EventDeletionHistoryCleared, Description: "conversation history was cleared; audit events are kept", Levels: tenant},

		{Name: EventDeletionReceipts, Description: "finished action preparations and their receipts were removed (erasure or retention)", Levels: tenant},

		{Name: EventAuditExported, Description: "the audit log was exported", Levels: tenant},
		{Name: EventAuditErased, Description: "audit payloads were replaced by signed tombstones (erasure or retention)", Levels: both,
			Required: []string{"kind", "reason", "operator"}},
	}
}
