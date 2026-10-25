package egress

import "net/netip"

// addrClass is how reachable an address is (see the package comment).
type addrClass int

const (
	// classPublic: ordinary routable address.
	classPublic addrClass = iota
	// classLocal: loopback, private or special-use. Reachable only when the
	// allowlist entry is that literal (or "localhost" for loopback).
	classLocal
	// classNever: link-local, cloud metadata, unspecified, multicast,
	// broadcast and reserved blocks. No entry can reach them.
	classNever
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

var (
	// neverV4 and neverV6 are unreachable even when listed.
	neverV4 = prefixes(
		"0.0.0.0/8",          // "this network", unspecified
		"169.254.0.0/16",     // link-local; includes the 169.254.169.254 metadata service and 169.254.170.2
		"224.0.0.0/4",        // multicast
		"240.0.0.0/4",        // reserved, includes 255.255.255.255 broadcast
		"168.63.129.16/32",   // Azure wire server
		"100.100.100.200/32", // Alibaba Cloud metadata
		"192.0.0.192/32",     // Oracle Cloud metadata
	)
	neverV6 = prefixes(
		"::/128",        // unspecified
		"fe80::/10",     // link-local
		"ff00::/8",      // multicast
		"fd00:ec2::/32", // AWS IMDS over IPv6 (fd00:ec2::254)
		"fd20:ce::/32",  // Google Cloud metadata over IPv6
	)
	// localV4 and localV6 are special-use blocks reachable only by a literal
	// allowlist entry.
	localV4 = prefixes(
		"127.0.0.0/8",     // loopback
		"10.0.0.0/8",      // RFC 1918
		"172.16.0.0/12",   // RFC 1918
		"192.168.0.0/16",  // RFC 1918
		"100.64.0.0/10",   // carrier-grade NAT
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation
		"192.88.99.0/24",  // 6to4 relay anycast
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
	)
	localV6 = prefixes(
		"::1/128",        // loopback
		"fc00::/7",       // unique local
		"100::/64",       // discard-only
		"2001::/32",      // Teredo
		"2001:db8::/32",  // documentation
		"64:ff9b:1::/48", // local-use NAT64
	)
	// globalV6 is the only IPv6 range handed out as public unicast.
	globalV6 = netip.MustParsePrefix("2000::/3")

	nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
)

func inAny(ps []netip.Prefix, ip netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// worse returns the more restrictive of two classes.
func worse(a, b addrClass) addrClass {
	if b > a {
		return b
	}
	return a
}

// classify says how reachable ip is. IPv6 addresses that carry an IPv4 address
// (IPv4-mapped, IPv4-compatible, NAT64, 6to4) are classified as the stricter of
// the IPv6 range they sit in and the IPv4 address they carry, so no spelling of
// a loopback or metadata address passes. An invalid address is never.
func classify(ip netip.Addr) addrClass {
	if !ip.IsValid() {
		return classNever
	}
	ip = ip.WithZone("")
	if ip.Is4In6() {
		return classifyV4(ip.Unmap())
	}
	if ip.Is4() {
		return classifyV4(ip)
	}
	return classifyV6(ip)
}

func classifyV4(ip netip.Addr) addrClass {
	switch {
	case inAny(neverV4, ip):
		return classNever
	case inAny(localV4, ip):
		return classLocal
	}
	return classPublic
}

func classifyV6(ip netip.Addr) addrClass {
	b := ip.As16()
	c := classPublic
	// ::1 has 96 leading zero bits too, but it is the loopback address, not
	// a carried IPv4 address.
	v4compat := isV4Compatible(b) && ip != netip.IPv6Loopback()
	switch {
	case inAny(neverV6, ip):
		return classNever
	case inAny(localV6, ip):
		c = classLocal
	case !globalV6.Contains(ip) && !nat64WellKnown.Contains(ip) && !v4compat:
		// Outside 2000::/3 and not a form that carries an IPv4 address:
		// reserved or not allocated.
		c = classLocal
	}
	// Forms that carry an IPv4 address: the carried address must pass too.
	switch {
	case nat64WellKnown.Contains(ip):
		c = worse(c, classifyV4(netip.AddrFrom4([4]byte(b[12:16]))))
	case sixToFour.Contains(ip):
		c = worse(c, classifyV4(netip.AddrFrom4([4]byte(b[2:6]))))
	case v4compat:
		// ::a.b.c.d (deprecated IPv4-compatible): the first 96 bits are zero.
		c = worse(c, worse(classLocal, classifyV4(netip.AddrFrom4([4]byte(b[12:16])))))
	}
	return c
}

// isV4Compatible reports whether the first 96 bits are zero, so the address is
// ::a.b.c.d. ::, ::1 and the like are caught by their own prefixes before this
// matters.
func isV4Compatible(b [16]byte) bool {
	for _, x := range b[:12] {
		if x != 0 {
			return false
		}
	}
	return true
}
