package apppkg

import (
	"strings"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// checkSources implements section 14 item 15 and master plan 4.5 check 3. The
// schema already makes every field of master plan 12.2 structurally required,
// so a source with no owner or no scope never reaches this point (it is a
// schema.rag_source finding); what is checked here is whether the declarations
// agree with the manifest.
func (md *model) checkSources() {
	for _, id := range md.sourceIDs() {
		e := md.sources[id]
		s, path := e.s, e.path
		add := func(ch *check, format string, args ...any) { md.c.add(ch, "", path, format, args...) }

		if string(s.Scope.ApplicationID) != string(md.m.Application.ID) {
			add(chkRAGScope, "source %s has scope.application_id %s, but the manifest's application.id is %s",
				q(id), q(string(s.Scope.ApplicationID)), q(string(md.m.Application.ID)))
		}

		// the egress entries this source may use
		own := map[string]bool{}
		for _, ref := range s.Egress.Entries {
			if _, ok := md.m.Network.Egress[string(ref)]; !ok {
				add(chkRAGEgress, "source %s lists egress entry %s, which network.egress does not declare", q(id), q(string(ref)))
				continue
			}
			own[string(ref)] = true
		}

		switch r := s.Root.Value.(type) {
		case ap.RAGSourceRootAPI:
			md.checkAPIRoot(id, path, r, own)
		case ap.RAGSourceRootPackage:
			if p := r.Path; !md.packageFolderHasFiles(p) {
				add(chkRAGRoot, "source %s has a package root %s that is not a folder below documentation/ holding files", q(id), q(p))
			}
		}

		if ev := s.Sync.Event; ev != nil {
			switch tr, ok := md.transportOf(string(ev.Endpoint)); {
			case !ok:
				add(chkRAGSync, "source %s: sync.event names endpoint %s, which the manifest does not declare", q(id), q(string(ev.Endpoint)))
			case tr != "events":
				add(chkRAGSync, "source %s: sync.event names endpoint %s, which is a %s endpoint, not an events endpoint", q(id), q(string(ev.Endpoint)), tr)
			default:
				ee, _ := md.m.Endpoints[string(ev.Endpoint)].Value.(ap.ManifestEventsEndpoint)
				if _, ok := ee.EventTypes[string(ev.EventType)]; !ok {
					add(chkRAGSync, "source %s: endpoint %s declares no event type %s", q(id), q(string(ev.Endpoint)), q(string(ev.EventType)))
				}
			}
		}

		if c, ok := s.Citation.Value.(ap.RAGSourceCitationAdapterOperation); ok {
			md.checkCall(path, "", "source "+q(id)+" citation", c.Call)
		}
	}
}

// checkAPIRoot checks an api root: it names a declared mcp or openapi endpoint,
// or a URL whose host and port one of the source's own egress entries allows.
func (md *model) checkAPIRoot(id, path string, r ap.RAGSourceRootAPI, own map[string]bool) {
	if r.Endpoint != nil {
		ep := string(*r.Endpoint)
		switch tr, ok := md.transportOf(ep); {
		case !ok:
			md.c.add(chkRAGRoot, "", path, "source %s: root names endpoint %s, which the manifest does not declare", q(id), q(ep))
		case tr != "mcp" && tr != "openapi":
			md.c.add(chkRAGRoot, "", path, "source %s: root names endpoint %s, which is a %s endpoint; a source reads through an mcp or openapi endpoint", q(id), q(ep), tr)
		}
	}
	if r.URL != nil {
		if !md.egressAllows(string(*r.URL), md.m.Network.Egress, own) {
			md.c.add(chkRAGRoot, "", path, "source %s: root URL %s has no matching entry in the source's own egress list (exact host, port and scheme)", q(id), q(string(*r.URL)))
		}
	}
	if r.Endpoint == nil && r.URL == nil {
		md.c.add(chkRAGRoot, "", path, "source %s: an api root needs an endpoint or a url", q(id))
	}
}

// packageFolderHasFiles reports whether p names a folder below documentation/
// that holds at least one file of the package. A package root is read from the
// signed package itself, so it can never reach outside it.
func (md *model) packageFolderHasFiles(p string) bool {
	p = strings.TrimSuffix(p, "/")
	if !strings.HasPrefix(p, "documentation/") {
		return false
	}
	prefix := p + "/"
	for _, f := range md.snap.Paths() {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

// checkCall checks an adapter call used outside a capability (a citation
// resolver, a test provisioning call) against the manifest: the endpoint must
// exist and the call key must fit its transport.
func (md *model) checkCall(path, capability, what string, c ap.AdapterCall) {
	p := md.callProblem(c)
	if p == "" {
		return
	}
	if _, ok := md.transportOf(string(c.Endpoint)); !ok {
		md.c.add(chkEndpointUnknown, capability, path, "%s: %s", what, p)
		return
	}
	md.c.add(chkEndpointTransport, capability, path, "%s: %s", what, p)
}
