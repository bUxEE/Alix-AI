package openapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"

	"github.com/buxee/alix-ai/internal/adapters"
)

// role is what an approved operation is used for. It decides which methods the
// operation may have and where the values of a call go.
type role string

const (
	roleRead      role = "read"
	roleValidate  role = "validate"
	rolePrepare   role = "prepare"
	roleCommit    role = "commit"
	roleStatus    role = "status"
	roleCancel    role = "cancel"
	roleResolve   role = "resolve"
	roleDescribe  role = "describe"
	roleHealth    role = "health"
	roleInspect   role = "inspect"
	roleProvision role = "provisioning"
)

// roleOf maps an adapter operation to its role.
func roleOf(op adapters.Operation) role { return role(op) }

// allowedMethods is the method rule of each role. An operation that only reads
// is GET or POST (a search with a body is a read); one that changes or stages
// something is never GET, which caches, prefetchers and crawlers may repeat; a
// read is never PUT, PATCH or DELETE.
func allowedMethods(r role) []string {
	switch r {
	case roleValidate, rolePrepare:
		return []string{http.MethodPost, http.MethodPut, http.MethodPatch}
	case roleCommit, roleCancel:
		return []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	case roleProvision:
		return []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	}
	return []string{http.MethodGet, http.MethodPost}
}

// endpoint is one API with its loaded document.
type endpoint struct {
	cfg    Endpoint
	base   *url.URL
	origin origin
	doc    *openapi3.T
	client *http.Client

	// ops holds every operation of the document that has an operationId.
	ops map[string]*operation
	// anonymous lists "METHOD /path" for operations that have no operationId:
	// they cannot be mapped, and the audit is told they exist.
	anonymous []string
	// approved is the set of operationIds the package maps on this endpoint
	// (capabilities and management).
	approved map[string]bool
}

// param is one parameter an operation declares, with what the adapter needs to
// serialise a value for it.
type param struct {
	p       *openapi3.Parameter
	in      string
	explode bool
	array   bool
}

// operation is one operation of a document, with what the adapter needs to
// build a request for it. It is built only for approved operations.
type operation struct {
	ep     *endpoint
	id     string
	method string
	path   string
	op     *openapi3.Operation
	route  *routers.Route

	// params indexes the parameters by name (header names lower-cased).
	params map[string]*param
	// order lists the parameter keys in document order.
	order []string
	// body is the JSON request body the operation declares, nil when it declares
	// none.
	body         *openapi3.MediaType
	bodyRequired bool
	// authDeclared is true when the operation declares a header parameter for
	// the credential header.
	authDeclared bool
}

// route is one mapped operation: the operation, the binding's timeout and its
// argument renames.
type route struct {
	op      *operation
	role    role
	timeout time.Duration // 0 leaves the caller's deadline
	args    map[string]string
}

// loadDoc loads and validates an OpenAPI 3.x document. It loads nothing but the
// document itself: an external $ref (a file or a URL) is an error, and the
// document's `servers` and `security` are never consulted.
func loadDoc(raw []byte) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = false
	doc, err := loader.LoadFromData(raw)
	if err != nil {
		return nil, errors.New("the OpenAPI document does not load: " + reasonOf(err))
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		return nil, errors.New("the document is not an OpenAPI 3.x document")
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, errors.New("the OpenAPI document is invalid: " + reasonOf(err))
	}
	return doc, nil
}

// reasonOf keeps a library error short.
func reasonOf(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 200)
}

// newEndpoint loads the document of an endpoint and indexes its operations.
func newEndpoint(cfg Endpoint) (*endpoint, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, cfgErr("endpoint %q: %v", cfg.ID, err)
	}
	doc, err := loadDoc(cfg.Spec)
	if err != nil {
		return nil, cfgErr("endpoint %q: %v", cfg.ID, err)
	}
	ep := &endpoint{cfg: cfg, base: base, origin: originOf(base), doc: doc, ops: map[string]*operation{}, approved: map[string]bool{}}
	if doc.Paths == nil {
		return ep, nil
	}
	paths := doc.Paths.Map()
	keys := make([]string, 0, len(paths))
	for p := range paths {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, path := range keys {
		item := paths[path]
		if item == nil {
			continue
		}
		methods := item.Operations()
		ms := make([]string, 0, len(methods))
		for m := range methods {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		for _, m := range ms {
			op := methods[m]
			if op.OperationID == "" {
				ep.anonymous = append(ep.anonymous, m+" "+path)
				continue
			}
			if _, dup := ep.ops[op.OperationID]; dup {
				return nil, cfgErr("endpoint %q: the operationId %q is used twice", cfg.ID, truncate(op.OperationID, 40))
			}
			ep.ops[op.OperationID] = &operation{
				ep: ep, id: op.OperationID, method: strings.ToUpper(m), path: path, op: op,
				route: &routers.Route{Spec: doc, Path: path, PathItem: item, Method: strings.ToUpper(m), Operation: op},
			}
		}
	}
	return ep, nil
}

// prepare checks that the operation can be used for a role and computes what
// building a request needs. It is called for every mapping of the operation and
// is idempotent.
func (o *operation) prepare(r role) error {
	fail := func(format string, args ...any) error {
		return cfgErr("endpoint %q: operation %q: %s", o.ep.cfg.ID, truncate(o.id, 40), fmt.Sprintf(format, args...))
	}
	allowed := false
	for _, m := range allowedMethods(r) {
		allowed = allowed || m == o.method
	}
	if !allowed {
		return fail("a %s operation cannot use the method %s", r, o.method)
	}
	if o.params == nil {
		if err := o.indexParams(fail); err != nil {
			return err
		}
	}
	if err := o.checkResponses(fail); err != nil {
		return err
	}
	return nil
}

// indexParams merges the path item's parameters with the operation's (the
// operation's win) and checks that every one can be serialised.
func (o *operation) indexParams(fail func(string, ...any) error) error {
	o.params = map[string]*param{}
	seen := map[string]bool{}
	var all []*openapi3.Parameter
	for _, ref := range o.route.PathItem.Parameters {
		if ref == nil || ref.Value == nil {
			continue
		}
		overridden := false
		for _, oref := range o.op.Parameters {
			if oref != nil && oref.Value != nil && oref.Value.In == ref.Value.In && oref.Value.Name == ref.Value.Name {
				overridden = true
			}
		}
		if !overridden {
			all = append(all, ref.Value)
		}
	}
	for _, ref := range o.op.Parameters {
		if ref != nil && ref.Value != nil {
			all = append(all, ref.Value)
		}
	}
	for _, p := range all {
		key := p.Name
		switch p.In {
		case openapi3.ParameterInHeader:
			key = strings.ToLower(p.Name)
			if strings.EqualFold(p.Name, o.ep.cfg.Settings.authHeader()) {
				o.authDeclared = true
			}
		case openapi3.ParameterInPath, openapi3.ParameterInQuery:
		default:
			return fail("it declares the %s parameter %q; only path, query and header parameters are supported", p.In, truncate(p.Name, 40))
		}
		if seen[key] {
			return fail("the parameter name %q is declared in more than one place", truncate(p.Name, 40))
		}
		seen[key] = true
		if len(p.Content) > 0 {
			return fail("the parameter %q is encoded as a media type, which is not supported", truncate(p.Name, 40))
		}
		style := p.Style
		switch p.In {
		case openapi3.ParameterInQuery:
			if style != "" && style != "form" {
				return fail("the query parameter %q uses the style %q; only form is supported", truncate(p.Name, 40), truncate(style, 20))
			}
		default:
			if style != "" && style != "simple" {
				return fail("the %s parameter %q uses the style %q; only simple is supported", p.In, truncate(p.Name, 40), truncate(style, 20))
			}
		}
		pr := &param{p: p, in: p.In, explode: p.In == openapi3.ParameterInQuery}
		if p.Explode != nil {
			pr.explode = *p.Explode
		}
		if p.Schema != nil && p.Schema.Value != nil {
			t := p.Schema.Value.Type
			switch {
			case t.Includes("object"):
				return fail("the parameter %q is an object, which is not supported", truncate(p.Name, 40))
			case t.Includes("array"):
				pr.array = true
				if it := p.Schema.Value.Items; it != nil && it.Value != nil && (it.Value.Type.Includes("object") || it.Value.Type.Includes("array")) {
					return fail("the parameter %q is a list of objects or lists, which is not supported", truncate(p.Name, 40))
				}
			}
		}
		o.params[key] = pr
		o.order = append(o.order, key)
	}
	if rb := o.op.RequestBody; rb != nil && rb.Value != nil {
		mt := rb.Value.Content["application/json"]
		if mt == nil || mt.Schema == nil {
			return fail("its request body is not described as application/json with a schema")
		}
		o.body, o.bodyRequired = mt, rb.Value.Required
	}
	// Every {name} of the path has a path parameter.
	for _, name := range pathNames(o.path) {
		if p := o.params[name]; p == nil || p.in != openapi3.ParameterInPath {
			return fail("the path names {%s} and no path parameter declares it", truncate(name, 40))
		}
	}
	return nil
}

// checkResponses requires a described JSON result for at least one successful
// status: an operation whose success cannot be checked against the document
// would be used best-effort, and that is not allowed.
func (o *operation) checkResponses(fail func(string, ...any) error) error {
	if o.op.Responses == nil {
		return fail("it declares no responses")
	}
	for code, ref := range o.op.Responses.Map() {
		if ref == nil || ref.Value == nil || !(strings.HasPrefix(code, "2")) {
			continue
		}
		if mt := ref.Value.Content["application/json"]; mt != nil && mt.Schema != nil {
			return nil
		}
	}
	return fail("none of its 2xx responses is described as application/json with a schema")
}

// pathNames lists the {name} segments of a path template.
func pathNames(tmpl string) []string {
	var out []string
	for {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			return out
		}
		out = append(out, tmpl[i+1:i+j])
		tmpl = tmpl[i+j+1:]
	}
}

// responseFor finds the response the document describes for a status: the exact
// code, then its range (2XX), then default.
func (o *operation) responseFor(status int) *openapi3.Response {
	rs := o.op.Responses
	if rs == nil {
		return nil
	}
	if r := rs.Status(status); r != nil && r.Value != nil {
		return r.Value
	}
	if r := rs.Default(); r != nil && r.Value != nil {
		return r.Value
	}
	return nil
}

// bodyDeclares reports whether the request body schema lists a property.
func (o *operation) bodyDeclares(name string) bool {
	if o.body == nil || o.body.Schema == nil || o.body.Schema.Value == nil {
		return false
	}
	var has func(s *openapi3.Schema, depth int) bool
	has = func(s *openapi3.Schema, depth int) bool {
		if s == nil || depth > 4 {
			return false
		}
		if _, ok := s.Properties[name]; ok {
			return true
		}
		for _, sub := range s.AllOf {
			if sub != nil && has(sub.Value, depth+1) {
				return true
			}
		}
		return false
	}
	return has(o.body.Schema.Value, 0)
}

// param finds the parameter a field name belongs to. Header parameters the
// protocol owns (the credential, the identity, the idempotency key, Content-Type
// and the like) are never found: no field can fill them.
func (o *operation) param(name string) *param {
	if p := o.params[name]; p != nil && p.in != openapi3.ParameterInHeader {
		return p
	}
	k := strings.ToLower(name)
	if p := o.params[k]; p != nil && p.in == openapi3.ParameterInHeader && !o.ep.cfg.Settings.protocolHeader(name) {
		return p
	}
	return nil
}

// unmappedName is how the audit is told about an operation nobody maps: its
// operationId, as plain printable text.
func unmappedName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 120 {
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
