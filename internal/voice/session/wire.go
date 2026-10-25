package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	alog "github.com/buxee/alix-ai/internal/log"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	voicewire "github.com/buxee/alix-ai/pkg/contracts/gen/voice"
)

// maxErrorMessage bounds the message of an error event.
const maxErrorMessage = 512

func commonIdentifier(s string) common.Identifier { return common.Identifier(s) }

// eventName names an event for a log line.
func eventName(ev voicewire.ServerEventValue) string { return fmt.Sprintf("%T", ev) }

// wireError is the typed error of an event and the degradation effect it names.
// Only typed fields leave: an untyped failure is internal with a fixed message,
// and a typed message is bounded and scrubbed of anything credential-shaped.
func wireError(err error) (common.Error, voicewire.Effect) {
	code := contracts.CodeOf(err)
	var ce *contracts.Error
	switch {
	case code == contracts.ErrInternal && errors.Is(err, context.DeadlineExceeded):
		return common.Error{Code: common.ErrorCode(contracts.ErrTimeout), Message: "the voice component timed out", Retryable: true}, ""
	case code == contracts.ErrInternal && (errors.Is(err, context.Canceled) || errors.Is(err, providers.ErrStreamClosed)):
		return common.Error{Code: common.ErrorCode(contracts.ErrUnavailable), Message: "the voice request was cancelled", Retryable: true}, ""
	case code == contracts.ErrInternal || !errors.As(err, &ce) || ce == nil:
		return common.Error{Code: common.ErrorCode(contracts.ErrInternal), Message: "the voice session could not complete the request"}, ""
	}
	msg := alog.RedactString(ce.Message)
	if utf8.RuneCountInString(msg) > maxErrorMessage {
		msg = string([]rune(msg)[:maxErrorMessage])
	}
	out := common.Error{Code: common.ErrorCode(ce.Code), Message: msg, Retryable: ce.Code.Retryable()}
	var effect voicewire.Effect
	if e, ok := ce.Details["effect"].(string); ok {
		if v := voicewire.Effect(e); v.Valid() {
			effect = v
		}
	}
	return out, effect
}

// emitError sends the error event of err, tagged with the utterance and the
// turn it belongs to when there are any.
func (s *Session) emitError(err error, utterance, turn string) {
	ce, effect := wireError(err)
	ev := voicewire.Error{Error: ce}
	if effect != "" {
		ev.Effect = &effect
	}
	if utterance != "" {
		u := voicewire.UtteranceID(utterance)
		ev.UtteranceID = &u
	}
	if turn != "" {
		t := actions.TurnID(turn)
		ev.TurnID = &t
	}
	s.emit(ev)
}

// withEffect returns err's effect-bearing form for a voice role that failed:
// the failure of a speech component is always a degradation the client can
// act on (type instead, read instead of listen, end the capture yourself).
func withEffect(err error, role providers.Role) error {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		if _, has := ce.Details["effect"]; has {
			return err
		}
		if ce.Code == contracts.ErrUnavailable || ce.Code == contracts.ErrTimeout || ce.Code == contracts.ErrResourceExhausted || ce.Code == contracts.ErrSchemaInvalid {
			c := *ce
			c.Details = map[string]any{}
			for k, v := range ce.Details {
				c.Details[k] = v
			}
			c.Details["effect"] = role.Effect()
			return &c
		}
		return err
	}
	if err == nil {
		return nil
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, "voice: the "+strings.ToLower(role.String())+" component failed").
		WithDetails("effect", role.Effect())
}
