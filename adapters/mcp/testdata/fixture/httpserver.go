package fixture

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// HTTPServer serves a Server over plain HTTP on a loopback port. It can be taken
// down (the listener and every connection are closed, so a new connection is
// refused) and brought back on the same port.
type HTTPServer struct {
	*Server
	// Addr is the host:port the server listens on.
	Addr string
	// URL is the endpoint URL, with the path /mcp.
	URL string

	mu      sync.Mutex
	srv     *http.Server
	down    bool
	accepts atomic.Int64
}

// StartHTTP starts a server on 127.0.0.1 and stops it when the test ends.
func StartHTTP(tb testing.TB, s *Server) *HTTPServer {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	h := &HTTPServer{Server: s, Addr: ln.Addr().String()}
	h.URL = "http://" + h.Addr + "/mcp"
	h.serve(ln)
	tb.Cleanup(h.stop)
	return h
}

func (h *HTTPServer) serve(ln net.Listener) {
	srv := &http.Server{
		Handler:           h.Server,
		ReadHeaderTimeout: 5 * time.Second,
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateNew {
				h.accepts.Add(1)
			}
		},
	}
	h.mu.Lock()
	h.srv, h.down = srv, false
	h.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

func (h *HTTPServer) stop() {
	h.mu.Lock()
	srv := h.srv
	h.srv, h.down = nil, true
	h.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	}
}

// Down closes the listener and every connection, so that connecting to the
// server is refused.
func (h *HTTPServer) Down() {
	h.mu.Lock()
	srv := h.srv
	h.srv, h.down = nil, true
	h.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// Up brings the server back on the same port. It is a no-op when it is up.
func (h *HTTPServer) Up(tb testing.TB) {
	tb.Helper()
	h.mu.Lock()
	down := h.down
	h.mu.Unlock()
	if !down {
		return
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", h.Addr); err == nil {
			h.serve(ln)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	tb.Fatalf("cannot listen on %s again: %v", h.Addr, err)
}

// Accepts is how many connections the server has accepted.
func (h *HTTPServer) Accepts() int { return int(h.accepts.Load()) }
