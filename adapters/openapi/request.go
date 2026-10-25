package openapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// callArgs is everything a call puts on the wire.
type callArgs struct {
	// fields are the capability's field values (after the binding's renames):
	// scalars and flat lists, canonical JSON values.
	fields map[string]any
	// optional names the fields that are sent only when the operation declares a
	// place for them (the reserved entity_type of a resolver, whose operation
	// is already specific to the type).
	optional map[string]bool
	// key is the idempotency key of a Commit, Status or Cancel.
	key string
	// prepared is the body of a Commit.
	prepared *actions.PreparedAction
	// identity is the identity context of a call made for a scope, nil for the
	// calls that are not (describe, health, provisioning).
	identity *adapters.Identity
	// aspects are the aspects an Inspect asks for (all when empty).
	aspects []adapters.Aspect
}

// wire is one request, ready to send, and the copy that is checked against the
// document.
type wire struct {
	req   *http.Request
	check *openapi3filter.RequestValidationInput
	// method of the request, for the response check.
	method string
}

func violates(msg string) *contracts.Error {
	return typed(contracts.ErrValidationFailed, "adapter request: "+msg, ReasonRequestViolatesSpec)
}

func notEncodable(msg string) *contracts.Error {
	return typed(contracts.ErrValidationFailed, "adapter request: "+msg, ReasonValueNotEncodable)
}

// canonicalFields round-trips the fields through JSON, so every value is a
// string, bool, json.Number or a list of them (adapters.Wrap has already refused
// anything else), exactly the form the core validated.
func canonicalFields(in map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, v := range in {
		c, err := canonicalValue(v)
		if err != nil {
			return nil, contracts.New(contracts.ErrValidationFailed, "adapter request: a field value cannot be encoded as JSON")
		}
		out[k] = c
	}
	return out, nil
}

func canonicalValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// scalar renders a scalar value as the text of a parameter.
func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	}
	return "", false
}

// items renders a value as the list of texts of a parameter: one for a scalar.
func items(v any) ([]string, bool) {
	if l, ok := v.([]any); ok {
		out := make([]string, 0, len(l))
		for _, it := range l {
			s, ok := scalar(it)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	s, ok := scalar(v)
	return []string{s}, ok
}

// validHeaderValue reports whether s can be a header value: no control
// characters (a CR or LF would split the message) and valid UTF-8.
func validHeaderValue(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// pathSegment checks and escapes the value of a path parameter. A value that
// would change which operation is addressed (an empty segment, a dot segment, a
// separator) is refused, whatever the application's router does with it.
func pathSegment(s string) (string, error) {
	switch {
	case s == "" || s == "." || s == "..":
		return "", notEncodable("a path parameter is empty or a dot segment")
	case strings.ContainsAny(s, "/\\") || !validHeaderValue(s) || strings.ContainsRune(s, '\t'):
		return "", notEncodable("a path parameter holds a separator or a control character")
	}
	return url.PathEscape(s), nil
}

// assign puts one value in the place its parameter declares.
func (b *builder) assign(p *param, name string, v any) error {
	list, ok := items(v)
	if !ok {
		return notEncodable("a parameter value is not a scalar or a flat list of scalars")
	}
	_, isList := v.([]any)
	switch p.in {
	case openapi3.ParameterInPath:
		if isList {
			return notEncodable("a path parameter cannot take a list")
		}
		seg, err := pathSegment(list[0])
		if err != nil {
			return err
		}
		b.pathRaw[name], b.pathVal[name] = seg, list[0]
	case openapi3.ParameterInQuery:
		if isList && p.explode {
			for _, it := range list {
				b.query.Add(name, it)
			}
			return nil
		}
		if isList {
			for _, it := range list {
				if strings.Contains(it, ",") {
					return notEncodable("a list item holds a comma, which the parameter's style cannot carry")
				}
			}
		}
		b.query.Set(name, strings.Join(list, ","))
	case openapi3.ParameterInHeader:
		for _, it := range list {
			if !validHeaderValue(it) || isList && strings.Contains(it, ",") {
				return notEncodable("a header value holds a control character or a comma")
			}
		}
		b.header.Set(p.p.Name, strings.Join(list, ","))
	}
	return nil
}

// builder collects the parts of one request.
type builder struct {
	pathRaw map[string]string // escaped, for the URL
	pathVal map[string]string // as the application's router will decode them
	query   url.Values
	header  http.Header
	body    map[string]any
}

// build assembles the request of a call and checks it against the document. A
// request the document does not allow is never sent.
func (a *Adapter) build(ctx context.Context, rt *route, args callArgs) (*wire, error) {
	op, cfg := rt.op, &rt.op.ep.cfg.Settings
	b := &builder{pathRaw: map[string]string{}, pathVal: map[string]string{}, query: url.Values{}, header: http.Header{}}

	// The protocol's own values come first, and a field can never take their place.
	if args.key != "" {
		if p := op.params["idempotency_key"]; p != nil && p.in != openapi3.ParameterInHeader {
			if err := b.assign(p, "idempotency_key", args.key); err != nil {
				return nil, err
			}
		}
	}
	if len(args.aspects) > 0 {
		names := make([]any, len(args.aspects))
		for i, as := range args.aspects {
			names[i] = string(as)
		}
		if p := op.params["aspects"]; p != nil && p.in == openapi3.ParameterInQuery {
			if err := b.assign(p, "aspects", names); err != nil {
				return nil, err
			}
		} else {
			return nil, violates("the inspect operation declares no aspects query parameter")
		}
	}

	names := make([]string, 0, len(args.fields))
	for n := range args.fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		v := args.fields[name]
		if p := op.param(name); p != nil {
			if (name == "idempotency_key" && args.key != "") || (name == "aspects" && len(args.aspects) > 0) {
				return nil, typed(contracts.ErrValidationFailed, "adapter request: a field named like a parameter the protocol fills", ReasonFieldNotAccepted)
			}
			if err := b.assign(p, p.p.Name, v); err != nil {
				return nil, err
			}
			continue
		}
		if op.body != nil && (!args.optional[name] || op.bodyDeclares(name)) {
			if b.body == nil {
				b.body = map[string]any{}
			}
			b.body[name] = v
			continue
		}
		if args.optional[name] {
			continue
		}
		return nil, typed(contracts.ErrValidationFailed, "adapter request: a field has no parameter and no request body to travel in", ReasonFieldNotAccepted)
	}

	// The body: a Commit carries the prepared action, everything else the fields
	// that are not parameters.
	var body []byte
	switch {
	case args.prepared != nil:
		if op.body == nil {
			return nil, violates("the commit operation declares no request body for the prepared action")
		}
		raw, err := json.Marshal(args.prepared)
		if err != nil {
			return nil, violates("the prepared action cannot be encoded")
		}
		body = raw
	case op.body != nil && (len(b.body) > 0 || op.bodyRequired):
		if b.body == nil {
			b.body = map[string]any{}
		}
		raw, err := json.Marshal(b.body)
		if err != nil {
			return nil, violates("the request body cannot be encoded")
		}
		body = raw
	}
	if len(body) > maxRequestBytes {
		return nil, typed(contracts.ErrValidationFailed, "adapter request: the request body is too large", ReasonRequestTooLarge)
	}

	u, err := a.requestURL(op, b)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Accept", "application/json")
	header.Set("User-Agent", a.cfg.UserAgent)
	if len(body) > 0 {
		header.Set("Content-Type", "application/json")
	}
	for k, vs := range b.header {
		header[k] = vs
	}
	if args.identity != nil {
		raw, err := json.Marshal(args.identity)
		if err != nil {
			return nil, violates("the identity cannot be encoded")
		}
		header.Set(cfg.Identity.Header, base64.RawURLEncoding.EncodeToString(raw))
	}
	if args.key != "" {
		header.Set(cfg.Idempotency.Header, args.key)
	}

	mk := func(withBody bool) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, op.method, u.String(), nil)
		if err != nil {
			return nil, violates("the request cannot be built")
		}
		req.Header = header.Clone()
		if withBody && len(body) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		return req, nil
	}
	// The request that is sent. Every request asks for the connection to be
	// closed after its reply, so a connection is never reused: net/http retries a
	// request on its own when a reused connection turns out to be dead, and
	// this adapter never retries (a repeat of a Commit is the coordinator's
	// decision, taken after Status). The body has no GetBody for the same reason.
	req, err := mk(true)
	if err != nil {
		return nil, err
	}
	req.Close = true
	req.GetBody = nil

	// The copy that is validated: the same method, URL, headers and body bytes,
	// plus a placeholder for the credential when the document declares its
	// header (the real credential is added on the way out and never seen by the
	// validator or by an error message).
	vreq, err := mk(true)
	if err != nil {
		return nil, err
	}
	if op.authDeclared {
		placeholder := "placeholder"
		if s := *cfg.Auth.Scheme; s != "" {
			placeholder = s + " " + placeholder
		}
		vreq.Header.Set(cfg.authHeader(), placeholder)
	}
	input := &openapi3filter.RequestValidationInput{
		Request:    vreq,
		PathParams: b.pathVal,
		Route:      op.route,
		Options: &openapi3filter.Options{
			AuthenticationFunc:  openapi3filter.NoopAuthenticationFunc,
			SkipSettingDefaults: true,
		},
	}
	if err := openapi3filter.ValidateRequest(ctx, input); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, requestViolation(err)
	}
	return &wire{req: req, check: input, method: op.method}, nil
}

// requestViolation turns a validation failure into a fixed-message error that
// names where the request is wrong and never what it held.
func requestViolation(err error) error {
	e := violates("the request does not satisfy the operation's description in the OpenAPI document")
	var re *openapi3filter.RequestError
	if errors.As(err, &re) {
		switch {
		case re.Parameter != nil:
			e.WithDetails("location", "parameter "+safeToken(re.Parameter.Name)+" ("+safeToken(re.Parameter.In)+")")
		case re.RequestBody != nil:
			e.WithDetails("location", "request body"+pointerOf(err))
		}
		return e
	}
	var se *openapi3filter.SecurityRequirementsError
	if errors.As(err, &se) {
		e.WithDetails("location", "security requirements")
	}
	return e
}

// pointerOf renders the JSON pointer of the schema error in err's chain, with
// names the application chose reduced to safe tokens.
func pointerOf(err error) string {
	var se *openapi3.SchemaError
	if !errors.As(err, &se) || se == nil {
		return ""
	}
	var b strings.Builder
	for _, seg := range se.JSONPointer() {
		b.WriteString("/" + safeToken(seg))
	}
	if se.SchemaField != "" {
		b.WriteString(" (" + safeToken(se.SchemaField) + ")")
	}
	return truncate(b.String(), 200)
}

// safeToken keeps a name short and printable, so text a peer chose cannot put
// anything else in an error.
func safeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 64 {
			b.WriteString("...")
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.', r == '$':
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// requestURL builds the URL: the endpoint's base, the operation's path with its
// parameters in it, and the query. It never takes anything from the document's
// `servers`.
func (a *Adapter) requestURL(op *operation, b *builder) (*url.URL, error) {
	var esc strings.Builder
	tmpl := op.path
	for {
		i := strings.IndexByte(tmpl, '{')
		j := strings.IndexByte(tmpl, '}')
		if i < 0 || j < i {
			esc.WriteString(tmpl)
			break
		}
		esc.WriteString(tmpl[:i])
		name := tmpl[i+1 : j]
		seg, ok := b.pathRaw[name]
		if !ok {
			return nil, violates("a path parameter has no value")
		}
		esc.WriteString(seg)
		tmpl = tmpl[j+1:]
	}
	base := op.ep.base
	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	u := &url.URL{Scheme: base.Scheme, Host: base.Host}
	u.RawPath = basePath + esc.String()
	decoded, err := url.PathUnescape(u.RawPath)
	if err != nil {
		return nil, violates("the request path cannot be built")
	}
	u.Path = decoded
	if len(b.query) > 0 {
		u.RawQuery = b.query.Encode()
	}
	return u, nil
}
