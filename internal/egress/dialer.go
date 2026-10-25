package egress

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Resolver turns a host name into addresses. *net.Resolver implements it; tests
// inject their own so no real DNS is involved.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

const (
	defaultDialTimeout = 15 * time.Second
	attemptTimeout     = 10 * time.Second
	// maxResolved bounds how many addresses of one answer are considered. An
	// answer that long is hostile or broken; it is refused rather than
	// truncated, because truncation would hide the addresses not examined.
	maxResolved = 32
)

type settings struct {
	resolver    Resolver
	onDeny      func(Denial)
	dialTimeout time.Duration
	tls         *tls.Config
	platform    *PlatformDeny
}

// Option configures a Dialer or a Registry.
type Option func(*settings)

// WithResolver replaces the system resolver.
func WithResolver(r Resolver) Option { return func(s *settings) { s.resolver = r } }

// WithDenyHook registers a function called, synchronously, for every refusal
// the Dialer or its HTTP client makes. It is how the audit log hears about
// them (internal/egress cannot import it). It must be fast and must not block.
func WithDenyHook(fn func(Denial)) Option { return func(s *settings) { s.onDeny = fn } }

// WithDialTimeout bounds one whole connect: resolution plus every address
// tried. The default is 15 seconds.
func WithDialTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.dialTimeout = d
		}
	}
}

// WithTLSConfig supplies the TLS settings of HTTPClient, for example a private
// CA pool or a client certificate. The minimum version is raised to TLS 1.2 and
// certificate verification cannot be turned off whatever the value says.
func WithTLSConfig(c *tls.Config) Option { return func(s *settings) { s.tls = c } }

func newSettings(opts []Option) settings {
	s := settings{dialTimeout: defaultDialTimeout}
	for _, o := range opts {
		if o != nil {
			o(&s)
		}
	}
	if s.resolver == nil {
		s.resolver = net.DefaultResolver
	}
	return s
}

// Dialer connects only to allowlisted destinations. Build one with NewDialer or
// Registry.Dialer; the zero value refuses everything. A Dialer is safe for
// concurrent use.
type Dialer struct {
	source func() *Allowlist
	app    string
	set    settings

	// dial replaces the connect step. Tests set it so no socket is opened;
	// it receives the vetted IP literal address, never a name.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewDialer returns a Dialer for one allowlist. A nil allowlist allows nothing.
func NewDialer(a *Allowlist, opts ...Option) *Dialer {
	return &Dialer{source: func() *Allowlist { return a }, set: newSettings(opts)}
}

func (d *Dialer) allowlist() *Allowlist {
	if d == nil || d.source == nil {
		return nil
	}
	return d.source()
}

// refuse records a denial and returns the typed error.
func (d *Dialer) refuse(reason Reason, host string, port int) error {
	if d != nil && d.set.onDeny != nil {
		d.set.onDeny(Denial{Application: d.app, Host: cleanHost(host), Port: port, Reason: reason})
	}
	return deny(reason, host, port)
}

// DialContext implements the dial function of net/http transports. See the
// package comment for what it refuses and how it connects.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, d.refuse(ReasonBadAddress, address, 0)
	}
	port, ok := splitPort(portText)
	if !ok {
		return nil, d.refuse(ReasonBadAddress, host, 0)
	}
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, d.refuse(ReasonBadNetwork, host, port)
	}
	allow := d.allowlist()
	if allow.Len() == 0 {
		return nil, d.refuse(ReasonNoAllowlist, host, port)
	}
	e, ok := allow.match(host, port)
	if !ok {
		// Nothing is resolved for an unlisted name: a DNS query for it would
		// itself be an outbound request.
		return nil, d.refuse(ReasonNotAllowlisted, host, port)
	}

	// The platform's own destinations are refused even when listed (PT-001).
	// The name is checked before anything is resolved, the addresses after,
	// both against one snapshot of the list.
	deny := d.set.platform.snapshot()
	if err := d.refusePlatform(deny, host, port, nil); err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, d.set.dialTimeout)
	defer cancel()

	addrs, err := d.candidates(cctx, e)
	if err != nil {
		return nil, err
	}
	if err := d.refusePlatform(deny, host, port, addrs); err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if !permitted(e, a) {
			return nil, d.refuse(ReasonBlockedAddress, host, port)
		}
	}
	addrs = familyFilter(network, addrs)
	if len(addrs) == 0 {
		return nil, contracts.New(contracts.ErrUnavailable, "egress: destination has no address for the requested network").
			WithDetails("component", component)
	}
	return d.connect(cctx, addrs, port)
}

// candidates returns the addresses a connection to entry e may use: the
// literal itself, the two loopback addresses for "localhost" (no DNS, so a
// hosts file cannot change them), or the answer of the resolver.
func (d *Dialer) candidates(ctx context.Context, e entry) ([]netip.Addr, error) {
	switch {
	case e.literal():
		return []netip.Addr{e.ip}, nil
	case e.isLocalhost:
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}, nil
	}
	res, err := d.set.resolver.LookupNetIP(ctx, "ip", e.host)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "egress: cannot resolve the destination").
			WithDetails("component", component)
	}
	if len(res) == 0 || len(res) > maxResolved {
		return nil, contracts.New(contracts.ErrUnavailable, "egress: the resolver gave no usable answer").
			WithDetails("component", component)
	}
	out := make([]netip.Addr, 0, len(res))
	for _, a := range res {
		a = a.WithZone("").Unmap()
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

// permitted decides one address for one allowlist entry.
func permitted(e entry, a netip.Addr) bool {
	switch classify(a) {
	case classPublic:
		return true
	case classLocal:
		// Reachable only because the operator named this very address, or
		// named "localhost" and this is loopback. Never because a name
		// happened to resolve here.
		return e.literal() || (e.isLocalhost && a.IsLoopback())
	}
	return false
}

func familyFilter(network string, addrs []netip.Addr) []netip.Addr {
	switch network {
	case "tcp4":
		return slices.DeleteFunc(slices.Clone(addrs), func(a netip.Addr) bool { return !a.Is4() })
	case "tcp6":
		return slices.DeleteFunc(slices.Clone(addrs), func(a netip.Addr) bool { return !a.Is6() })
	}
	return addrs
}

// connect dials the vetted addresses one after the other. It dials IP
// literals only, so the check above is the check of the connection that is
// made.
func (d *Dialer) connect(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, error) {
	var last error
	for _, a := range addrs {
		address := net.JoinHostPort(a.String(), strconv.Itoa(port))
		var (
			c   net.Conn
			err error
		)
		if d.dial != nil {
			c, err = d.dial(ctx, "tcp", address)
		} else {
			nd := net.Dialer{Timeout: attemptTimeout, KeepAlive: 30 * time.Second, Control: recheck(a)}
			c, err = nd.DialContext(ctx, "tcp", address)
		}
		if err == nil {
			return c, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// recheck is the net.Dialer Control hook: it runs on the socket about to
// connect and refuses anything but the address that was vetted. It cannot fail
// for a correct dial; it exists so that a bug above, or a platform that
// resolves behind our back, still cannot reach another address.
func recheck(vetted netip.Addr) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || ap.Addr().Unmap() != vetted {
			return deny(ReasonBlockedAddress, "", 0)
		}
		return nil
	}
}
