package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Header member names.
const (
	HeaderAlgorithm = "alg"
	HeaderType      = "typ"
	HeaderKeyID     = "kid"
)

// Claim names. The context levels reuse the names of schemas/common/scope; the
// principal is the registered "sub" claim. There is deliberately no claim for
// the deployment level: it comes from configuration, and a token that carries
// one is rejected.
const (
	ClaimIssuer           = "iss"
	ClaimAudience         = "aud"
	ClaimSubject          = "sub"
	ClaimExpiry           = "exp"
	ClaimNotBefore        = "nbf"
	ClaimIssuedAt         = "iat"
	ClaimTokenID          = "jti"
	ClaimApplication      = "application_id"
	ClaimTenant           = "tenant_id"
	ClaimWorkspace        = "workspace_id"
	ClaimRoles            = "roles"
	ClaimCapabilityScopes = "capability_scopes"
	ClaimSession          = "session_id"
)

var (
	headerMembers = map[string]bool{HeaderAlgorithm: true, HeaderType: true, HeaderKeyID: true}
	claimMembers  = map[string]bool{
		ClaimIssuer: true, ClaimAudience: true, ClaimSubject: true, ClaimExpiry: true,
		ClaimNotBefore: true, ClaimIssuedAt: true, ClaimTokenID: true,
		ClaimApplication: true, ClaimTenant: true, ClaimWorkspace: true,
		ClaimRoles: true, ClaimCapabilityScopes: true, ClaimSession: true,
	}
)

// Bounds that keep a hostile but correctly signed token from costing much.
const (
	maxRoles            = 64
	maxCapabilityScopes = 256
	maxIssuerLen        = 256
	maxKeyIDLen         = 64
	minTokenIDLen       = 16
	maxTokenIDLen       = 128
	// maxNumericDateDigits keeps a NumericDate far beyond any real timestamp
	// yet small enough that arithmetic on two of them cannot overflow an int64.
	maxNumericDateDigits = 12
)

var errBadJSON = errors.New("identity: not a strict JSON object")

// parseObject decodes data as exactly one JSON object and returns its members
// as raw values. It is stricter than encoding/json: invalid UTF-8, duplicate
// member names (compared after unescaping, case-sensitively) and trailing data
// are errors. Values stay raw so callers can type-check them without the
// standard library's lenient conversions (null into string, case-insensitive
// field matching).
func parseObject(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errBadJSON
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errBadJSON
	}
	out := make(map[string]json.RawMessage)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, errBadJSON
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errBadJSON
		}
		if _, dup := out[key]; dup {
			return nil, errBadJSON
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errBadJSON
		}
		out[key] = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errBadJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errBadJSON
	}
	return out, nil
}

// onlyMembers reports whether every member name is in allowed.
func onlyMembers(m map[string]json.RawMessage, allowed map[string]bool) bool {
	for k := range m {
		if !allowed[k] {
			return false
		}
	}
	return true
}

// jsonString returns the string a raw JSON value holds. Anything that is not a
// JSON string (including null) is refused, as is a string containing U+FFFD,
// which is what the standard library substitutes for broken escapes.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		return "", false
	}
	return s, true
}

// jsonStringList returns the strings of a raw JSON array of strings, refusing
// null elements and arrays longer than limit.
func jsonStringList(raw json.RawMessage, limit int) ([]string, bool) {
	if len(raw) < 2 || raw[0] != '[' {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) > limit {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := jsonString(it)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// jsonNumericDate returns a NumericDate as whole seconds. It accepts only a
// plain non-negative integer of at most maxNumericDateDigits digits: no sign,
// fraction, exponent or leading zero, so every instant has one spelling and no
// arithmetic on two dates can overflow.
func jsonNumericDate(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || len(raw) > maxNumericDateDigits {
		return 0, false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, false
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// header is the validated JOSE header.
type header struct {
	keyID string
}

// parseHeader enforces the header profile: exactly alg, typ and kid, with the
// one accepted algorithm, the explicit token type and a well-formed key id.
func parseHeader(data []byte) (header, error) {
	m, err := parseObject(data)
	if err != nil || !onlyMembers(m, headerMembers) {
		return header{}, unauthenticated(ReasonBadHeader)
	}
	alg, ok1 := jsonString(m[HeaderAlgorithm])
	typ, ok2 := jsonString(m[HeaderType])
	kid, ok3 := jsonString(m[HeaderKeyID])
	if !ok1 || !ok2 || !ok3 || alg != Algorithm || typ != TokenType || !validKeyID(kid) {
		return header{}, unauthenticated(ReasonBadHeader)
	}
	return header{keyID: kid}, nil
}

// claims are the parsed claims of a token whose signature has verified.
// Required authentication claims are checked for presence by parseClaims; the
// context levels may be empty and are judged by NewScope, so that a missing
// level surfaces as scope_missing.
type claims struct {
	issuer, audience, tokenID string
	issuedAt, notBefore, exp  int64
	context                   ContextClaims
}

func parseClaims(data []byte) (claims, error) {
	bad := unauthenticated(ReasonBadClaims)
	m, err := parseObject(data)
	if err != nil || !onlyMembers(m, claimMembers) {
		return claims{}, bad
	}
	var c claims
	var ok bool

	// Authentication claims: all required.
	if c.issuer, ok = jsonString(m[ClaimIssuer]); !ok || c.issuer == "" || len(c.issuer) > maxIssuerLen {
		return claims{}, bad
	}
	if c.audience, ok = parseAudience(m[ClaimAudience]); !ok {
		return claims{}, bad
	}
	if c.tokenID, ok = jsonString(m[ClaimTokenID]); !ok || !validTokenID(c.tokenID) {
		return claims{}, bad
	}
	if c.issuedAt, ok = jsonNumericDate(m[ClaimIssuedAt]); !ok {
		return claims{}, bad
	}
	if c.notBefore, ok = jsonNumericDate(m[ClaimNotBefore]); !ok {
		return claims{}, bad
	}
	if c.exp, ok = jsonNumericDate(m[ClaimExpiry]); !ok {
		return claims{}, bad
	}

	// Context levels: optional here (absent means empty, judged by NewScope),
	// but if present they must have the right type.
	str := func(name string, dst *string) bool {
		raw, present := m[name]
		if !present {
			return true
		}
		s, ok := jsonString(raw)
		*dst = s
		return ok
	}
	list := func(name string, limit int, dst *[]string) bool {
		raw, present := m[name]
		if !present {
			return true
		}
		l, ok := jsonStringList(raw, limit)
		*dst = l
		return ok
	}
	if !str(ClaimApplication, &c.context.ApplicationID) ||
		!str(ClaimTenant, &c.context.TenantID) ||
		!str(ClaimWorkspace, &c.context.WorkspaceID) ||
		!str(ClaimSubject, &c.context.PrincipalID) ||
		!str(ClaimSession, &c.context.SessionID) ||
		!list(ClaimRoles, maxRoles, &c.context.Roles) ||
		!list(ClaimCapabilityScopes, maxCapabilityScopes, &c.context.CapabilityScopes) {
		return claims{}, bad
	}
	return c, nil
}

// parseAudience accepts aud as a string or as an array holding exactly one
// string. A token addressed to several audiences could be replayed against
// another deployment whose replay cache has never seen it, so it is refused.
func parseAudience(raw json.RawMessage) (string, bool) {
	if len(raw) > 0 && raw[0] == '[' {
		l, ok := jsonStringList(raw, 1)
		if !ok || len(l) != 1 {
			return "", false
		}
		return l[0], l[0] != ""
	}
	s, ok := jsonString(raw)
	return s, ok && s != ""
}

func validKeyID(s string) bool {
	return len(s) > 0 && len(s) <= maxKeyIDLen && plainToken(s)
}

func validTokenID(s string) bool {
	return len(s) >= minTokenIDLen && len(s) <= maxTokenIDLen && plainToken(s)
}

// plainToken reports whether s consists only of ASCII letters, digits and the
// characters - _ .
func plainToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
