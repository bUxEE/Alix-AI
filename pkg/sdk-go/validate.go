package sdkgo

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Local validation that runs before anything is sent. A malformed argument must never cost a single-use
// context token or reach the wire, so every check here happens before the token provider is called.

var (
	pathIDPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	confirmationPattern = regexp.MustCompile(`^cfm_[A-Za-z0-9_-]{43}$`)
	token68Pattern      = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
	sourceIDPattern     = regexp.MustCompile(`^src_[0-9A-HJKMNP-TV-Z]{26}$`)
	generationPattern   = regexp.MustCompile(`^gen_[0-9A-HJKMNP-TV-Z]{26}$`)
	filePattern         = regexp.MustCompile(`^fil_[0-9A-HJKMNP-TV-Z]{26}$`)
)

// pathID checks a session, turn, preparation or application id as it appears in a path: the pattern of the
// OpenAPI document. "." and ".." match it but URL normalisation would rewrite the path, so they are refused.
func pathID(argument, value string) error {
	if !pathIDPattern.MatchString(value) || value == "." || value == ".." {
		return invalidArgument(argument, "it is not a valid identifier")
	}
	return nil
}

func patternID(argument string, pattern *regexp.Regexp, value string) error {
	if !pattern.MatchString(value) {
		return invalidArgument(argument, "it is not a well-formed identifier")
	}
	return nil
}

// checkConfirmationToken validates the confirmation secret. The value is never echoed in the error.
func checkConfirmationToken(value string) error {
	if !confirmationPattern.MatchString(value) {
		return invalidArgument("token", "the confirmation token is malformed")
	}
	return nil
}

// checkContextToken refuses anything that is not a bare bearer credential (whitespace, a "Bearer " prefix, a
// newline that would split the header).
func checkContextToken(value string) error {
	if value == "" || len(value) > 16*1024 || !token68Pattern.MatchString(value) {
		return localError(contracts.ErrUnauthenticated,
			`the token provider must return the bare context token (no whitespace, no "Bearer" prefix)`,
			"reason", "bad_token_shape")
	}
	return nil
}

// isLoopbackHost reports a loopback name or address. An address must parse as one: "127.foo.example.com" is a
// DNS name that can resolve anywhere.
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// normalizeBaseURL returns the base URL without a trailing slash, path prefix kept. The context token
// travels as a bearer credential, so plain http is accepted only for a loopback host unless the caller opts
// in; credentials in the URL, a query and a fragment are refused.
func normalizeBaseURL(input string, allowInsecureHTTP bool) (*url.URL, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, invalidArgument("BaseURL", "it is required")
	}
	u, err := url.Parse(input)
	if err != nil || u.Host == "" {
		return nil, invalidArgument("BaseURL", "it is not an absolute URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, invalidArgument("BaseURL", "it must be http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalidArgument("BaseURL", "it must not carry credentials, a query or a fragment")
	}
	if u.Scheme == "http" && !allowInsecureHTTP && !isLoopbackHost(u.Hostname()) {
		return nil, invalidArgument("BaseURL",
			"it must be https: the context token is a bearer credential (http is accepted only for loopback; set AllowInsecureHTTP to override)")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// validText checks a question or search text: a non-space character, no NUL, at most 2000 characters.
func validText(argument, value string) error {
	if strings.TrimSpace(value) == "" {
		return invalidArgument(argument, "it must be a non-empty string")
	}
	if strings.ContainsRune(value, 0) {
		return invalidArgument(argument, "it must not contain a NUL")
	}
	if !utf8.ValidString(value) {
		return invalidArgument(argument, "it must be valid UTF-8")
	}
	if utf8.RuneCountInString(value) > 2000 {
		return invalidArgument(argument, "it must be at most 2000 characters")
	}
	return nil
}
