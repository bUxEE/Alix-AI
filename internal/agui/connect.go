package agui

import (
	"github.com/buxee/alix-ai/pkg/contracts"
)

// HistoryTurn is one message of the thread's history as the session store keeps it: the user's text or the verified
// assistant reply of a turn. The caller passes what the history service would show the caller (withheld text stays
// withheld); this package adds no text of its own.
type HistoryTurn struct {
	TurnID string
	// Role is "user" or "assistant".
	Role string
	Text string
}

const (
	// MessagesSnapshotMaxBytes bounds the content of a messages snapshot; the newest messages that fit are kept.
	MessagesSnapshotMaxBytes = 192 << 10
	messageMaxRunes          = 8000
)

// HistoryMessages renders the thread's history as the messages of a MESSAGES_SNAPSHOT, oldest first. Only user and
// assistant messages exist in it. Ids are derived from the turn id (msg_<turn>_user, msg_<turn>_1, the first message of
// an assistant turn, like the live stream's), so the same history always gives the same snapshot. The newest messages
// that fit MessagesSnapshotMaxBytes are kept, each cut to messageMaxRunes.
func HistoryMessages(turns []HistoryTurn) []Message {
	var rev []Message
	size := 0
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		var m Message
		switch t.Role {
		case "user":
			m = Message{ID: "msg_" + t.TurnID + "_user", Role: RoleUser}
		case "assistant":
			m = Message{ID: "msg_" + t.TurnID + "_1", Role: RoleAssistant}
		default:
			continue
		}
		m.Content = clipRunes(t.Text, messageMaxRunes)
		if size+len(m.Content)+len(m.ID) > MessagesSnapshotMaxBytes {
			break
		}
		size += len(m.Content) + len(m.ID)
		rev = append(rev, m)
	}
	out := make([]Message, 0, len(rev))
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

// Connect produces the events of a connect run (a run with no message: a client that joins a thread asks for where it
// stands): RUN_STARTED, the history as a MESSAGES_SNAPSHOT, the client-safe state, RUN_FINISHED. No turn runs and no
// model is called. Every event carries Seq 0: a connect run has no TurnEvents, and a client that reconnects simply
// connects again.
func Connect(rc RunContext, history []HistoryTurn) ([]Emitted, error) {
	m, err := NewMapper(rc)
	if err != nil {
		return nil, err
	}
	b := &batch{m: m}
	b.emit(NewRunStarted(rc.ThreadID, rc.RunID))
	b.emit(NewMessagesSnapshot(HistoryMessages(history)))
	if rc.StateBefore != nil {
		if ev, err := InitialState(m.rc.StateMode, *rc.StateBefore); err == nil {
			b.emit(ev)
		}
	}
	b.emit(NewRunFinished(rc.ThreadID, rc.RunID, map[string]any{"connect": true}, &Outcome{Type: "success"}))
	if b.err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, b.err, "agui: the connect events could not be encoded")
	}
	return b.out, nil
}

// UIResultRun produces the events of the run a client starts to hand back the result of a UI tool call (Request.UIResult):
// RUN_STARTED, the client-safe state, RUN_FINISHED with a success outcome. No turn runs, no model is called and nothing is
// looked up: the result is untrusted data that the client resends with the thread, so it reaches the NEXT turn as one
// more line of client context. Unlike Connect it sends no MESSAGES_SNAPSHOT: the client already holds the tool call and
// its result, and a snapshot of the stored history (user and assistant text only) would replace them, so the run needs no
// history service and no history permission either. Every event carries Seq 0.
func UIResultRun(rc RunContext) ([]Emitted, error) {
	m, err := NewMapper(rc)
	if err != nil {
		return nil, err
	}
	b := &batch{m: m}
	b.emit(NewRunStarted(rc.ThreadID, rc.RunID))
	if rc.StateBefore != nil {
		if ev, err := InitialState(m.rc.StateMode, *rc.StateBefore); err == nil {
			b.emit(ev)
		}
	}
	b.emit(NewRunFinished(rc.ThreadID, rc.RunID, map[string]any{"uiResult": true}, &Outcome{Type: "success"}))
	if b.err != nil {
		return nil, contracts.Wrap(contracts.ErrInternal, b.err, "agui: the UI result run could not be encoded")
	}
	return b.out, nil
}
