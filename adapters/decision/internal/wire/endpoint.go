// Package wire is the transport the two decision scorer adapters
// (adapters/decision/localai and adapters/decision/clm) share. It is private to
// adapters/decision (a Go internal package) and imports only the standard
// library, pkg/contracts and the generated decision contract.
//
// It enforces, for every request, the same posture as adapters/localai (master
// plan 16.1, HC-37, HC-38):
//
//   - plaintext http only to a literal loopback address or localhost, re-checked
//     at dial time; https to any host with the certificate always verified; unix
//     sockets by absolute path;
//   - a fixed, per-adapter endpoint allowlist (method and exact path), separate
//     from the five-route allowlist of adapters/localai and from the voice one,
//     so none of them can widen another;
//   - no redirect is followed, no proxy from the environment is used;
//   - responses are size capped, and no request or response body is ever copied
//     into an error or a log record.
package wire

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

// TLSConfig is the TLS client configuration for an https endpoint. There is no
// switch that disables certificate verification.
type TLSConfig struct {
	// CAFile is a PEM bundle that replaces the system roots.
	CAFile string `yaml:"ca_file"`
	// CertFile and KeyFile are a client certificate for mutual TLS.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ServerName overrides the name the server certificate must match.
	ServerName string `yaml:"server_name"`
}

// IsZero reports whether no TLS setting is present.
func (t TLSConfig) IsZero() bool { return t == TLSConfig{} }

// Endpoint is a parsed, vetted endpoint URL.
type Endpoint struct {
	// Scheme is "http", "https" or "unix".
	Scheme string
	// Host is host:port for http and https and a fixed placeholder for unix.
	Host string
	// Socket is the socket path of a unix endpoint.
	Socket string
	// Loopback is true for a literal loopback address, localhost or a unix socket.
	Loopback bool
}

// RequestScheme is the scheme requests carry (a unix endpoint speaks http).
func (e Endpoint) RequestScheme() string {
	if e.Scheme == "unix" {
		return "http"
	}
	return e.Scheme
}

const unixHost = "localhost"

// ParseEndpoint vets an endpoint URL. who prefixes the messages ("localai
// decisions"). The rules: http only to loopback; https to any host; unix by
// absolute path; no credentials, path prefix, query or fragment.
func ParseEndpoint(who, raw string) (Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%s: endpoint is not a valid URL", who)
	}
	if u.User != nil {
		return Endpoint{}, fmt.Errorf("%s: endpoint must not carry credentials", who)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return Endpoint{}, fmt.Errorf("%s: endpoint must not have a query or fragment", who)
	}
	switch strings.ToLower(u.Scheme) {
	case "unix":
		return parseUnix(who, u)
	case "http", "https":
		return parseTCP(who, u)
	}
	return Endpoint{}, fmt.Errorf("%s: endpoint scheme must be http, https or unix", who)
}

func parseUnix(who string, u *url.URL) (Endpoint, error) {
	if u.Host != "" {
		return Endpoint{}, fmt.Errorf("%s: a unix endpoint is unix:///absolute/path", who)
	}
	path := u.Path
	// unix:///C:/dir/x.sock on Windows: the URL path is /C:/dir/x.sock.
	if len(path) >= 3 && path[0] == '/' && filepath.IsAbs(path[1:]) {
		path = path[1:]
	}
	if path == "" || path == "/" || strings.HasSuffix(path, "/") || strings.ContainsRune(path, 0) || !(filepath.IsAbs(path) || strings.HasPrefix(path, "/")) {
		return Endpoint{}, fmt.Errorf("%s: a unix endpoint needs an absolute socket path", who)
	}
	return Endpoint{Scheme: "unix", Host: unixHost, Socket: path, Loopback: true}, nil
}

func parseTCP(who string, u *url.URL) (Endpoint, error) {
	scheme := strings.ToLower(u.Scheme)
	if u.Path != "" && u.Path != "/" || u.Opaque != "" {
		return Endpoint{}, fmt.Errorf("%s: endpoint must not have a path prefix", who)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return Endpoint{}, fmt.Errorf("%s: endpoint has no host", who)
	}
	loopback, err := classifyHost(who, hostname)
	if err != nil {
		return Endpoint{}, err
	}
	if !loopback && scheme != "https" {
		return Endpoint{}, fmt.Errorf("%s: refusing plaintext http to a non-loopback host: use loopback, a unix socket or an https endpoint with TLS (master plan 16.1)", who)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return Endpoint{Scheme: scheme, Host: net.JoinHostPort(hostname, port), Loopback: loopback}, nil
}

func classifyHost(who, hostname string) (bool, error) {
	name := strings.TrimSuffix(strings.ToLower(hostname), ".")
	if name == "localhost" {
		return true, nil
	}
	if ip, err := netip.ParseAddr(hostname); err == nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return false, fmt.Errorf("%s: endpoint address is not usable", who)
		}
		return ip.Unmap().IsLoopback(), nil
	}
	return false, nil
}

// loopbackOnly is a net.Dialer Control hook for plaintext endpoints. It runs
// after name resolution with the address about to be connected, so a name that
// resolves off-box (DNS rebinding, a hosts-file edit) is refused.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("decision wire: refusing to connect to an unparseable address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Unmap().IsLoopback() {
		return errors.New("decision wire: refusing plaintext connection to a non-loopback address")
	}
	return nil
}

// BuildTLS returns the client TLS configuration of an https endpoint.
func BuildTLS(who string, c TLSConfig) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot read tls.ca_file", who)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: tls.ca_file holds no PEM certificate", who)
		}
		cfg.RootCAs = pool
	}
	if c.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("%s: cannot load the tls client certificate and key", who)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// ValidateTLS checks the TLS settings against the endpoint without touching
// the file system.
func ValidateTLS(who string, ep Endpoint, c TLSConfig) error {
	if !c.IsZero() && ep.Scheme != "https" {
		return fmt.Errorf("%s: tls settings need an https endpoint", who)
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return fmt.Errorf("%s: tls.cert_file and tls.key_file must be set together", who)
	}
	return nil
}

// newTransport builds a dedicated transport: never the default one, no proxy
// from the environment, no compression.
func newTransport(ep Endpoint, tlsCfg *tls.Config) *http.Transport {
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
	switch ep.Scheme {
	case "unix":
		sock := ep.Socket
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
