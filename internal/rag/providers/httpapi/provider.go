package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Provider is the document API source provider. It is stateless and safe for
// concurrent use: every call builds its own HTTP client from the egress Dialer
// of the source's application and releases it when the call (or, for Open, the
// returned reader) ends.
type Provider struct {
	cfg   Config
	clk   clock.Clock
	log   *slog.Logger
	paths source.Paths
}

var _ source.Provider = (*Provider)(nil)

// New returns a Provider. It is validation_failed for a configuration the
// provider refuses (a malformed parameter or header name, a negative limit).
func New(cfg Config) (*Provider, error) {
	c, err := cfg.normalise()
	if err != nil {
		return nil, err
	}
	// The POSIX flavour on every host: an API item key is a URL path, not a
	// file name, so Windows reserved names and colons are legitimate in it and
	// the answer must not depend on which OS the daemon runs on.
	return &Provider{cfg: c, clk: c.Clock, log: c.Logger, paths: source.Paths{}}, nil
}

// Kinds implements source.Provider.
func (p *Provider) Kinds() []ap.RAGSourceType {
	return []ap.RAGSourceType{ap.RAGSourceTypeApplicationDocumentAPI, ap.RAGSourceTypeHTTPAPI}
}

func interrupted(err error) error {
	return contracts.Wrap(contracts.ErrTimeout, err, "the operation was interrupted")
}

func unavailable(msg string, cause error) *contracts.Error {
	return contracts.Wrap(contracts.ErrUnavailable, cause, msg)
}

func invalid(format string, args ...any) *contracts.Error {
	return contracts.Newf(contracts.ErrValidationFailed, format, args...)
}

// asContractsError returns err as a typed error, wrapping an untyped one as
// unavailable (never as something that looks like success).
func asContractsError(err error, msg string) *contracts.Error {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		return ce
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, msg)
}

// rootInfo is the validated root of an API source: the origin and the path
// below which every request is made.
type rootInfo struct {
	scheme string
	host   string // host[:port], as in the URL
	path   string // no trailing slash; "" for the bare origin
}

func (r rootInfo) origin() origin { return originOf(&url.URL{Scheme: r.scheme, Host: r.host}) }

// listURL is the URL of the list endpoint.
func (r rootInfo) listURL(q url.Values) string {
	u := url.URL{Scheme: r.scheme, Host: r.host, Path: r.path, RawQuery: q.Encode()}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// itemURL is the URL of one item. The key was validated as a clean relative
// path; url.URL escapes every segment, so a "?", "#" or "%" in it stays in the
// path.
func (r rootInfo) itemURL(key string, q url.Values) string {
	u := url.URL{Scheme: r.scheme, Host: r.host, Path: r.path + "/" + key, RawQuery: q.Encode()}
	return u.String()
}

// resolveRoot validates the declared root and returns where requests go.
func (p *Provider) resolveRoot(def source.Definition) (rootInfo, error) {
	api, ok := def.Decl.Root.Value.(ap.RAGSourceRootAPI)
	if !ok {
		return rootInfo{}, invalid("the source declares no api root")
	}
	var raw string
	switch {
	case api.URL != nil && api.Endpoint == nil:
		raw = string(*api.URL)
	case api.Endpoint != nil && api.URL == nil:
		if p.cfg.Endpoints == nil {
			return rootInfo{}, invalid("the api root names a manifest endpoint and no endpoint resolver is configured")
		}
		base, ok := p.cfg.Endpoints(def.ApplicationID, string(*api.Endpoint))
		if !ok || base == "" {
			return rootInfo{}, invalid("the api root's endpoint is not known")
		}
		raw = base
	default:
		return rootInfo{}, invalid("an api root names exactly one of endpoint and url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return rootInfo{}, invalid("the api root URL is not a valid URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return rootInfo{}, invalid("the api root URL must be http or https")
	case u.Host == "" || u.Hostname() == "":
		return rootInfo{}, invalid("the api root URL has no host")
	case u.User != nil:
		return rootInfo{}, invalid("the api root URL must not carry user information")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return rootInfo{}, invalid("the api root URL must not carry a query or a fragment")
	case u.RawPath != "":
		return rootInfo{}, invalid("the api root URL path must not use percent-encoding")
	}
	base := strings.TrimRight(u.Path, "/")
	if strings.Contains(base, "//") {
		return rootInfo{}, invalid("the api root URL path has an empty segment")
	}
	if api.BasePath != nil {
		bp := strings.TrimRight(*api.BasePath, "/")
		if bp != "" && (bp[0] != '/' || strings.ContainsAny(bp, "?#%\\") || strings.IndexFunc(bp, func(r rune) bool { return r <= 0x20 || r == 0x7f }) >= 0) {
			return rootInfo{}, invalid("the api root's base_path is not a plain absolute path")
		}
		base += bp
	}
	for _, seg := range strings.Split(strings.TrimPrefix(base, "/"), "/") {
		if base != "" && (seg == "" || seg == "." || seg == "..") {
			return rootInfo{}, invalid("the api root's path has an empty, \".\" or \"..\" segment")
		}
	}
	return rootInfo{scheme: u.Scheme, host: u.Host, path: base}, nil
}

// sentNames lists the query parameter names the provider sends on a request of
// this source, for the query_scope whitelist.
func (p *Provider) sentNames(def source.Definition) []string {
	n := []string{p.cfg.Params.Limit, p.cfg.Params.Cursor, p.cfg.Params.Application}
	if def.Tenant.Mode != ap.RAGSourceScopeBindingModeShared {
		n = append(n, p.cfg.Params.Tenant)
	}
	if def.Workspace.Mode != ap.RAGSourceScopeBindingModeShared {
		n = append(n, p.cfg.Params.Workspace)
	}
	return n
}

// checkQueryScope applies the declaration's query_scope: a fixed parameter may
// not reuse a name the provider owns (it would override the scope), and when a
// query_scope is declared every name the provider sends must be whitelisted.
func (p *Provider) checkQueryScope(def source.Definition) (fixed url.Values, err error) {
	qs := def.Decl.QueryScope
	fixed = url.Values{}
	if qs == nil {
		return fixed, nil
	}
	reserved := map[string]bool{}
	for _, n := range p.cfg.Params.all() {
		reserved[strings.ToLower(n)] = true
	}
	for name, value := range qs.FixedParameters {
		if !paramNameRE.MatchString(name) {
			return nil, invalid("a fixed query parameter has a malformed name")
		}
		if reserved[strings.ToLower(name)] {
			return nil, invalid("the fixed query parameter %q is one the provider sends itself (pagination or scope)", name)
		}
		fixed.Set(name, value)
	}
	allowed := map[string]bool{}
	for _, n := range qs.AllowedParameters {
		allowed[n] = true
	}
	for _, n := range p.sentNames(def) {
		if !allowed[n] {
			return nil, invalid("query_scope.allowed_parameters does not list %q, which the provider always sends", n)
		}
	}
	return fixed, nil
}

// session is the state of one call: the validated definition, the verified
// scope, the matcher and the HTTP client bound to the application's egress
// allowlist and the source's credential.
type session struct {
	p      *Provider
	def    source.Definition
	scope  contracts.Scope
	m      *source.Matcher
	root   rootInfo
	fixed  url.Values
	client *http.Client
}

// begin checks what every call needs: a live context, a scope the source's
// binding admits, a definition this provider can serve, and a source that is
// allowed to touch the network at all. Nothing is sent before every check has
// passed.
func (p *Provider) begin(ctx context.Context, scope contracts.Scope, def source.Definition) (*session, error) {
	if err := ctx.Err(); err != nil {
		return nil, interrupted(err)
	}
	if err := def.CheckScope(scope); err != nil {
		return nil, err
	}
	switch def.Type {
	case ap.RAGSourceTypeApplicationDocumentAPI, ap.RAGSourceTypeHTTPAPI:
	default:
		return nil, invalid("the HTTP API provider serves application_document_api and http_api sources, not %q", string(def.Type))
	}
	if def.Root.Kind != source.RootAPI {
		return nil, invalid("the HTTP API provider serves api roots, not %q", def.Root.Kind)
	}
	root, err := p.resolveRoot(def)
	if err != nil {
		return nil, err
	}
	if len(def.Decl.Egress.Entries) == 0 {
		// An empty list means the source makes no network connection. Refuse
		// before any dialing, with the egress gate's own typed denial inside.
		denial := (*egress.Allowlist)(nil).CheckURL(&url.URL{Scheme: root.scheme, Host: root.host})
		return nil, contracts.Wrap(contracts.ErrPermissionDenied, denial, "the source declares no egress entries, so it makes no network connection")
	}
	m, err := source.NewMatcher(def.Include, def.Exclude, false)
	if err != nil {
		return nil, err
	}
	fixed, err := p.checkQueryScope(def)
	if err != nil {
		return nil, err
	}
	s := &session{p: p, def: def, scope: scope, m: m, root: root, fixed: fixed}
	s.client = p.newClient(s)
	return s, nil
}

// close releases the idle connections of the call.
func (s *session) close() {
	if s != nil && s.client != nil {
		s.client.CloseIdleConnections()
	}
}

// query returns the parameters every request carries: the declaration's fixed
// parameters and the verified scope (see the package comment).
func (s *session) query() url.Values {
	q := url.Values{}
	for k, v := range s.fixed {
		q[k] = append([]string(nil), v...)
	}
	n := s.p.cfg.Params
	q.Set(n.Application, s.scope.ApplicationID)
	if s.def.Tenant.Mode != ap.RAGSourceScopeBindingModeShared {
		q.Set(n.Tenant, s.scope.TenantID)
	}
	if s.def.Workspace.Mode != ap.RAGSourceScopeBindingModeShared {
		q.Set(n.Workspace, s.scope.WorkspaceID)
	}
	return q
}

// eligible validates an item key from the caller or the API and applies the
// include and exclude rules.
func (s *session) eligible(key string) error {
	if err := s.p.paths.ValidKey(key); err != nil {
		return err
	}
	if !s.m.Eligible(key) {
		return fmt.Errorf("%w: %q", source.ErrNotAdmitted, key)
	}
	return nil
}

// Probe implements source.Provider.
func (p *Provider) Probe(ctx context.Context, scope contracts.Scope, def source.Definition) (source.Health, error) {
	s, err := p.begin(ctx, scope, def)
	if err != nil {
		return source.Health{}, err
	}
	defer s.close()
	if _, err := s.listPage(ctx, "", 1); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return source.Health{}, interrupted(cerr)
		}
		return source.Health{Availability: source.Unavailable, Err: asContractsError(err, "the API cannot be reached")}, nil
	}
	return source.Health{Availability: source.Available, CheckedAt: p.clk.Now()}, nil
}
