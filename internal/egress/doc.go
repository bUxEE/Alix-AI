// Package egress is the outbound network gate of AlixAI (master plan 16.1:
// "Outbound network access is denied by default. Each application/API/MCP
// connector receives an explicit destination allowlist"). Every connector that
// reaches an application over the network (the MCP HTTP transport, the OpenAPI
// adapter, the document API source, the native adapter's TCP fallback) dials
// through a Dialer built here, so the rule lives in one place and not in each
// transport.
//
// # Model
//
// An Allowlist is an immutable list of Destinations: an exact host name or IP
// literal, the ports allowed on it and the scheme (https, or plain http to
// loopback only). It mirrors network.egress of the application package
// manifest, which carries no wildcards, and so does this package: "*",
// "*.example.com" and partial names are refused when the list is built, and a
// request matches only when the host and the port both match exactly (host
// names compare case-insensitively with one trailing dot ignored). Subdomains
// and parent domains of a listed name are different hosts.
//
// A Registry holds one Allowlist per application. An application without an
// entry has no allowlist and can reach nothing: default deny is the state of
// every Dialer, Allowlist and Registry value that has not been configured,
// including the zero values and a nil *Allowlist.
//
// # What the dialer does
//
// Dialer.DialContext refuses, with a typed permission_denied error and before
// any packet is sent:
//
//   - any network other than tcp, tcp4 and tcp6;
//   - a host and port that are not on the allowlist;
//   - a destination whose address is not safe to reach (see below).
//
// For an allowed host name it resolves the name itself, classifies every
// address that came back and connects to the vetted IP literal, never to the
// name again, so a resolver that answers differently the second time (DNS
// rebinding) cannot redirect the connection that was checked. Every dial
// resolves again and is checked again; nothing about an earlier answer is
// cached. A net.Dialer Control hook re-checks the address of the socket itself
// as defence in depth.
//
// # Addresses that are never reached
//
// Addresses are examined after IPv4-mapped (::ffff:a.b.c.d), IPv4-compatible,
// NAT64 (64:ff9b::/96) and 6to4 (2002::/16) forms are reduced to the IPv4
// address they carry. Three classes result:
//
//   - never: link-local (169.254.0.0/16, fe80::/10), the cloud metadata
//     addresses (169.254.169.254, 169.254.170.2, 168.63.129.16,
//     100.100.100.200, 192.0.0.192, fd00:ec2::254), the unspecified address,
//     multicast, broadcast and the 0.0.0.0/8 and 240.0.0.0/4 blocks. No
//     allowlist entry can reach them.
//   - local: loopback, RFC 1918 and unique-local ranges, carrier-grade NAT
//     (100.64.0.0/10), documentation, benchmarking and other special-use
//     blocks. They are reachable only when the allowlist entry is itself that
//     literal address (an operator who writes 10.1.2.3 vouches for it), or is
//     the name "localhost" and the address is loopback. A host NAME that
//     resolves to a local address is refused even when it is listed: that is
//     what a rebinding attack looks like, and an operator who runs an
//     application on a private address lists the address.
//   - public: everything else.
//
// A name whose answer contains any address that fails the check is refused as a
// whole; the dialer never falls back to the addresses that pass.
//
// # HTTP
//
// Dialer.HTTPClient returns a client for connectors that speak HTTP. Its
// transport ignores the proxy environment (a proxy would move the dial to the
// proxy and bypass the check), dials through the Dialer, verifies TLS
// certificates with no skip switch, and checks every request URL against the
// allowlist before any I/O: scheme and exact host and port, no userinfo. A
// redirect is followed only to another allowlisted destination, never from
// https to http, at most three times, and a redirect to a different host or
// port drops every request header except a short safe list so credentials
// sent in custom headers do not follow it.
//
// # Errors
//
// A refusal is a *contracts.Error with code permission_denied and
// Details["component"] == "egress" and Details["reason"] one of the Reason
// constants; IsDenied and ReasonOf read it. A failure to resolve a name is
// unavailable. Errors carry the destination host and port, never a URL, path,
// query or header.
//
// # Limits of this package
//
// It decides where a connection may go. It does not limit response size, rate
// or time: those belong to the connector (adapters.ReadLimited and
// adapters.Wrap). Revoking an allowlist in a Registry applies to the next dial;
// connections a Transport already keeps alive are closed by the caller.
package egress
