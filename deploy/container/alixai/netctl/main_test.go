package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoopbackURL(t *testing.T) {
	cases := []struct {
		raw string
		ok  bool
	}{
		{"http://127.0.0.1:7470/healthz", true},
		{"http://localhost:8080/readyz", true},
		{"http://LOCALHOST:8080/readyz", true},
		{"http://[::1]:7470/healthz", true},
		{"http://127.1.2.3:1/x", true},
		{"https://127.0.0.1:7470/healthz", false},
		{"http://example.com/healthz", false},
		{"http://10.0.0.5:8080/", false},
		{"http://0.0.0.0:8080/", false},
		{"http://localhost.evil.test/", false},
		{"http://user:pw@127.0.0.1:8080/", false},
		{"//127.0.0.1:8080/", false},
		{"127.0.0.1:8080", false},
		{"", false},
	}
	for _, tc := range cases {
		_, err := loopbackURL(tc.raw)
		if (err == nil) != tc.ok {
			t.Errorf("loopbackURL(%q): err = %v, want ok = %v", tc.raw, err, tc.ok)
		}
	}
}

func TestParsePair(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want pair
	}{
		{"127.0.0.1:8080=localai:8080", true, pair{"127.0.0.1:8080", "localai:8080"}},
		{"0.0.0.0:7471=127.0.0.1:7470", true, pair{"0.0.0.0:7471", "127.0.0.1:7470"}},
		{"[::1]:1=host.internal:2", true, pair{"[::1]:1", "host.internal:2"}},
		{"localhost:8080=localai:8080", false, pair{}}, // listen host must be an IP literal
		{"127.0.0.1:8080", false, pair{}},
		{"127.0.0.1:8080=", false, pair{}},
		{"=localai:8080", false, pair{}},
		{"127.0.0.1:0=localai:8080", false, pair{}},
		{"127.0.0.1:70000=localai:8080", false, pair{}},
		{"127.0.0.1:8080=localai", false, pair{}},
		{"127.0.0.1:8080=:8080", false, pair{}},
		{"127.0.0.1:8080=localai:http", false, pair{}},
		{"127.0.0.1:8080=user@localai:8080", false, pair{}},
		{"127.0.0.1:8080=127.0.0.1:8080", false, pair{}}, // forwards to itself
	}
	for _, tc := range cases {
		got, err := parsePair(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("parsePair(%q): err = %v, want ok = %v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("parsePair(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseFlags(t *testing.T) {
	var d time.Duration
	var n int
	known := map[string]flagSetter{"timeout": durationFlag(&d), "max-conns": intFlag(&n)}
	rest, err := parseFlags([]string{"-timeout", "2s", "--max-conns=7", "a=b", "-x"}, known)
	if err != nil || d != 2*time.Second || n != 7 || len(rest) != 2 || rest[0] != "a=b" {
		t.Fatalf("rest = %v, d = %v, n = %d, err = %v", rest, d, n, err)
	}
	for _, bad := range [][]string{{"-nope", "1"}, {"-timeout"}, {"-timeout", "soon"}, {"-max-conns", "many"}} {
		if _, err := parseFlags(bad, known); err == nil {
			t.Errorf("parseFlags(%v) was accepted", bad)
		}
	}
}

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestProbeStatusMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusNoContent)
		case "/down":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/redirect":
			http.Redirect(w, r, "http://example.com/", http.StatusFound)
		case "/slow":
			time.Sleep(2 * time.Second)
		}
	}))
	defer srv.Close()

	for _, tc := range []struct {
		path string
		want int
	}{{"/ok", 0}, {"/down", 1}, {"/redirect", 1}, {"/unnamed-path-answers-200", 0}} {
		code, _, errs := runArgs(t, "probe", srv.URL+tc.path)
		if code != tc.want {
			t.Errorf("probe %s: exit %d (%s), want %d", tc.path, code, errs, tc.want)
		}
	}
	if code, _, _ := runArgs(t, "probe", "-timeout", "100ms", srv.URL+"/slow"); code != 1 {
		t.Errorf("a slow answer: exit %d, want 1", code)
	}
}

func TestProbeRefusesWhatIsNotLoopback(t *testing.T) {
	for _, args := range [][]string{
		{"probe", "http://example.com/healthz"},
		{"probe", "https://127.0.0.1/healthz"},
		{"probe"},
		{"probe", "http://127.0.0.1:1/a", "http://127.0.0.1:1/b"},
		{"probe", "-timeout", "2h", "http://127.0.0.1:1/a"},
		{"forward"},
		{"serve"},
		{},
	} {
		if code, _, _ := runArgs(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (refused command line)", args, code)
		}
	}
	// Nothing listens on port 1: a refused connection is a failed probe, exit 1.
	if code, _, _ := runArgs(t, "probe", "-timeout", "500ms", "http://127.0.0.1:1/healthz"); code != 1 {
		t.Errorf("a closed port: exit %d, want 1", code)
	}
}

// echoServer answers every connection with what it receives, upper-cased.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				_, _ = c.Write(bytes.ToUpper(b))
			}()
		}
	}()
	return ln
}

func startForward(t *testing.T, target string, maxConns int) (addr string, stop func()) {
	t.Helper()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveForward(ctx, []net.Listener{front}, []pair{{front.Addr().String(), target}}, maxConns, time.Second, io.Discard)
	}()
	return front.Addr().String(), func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("forward did not stop")
		}
	}
}

func TestForwardCarriesBytesBothWays(t *testing.T) {
	up := echoServer(t)
	addr, stop := startForward(t, up.Addr().String(), 4)
	defer stop()

	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hello through the pipe")); err != nil {
		t.Fatal(err)
	}
	// Half-close: the echo server reads to EOF, so the end of the request must travel.
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "HELLO THROUGH THE PIPE" {
		t.Fatalf("got %q, err = %v", got, err)
	}
}

func TestForwardClosesClientWhenTargetIsDown(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := dead.Addr().String()
	_ = dead.Close() // nothing listens there any more
	addr, stop := startForward(t, target, 4)
	defer stop()

	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("read err = %v, want the connection closed by the forwarder", err)
	}
}

func TestForwardConnectionCap(t *testing.T) {
	// A target that never answers keeps the slot busy.
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := hold.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	addr, stop := startForward(t, hold.Addr().String(), 1)
	defer stop()

	first, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("the first connection never reached the target")
	}
	second, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("second read err = %v, want the over-cap connection closed at once", err)
	}
}

func TestForwardRefusesBadCommandLines(t *testing.T) {
	for _, args := range [][]string{
		{"forward", "localhost:8080=localai:8080"},
		{"forward", "127.0.0.1:8080=localai:8080", "127.0.0.1:8080=other:1"},
		{"forward", "-max-conns", "0", "127.0.0.1:8080=localai:8080"},
		{"forward", "-max-conns", "99999", "127.0.0.1:8080=localai:8080"},
		{"forward", "-dial-timeout", "0s", "127.0.0.1:8080=localai:8080"},
		{"forward", "-listen", "127.0.0.1:8080=localai:8080"},
	} {
		if code, _, errs := runArgs(t, args...); code != 2 {
			t.Errorf("%v: exit %d (%s), want 2", args, code, errs)
		}
	}
}
