package egress

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
)

// PlatformDeny is the set of destinations that belong to the platform itself:
// the gateway's listen address, the operator admin, operations and metrics
// listeners and the LocalAI base URL (master plan 16.1: only AlixAI calls
// LocalAI, and outbound access is denied by default). A Dialer that carries one
// refuses a connection to any of them even when the allowlist names the very
// host and port, because a package's own declaration, or an operator's wide
// entry, must never turn a connector into a request-forgery primitive against
// the platform (PT-001).
//
// The composition root fills it from the running configuration. It is safe for
// concurrent use, and the Dialers that hold it see an addition at once.
//
// What an entry denies:
//   - an IP literal: that address, and every other loopback address when it is
//     loopback (a listener on 127.0.0.1 is no safer for being reached as ::1);
//   - an unspecified address or an empty host (a listener on every interface):
//     the port on every loopback, private or other local-use address;
//   - "localhost": the port on every loopback address;
//   - any other name: the port when the dialed host is that name. A name is not
//     resolved here (a lookup would itself be outbound traffic), so a name entry
//     is a backstop for a deployment that reaches a platform service by name.
type PlatformDeny struct {
	mu      sync.RWMutex
	entries []denyEntry
	sources []func() []string
}

type denyEntry struct {
	port  int
	ip    netip.Addr // valid for a literal
	name  string     // a DNS name or "localhost"
	every bool       // a listener on every interface
}

// NewPlatformDeny returns an empty deny-list.
func NewPlatformDeny() *PlatformDeny { return &PlatformDeny{} }

// Add denies one destination given as host:port ("127.0.0.1:7470",
// "[::]:7470", ":7470").
func (p *PlatformDeny) Add(hostPort string) error {
	e, err := parseDenyHostPort(hostPort)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
	return nil
}

// AddURL denies the host and port of a URL; a URL with no port means the
// default port of its scheme.
func (p *PlatformDeny) AddURL(rawURL string) error {
	e, err := parseDenyURL(rawURL)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
	return nil
}

// AddSource registers a function that lists destinations (each a host:port or a
// URL) and is called on every dial, so a value that is only known later (a
// listener that bound an ephemeral port, a configuration that was reloaded) is
// denied from the moment it exists. An entry the function returns that does not
// parse is skipped: the list can only grow stricter.
func (p *PlatformDeny) AddSource(fn func() []string) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sources = append(p.sources, fn)
}

// Len reports how many destinations are denied right now.
func (p *PlatformDeny) Len() int { return len(p.snapshot()) }

func (p *PlatformDeny) snapshot() []denyEntry {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	out := append([]denyEntry(nil), p.entries...)
	srcs := append([]func() []string(nil), p.sources...)
	p.mu.RUnlock()
	for _, fn := range srcs {
		for _, s := range fn() {
			var (
				e   denyEntry
				err error
			)
			if strings.Contains(s, "://") {
				e, err = parseDenyURL(s)
			} else {
				e, err = parseDenyHostPort(s)
			}
			if err == nil {
				out = append(out, e)
			}
		}
	}
	return out
}

func parseDenyURL(rawURL string) (denyEntry, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return denyEntry{}, fmt.Errorf("egress platform deny: %q is not a URL with a host", rawURL)
	}
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http", "ws":
			port = "80"
		case "https", "wss":
			port = "443"
		}
	}
	return newDenyEntry(u.Hostname(), port)
}

func parseDenyHostPort(hostPort string) (denyEntry, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(hostPort))
	if err != nil {
		return denyEntry{}, fmt.Errorf("egress platform deny: %q is not host:port", hostPort)
	}
	return newDenyEntry(host, port)
}

func newDenyEntry(host, portText string) (denyEntry, error) {
	port, ok := splitPort(portText)
	if !ok {
		return denyEntry{}, fmt.Errorf("egress platform deny: %q has no usable port", portText)
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	e := denyEntry{port: port}
	switch ip, err := netip.ParseAddr(host); {
	case host == "":
		e.every = true
	case err == nil:
		e.ip = ip.Unmap()
		e.every = e.ip.IsUnspecified()
	default:
		e.name = host
	}
	return e, nil
}

// denies reports whether a dial to host:port (host as dialed, addrs as it
// resolves) reaches the platform. addrs may be nil to check the name alone.
func (p *PlatformDeny) denies(host string, port int, addrs []netip.Addr) bool {
	return deniedBy(p.snapshot(), host, port, addrs)
}

// deniedBy is denies over one snapshot of the list.
func deniedBy(entries []denyEntry, host string, port int, addrs []netip.Addr) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	for _, e := range entries {
		if e.port != port {
			continue
		}
		if e.name != "" && e.name == host {
			return true
		}
		for _, a := range candidatesOf(host, addrs) {
			switch {
			case e.name == "localhost":
				if a.IsLoopback() {
					return true
				}
			case e.every:
				if a.IsLoopback() || classify(a) == classLocal {
					return true
				}
			case e.ip.IsValid():
				if a == e.ip || (e.ip.IsLoopback() && a.IsLoopback()) {
					return true
				}
			}
		}
	}
	return false
}

// candidatesOf lists the addresses a dial can land on: the resolved ones and,
// when the host is itself a literal, that literal.
func candidatesOf(host string, addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, 0, len(addrs)+1)
	if ip, err := netip.ParseAddr(host); err == nil {
		out = append(out, ip.WithZone("").Unmap())
	}
	for _, a := range addrs {
		out = append(out, a.WithZone("").Unmap())
	}
	return out
}

// WithPlatformDeny makes every Dialer built with the option, or handed out by
// a Registry built with it, refuse the destinations of p even when the
// allowlist names them. A nil p changes nothing.
func WithPlatformDeny(p *PlatformDeny) Option {
	return func(s *settings) {
		if p != nil {
			s.platform = p
		}
	}
}

// PlatformDeny returns the deny-list the registry carries: the one it was built
// with (WithPlatformDeny) or its own empty one, so the composition root can
// always fill it.
func (r *Registry) PlatformDeny() *PlatformDeny {
	if r == nil {
		return nil
	}
	return r.set.platform
}

// refusePlatform returns the typed refusal when host:port (as dialed and as it
// resolves) is a platform destination of deny, one snapshot of the list.
func (d *Dialer) refusePlatform(deny []denyEntry, host string, port int, addrs []netip.Addr) error {
	if deniedBy(deny, host, port, addrs) {
		return d.refuse(ReasonPlatformPort, host, port)
	}
	return nil
}
