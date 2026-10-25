package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// ErrNotLoopback marks the refusal to bind, serve or answer anything that is
// not the machine itself.
var ErrNotLoopback = errors.New("metrics are only ever served on a loopback address")

// DefaultListen is the address the `metrics` section proposes. The listener is
// off until an operator enables it.
const DefaultListen = "127.0.0.1:9464"

// CheckLoopbackAddr reports whether addr ("host:port") names a literal
// loopback address: 127.0.0.0/8, ::1 or the name localhost. An empty host
// (":9464"), an unspecified address (0.0.0.0, ::), a LAN or public address and
// any other name are refused: a name is resolved by someone else and may move.
func CheckLoopbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("metrics: %q is not host:port: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("metrics: %q has no valid port", addr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.Unmap().IsLoopback() {
		return fmt.Errorf("metrics: %q is not a loopback address: %w", addr, ErrNotLoopback)
	}
	return nil
}

// Listen binds addr, which must pass CheckLoopbackAddr, and checks the bound
// address again so a platform that maps a name elsewhere cannot widen it. An
// empty addr is DefaultListen's host on an ephemeral port.
func Listen(addr string) (net.Listener, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if err := CheckLoopbackAddr(addr); err != nil {
		return nil, err
	}
	if host, port, _ := net.SplitHostPort(addr); strings.EqualFold(host, "localhost") {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics: cannot listen on %s: %w", addr, err)
	}
	if !LoopbackListener(ln) {
		_ = ln.Close()
		return nil, fmt.Errorf("metrics: refusing the listener %s: %w", ln.Addr(), ErrNotLoopback)
	}
	return ln, nil
}

// LoopbackListener reports whether ln is a TCP listener on a loopback address.
func LoopbackListener(ln net.Listener) bool {
	if ln == nil {
		return false
	}
	a, ok := ln.Addr().(*net.TCPAddr)
	return ok && a.IP != nil && a.IP.IsLoopback()
}

func loopbackIP(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Zone() == "" && ip.Unmap().IsLoopback()
}

// LoopbackRequest is true only for a request whose peer and local address are
// both loopback and whose Host names the machine itself. A request whose
// addresses cannot be read is not loopback: the check fails closed. The Host
// rule is what stops a web page from reading metrics through DNS rebinding.
func LoopbackRequest(r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackIP(peer) {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	tcp, ok := local.(*net.TCPAddr)
	if !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || loopbackIP(host)
}

// LoopbackOnly answers 403 to every request that is not LoopbackRequest, for
// every path and method, before next sees it.
func LoopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !LoopbackRequest(r) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MetricsPath is the one path Handler serves.
const MetricsPath = "/metrics"

// Handler serves the registry at /metrics in the text exposition format, to
// loopback requests only (GET and HEAD). The output is built in memory first,
// so a failure never leaves half a scrape on the wire.
func Handler(reg *Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+MetricsPath, func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := WriteText(&buf, reg.Gather(r.Context())); err != nil {
			http.Error(w, "the metrics could not be rendered", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Type", ContentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(buf.Bytes())
	})
	return LoopbackOnly(mux)
}

// Serve serves h on ln until ctx is done, then drains in-flight requests. A
// listener that is not loopback TCP is refused and closed before anything is
// served, whoever bound it, and h runs behind LoopbackOnly whatever it is.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	if !LoopbackListener(ln) {
		addr := "<nil>"
		if ln != nil {
			addr = ln.Addr().String()
			_ = ln.Close()
		}
		return fmt.Errorf("metrics: refusing the listener %s: %w", addr, ErrNotLoopback)
	}
	srv := &http.Server{
		Handler:           LoopbackOnly(h),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       15 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
