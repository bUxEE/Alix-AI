package mcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// sdkTransport is the SDK's transport interface under this package's name.
type sdkTransport = mcp.Transport

// errTooLarge is what the size limiters return when a reply passes the limit.
// It is matched with errors.Is where the SDK keeps the chain, and by the
// limiters' trip counter where it does not.
var errTooLarge = errors.New("mcp: the reply is larger than the size limit")

// transportCap is the largest JSON-RPC message the transports accept: a
// structured result of at most limit bytes, its text mirror, the envelope and
// the headers.
func transportCap(limit int64) int64 { return 2*limit + framingSlack }

// ---- HTTP -------------------------------------------------------------------

// httpClient returns the client of an HTTP endpoint: the egress client (every
// connection and redirect held to the allowlist, certificates verified, no proxy)
// behind a transport that adds the endpoint's credential to requests for its own
// origin only and cuts off a reply that passes the size limit.
func (a *Adapter) httpClient(ep *endpointConn) *http.Client {
	base := a.cfg.Dialer.HTTPClient(0)
	u, _ := url.Parse(ep.cfg.URL) // validated by New
	return &http.Client{
		Transport: &endpointTripper{
			next:   base.Transport,
			origin: originOf(u),
			auth:   ep.cfg.Authorization,
			limit:  transportCap(a.cfg.MaxResponseBytes),
			trips:  &ep.trips,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// A server answered: whatever becomes of the next hop, the request
			// reached it.
			if f, _ := req.Context().Value(redirectKey{}).(*atomic.Bool); f != nil {
				f.Store(true)
			}
			return base.CheckRedirect(req, via)
		},
	}
}

// redirectKey is the context key of the flag, set by the redirect policy, that
// says a request made under the context was answered with a redirect.
type redirectKey struct{}

// redirected reports whether a request made under ctx was answered with a
// redirect: the failure of a later hop is then no proof that nothing was sent.
func redirected(ctx context.Context) bool {
	f, _ := ctx.Value(redirectKey{}).(*atomic.Bool)
	return f != nil && f.Load()
}

type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port: u.Port()}
	if o.port == "" {
		o.port = map[string]string{"http": "80", "https": "443"}[o.scheme]
	}
	return o
}

type endpointTripper struct {
	next   http.RoundTripper
	origin origin
	auth   func(context.Context) (string, error)
	limit  int64
	trips  *atomic.Int64
}

func (t *endpointTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.auth != nil && req.URL != nil && originOf(req.URL) == t.origin {
		v, err := t.auth(req.Context())
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, contracts.New(contracts.ErrUnavailable, "the credential for the application's endpoint could not be read")
		}
		if v != "" {
			req = req.Clone(req.Context())
			req.Header.Set("Authorization", v)
		}
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > t.limit {
		_ = resp.Body.Close()
		t.trips.Add(1)
		return nil, errTooLarge
	}
	resp.Body = &limitedBody{rc: resp.Body, limit: t.limit, trips: t.trips}
	return resp, nil
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the transport.
func (t *endpointTripper) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// limitedBody fails a reply body that passes the limit, so a server that sends
// without end costs at most the limit (plus one read buffer) of memory.
type limitedBody struct {
	rc      io.ReadCloser
	limit   int64
	n       int64
	tripped bool
	trips   *atomic.Int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.tripped {
		return 0, errTooLarge
	}
	n, err := b.rc.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		b.tripped = true
		b.trips.Add(1)
		return 0, errTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// httpTransport builds the streamable HTTP transport of an endpoint. The
// standalone server-to-client stream is off: the adapter has no use for server
// initiated messages and a long-lived stream is a resource a server could hold.
func (a *Adapter) httpTransport(ep *endpointConn) mcp.Transport {
	return &mcp.StreamableClientTransport{
		Endpoint:             ep.cfg.URL,
		HTTPClient:           ep.http,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
		MaxEventSize:         int(transportCap(a.cfg.MaxResponseBytes)),
	}
}

// ---- stdio ------------------------------------------------------------------

// stdioProc is a started stdio server.
type stdioProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	closeOnce sync.Once
	reaped    chan struct{}
}

// stdioGrace is how long a server gets to exit after its input is closed, before
// it is killed.
const stdioGrace = 2 * time.Second

// startStdio starts the command and returns the SDK transport over its pipes
// and the process, which the caller closes when the session ends. The command is
// run directly, with only the environment the Command names, no stderr, and its
// output bounded to one line of the transport cap.
func startStdio(c *Command, limit int64, trips *atomic.Int64) (mcp.Transport, *stdioProc, error) {
	cmd := exec.Command(c.Path, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{}, c.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	p := &stdioProc{cmd: cmd, stdin: stdin, reaped: make(chan struct{})}
	return &stdioTransport{Transport: &mcp.IOTransport{
		Reader:        &lineLimitReader{rc: stdout, proc: p, limit: limit, trips: trips},
		Writer:        stdin,
		MaxLineLength: int(limit) + 1024,
	}, proc: p}, p, nil
}

// stdioTransport ends the process when a message is still being written as the
// context it is written under ends. The SDK looks at the context only before it
// writes, so a server that stops reading would otherwise hold the write, and
// every call queued behind it, for good.
type stdioTransport struct {
	mcp.Transport
	proc *stdioProc
}

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &stdioConn{Connection: c, proc: t.proc}, nil
}

type stdioConn struct {
	mcp.Connection
	proc *stdioProc
}

func (c *stdioConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.proc.Close() })
	defer stop()
	return c.Connection.Write(ctx, msg)
}

// Close ends the process: closing its input asks it to stop, and one that does
// not exit within stdioGrace is killed. It reaps the process either way and is
// safe to call more than once and from several goroutines.
func (p *stdioProc) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = p.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(stdioGrace):
			_ = p.cmd.Process.Kill()
			<-done
		}
		close(p.reaped)
	})
	<-p.reaped
	return nil
}

// lineLimitReader passes the server's output through and fails a line that is
// longer than the limit. Closing it ends the process.
type lineLimitReader struct {
	rc    io.ReadCloser
	proc  *stdioProc
	limit int64
	line  int64
	trips *atomic.Int64
	dead  bool
}

func (r *lineLimitReader) Read(p []byte) (int, error) {
	if r.dead {
		return 0, errTooLarge
	}
	n, err := r.rc.Read(p)
	for rest := p[:n]; ; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			r.line += int64(len(rest))
		} else {
			r.line += int64(i)
		}
		if r.line > r.limit {
			r.dead = true
			r.trips.Add(1)
			return 0, errTooLarge
		}
		if i < 0 {
			return n, err
		}
		r.line, rest = 0, rest[i+1:]
	}
}

// Close ends the process, so a session closed for any reason leaves none
// behind.
func (r *lineLimitReader) Close() error {
	_ = r.rc.Close()
	if r.proc == nil {
		return nil
	}
	return r.proc.Close()
}
