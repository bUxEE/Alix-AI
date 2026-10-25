package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
)

// scriptedResolver answers LookupNetIP from a table, one answer per call: the
// n-th lookup of a host returns its n-th answer and the last answer repeats.
// It counts lookups so tests can prove that an unlisted name was never
// resolved.
type scriptedResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	err     error
	calls   map[string]int
}

func newResolver(host string, answers ...[]string) *scriptedResolver {
	r := &scriptedResolver{answers: map[string][][]netip.Addr{}, calls: map[string]int{}}
	r.set(host, answers...)
	return r
}

func (r *scriptedResolver) set(host string, answers ...[]string) {
	for _, a := range answers {
		var addrs []netip.Addr
		for _, s := range a {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		r.answers[host] = append(r.answers[host], addrs)
	}
}

func (r *scriptedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.calls[host]
	r.calls[host]++
	if r.err != nil {
		return nil, r.err
	}
	seq := r.answers[host]
	if len(seq) == 0 {
		return nil, errors.New("no such host")
	}
	if n >= len(seq) {
		n = len(seq) - 1
	}
	return append([]netip.Addr(nil), seq[n]...), nil
}

func (r *scriptedResolver) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := 0
	for _, n := range r.calls {
		t += n
	}
	return t
}

// dialRecorder replaces the connect step: it records the address it was asked
// to connect to and returns one end of an in-memory pipe, so no socket opens.
type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (r *dialRecorder) dial(_ context.Context, _, address string) (net.Conn, error) {
	r.mu.Lock()
	r.addrs = append(r.addrs, address)
	r.mu.Unlock()
	c, other := net.Pipe()
	_ = other.Close()
	return c, nil
}

func (r *dialRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.addrs...)
}

// hooked builds a Dialer over the allowlist with a scripted resolver and a
// recording connect step.
func hooked(t testing.TB, a *Allowlist, res Resolver, opts ...Option) (*Dialer, *dialRecorder) {
	t.Helper()
	rec := &dialRecorder{}
	d := NewDialer(a, append([]Option{WithResolver(res)}, opts...)...)
	d.dial = rec.dial
	return d, rec
}

func mustList(t testing.TB, dests ...Destination) *Allowlist {
	t.Helper()
	a, err := NewAllowlist(dests...)
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	return a
}

func closeConn(c net.Conn) {
	if c != nil {
		_ = c.Close()
	}
}
