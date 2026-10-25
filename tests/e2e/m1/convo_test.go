package m1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// proposalJSON is what a model writes as a TaskProposal: the capability (empty for none), the
// intent, and the field values it read out of the user's words.
func proposalJSON(capability, intent string, fields map[string]any) string {
	m := map[string]any{
		"capability_id": nil, "fields": map[string]any{}, "entity_refs": []any{}, "intent": intent,
		"uncertainty": map[string]any{"overall": 0.9},
	}
	if capability != "" {
		m["capability_id"] = capability
	}
	if fields != nil {
		m["fields"] = fields
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// convo is one conversation: a principal, in a session, talking to the service over HTTP.
type convo struct {
	t       tb
	r       *rig
	w       *world
	p       *person
	session string
}

func (r *rig) convo(t tb, w *world, p *person) *convo {
	return &convo{t: t, r: r, w: w, p: p, session: "ses_gate_" + randomSuffix()}
}

// token is a fresh single-use token of the conversation's principal and session.
func (c *convo) token() string {
	c.t.Helper()
	return c.w.token(c.t, c.p, c.session)
}

func (c *convo) call(method, path string, body any, headers ...string) apiReply {
	c.t.Helper()
	return c.r.call(c.token(), method, path, body, headers...)
}

// open opens the session, which the first token of a session does.
func (c *convo) open() apiReply {
	c.t.Helper()
	return c.call(http.MethodPost, "/v1/sessions", nil)
}

func (c *convo) turnsPath() string { return "/v1/sessions/" + url.PathEscape(c.session) + "/turns" }

// say sends the user's words as one turn.
func (c *convo) say(text, lang string) *turn {
	c.t.Helper()
	return c.turn(map[string]any{"text": text, "language": lang})
}

// confirm presents the structured confirmation of a preparation, as the client UI does.
func (c *convo) confirm(preparationID, token string) *turn {
	c.t.Helper()
	return c.turn(map[string]any{"confirmation": map[string]any{"preparation_id": preparationID, "token": token}})
}

// turn sends one turn request and reads what came back.
func (c *convo) turn(body any) *turn {
	c.t.Helper()
	return readTurn(c.t, c.call(http.MethodPost, c.turnsPath(), body))
}

// confirmAction confirms through the actions route instead of a turn.
func (c *convo) confirmAction(preparationID, token string) apiReply {
	c.t.Helper()
	return c.call(http.MethodPost, "/v1/actions/"+url.PathEscape(preparationID)+"/confirm", map[string]any{"token": token})
}

func (c *convo) preparation(preparationID string) apiReply {
	c.t.Helper()
	return c.call(http.MethodGet, "/v1/actions/"+url.PathEscape(preparationID), nil)
}

func (c *convo) pending() apiReply {
	c.t.Helper()
	return c.call(http.MethodGet, "/v1/sessions/"+url.PathEscape(c.session)+"/pending", nil)
}

// ---- what a turn says ----------------------------------------------------------------------

// turn is the answer to one turn request.
type turn struct {
	Reply apiReply
	// Body is the decoded JSON turn document; zero when the request was refused before a turn ran.
	Body gateway.TurnBody
	ok   bool
}

func readTurn(t tb, reply apiReply) *turn {
	t.Helper()
	tr := &turn{Reply: reply}
	if reply.Status == http.StatusOK && strings.HasPrefix(reply.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(reply.Body, &tr.Body); err != nil {
			t.Fatalf("the turn document does not decode: %v\n%s", err, reply.Body)
		}
		tr.ok = true
	}
	return tr
}

// ran reports whether a turn ran: the request was accepted and answered with the turn's events.
func (tr *turn) ran() bool { return tr.ok }

func (tr *turn) String() string {
	if !tr.ok {
		return tr.Reply.String()
	}
	return fmt.Sprintf("turn %s: act %s, finish %s, code %q, text %q", tr.Body.TurnID, tr.act(), tr.finish(), tr.code(), tr.text())
}

func (tr *turn) directive() (conversation.DialogueDirective, bool) {
	for _, ev := range tr.Body.Events {
		if d, ok := ev.Value.(conversation.DirectiveEvent); ok {
			return d.Directive, true
		}
	}
	return conversation.DialogueDirective{}, false
}

// act is the dialogue act the core chose.
func (tr *turn) act() string {
	d, _ := tr.directive()
	return string(d.Act)
}

// prepared is the PreparedAction the turn announced, if it did.
func (tr *turn) prepared() *act.PreparedAction {
	for _, ev := range tr.Body.Events {
		if p, ok := ev.Value.(conversation.PreparedActionEvent); ok {
			pa := p.PreparedAction
			return &pa
		}
	}
	return nil
}

// receipt is the ActionReceipt the turn reported, if it did.
func (tr *turn) receipt() *act.ActionReceipt {
	for _, ev := range tr.Body.Events {
		if r, ok := ev.Value.(conversation.ReceiptEvent); ok {
			rc := r.Receipt
			return &rc
		}
	}
	return nil
}

// text is the response the user reads: the token events, in order.
func (tr *turn) text() string {
	var sb strings.Builder
	for _, ev := range tr.Body.Events {
		if tk, ok := ev.Value.(conversation.TokenEvent); ok {
			sb.WriteString(tk.Text)
		}
	}
	return sb.String()
}

// errorEvent is the typed error the turn carries, if it failed.
func (tr *turn) errorEvent() (conversation.ErrorEvent, bool) {
	for _, ev := range tr.Body.Events {
		if e, ok := ev.Value.(conversation.ErrorEvent); ok {
			return e, true
		}
	}
	return conversation.ErrorEvent{}, false
}

func (tr *turn) finish() conversation.DoneEventFinishReason {
	for _, ev := range tr.Body.Events {
		if d, ok := ev.Value.(conversation.DoneEvent); ok {
			return d.FinishReason
		}
	}
	return ""
}

// code is the ErrCode the request ended with: the HTTP error envelope's when the request was
// refused before a turn ran, else the turn's error event's, else none.
func (tr *turn) code() contracts.ErrCode {
	if !tr.ok {
		return tr.Reply.code()
	}
	if e, ok := tr.errorEvent(); ok {
		return contracts.ErrCode(e.Error.Code)
	}
	return ""
}

// confirmation is the single-use secret that came with the prepared action.
func (tr *turn) confirmation() (preparationID, token string) {
	if tr.Body.Confirmation == nil {
		return "", ""
	}
	return tr.Body.Confirmation.PreparationID, tr.Body.Confirmation.Token
}

// factValue renders a fact the way the verifier compares it: the display form when the host gave
// one, else the canonical value.
func factValue(f act.Fact) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	var m struct {
		Value struct {
			Value any `json:"value"`
		} `json:"value"`
		Display string `json:"display"`
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if dec.Decode(&m) != nil {
		return ""
	}
	if m.Display != "" {
		return m.Display
	}
	return fmt.Sprint(m.Value.Value)
}

// facts maps field name to rendered value for a list of facts (the last one wins for a repeated
// field).
func factMap(fs []act.Fact) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		out[string(f.Field)] = factValue(f)
	}
	return out
}

// unescape removes the Markdown escapes the response sanitiser puts before punctuation
// (an identifier's underscore becomes a backslash and an underscore), so a response can be held
// to the facts it states.
func unescape(s string) string {
	var sb strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == '\\' && i+1 < len(rs) {
			continue
		}
		sb.WriteRune(rs[i])
	}
	return sb.String()
}

// factKind is the kind of a fact's canonical value (text, identifier, integer, boolean, date,
// datetime, ...).
func factKind(f act.Fact) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	var m struct {
		Value struct {
			Kind string `json:"kind"`
		} `json:"value"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Value.Kind
}

// stated reports whether a response states a fact in a form a test can look for verbatim: text,
// identifiers and integers by their value, and anything with the application's display form by
// that form. Booleans and dates are written by the template in the response language's own
// wording and are checked by the verifier, not by substring.
func stated(f act.Fact, response string) (checked, ok bool) {
	text := unescape(response)
	if f.Display != nil && *f.Display != "" {
		return true, strings.Contains(text, *f.Display)
	}
	switch factKind(f) {
	case "text", "identifier", "integer":
		return true, strings.Contains(text, factValue(f))
	}
	return false, false
}

// ---- the event stream ----------------------------------------------------------------------

// sseFrame is one server-sent event as it went over the wire.
type sseFrame struct {
	ID    string
	Event string
	Data  string
}

// sayStream sends the user's words and asks for the turn as server-sent events.
func (c *convo) sayStream(text, lang string) (*turn, []sseFrame) {
	c.t.Helper()
	reply := c.call(http.MethodPost, c.turnsPath(), map[string]any{"text": text, "language": lang}, "Accept", "text/event-stream")
	return readStream(c.t, reply)
}

// readStream reads an event-stream turn into the same shape as a JSON turn: the events in order,
// and the confirmation secret from the header it travels in (it is no event).
func readStream(t tb, reply apiReply) (*turn, []sseFrame) {
	t.Helper()
	tr := &turn{Reply: reply}
	if reply.Status != http.StatusOK || !strings.HasPrefix(reply.Header.Get("Content-Type"), "text/event-stream") {
		return tr, nil
	}
	var frames []sseFrame
	for _, block := range strings.Split(strings.TrimSpace(string(reply.Body)), "\n\n") {
		var f sseFrame
		for _, line := range strings.Split(block, "\n") {
			switch k, v, _ := strings.Cut(line, ": "); k {
			case "id":
				f.ID = v
			case "event":
				f.Event = v
			case "data":
				f.Data = v
			}
		}
		frames = append(frames, f)
		var ev conversation.TurnEvent
		if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
			t.Fatalf("an event of the stream does not decode: %v: %s", err, f.Data)
		}
		tr.Body.Events = append(tr.Body.Events, ev)
		if d, ok := ev.Value.(conversation.DirectiveEvent); ok {
			tr.Body.TurnID = string(d.TurnID)
		}
	}
	if secret := reply.Header.Get(gateway.HeaderConfirmationToken); secret != "" {
		pid := ""
		for _, ev := range tr.Body.Events {
			if d, ok := ev.Value.(conversation.DoneEvent); ok && d.PreparationID != nil {
				pid = string(*d.PreparationID)
			}
		}
		tr.Body.Confirmation = &gateway.ConfirmationGrant{PreparationID: pid, Token: secret}
	}
	tr.ok = true
	return tr, frames
}

// application is what the API reports of the application: its status and its installed packages.
func (c *convo) application() (status string, packages []gateway.PackageBody, reply apiReply) {
	c.t.Helper()
	reply = c.call(http.MethodGet, "/v1/applications/"+clinicApplication, nil)
	if reply.Status != http.StatusOK {
		return "", nil, reply
	}
	var body gateway.ApplicationBody
	if err := json.Unmarshal(reply.Body, &body); err != nil {
		c.t.Fatalf("the application body does not decode: %v: %s", err, reply.Body)
	}
	return body.Status, body.Packages, reply
}
