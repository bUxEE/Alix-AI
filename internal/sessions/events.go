package sessions

import (
	"context"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// This file is the session side of the event adapter (master plan 9.3 item 4,
// 11.3): what a host application's event can do to stored sessions, and how the
// sessions it touched are told.
//
// An event can only invalidate. ApplyEvent turns it into an Invalidation, which
// voids, discards or clears pending state, and the only effectful thing it is
// handed is an Invalidator: the one method of the action coordinator that ends
// a preparation. There is no path from here to prepare, confirm, commit or call
// a tool: nothing in this file imports the coordinator, an adapter, the policy
// engine or the dialogue engine (TestEventNeverCommits holds it to that), and the
// state a sweep leaves is one from which the user must ask again.
//
// The caller authenticates the source of the event (the event adapter verifies a
// signature, deduplicates by event id and derives the scope from the signed
// payload). ApplyEvent trusts that and nothing else: it uses the application,
// tenant and workspace of the scope it is given, and insists that scope names a
// service identity allowed to deliver events (EventCapability).

// EventCapability is the capability scope the service identity of an event source
// carries. A scope without it cannot deliver events: ApplyEvent fails closed.
const EventCapability = "events.deliver"

// Bounds of an ApplicationEvent.
const (
	// MaxEventIDLen bounds an event ID (the longest scope value).
	MaxEventIDLen = contracts.MaxScopeValueLen
	// MaxEventTypeLen bounds an event's type label.
	MaxEventTypeLen = 64
	// MaxEventCapabilities bounds the capabilities one event may name.
	MaxEventCapabilities = 100
)

// ApplicationEvent is a typed event the host application reported, after the
// caller verified where it came from. It carries no scope (the caller passes the
// authenticated one) and no authority: it can only invalidate.
type ApplicationEvent struct {
	// ID identifies the event. The caller deduplicates by it; ApplyEvent is
	// idempotent anyway, because invalidating what is already invalid does nothing.
	ID string
	// Type is the host's own name for the event ("invoice.paid"): a short label
	// for the session notice, never interpreted.
	Type string
	// Kind says what kind of invalidation the event is. Timeouts are the core's
	// own and are not an event a host can report.
	Kind InvalidationKind
	// Entities are the records concerned (a record revision, or a selector).
	Entities []EntityRef
	// Capabilities are the capabilities concerned: a selector, or for a
	// permission change the capabilities no longer permitted.
	Capabilities []string
	// Everything applies the event to all pending state of the sessions it reaches,
	// when the host cannot say more.
	Everything bool
	// PrincipalID restricts the event to one principal's sessions; required for a
	// principal change.
	PrincipalID string
}

func eventErr(msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "sessions: "+msg)
}

// eventIdentifier reports whether s is a legal event ID: the scope-value grammar
// (1 to 128 characters, no whitespace, control character or wildcard).
func eventIdentifier(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > MaxEventIDLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// eventTypeLabel reports whether s is a legal event type label: a lower-case word
// of letters, digits and . _ : - that starts with a letter or digit.
func eventTypeLabel(s string) bool {
	if s == "" || len(s) > MaxEventTypeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == ':' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// Validate reports whether the event is well formed. A malformed event is
// refused, never read as "everything" or "nothing", and an unknown kind is
// refused rather than mapped to the nearest one.
func (e ApplicationEvent) Validate() error {
	if !eventIdentifier(e.ID) {
		return eventErr("an event needs an ID of 1 to 128 characters with no whitespace, control character or wildcard")
	}
	if !eventTypeLabel(e.Type) {
		return eventErr("an event needs a type label of lower-case letters, digits and . _ : - (at most 64 characters)")
	}
	if len(e.Capabilities) > MaxEventCapabilities {
		return eventErr("an event names too many capabilities")
	}
	switch e.Kind {
	case KindRecordRevision:
		if len(e.Capabilities) > 0 || e.Everything {
			return eventErr("a record revision names records, not capabilities or everything")
		}
	case KindApplicationEvent:
	case KindPermissionChange:
		if len(e.Entities) > 0 {
			return eventErr("a permission change names capabilities or everything, not records")
		}
	case KindPrincipalChange:
		if len(e.Entities) > 0 || len(e.Capabilities) > 0 || e.Everything {
			return eventErr("a principal change names only the principal")
		}
		if strings.TrimSpace(e.PrincipalID) == "" {
			return eventErr("a principal change names the principal")
		}
	case KindTimeout:
		return eventErr("a timeout is not an event a host can report")
	default:
		return eventErr("unknown event kind")
	}
	if e.PrincipalID != "" && !eventIdentifier(e.PrincipalID) {
		return eventErr("an event names a malformed principal")
	}
	return e.invalidation().Validate()
}

// invalidation is the Invalidation the event stands for.
func (e ApplicationEvent) invalidation() Invalidation {
	return Invalidation{
		Kind:         e.Kind,
		Entities:     slices.Clone(e.Entities),
		Capabilities: slices.Clone(e.Capabilities),
		Everything:   e.Everything,
		PrincipalID:  e.PrincipalID,
	}
}

// Invalidation returns the invalidation the event stands for, after validating it.
func (e ApplicationEvent) Invalidation() (Invalidation, error) {
	if err := e.Validate(); err != nil {
		return Invalidation{}, err
	}
	return e.invalidation(), nil
}

// Notice tells one session what an event did to it. It is delivered only after the
// session's new state is saved, under the scope the session is bound to (its own
// principal and session ID, which is what a stream of turn events is keyed by),
// and only for a session the event changed.
type Notice struct {
	// Scope is the scope the session is bound to.
	Scope contracts.Scope
	// EventID and EventType identify the event; Kind is what sort of event it was.
	EventID   string
	EventType string
	Kind      InvalidationKind
	// Impact is what the event did to this session.
	Impact Impact
	// Events are the TurnEvents that tell the session: a complete turn of its own
	// (one turn_id, seq from 0), made of core data only and no model text. They
	// never carry a prepared action to confirm, a receipt, a directive or a
	// progress stage that suggests work is being done.
	Events []conversation.TurnEvent
}

// Notifier delivers notices to the sessions an event changed. The composition
// root wires it to the turn-event stream; this package does not know the
// stream. A failure is counted and never undoes the invalidation.
type Notifier interface {
	Notify(ctx context.Context, n Notice) error
}

// EventResult is the outcome of ApplyEvent.
type EventResult struct {
	SweepResult
	// Notified is how many notices the Notifier accepted; NotifyFailed how many it
	// refused. Notices are best effort: the sessions are invalidated either way.
	Notified     int
	NotifyFailed int
}

// ApplyEvent invalidates the sessions of the service scope's application, tenant
// and workspace that the event concerns, then tells each session it changed.
//
// It does what InvalidateWorkspace does (the same candidate selection through the
// lookup rows, the same per-session invalidation under the identity each session
// is bound to, the same failure rule: the preparations a session loses are
// invalidated through ivr first and the state is saved only if that worked, so a
// failure leaves the session as it was for the event to be redelivered) and
// additionally reports each changed session, which the aggregate result of
// InvalidateWorkspace cannot. A failure in one session does not stop the others;
// the first is returned after the sweep. Other tenants' and workspaces' sessions
// are never read.
//
// service is the authenticated context of the event source: only its application,
// tenant and workspace are used, and it must carry EventCapability. n may be nil.
// ApplyEvent never prepares, confirms or commits anything and calls no tool.
func (s *Store) ApplyEvent(ctx context.Context, service contracts.Scope, ev ApplicationEvent, ivr Invalidator, n Notifier) (EventResult, error) {
	var out EventResult
	if err := service.Validate(); err != nil {
		return out, err
	}
	if !service.Permits(EventCapability) {
		return out, contracts.New(contracts.ErrPermissionDenied, "sessions: the caller is not an event source")
	}
	inv, err := ev.Invalidation()
	if err != nil {
		return out, err
	}
	if ivr == nil {
		return out, contracts.New(contracts.ErrInternal, "sessions: an event cannot be applied without an invalidator")
	}
	gen := ids.NewGenerator(s.clk)
	sw := s.candidates(service, inv)
	var first error
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return out, contracts.Wrap(contracts.ErrTimeout, err, "sessions: the event sweep was interrupted")
		}
		page, err := s.sweepRows(ctx, service, sw, after)
		if err != nil {
			return out, wrap(err)
		}
		for _, c := range page {
			after = c.seq
			sess, _, err := c.decode()
			if err != nil {
				if first == nil {
					first = wrap(err)
				}
				continue
			}
			saved, imp, err := s.apply(ctx, sess, inv, ivr, applyOpts{})
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			if !imp.Changed() {
				continue
			}
			out.Sessions++
			out.Impact.merge(imp)
			if n == nil {
				continue
			}
			// The session is saved. Tell it, under the scope it is bound to; a
			// refusal to be told is counted and changes nothing.
			notice := Notice{
				Scope: saved.Scope.Clone(), EventID: ev.ID, EventType: ev.Type, Kind: ev.Kind, Impact: imp,
				Events: eventNoticeEvents(act.TurnID(gen.New("trn")), ev, imp),
			}
			if err := n.Notify(ctx, notice); err != nil {
				out.NotifyFailed++
			} else {
				out.Notified++
			}
		}
		if len(page) < s.cfg.SweepBatch {
			return out, first
		}
	}
}

// eventNoticeMessage is the fixed text of a notice. It is the core's, not the
// model's, and says the one thing a user who sees a confirmation vanish needs to
// know.
const eventNoticeMessage = "The application reported a change that invalidated pending state. Nothing was committed."

// eventNoticeEvents builds the TurnEvents that tell a session its state was
// invalidated by an event: a typed stale error, then the done event that ends the
// turn as failed. The schema has no event variant for "state changed", and
// widening it is not this task's to do; a typed stale error is the honest
// existing one (a failed turn is never replaced by a success-looking message) and
// carries the structured facts in its details. Everything in it is core data:
// counts, identifiers and the fixed text above.
//
// A session that ended because the user behind it changed is told nothing about
// what it held: whoever is on the other end of it is not the user who held it.
func eventNoticeEvents(turn act.TurnID, ev ApplicationEvent, imp Impact) []conversation.TurnEvent {
	details := map[string]any{
		"event_id":   ev.ID,
		"event_type": ev.Type,
		"kind":       string(ev.Kind),
	}
	if imp.Ended {
		details["ended"] = true
	} else {
		if len(imp.Voided) > 0 {
			prep := make([]any, 0, len(imp.Voided))
			for _, v := range imp.Voided {
				prep = append(prep, v.PreparedID)
			}
			details["preparation_ids"] = prep
		}
		for k, v := range map[string]int{
			"goals_ended":      imp.GoalsEnded,
			"pending_cleared":  imp.PendingCleared,
			"entities_dropped": imp.EntitiesDropped,
			"results_dropped":  imp.ResultsDropped,
		} {
			if v > 0 {
				details[k] = v
			}
		}
	}
	return []conversation.TurnEvent{
		{Value: conversation.ErrorEvent{
			TurnID: turn, Seq: 0,
			Error: common.Error{Code: common.ErrorCodeStale, Message: eventNoticeMessage, Retryable: false, Details: details},
		}},
		{Value: conversation.DoneEvent{TurnID: turn, Seq: 1, FinishReason: conversation.DoneEventFinishReasonFailed}},
	}
}
