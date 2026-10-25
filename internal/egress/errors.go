package egress

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Reason says why a connection was refused. It is stable: audit records and
// tests match on it.
type Reason string

const (
	// ReasonNoAllowlist: no allowlist is configured for the caller (default
	// deny).
	ReasonNoAllowlist Reason = "no_allowlist"
	// ReasonNotAllowlisted: the host and port are not both on the allowlist.
	ReasonNotAllowlisted Reason = "not_allowlisted"
	// ReasonBlockedAddress: the destination resolves to an address that is
	// never reachable, or to a local address the allowlist entry does not
	// name.
	ReasonBlockedAddress Reason = "blocked_address"
	// ReasonPlatformPort: the destination is one of the platform's own
	// (the gateway, the operator listeners, LocalAI) and is refused even
	// when the allowlist names it.
	ReasonPlatformPort Reason = "platform_port"
	// ReasonBadNetwork: the network is not tcp, tcp4 or tcp6.
	ReasonBadNetwork Reason = "network_not_allowed"
	// ReasonBadAddress: the address could not be parsed as host and port.
	ReasonBadAddress Reason = "bad_address"
	// ReasonScheme: the URL scheme is not the one the allowlist entry names,
	// or is not http or https.
	ReasonScheme Reason = "scheme_not_allowed"
	// ReasonRedirect: a redirect that the policy refuses (unlisted target,
	// scheme downgrade or too many hops).
	ReasonRedirect Reason = "redirect_not_allowed"
	// ReasonURL: the URL carries something the gate refuses (userinfo, an
	// opaque form, no host).
	ReasonURL Reason = "url_not_allowed"
)

// Denial is what the deny hook receives.
type Denial struct {
	// Application is the application the Dialer belongs to ("" when it was
	// built without a Registry).
	Application string
	Host        string
	Port        int
	Reason      Reason
}

const component = "egress"

// deny builds the typed refusal. The host is cleaned (it can be attacker
// supplied text) and bounded; nothing else about the request is included.
func deny(reason Reason, host string, port int) *contracts.Error {
	e := contracts.New(contracts.ErrPermissionDenied, "egress denied: "+reasonText(reason)).
		WithDetails("component", component).
		WithDetails("reason", string(reason))
	switch {
	case host != "" && port > 0:
		e.WithDetails("destination", net.JoinHostPort(cleanHost(host), strconv.Itoa(port)))
	case host != "":
		e.WithDetails("destination", cleanHost(host))
	}
	return e
}

func reasonText(r Reason) string {
	switch r {
	case ReasonNoAllowlist:
		return "no destination allowlist is configured"
	case ReasonNotAllowlisted:
		return "destination is not on the allowlist"
	case ReasonBlockedAddress:
		return "destination address is not reachable"
	case ReasonPlatformPort:
		return "destination is a platform port"
	case ReasonBadNetwork:
		return "network is not allowed"
	case ReasonBadAddress:
		return "address is malformed"
	case ReasonScheme:
		return "scheme is not allowed for this destination"
	case ReasonRedirect:
		return "redirect is not allowed"
	case ReasonURL:
		return "url is not allowed"
	}
	return "refused"
}

// cleanHost makes untrusted host text safe to put in an error: printable
// ASCII only, at most 255 characters.
func cleanHost(h string) string {
	var b strings.Builder
	for _, r := range h {
		if b.Len() >= 255 {
			break
		}
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			b.WriteByte('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsDenied reports whether err is a refusal produced by this package.
func IsDenied(err error) bool { return ReasonOf(err) != "" }

// ReasonOf returns the Reason of a refusal produced by this package, or "".
func ReasonOf(err error) Reason {
	var ce *contracts.Error
	for err != nil {
		if errors.As(err, &ce) && ce != nil {
			if ce.Code == contracts.ErrPermissionDenied && ce.Details["component"] == component {
				if r, ok := ce.Details["reason"].(string); ok {
					return Reason(r)
				}
			}
			err = ce.Unwrap()
			continue
		}
		return ""
	}
	return ""
}
