package apppkg

import (
	"fmt"
	"strings"
	"testing"
)

const loopbackEndpointFrom = "url: https://library.example.test/mcp"

// loopbackPackage repoints the valid-minimal endpoint at url and, when entry is not empty, adds
// that block to network.egress.
func loopbackPackage(t testing.TB, url, entry string) pkgFiles {
	t.Helper()
	p := valid(t)
	m := string(p[egressManifest])
	if strings.Count(m, loopbackEndpointFrom) != 1 || strings.Count(m, "  egress:\n") != 1 {
		t.Fatal("the fixture manifest changed shape")
	}
	m = strings.Replace(m, loopbackEndpointFrom, "url: "+url, 1)
	if entry != "" {
		m = strings.Replace(m, "  egress:\n", "  egress:\n"+entry, 1)
	}
	p[egressManifest] = []byte(m)
	return p
}

func loopbackEntry(host string, port int, scheme string) string {
	return fmt.Sprintf("    loopback-app:\n      host: %s\n      ports:\n      - %d\n      scheme: %s\n      purpose: the application on this machine\n", host, port, scheme)
}

// PT-001. A loopback endpoint needs a network.egress entry with the same exact host, port and
// scheme, like any other. The mutations this pins: loopback treated as exempt again; the port or
// the scheme not compared; "localhost" and "127.0.0.1" treated as the same host; a default port
// invented for a URL that has none.
func TestLoopbackEndpointNeedsAnEgressEntry(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		entry string
		ok    bool
	}{
		{"no entry for 127.0.0.1", "http://127.0.0.1:8787/mcp", "", false},
		{"no entry for localhost", "http://localhost:8787/mcp", "", false},
		{"no entry for the IPv6 loopback", "http://[::1]:8787/mcp", "", false},
		{"no entry for another 127/8 address", "http://127.0.0.2:8787/mcp", "", false},
		{"LocalAI's published port, no entry", "http://127.0.0.1:8080/mcp", "", false},
		{"the same host, port and scheme", "http://127.0.0.1:8787/mcp", loopbackEntry("127.0.0.1", 8787, "http"), true},
		{"localhost with its own entry", "http://localhost:8787/mcp", loopbackEntry("localhost", 8787, "http"), true},
		{"another port than the entry", "http://127.0.0.1:8080/mcp", loopbackEntry("127.0.0.1", 8787, "http"), false},
		{"another scheme than the entry", "https://127.0.0.1:8787/mcp", loopbackEntry("127.0.0.1", 8787, "http"), false},
		{"localhost is not 127.0.0.1", "http://localhost:8787/mcp", loopbackEntry("127.0.0.1", 8787, "http"), false},
		{"another loopback address than the entry", "http://127.0.0.2:8787/mcp", loopbackEntry("127.0.0.1", 8787, "http"), false},
		{"no port in the url means port 80", "http://127.0.0.1/mcp", loopbackEntry("127.0.0.1", 8787, "http"), false},
		{"no port in the url, entry for 80", "http://127.0.0.1/mcp", loopbackEntry("127.0.0.1", 80, "http"), true},
		{"host case does not matter", "http://LocalHost:8787/mcp", loopbackEntry("localhost", 8787, "http"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustLoad(t, loopbackPackage(t, c.url, c.entry).signed(t), loadOptions())
			_, missing := find(res.Findings, "endpoint.egress_missing")
			if c.ok && (missing || res.Package == nil) {
				t.Fatalf("an endpoint with its entry was refused:\n%s", dump(res.Findings))
			}
			if !c.ok {
				requireCritical(t, res, "endpoint.egress_missing")
			}
		})
	}
}

// The control of the cases above: a loopback endpoint is not refused for being loopback, only for
// the missing entry, so the fixture is valid once the entry exists.
func TestLoopbackEndpointWithEntryLoadsCleanly(t *testing.T) {
	res := mustLoad(t, loopbackPackage(t, "http://127.0.0.1:8787/mcp", loopbackEntry("127.0.0.1", 8787, "http")).signed(t), loadOptions())
	if res.Package == nil || res.Critical() {
		t.Fatalf("a loopback endpoint with its entry must load:\n%s", dump(res.Findings))
	}
}

// The manifest schema does not accept an IPv6 literal as an egress host, so an IPv6 loopback
// endpoint can never be listed and is refused whatever the package says.
func TestIPv6LoopbackEndpointCannotBeListed(t *testing.T) {
	res := mustLoad(t, loopbackPackage(t, "http://[::1]:8787/mcp", loopbackEntry("::1", 8787, "http")).signed(t), loadOptions())
	if res.Package != nil || !res.Critical() {
		t.Fatalf("an IPv6 loopback endpoint loaded:\n%s", dump(res.Findings))
	}
}
