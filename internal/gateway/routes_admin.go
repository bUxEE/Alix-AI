package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/fsperm"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The operator admin interface (M1-24): the one way `alixai audit run` and
// `alixai audit quarantine` make a RUNNING server drop or reload what it holds
// for an application at once, instead of leaving it to the capability
// registry's 30 second cache.
//
//	POST /admin/registry/refresh      {"application_id": "demo-library"}
//	POST /admin/registry/deactivate   {"application_id": "demo-library"}
//
// Both only bring the server's caches back in line with the store, which stays
// the authority: refresh reloads the application's active package (and drops it
// when there is none), deactivate evicts it (the next lookup reloads whatever
// the store says is active). Neither can activate a package, widen a capability
// or change a scope, so a mistaken or repeated call costs a reload and nothing
// else.
//
// # Why this is not a route area
//
// It is deliberately NOT registered with RegisterArea and is never mounted on
// the server NewServer builds. The public mux is the public API: the route
// inventory, the OpenAPI comparison and the forbidden-surface deny-list
// (SEC-092) all describe exactly that surface, and an operator route has no
// place on it whatever guards it. The admin interface is its own http.Handler
// on its own listener:
//
//   - the listener is the literal loopback address 127.0.0.1 on an ephemeral
//     port, bound by AdminListen, and AdminServer.Serve refuses any other
//     listener, with no TLS exception (SEC-093): a certificate does not make an
//     operator interface safe to publish, so nothing here ever binds beyond the
//     machine;
//   - every request, for every path and method, must come from a loopback peer
//     on a loopback local address with a loopback Host, and must carry the
//     operator credential in AdminTokenHeader, before the router sees it.
//     A /v1 context token is not that credential: it is compared to the secret
//     like any other wrong value and refused;
//   - the credential is a random 256-bit secret made when the process starts. It
//     lives in memory and in the owner-only endpoint file (AdminEndpointFile) in
//     the data directory, so "operator" means "can read the data directory's
//     secrets", the same boundary as the store and the signing keys (SEC-081).
//     Restarting the process replaces it. It is never logged and never in a URL.
//
// The endpoint file also tells the CLI where the interface listens, because the
// port is chosen at start. A file left by a process that died points at nothing:
// the client reports that as "no running server" (ErrAdminUnreachable).
const (
	// AdminTokenHeader carries the operator credential.
	AdminTokenHeader = "X-Alixai-Admin-Token"
	// AdminEndpointFile is the file below the data directory that holds the
	// address and the credential of the running server's admin interface.
	AdminEndpointFile = "admin-endpoint.json"
	// AdminEndpointFormat is its format marker.
	AdminEndpointFormat = "alixai.admin-endpoint/v1"

	// AdminPathRefresh and AdminPathDeactivate are the two routes.
	AdminPathRefresh    = "/admin/registry/refresh"
	AdminPathDeactivate = "/admin/registry/deactivate"

	adminMinTokenLen = 32
	adminMaxTokenLen = 256
	adminMaxBody     = 4 << 10
)

// ErrAdminUnreachable marks a failure to connect to the admin interface: no
// process is listening at the recorded address, which is what a server that is
// not running (or died and left its endpoint file) looks like.
var ErrAdminUnreachable = errors.New("no server is listening at the recorded admin address")

// ErrAdminNeedsLoopback marks the refusal to serve the admin interface on
// anything but a loopback TCP listener.
var ErrAdminNeedsLoopback = errors.New("the admin interface only ever listens on a loopback TCP address")

// AdminControl is what the admin routes act on. The composition root builds it
// from the capability registry, the package source and the adapter cache, so one
// call brings every cache of the process in line with the store.
type AdminControl interface {
	// Refresh reloads the application's active package, or drops the
	// application when the store has none. An error means the process could
	// not load what the store calls active; it serves nothing for the
	// application until it can.
	Refresh(ctx context.Context, applicationID string) error
	// Deactivate evicts the application from the process's caches at once.
	Deactivate(applicationID string)
}

// NewAdminToken returns a random operator credential: 256 bits, hex encoded.
func NewAdminToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gateway: cannot make an admin credential: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// AdminOptions is everything NewAdminServer needs.
type AdminOptions struct {
	// Control is what the routes act on. Required.
	Control AdminControl
	// Token is the operator credential, at least 32 characters (NewAdminToken
	// makes one). Required: an admin interface with no secret does not exist.
	Token string
	// Logger receives the access log. Nil discards. It should come from
	// internal/log so values are redacted.
	Logger *slog.Logger
	// Clock times requests. Nil is the wall clock.
	Clock clock.Clock
}

// AdminServer is the operator admin interface: a handler and its listener
// lifecycle. It is separate from Server on purpose (see the comment above).
type AdminServer struct {
	handler http.Handler
	log     *slog.Logger
	tokenH  [sha256.Size]byte
	control AdminControl
}

// NewAdminServer builds the admin interface. It binds nothing.
func NewAdminServer(opts AdminOptions) (*AdminServer, error) {
	switch {
	case isNilValue(opts.Control):
		return nil, contracts.New(contracts.ErrValidationFailed, "gateway: the admin interface needs something to act on")
	case len(opts.Token) < adminMinTokenLen || len(opts.Token) > adminMaxTokenLen:
		return nil, contracts.Newf(contracts.ErrValidationFailed, "gateway: the admin credential must be %d to %d characters", adminMinTokenLen, adminMaxTokenLen)
	}
	log, clk := opts.Logger, opts.Clock
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if clk == nil {
		clk = clock.Real{}
	}
	a := &AdminServer{log: log, tokenH: sha256.Sum256([]byte(opts.Token)), control: opts.Control}

	rt := &Router{mux: http.NewServeMux()}
	rt.area = "admin"
	rt.HandleFunc("POST "+AdminPathRefresh, a.refresh)
	rt.HandleFunc("POST "+AdminPathDeactivate, a.deactivate)
	rt.mux.HandleFunc("/", rt.notFoundOrMethodNotAllowed)

	// The same envelope, request IDs, body limit, panic recovery and access log
	// as the public server, with a body limit sized for one application ID.
	cfg := DefaultConfig()
	cfg.MaxBodyBytes = adminMaxBody
	base := &Server{cfg: cfg, log: log, clk: clk, ids: ids.NewGenerator(clk)}
	a.handler = base.wrap(a.guard(rt.mux))
	return a, nil
}

// Handler returns the full handler, guard included. Serve uses it; a test
// drives it directly.
func (a *AdminServer) Handler() http.Handler { return a.handler }

// AdminListen binds the admin interface's listener: the loopback address on an
// ephemeral port. The bound address is checked again, so a platform that maps
// "127.0.0.1" elsewhere cannot widen it.
func AdminListen() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "gateway: cannot bind the admin interface")
	}
	if !adminLoopbackListener(ln) {
		_ = ln.Close()
		return nil, contracts.Wrap(contracts.ErrValidationFailed, ErrAdminNeedsLoopback, "gateway: refusing the admin listener "+ln.Addr().String())
	}
	return ln, nil
}

func adminLoopbackListener(ln net.Listener) bool {
	a, ok := ln.Addr().(*net.TCPAddr)
	return ok && a.IP != nil && a.IP.IsLoopback()
}

// Serve serves the admin interface on ln until ctx is done, then drains
// in-flight requests. A listener that is not loopback TCP is refused and closed
// before anything is served, whoever bound it.
func (a *AdminServer) Serve(ctx context.Context, ln net.Listener) error {
	if !adminLoopbackListener(ln) {
		_ = ln.Close()
		return contracts.Wrap(contracts.ErrValidationFailed, ErrAdminNeedsLoopback, "gateway: refusing the admin listener "+ln.Addr().String())
	}
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    DefaultMaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// adminLoopbackIP reports whether host (no port) is a literal loopback IP.
func adminLoopbackIP(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// adminLoopbackRequest is true only for a request whose peer and local address
// are both loopback and whose Host names the machine itself. The server never
// binds anywhere else; this is the second lock on the same door. A request
// whose addresses cannot be read is not loopback.
func adminLoopbackRequest(r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !adminLoopbackIP(peer) {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	tcp, ok := local.(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || adminLoopbackIP(host)
}

// guard runs before the router: loopback first, then the operator credential.
// Both refusals are the same for every path, so an unauthorised caller learns
// nothing about which routes exist.
func (a *AdminServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !adminLoopbackRequest(r) {
			WriteError(w, r, contracts.New(contracts.ErrPermissionDenied, "the admin interface answers loopback callers only"))
			return
		}
		got := sha256.Sum256([]byte(r.Header.Get(AdminTokenHeader)))
		if subtle.ConstantTimeCompare(got[:], a.tokenH[:]) != 1 {
			WriteError(w, r, contracts.New(contracts.ErrUnauthenticated, "the operator credential is missing or wrong"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type adminRequest struct {
	ApplicationID string `json:"application_id"`
}

// AdminResult is the body of a successful admin call.
type AdminResult struct {
	ApplicationID string `json:"application_id"`
	// Action is "refresh" or "deactivate".
	Action string `json:"action"`
	// Status is "ok".
	Status string `json:"status"`
}

func (a *AdminServer) decode(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req adminRequest
	if !decodeJSON(w, r, &req) {
		return "", false
	}
	if !validPathID(req.ApplicationID) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "application_id is required: 1 to 128 letters, digits and ._-"))
		return "", false
	}
	return req.ApplicationID, true
}

func (a *AdminServer) refresh(w http.ResponseWriter, r *http.Request) {
	app, ok := a.decode(w, r)
	if !ok {
		return
	}
	if err := a.control.Refresh(r.Context(), app); err != nil {
		// Whatever the cause, the registry has already dropped the application:
		// it serves nothing until it can load what the store calls active.
		code := contracts.CodeOf(err)
		if code == contracts.ErrInternal {
			code = contracts.ErrUnavailable
		}
		WriteError(w, r, contracts.Wrap(code, err, "the server could not load the application's active package; it serves nothing for it until it can").
			WithDetails("application_id", app))
		return
	}
	loggerFrom(r.Context()).Info("admin: registry refreshed", slog.String("application_id", app))
	writeJSON(w, http.StatusOK, AdminResult{ApplicationID: app, Action: "refresh", Status: "ok"})
}

func (a *AdminServer) deactivate(w http.ResponseWriter, r *http.Request) {
	app, ok := a.decode(w, r)
	if !ok {
		return
	}
	a.control.Deactivate(app)
	loggerFrom(r.Context()).Info("admin: application evicted from the registry", slog.String("application_id", app))
	writeJSON(w, http.StatusOK, AdminResult{ApplicationID: app, Action: "deactivate", Status: "ok"})
}

// AdminForget drops the package Applications cached for an application, so the
// next call re-opens (and re-verifies) it. The admin routes call it through the
// composition root's AdminControl; the cache otherwise ages out after
// DefaultPackageMaxAge.
func (a *Applications) AdminForget(applicationID string) { a.forget(applicationID) }

// ---- the endpoint file ----

// AdminEndpoint is what a running server records for the CLI.
type AdminEndpoint struct {
	Format string `json:"format"`
	// URL is the base URL of the admin interface: http://127.0.0.1:<port>.
	URL string `json:"url"`
	// Token is the operator credential. It is a secret: the file is owner-only.
	Token     string    `json:"token"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

func (e AdminEndpoint) validate() error {
	if len(e.Token) < adminMinTokenLen || len(e.Token) > adminMaxTokenLen {
		return errors.New("the admin endpoint file holds no usable credential")
	}
	u, err := url.Parse(e.URL)
	if err != nil || u.Scheme != "http" || u.Port() == "" || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the admin endpoint file holds no usable address")
	}
	// The credential is never sent anywhere but this machine, whatever the file says.
	if !adminLoopbackIP(u.Hostname()) {
		return errors.New("the admin endpoint file names a non-loopback address; refusing to send the operator credential there")
	}
	return nil
}

// WriteAdminEndpoint records ep in dataDir, replacing any earlier one: written
// to a private temporary file (created exclusively and made owner only before
// the credential is written: mode 0600, on Windows a protected ACL for the
// current user; internal/fsperm) and renamed into place, so a reader never sees
// half of it and the credential is never readable by another user, not even for
// a moment (SEC-081).
func WriteAdminEndpoint(dataDir string, ep AdminEndpoint) error {
	ep.Format = AdminEndpointFormat
	if err := ep.validate(); err != nil {
		return err
	}
	if _, err := fsperm.MkdirOwnerOnly(dataDir); err != nil {
		return fmt.Errorf("gateway: cannot create the data directory: %w", err)
	}
	b, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, AdminEndpointFile)
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	f, err := fsperm.CreateFile(tmp)
	if err != nil {
		return fmt.Errorf("gateway: cannot write the admin endpoint file: %w", err)
	}
	_, werr := f.Write(append(b, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gateway: cannot write the admin endpoint file: %w", werr)
	}
	return nil
}

// ReadAdminEndpoint reads the endpoint a running server recorded in dataDir. An
// error wrapping os.ErrNotExist means no server has recorded one (or it stopped
// cleanly and removed it). A file that is not owner only (other users may read
// it: POSIX mode bits, or on Windows an ACL that inherits or grants another
// user) is refused: the credential in it is no longer secret.
func ReadAdminEndpoint(dataDir string) (AdminEndpoint, error) {
	path := filepath.Join(dataDir, AdminEndpointFile)
	st, err := os.Stat(path)
	if err != nil {
		return AdminEndpoint{}, err
	}
	if !st.Mode().IsRegular() {
		return AdminEndpoint{}, fmt.Errorf("%s is not a regular file", path)
	}
	if err := fsperm.Check(path, false); errors.Is(err, fsperm.ErrNotOwnerOnly) {
		return AdminEndpoint{}, fmt.Errorf("%w; it holds the operator credential, so it is not used: restrict it to the owner (chmod 600, or icacls /inheritance:r on Windows), or restart the server", err)
	} else if err != nil {
		return AdminEndpoint{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return AdminEndpoint{}, err
	}
	var ep AdminEndpoint
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ep); err != nil {
		return AdminEndpoint{}, fmt.Errorf("%s is not a valid admin endpoint file: %w", path, err)
	}
	if ep.Format != AdminEndpointFormat {
		return AdminEndpoint{}, fmt.Errorf("%s has format %q, want %q", path, ep.Format, AdminEndpointFormat)
	}
	if err := ep.validate(); err != nil {
		return AdminEndpoint{}, fmt.Errorf("%s: %w", path, err)
	}
	return ep, nil
}

// RemoveAdminEndpoint removes the endpoint file when it still holds token: a
// process that stops removes only its own record, never that of the process that
// replaced it.
func RemoveAdminEndpoint(dataDir, token string) {
	ep, err := ReadAdminEndpoint(dataDir)
	if err != nil || subtle.ConstantTimeCompare([]byte(ep.Token), []byte(token)) != 1 {
		return
	}
	_ = os.Remove(filepath.Join(dataDir, AdminEndpointFile))
}

// ---- the client ----

// AdminClient calls the admin interface of a running server.
type AdminClient struct {
	ep AdminEndpoint
	hc *http.Client
}

// NewAdminClient returns a client for ep. The address must be loopback; no
// proxy is ever used, so the credential cannot be sent to one.
func NewAdminClient(ep AdminEndpoint) (*AdminClient, error) {
	if err := ep.validate(); err != nil {
		return nil, err
	}
	return &AdminClient{ep: ep, hc: &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Refresh asks the server to reload the application's active package.
func (c *AdminClient) Refresh(ctx context.Context, applicationID string) error {
	return c.call(ctx, AdminPathRefresh, applicationID)
}

// Deactivate asks the server to evict the application at once.
func (c *AdminClient) Deactivate(ctx context.Context, applicationID string) error {
	return c.call(ctx, AdminPathDeactivate, applicationID)
}

func (c *AdminClient) call(ctx context.Context, path, applicationID string) error {
	body, err := json.Marshal(adminRequest{ApplicationID: applicationID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.ep.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(AdminTokenHeader, c.ep.Token)
	resp, err := c.hc.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("%w (%s)", ErrAdminUnreachable, c.ep.URL)
		}
		// The request may have been sent: not "no server", and not a success.
		return contracts.Wrap(contracts.ErrUnavailable, err, "the running server did not answer the admin call")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, adminMaxBody))
	if resp.StatusCode == http.StatusOK {
		var res AdminResult
		if err := json.Unmarshal(raw, &res); err != nil || res.Status != "ok" || res.ApplicationID != applicationID {
			return contracts.New(contracts.ErrUnavailable, "the running server's admin answer is not the one expected")
		}
		return nil
	}
	var ce contracts.Error
	if err := json.Unmarshal(raw, &ce); err != nil || ce.Code == "" {
		return contracts.Newf(contracts.ErrUnavailable, "the running server's admin interface answered %d", resp.StatusCode)
	}
	return &ce
}
