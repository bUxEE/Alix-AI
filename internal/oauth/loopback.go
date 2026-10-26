package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// LoopbackServer is an ephemeral 127.0.0.1 listener for one OAuth callback. It accepts the real
// callback GET on the configured path, completes the exchange server-side, serves the callback page,
// then closes. Spurious GETs (wrong or missing state, missing code) get a 400 neutral page and do
// not abort the pending flow. It never binds 0.0.0.0 or a non-loopback address.
type LoopbackServer struct {
	Engine *Engine
	Scope  contracts.Scope
	FlowID string
	Path   string // e.g. /auth/callback or /oauth/callback
	Opener string // empty → CLI close message (no postMessage)
	// CLIClose forces the "AlixAI is connected..." copy.
	CLIClose bool

	ln       net.Listener
	once     sync.Once
	done     chan struct{}
	redirect string
}

// errCallbackIgnored means the request was not a usable callback for this flow: keep listening.
var errCallbackIgnored = errors.New("oauth: callback ignored")

func isCallbackIgnored(err error) bool { return errors.Is(err, errCallbackIgnored) }

// BindLoopback listens on 127.0.0.1:<random> and returns the server and canonical redirect URI.
// Call Begin with that URI (not the placeholder), AttachLoopback, then Serve in a goroutine.
func (e *Engine) BindLoopback(path string) (*LoopbackServer, string, error) {
	if path == "" {
		path = defaultLoopbackPath
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fail(contracts.ErrUnavailable, ReasonProviderDown, "oauth: could not bind a loopback listener")
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || addr.IP == nil || !addr.IP.IsLoopback() || addr.IP.To4() == nil {
		_ = ln.Close()
		return nil, "", fail(contracts.ErrInternal, ReasonBadConfig, "oauth: loopback listener is not 127.0.0.1")
	}
	redirect := LoopbackRedirectURI(addr.Port, path)
	s := &LoopbackServer{Engine: e, Path: path, ln: ln, done: make(chan struct{}), redirect: redirect}
	return s, redirect, nil
}

// RedirectURI is the bound http://127.0.0.1:<port><path>.
func (s *LoopbackServer) RedirectURI() string { return s.redirect }

// Close stops the listener.
func (s *LoopbackServer) Close() error {
	s.once.Do(func() {
		_ = s.ln.Close()
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	})
	return nil
}

// Wait blocks until the one callback is handled or the context ends.
func (s *LoopbackServer) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

// Serve runs until Close after a real callback (or listener failure).
func (s *LoopbackServer) Serve() {
	defer s.Close()
	if tl, ok := s.ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(DefaultStateTTL + time.Minute))
	}
	mux := http.NewServeMux()
	mux.HandleFunc(s.Path, s.handle)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	_ = srv.Serve(s.ln)
}

func (s *LoopbackServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != s.Path {
		http.NotFound(w, r)
		return
	}
	cb := CallbackFromQuery(s.redirect, r.URL.Query())
	pageStatus := "failed"
	var err error
	if s.FlowID != "" {
		_, err = s.Engine.CompleteLoopback(r.Context(), s.Scope, s.FlowID, cb)
	} else {
		_, err = s.Engine.Complete(r.Context(), s.Scope, cb)
	}
	if isCallbackIgnored(err) {
		s.writePage(w, http.StatusBadRequest, "failed")
		return // keep listening
	}
	if err == nil {
		pageStatus = "completed"
	}
	s.writePage(w, http.StatusOK, pageStatus)
	go func() { _ = s.Close() }()
}

func (s *LoopbackServer) writePage(w http.ResponseWriter, code int, status string) {
	nonce := newCSPNonce()
	body := RenderCallbackPage(CallbackPageInput{
		FlowID: s.FlowID, Status: status, OpenerOrigin: s.Opener, Nonce: nonce,
		CLIClose: s.CLIClose || s.Opener == "",
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", CallbackCSP(nonce))
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func newCSPNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AttachLoopback records the listener under the flow id so cancel/expiry can close it.
func (e *Engine) AttachLoopback(flowID string, c interface{ Close() error }) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.loopbacks == nil {
		e.loopbacks = map[string]io.Closer{}
	}
	e.loopbacks[flowID] = loopCloser{c}
}

type loopCloser struct{ c interface{ Close() error } }

func (l loopCloser) Close() error { return l.c.Close() }

// CompleteLoopback completes a callback bound to a flow id (loopback path, including NoState providers).
// A GET that is not this flow's real callback (wrong/missing state, or NoState without a code) returns
// errCallbackIgnored without marking the flow failed or consuming the pending authorization.
func (e *Engine) CompleteLoopback(ctx context.Context, scope contracts.Scope, flowID string, cb CallbackRequest) (Connection, error) {
	e.mu.Lock()
	frec, ok := e.flows[flowID]
	e.mu.Unlock()
	if !ok || !sameOwner(frec.scope, scope) {
		return Connection{}, flowNotFound()
	}
	p := e.prov[frec.Provider]
	var (
		pf  *pendingFlow
		err error
	)
	if p.NoState {
		// OpenRouter has no state: require a non-empty code before consuming the pending flow.
		if cb.Code == "" {
			return Connection{}, errCallbackIgnored
		}
		pf, err = e.takeByFlow(flowID)
	} else {
		if cb.State == "" {
			return Connection{}, errCallbackIgnored
		}
		if !e.stateBelongsToFlow(cb.State, flowID) {
			return Connection{}, errCallbackIgnored
		}
		pf, err = e.take(cb.State)
	}
	if err != nil {
		e.markFlowByIDFailed(flowID, ReasonOf(err))
		return Connection{}, err
	}
	conn, err := e.completeTaken(ctx, scope, pf, cb)
	if err != nil {
		e.markFlowByIDFailed(flowID, ReasonOf(err))
		return Connection{}, err
	}
	e.markFlowByIDCompleted(flowID, conn)
	return conn, nil
}

// stateBelongsToFlow reports whether state is the pending authorization of flowID (without consuming it).
func (e *Engine) stateBelongsToFlow(state, flowID string) bool {
	key := stateKey(state)
	e.mu.Lock()
	defer e.mu.Unlock()
	f, ok := e.pending[key]
	return ok && f.flowID == flowID
}

func (e *Engine) takeByFlow(flowID string) (*pendingFlow, error) {
	now := e.clk.Now()
	key := "lb:" + flowID
	e.mu.Lock()
	defer e.mu.Unlock()
	e.purgeLocked(now)
	f, ok := e.pending[key]
	if !ok {
		if u, used := e.consumed[key]; used {
			if u.expired {
				return nil, fail(contracts.ErrExpired, ReasonStateExpired, "oauth: the authorization expired; start again")
			}
			return nil, fail(contracts.ErrReplayed, ReasonReplayed, "oauth: this authorization was already used")
		}
		return nil, fail(contracts.ErrValidationFailed, ReasonStateUnknown, "oauth: the state is unknown")
	}
	delete(e.pending, key)
	e.consumed[key] = usedState{until: now.Add(e.ttl)}
	return f, nil
}
