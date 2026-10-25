// Command alixai-netctl is the one helper next to the alixai binary in the
// container images. The images are distroless: there is no shell, no curl and
// no socat, so what a container stack needs besides the service itself is
// built here, small enough to read in one sitting.
//
//	alixai-netctl probe [-timeout 4s] URL
//	alixai-netctl forward [-max-conns 256] [-dial-timeout 5s] LISTEN=TARGET...
//
// probe is the container health check: one GET to a loopback URL, exit 0 for a
// 2xx answer and 1 for anything else. A URL whose host is not a loopback
// literal or "localhost", and any scheme but http, is refused: the check never
// leaves the container it runs in.
//
// forward is a TCP relay, a byte pipe with no protocol knowledge, no logging of
// payload and no way to choose a destination at run time: every LISTEN=TARGET
// pair comes from the command line of the unit that starts it. LISTEN must be
// an IP literal and a port (0.0.0.0:7471 listens on every interface of the
// container, which is the network namespace its caller built), TARGET a host
// name or IP and a port. It exists for two jobs in deploy/container/stack:
// carry the loopback address AlixAI dials for LocalAI to the private LocalAI
// container, and carry the published loopback port of the host to the
// loopback listener of AlixAI, so that neither of them needs a network that
// reaches the internet.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	usage = "usage: alixai-netctl probe [-timeout D] URL | alixai-netctl forward [-max-conns N] [-dial-timeout D] LISTEN=TARGET..."

	defaultProbeTimeout = 4 * time.Second
	defaultMaxConns     = 256
	defaultDialTimeout  = 5 * time.Second
	maxMaxConns         = 4096
	probeBodyLimit      = 64 << 10
	drainTimeout        = 30 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it returns the exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "probe":
		err = probeCmd(ctx, args[1:], stdout)
	case "forward":
		err = forwardCmd(ctx, args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "alixai-netctl: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "alixai-netctl: %v\n", err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// usageError marks a refused command line, as opposed to a failed probe.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// ---- probe ----

func probeCmd(ctx context.Context, args []string, stdout io.Writer) error {
	timeout := defaultProbeTimeout
	rest, err := parseFlags(args, map[string]flagSetter{
		"timeout": durationFlag(&timeout),
	})
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("probe needs exactly one URL")
	}
	target, err := loopbackURL(rest[0])
	if err != nil {
		return err
	}
	if timeout <= 0 || timeout > time.Minute {
		return usagef("-timeout must be between 1ns and 1m")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return usagef("bad URL: %v", err)
	}
	client := &http.Client{
		// A redirect would leave the loopback address the check is bound to.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("probe failed: %w", urlErrorCause(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe failed: status %d", resp.StatusCode)
	}
	_, err = fmt.Fprintf(stdout, "ok %d\n", resp.StatusCode)
	return err
}

// urlErrorCause drops the URL from a client error: the caller knows it already.
func urlErrorCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// loopbackURL accepts http://127.0.0.1:P/path, http://[::1]:P/path and
// http://localhost:P/path, nothing else.
func loopbackURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil {
		return "", usagef("the probe URL must be http://HOST:PORT/PATH with a loopback HOST")
	}
	host := u.Hostname()
	if !strings.EqualFold(host, "localhost") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.Unmap().IsLoopback() {
			return "", usagef("the probe URL host must be localhost or a loopback address")
		}
	}
	return u.String(), nil
}

// ---- forward ----

// pair is one LISTEN=TARGET of the command line.
type pair struct {
	listen string
	target string
}

func parsePair(s string) (pair, error) {
	l, t, ok := strings.Cut(s, "=")
	if !ok || l == "" || t == "" {
		return pair{}, usagef("%q is not LISTEN=TARGET", s)
	}
	lh, lp, err := net.SplitHostPort(l)
	if err != nil {
		return pair{}, usagef("listen address %q is not IP:PORT", l)
	}
	if _, err := netip.ParseAddr(lh); err != nil {
		return pair{}, usagef("listen host %q is not an IP literal", lh)
	}
	if !validPort(lp) {
		return pair{}, usagef("listen port %q is not 1-65535", lp)
	}
	th, tp, err := net.SplitHostPort(t)
	if err != nil || th == "" || strings.ContainsAny(th, " \t/\\@") || !validPort(tp) {
		return pair{}, usagef("target %q is not HOST:PORT", t)
	}
	if l == t {
		return pair{}, usagef("%q would forward to itself", s)
	}
	return pair{listen: l, target: t}, nil
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

func forwardCmd(ctx context.Context, args []string, stderr io.Writer) error {
	maxConns := defaultMaxConns
	dialTimeout := defaultDialTimeout
	rest, err := parseFlags(args, map[string]flagSetter{
		"max-conns":    intFlag(&maxConns),
		"dial-timeout": durationFlag(&dialTimeout),
	})
	if err != nil {
		return err
	}
	if maxConns < 1 || maxConns > maxMaxConns {
		return usagef("-max-conns must be between 1 and %d", maxMaxConns)
	}
	if dialTimeout <= 0 || dialTimeout > time.Minute {
		return usagef("-dial-timeout must be between 1ns and 1m")
	}
	if len(rest) == 0 {
		return usagef("forward needs at least one LISTEN=TARGET")
	}
	pairs := make([]pair, 0, len(rest))
	seen := map[string]bool{}
	for _, s := range rest {
		p, err := parsePair(s)
		if err != nil {
			return err
		}
		if seen[p.listen] {
			return usagef("%s is listened on twice", p.listen)
		}
		seen[p.listen] = true
		pairs = append(pairs, p)
	}

	listeners := make([]net.Listener, 0, len(pairs))
	for _, p := range pairs {
		ln, err := net.Listen("tcp", p.listen)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen %s: %w", p.listen, err)
		}
		listeners = append(listeners, ln)
	}
	serveForward(ctx, listeners, pairs, maxConns, dialTimeout, stderr)
	return nil
}

// serveForward relays every accepted connection of listeners[i] to
// pairs[i].target until ctx ends. Each listener has its own connection cap: a
// full listener closes new connections at once instead of queueing them, so a
// flood on one path cannot starve the other.
func serveForward(ctx context.Context, listeners []net.Listener, pairs []pair, maxConns int, dialTimeout time.Duration, stderr io.Writer) {
	go func() {
		<-ctx.Done()
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	var wg sync.WaitGroup
	for i := range listeners {
		wg.Add(1)
		go func(ln net.Listener, target string) {
			defer wg.Done()
			slots := make(chan struct{}, maxConns)
			var conns sync.WaitGroup
			defer conns.Wait()
			for {
				c, err := ln.Accept()
				if err != nil {
					if ctx.Err() == nil {
						fmt.Fprintf(stderr, "alixai-netctl: accept on %s: %v\n", ln.Addr(), err)
					}
					return
				}
				select {
				case slots <- struct{}{}:
				default:
					_ = c.Close()
					continue
				}
				conns.Add(1)
				go func() {
					defer conns.Done()
					defer func() { <-slots }()
					relay(ctx, c, target, dialTimeout)
				}()
			}
		}(listeners[i], pairs[i].target)
	}
	wg.Wait()
}

// relay pipes one accepted connection to target and back. A target that cannot
// be dialled closes the client without a word: the caller sees a reset, which
// is what the unit behind the forwarder being down looks like.
func relay(ctx context.Context, client net.Conn, target string, dialTimeout time.Duration) {
	defer client.Close()
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	up, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(up, client)
	go pipe(client, up)
	select {
	case <-done:
		// One direction finished: let the other drain, but not forever.
		select {
		case <-done:
		case <-time.After(drainTimeout):
		case <-ctx.Done():
		}
	case <-ctx.Done():
	}
}

// ---- flags ----

// flagSetter stores the value of one flag.
type flagSetter func(value string) error

func durationFlag(dst *time.Duration) flagSetter {
	return func(v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return usagef("%q is not a duration", v)
		}
		*dst = d
		return nil
	}
}

func intFlag(dst *int) flagSetter {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return usagef("%q is not a number", v)
		}
		*dst = n
		return nil
	}
}

// parseFlags reads leading "-name value" and "-name=value" pairs and returns
// what follows. It is deliberately smaller than package flag: two flags, no
// help text, and an unknown flag is a refused command line.
func parseFlags(args []string, known map[string]flagSetter) ([]string, error) {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		name, value, hasValue := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
		set, ok := known[name]
		if !ok {
			return nil, usagef("unknown flag %q", args[0])
		}
		args = args[1:]
		if !hasValue {
			if len(args) == 0 {
				return nil, usagef("flag -%s needs a value", name)
			}
			value, args = args[0], args[1:]
		}
		if err := set(value); err != nil {
			return nil, err
		}
	}
	return args, nil
}
