package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Resolver looks up host addresses. *net.Resolver implements it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

const (
	maxRedirects       = 5
	defaultHTTPTimeout = 15 * time.Second
	maxBodyBytes       = 2 << 20 // 2 MiB
	maxExtractRunes    = 200_000
)

func normalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.Fragment = ""
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else if scheme == "http" {
			port = "80"
		}
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	q := u.RawQuery
	out := scheme + "://" + host
	if !((scheme == "https" && port == "443") || (scheme == "http" && port == "80")) {
		out += ":" + port
	}
	out += path
	if q != "" {
		out += "?" + q
	}
	return out
}

func validatePublicURL(raw string, httpsOnly bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, contracts.New(contracts.ErrWebQueryRefused, "web: URL is malformed").
			WithDetails("reason", "bad_url")
	}
	scheme := strings.ToLower(u.Scheme)
	if httpsOnly {
		if scheme != "https" {
			return nil, contracts.New(contracts.ErrWebQueryRefused, "web: only https URLs may be fetched").
				WithDetails("reason", "non_https")
		}
	} else if scheme != "https" && scheme != "http" {
		return nil, contracts.New(contracts.ErrWebQueryRefused, "web: URL scheme is not allowed").
			WithDetails("reason", "bad_scheme")
	}
	if u.User != nil {
		return nil, contracts.New(contracts.ErrWebQueryRefused, "web: URL must not carry userinfo").
			WithDetails("reason", "userinfo")
	}
	host := u.Hostname()
	if host == "" {
		return nil, contracts.New(contracts.ErrWebQueryRefused, "web: URL host is missing").
			WithDetails("reason", "bad_host")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil || (p != 443 && p != 80) {
		return nil, contracts.New(contracts.ErrWebQueryRefused, "web: only ports 80 and 443 are allowed").
			WithDetails("reason", "bad_port")
	}
	return u, nil
}

func isBlockedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
		return true
	}
	// Extra never-blocks (cloud metadata etc.) even if GlobalUnicast claims otherwise.
	for _, p := range []string{
		"169.254.0.0/16", "0.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4",
		"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15",
		"fe80::/10", "ff00::/8", "fc00::/7", "::1/128",
	} {
		if netip.MustParsePrefix(p).Contains(ip) {
			return true
		}
	}
	return false
}

func resolvePublic(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	// Literal IP in the URL.
	if ip, err := netip.ParseAddr(host); err == nil {
		if isBlockedAddr(ip) {
			return nil, contracts.New(contracts.ErrWebQueryRefused, "web: address is not publicly routable").
				WithDetails("reason", "private_ip")
		}
		return []netip.Addr{ip}, nil
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, contracts.Wrap(contracts.ErrUnavailable, err, "web: DNS lookup failed")
	}
	if len(addrs) == 0 {
		return nil, contracts.New(contracts.ErrWebQueryRefused, "web: host resolved to no addresses").
			WithDetails("reason", "no_addrs")
	}
	var out []netip.Addr
	for _, a := range addrs {
		if isBlockedAddr(a) {
			return nil, contracts.New(contracts.ErrWebQueryRefused, "web: address is not publicly routable").
				WithDetails("reason", "private_ip")
		}
		out = append(out, a)
	}
	return out, nil
}

// safeClient builds an HTTP client that re-checks destination on every dial and redirect.
type safeClient struct {
	resolver Resolver
	timeout  time.Duration
	client   *http.Client
}

func newSafeClient(r Resolver, timeout time.Duration) *safeClient {
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	sc := &safeClient{resolver: r, timeout: timeout}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			p, err := strconv.Atoi(port)
			if err != nil || (p != 80 && p != 443) {
				return nil, fmt.Errorf("web: refused port")
			}
			addrs, err := resolvePublic(ctx, sc.resolver, host)
			if err != nil {
				return nil, err
			}
			var last error
			d := net.Dialer{Timeout: timeout}
			for _, a := range addrs {
				c, err := d.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
				if err == nil {
					return c, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("web: dial failed")
			}
			return nil, last
		},
		DisableKeepAlives: true,
	}
	sc.client = &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return contracts.New(contracts.ErrWebQueryRefused, "web: too many redirects").
					WithDetails("reason", "redirect_limit")
			}
			if _, err := validatePublicURL(req.URL.String(), true); err != nil {
				return err
			}
			if _, err := resolvePublic(req.Context(), sc.resolver, req.URL.Hostname()); err != nil {
				return err
			}
			return nil
		},
	}
	return sc
}
