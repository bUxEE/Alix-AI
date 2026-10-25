package oauth

import (
	"net"
	"net/url"
	"slices"
	"strconv"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// LoopbackRedirectURI builds the loopback redirect of a listener the caller bound on a random port
// (RFC 8252 7.3): http://127.0.0.1:<port><path>. An empty path means the default "/oauth/callback". The
// engine never opens the listener; the gateway does.
func LoopbackRedirectURI(port int, path string) string {
	if path == "" {
		path = defaultLoopbackPath
	}
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) + path
}

// checkRedirect accepts a redirect URI for a provider, or refuses it. A remote redirect must equal a
// registered URI character for character (no prefix match, no extra path, query or case folding). A loopback
// redirect (RFC 8252 7.3) must be http on the IP literal 127.0.0.1 or [::1] with an explicit port and exactly
// the configured path; only the port may differ between flows. Anything else is refused, and "localhost" is
// refused because a name can resolve elsewhere.
func (c ProviderConfig) checkRedirect(raw string) error {
	reject := func() error {
		return fail(contracts.ErrValidationFailed, ReasonRedirectRejected, "oauth: the redirect URI is not registered for this provider")
	}
	if raw == "" || len(raw) > 512 {
		return reject()
	}
	if slices.Contains(c.RedirectURIs, raw) {
		return nil
	}
	if !c.Loopback {
		return reject()
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery {
		return reject()
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || (host != "127.0.0.1" && host != "::1") {
		return reject()
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return reject()
	}
	// Rebuild it: what is bound is exactly what the canonical form says, whatever tricks the input played.
	if raw != "http://"+net.JoinHostPort(host, u.Port())+c.LoopbackPath {
		return reject()
	}
	return nil
}
