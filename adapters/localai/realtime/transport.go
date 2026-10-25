package realtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/buxee/alix-ai/adapters/localai"
)

// pathRealtime is the one route of the Realtime allowlist (LocalAI v4.10.0
// core/http/routes/openai.go: app.GET("/v1/realtime", ...)).
const pathRealtime = "/v1/realtime"

// allowedEndpoints is the complete set of routes this package can call: the
// Realtime WebSocket upgrade, and nothing else. It is a fixed table, separate
// from the tables of package localai and package voice, so none can widen
// another. The sibling routes of the same family (/v1/realtime/sessions,
// /transcription_session, /calls) and every other route are unreachable.
var allowedEndpoints = []string{http.MethodGet + " " + pathRealtime}

// AllowedEndpoints lists the routes the client may call, as "METHOD /path".
func AllowedEndpoints() []string { return append([]string(nil), allowedEndpoints...) }

type endpointSpec struct {
	scheme string // "http", "https" or "unix"
	host   string // host:port, or a placeholder for unix
	socket string
}

const unixHost = "localhost"

func parseEndpoint(raw string) (endpointSpec, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpointSpec{}, errors.New("realtime: endpoint is not a valid URL")
	}
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "unix":
		path := u.Path
		if len(path) >= 3 && path[0] == '/' && filepath.IsAbs(path[1:]) {
			path = path[1:]
		}
		if path == "" {
			return endpointSpec{}, errors.New("realtime: a unix endpoint needs an absolute socket path")
		}
		return endpointSpec{scheme: scheme, host: unixHost, socket: path}, nil
	case "http", "https":
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[scheme]
		}
		return endpointSpec{scheme: scheme, host: net.JoinHostPort(u.Hostname(), port)}, nil
	}
	return endpointSpec{}, errors.New("realtime: endpoint scheme must be http, https or unix")
}

// loopbackOnly is a net.Dialer Control hook for plaintext endpoints: it runs
// after name resolution, so a name that resolves off-box is refused.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("realtime: refusing to connect to an unparseable address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return errors.New("realtime: refusing plaintext connection to a non-loopback address")
	}
	return nil
}

// buildTLS returns the client TLS configuration for an https endpoint:
// certificates are always verified, TLS 1.2 or later.
func buildTLS(c localai.TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, errors.New("realtime: cannot read tls.ca_file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("realtime: tls.ca_file holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, errors.New("realtime: cannot load the tls client certificate and key")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// newTransport builds a dedicated transport: no shared default transport, no
// proxy environment (which would carry audio and the key to whatever
// HTTPS_PROXY names), no compression.
func newTransport(spec endpointSpec, tlsCfg *tls.Config) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConns:        2,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsCfg,
	}
	switch spec.scheme {
	case "unix":
		sock := spec.socket
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", sock)
		}
	case "http":
		d.Control = loopbackOnly
		tr.DialContext = d.DialContext
	default:
		tr.DialContext = d.DialContext
	}
	return tr
}

// lookupEndpoint reports whether a request is the allowlisted upgrade. The
// match is exact: method GET, the path with no escaping, no fragment, no
// credentials, and a query holding the single parameter "model" in canonical
// form. Any other parameter is refused, in particular localai_assistant, which
// would switch on the runtime's in-process management tools.
func lookupEndpoint(method string, u *url.URL) bool {
	if u == nil || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.User != nil || u.ForceQuery {
		return false
	}
	if method != http.MethodGet || u.Path != pathRealtime {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) != 1 || len(q["model"]) != 1 || q["model"][0] == "" {
		return false
	}
	return u.RawQuery == q.Encode()
}

var errForbiddenEndpoint = errors.New("realtime: request refused by the endpoint allowlist")

// guardTransport is the single choke point every request, the WebSocket
// upgrade included, passes through. It refuses anything outside the allowlist
// or aimed at another scheme or host before any I/O, and attaches the API key
// itself, so the key can only travel to the configured host on the allowlisted
// route.
type guardTransport struct {
	next   http.RoundTripper
	scheme string
	host   string
	key    apiKey
}

func (g *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !lookupEndpoint(req.Method, req.URL) || !strings.EqualFold(req.URL.Scheme, g.scheme) || !strings.EqualFold(req.URL.Host, g.host) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errForbiddenEndpoint
	}
	out := req.Clone(req.Context())
	out.Header.Del("Authorization")
	if g.key.isSet() {
		out.Header.Set("Authorization", "Bearer "+g.key.reveal())
	}
	return g.next.RoundTrip(out)
}

// refuseRedirects stops the client following any redirect, which could carry
// the key and the audio to another host or route.
func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
