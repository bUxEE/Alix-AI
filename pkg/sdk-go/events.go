package sdkgo

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Turn events arrive from a server the SDK does not control (a proxy sits in between, a new server may be
// older or newer). Decoding is the generated, strict one: an unknown event type, an unknown member or a null
// for a required one is an error, never an event to skip. On top of that the SDK enforces what the API
// promises about the sequence of a turn: one turn_id throughout, seq running without gaps, `done` last, and,
// for server-sent events, an `event:` and an `id:` that agree with the event they carry.

func malformedEvent(reason string, extra ...any) *contracts.Error {
	return localError(contracts.ErrInternal, "the server sent a malformed turn event: "+reason,
		append([]any{"reason", "malformed_event"}, extra...)...)
}

var turnIDPattern = regexp.MustCompile(`^trn_[0-9A-HJKMNP-TV-Z]{26}$`)

const (
	maxEventSeq       = 4294967295 // schemas/conversation/turn-event: seq maximum
	maxTokenTextRunes = 4096       // schemas/conversation/turn-event: tokenEvent.text maxLength
)

// parseTurnEvent decodes one event and refuses what the turn-event schema refuses.
//
// The generated decoder is shape-only and Go's encoding/json matches member names without regard to case and
// fills a missing member with its zero value, so on its own it delivers {"tYpe":"token","turn_id":"..."} as a
// token with seq 0 and no text (SEC-097). Three checks close that, each failing closed:
//   - the document is read again with exact member names, and what the typed value encodes back to must be the
//     same document: a member in another case, a missing required member, a null for a member that may not be
//     null, an unknown member and any difference in a nested value all show up as a difference;
//   - the envelope is checked against the schema's constraints (turn_id pattern, seq range);
//   - a token's text has the schema's length (1 to 4096 characters).
func parseTurnEvent(data []byte) (conversation.TurnEvent, error) {
	var ev conversation.TurnEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return ev, malformedEvent(err.Error())
	}
	if ev.Value == nil {
		return ev, malformedEvent("an event must be a JSON object")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return ev, malformedEvent("an event must be a JSON object")
	}
	if _, ok := members["type"]; !ok {
		return ev, malformedEvent(`the member "type" is missing (member names are case-sensitive)`)
	}
	enc, err := json.Marshal(ev)
	if err != nil {
		return ev, malformedEvent(err.Error())
	}
	var sent, canonical any
	if err := json.Unmarshal(data, &sent); err != nil {
		return ev, malformedEvent(err.Error())
	}
	if err := json.Unmarshal(enc, &canonical); err != nil {
		return ev, malformedEvent(err.Error())
	}
	if !reflect.DeepEqual(sent, canonical) {
		return ev, malformedEvent("the event differs from its schema form: a required member is missing, a member name is in another case, or a value is null where the schema allows none")
	}
	if env, ok := Envelope(ev); ok {
		if !turnIDPattern.MatchString(env.TurnID) {
			return ev, malformedEvent("turn_id is not a turn id")
		}
		if env.Seq < 0 || env.Seq > maxEventSeq {
			return ev, malformedEvent("seq is out of range")
		}
	}
	if tok, ok := ev.Value.(conversation.TokenEvent); ok {
		if n := utf8.RuneCountInString(tok.Text); n < 1 || n > maxTokenTextRunes {
			return ev, malformedEvent("a token's text must be 1 to 4096 characters")
		}
	}
	if why := turnEventRules(ev); why != "" {
		return ev, malformedEvent(why)
	}
	return ev, nil
}

// turnEventRules checks what the schemas of the event's payload add to the shape the generated decoder
// reads: the cross-member rules (if/then) and the bounds of the nested documents. They mirror
// schemas/conversation/turn-event and the documents it references (common/error, conversation/citation,
// actions/prepared-action, shared and action-receipt); TestParseTurnEventAgreesWithTheSchemaExamples runs the
// schemas' own valid and invalid examples through them, so a rule that drifts from its schema fails there.
func turnEventRules(ev conversation.TurnEvent) string {
	switch v := ev.Value.(type) {
	case conversation.DoneEvent:
		if (v.FinishReason == conversation.DoneEventFinishReasonAwaitingConfirmation) != (v.PreparationID != nil) {
			return "done.preparation_id is present exactly when finish_reason is awaiting_confirmation"
		}
	case conversation.ErrorEvent:
		if v.Error.Retryable != turnErrorRetryable(v.Error.Code) {
			return "error.retryable does not match the error code"
		}
	case conversation.ProgressEvent:
		if v.Percent != nil && (*v.Percent < 0 || *v.Percent > 100) {
			return "progress.percent must be 0 to 100"
		}
	case conversation.CitationEvent:
		if n := len(v.Citation.Claims); n < 1 || n > 32 {
			return "a citation needs 1 to 32 claims"
		}
	case conversation.PreparedActionEvent:
		return preparedActionRules(v.PreparedAction)
	case conversation.ReceiptEvent:
		return receiptRules(v.Receipt)
	}
	return ""
}

// turnErrorRetryable is the schema's rule: an error is retryable exactly for these codes.
func turnErrorRetryable(code common.ErrorCode) bool {
	switch code {
	case common.ErrorCodeUnavailable, common.ErrorCodeDegraded, common.ErrorCodeTimeout,
		common.ErrorCodeRateLimited, common.ErrorCodeResourceExhausted:
		return true
	}
	return false
}

func preparedActionRules(p actions.PreparedAction) string {
	mode, bindings := p.Confirmation.Mode, p.Confirmation.Bindings
	switch {
	case p.RiskLevel >= actions.RiskLevel2 && mode == actions.ConfirmationRequirementModeNone:
		return "a risk level of 2 or 3 is never executed without confirmation"
	case p.RiskLevel == actions.RiskLevel3 && mode != actions.ConfirmationRequirementModeStepUp:
		return "risk level 3 needs step_up confirmation"
	case mode == actions.ConfirmationRequirementModeNone && len(bindings) != 0:
		return "a confirmation of mode none has no bindings"
	case mode != actions.ConfirmationRequirementModeNone && len(bindings) != 4:
		return "a confirmation is bound to all four items"
	}
	seen := map[actions.ConfirmationRequirementBindingsItem]bool{}
	for _, b := range bindings {
		if seen[b] {
			return "confirmation bindings must be unique"
		}
		seen[b] = true
	}
	for _, e := range p.Effects {
		if e.External && p.RiskLevel < actions.RiskLevel2 {
			return "an external side effect is risk level 2 or higher"
		}
	}
	return ""
}

func receiptRules(r actions.ActionReceipt) string {
	switch r.Status {
	case actions.ActionReceiptStatusCommitted, actions.ActionReceiptStatusQueued:
		if len(r.Errors) != 0 {
			return "a committed or queued receipt carries no errors"
		}
	default:
		if len(r.Errors) == 0 {
			return "a receipt that is not committed or queued carries at least one error"
		}
	}
	if r.Status == actions.ActionReceiptStatusRejected && len(r.Records) != 0 {
		return "a rejected receipt names no records"
	}
	if r.Status == actions.ActionReceiptStatusQueued && r.FollowUp == nil {
		return "a queued receipt needs follow_up"
	}
	return ""
}

// EventEnvelope is what every TurnEvent carries besides its payload.
type EventEnvelope struct {
	Type   string
	TurnID string
	Seq    int64
}

// Envelope returns the type, turn_id and seq of an event. ok is false for the zero TurnEvent.
func Envelope(ev conversation.TurnEvent) (e EventEnvelope, ok bool) {
	switch v := ev.Value.(type) {
	case conversation.TokenEvent:
		return EventEnvelope{"token", string(v.TurnID), int64(v.Seq)}, true
	case conversation.DirectiveEvent:
		return EventEnvelope{"directive", string(v.TurnID), int64(v.Seq)}, true
	case conversation.ClarificationEvent:
		return EventEnvelope{"clarification", string(v.TurnID), int64(v.Seq)}, true
	case conversation.PreparedActionEvent:
		return EventEnvelope{"prepared_action", string(v.TurnID), int64(v.Seq)}, true
	case conversation.ReceiptEvent:
		return EventEnvelope{"receipt", string(v.TurnID), int64(v.Seq)}, true
	case conversation.CitationEvent:
		return EventEnvelope{"citation", string(v.TurnID), int64(v.Seq)}, true
	case conversation.ProgressEvent:
		return EventEnvelope{"progress", string(v.TurnID), int64(v.Seq)}, true
	case conversation.ErrorEvent:
		return EventEnvelope{"error", string(v.TurnID), int64(v.Seq)}, true
	case conversation.DoneEvent:
		return EventEnvelope{"done", string(v.TurnID), int64(v.Seq)}, true
	case conversation.BlockEvent:
		return EventEnvelope{"block", string(v.TurnID), int64(v.Seq)}, true
	}
	return EventEnvelope{}, false
}

// turnSequence enforces the sequence of one turn.
type turnSequence struct {
	turnID  string
	nextSeq int64
	done    bool
}

func (s *turnSequence) accept(ev conversation.TurnEvent, fr *frame) error {
	env, ok := Envelope(ev)
	if !ok || env.TurnID == "" || env.Seq < 0 {
		return malformedEvent("an event needs a turn_id and a seq")
	}
	if s.done {
		return malformedEvent("an event follows the done event")
	}
	if s.turnID == "" {
		s.turnID = env.TurnID
	} else if env.TurnID != s.turnID {
		return malformedEvent("the turn_id changed within one turn", "seq", env.Seq)
	}
	if env.Seq != s.nextSeq {
		return localError(contracts.ErrInternal,
			fmt.Sprintf("the event sequence is broken: expected seq %d, got %d", s.nextSeq, env.Seq),
			"reason", "sequence_gap", "expected", s.nextSeq, "got", env.Seq)
	}
	if fr != nil && fr.framed {
		if fr.event != env.Type {
			return malformedEvent("the frame's event name differs from the event type", "seq", env.Seq)
		}
		if !fr.hasID || fr.id != strconv.FormatInt(env.Seq, 10) {
			return malformedEvent("the frame's id differs from the event's seq", "seq", env.Seq)
		}
	}
	if done, isDone := ev.Value.(conversation.DoneEvent); isDone {
		awaiting := done.FinishReason == conversation.DoneEventFinishReasonAwaitingConfirmation
		if awaiting != (done.PreparationID != nil) {
			return malformedEvent("done.preparation_id is present exactly when finish_reason is awaiting_confirmation")
		}
		s.done = true
	}
	s.nextSeq++
	return nil
}

// finish fails unless `done` was accepted: a stream that ends earlier is an unknown outcome.
func (s *turnSequence) finish() error {
	if s.done {
		return nil
	}
	return localError(contracts.ErrUncertainOutcome,
		"the turn ended before its done event: what the turn did is unknown; read the session's pending state before sending anything again",
		"reason", "stream_ended_early", "received", s.nextSeq)
}

// TurnText is the text of a turn: its token events, joined in order.
func TurnText(events []conversation.TurnEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if t, ok := ev.Value.(conversation.TokenEvent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// DoneOf returns the `done` event of a finished turn.
func DoneOf(events []conversation.TurnEvent) (conversation.DoneEvent, bool) {
	if len(events) == 0 {
		return conversation.DoneEvent{}, false
	}
	d, ok := events[len(events)-1].Value.(conversation.DoneEvent)
	return d, ok
}
