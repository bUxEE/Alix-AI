package sdktest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	sdkgo "github.com/buxee/alix-ai/pkg/sdk-go"
	"github.com/coder/websocket"
)

type session struct {
	claims      Claims
	status      string
	turnCount   int
	created     time.Time
	updated     time.Time
	pendingPrep string
}

// outEvent is one event of a turn, as the stream sends it.
type outEvent struct {
	typ  string
	data []byte
}

type turn struct {
	id     string
	claims Claims
	events []outEvent
}

type prep struct {
	claims  Claims
	action  actions.PreparedAction
	expires time.Time
	receipt *actions.ActionReceipt
	secret  string
	used    bool
	commit  CommitPlan
}

func sessionKey(c Claims) string {
	return c.ApplicationID + "|" + c.TenantID + "|" + c.WorkspaceID + "|" + c.SessionID
}

func sameScope(a, b Claims) bool {
	return sessionKey(a) == sessionKey(b) && a.PrincipalID == b.PrincipalID
}

// effective is the preparation as a client sees it now: a prepared one past its expiry is expired.
func (f *Fake) effective(p *prep) actions.PreparedAction {
	a := p.action
	if a.State == actions.PreparedActionStatePrepared && !f.now().Before(p.expires) {
		a.State = actions.PreparedActionStateExpired
	}
	return a
}

// session returns the session the path names, which must be the token's.
func (c *call) session() (*session, bool) {
	if id := c.param("session_id"); id != c.claims.SessionID {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the path names another session than the token's")
		return nil, false
	}
	return c.f.openSession(c.claims), true
}

func (c *call) sessionDoc(s *session) map[string]any {
	return map[string]any{
		"session_id": s.claims.SessionID, "status": s.status, "turn_count": s.turnCount,
		"created_at": c.f.stamp(s.created), "updated_at": c.f.stamp(s.updated), "pending": c.f.pendingDoc(s),
	}
}

func (f *Fake) pendingDoc(s *session) any {
	p := f.preps[s.pendingPrep]
	if s.status != "active" || p == nil {
		return nil
	}
	a := f.effective(p)
	if a.State != actions.PreparedActionStatePrepared {
		return nil
	}
	return map[string]any{
		"kind": "confirmation", "capability_id": a.CapabilityID, "phase": "awaiting_confirmation",
		"collected_fields": []any{}, "preparation": a, "expires_at": a.ExpiresAt,
	}
}

// openSession is the Open of the real service: the session the token names, created on first sight.
func (f *Fake) openSession(claims Claims) *session {
	key := sessionKey(claims)
	s, ok := f.sessions[key]
	if !ok {
		now := f.now()
		s = &session{claims: claims, status: "active", created: now, updated: now}
		f.sessions[key] = s
	}
	return s
}

func (c *call) createSession() {
	if c.r.ContentLength > 0 {
		raw, ok := c.readBody()
		if !ok {
			return
		}
		var empty struct{}
		if !c.closedObject(raw, &empty) {
			return
		}
	}
	s := c.f.openSession(c.claims)
	if s.status == "ended" {
		c.fail(http.StatusGone, contracts.ErrExpired, "the session has ended")
		return
	}
	c.json(http.StatusOK, c.sessionDoc(s))
}

func (c *call) getSession() {
	s, ok := c.session()
	if !ok {
		return
	}
	if s.status == "ended" {
		c.fail(http.StatusGone, contracts.ErrExpired, "the session has ended")
		return
	}
	c.json(http.StatusOK, c.sessionDoc(s))
}

func (c *call) endSession() {
	s, ok := c.session()
	if !ok {
		return
	}
	if p := c.f.preps[s.pendingPrep]; p != nil && p.action.State == actions.PreparedActionStatePrepared {
		p.action.State = actions.PreparedActionStateInvalidated
	}
	s.status = "ended"
	s.updated = c.f.now()
	c.json(http.StatusOK, map[string]any{"session_id": s.claims.SessionID, "status": "ended"})
}

func (c *call) getPending() {
	s, ok := c.session()
	if !ok {
		return
	}
	if s.status == "ended" {
		c.fail(http.StatusGone, contracts.ErrExpired, "the session has ended")
		return
	}
	c.json(http.StatusOK, map[string]any{"pending": c.f.pendingDoc(s)})
}

// ---- turns ----

type turnRequest struct {
	Text         *string `json:"text"`
	Language     *string `json:"language"`
	ChoiceID     *string `json:"choice_id"`
	Confirmation *struct {
		PreparationID string `json:"preparation_id"`
		Token         string `json:"token"`
	} `json:"confirmation"`
	Reject *struct {
		PreparationID string `json:"preparation_id"`
	} `json:"reject"`
	Attachments []string `json:"attachments"`
}

var (
	confirmationToken = regexp.MustCompile(`^cfm_[A-Za-z0-9_-]{43}$`)
	fileID            = regexp.MustCompile(`^fil_[0-9A-HJKMNP-TV-Z]{26}$`)
)

// input converts and checks the request body: exactly one of text, choice_id, confirmation and reject.
func (t turnRequest) input() (sdkgo.TurnInput, bool) {
	var in sdkgo.TurnInput
	kinds := 0
	if t.Text != nil {
		kinds++
		in.Text = *t.Text
	}
	if t.ChoiceID != nil {
		kinds++
		in.ChoiceID = *t.ChoiceID
	}
	if t.Confirmation != nil {
		kinds++
		in.Confirmation = &sdkgo.ConfirmationInput{PreparationID: t.Confirmation.PreparationID, Token: t.Confirmation.Token}
	}
	if t.Reject != nil {
		kinds++
		in.Reject = &sdkgo.RejectionInput{PreparationID: t.Reject.PreparationID}
	}
	if t.Language != nil {
		in.Language = *t.Language
	}
	in.Attachments = t.Attachments
	ok := kinds == 1 && attachmentsValid(t) && (t.Text == nil || *t.Text != "") && (t.ChoiceID == nil || *t.ChoiceID != "") &&
		(t.Confirmation == nil || confirmationToken.MatchString(t.Confirmation.Token))
	return in, ok
}

// attachmentsValid is the shape check of a turn's files: only with a text, at most four, each a file id.
func attachmentsValid(t turnRequest) bool {
	if t.Attachments == nil {
		return true
	}
	if t.Text == nil || len(t.Attachments) > sdkgo.MaxTurnAttachments {
		return false
	}
	for _, id := range t.Attachments {
		if !fileID.MatchString(id) {
			return false
		}
	}
	return true
}

// eventBuilder numbers the events of one turn.
type eventBuilder struct {
	turnID string
	events []outEvent
}

func (b *eventBuilder) seq() conversation.Seq { return conversation.Seq(len(b.events)) }

func (b *eventBuilder) add(typ string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		panic("sdktest: " + err.Error()) // a programming error of the fake: its own documents always encode
	}
	b.events = append(b.events, outEvent{typ: typ, data: data})
}

func (b *eventBuilder) tid() actions.TurnID { return actions.TurnID(b.turnID) }

// text adds the text as token events of at most 48 characters, so a stream has several events to resume.
func (b *eventBuilder) text(s string) {
	runes := []rune(s)
	for len(runes) > 0 {
		n := min(48, len(runes))
		b.add("token", conversation.TokenEvent{TurnID: b.tid(), Seq: b.seq(), Text: string(runes[:n])})
		runes = runes[n:]
	}
}

func (b *eventBuilder) failure(code contracts.ErrCode, message string) {
	b.add("error", conversation.ErrorEvent{TurnID: b.tid(), Seq: b.seq(),
		Error: common.Error{Code: common.ErrorCode(code), Message: message, Retryable: code.Retryable()}})
}

func (b *eventBuilder) done(reason conversation.DoneEventFinishReason, preparationID string) {
	e := conversation.DoneEvent{TurnID: b.tid(), Seq: b.seq(), FinishReason: reason}
	if preparationID != "" {
		id := actions.PreparationID(preparationID)
		e.PreparationID = &id
	}
	b.add("done", e)
}

func (c *call) sendTurn() {
	s, ok := c.session()
	if !ok {
		return
	}
	if s.status == "ended" {
		c.fail(http.StatusGone, contracts.ErrExpired, "the session has ended")
		return
	}
	if !strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/json") {
		c.fail(http.StatusUnsupportedMediaType, contracts.ErrValidationFailed, "a turn is sent as application/json")
		return
	}
	raw, ok := c.readBody()
	if !ok {
		return
	}
	var req turnRequest
	if !c.closedObject(raw, &req) {
		return
	}
	input, valid := req.input()
	if !valid {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "exactly one of text, choice_id, confirmation and reject is required")
		return
	}

	// The files are resolved together before anything runs: a refused turn calls no handler and is recorded nowhere.
	if !c.f.attachable(c.claims, input.Attachments) {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the file reference does not resolve for this caller")
		return
	}

	b := &eventBuilder{turnID: c.f.typedID("trn")}
	secret := ""
	switch {
	case input.ChoiceID != "":
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "there is no clarification to answer")
		return
	case input.Confirmation != nil:
		c.confirmationTurn(b, s, input.Confirmation)
	case input.Reject != nil:
		c.rejectionTurn(b, s, input.Reject.PreparationID)
	default:
		plan := Reply("Hello from the fake AlixAI.")
		if cb := c.f.onTurn; cb != nil {
			c.f.mu.Unlock() // the test's callback may call the fake
			plan = cb(Turn{SessionID: s.claims.SessionID, Input: input})
			c.f.mu.Lock()
		}
		var proceed bool
		if secret, proceed = c.planEvents(b, s, plan); !proceed {
			return
		}
	}
	t := &turn{id: b.turnID, claims: c.claims, events: b.events}
	c.f.turns[t.id] = t
	s.turnCount++
	s.updated = c.f.now()

	if strings.Contains(c.r.Header.Get("Accept"), "text/event-stream") {
		if secret != "" {
			c.w.Header().Set(sdkgo.ConfirmationTokenHeader, secret)
		}
		c.streamSSE(t, 0, false)
		return
	}
	doc := map[string]any{"turn_id": t.id, "events": rawEvents(t.events)}
	if secret != "" {
		doc["confirmation"] = map[string]any{"preparation_id": s.pendingPrep, "token": secret}
	}
	c.json(http.StatusOK, doc)
}

func rawEvents(events []outEvent) []json.RawMessage {
	out := make([]json.RawMessage, len(events))
	for i, e := range events {
		out[i] = e.data
	}
	return out
}

// planEvents turns a plan into the events of the turn. It returns the confirmation secret, if the turn
// prepared an action that needs one, and false when the plan answered the request itself.
func (c *call) planEvents(b *eventBuilder, s *session, plan TurnPlan) (secret string, proceed bool) {
	switch plan.kind {
	case planReject:
		c.fail(plan.status, plan.code, plan.message)
		return "", false
	case planFail:
		b.failure(plan.code, plan.message)
		b.done(conversation.DoneEventFinishReasonFailed, "")
	case planPrepare:
		in := plan.prepare
		if !contains(c.claims.CapabilityScopes, in.CapabilityID) {
			b.failure(contracts.ErrPermissionDenied, "the capability is not in the token's scopes")
			b.done(conversation.DoneEventFinishReasonFailed, "")
			break
		}
		p := c.f.prepare(c.claims, in)
		s.pendingPrep = string(p.action.PreparationID)
		text := in.Text
		if text == "" {
			text = "Shall I go ahead? " + in.Summary
		}
		b.text(text)
		b.add("prepared_action", conversation.PreparedActionEvent{TurnID: b.tid(), Seq: b.seq(), PreparedAction: p.action})
		b.done(conversation.DoneEventFinishReasonAwaitingConfirmation, string(p.action.PreparationID))
		return p.secret, true
	default:
		b.text(plan.text)
		b.done(conversation.DoneEventFinishReasonCompleted, "")
	}
	return "", true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// confirmationTurn handles a confirmation the user sent as a turn.
func (c *call) confirmationTurn(b *eventBuilder, s *session, conf *sdkgo.ConfirmationInput) {
	p := c.f.preps[conf.PreparationID]
	if p == nil || !sameScope(p.claims, c.claims) {
		b.failure(contracts.ErrScopeViolation, "the preparation is not this session's")
		b.done(conversation.DoneEventFinishReasonFailed, "")
		return
	}
	if code, msg, _ := c.f.confirmCheck(p, conf.Token); code != "" {
		b.failure(code, msg)
		b.done(conversation.DoneEventFinishReasonFailed, "")
		return
	}
	receipt, _ := c.f.commit(p)
	s.pendingPrep = ""
	b.text("The action ended " + string(receipt.Status) + ".")
	b.add("receipt", conversation.ReceiptEvent{TurnID: b.tid(), Seq: b.seq(), Receipt: receipt})
	b.done(conversation.DoneEventFinishReasonCompleted, "")
}

func (c *call) rejectionTurn(b *eventBuilder, s *session, id string) {
	p := c.f.preps[id]
	if p == nil || !sameScope(p.claims, c.claims) {
		b.failure(contracts.ErrScopeViolation, "the preparation is not this session's")
		b.done(conversation.DoneEventFinishReasonFailed, "")
		return
	}
	if p.action.State == actions.PreparedActionStatePrepared {
		p.action.State = actions.PreparedActionStateRejected
	}
	s.pendingPrep = ""
	b.text("Cancelled. Nothing was changed.")
	b.done(conversation.DoneEventFinishReasonCompleted, "")
}

// ---- event streams ----

// popDrop returns the number of events after which the next stream connection is cut, or -1.
func (f *Fake) popDrop() int {
	if len(f.drops) == 0 {
		return -1
	}
	n := f.drops[0]
	f.drops = f.drops[1:]
	return n
}

// streamSSE sends the events of a turn from the index `from` as server-sent events. A queued drop cuts the
// connection after that many events, without `done`.
func (c *call) streamSSE(t *turn, from int, retryHint bool) {
	drop := c.f.popDrop()
	events := t.events[from:]
	c.unlock()
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	c.w.WriteHeader(http.StatusOK)
	flush := func() { http.NewResponseController(c.w).Flush() }
	if retryHint {
		fmt.Fprint(c.w, "retry: 1000\n\n")
	}
	for i, ev := range events {
		if drop >= 0 && i >= drop {
			return
		}
		fmt.Fprintf(c.w, "id: %d\nevent: %s\ndata: %s\n\n", from+i, ev.typ, ev.data)
		flush()
	}
}

var cursorPattern = regexp.MustCompile(`^[0-9]{1,10}$`)

func (c *call) streamTurnEvents() {
	if id := c.param("session_id"); id != c.claims.SessionID {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the path names another session than the token's")
		return
	}
	t := c.f.turns[c.param("turn_id")]
	if t == nil || !sameScope(t.claims, c.claims) {
		c.fail(http.StatusForbidden, contracts.ErrScopeViolation, "the turn is not visible", "reason", "turn_not_visible")
		return
	}
	cursor := -1
	for _, raw := range []string{c.r.Header.Get("Last-Event-ID"), c.r.URL.Query().Get("cursor")} {
		if raw == "" {
			continue
		}
		if !cursorPattern.MatchString(raw) {
			c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the cursor is not a sequence number")
			return
		}
		n, _ := strconv.Atoi(raw)
		cursor = max(cursor, n) // the later of the two wins
	}
	last := len(t.events) - 1
	if cursor > last {
		c.fail(http.StatusBadRequest, contracts.ErrValidationFailed, "the cursor is ahead of the stream")
		return
	}
	if cursor == last {
		c.w.WriteHeader(http.StatusNoContent) // the turn is finished and the client has every event
		return
	}
	if first := max(0, len(t.events)-c.f.retain); c.f.retain > 0 && cursor+1 < first {
		c.fail(http.StatusGone, contracts.ErrExpired, "the cursor is behind the retained window", "reason", "cursor_expired")
		return
	}
	if strings.EqualFold(c.r.Header.Get("Upgrade"), "websocket") {
		c.streamWebSocket(t, cursor+1)
		return
	}
	c.streamSSE(t, cursor+1, true)
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	r.setStatus(http.StatusSwitchingProtocols)
	return http.NewResponseController(r.ResponseWriter).Hijack()
}

// streamWebSocket sends one text message per event, then a normal closure; a queued drop closes with 1013.
func (c *call) streamWebSocket(t *turn, from int) {
	drop := c.f.popDrop()
	events := t.events[from:]
	c.unlock()
	conn, err := websocket.Accept(c.w, c.r, &websocket.AcceptOptions{Subprotocols: []string{sdkgo.WebSocketSubprotocol}})
	if err != nil {
		return // Accept has already written the refusal
	}
	ctx := conn.CloseRead(c.r.Context())
	for i, ev := range events {
		if drop >= 0 && i >= drop {
			_ = conn.Close(websocket.StatusTryAgainLater, "slow_consumer")
			return
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := conn.Write(wctx, websocket.MessageText, ev.data)
		cancel()
		if err != nil {
			return
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}
