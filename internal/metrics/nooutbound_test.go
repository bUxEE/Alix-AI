package metrics

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// errOutboundTrapped is what the trap answers to every destination it does not
// allow.
var errOutboundTrapped = errors.New("outbound connection refused by the no-telemetry trap")

// outboundTrap stands in for the process's dialer and resolver. It records every
// destination something tries to reach and refuses all but the allowed ones.
type outboundTrap struct {
	mu      sync.Mutex
	allowed map[string]bool
	seen    []string
}

func (g *outboundTrap) record(addr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, addr)
	return g.allowed[addr]
}

func (g *outboundTrap) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if !g.record(addr) {
		return nil, errOutboundTrapped
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// resolverDial is the Dial of the replacement resolver: a lookup that needs a
// name server reaches it, so a DNS query (the first thing a phone-home does) is
// recorded and refused.
func (g *outboundTrap) resolverDial(_ context.Context, _, addr string) (net.Conn, error) {
	g.record("dns:" + addr)
	return nil, errOutboundTrapped
}

// refused lists the destinations that were not allowed.
func (g *outboundTrap) refused() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, a := range g.seen {
		if !g.allowed[a] {
			out = append(out, a)
		}
	}
	return out
}

func (g *outboundTrap) reset() {
	g.mu.Lock()
	g.seen = nil
	g.mu.Unlock()
}

// installOutboundTrap replaces http.DefaultTransport and net.DefaultResolver
// for the test, so any code in the process that reaches for the default client
// or the default resolver goes through the trap. Not for parallel tests.
func installOutboundTrap(t *testing.T, allowed ...string) *outboundTrap {
	t.Helper()
	g := &outboundTrap{allowed: map[string]bool{}}
	for _, a := range allowed {
		g.allowed[a] = true
	}
	oldTransport, oldResolver := http.DefaultTransport, net.DefaultResolver
	http.DefaultTransport = &http.Transport{DialContext: g.dial}
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: g.resolverDial}
	t.Cleanup(func() {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		http.DefaultTransport, net.DefaultResolver = oldTransport, oldResolver
	})
	return g
}

// SEC-071: Rule 3 (no mandatory telemetry): the metrics server and the collectors make
// no outbound connection. The test runs them under a dialer and a resolver that
// refuse every destination except the listener's own loopback address (what the
// operator's scraper would connect to), and then reads the source of every
// package that produces or serves operational data to prove that no code in them
// can dial, resolve, push or run a program. With external mode off there is no
// provider host to allow; with it on, the allowed set would be the provider
// hosts of internal/egress and still never a metrics or diagnostics destination.
func TestNoOutboundTelemetry(t *testing.T) {
	t.Run("the server and every collector make no outbound connection", func(t *testing.T) {
		ln, err := Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		trap := installOutboundTrap(t, ln.Addr().String())

		// Positive controls: the trap does catch a dial and a DNS lookup, so a
		// clean result below means something.
		if _, err := http.Get("http://192.0.2.1:9/"); err == nil {
			t.Fatal("the trap let a connection to 192.0.2.1 through")
		}
		if _, err := net.DefaultResolver.LookupHost(context.Background(), "telemetry.example.invalid"); err == nil {
			t.Fatal("the trap let a DNS lookup through")
		}
		if got := trap.refused(); len(got) < 2 || got[0] != "192.0.2.1:9" || !strings.HasPrefix(got[1], "dns:") {
			t.Fatalf("the trap recorded %v, want the deliberate dial and the lookup", got)
		}
		trap.reset()

		reg := NewRegistry()
		for _, c := range []Collector{
			RuntimeCollector(nil),
			CollectorFunc("queue", func(context.Context) ([]Family, error) {
				return []Family{{Name: "alixai_queue_jobs", Type: Gauge, Samples: []Sample{{Labels: []Label{{"class", "turn"}, {"state", "pending"}}, Value: 3}}}}, nil
			}),
			CollectorFunc("failing", func(context.Context) ([]Family, error) { return nil, errors.New("down") }),
		} {
			if err := reg.Register(c); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, ln, Handler(reg)) }()

		for i := 0; i < 3; i++ {
			resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
			if err != nil {
				t.Fatalf("scrape %d: %v", i, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "alixai_queue_jobs") {
				t.Fatalf("scrape %d: %d %s", i, resp.StatusCode, body)
			}
		}
		// Give anything on a timer a chance to fire before judging.
		time.Sleep(150 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve ended with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not stop")
		}
		if got := trap.refused(); len(got) != 0 {
			t.Errorf("the metrics stack tried to reach %v: nothing may leave the machine", got)
		}
	})

	t.Run("no code in the metrics, diagnostics and ops route can dial out", func(t *testing.T) {
		files := map[string][]string{}
		for dir, pattern := range map[string]string{"metrics": "*.go", "diag": "../diag/*.go", "ops route": "../gateway/routes_ops.go"} {
			ms, err := filepath.Glob(pattern)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if !strings.HasSuffix(m, "_test.go") {
					files[dir] = append(files[dir], m)
				}
			}
			if len(files[dir]) == 0 {
				t.Fatalf("no source files found for %s (%s): the scan is not seeing the code", dir, pattern)
			}
		}
		for dir, list := range files {
			sort.Strings(list)
			for _, f := range list {
				for _, v := range outboundViolations(t, f) {
					t.Errorf("%s: %s", dir, v)
				}
			}
		}
	})

	t.Run("the scan flags what it is meant to flag", func(t *testing.T) {
		src := "package x\nimport (\n\t\"net\"\n\t\"net/http\"\n\t\"os/exec\"\n\tweb \"net/http\"\n)\nfunc f() { http.Get(\"u\"); net.Dial(\"tcp\", \"a\"); web.Post(\"u\", \"\", nil); _ = &http.Client{}; exec.Command(\"x\") }\n"
		af, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		// The os/exec import, http.Get, net.Dial, web.Post (an aliased import) and http.Client.
		if got := scanOutbound(af); len(got) != 5 {
			t.Errorf("scan found %v, want 5 findings", got)
		}
	})
}

// forbiddenImports are packages whose presence means a process, a program or a
// mail or RPC channel; none belongs in an exporter of local data.
var forbiddenImports = map[string]bool{
	"os/exec": true, "net/smtp": true, "net/rpc": true, "plugin": true, "net/http/httputil": true,
}

// forbiddenCalls are the client-side entry points of the standard library: the
// ways a package makes an outbound connection or a lookup. Serving (http.Server,
// http.Error, net.Listen) is not on the list.
var forbiddenCalls = map[string]map[string]bool{
	"net/http": setOf("Get", "Head", "Post", "PostForm", "NewRequest", "NewRequestWithContext", "Client", "DefaultClient", "DefaultTransport", "Transport", "ProxyFromEnvironment", "ProxyURL"),
	"net": setOf("Dial", "DialTimeout", "DialTCP", "DialUDP", "DialIP", "DialUnix", "Dialer", "LookupHost", "LookupIP", "LookupAddr",
		"LookupCNAME", "LookupMX", "LookupNS", "LookupSRV", "LookupTXT", "LookupPort", "DefaultResolver", "Resolver", "ResolveTCPAddr", "ResolveUDPAddr", "ResolveIPAddr"),
	"crypto/tls": setOf("Dial", "DialWithDialer", "Client"),
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func outboundViolations(t *testing.T, file string) []string {
	t.Helper()
	af, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range scanOutbound(af) {
		out = append(out, file+": "+v)
	}
	return out
}

// scanOutbound returns the forbidden imports and client calls of one file,
// resolving each import's local name so an alias does not hide a call.
func scanOutbound(af *ast.File) []string {
	var out []string
	local := map[string]string{} // local name -> import path
	for _, imp := range af.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if forbiddenImports[p] {
			out = append(out, "imports "+p)
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		local[name] = p
	}
	ast.Inspect(af, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if p, ok := local[id.Name]; ok && forbiddenCalls[p][sel.Sel.Name] {
			out = append(out, "uses "+p+"."+sel.Sel.Name)
		}
		return true
	})
	return out
}
