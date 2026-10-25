package egress

import (
	"context"
	"net/netip"
	"testing"
)

// PT-001. A destination of the platform is refused even when the allowlist names the very host
// and port; every mutation that weakens the check (match the port only on the literal, forget the
// other loopback spellings, skip the resolved addresses, ignore later additions) fails one case.
func TestPlatformDenyRefusesListedDestinations(t *testing.T) {
	type tc struct {
		name   string
		deny   []string // host:port entries
		listed Destination
		dial   string
		want   bool // refused as a platform port
	}
	loop := func(host string, port int) Destination {
		return Destination{Host: host, Ports: []int{port}, Scheme: SchemeHTTP}
	}
	cases := []tc{
		{"exact literal", []string{"127.0.0.1:8080"}, loop("127.0.0.1", 8080), "127.0.0.1:8080", true},
		{"another port of the same host is not denied", []string{"127.0.0.1:8080"}, loop("127.0.0.1", 8787), "127.0.0.1:8787", false},
		{"the IPv6 loopback spelling of a v4 listener", []string{"127.0.0.1:8080"}, loop("::1", 8080), "[::1]:8080", true},
		{"localhost reaches a loopback listener", []string{"127.0.0.1:8080"}, loop("localhost", 8080), "localhost:8080", true},
		{"another loopback address", []string{"127.0.0.1:8080"}, loop("127.0.0.2", 8080), "127.0.0.2:8080", true},
		{"a listener on every interface", []string{"0.0.0.0:7470"}, loop("127.0.0.1", 7470), "127.0.0.1:7470", true},
		{"a listener on every interface, v6 form", []string{"[::]:7470"}, loop("127.0.0.1", 7470), "127.0.0.1:7470", true},
		{"a bare port means every interface", []string{":7470"}, loop("127.0.0.1", 7470), "127.0.0.1:7470", true},
		{"every interface does not cover other ports", []string{"0.0.0.0:7470"}, loop("127.0.0.1", 7471), "127.0.0.1:7471", false},
		{"a localhost entry denies the port on loopback literals", []string{"localhost:9464"}, loop("127.0.0.1", 9464), "127.0.0.1:9464", true},
		{"a name entry denies that name", []string{"localai:8080"}, Destination{Host: "localai", Ports: []int{8080}, Scheme: SchemeHTTPS}, "localai:8080", true},
		{"a name entry does not deny another name", []string{"localai:8080"}, Destination{Host: "other.example", Ports: []int{8080}, Scheme: SchemeHTTPS}, "other.example:8080", false},
		{"a private literal is denied when named", []string{"10.1.2.3:8080"}, Destination{Host: "10.1.2.3", Ports: []int{8080}, Scheme: SchemeHTTPS}, "10.1.2.3:8080", true},
		{"a private literal on another port is not", []string{"10.1.2.3:8080"}, Destination{Host: "10.1.2.3", Ports: []int{8081}, Scheme: SchemeHTTPS}, "10.1.2.3:8081", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pd := NewPlatformDeny()
			for _, e := range c.deny {
				if err := pd.Add(e); err != nil {
					t.Fatalf("Add %q: %v", e, err)
				}
			}
			res := newResolver("localai", []string{"203.0.113.9"})
			res.set("other.example", []string{"198.51.100.7"})
			d, rec := hooked(t, mustList(t, c.listed), res, WithPlatformDeny(pd))
			conn, err := d.DialContext(context.Background(), "tcp", c.dial)
			closeConn(conn)
			got := ReasonOf(err) == ReasonPlatformPort
			if got != c.want {
				t.Fatalf("dial %s: reason %q (err %v), refused as platform port = %v, want %v", c.dial, ReasonOf(err), err, got, c.want)
			}
			if c.want && len(rec.calls()) != 0 {
				t.Fatalf("a socket was opened to %v before the platform check", rec.calls())
			}
		})
	}
}

func TestPlatformDenyNameIsCheckedBeforeAnyLookup(t *testing.T) {
	pd := NewPlatformDeny()
	if err := pd.Add("localai:8080"); err != nil {
		t.Fatal(err)
	}
	res := newResolver("localai", []string{"203.0.113.9"})
	d, _ := hooked(t, mustList(t, Destination{Host: "localai", Ports: []int{8080}, Scheme: SchemeHTTPS}), res, WithPlatformDeny(pd))
	if _, err := d.DialContext(context.Background(), "tcp", "localai:8080"); ReasonOf(err) != ReasonPlatformPort {
		t.Fatalf("reason %q, want platform_port", ReasonOf(err))
	}
	if res.total() != 0 {
		t.Fatalf("the denied name was resolved %d times: a lookup is itself outbound traffic", res.total())
	}
}

func TestPlatformDenyDenialReachesTheHook(t *testing.T) {
	pd := NewPlatformDeny()
	_ = pd.Add("127.0.0.1:8080")
	var got []Denial
	d, _ := hooked(t, mustList(t, Destination{Host: "127.0.0.1", Ports: []int{8080}, Scheme: SchemeHTTP}), newResolver("x"),
		WithPlatformDeny(pd), WithDenyHook(func(x Denial) { got = append(got, x) }))
	_, _ = d.DialContext(context.Background(), "tcp", "127.0.0.1:8080")
	if len(got) != 1 || got[0].Reason != ReasonPlatformPort || got[0].Port != 8080 {
		t.Fatalf("hook saw %+v, want one platform_port denial", got)
	}
}

// Without a deny-list, or with an empty one, a listed loopback destination is reachable: the
// check is what refuses it, not the allowlist.
func TestPlatformDenyEmptyListChangesNothing(t *testing.T) {
	for name, opts := range map[string][]Option{"none": nil, "empty": {WithPlatformDeny(NewPlatformDeny())}, "nil": {WithPlatformDeny(nil)}} {
		t.Run(name, func(t *testing.T) {
			d, rec := hooked(t, mustList(t, Destination{Host: "127.0.0.1", Ports: []int{8080}, Scheme: SchemeHTTP}), newResolver("x"), opts...)
			conn, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:8080")
			closeConn(conn)
			if err != nil || len(rec.calls()) != 1 {
				t.Fatalf("err %v, dials %v", err, rec.calls())
			}
		})
	}
}

// An address that only becomes known later (an ephemeral listener, a reload) is denied from then
// on by the Dialers already handed out, and the Registry always carries a list to fill.
func TestPlatformDenyRegistryAndLaterAdditions(t *testing.T) {
	reg := NewRegistry(WithResolver(newResolver("x")))
	pd := reg.PlatformDeny()
	if pd == nil {
		t.Fatal("a registry carries no platform deny-list")
	}
	if err := reg.Set("app", mustList(t, Destination{Host: "127.0.0.1", Ports: []int{41999}, Scheme: SchemeHTTP})); err != nil {
		t.Fatal(err)
	}
	d := reg.Dialer("app")
	rec := &dialRecorder{}
	d.dial = rec.dial
	conn, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:41999")
	closeConn(conn)
	if err != nil {
		t.Fatalf("before the platform listener exists the dial is allowed: %v", err)
	}
	pd.AddSource(func() []string { return []string{"http://127.0.0.1:41999"} })
	if _, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:41999"); ReasonOf(err) != ReasonPlatformPort {
		t.Fatalf("after the addition: reason %q, want platform_port", ReasonOf(err))
	}
	if pd.Len() != 1 {
		t.Fatalf("Len %d, want 1", pd.Len())
	}
	reg2 := NewRegistry(WithPlatformDeny(pd))
	if reg2.PlatformDeny() != pd {
		t.Fatal("a registry built with a deny-list does not carry that one")
	}
}

// A dial reads the deny-list once: the name check and the address check share one snapshot, so a
// source is called once per dial, not once per check.
func TestPlatformDenySourceIsReadOncePerDial(t *testing.T) {
	pd := NewPlatformDeny()
	calls := 0
	pd.AddSource(func() []string { calls++; return []string{"127.0.0.1:9"} })
	d, _ := hooked(t, mustList(t, Destination{Host: "127.0.0.1", Ports: []int{8080}, Scheme: SchemeHTTP}), newResolver("x"), WithPlatformDeny(pd))
	conn, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:8080")
	closeConn(conn)
	if err != nil {
		t.Fatalf("a destination the list does not name was refused: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the deny-list source was called %d times for one dial, want 1", calls)
	}
}

func TestPlatformDenyEntryParsing(t *testing.T) {
	pd := NewPlatformDeny()
	for _, bad := range []string{"", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:x"} {
		if err := pd.Add(bad); err == nil {
			t.Errorf("Add(%q) accepted", bad)
		}
	}
	for _, bad := range []string{"", "http://", "not a url", "http://:80"} {
		if err := pd.AddURL(bad); err == nil {
			t.Errorf("AddURL(%q) accepted", bad)
		}
	}
	if pd.Len() != 0 {
		t.Fatalf("a refused entry was kept: %d", pd.Len())
	}
	if err := pd.AddURL("http://localai.internal"); err != nil {
		t.Fatal(err)
	}
	if !pd.denies("localai.internal", 80, nil) || pd.denies("localai.internal", 443, nil) {
		t.Error("a URL without a port must mean the scheme's default port, and only it")
	}
	// a source that returns garbage never loosens the list
	pd.AddSource(func() []string { return []string{"garbage", "127.0.0.1:9", "http://127.0.0.1:10"} })
	if !pd.denies("127.0.0.1", 9, []netip.Addr{netip.MustParseAddr("127.0.0.1")}) || !pd.denies("127.0.0.1", 10, nil) {
		t.Error("the valid entries of a source with a bad one are not denied")
	}
}
