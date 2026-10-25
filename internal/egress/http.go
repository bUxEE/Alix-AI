package egress

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxRedirects is how many redirects an egress HTTP client follows.
const maxRedirects = 3

// redirectSafeHeaders are the request headers that survive a redirect to a
// different origin. Everything else (Authorization, Cookie, API-key headers a
// connector sets itself, Referer) is dropped, so a credential never follows a
// redirect to another host.
var redirectSafeHeaders = []string{"Accept", "Accept-Language", "User-Agent", "Content-Type"}

// HTTPClient returns an *http.Client whose every connection and request is
// held to the allowlist: see the package comment. timeout bounds one whole
// request including redirects and reading the body; zero means no client-level
// limit (the caller then bounds it with a context).
func (d *Dialer) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport:     &guardTransport{d: d, next: d.transport()},
		CheckRedirect: d.checkRedirect,
		Timeout:       timeout,
	}
}

// transport builds the underlying transport: direct (the proxy environment is
// ignored, because a proxy would receive the dial and bypass the check),
// dialing through the Dialer, certificates always verified, TLS 1.2 or later.
func (d *Dialer) transport() *http.Transport {
	var cfg *tls.Config
	if d != nil && d.set.tls != nil {
		cfg = d.set.tls.Clone()
	} else {
		cfg = &tls.Config{}
	}
	cfg.InsecureSkipVerify = false
	if cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSClientConfig:       cfg,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// guardTransport checks every request URL before any I/O, so a request for an
// unlisted destination, a wrong scheme or a URL with userinfo never reaches the
// dialer (and an idle keep-alive connection to a revoked destination is not
// reused for it).
type guardTransport struct {
	d    *Dialer
	next *http.Transport
}

func (g *guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := g.d.allowlist().CheckURL(req.URL); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if ReasonOf(err) != "" && g.d != nil && g.d.set.onDeny != nil {
			host, port := "", 0
			if req.URL != nil {
				host = req.URL.Hostname()
				port = effectivePort(req.URL)
			}
			g.d.set.onDeny(Denial{Application: g.d.app, Host: cleanHost(host), Port: port, Reason: ReasonOf(err)})
		}
		return nil, err
	}
	return g.next.RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the
// transport.
func (g *guardTransport) CloseIdleConnections() { g.next.CloseIdleConnections() }

// effectivePort is the port a request to u would connect to.
func effectivePort(u *url.URL) int {
	if p := u.Port(); p != "" {
		if n, ok := splitPort(p); ok {
			return n
		}
		return 0
	}
	if strings.EqualFold(u.Scheme, SchemeHTTPS) {
		return 443
	}
	return 80
}

// checkRedirect is the client's redirect policy.
func (d *Dialer) checkRedirect(req *http.Request, via []*http.Request) error {
	host := req.URL.Hostname()
	port := effectivePort(req.URL)
	if len(via) == 0 || len(via) > maxRedirects {
		return d.refuse(ReasonRedirect, host, port)
	}
	prev := via[len(via)-1].URL
	if strings.EqualFold(prev.Scheme, SchemeHTTPS) && !strings.EqualFold(req.URL.Scheme, SchemeHTTPS) {
		return d.refuse(ReasonRedirect, host, port)
	}
	if err := d.allowlist().CheckURL(req.URL); err != nil {
		return d.refuse(ReasonRedirect, host, port)
	}
	// Headers are compared with the origin the request was made to, not with the hop before: net/http copies the
	// original request's headers onto every hop, so after A -> B a B -> B hop would otherwise get them back.
	if !sameOrigin(via[0].URL, req.URL) {
		for name := range req.Header {
			if !isSafeHeader(name) {
				delete(req.Header, name)
			}
		}
	}
	return nil
}

func isSafeHeader(name string) bool {
	for _, s := range redirectSafeHeaders {
		if strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(strings.TrimSuffix(a.Hostname(), "."), strings.TrimSuffix(b.Hostname(), ".")) &&
		effectivePort(a) == effectivePort(b)
}

// StripURL removes the request URL that net/http adds to its errors. An
// *url.Error prints the full URL, query string included, which can hold a
// credential; connectors call StripURL before they log or return an error from
// HTTPClient. Errors that are not *url.Error are returned unchanged.
func StripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}
