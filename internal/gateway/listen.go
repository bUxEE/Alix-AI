package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	// ErrInsecureListener marks the refusal to listen beyond loopback without
	// TLS (SEC-093, master plan 16.1).
	ErrInsecureListener = errors.New("a listener beyond loopback needs tls.cert_file and tls.key_file")
	// ErrSpikeNeedsLoopback marks the refusal to serve the unauthenticated
	// dev spike endpoint anywhere but loopback or a unix socket (SEC-092),
	// whatever the TLS setting.
	ErrSpikeNeedsLoopback = errors.New("dev.spike_endpoint is unauthenticated and needs a loopback or unix socket listener")
)

// ListenPolicy is what the listener decision depends on.
type ListenPolicy struct {
	// TLS is true when a server certificate and key are configured.
	TLS bool
	// SpikeEndpoint is true when the unauthenticated dev endpoint is enabled.
	SpikeEndpoint bool
}

// isLoopbackHost reports whether host (no port) is a literal loopback address
// or "localhost". An empty host (all interfaces), a wildcard, a LAN address and
// any other name are not loopback: a name is not resolved here, because the
// answer could change between this check and the bind.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.Unmap().IsLoopback()
}

// isLoopbackAddr reports whether a bound address is local: a loopback IP or a
// unix socket. Anything else, including an unspecified IP and an address type
// this code does not know, is not.
func isLoopbackAddr(a net.Addr) bool {
	switch v := a.(type) {
	case *net.TCPAddr:
		return v.IP != nil && v.IP.IsLoopback()
	case *net.UnixAddr:
		return true
	}
	return false
}

// CheckAddr decides, before anything is bound, whether addr (host:port) may be
// listened on. A non-loopback address needs TLS, and the unauthenticated spike
// endpoint is never served beyond loopback. The refusals are validation_failed
// errors wrapping ErrInsecureListener or ErrSpikeNeedsLoopback.
func (p ListenPolicy) CheckAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return contracts.Wrap(contracts.ErrValidationFailed, err, "gateway: listen address must be host:port")
	}
	if isLoopbackHost(host) {
		return nil
	}
	if p.SpikeEndpoint {
		return contracts.Wrap(contracts.ErrValidationFailed, ErrSpikeNeedsLoopback, "gateway: refusing to listen on "+addr)
	}
	if !p.TLS {
		return contracts.Wrap(contracts.ErrValidationFailed, ErrInsecureListener, "gateway: refusing to listen on "+addr)
	}
	return nil
}

// CheckListener repeats the decision on an already bound listener, so a
// listener handed in (or a name that resolved somewhere unexpected) cannot get
// around CheckAddr.
func (p ListenPolicy) CheckListener(ln net.Listener) error {
	if isLoopbackAddr(ln.Addr()) {
		return nil
	}
	if p.SpikeEndpoint {
		return contracts.Wrap(contracts.ErrValidationFailed, ErrSpikeNeedsLoopback, "gateway: listener "+ln.Addr().String()+" is not loopback")
	}
	if !p.TLS {
		return contracts.Wrap(contracts.ErrValidationFailed, ErrInsecureListener, "gateway: listener "+ln.Addr().String()+" is not loopback")
	}
	return nil
}

// Listen binds addr after CheckAddr, then checks the bound address too.
func Listen(addr string, p ListenPolicy) (net.Listener, error) {
	if err := p.CheckAddr(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "gateway: cannot listen on "+addr)
	}
	if err := p.CheckListener(ln); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// serverTLS reads the certificate files and builds the server's TLS
// configuration: TLS 1.2 or later, Go's default AEAD cipher suites, and, when
// a client CA is configured, mandatory verified client certificates.
func (t TLSConfig) serverTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("gateway: tls certificate: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if t.ClientCAFile != "" {
		pem, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, fmt.Errorf("gateway: tls client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("gateway: tls client CA file holds no PEM certificate")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}
