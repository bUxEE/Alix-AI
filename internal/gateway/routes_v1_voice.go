package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/dialogue"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/voice"
	"github.com/buxee/alix-ai/internal/stream"
	vsession "github.com/buxee/alix-ai/internal/voice/session"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The voice area of the public API (master plan 13 and 14.2, "Voice"). The
// contract is schemas/api/paths/voice.yaml and the messages are schemas/voice;
// TestRoutesMatchOpenAPISpec fails on any difference in either direction.
//
//	GET /v1/sessions/{session_id}/voice    one voice conversation, as a WebSocket
//
// # What it is
//
// Voice is an installable interface to the SAME session as text. The route
// authenticates like every /v1 route, refuses everything that can be refused
// with an HTTP status BEFORE the upgrade (the token, the session in the path, a
// quarantined application, a deployment with no speech recognition, a request
// that is not a WebSocket upgrade), and then hands the connection to a voice
// session (internal/voice/session) that speaks the protocol of schemas/voice.
// The scope of everything the connection does is the verified token's, whole:
// no message carries a tenant, a workspace, a principal or a role, and the
// token's single use is spent by the upgrade request. The connection lives no
// longer than the token (Options.Deadline is the token's expiry).
//
// # The turn
//
// A finalised transcript runs through the dialogue engine like a typed message,
// and the route does for it what POST /v1/sessions/{session_id}/turns does for
// a text one: the events of the turn are published into the stream hub
// (voiceTurns), so GET .../turns/{turn_id}/events resumes a voice turn like any
// other, and the confirmation secret of a prepared action is withheld when the
// hub refused an event (the user would be confirming something the stream did
// not show). The secret leaves in the turn.completed event of the voice socket
// to the client's own confirm control, and the only way to confirm is the
// structured input of the turns route: voice has no way to confirm anything.
//
// # What it needs
//
// Only the ApplicationService: an area that Needs more breaks every test rig
// that provides the application service and not the voice stack
// (the project convention for new /v1 areas and the test rigs). The voice
// service, the engine and the stream hub are optional (Want): without the
// first two the route answers 503 unavailable with the text_input_only effect
// of master plan 18 (the text API is unaffected), and without the hub a voice
// turn is simply not resumable.

func init() { RegisterArea("v1_voice", registerV1Voice) }

// VoiceService is the part of the voice session service the route uses
// (*vsession.Service). The composition root provides it only when the voice
// profile is loaded; a text-only deployment has none.
type VoiceService interface {
	Open(ctx context.Context, scope contracts.Scope, o vsession.Options) (*vsession.Session, error)
}

var _ VoiceService = (*vsession.Service)(nil)

func registerV1Voice(rt *Router, d *Deps) {
	n := d.Needs()
	apps := Need[ApplicationService](n)
	voiceSvc, _ := Want[VoiceService](n)
	engine, _ := Want[*dialogue.Engine](n)
	hub, _ := Want[*stream.Hub](n)
	if !n.Ready() {
		return
	}
	h := &voiceHandler{apps: apps, svc: voiceSvc, engine: engine, turner: turnerFor(n, engine), hub: hub}
	rt.V1().HandleFunc("GET /sessions/{session_id}/voice", h.serve)
}

type voiceHandler struct {
	apps   ApplicationService
	svc    VoiceService
	engine *dialogue.Engine
	turner ddec.Turner // the decision wrapper when provided, else the engine (turnerFor)
	hub    *stream.Hub
}

func (h *voiceHandler) serve(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	// A quarantined application serves no traffic, voice included.
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		w.Header().Set("Upgrade", "websocket")
		WriteErrorStatus(w, r, http.StatusUpgradeRequired, contracts.New(contracts.ErrValidationFailed, "this route is a WebSocket: ask for Upgrade: websocket"))
		return
	}
	if h.svc == nil || h.engine == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "voice is not available on this deployment").
			WithDetails("effect", providers.RoleSTT.Effect()))
		return
	}
	q := r.URL.Query()
	language := q.Get("language")
	if language != "" && !voice.ValidLanguage(language) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the language is not a language tag"))
		return
	}
	speak := q.Get("speak")
	if speak != "" && speak != "true" && speak != "false" {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "speak is true or false"))
		return
	}
	opts := vsession.Options{
		Turns:    voiceTurns{turner: h.turner, hub: h.hub, log: loggerFrom(r.Context())},
		Language: language,
		NoSpeech: speak == "false",
		Logger:   loggerFrom(r.Context()),
	}
	// The connection never outlives the credential it was opened with.
	if v, ok := identity.VerifiedFromContext(r.Context()); ok {
		opts.Deadline = v.ExpiresAt
	}
	sess, err := h.svc.Open(r.Context(), scope, opts)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		// Accept has written the refusal (a bad Origin is 403).
		sess.Close(vsession.ReasonClientClosed)
		return
	}
	conn.SetReadLimit(sess.ReadLimit())
	reason := sess.Serve(r.Context(), voiceSocket{c: conn})
	switch reason {
	case vsession.ReasonClientClosed:
		_ = conn.Close(websocket.StatusNormalClosure, string(reason))
	case vsession.ReasonProtocol:
		_ = conn.Close(websocket.StatusPolicyViolation, string(reason))
	case vsession.ReasonWriteFailed:
		_ = conn.CloseNow()
	default:
		_ = conn.Close(websocket.StatusGoingAway, string(reason))
	}
}

// voiceSocket is the WebSocket under a voice session.
type voiceSocket struct{ c *websocket.Conn }

func (v voiceSocket) Read(ctx context.Context) (bool, []byte, error) {
	typ, data, err := v.c.Read(ctx)
	return typ == websocket.MessageBinary, data, err
}

func (v voiceSocket) Write(ctx context.Context, binary bool, data []byte) error {
	typ := websocket.MessageText
	if binary {
		typ = websocket.MessageBinary
	}
	return v.c.Write(ctx, typ, data)
}

// voiceTurns runs the text of a finalised transcript as a turn and publishes
// what the engine decided into the stream hub, the way the turns route does,
// so that the turn is resumable and visible to AG-UI like any other.
type voiceTurns struct {
	turner ddec.Turner
	hub    *stream.Hub
	log    *slog.Logger
}

func (v voiceTurns) RunTextTurn(ctx context.Context, scope contracts.Scope, text, language string) (dialogue.Result, error) {
	res, terr := v.turner.Turn(ctx, scope, dialogue.TurnInput{Text: text, Language: language})
	if res.TurnID == "" {
		// The turn could not be run: an error, never a result that ends well.
		if terr == nil {
			terr = contracts.New(contracts.ErrInternal, "the turn produced no result")
		}
		return res, terr
	}
	if v.hub == nil {
		return res, terr
	}
	turn, err := v.hub.Begin(scope, res.TurnID)
	if err != nil {
		v.log.Error("the voice turn ran and its event stream could not be opened", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(err)), "code", string(contracts.CodeOf(err)))
		res.Confirmation = actions.ConfirmationToken{}
		return res, err
	}
	if perr := buildTurnEvents(res, terr).publish(turn); perr != nil {
		// An event the hub refused is not skipped over: the turn is failed, the
		// secret is withheld (the user would confirm what the stream did not
		// show) and nothing of the response is spoken.
		v.log.Error("a voice turn event was refused; the turn was failed", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(perr)), "code", string(contracts.CodeOf(perr)))
		if ferr := turn.Fail(perr); ferr != nil {
			v.log.Error("the voice turn could not be failed cleanly", "turn_id", res.TurnID, "reason", string(stream.ReasonOf(ferr)))
		}
		res.Confirmation = actions.ConfirmationToken{}
		return res, perr
	}
	return res, terr
}
