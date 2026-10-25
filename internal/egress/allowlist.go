package egress

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Schemes an allowlist entry can name.
const (
	SchemeHTTPS = "https"
	SchemeHTTP  = "http"
)

// Limits on one allowlist. The manifest allows 64 egress entries with 8 ports
// each; these leave room for the union of several connectors.
const (
	maxEntries       = 256
	maxPortsPerEntry = 16
	maxHostLen       = 253
)

// Destination is one allowed place to connect to. It mirrors an entry of
// network.egress in the application package manifest.
type Destination struct {
	// Host is an exact DNS name or an IP literal (IPv4, or IPv6 without
	// brackets or zone). It is matched exactly: there are no wildcards, and
	// "example.com" does not cover "www.example.com". DNS names compare
	// case-insensitively.
	Host string
	// Ports are the allowed destination ports, 1 to 65535. At least one.
	Ports []int
	// Scheme is "https", or "http" for loopback hosts only ("localhost" or a
	// loopback IP literal). It is enforced at the HTTP layer; a raw dial
	// checks host and port only.
	Scheme string
}

// hostnameRE is the manifest's hostname grammar (defs.schema.json
// #/$defs/hostname) applied to the lower-cased host.
var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// hostDelimiters are characters that can never be part of a bare host. They
// are what a URL, a host:port pair, userinfo or a wildcard would add.
const hostDelimiters = "*?/\\@#[]%"

type entry struct {
	// host is the canonical host: the lower-case name, or ip.String() for a
	// literal.
	host        string
	ip          netip.Addr // valid for a literal
	isLocalhost bool       // the name "localhost"
	ports       []int
	scheme      string
}

func (e entry) literal() bool { return e.ip.IsValid() }

// Allowlist is an immutable set of destinations. The zero value and a nil
// *Allowlist allow nothing.
type Allowlist struct {
	entries []entry
}

// NewAllowlist validates and freezes a list of destinations. It refuses
// wildcards, empty or malformed hosts, URLs and host:port strings in place of
// a host, ambiguous numeric hosts (a host whose last label is a number must be
// a canonical IPv4 address), ports outside 1 to 65535 and a plain-http entry
// for anything but loopback. An empty list is valid and allows nothing.
func NewAllowlist(dests ...Destination) (*Allowlist, error) {
	if len(dests) > maxEntries {
		return nil, invalidList("allowlist has %d entries, the limit is %d", len(dests), maxEntries)
	}
	a := &Allowlist{entries: make([]entry, 0, len(dests))}
	for i, d := range dests {
		e, err := newEntry(d)
		if err != nil {
			return nil, invalidList("destination %d: %v", i, err)
		}
		a.entries = append(a.entries, e)
	}
	return a, nil
}

func invalidList(format string, args ...any) error {
	return contracts.New(contracts.ErrValidationFailed, "egress allowlist: "+fmt.Sprintf(format, args...))
}

func newEntry(d Destination) (entry, error) {
	var e entry
	host := d.Host
	switch {
	case host == "":
		return e, fmt.Errorf("host is empty")
	case len(host) > maxHostLen:
		return e, fmt.Errorf("host is longer than %d characters", maxHostLen)
	case strings.ContainsAny(host, hostDelimiters+" \t\r\n"):
		return e, fmt.Errorf("host must be a bare host name or IP address: no wildcard, scheme, port, path, userinfo, brackets or zone")
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		e.host, e.isLocalhost = host, true
	} else if ip, err := netip.ParseAddr(host); err == nil {
		e.ip = ip.Unmap()
		e.host = e.ip.String()
	} else {
		switch {
		case strings.Contains(host, ":"):
			return e, fmt.Errorf("host is not a valid IP address")
		case !hostnameRE.MatchString(host):
			return e, fmt.Errorf("host is not a valid lower-case DNS name")
		case lastLabelNumeric(host):
			return e, fmt.Errorf("host looks like an IPv4 address but is not a canonical dotted quad")
		}
		e.host = host
	}

	if len(d.Ports) == 0 || len(d.Ports) > maxPortsPerEntry {
		return e, fmt.Errorf("ports must list between 1 and %d ports", maxPortsPerEntry)
	}
	for _, p := range d.Ports {
		if p < 1 || p > 65535 {
			return e, fmt.Errorf("port %d is outside 1 to 65535", p)
		}
	}
	e.ports = slices.Clone(d.Ports)
	slices.Sort(e.ports)
	e.ports = slices.Compact(e.ports)

	switch d.Scheme {
	case SchemeHTTPS:
	case SchemeHTTP:
		if !e.isLocalhost && !(e.literal() && e.ip.IsLoopback()) {
			return e, fmt.Errorf("plain http is allowed only to localhost or a loopback address")
		}
	default:
		return e, fmt.Errorf("scheme must be %q or %q", SchemeHTTPS, SchemeHTTP)
	}
	e.scheme = d.Scheme
	return e, nil
}

// lastLabelNumeric reports whether the final label is all digits or a 0x hex
// number: such a host is read as an IPv4 address by some resolvers.
func lastLabelNumeric(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if strings.HasPrefix(last, "0x") {
		return true
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return false
		}
	}
	return last != ""
}

// Destinations returns a copy of the allowed destinations.
func (a *Allowlist) Destinations() []Destination {
	if a == nil {
		return nil
	}
	out := make([]Destination, len(a.entries))
	for i, e := range a.entries {
		out[i] = Destination{Host: e.host, Ports: slices.Clone(e.ports), Scheme: e.scheme}
	}
	return out
}

// Len returns the number of entries.
func (a *Allowlist) Len() int {
	if a == nil {
		return 0
	}
	return len(a.entries)
}

// Allows reports whether host and port are both on the allowlist. It is an
// exact match; it does not resolve the name or look at the address class (the
// Dialer does).
func (a *Allowlist) Allows(host string, port int) bool {
	_, ok := a.match(host, port)
	return ok
}

// match finds the entry for a host (as it appears in an address or URL) and
// port.
func (a *Allowlist) match(host string, port int) (entry, bool) {
	if a == nil {
		return entry{}, false
	}
	canon, ip, ok := normaliseHost(host)
	if !ok {
		return entry{}, false
	}
	for _, e := range a.entries {
		if e.literal() {
			if !ip.IsValid() || ip != e.ip {
				continue
			}
		} else if ip.IsValid() || e.host != canon {
			continue
		}
		if slices.Contains(e.ports, port) {
			return e, true
		}
	}
	return entry{}, false
}

// normaliseHost canonicalises a host taken from an address or URL: one
// trailing dot is dropped, DNS names are lower-cased and IP literals are parsed
// (IPv4-mapped forms reduce to IPv4). Anything with a zone, a wildcard,
// whitespace, a control character or a delimiter is refused.
func normaliseHost(h string) (canon string, ip netip.Addr, ok bool) {
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > maxHostLen {
		return "", netip.Addr{}, false
	}
	for _, r := range h {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(hostDelimiters, r) {
			return "", netip.Addr{}, false
		}
	}
	h = strings.ToLower(h)
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		return a.String(), a, true
	}
	return h, netip.Addr{}, true
}

// splitPort converts the port text of an address to a number. Only decimal
// ports are accepted; service names ("http") are not.
func splitPort(p string) (int, bool) {
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}
	return int(n), true
}

// CheckURL reports whether a request to u is allowed: the scheme is http or
// https, there is no userinfo, the host and the effective port (the scheme's
// default when absent) match an entry exactly and the entry names the same
// scheme. A nil or empty allowlist refuses every URL.
func (a *Allowlist) CheckURL(u *url.URL) error {
	if a.Len() == 0 {
		return deny(ReasonNoAllowlist, "", 0)
	}
	if u == nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return deny(ReasonURL, "", 0)
	}
	scheme := strings.ToLower(u.Scheme)
	var port int
	switch scheme {
	case SchemeHTTPS:
		port = 443
	case SchemeHTTP:
		port = 80
	default:
		return deny(ReasonScheme, u.Hostname(), 0)
	}
	if p := u.Port(); p != "" {
		n, ok := splitPort(p)
		if !ok {
			return deny(ReasonBadAddress, u.Hostname(), 0)
		}
		port = n
	}
	e, ok := a.match(u.Hostname(), port)
	if !ok {
		return deny(ReasonNotAllowlisted, u.Hostname(), port)
	}
	if e.scheme != scheme {
		return deny(ReasonScheme, u.Hostname(), port)
	}
	return nil
}
