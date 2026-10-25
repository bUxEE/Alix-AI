package localai

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// endpointSpec is a parsed, vetted Config.Endpoint.
type endpointSpec struct {
	scheme string // "http", "https" or "unix"
	// host is host:port for http and https, and a fixed placeholder for unix
	// (the socket path is in socket).
	host   string
	socket string
	// loopback is true for a literal loopback address or "localhost". A
	// plaintext http endpoint is only ever loopback.
	loopback bool
}

// unixHost is the Host header value used on a unix socket.
const unixHost = "localhost"

// parseEndpoint vets an endpoint URL. The rules are the security policy of
// master plan 16.1 and hardening checklist HC-38:
//
//   - http only to a literal loopback address or localhost (re-checked at
//     dial time, see loopbackOnly);
//   - https to any host, certificate always verified;
//   - unix sockets by absolute path;
//   - no credentials, path prefix, query or fragment in the URL.
func parseEndpoint(raw string) (endpointSpec, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpointSpec{}, errors.New("localai: endpoint is not a valid URL")
	}
	if u.User != nil {
		return endpointSpec{}, errors.New("localai: endpoint must not carry credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return endpointSpec{}, errors.New("localai: endpoint must not have a query or fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "unix":
		return parseUnixEndpoint(u)
	case "http", "https":
		return parseTCPEndpoint(u)
	}
	return endpointSpec{}, errors.New("localai: endpoint scheme must be http, https or unix")
}

func parseUnixEndpoint(u *url.URL) (endpointSpec, error) {
	if u.Host != "" {
		return endpointSpec{}, errors.New("localai: a unix endpoint is unix:///absolute/path")
	}
	path := u.Path
	// unix:///C:/dir/x.sock on Windows: the URL path is /C:/dir/x.sock.
	if len(path) >= 3 && path[0] == '/' && filepath.IsAbs(path[1:]) {
		path = path[1:]
	}
	if path == "" || strings.ContainsRune(path, 0) || !(filepath.IsAbs(path) || strings.HasPrefix(path, "/")) {
		return endpointSpec{}, errors.New("localai: a unix endpoint needs an absolute socket path")
	}
	return endpointSpec{scheme: "unix", host: unixHost, socket: path, loopback: true}, nil
}

func parseTCPEndpoint(u *url.URL) (endpointSpec, error) {
	scheme := strings.ToLower(u.Scheme)
	if u.Path != "" && u.Path != "/" || u.Opaque != "" {
		return endpointSpec{}, errors.New("localai: endpoint must not have a path prefix")
	}
	hostname := u.Hostname()
	if hostname == "" {
		return endpointSpec{}, errors.New("localai: endpoint has no host")
	}
	loopback, err := classifyHost(hostname)
	if err != nil {
		return endpointSpec{}, err
	}
	if !loopback && scheme != "https" {
		return endpointSpec{}, fmt.Errorf("localai: refusing plaintext http to non-loopback host %q: use loopback, a unix socket or an https endpoint with TLS (master plan 16.1)", hostname)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return endpointSpec{scheme: scheme, host: net.JoinHostPort(hostname, port), loopback: loopback}, nil
}

// classifyHost reports whether hostname is loopback: a literal 127.0.0.0/8 or
// ::1 address (an IPv4-mapped loopback included) or the name "localhost".
// Unspecified and multicast addresses are refused outright. Any other name or
// address is not loopback; it is accepted only for https.
func classifyHost(hostname string) (bool, error) {
	name := strings.TrimSuffix(strings.ToLower(hostname), ".")
	if name == "localhost" {
		return true, nil
	}
	if ip, err := netip.ParseAddr(hostname); err == nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return false, fmt.Errorf("localai: %q is not a usable endpoint address", hostname)
		}
		return ip.Unmap().IsLoopback(), nil
	}
	return false, nil
}

// loopbackOnly is a net.Dialer Control hook for plaintext endpoints. It runs
// after name resolution with the address about to be connected, so a name
// that resolves off-box (DNS rebinding, a hosts-file edit) is refused.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("localai: refusing to connect to an unparseable address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return errors.New("localai: refusing plaintext connection to a non-loopback address")
	}
	return nil
}

// buildTLS returns the client TLS configuration for an https endpoint:
// certificates are always verified (there is no skip switch), TLS 1.2 or
// later.
func buildTLS(c TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, errors.New("localai: cannot read tls.ca_file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("localai: tls.ca_file holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, errors.New("localai: cannot load the tls client certificate and key")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// newTransport builds a dedicated transport. It never shares the default
// transport and ignores the proxy environment, which would otherwise carry
// prompts and the key to whatever HTTPS_PROXY names. Compression is off so a
// server-side gzip layer cannot buffer server-sent events.
func newTransport(spec endpointSpec, tlsCfg *tls.Config) *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     tlsCfg,
	}
	switch {
	case spec.scheme == "unix":
		sock := spec.socket
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", sock)
		}
	case spec.scheme == "http":
		d.Control = loopbackOnly
		tr.DialContext = d.DialContext
	default:
		tr.DialContext = d.DialContext
	}
	return tr
}

// Endpoint allowlist ------------------------------------------------------

// endpoint is one callable route.
type endpoint struct {
	method string
	path   string
	// auth: the Authorization header is attached. The readiness route is
	// anonymous upstream and is called without the key.
	auth bool
}

// allowedEndpoints is the complete set of routes the client can call
// (hardening checklist HC-37, master plan 16.1). It is a fixed table, not
// configuration. Model install, apply, import, edit and delete, backends,
// galleries, settings, agents, MCP, the CORS proxy and metrics are absent, so
// they are unreachable through this client.
var allowedEndpoints = []endpoint{
	{method: http.MethodGet, path: "/readyz", auth: false},
	{method: http.MethodGet, path: "/version", auth: true},
	{method: http.MethodGet, path: "/v1/models", auth: true},
	{method: http.MethodPost, path: "/v1/chat/completions", auth: true},
	{method: http.MethodPost, path: "/v1/embeddings", auth: true},
}

// AllowedEndpoints lists the routes the client may call, as "METHOD /path".
func AllowedEndpoints() []string {
	out := make([]string, len(allowedEndpoints))
	for i, e := range allowedEndpoints {
		out[i] = e.method + " " + e.path
	}
	return out
}

// lookupEndpoint returns the allowlist entry for a request URL and method. The
// match is exact: no case folding, no cleaning, no escaped path, no query.
func lookupEndpoint(method string, u *url.URL) (endpoint, bool) {
	if u == nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.User != nil {
		return endpoint{}, false
	}
	for _, e := range allowedEndpoints {
		if e.method == method && e.path == u.Path {
			return e, true
		}
	}
	return endpoint{}, false
}

// errForbiddenEndpoint is the internal sentinel behind refused requests.
var errForbiddenEndpoint = errors.New("localai: request refused by the endpoint allowlist")

// guardTransport is the single choke point every request passes through. It
// refuses anything outside the allowlist or aimed at another scheme or host
// before any I/O, and attaches the API key itself, so the key can only travel
// to the configured host on an authenticated allowlisted route.
type guardTransport struct {
	next   http.RoundTripper
	scheme string
	host   string
	key    Secret
}

func (g *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	e, ok := lookupEndpoint(req.Method, req.URL)
	if !ok || !strings.EqualFold(req.URL.Scheme, g.scheme) || !strings.EqualFold(req.URL.Host, g.host) {
		closeBody(req)
		return nil, errForbiddenEndpoint
	}
	out := req.Clone(req.Context())
	out.Header.Del("Authorization")
	if e.auth && g.key.IsSet() {
		out.Header.Set("Authorization", "Bearer "+g.key.reveal())
	}
	return g.next.RoundTrip(out)
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// refuseRedirects stops the client from following any redirect: a 3xx is
// returned to the caller and treated as an unexpected status. Following one
// could carry the key and the prompt to another host or route.
func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
