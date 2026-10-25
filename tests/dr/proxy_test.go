package dr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A kill point is where, relative to the application, the service dies during a commit.
type killPoint string

const (
	// killBeforeApply holds the commit call and never forwards it: the application never sees it.
	killBeforeApply killPoint = "before_the_application_applies"
	// killAfterApply forwards the commit call, lets the application apply it, and swallows the reply:
	// the application has the booking and AlixAI never learns.
	killAfterApply killPoint = "after_the_application_applies"
)

// injection is one armed fault: the next call of a tool is held at a kill point.
type injection struct {
	tool  string
	point killPoint
	// reached is closed when the call arrives at the proxy; applied once the application has
	// answered it (killAfterApply only).
	reached chan struct{}
	applied chan struct{}
	release chan struct{}
}

// mcpProxy is a loopback reverse proxy between AlixAI and the application. The package under drill
// names its address as the endpoint, so every MCP call passes through it. It forwards everything
// untouched, except for one injection at a time: the next call of one named tool is held so that a
// test can kill the service while that call is in flight. It is the drill's only seam.
type mcpProxy struct {
	ln     net.Listener
	srv    *http.Server
	target *url.URL
	port   int

	mu    sync.Mutex
	armed *injection
	seen  map[string]int // calls per tool that reached the proxy
}

func startProxy(targetMCP string) (*mcpProxy, error) {
	u, err := url.Parse(targetMCP)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &mcpProxy{ln: ln, target: u, port: ln.Addr().(*net.TCPAddr).Port, seen: map[string]int{}}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.handle), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *mcpProxy) stop() {
	p.releaseAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.srv.Shutdown(ctx)
	_ = p.srv.Close()
}

// arm makes the next call of tool stop at the kill point.
func (p *mcpProxy) arm(tool string, point killPoint) *injection {
	inj := &injection{tool: tool, point: point, reached: make(chan struct{}), applied: make(chan struct{}), release: make(chan struct{})}
	p.mu.Lock()
	p.armed = inj
	p.mu.Unlock()
	return inj
}

// disarm cancels an injection that has not fired and lets a held call go.
func (p *mcpProxy) releaseAll() {
	p.mu.Lock()
	inj := p.armed
	p.armed = nil
	p.mu.Unlock()
	if inj != nil {
		select {
		case <-inj.release:
		default:
			close(inj.release)
		}
	}
}

// calls is how many calls of the tool reached the proxy.
func (p *mcpProxy) calls(tool string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen[tool]
}

// toolOf names the tool of a JSON-RPC tools/call, or "" for anything else.
func toolOf(body []byte) string {
	var m struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &m) != nil || m.Method != "tools/call" {
		return ""
	}
	return m.Params.Name
}

func (p *mcpProxy) take(tool string) *injection {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tool != "" {
		p.seen[tool]++
	}
	if p.armed != nil && p.armed.tool == tool {
		inj := p.armed
		p.armed = nil
		return inj
	}
	return nil
}

func (p *mcpProxy) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	inj := p.take(toolOf(body))
	if inj != nil && inj.point == killBeforeApply {
		close(inj.reached)
		hold(r, inj)
		return
	}
	status, header, reply, err := p.forward(r, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if inj != nil {
		close(inj.reached)
		close(inj.applied)
		hold(r, inj)
		return
	}
	for k, vs := range header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(reply)
}

// hold keeps a call unanswered until the caller goes away (the service was killed), the test lets
// it go, or a generous limit passes.
func hold(r *http.Request, inj *injection) {
	select {
	case <-r.Context().Done():
	case <-inj.release:
	case <-time.After(2 * time.Minute):
	}
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true,
}

func (p *mcpProxy) forward(r *http.Request, body []byte) (int, http.Header, []byte, error) {
	// The package names the proxy as http://127.0.0.1:PORT/mcp, the application serves /mcp: the
	// path is the same on both sides.
	target := *p.target
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, vs := range r.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] || strings.EqualFold(k, "Origin") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := clinicHTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	h := http.Header{}
	for k, vs := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		h[k] = vs
	}
	return resp.StatusCode, h, reply, nil
}

// wait blocks until ch is closed, or fails after a limit.
func waitFor(ch <-chan struct{}, what string, limit time.Duration) error {
	select {
	case <-ch:
		return nil
	case <-time.After(limit):
		return errors.New("timed out waiting for " + what)
	}
}
