package voice

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

// Paths of the three audio routes (LocalAI v4.10.0, see the package
// documentation). They are the whole of the voice allowlist.
const (
	pathTranscribe = "/v1/audio/transcriptions"
	pathSpeech     = "/v1/audio/speech"
	pathVAD        = "/v1/vad"
)

// allowedEndpoints is the complete set of routes this package can call. It is
// a fixed table, not configuration, and it is deliberately separate from the
// five-route table of package localai (chat, embeddings, health, version and
// model list): neither list can widen the other. Aliases (/tts, /vad,
// /audio/...), voice listing, model install, backends, galleries, agents and
// every other route are absent, so they are unreachable through this client.
// Every route needs the API key, so the key is attached to all of them.
var allowedEndpoints = []string{
	http.MethodPost + " " + pathTranscribe,
	http.MethodPost + " " + pathSpeech,
	http.MethodPost + " " + pathVAD,
}

// AllowedEndpoints lists the routes the client may call, as "METHOD /path".
func AllowedEndpoints() []string { return append([]string(nil), allowedEndpoints...) }

// endpointSpec is a parsed Endpoint. It is parsed only after
// localai.Config.Validate accepted the URL, so every policy check (loopback or
// TLS, no credentials, no path prefix) has already run on it.
type endpointSpec struct {
	scheme string // "http", "https" or "unix"
	host   string // host:port, or a placeholder for unix
	socket string
}

const unixHost = "localhost"

func parseEndpoint(raw string) (endpointSpec, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpointSpec{}, errors.New("voice: endpoint is not a valid URL")
	}
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "unix":
		path := u.Path
		// unix:///C:/dir/x.sock on Windows: the URL path is /C:/dir/x.sock.
		if len(path) >= 3 && path[0] == '/' && filepath.IsAbs(path[1:]) {
			path = path[1:]
		}
		if path == "" {
			return endpointSpec{}, errors.New("voice: a unix endpoint needs an absolute socket path")
		}
		return endpointSpec{scheme: scheme, host: unixHost, socket: path}, nil
	case "http", "https":
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[scheme]
		}
		return endpointSpec{scheme: scheme, host: net.JoinHostPort(u.Hostname(), port)}, nil
	}
	return endpointSpec{}, errors.New("voice: endpoint scheme must be http, https or unix")
}

// loopbackOnly is a net.Dialer Control hook for plaintext endpoints. It runs
// after name resolution with the address about to be connected, so a name that
// resolves off-box is refused at connect time.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("voice: refusing to connect to an unparseable address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return errors.New("voice: refusing plaintext connection to a non-loopback address")
	}
	return nil
}

// buildTLS returns the client TLS configuration for an https endpoint:
// certificates are always verified (there is no skip switch), TLS 1.2 or later.
func buildTLS(c localai.TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, errors.New("voice: cannot read tls.ca_file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("voice: tls.ca_file holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, errors.New("voice: cannot load the tls client certificate and key")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// newTransport builds a dedicated transport: it never shares the default
// transport and ignores the proxy environment, which would otherwise carry
// audio and the key to whatever HTTPS_PROXY names. Compression is off so a
// server-side gzip layer cannot buffer an event stream or an audio stream.
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

// lookupEndpoint reports whether a request is on the allowlist. The match is
// exact: no case folding, no cleaning, no escaped path, no query.
func lookupEndpoint(method string, u *url.URL) bool {
	if u == nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || u.User != nil {
		return false
	}
	want := method + " " + u.Path
	for _, e := range allowedEndpoints {
		if e == want {
			return true
		}
	}
	return false
}

// errForbiddenEndpoint is the internal sentinel behind refused requests.
var errForbiddenEndpoint = errors.New("voice: request refused by the endpoint allowlist")

// guardTransport is the single choke point every request passes through. It
// refuses anything outside the allowlist or aimed at another scheme or host
// before any I/O, and attaches the API key itself, so the key can only travel
// to the configured host on an allowlisted route.
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

// refuseRedirects stops the client from following any redirect: a 3xx is
// returned to the caller and treated as an unexpected status. Following one
// could carry the key and the audio to another host or route.
func refuseRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
