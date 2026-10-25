package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

const apiHost = "api.example.test"

// publicIP is a routable address used where a name must resolve to something
// reachable. No connection is made to it: the connect step is replaced.
const publicIP = "93.184.216.34"

// TestEgressDenyByDefault: every state in which nothing was configured refuses
// every destination, with a typed error, before any name is resolved and before
// any connection is attempted.
//
// SEC-055
func TestEgressDenyByDefault(t *testing.T) {
	res := newResolver(apiHost, []string{publicIP})
	other := mustList(t, Destination{Host: "other.example.test", Ports: []int{443}, Scheme: SchemeHTTPS})

	reg := NewRegistry(WithResolver(res))
	if err := reg.Set("app_other", other); err != nil {
		t.Fatal(err)
	}
	revoked := mustList(t, Destination{Host: apiHost, Ports: []int{443}, Scheme: SchemeHTTPS})
	if err := reg.Set("app_revoked", revoked); err != nil {
		t.Fatal(err)
	}
	reg.Revoke("app_revoked")

	rec := &dialRecorder{}
	cases := []struct {
		name string
		d    *Dialer
		want Reason
	}{
		{"nil allowlist", NewDialer(nil, WithResolver(res)), ReasonNoAllowlist},
		{"empty allowlist", NewDialer(mustList(t), WithResolver(res)), ReasonNoAllowlist},
		{"zero value dialer", &Dialer{}, ReasonNoAllowlist},
		{"nil dialer", nil, ReasonNoAllowlist},
		{"application never configured", reg.Dialer("app_unknown"), ReasonNoAllowlist},
		{"application with no id", reg.Dialer(""), ReasonNoAllowlist},
		{"another application's allowlist", reg.Dialer("app_a"), ReasonNoAllowlist},
		{"revoked application", reg.Dialer("app_revoked"), ReasonNoAllowlist},
		{"nil registry", (*Registry)(nil).Dialer("app_a"), ReasonNoAllowlist},
		{"listed for someone else, not for me", func() *Dialer {
			d := reg.Dialer("app_other")
			d.dial = rec.dial
			return d
		}(), ReasonNotAllowlisted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.d != nil && tc.d.dial == nil {
				tc.d.dial = rec.dial
			}
			for _, address := range []string{apiHost + ":443", publicIP + ":443", "127.0.0.1:80", "localhost:8080"} {
				c, err := tc.d.DialContext(context.Background(), "tcp", address)
				if err == nil {
					closeConn(c)
					t.Fatalf("dial %s succeeded with nothing allowed", address)
				}
				if !IsDenied(err) || ReasonOf(err) != tc.want {
					t.Fatalf("dial %s: got %v (reason %q), want a %s refusal", address, err, ReasonOf(err), tc.want)
				}
				if contracts.CodeOf(err) != contracts.ErrPermissionDenied || contracts.CodeOf(err).Retryable() {
					t.Fatalf("dial %s: code %s, want a non-retryable permission_denied", address, contracts.CodeOf(err))
				}
			}
		})
	}
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("the connect step ran %d times under default deny: %v", len(got), got)
	}
	if res.total() != 0 {
		t.Fatalf("a name was resolved under default deny (%d lookups)", res.total())
	}

	t.Run("http client", func(t *testing.T) {
		var hits atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		defer srv.Close()

		for name, d := range map[string]*Dialer{
			"nil allowlist": NewDialer(nil),
			"unconfigured":  reg.Dialer("app_unknown"),
			"zero value":    {},
		} {
			resp, err := d.HTTPClient(egressTestWait).Get(srv.URL + "/secret?token=abc")
			if err == nil {
				resp.Body.Close()
				t.Fatalf("%s: request to a loopback server succeeded with nothing allowed", name)
			}
			if !IsDenied(err) {
				t.Fatalf("%s: got %v, want an egress refusal", name, err)
			}
			if strings.Contains(StripURL(err).Error(), "token=abc") {
				t.Fatalf("%s: stripped error still carries the query: %v", name, StripURL(err))
			}
		}
		if hits.Load() != 0 {
			t.Fatalf("the server saw %d requests that default deny should have stopped", hits.Load())
		}
	})

	t.Run("registry changes apply to dialers already handed out", func(t *testing.T) {
		d := reg.Dialer("app_live")
		d.dial = rec.dial
		if c, err := d.DialContext(context.Background(), "tcp", publicIP+":443"); err == nil {
			closeConn(c)
			t.Fatal("dial succeeded before the application was configured")
		}
		if err := reg.Set("app_live", mustList(t, Destination{Host: publicIP, Ports: []int{443}, Scheme: SchemeHTTPS})); err != nil {
			t.Fatal(err)
		}
		c, err := d.DialContext(context.Background(), "tcp", publicIP+":443")
		if err != nil {
			t.Fatalf("dial after Set: %v", err)
		}
		closeConn(c)
		reg.Revoke("app_live")
		if c, err := d.DialContext(context.Background(), "tcp", publicIP+":443"); err == nil {
			closeConn(c)
			t.Fatal("dial succeeded after Revoke")
		}
	})

	t.Run("deny hook hears every refusal", func(t *testing.T) {
		var mu sync.Mutex
		var got []Denial
		reg := NewRegistry(WithDenyHook(func(d Denial) {
			mu.Lock()
			got = append(got, d)
			mu.Unlock()
		}))
		d := reg.Dialer("app_hook")
		_, _ = d.DialContext(context.Background(), "tcp", apiHost+":443")
		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 || got[0].Application != "app_hook" || got[0].Host != apiHost || got[0].Port != 443 || got[0].Reason != ReasonNoAllowlist {
			t.Fatalf("deny hook got %+v", got)
		}
	})
}

// TestEgressAllowlistExactHostPort: a destination is reachable only when the
// host and the port both match an entry exactly. Parent domains, subdomains,
// look-alike names, other ports, userinfo tricks and IP spellings of a listed
// name all fail, and a refused name is never resolved.
//
// SEC-055
func TestEgressAllowlistExactHostPort(t *testing.T) {
	list := mustList(t,
		Destination{Host: apiHost, Ports: []int{443}, Scheme: SchemeHTTPS},
		Destination{Host: "multi.example.test", Ports: []int{443, 8443}, Scheme: SchemeHTTPS},
		Destination{Host: "10.1.2.3", Ports: []int{8443}, Scheme: SchemeHTTPS},
	)
	res := newResolver(apiHost, []string{publicIP})
	res.set("multi.example.test", []string{publicIP})

	allowed := []string{
		apiHost + ":443",
		"API.Example.TEST:443",
		apiHost + ".:443",
		"multi.example.test:443",
		"multi.example.test:8443",
		"10.1.2.3:8443",
		"[::ffff:10.1.2.3]:8443",
	}
	for _, address := range allowed {
		t.Run("allowed "+address, func(t *testing.T) {
			d, rec := hooked(t, list, res)
			c, err := d.DialContext(context.Background(), "tcp", address)
			if err != nil {
				t.Fatalf("got %v, want the dial to be allowed", err)
			}
			closeConn(c)
			if len(rec.calls()) != 1 {
				t.Fatalf("connect step ran %d times, want 1", len(rec.calls()))
			}
		})
	}

	denied := []string{
		apiHost + ":444",              // right host, wrong port
		apiHost + ":80",               // right host, default http port
		apiHost + ":8443",             // another entry's port
		"multi.example.test:80",       //
		"example.test:443",            // parent domain
		"www." + apiHost + ":443",     // subdomain
		"xapi.example.test:443",       // longer label
		"pi.example.test:443",         // shorter label
		apiHost + ".evil.test:443",    // listed name as a prefix
		"evil-" + apiHost + ":443",    //
		apiHost + "@evil.test:443",    // userinfo spelling
		apiHost + "%00.evil.test:443", // NUL spelling
		apiHost + " :443",             // trailing space
		"*.example.test:443",          // wildcard text
		"evil.test:443",               //
		":443",                        // empty host
		"[::1]:443",                   // IPv6 loopback, not listed
		publicIP + ":443",             // the IP of a listed name is not the name
		"10.1.2.3:443",                // listed IP, wrong port
		"10.1.2.4:8443",               // neighbouring IP
		"010.1.2.3:8443",              // octal-looking spelling
		"0x0a.1.2.3:8443",             // hex spelling
		"167838211:8443",              // decimal spelling of 10.1.2.3
		"10.1.2.3.evil.test:8443",     // IP as a name prefix
		"[::ffff:10.1.2.4]:8443",      // mapped neighbour
		apiHost + ":0",                // port zero
		apiHost + ":65536",            // port out of range
		apiHost + ":https",            // service name
		apiHost + ":+443",             // signed port
		strings.Repeat("a", 300) + ".example.test:443",
	}
	for _, address := range denied {
		t.Run("denied "+address[:min(len(address), 60)], func(t *testing.T) {
			res := newResolver(apiHost, []string{publicIP})
			d, rec := hooked(t, list, res)
			c, err := d.DialContext(context.Background(), "tcp", address)
			if err == nil {
				closeConn(c)
				t.Fatalf("dial to %q succeeded, want a refusal", address)
			}
			if !IsDenied(err) {
				t.Fatalf("got %v, want an egress refusal", err)
			}
			if len(rec.calls()) != 0 {
				t.Fatalf("connect step ran for a refused destination: %v", rec.calls())
			}
			if res.total() != 0 {
				t.Fatal("a refused destination was resolved")
			}
		})
	}

	t.Run("the dialer connects to the vetted IP, never to the name", func(t *testing.T) {
		d, rec := hooked(t, list, res)
		c, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
		if err != nil {
			t.Fatal(err)
		}
		closeConn(c)
		if got := rec.calls(); len(got) != 1 || got[0] != publicIP+":443" {
			t.Fatalf("connect step got %v, want [%s:443]", got, publicIP)
		}
	})

	t.Run("networks other than tcp are refused", func(t *testing.T) {
		d, rec := hooked(t, list, res)
		for _, n := range []string{"udp", "unix", "ip4:icmp", "tcp5", ""} {
			c, err := d.DialContext(context.Background(), n, apiHost+":443")
			if err == nil {
				closeConn(c)
				t.Fatalf("network %q allowed", n)
			}
			if ReasonOf(err) != ReasonBadNetwork {
				t.Fatalf("network %q: reason %q, want %q", n, ReasonOf(err), ReasonBadNetwork)
			}
		}
		if len(rec.calls()) != 0 {
			t.Fatal("connect step ran for a refused network")
		}
	})

	t.Run("malformed address", func(t *testing.T) {
		d, _ := hooked(t, list, res)
		for _, a := range []string{"", "no-port", apiHost, "[::1", ":::"} {
			c, err := d.DialContext(context.Background(), "tcp", a)
			if err == nil {
				closeConn(c)
				t.Fatalf("address %q allowed", a)
			}
			if !IsDenied(err) {
				t.Fatalf("address %q: got %v, want an egress refusal", a, err)
			}
		}
	})

	t.Run("URL check", func(t *testing.T) {
		cases := []struct {
			raw  string
			want Reason // "" means allowed
		}{
			{"https://" + apiHost + "/v1/items?x=1", ""},
			{"https://" + apiHost + ":443/", ""},
			{"HTTPS://API.EXAMPLE.TEST/", ""},
			{"https://multi.example.test:8443/", ""},
			{"https://10.1.2.3:8443/", ""},
			{"https://" + apiHost + ":8443/", ReasonNotAllowlisted},
			{"http://" + apiHost + "/", ReasonNotAllowlisted}, // port 80 is not listed
			{"http://" + apiHost + ":443/", ReasonScheme},
			{"https://" + apiHost + ".evil.test/", ReasonNotAllowlisted},
			{"https://www." + apiHost + "/", ReasonNotAllowlisted},
			{"https://user:pw@" + apiHost + "/", ReasonURL},
			{"https://" + apiHost + "@evil.test/", ReasonURL}, // userinfo
			{"ftp://" + apiHost + "/", ReasonScheme},
			{"//" + apiHost + "/", ReasonScheme},
			{"file:///etc/passwd", ReasonURL},
			{"https:///path", ReasonURL},
			{"mailto:a@" + apiHost, ReasonURL},
		}
		for _, tc := range cases {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.raw, err)
			}
			err = list.CheckURL(u)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("%s: refused: %v", tc.raw, err)
			case tc.want != "" && ReasonOf(err) != tc.want:
				t.Errorf("%s: reason %q (err %v), want %q", tc.raw, ReasonOf(err), err, tc.want)
			}
		}
		if err := list.CheckURL(nil); ReasonOf(err) != ReasonURL {
			t.Errorf("nil URL: %v", err)
		}
		if err := (*Allowlist)(nil).CheckURL(&url.URL{Scheme: "https", Host: apiHost}); ReasonOf(err) != ReasonNoAllowlist {
			t.Errorf("nil allowlist: %v", err)
		}
	})
}

// TestAllowlistConstructionRefusesWildcardsAndAmbiguity: the list itself cannot
// be written with a wildcard, a URL, a port inside the host or an IP spelling a
// resolver might read differently.
func TestAllowlistConstructionRefusesWildcardsAndAmbiguity(t *testing.T) {
	bad := []Destination{
		{Host: "", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "*", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "*.example.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "api.*.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "https://api.example.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "api.example.test:443", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "api.example.test/path", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "user@api.example.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "[::1]", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "fe80::1%eth0", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "api.example.test ", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "api.example.test.", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "-api.example.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "api_x.example.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "bücher.example.test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: strings.Repeat("a", 64) + ".test", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "2130706433", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "0x7f.0.0.1", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "127.1", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "999.1.1.1", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "010.0.0.1", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "app.example.test", Ports: nil, Scheme: SchemeHTTPS},
		{Host: "app.example.test", Ports: []int{0}, Scheme: SchemeHTTPS},
		{Host: "app.example.test", Ports: []int{65536}, Scheme: SchemeHTTPS},
		{Host: "app.example.test", Ports: []int{-1}, Scheme: SchemeHTTPS},
		{Host: "app.example.test", Ports: make([]int, maxPortsPerEntry+1), Scheme: SchemeHTTPS},
		{Host: "app.example.test", Ports: []int{443}, Scheme: ""},
		{Host: "app.example.test", Ports: []int{443}, Scheme: "ftp"},
		{Host: "app.example.test", Ports: []int{443}, Scheme: "HTTPS"},
		{Host: "app.example.test", Ports: []int{80}, Scheme: SchemeHTTP},
		{Host: "10.0.0.5", Ports: []int{80}, Scheme: SchemeHTTP},
	}
	for i, d := range bad {
		if _, err := NewAllowlist(d); !contracts.HasCode(err, contracts.ErrValidationFailed) {
			t.Errorf("destination %d %+v: got %v, want validation_failed", i, d, err)
		}
	}
	if _, err := NewAllowlist(make([]Destination, maxEntries+1)...); err == nil {
		t.Error("an allowlist over the entry limit was accepted")
	}

	good := []Destination{
		{Host: "API.Example.Test", Ports: []int{443, 443, 8443}, Scheme: SchemeHTTPS},
		{Host: "10.0.0.5", Ports: []int{8443}, Scheme: SchemeHTTPS},
		{Host: "2001:db8::5", Ports: []int{443}, Scheme: SchemeHTTPS},
		{Host: "localhost", Ports: []int{8080}, Scheme: SchemeHTTP},
		{Host: "127.0.0.1", Ports: []int{8080}, Scheme: SchemeHTTP},
		{Host: "::1", Ports: []int{8080}, Scheme: SchemeHTTP},
	}
	a, err := NewAllowlist(good...)
	if err != nil {
		t.Fatal(err)
	}
	dests := a.Destinations()
	if dests[0].Host != "api.example.test" || fmt.Sprint(dests[0].Ports) != "[443 8443]" {
		t.Errorf("normalised destination = %+v", dests[0])
	}
	// The list is immutable: neither the input nor the copy aliases it.
	good[0].Ports[0] = 1
	dests[0].Ports[0] = 2
	if !a.Allows("api.example.test", 443) || a.Allows("api.example.test", 1) || a.Allows("api.example.test", 2) {
		t.Error("allowlist changed after the caller mutated its inputs")
	}
	if (*Allowlist)(nil).Len() != 0 || (*Allowlist)(nil).Allows("a", 1) || (*Allowlist)(nil).Destinations() != nil {
		t.Error("a nil allowlist is not empty")
	}
}

// TestEgressBlocksDNSRebindToPrivate: a listed name whose answer contains a
// private, loopback, link-local, metadata or reserved address is refused, in
// every spelling including IPv4-mapped, NAT64 and 6to4 forms, and a name that
// changes its answer between two dials is checked again each time.
//
// SEC-056
func TestEgressBlocksDNSRebindToPrivate(t *testing.T) {
	list := mustList(t, Destination{Host: apiHost, Ports: []int{443}, Scheme: SchemeHTTPS})

	blocked := []string{
		// loopback
		"127.0.0.1", "127.255.255.254", "::1", "::ffff:127.0.0.1", "::127.0.0.1", "::7f00:1",
		"64:ff9b::7f00:1", "2002:7f00:1::",
		// RFC 1918 and carrier-grade NAT
		"10.0.0.1", "10.255.255.255", "172.16.0.1", "172.31.255.255", "192.168.0.1", "192.168.255.255", "100.64.0.1",
		"::ffff:10.0.0.1", "::ffff:192.168.1.1", "64:ff9b::a00:1", "2002:a00:1::", "fc00::1", "fd12:3456::1",
		// link-local and cloud metadata
		"169.254.169.254", "169.254.0.1", "169.254.170.2", "::ffff:169.254.169.254", "::a9fe:a9fe",
		"64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::", "fe80::1", "fe80::a9fe:a9fe", "fd00:ec2::254", "fd20:ce::254",
		"168.63.129.16", "100.100.100.200", "192.0.0.192",
		// unspecified, multicast, broadcast, reserved
		"0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "239.255.255.250", "ff02::1", "255.255.255.255", "240.0.0.1",
		// documentation, benchmarking, Teredo, discard, not yet allocated
		"192.0.2.1", "198.51.100.7", "203.0.113.9", "198.18.0.1", "2001:db8::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2",
		"100::1", "4000::1", "::ffff:0:10.0.0.1",
	}
	for _, ip := range blocked {
		t.Run(ip, func(t *testing.T) {
			res := newResolver(apiHost, []string{ip})
			d, rec := hooked(t, list, res)
			c, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
			if err == nil {
				closeConn(c)
				t.Fatalf("%s: a name that resolves to %s was dialled", apiHost, ip)
			}
			if ReasonOf(err) != ReasonBlockedAddress || contracts.CodeOf(err) != contracts.ErrPermissionDenied {
				t.Fatalf("%s: got %v, want a blocked_address refusal", ip, err)
			}
			if len(rec.calls()) != 0 {
				t.Fatalf("%s: connect step ran: %v", ip, rec.calls())
			}
			if res.total() != 1 {
				t.Fatalf("%s: %d lookups, want 1", ip, res.total())
			}
		})
	}

	t.Run("a mixed answer is refused as a whole", func(t *testing.T) {
		for _, answer := range [][]string{
			{publicIP, "10.0.0.1"},
			{"10.0.0.1", publicIP},
			{publicIP, "169.254.169.254", "93.184.216.35"},
			{publicIP, "::ffff:127.0.0.1"},
		} {
			d, rec := hooked(t, list, newResolver(apiHost, answer))
			c, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
			if err == nil {
				closeConn(c)
				t.Fatalf("answer %v was dialled", answer)
			}
			if ReasonOf(err) != ReasonBlockedAddress {
				t.Fatalf("answer %v: got %v", answer, err)
			}
			if len(rec.calls()) != 0 {
				t.Fatalf("answer %v: the passing address was dialled as a fallback: %v", answer, rec.calls())
			}
		}
	})

	t.Run("public answers are allowed", func(t *testing.T) {
		for _, ip := range []string{publicIP, "8.8.8.8", "1.1.1.1", "2606:2800:220:1:248:1893:25c8:1946", "::ffff:8.8.8.8", "64:ff9b::808:808"} {
			d, rec := hooked(t, list, newResolver(apiHost, []string{ip}))
			c, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
			if err != nil {
				t.Fatalf("%s: got %v, want an allowed dial", ip, err)
			}
			closeConn(c)
			if len(rec.calls()) != 1 {
				t.Fatalf("%s: connect step ran %d times", ip, len(rec.calls()))
			}
		}
	})

	t.Run("rebinding between dials is caught on the second dial", func(t *testing.T) {
		// First answer is public, the next is the metadata service: a TTL-0
		// rebinding attack. Nothing about the first answer is remembered.
		res := newResolver(apiHost, []string{publicIP}, []string{"169.254.169.254"})
		d, rec := hooked(t, list, res)
		c, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
		if err != nil {
			t.Fatalf("first dial: %v", err)
		}
		closeConn(c)
		c, err = d.DialContext(context.Background(), "tcp", apiHost+":443")
		if err == nil {
			closeConn(c)
			t.Fatal("second dial reached the rebound address")
		}
		if ReasonOf(err) != ReasonBlockedAddress {
			t.Fatalf("second dial: %v", err)
		}
		if got := rec.calls(); len(got) != 1 || got[0] != publicIP+":443" {
			t.Fatalf("connect step calls = %v, want only the first, vetted address", got)
		}
	})

	t.Run("a name never resolves into the connect step", func(t *testing.T) {
		// Whatever the resolver would say on a later lookup, the connect step
		// receives the IP literal that was checked.
		res := newResolver(apiHost, []string{publicIP}, []string{"10.0.0.1"})
		d, rec := hooked(t, list, res)
		c, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
		if err != nil {
			t.Fatal(err)
		}
		closeConn(c)
		if got := rec.calls(); len(got) != 1 {
			t.Fatalf("calls = %v", got)
		} else if host, _, _ := net.SplitHostPort(got[0]); host != publicIP {
			t.Fatalf("connect step received %q, want the vetted literal %s", got[0], publicIP)
		}
		if res.total() != 1 {
			t.Fatalf("%d lookups for one dial", res.total())
		}
	})

	t.Run("listing a literal names the operator's intent", func(t *testing.T) {
		cases := []struct {
			host string
			want Reason // "" means allowed
		}{
			{"10.1.2.3", ""},     // private, but named
			{"127.0.0.1", ""},    // loopback, but named
			{"::1", ""},          //
			{"192.168.1.20", ""}, //
			{"169.254.169.254", ReasonBlockedAddress}, // metadata is never reachable
			{"169.254.1.1", ReasonBlockedAddress},     // link-local is never reachable
			{"fe80::1", ReasonBlockedAddress},
			{"fd00:ec2::254", ReasonBlockedAddress},
			{"0.0.0.0", ReasonBlockedAddress},
			{"224.0.0.1", ReasonBlockedAddress},
			{"::ffff:169.254.169.254", ReasonBlockedAddress}, // listed in mapped form
			{"64:ff9b::a9fe:a9fe", ReasonBlockedAddress},
		}
		for _, tc := range cases {
			a, err := NewAllowlist(Destination{Host: tc.host, Ports: []int{8443}, Scheme: SchemeHTTPS})
			if err != nil {
				t.Fatalf("list %s: %v", tc.host, err)
			}
			res := newResolver("unused", []string{publicIP})
			d, rec := hooked(t, a, res)
			ip := netip.MustParseAddr(tc.host).Unmap()
			c, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort(tc.host, "8443"))
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("%s: refused: %v", tc.host, err)
			case tc.want != "" && ReasonOf(err) != tc.want:
				t.Errorf("%s: got %v, want %s", tc.host, err, tc.want)
			}
			if err == nil {
				closeConn(c)
				if got := rec.calls(); len(got) != 1 || got[0] != net.JoinHostPort(ip.String(), "8443") {
					t.Errorf("%s: connect step got %v", tc.host, got)
				}
			}
			if res.total() != 0 {
				t.Errorf("%s: an IP literal was sent to the resolver", tc.host)
			}
		}
	})

	t.Run("localhost is the two loopback addresses and never resolved", func(t *testing.T) {
		a := mustList(t, Destination{Host: "localhost", Ports: []int{8080}, Scheme: SchemeHTTP})
		res := newResolver("localhost", []string{"10.0.0.1"}) // a poisoned hosts file
		d, rec := hooked(t, a, res)
		c, err := d.DialContext(context.Background(), "tcp", "LocalHost:8080")
		if err != nil {
			t.Fatal(err)
		}
		closeConn(c)
		if got := rec.calls(); len(got) != 1 || got[0] != "127.0.0.1:8080" {
			t.Fatalf("connect step got %v, want [127.0.0.1:8080]", got)
		}
		if res.total() != 0 {
			t.Fatal("localhost was sent to the resolver")
		}
		// A different name that resolves to loopback is not localhost.
		b := mustList(t, Destination{Host: apiHost, Ports: []int{443}, Scheme: SchemeHTTPS})
		d2, _ := hooked(t, b, newResolver(apiHost, []string{"127.0.0.1"}))
		if c, err := d2.DialContext(context.Background(), "tcp", apiHost+":443"); err == nil {
			closeConn(c)
			t.Fatal("a name resolving to loopback was dialled")
		}
	})

	t.Run("resolution failures are unavailable, not allowed and not denied", func(t *testing.T) {
		res := newResolver(apiHost)
		res.err = errors.New("server misbehaving")
		d, rec := hooked(t, list, res)
		_, err := d.DialContext(context.Background(), "tcp", apiHost+":443")
		if !contracts.HasCode(err, contracts.ErrUnavailable) || IsDenied(err) {
			t.Fatalf("got %v, want unavailable", err)
		}
		if len(rec.calls()) != 0 {
			t.Fatal("connect step ran after a failed lookup")
		}
		empty := newResolver(apiHost)
		empty.answers[apiHost] = [][]netip.Addr{{}}
		d, _ = hooked(t, list, empty)
		if _, err := d.DialContext(context.Background(), "tcp", apiHost+":443"); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("empty answer: got %v, want unavailable", err)
		}
		many := make([]string, maxResolved+1)
		for i := range many {
			many[i] = fmt.Sprintf("93.184.216.%d", i+1)
		}
		d, _ = hooked(t, list, newResolver(apiHost, many))
		if _, err := d.DialContext(context.Background(), "tcp", apiHost+":443"); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("oversized answer: got %v, want unavailable", err)
		}
	})

	t.Run("context cancellation passes through", func(t *testing.T) {
		d, _ := hooked(t, list, blockingResolver{})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := d.DialContext(ctx, "tcp", apiHost+":443")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want context.DeadlineExceeded", err)
		}
	})
}

type blockingResolver struct{}

func (blockingResolver) LookupNetIP(ctx context.Context, _, _ string) ([]netip.Addr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestClassify pins the address classes the rebinding test relies on.
func TestClassify(t *testing.T) {
	cases := map[string]addrClass{
		"8.8.8.8": classPublic, "93.184.216.34": classPublic, "2606:4700::1111": classPublic,
		"127.0.0.1": classLocal, "::1": classLocal, "10.0.0.1": classLocal, "172.16.5.5": classLocal,
		"172.15.0.1": classPublic, "172.32.0.1": classPublic, "192.168.1.1": classLocal, "100.64.0.1": classLocal,
		"100.128.0.1": classPublic, "fc00::1": classLocal, "fdff::1": classLocal,
		"169.254.169.254": classNever, "fe80::1": classNever, "0.0.0.0": classNever, "::": classNever,
		"224.0.0.1": classNever, "ff02::1": classNever, "255.255.255.255": classNever,
		"::ffff:127.0.0.1": classLocal, "::ffff:169.254.169.254": classNever, "::ffff:8.8.8.8": classPublic,
		"2002:a9fe:a9fe::": classNever, "2002:808:808::": classPublic, "2002:7f00:1::": classLocal,
		"64:ff9b::808:808": classPublic, "64:ff9b::7f00:1": classLocal, "64:ff9b::a9fe:a9fe": classNever,
		"::7f00:1": classLocal, "::808:808": classLocal,
	}
	for s, want := range cases {
		if got := classify(netip.MustParseAddr(s)); got != want {
			t.Errorf("classify(%s) = %d, want %d", s, got, want)
		}
	}
	if classify(netip.Addr{}) != classNever {
		t.Error("an invalid address is not never")
	}
	if classify(netip.MustParseAddr("fe80::1%eth0")) != classNever {
		t.Error("a zoned link-local address is not never")
	}
}

// redirectFixture is a pair of loopback servers: "origin" (allowlisted) and
// "target" (not), with a counter on the target.
type redirectFixture struct {
	origin, target *httptest.Server
	targetHits     atomic.Int64
	seen           chan http.Header
}

func newRedirectFixture(t *testing.T) *redirectFixture {
	t.Helper()
	f := &redirectFixture{seen: make(chan http.Header, 8)}
	f.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.targetHits.Add(1)
		select {
		case f.seen <- r.Header.Clone():
		default:
		}
		_, _ = io.WriteString(w, "target")
	}))
	f.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-target":
			http.Redirect(w, r, f.target.URL+"/landing", http.StatusFound)
		case "/to-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/to-name":
			http.Redirect(w, r, "http://other.example.test/", http.StatusMovedPermanently)
		case "/to-userinfo":
			http.Redirect(w, r, "http://user:pw@"+r.Host+"/ok", http.StatusFound)
		case "/to-ok":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/ok":
			_, _ = io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.origin.Close)
	t.Cleanup(f.target.Close)
	return f
}

func loopbackEntry(t *testing.T, srv *httptest.Server) Destination {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	var port int
	if _, err := fmt.Sscanf(u.Port(), "%d", &port); err != nil {
		t.Fatal(err)
	}
	return Destination{Host: u.Hostname(), Ports: []int{port}, Scheme: SchemeHTTP}
}

// TestEgressBlocksRedirectToUnlisted: a listed server that redirects to an
// unlisted one (another port of the same host, a metadata address, another
// name) is not followed; the unlisted server is never contacted.
//
// SEC-056
func TestEgressBlocksRedirectToUnlisted(t *testing.T) {
	f := newRedirectFixture(t)
	d := NewDialer(mustList(t, loopbackEntry(t, f.origin)))
	client := d.HTTPClient(egressTestWait)

	get := func(path string) (string, error) {
		resp, err := client.Get(f.origin.URL + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}

	// Positive control: a redirect inside the allowlist is followed.
	if body, err := get("/to-ok"); err != nil || body != "ok" {
		t.Fatalf("redirect to a listed path: body %q, err %v", body, err)
	}

	for _, path := range []string{"/to-target", "/to-metadata", "/to-name", "/to-userinfo"} {
		t.Run(path, func(t *testing.T) {
			body, err := get(path)
			if err == nil {
				t.Fatalf("redirect was followed, body %q", body)
			}
			if !IsDenied(err) || ReasonOf(err) != ReasonRedirect {
				t.Fatalf("got %v (reason %q), want a redirect_not_allowed refusal", err, ReasonOf(err))
			}
		})
	}
	if f.targetHits.Load() != 0 {
		t.Fatalf("the unlisted server was contacted %d times", f.targetHits.Load())
	}

	t.Run("a redirect loop stops", func(t *testing.T) {
		_, err := get("/loop")
		if ReasonOf(err) != ReasonRedirect {
			t.Fatalf("got %v, want redirect_not_allowed", err)
		}
	})

	t.Run("a request to the unlisted server is refused before connecting", func(t *testing.T) {
		resp, err := client.Get(f.target.URL + "/direct")
		if err == nil {
			resp.Body.Close()
			t.Fatal("direct request to the unlisted server succeeded")
		}
		if ReasonOf(err) != ReasonNotAllowlisted {
			t.Fatalf("got %v", err)
		}
		if f.targetHits.Load() != 0 {
			t.Fatal("the unlisted server was contacted")
		}
	})

	t.Run("a redirect to another listed origin drops credentials", func(t *testing.T) {
		both := NewDialer(mustList(t, loopbackEntry(t, f.origin), loopbackEntry(t, f.target)))
		req, _ := http.NewRequest(http.MethodGet, f.origin.URL+"/to-target", nil)
		req.Header.Set("Authorization", "Bearer test-only-not-a-secret")
		req.Header.Set("X-Api-Key", "test-only-not-a-secret")
		req.Header.Set("Cookie", "sid=test-only")
		req.Header.Set("Accept", "application/json")
		resp, err := both.HTTPClient(egressTestWait).Do(req)
		if err != nil {
			t.Fatalf("redirect between two listed origins: %v", err)
		}
		resp.Body.Close()
		h := egressWaitSeen(t, f.seen)
		for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Referer"} {
			if h.Get(name) != "" {
				t.Errorf("header %s followed the redirect to another origin", name)
			}
		}
		if h.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q, want it kept", h.Get("Accept"))
		}
	})

	t.Run("same-origin redirects keep their headers", func(t *testing.T) {
		var got atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/end", http.StatusFound)
				return
			}
			got.Store(r.Header.Get("X-Api-Key"))
		}))
		defer srv.Close()
		c := NewDialer(mustList(t, loopbackEntry(t, srv))).HTTPClient(egressTestWait)
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
		req.Header.Set("X-Api-Key", "test-only-not-a-secret")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got.Load() != "test-only-not-a-secret" {
			t.Fatalf("X-Api-Key = %v after a same-origin redirect", got.Load())
		}
	})

	// net/http builds the headers of every hop again from the original request, so a hop is compared with the
	// origin the request was made to, not with the hop before it: A -> B -> B must not hand B the credential on its
	// second hop because B -> B is "same origin", and A -> B -> A gives it back to A only.
	t.Run("a second hop on the other origin still drops credentials", func(t *testing.T) {
		var mu sync.Mutex
		seen := map[string]string{}
		record := func(r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			seen[r.Host+r.URL.Path] = r.Header.Get("X-Api-Key") + "|" + r.Header.Get("Authorization")
		}
		var a, b *httptest.Server
		b = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			record(r)
			switch r.URL.Path {
			case "/hop1":
				http.Redirect(w, r, "/hop2", http.StatusFound)
			case "/back":
				http.Redirect(w, r, a.URL+"/end", http.StatusFound)
			}
		}))
		defer b.Close()
		a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			record(r)
			switch r.URL.Path {
			case "/start":
				http.Redirect(w, r, b.URL+"/hop1", http.StatusFound)
			case "/round":
				http.Redirect(w, r, b.URL+"/back", http.StatusFound)
			}
		}))
		defer a.Close()
		c := NewDialer(mustList(t, loopbackEntry(t, a), loopbackEntry(t, b))).HTTPClient(egressTestWait)
		do := func(path string) {
			t.Helper()
			req, _ := http.NewRequest(http.MethodGet, a.URL+path, nil)
			req.Header.Set("X-Api-Key", "test-only-key")
			req.Header.Set("Authorization", "Bearer test-only-token")
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		}
		do("/start")
		do("/round")
		ha, hb := strings.TrimPrefix(a.URL, "http://"), strings.TrimPrefix(b.URL, "http://")
		mu.Lock()
		defer mu.Unlock()
		for _, k := range []string{hb + "/hop1", hb + "/hop2", hb + "/back"} {
			if v, ok := seen[k]; !ok || v != "|" {
				t.Errorf("%s saw %q (seen %v), want no credential", k, v, ok)
			}
		}
		if v := seen[ha+"/end"]; v != "test-only-key|Bearer test-only-token" {
			t.Errorf("back on the origin, %s saw %q, want the credentials", ha+"/end", v)
		}
	})

	t.Run("an https to http downgrade is refused", func(t *testing.T) {
		both := mustList(t,
			Destination{Host: apiHost, Ports: []int{443}, Scheme: SchemeHTTPS},
			Destination{Host: "127.0.0.1", Ports: []int{8080}, Scheme: SchemeHTTP},
		)
		dd := NewDialer(both)
		from, _ := url.Parse("https://" + apiHost + "/start")
		to, _ := url.Parse("http://127.0.0.1:8080/landing")
		prev, _ := http.NewRequest(http.MethodGet, from.String(), nil)
		next, _ := http.NewRequest(http.MethodGet, to.String(), nil)
		if err := dd.checkRedirect(next, []*http.Request{prev}); ReasonOf(err) != ReasonRedirect {
			t.Fatalf("downgrade: %v", err)
		}
		// The same target is fine from a plain-http origin.
		prevHTTP, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/start", nil)
		if err := dd.checkRedirect(next, []*http.Request{prevHTTP}); err != nil {
			t.Fatalf("http to http: %v", err)
		}
	})

	t.Run("redirect policy refuses a missing history and too many hops", func(t *testing.T) {
		dd := NewDialer(mustList(t, Destination{Host: "127.0.0.1", Ports: []int{8080}, Scheme: SchemeHTTP}))
		next, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/x", nil)
		if err := dd.checkRedirect(next, nil); ReasonOf(err) != ReasonRedirect {
			t.Fatalf("no history: %v", err)
		}
		via := make([]*http.Request, maxRedirects+1)
		for i := range via {
			via[i] = next
		}
		if err := dd.checkRedirect(next, via); ReasonOf(err) != ReasonRedirect {
			t.Fatalf("too many hops: %v", err)
		}
	})
}

// TestEgressConnectsToListedLoopback proves the real dial path (no test hook):
// a listed loopback literal connects, and the re-check on the socket accepts
// exactly the vetted address.
func TestEgressConnectsToListedLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.WriteString(c, "hello")
			_ = c.Close()
		}
	}()
	_, portText, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	_, _ = fmt.Sscanf(portText, "%d", &port)

	d := NewDialer(mustList(t, Destination{Host: "127.0.0.1", Ports: []int{port}, Scheme: SchemeHTTP}))
	c, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := io.ReadAll(c)
	if string(b) != "hello" {
		t.Fatalf("read %q", b)
	}

	// The same socket, offered another port that is not listed, is refused.
	if c2, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port+1))); err == nil {
		closeConn(c2)
		t.Fatal("an unlisted port on a listed host was dialled")
	}
}

// TestRecheckRefusesAnyOtherAddress covers the Control hook directly: it is
// the last line of defence and cannot fire in a correct dial.
func TestRecheckRefusesAnyOtherAddress(t *testing.T) {
	vetted := netip.MustParseAddr(publicIP)
	fn := recheck(vetted)
	if err := fn("tcp4", publicIP+":443", nil); err != nil {
		t.Fatalf("the vetted address was refused: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:80", "not-an-address", "[::ffff:127.0.0.1]:443", ""} {
		if err := fn("tcp", addr, nil); ReasonOf(err) != ReasonBlockedAddress {
			t.Errorf("%q: got %v", addr, err)
		}
	}
	if err := recheck(netip.MustParseAddr("8.8.8.8"))("tcp6", "[::ffff:8.8.8.8]:443", nil); err != nil {
		t.Errorf("mapped form of the vetted address refused: %v", err)
	}
}

func TestRegistryRejectsBadApplicationIDs(t *testing.T) {
	r := NewRegistry()
	l := mustList(t, Destination{Host: apiHost, Ports: []int{443}, Scheme: SchemeHTTPS})
	for _, id := range []string{"", " ", "app one", "app*", "a\nb", strings.Repeat("x", contracts.MaxScopeValueLen+1)} {
		if err := r.Set(id, l); !contracts.HasCode(err, contracts.ErrScopeViolation) {
			t.Errorf("Set(%q) = %v, want scope_violation", id, err)
		}
	}
	if err := r.Set("app_ok", l); err != nil {
		t.Fatal(err)
	}
	if r.Allowlist("app_ok") != l || r.Allowlist("app_other") != nil {
		t.Fatal("registry lookup")
	}
	if err := r.Set("app_ok", nil); err != nil || r.Allowlist("app_ok") != nil {
		t.Fatal("Set(nil) must revoke")
	}
}

func TestRefusalErrorsCarryNoURLOrSecrets(t *testing.T) {
	d := NewDialer(nil)
	_, err := d.DialContext(context.Background(), "tcp", "evil.test\x00\x1b[31m:443")
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		t.Fatalf("not a contracts.Error: %v", err)
	}
	if ce.Retryable {
		t.Error("a refusal is retryable")
	}
	for _, r := range ce.Error() + fmt.Sprint(ce.Details) {
		if r < ' ' || r == 0x7f {
			t.Fatalf("control character in %q", ce.Error())
		}
	}
	if !IsDenied(fmt.Errorf("wrapped: %w", err)) || ReasonOf(fmt.Errorf("wrapped: %w", err)) != ReasonBadAddress {
		t.Error("ReasonOf does not see through wrapping")
	}
	if IsDenied(nil) || IsDenied(errors.New("plain")) || IsDenied(contracts.New(contracts.ErrPermissionDenied, "host said no")) {
		t.Error("IsDenied claims errors that are not egress refusals")
	}
}

// egressTestWait bounds every wait on a loopback server in these tests. A loopback request takes
// milliseconds; the bound is not a pace but a ceiling far above what a machine under a parallel
// build adds (a 5 s client timeout was seen to trip once, M0-12), and a hang still ends.
const egressTestWait = 30 * time.Second

// egressWaitSeen receives the headers the target server saw, bounded: an unbounded receive would hang
// the whole package run if the request never arrived.
func egressWaitSeen(t *testing.T, seen <-chan http.Header) http.Header {
	t.Helper()
	select {
	case h := <-seen:
		return h
	case <-time.After(egressTestWait):
		t.Fatalf("the target server saw no request within %s", egressTestWait)
		return nil
	}
}
