package bridge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Identity is the minimum identity context AlixAI sends with a call (master plan
// 10.1): the principal, roles, tenant and workspace the host must authorise the
// request for. It is not a credential. The host reauthorises every read, prepare
// and commit from it.
type Identity struct {
	ApplicationID string   `json:"application_id"`
	TenantID      string   `json:"tenant_id"`
	WorkspaceID   string   `json:"workspace_id"`
	PrincipalID   string   `json:"principal_id"`
	Roles         []string `json:"roles"`
	SessionID     string   `json:"session_id"`
}

// validate applies the rules of contracts.Scope to the six levels the wire
// carries (the other two, the deployment and the capability scopes, are AlixAI's
// own and never leave it: placeholders stand in for them).
func (i Identity) validate() error {
	return contracts.Scope{
		DeploymentID: "-", CapabilityScopes: []string{"-"},
		ApplicationID: i.ApplicationID, TenantID: i.TenantID, WorkspaceID: i.WorkspaceID,
		PrincipalID: i.PrincipalID, Roles: i.Roles, SessionID: i.SessionID,
	}.Validate()
}

// Call is one request for an operation the host declared.
type Call struct {
	// Method is the name the operation was registered under.
	Method string
	// Identity is the principal and context to authorise the request for.
	Identity Identity
	// Fields are the capability's field values (JSON scalars or flat lists of
	// scalars; numbers arrive as json.Number). Set for read, validate and
	// prepare.
	Fields map[string]any
	// IdempotencyKey is set for commit, status and cancel.
	IdempotencyKey string
	// Prepared is the action to execute, for commit.
	Prepared *actions.PreparedAction
}

// Reply is what a Handler answers.
type Reply struct {
	// Result is the host's members of a DeterministicResult (status,
	// identifiers, values, errors, allowed_next_actions, freshness and, for a
	// prepare, the preparation): any value that encodes as a JSON object, or a
	// json.RawMessage. A refusal is a result with status rejected, never an error.
	Result any
	// IdempotencyKey is the key of the call, echoed so that a reply for another
	// request is recognisable. Set it for commit, status and cancel.
	IdempotencyKey string
}

// Handler serves one declared operation. It must authorise the request itself:
// the bridge authenticates AlixAI, not the principal. ctx ends when the caller
// gives up (the connection closes) and when the Server closes. An error is
// reported to AlixAI as "the host could not complete the call", never with its
// text; a commit that returns one is treated as uncertain.
type Handler func(ctx context.Context, call Call) (Reply, error)

// ManageHandler serves one management (provisioning) operation: the disposable
// tenants of the integration audit. It receives no identity and its result must
// encode as a JSON object.
type ManageHandler func(ctx context.Context, args map[string]any) (any, error)

// Health is what the host reports about itself.
type Health struct {
	// Status is "ok", "degraded" or "unavailable".
	Status string `json:"status"`
	// Reason says why, when Status is not "ok".
	Reason string `json:"reason,omitempty"`
}

// PeerCredentials are the operating system's account of the process on the other
// end of a Unix socket (SO_PEERCRED). They are available on Linux only.
type PeerCredentials struct {
	UID, GID, PID int
}

// Config configures New.
type Config struct {
	// ApplicationID is the application this bridge belongs to. A peer that asks
	// for another one is refused.
	ApplicationID string
	// Keys are the application keys, by key id: at least MinKeyBytes bytes of
	// random data each, shared with AlixAI out of band. Several ids let a key be
	// rotated; remove an id to revoke it.
	Keys map[string][]byte
	// EndpointID, when set, is the endpoint ID of the application package that
	// AlixAI must name; a session for another endpoint is refused.
	EndpointID string
	// ApplicationVersion is reported by describe and health.
	ApplicationVersion string
	// ContractVersions are the adapter contract versions the host speaks; nil
	// means version 1.
	ContractVersions []int
	// MaxRequestBytes bounds a request frame; 0 means DefaultMaxRequestBytes.
	MaxRequestBytes int
	// MaxResponseBytes bounds a reply frame; 0 means DefaultMaxResponseBytes.
	MaxResponseBytes int
	// MaxConns bounds the connections served at once; 0 means 32. One more is
	// closed without an answer.
	MaxConns int
	// HandshakeTimeout bounds the handshake and the wait for the request; 0
	// means 5 seconds.
	HandshakeTimeout time.Duration
	// PeerCheck decides whether the operating system's account of the peer is
	// acceptable. It is called only where the system provides one (a Unix socket
	// on Linux). nil allows the bridge's own user and root. The key is still
	// required either way.
	PeerCheck func(PeerCredentials) error
	// Health reports the host's state; nil reports ok.
	Health func(ctx context.Context) (Health, error)
	// Inspect returns what the host exposes for the audit, as the JSON document
	// {"reports": [...]}; nil exposes nothing (every aspect unsupported).
	Inspect func(ctx context.Context, id Identity, aspects []string) (any, error)
	// Logger receives one line per refused peer or failed call; nil discards. It
	// never receives a key, a proof or a request body.
	Logger *slog.Logger
}

// Server is the host side of the native adapter's protocol. It serves only the
// operations the host declared with Handle, only to a peer that proved it holds
// the application key, and one call per connection.
type Server struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	methods   map[string]Handler
	manage    map[string]ManageHandler
	serving   bool
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	slots     chan struct{}
	wg        sync.WaitGroup
	base      context.Context
	stop      context.CancelFunc
}

var (
	methodRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	keyIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	idRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// New builds a Server. It validates the configuration and opens nothing.
func New(cfg Config) (*Server, error) {
	if cfg.ApplicationID == "" || !idRE.MatchString(cfg.ApplicationID) {
		return nil, errors.New("bridge: ApplicationID is empty or malformed")
	}
	if len(cfg.Keys) == 0 {
		return nil, errors.New("bridge: at least one application key is required")
	}
	keys := make(map[string][]byte, len(cfg.Keys))
	for id, k := range cfg.Keys {
		if !keyIDRE.MatchString(id) {
			return nil, errors.New("bridge: a key id is empty or malformed")
		}
		if len(k) < MinKeyBytes {
			return nil, fmt.Errorf("bridge: the application key %q is shorter than %d bytes", id, MinKeyBytes)
		}
		keys[id] = append([]byte(nil), k...)
	}
	cfg.Keys = keys
	if cfg.EndpointID != "" && !idRE.MatchString(cfg.EndpointID) {
		return nil, errors.New("bridge: EndpointID is malformed")
	}
	if len(cfg.ContractVersions) == 0 {
		cfg.ContractVersions = []int{1}
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 32
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 5 * time.Second
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{
		cfg: cfg, log: log, methods: map[string]Handler{}, manage: map[string]ManageHandler{},
		listeners: map[net.Listener]struct{}{}, conns: map[net.Conn]struct{}{}, slots: make(chan struct{}, cfg.MaxConns),
	}
	s.base, s.stop = context.WithCancel(context.Background())
	return s, nil
}

func reserved(name string) bool {
	return strings.HasPrefix(name, "session.") || strings.HasPrefix(name, "alixai.")
}

// Handle declares an operation: AlixAI can call method, and nothing else of the
// host. The name must be the native_method an application package maps it to. It
// must be called before Serve; a malformed, reserved or repeated name is an error.
func (s *Server) Handle(method string, h Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case h == nil:
		return errors.New("bridge: Handle needs a handler")
	case !methodRE.MatchString(method) || reserved(method):
		return fmt.Errorf("bridge: %q is not a method name the bridge can serve (reserved prefixes: session., alixai.)", method)
	case s.serving:
		return errors.New("bridge: operations are declared before Serve")
	case s.methods[method] != nil:
		return fmt.Errorf("bridge: %q is declared twice", method)
	}
	s.methods[method] = h
	return nil
}

// HandleManagement declares a management operation (create_tenant, seed,
// cleanup of the integration audit's fixtures). It is not a capability
// operation: it is reachable only when the adapter configuration of the package
// approves its name.
func (s *Server) HandleManagement(name string, h ManageHandler) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case h == nil:
		return errors.New("bridge: HandleManagement needs a handler")
	case !methodRE.MatchString(name):
		return fmt.Errorf("bridge: %q is not a management operation name", name)
	case s.serving:
		return errors.New("bridge: operations are declared before Serve")
	case s.manage[name] != nil:
		return fmt.Errorf("bridge: management operation %q is declared twice", name)
	}
	s.manage[name] = h
	return nil
}

// Serve accepts connections on l until Close, and returns nil then. Use
// ListenUnix or ListenLoopback to make l. Every connection is served on its own
// goroutine, bounded by Config.MaxConns.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = l.Close()
		return net.ErrClosed
	}
	s.serving = true
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	var delay time.Duration // the backoff after a temporary Accept error, as net/http does it
	for {
		c, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if errors.As(err, &ne) && ne.Temporary() { //nolint:staticcheck // how net/http tells a retryable Accept error (EMFILE, ENFILE)
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				s.log.Warn("native bridge: accepting a connection failed, retrying", slog.Duration("delay", delay))
				select {
				case <-time.After(delay):
				case <-s.base.Done():
				}
				continue
			}
			return err
		}
		delay = 0
		select {
		case s.slots <- struct{}{}:
		default:
			s.log.Warn("native bridge: too many connections, one was closed")
			_ = c.Close()
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			<-s.slots
			_ = c.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				<-s.slots
				s.wg.Done()
			}()
			s.serveConn(c)
		}()
	}
}

// Close stops accepting, ends the calls in flight (their contexts end) and waits
// for the connections to finish. It is idempotent.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ls := make([]net.Listener, 0, len(s.listeners))
	for l := range s.listeners {
		ls = append(ls, l)
	}
	cs := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.mu.Unlock()
	s.stop()
	for _, l := range ls {
		_ = l.Close()
	}
	for _, c := range cs {
		_ = c.Close()
	}
	s.wg.Wait()
	return nil
}

// ErrDropConnection, returned by a Handler, closes the connection without any
// reply: the host cannot answer this call and does not say why (it is shutting
// down, or a test needs a lost reply). The caller treats a commit as uncertain.
var ErrDropConnection = errors.New("bridge: drop the connection without a reply")

func (s *Server) reply(c *sconn, id json.RawMessage, result any, meta *rpcMeta) error {
	_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	var raw json.RawMessage
	switch v := result.(type) {
	case json.RawMessage:
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		raw = b
	}
	if !json.Valid(raw) {
		return errors.New("the result is not JSON")
	}
	out, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: raw, Meta: meta})
	if err != nil {
		return err
	}
	if c.ks != nil {
		return writeSealed(c, c.ks, labelReply, out, s.cfg.MaxResponseBytes)
	}
	return writeFrame(c, out, s.cfg.MaxResponseBytes)
}

func (s *Server) fail(c *sconn, id json.RawMessage, code int, msg string) {
	_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	if c.ks != nil {
		_ = writeSealed(c, c.ks, labelReply, out, 1<<16)
		return
	}
	_ = writeFrame(c, out, 1<<16)
}

func randomNonce() ([]byte, error) {
	n := make([]byte, nonceLen)
	_, err := rand.Read(n)
	return n, err
}

var b64 = base64.RawURLEncoding

type helloParams struct {
	Version       int    `json:"version"`
	ApplicationID string `json:"application_id"`
	EndpointID    string `json:"endpoint_id"`
	KeyID         string `json:"key_id"`
	Nonce         string `json:"nonce"`
}

type authParams struct {
	Proof string `json:"proof"`
}

func strictParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return errors.New("no params")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// handshake authenticates the peer: it must name this application, a known key
// id and the right endpoint, prove it holds the key over both nonces, and is
// proven to hold it in turn. It returns the key id that was used.
func (s *Server) handshake(c *sconn) (string, bool) {
	deny := func(id json.RawMessage, why string) (string, bool) {
		s.log.Warn("native bridge: a peer was refused", slog.String("reason", why))
		s.fail(c, id, codeAuthFailed, "authentication failed")
		return "", false
	}
	raw, err := readFrame(c, maxHandshakeFrame)
	if err != nil {
		return "", false
	}
	req, err := decodeRequest(raw)
	if err != nil {
		s.fail(c, nil, codeParse, "the frame is not a JSON-RPC request")
		return "", false
	}
	if req.Method != methodHello {
		return deny(req.ID, "no session.hello")
	}
	var hello helloParams
	if err := strictParams(req.Params, &hello); err != nil || hello.Version != protocolVersion {
		return deny(req.ID, "malformed session.hello")
	}
	key, known := s.cfg.Keys[hello.KeyID]
	cn, nerr := b64.DecodeString(hello.Nonce)
	switch {
	case hello.ApplicationID != s.cfg.ApplicationID:
		return deny(req.ID, "another application")
	case s.cfg.EndpointID != "" && hello.EndpointID != s.cfg.EndpointID:
		return deny(req.ID, "another endpoint")
	case !known:
		return deny(req.ID, "unknown key id")
	case nerr != nil || len(cn) != nonceLen:
		return deny(req.ID, "malformed nonce")
	}
	sn, err := randomNonce()
	if err != nil {
		s.fail(c, req.ID, codeInternal, "the host could not complete the call")
		return "", false
	}
	t := transcript(hello.ApplicationID, hello.EndpointID, hello.KeyID, cn, sn)
	err = s.reply(c, req.ID, map[string]any{
		"version": protocolVersion,
		"nonce":   b64.EncodeToString(sn),
		"proof":   b64.EncodeToString(proof(key, labelServer, t...)),
	}, nil)
	if err != nil {
		return "", false
	}

	raw, err = readFrame(c, maxHandshakeFrame)
	if err != nil {
		return "", false
	}
	req, err = decodeRequest(raw)
	if err != nil {
		s.fail(c, nil, codeParse, "the frame is not a JSON-RPC request")
		return "", false
	}
	var auth authParams
	if req.Method != methodAuth || strictParams(req.Params, &auth) != nil {
		return deny(req.ID, "no session.auth")
	}
	got, err := b64.DecodeString(auth.Proof)
	if err != nil || !hmac.Equal(got, proof(key, labelClient, t...)) {
		return deny(req.ID, "bad proof")
	}
	// From here every frame is sealed, the confirmation included: a peer that did
	// not see the proofs cannot make one.
	c.ks = sessionKey(key, t)
	if err := s.reply(c, req.ID, map[string]any{"authenticated": true}, nil); err != nil {
		return "", false
	}
	return hello.KeyID, true
}

func (s *Server) serveConn(nc net.Conn) {
	defer nc.Close()
	c := &sconn{Conn: nc}
	_ = c.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	if creds, ok := peerCredentials(nc); ok {
		check := s.cfg.PeerCheck
		if check == nil {
			check = defaultPeerCheck
		}
		if err := check(creds); err != nil {
			s.log.Warn("native bridge: the operating system's account of a peer was refused", slog.Int("peer_pid", creds.PID))
			s.fail(c, nil, codePeerRefused, "the peer is not allowed")
			return
		}
	}
	keyID, ok := s.handshake(c)
	if !ok {
		return
	}
	// One call per connection, to be sent within the handshake timeout.
	raw, err := readSealed(c, c.ks, labelRequest, s.cfg.MaxRequestBytes)
	if err != nil {
		switch {
		case errors.Is(err, errBadMAC):
			s.log.Warn("native bridge: a request failed its authentication code and was dropped", slog.String("key_id", keyID))
		case errors.Is(err, errFrameTooLarge) || errors.Is(err, errFrameEmpty):
			s.fail(c, nil, codeInvalidRequest, "the frame is empty or larger than the limit")
		}
		return
	}
	_ = c.SetDeadline(time.Time{})
	req, err := decodeRequest(raw)
	if err != nil {
		s.fail(c, nil, codeInvalidRequest, "the frame is not a JSON-RPC request")
		return
	}

	// The call ends when the peer goes away: any byte or EOF from it now is a
	// protocol violation or a hang-up.
	ctx, cancel := context.WithCancel(s.base)
	defer cancel()
	go func() {
		var one [1]byte
		_, _ = c.Read(one[:])
		cancel()
	}()
	s.dispatch(ctx, c, req, keyID)
}

func (s *Server) dispatch(ctx context.Context, c *sconn, req rpcRequest, keyID string) {
	internal := func(err error) {
		if errors.Is(err, ErrDropConnection) {
			return
		}
		s.log.Warn("native bridge: a call failed", slog.String("method", req.Method), slog.Bool("canceled", ctx.Err() != nil))
		s.fail(c, req.ID, codeInternal, "the host could not complete the call")
	}
	switch req.Method {
	case methodDescribe:
		s.mu.Lock()
		methods := make([]string, 0, len(s.methods))
		for m := range s.methods {
			methods = append(methods, m)
		}
		s.mu.Unlock()
		sort.Strings(methods)
		doc := map[string]any{"contract_versions": s.cfg.ContractVersions, "methods": methods}
		if s.cfg.ApplicationVersion != "" {
			doc["application_version"] = s.cfg.ApplicationVersion
		}
		if err := s.reply(c, req.ID, doc, nil); err != nil {
			internal(err)
		}
		return
	case methodHealth:
		h := Health{Status: "ok"}
		if s.cfg.Health != nil {
			var err error
			if h, err = s.cfg.Health(ctx); err != nil {
				internal(err)
				return
			}
		}
		doc := map[string]any{"status": h.Status}
		if h.Reason != "" {
			doc["reason"] = h.Reason
		}
		if s.cfg.ApplicationVersion != "" {
			doc["application_version"] = s.cfg.ApplicationVersion
		}
		if err := s.reply(c, req.ID, doc, nil); err != nil {
			internal(err)
		}
		return
	case methodInspect:
		var p struct {
			Identity *Identity `json:"identity"`
			Aspects  []string  `json:"aspects"`
		}
		if err := strictParams(req.Params, &p); err != nil || p.Identity == nil || s.checkIdentity(*p.Identity) != nil {
			s.fail(c, req.ID, codeInvalidParams, "the params are not valid")
			return
		}
		var doc any = map[string]any{"reports": []any{}}
		if s.cfg.Inspect != nil {
			var err error
			if doc, err = s.cfg.Inspect(ctx, *p.Identity, p.Aspects); err != nil {
				internal(err)
				return
			}
		}
		if err := s.reply(c, req.ID, doc, nil); err != nil {
			internal(err)
		}
		return
	case methodManage:
		var p struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		}
		if err := strictParams(req.Params, &p); err != nil {
			s.fail(c, req.ID, codeInvalidParams, "the params are not valid")
			return
		}
		s.mu.Lock()
		h := s.manage[p.Name]
		s.mu.Unlock()
		if h == nil {
			s.fail(c, req.ID, codeMethodNotFound, "the host declares no such management operation")
			return
		}
		out, err := h(ctx, p.Args)
		if err != nil {
			internal(err)
			return
		}
		if err := s.reply(c, req.ID, out, nil); err != nil {
			internal(err)
		}
		return
	}
	if reserved(req.Method) {
		s.fail(c, req.ID, codeMethodNotFound, "the host declares no such operation")
		return
	}
	s.mu.Lock()
	h := s.methods[req.Method]
	s.mu.Unlock()
	if h == nil {
		s.log.Warn("native bridge: a call to an undeclared method was refused", slog.String("key_id", keyID))
		s.fail(c, req.ID, codeMethodNotFound, "the host declares no such operation")
		return
	}
	var p struct {
		Identity       *Identity               `json:"identity"`
		Fields         map[string]any          `json:"fields"`
		IdempotencyKey string                  `json:"idempotency_key"`
		Prepared       *actions.PreparedAction `json:"prepared"`
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if len(req.Params) == 0 || dec.Decode(&p) != nil || p.Identity == nil || s.checkIdentity(*p.Identity) != nil {
		s.fail(c, req.ID, codeInvalidParams, "the params are not valid")
		return
	}
	rep, err := h(ctx, Call{Method: req.Method, Identity: *p.Identity, Fields: p.Fields, IdempotencyKey: p.IdempotencyKey, Prepared: p.Prepared})
	if err != nil {
		internal(err)
		return
	}
	var meta *rpcMeta
	if rep.IdempotencyKey != "" {
		meta = &rpcMeta{IdempotencyKey: rep.IdempotencyKey}
	}
	if err := s.reply(c, req.ID, rep.Result, meta); err != nil {
		internal(err)
	}
}

// checkIdentity refuses an identity that is not a valid context of this
// application: the host never sees a request that is bound to another
// application or that lacks a level of the context.
func (s *Server) checkIdentity(i Identity) error {
	if i.ApplicationID != s.cfg.ApplicationID {
		return errors.New("another application")
	}
	return i.validate()
}

func defaultPeerCheck(p PeerCredentials) error {
	if p.UID == 0 || p.UID == os.Getuid() {
		return nil
	}
	return errors.New("the peer runs as another user")
}

// sconn is a connection and, once the handshake has succeeded, its session key:
// from then on every frame in either direction is sealed with it.
type sconn struct {
	net.Conn
	ks []byte
}
