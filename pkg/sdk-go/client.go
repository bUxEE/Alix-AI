// Package sdkgo is the Go client of the AlixAI public API (master plan 15.1). It mirrors the TypeScript SDK
// (sdk/typescript): the same operations, the same checks before anything is sent, the same typed errors, so one
// description serves both. Documents that have a JSON Schema are the generated types of pkg/contracts/gen.
//
// The SDK has no dependencies beyond the standard library and one small WebSocket library. Credentials are
// held in memory only, for the length of one call: the host supplies a fresh single-use context token for
// every request and every event-stream open through ClientOptions.GetContextToken, and the SDK never mints,
// decodes, caches or retries with one. Test host code against the fake server in package sdktest.
package sdkgo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Defaults of ClientOptions.
const (
	DefaultTimeout       = 30 * time.Second
	DefaultTurnTimeout   = 10 * time.Minute
	DefaultAnswerTimeout = 2 * time.Minute
	DefaultMaxResumes    = 3
	DefaultResumeBackoff = 250 * time.Millisecond
)

// ClientOptions configures a Client. For every duration, zero means the default and a negative value
// disables the deadline.
type ClientOptions struct {
	// BaseURL is the AlixAI service, e.g. "https://alixai.example.com" or "http://127.0.0.1:8080". A path
	// prefix is kept. Plain http is accepted only for a loopback host unless AllowInsecureHTTP is set.
	BaseURL string
	// GetContextToken supplies a fresh single-use context token for each request and each event-stream open.
	GetContextToken TokenProvider
	// HTTPClient carries the requests. Its Transport is used as it is; redirects are never followed.
	// Defaults to a client with the default transport.
	HTTPClient *http.Client
	// Timeout is the deadline of an ordinary request. Default 30 seconds.
	Timeout time.Duration
	// TurnTimeout is the deadline of a turn (the whole event stream) or a confirmation. These wait on the model
	// and on the host application's commit, so the default is long: 10 minutes, the service's own limit.
	TurnTimeout time.Duration
	// AnswerTimeout is the deadline of a RAG answer, which waits on the model. Default 2 minutes.
	AnswerTimeout time.Duration
	// AllowInsecureHTTP allows plain http to a host that is not loopback. The context token is a bearer
	// credential: leave this off.
	AllowInsecureHTTP bool
	// MaxResumes bounds how many times an event stream that dropped is resumed. Default 3; negative disables
	// the automatic resume (a dropped stream is then an uncertain_outcome).
	MaxResumes int
	// ResumeBackoff is the pause before the first resume; it grows by that much for each further attempt, up to
	// 5 seconds (or ResumeBackoff, when that is longer). Default 250 milliseconds.
	ResumeBackoff time.Duration
}

// Client is a client of one AlixAI service. It is safe for concurrent use.
type Client struct {
	t             *transport
	timeout       time.Duration
	turnTimeout   time.Duration
	answerTimeout time.Duration
	maxResumes    int
	backoff       time.Duration
}

func orDefault(d, fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return d
}

// NewClient creates a client. It checks the options and sends nothing.
func NewClient(options ClientOptions) (*Client, error) {
	base, err := normalizeBaseURL(options.BaseURL, options.AllowInsecureHTTP)
	if err != nil {
		return nil, err
	}
	if options.GetContextToken == nil {
		return nil, invalidArgument("GetContextToken", "it is required: the host supplies a fresh single-use token per request")
	}
	hc := http.Client{}
	if options.HTTPClient != nil {
		hc = *options.HTTPClient
	}
	// A redirect would carry the bearer credential to another place: it is an error, never followed.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	maxResumes := options.MaxResumes
	if maxResumes == 0 {
		maxResumes = DefaultMaxResumes
	}
	return &Client{
		t:             &transport{base: base, hc: &hc, token: options.GetContextToken},
		timeout:       orDefault(options.Timeout, DefaultTimeout),
		turnTimeout:   orDefault(options.TurnTimeout, DefaultTurnTimeout),
		answerTimeout: orDefault(options.AnswerTimeout, DefaultAnswerTimeout),
		maxResumes:    maxResumes,
		backoff:       orDefault(options.ResumeBackoff, DefaultResumeBackoff),
	}, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// get is a read: no body, a JSON answer.
func get[T any](ctx context.Context, c *Client, op Operation, params map[string]string, o callOptions, check func(*T) error) (*T, error) {
	return doJSON(ctx, c.t, request{op: op, params: params, accept: "application/json"}, o.deadline(c.timeout), check)
}

func jsonBody(v any) []byte {
	b, _ := json.Marshal(v) // the values are plain structs and maps of strings: they always marshal
	return b
}

// ---- sessions ----

// CreateSession opens the session the context token names (creating it on first sight). Opening an existing
// one answers the same way.
func (c *Client) CreateSession(ctx context.Context, options ...CallOption) (*Session, error) {
	return doJSON(ctx, c.t, request{op: opCreateSession, accept: "application/json"},
		applyOptions(options).deadline(c.timeout), (*Session).validate)
}

// GetSession resumes a session, with its structured pending state.
func (c *Client) GetSession(ctx context.Context, sessionID string, options ...CallOption) (*Session, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	return get(ctx, c, opGetSession, map[string]string{"session_id": sessionID}, applyOptions(options), (*Session).validate)
}

// EndSession ends a session. A prepared action it waits on is invalidated; nothing is ever committed by
// ending. Ending twice is the same as ending once.
func (c *Client) EndSession(ctx context.Context, sessionID string, options ...CallOption) (*SessionEnded, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	return get(ctx, c, opEndSession, map[string]string{"session_id": sessionID}, applyOptions(options), (*SessionEnded).validate)
}

// GetPendingState is what the active goal waits for, or nil when there is none. The confirmation secret is
// not part of it.
func (c *Client) GetPendingState(ctx context.Context, sessionID string, options ...CallOption) (*PendingState, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	r, err := get(ctx, c, opGetPendingState, map[string]string{"session_id": sessionID}, applyOptions(options), (*pendingResponse).validate)
	if err != nil {
		return nil, err
	}
	return r.Pending, nil
}

// ---- turns ----

// turnBody validates a turn input and returns the request body, built from known members only.
func (c *Client) turnBody(input TurnInput) ([]byte, error) {
	if err := input.validate(); err != nil {
		return nil, err
	}
	return jsonBody(input), nil
}

// SendTurnJSON sends a turn and returns it as one JSON document (no streaming). The same checks as SendTurn.
func (c *Client) SendTurnJSON(ctx context.Context, sessionID string, input TurnInput, options ...CallOption) (*TurnResult, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	body, err := c.turnBody(input)
	if err != nil {
		return nil, err
	}
	o := applyOptions(options)
	r := request{
		op: opSendTurn, params: map[string]string{"session_id": sessionID}, accept: "application/json",
		contentType: "application/json", body: body,
	}
	return doJSON(ctx, c.t, r, o.deadline(c.turnTimeout), func(t *TurnResult) error { return checkTurnResult(t, opSendTurn) })
}

// decodeTurnResult decodes and checks the JSON form of a turn.
func decodeTurnResult(body []byte, op Operation) (*TurnResult, error) {
	var t TurnResult
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, unexpectedShape(op, err)
	}
	if err := checkTurnResult(&t, op); err != nil {
		return nil, err
	}
	return &t, nil
}

// checkTurnResult enforces the sequence of a JSON turn and the grant that comes with it.
func checkTurnResult(t *TurnResult, op Operation) error {
	if t.TurnID == "" {
		return malformedTurn("a JSON turn needs a turn_id and a non-empty events array")
	}
	seq := turnSequence{turnID: t.TurnID}
	preparationID := ""
	for _, ev := range t.Events {
		if err := seq.accept(ev, nil); err != nil {
			return err
		}
	}
	if err := seq.finish(); err != nil {
		return err
	}
	for _, ev := range t.Events {
		if id := preparedID(ev); id != "" {
			preparationID = id
		}
	}
	if g := t.Confirmation; g != nil {
		if g.PreparationID == "" || !confirmationPattern.MatchString(g.Token) {
			return malformedTurn("the confirmation member is malformed")
		}
		if preparationID != "" && preparationID != g.PreparationID {
			return malformedTurn("the confirmation names another preparation than the prepared_action event")
		}
	}
	return nil
}

// ---- actions ----

// GetAction is the prepared action with its current state.
func (c *Client) GetAction(ctx context.Context, preparationID string, options ...CallOption) (*actions.PreparedAction, error) {
	if err := pathID("preparationID", preparationID); err != nil {
		return nil, err
	}
	return get(ctx, c, opGetAction, map[string]string{"preparation_id": preparationID}, applyOptions(options), checkPrepared)
}

// ConfirmAction presents the single-use confirmation secret and commits the action. A commit the host refused
// (409) or whose outcome is partial or unknown (500) is an *APIError that carries the Receipt and Preparation;
// a reused secret is `replayed`.
func (c *Client) ConfirmAction(ctx context.Context, preparationID, token string, options ...CallOption) (*ActionOutcome, error) {
	if err := pathID("preparationID", preparationID); err != nil {
		return nil, err
	}
	if err := checkConfirmationToken(token); err != nil {
		return nil, err
	}
	r := request{
		op: opConfirmAction, params: map[string]string{"preparation_id": preparationID}, accept: "application/json",
		contentType: "application/json", body: jsonBody(map[string]string{"token": token}),
	}
	return doJSON(ctx, c.t, r, applyOptions(options).deadline(c.turnTimeout), (*ActionOutcome).validate)
}

// RejectAction declines a preparation. Nothing is sent to the application.
func (c *Client) RejectAction(ctx context.Context, preparationID string, options ...CallOption) (*actions.PreparedAction, error) {
	if err := pathID("preparationID", preparationID); err != nil {
		return nil, err
	}
	return get(ctx, c, opRejectAction, map[string]string{"preparation_id": preparationID}, applyOptions(options), checkPrepared)
}

// GetActionReceipt is the durable receipt of an action. It is `conflict` while the action has no outcome yet.
func (c *Client) GetActionReceipt(ctx context.Context, preparationID string, options ...CallOption) (*actions.ActionReceipt, error) {
	if err := pathID("preparationID", preparationID); err != nil {
		return nil, err
	}
	return get(ctx, c, opGetActionReceipt, map[string]string{"preparation_id": preparationID}, applyOptions(options), checkReceipt)
}

// ---- applications ----

// GetApplication is the caller's application with its installed packages and states.
func (c *Client) GetApplication(ctx context.Context, applicationID string, options ...CallOption) (*Application, error) {
	if err := pathID("applicationID", applicationID); err != nil {
		return nil, err
	}
	return get(ctx, c, opGetApplication, map[string]string{"application_id": applicationID}, applyOptions(options), (*Application).validate)
}

// ListCapabilities lists the capabilities the caller may see, with the effective risk level of each.
func (c *Client) ListCapabilities(ctx context.Context, options ...CallOption) (*CapabilityList, error) {
	return get(ctx, c, opListCapabilities, nil, applyOptions(options), (*CapabilityList).validate)
}

// RegisterApplication installs a signed .tar.gz application package into quarantine. It needs the
// `applications.register` scope. The archive is checked for the gzip magic before anything is sent.
func (c *Client) RegisterApplication(ctx context.Context, archive []byte, options ...CallOption) (*Registration, error) {
	if !bytes.HasPrefix(archive, []byte{0x1f, 0x8b}) {
		return nil, invalidArgument("archive", "an application package is a gzip-compressed tar archive")
	}
	r := request{op: opRegisterApplication, accept: "application/json", contentType: "application/gzip", body: archive}
	return doJSON(ctx, c.t, r, applyOptions(options).deadline(c.timeout), (*Registration).validate)
}

// ---- operations ----

// notReadyIsAReport lets the 503 of the probes through: a service that is not ready answers with its report,
// which is information, not a failure.
func notReadyIsAReport(status int) bool { return status == http.StatusServiceUnavailable }

// GetHealth is the readiness report plus the caller's application. A service that is not ready returns its
// report; it does not fail.
func (c *Client) GetHealth(ctx context.Context, options ...CallOption) (*Health, error) {
	o := applyOptions(options)
	return doJSON(ctx, c.t, request{op: opGetHealth, accept: "application/json", allowStatus: notReadyIsAReport},
		o.deadline(c.timeout), (*Health).validate)
}

// GetLiveness is liveness: unauthenticated, no context token is requested.
func (c *Client) GetLiveness(ctx context.Context, options ...CallOption) (*Liveness, error) {
	return get(ctx, c, opGetLiveness, nil, applyOptions(options), (*Liveness).validate)
}

// GetReadiness is readiness: unauthenticated. A service that is not ready returns its report; it does not fail.
func (c *Client) GetReadiness(ctx context.Context, options ...CallOption) (*ReadyReport, error) {
	o := applyOptions(options)
	return doJSON(ctx, c.t, request{op: opGetReadiness, accept: "application/json", allowStatus: notReadyIsAReport},
		o.deadline(c.timeout), (*ReadyReport).validate)
}
