package capabilities

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

var t0 = time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)

// testPkg is an active (or not) application package built in memory: the policy
// view comes from policy.NewPackage, the typed view from the maps beside it. It
// is how the registry and router are tested without a signed archive.
type testPkg struct {
	*policy.Package
	hash      string
	langs     []string
	specs     map[string]ap.Capability
	contracts map[[2]string]ap.ConversationContract
	terms     []ap.TerminologyTerm
}

func (p *testPkg) ContentHash() string          { return p.hash }
func (p *testPkg) SupportedLanguages() []string { return p.langs }
func (p *testPkg) Terminology() []ap.TerminologyTerm {
	return p.terms
}
func (p *testPkg) Spec(id string) (ap.Capability, bool) { s, ok := p.specs[id]; return s, ok }
func (p *testPkg) ContractFor(c, l string) (ap.ConversationContract, bool) {
	x, ok := p.contracts[[2]string{c, l}]
	return x, ok
}

// withStatus returns the same package in another activation state.
func (p *testPkg) withStatus(s policy.PackageStatus) *testPkg {
	cp := *p
	cp.Package = p.Package.WithStatus(s)
	return &cp
}

var _ Package = (*testPkg)(nil)

type termDef struct {
	id string
	// labels maps a language to its preferred label followed by its synonyms.
	labels map[string][]string
}

type capDef struct {
	id, title, purpose string
	audience           ap.CapabilityAudience // empty means user
	effect             policy.EffectClass    // empty means none
	risk               contracts.RiskLevel
	fields             []string
	next               []ap.CapabilityNextAction
	// terms maps a language to the term IDs of that language's conversation
	// contract. A language that is absent has no contract.
	terms map[string][]string
}

type pkgDef struct {
	app    string
	hash   string
	langs  []string
	caps   []capDef
	terms  []termDef
	roles  []string
	grants map[string][]string // role -> capability IDs
	status policy.PackageStatus
}

func (d pkgDef) build(tb testing.TB) *testPkg {
	tb.Helper()
	if d.status == "" {
		d.status = policy.StatusActive
	}
	if d.hash == "" {
		d.hash = fmt.Sprintf("%064x", len(d.app)*7919+len(d.caps))
	}
	p := &testPkg{
		hash:      d.hash,
		langs:     d.langs,
		specs:     map[string]ap.Capability{},
		contracts: map[[2]string]ap.ConversationContract{},
	}
	var decls []policy.CapabilityDecl
	for _, c := range d.caps {
		aud := policy.AudienceUser
		audience := ap.CapabilityAudienceUser
		if c.audience == ap.CapabilityAudienceAdmin {
			aud, audience = policy.AudienceAdmin, ap.CapabilityAudienceAdmin
		}
		eff := c.effect
		if eff == "" {
			eff = policy.EffectNone
		}
		conf := policy.ConfirmNone
		if c.risk >= 2 {
			conf = policy.ConfirmExplicit
		}
		if c.risk >= 3 {
			conf = policy.ConfirmStepUp
		}
		var fields []policy.FieldDecl
		for _, f := range c.fields {
			fields = append(fields, policy.FieldDecl{Name: f, Required: true})
		}
		decls = append(decls, policy.CapabilityDecl{
			ID: c.id, Audience: aud, EffectClass: eff, RiskLevel: c.risk, Confirmation: conf, Fields: fields,
		})
		p.specs[c.id] = ap.Capability{
			ID: ap.CapabilityID(c.id), Audience: audience, Title: ap.Label(c.title), Purpose: ap.Text(c.purpose),
			NextActions: c.next,
			// Examples are test-only in the schema. They carry words that must never
			// become routing evidence (TestTerminologyMatchRoutes checks them).
			Examples: []ap.CapabilityExample{{ID: "e1", Language: "en", TestOnly: true, Utterance: "zzzexample" + strconv.Itoa(len(p.specs)) + "xyz"}},
		}
		for lang, ids := range c.terms {
			var slugs []ap.Slug
			for _, id := range ids {
				slugs = append(slugs, ap.Slug(id))
			}
			p.contracts[[2]string{c.id, lang}] = ap.ConversationContract{
				CapabilityID: ap.CapabilityID(c.id), Language: ap.LanguageTag(lang), Terminology: slugs,
			}
		}
	}
	for _, t := range d.terms {
		labels := map[string]ap.TerminologyLabel{}
		for lang, ws := range t.labels {
			l := ap.TerminologyLabel{Preferred: ap.Label(ws[0])}
			for _, s := range ws[1:] {
				l.Synonyms = append(l.Synonyms, ap.Label(s))
			}
			labels[lang] = l
		}
		p.terms = append(p.terms, ap.TerminologyTerm{ID: ap.Slug(t.id), Labels: labels})
	}
	sort.Slice(p.terms, func(i, j int) bool { return p.terms[i].ID < p.terms[j].ID })

	type grant struct {
		Role         string   `json:"role"`
		Capabilities []string `json:"capabilities"`
	}
	var grants []grant
	roles := append([]string(nil), d.roles...)
	sort.Strings(roles)
	declared := map[string]bool{}
	for _, c := range d.caps {
		declared[c.id] = true
	}
	for _, r := range roles {
		var caps []string
		for _, id := range d.grants[r] {
			if declared[id] { // a grant for a capability the package does not declare is a policy error
				caps = append(caps, id)
			}
		}
		if len(caps) > 0 {
			grants = append(grants, grant{Role: r, Capabilities: caps})
		}
	}
	doc, err := json.Marshal(map[string]any{
		"policy_id": "default-access", "role_grants": grants,
		"risk_overrides": []any{}, "field_projection": []any{},
	})
	if err != nil {
		tb.Fatal(err)
	}
	hp, err := policy.ParseHostPolicy(doc)
	if err != nil {
		tb.Fatalf("ParseHostPolicy: %v\n%s", err, doc)
	}
	pp, err := policy.NewPackage(policy.PackageSpec{
		ApplicationID: d.app, Version: "1.0.0", Status: d.status, Roles: d.roles, Capabilities: decls,
		Policies: []*policy.HostPolicy{hp},
	})
	if err != nil {
		tb.Fatalf("NewPackage: %v", err)
	}
	p.Package = pp
	return p
}

// ---- the demo library package ----

const (
	appLib     = "demo-library"
	tenantA    = "tenant-a"
	workspace1 = "ws-1"

	capFind    = "catalog.find_title"
	capReserve = "reservations.create"
	capExtend  = "loans.extend"
	capDelete  = "members.delete_account"
	capReindex = "admin.reindex"
)

// demoDef is the synthetic demo-library application: two languages, five
// capabilities (one of them admin), terms with English and Italian labels.
func demoDef() pkgDef {
	return pkgDef{
		app:   appLib,
		langs: []string{"en", "it"},
		roles: []string{"member", "visitor", "staff"},
		grants: map[string][]string{
			"member":  {capFind, capReserve, capExtend},
			"visitor": {capFind},
			"staff":   {capFind, capReserve, capExtend, capDelete, capReindex},
		},
		terms: []termDef{
			{"title", map[string][]string{"en": {"title", "book", "work"}, "it": {"titolo", "libro", "opera"}}},
			{"search", map[string][]string{"en": {"search", "find", "look up"}, "it": {"cerca", "trova", "ricerca"}}},
			{"availability", map[string][]string{"en": {"availability", "available"}, "it": {"disponibilità", "disponibile"}}},
			{"reservation", map[string][]string{"en": {"reservation", "reserve", "hold"}, "it": {"prenotazione", "prenota", "riserva"}}},
			{"loan", map[string][]string{"en": {"loan", "borrowed"}, "it": {"prestito", "in prestito"}}},
			{"extension", map[string][]string{"en": {"extension", "extend", "renew"}, "it": {"proroga", "prorogare", "rinnova"}}},
			{"account", map[string][]string{"en": {"account", "membership"}, "it": {"account", "iscrizione"}}},
			{"deletion", map[string][]string{"en": {"delete", "close", "remove"}, "it": {"elimina", "cancella", "chiudi"}}},
		},
		caps: []capDef{
			{
				id: capFind, title: "Find a title", purpose: "Search the catalogue for a title and say whether a copy is available.",
				fields: []string{"query", "available_only"},
				terms:  map[string][]string{"en": {"title", "search", "availability"}, "it": {"title", "search", "availability"}},
				next: []ap.CapabilityNextAction{
					{ID: "reserve_it", After: ap.CapabilityNextActionAfterSuccess, CapabilityID: capReserve},
					{ID: "extend_it", After: ap.CapabilityNextActionAfterFailure, CapabilityID: capExtend},
				},
			},
			{
				id: capReserve, title: "Reserve a title", purpose: "Reserve a title for pickup on a chosen date.",
				effect: policy.EffectExternal, risk: 2, fields: []string{"title", "pickup_date"},
				terms: map[string][]string{"en": {"title", "reservation"}, "it": {"title", "reservation"}},
			},
			{
				id: capExtend, title: "Extend a loan", purpose: "Extend the due date of a borrowed title.",
				effect: policy.EffectReversible, risk: 1, fields: []string{"loan", "weeks"},
				terms: map[string][]string{"en": {"loan", "extension"}, "it": {"loan", "extension"}},
			},
			{
				id: capDelete, title: "Delete a membership", purpose: "Close the member's account and erase the member's data.",
				effect: policy.EffectPrivileged, risk: 3, fields: []string{"member"},
				terms: map[string][]string{"en": {"account", "deletion"}, "it": {"account", "deletion"}},
			},
			{
				id: capReindex, title: "Reindex the catalogue", purpose: "Rebuild the catalogue index. Operators only.",
				audience: ap.CapabilityAudienceAdmin,
			},
		},
	}
}

func demoPackage(tb testing.TB) *testPkg { tb.Helper(); return demoDef().build(tb) }

// scopeFor returns a valid scope in the demo application for a role holding a
// token for caps (nil means every demo capability).
func scopeFor(app, role string, caps []string) contracts.Scope {
	if caps == nil {
		caps = []string{capFind, capReserve, capExtend, capDelete, capReindex}
	}
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: app, TenantID: tenantA, WorkspaceID: workspace1,
		PrincipalID: "user-1", Roles: []string{role}, CapabilityScopes: caps, SessionID: "ses-1",
	}
}

func memberScope() contracts.Scope { return scopeFor(appLib, "member", nil) }

// reqFor builds a Request for the scope with its own binding.
func reqFor(s contracts.Scope, text string, mods ...func(*Request)) Request {
	r := Request{Scope: s, Binding: policy.BindingOf(s), Utterance: text}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func inLanguage(l string) func(*Request) { return func(r *Request) { r.Language = l } }
func withGoal(id, phase string) func(*Request) {
	return func(r *Request) { r.Goal = &ActiveGoal{CapabilityID: id, Phase: phase} }
}

// newRouter returns a router over a registry holding the packages, with every
// decision recorded by a gateway on a fake clock.
func newRouter(tb testing.TB, opts []RouterOption, pkgs ...Package) (*Router, *Registry) {
	tb.Helper()
	reg := NewRegistry(nil, WithClock(clock.NewFake(t0)))
	for _, p := range pkgs {
		if err := reg.Activate(p); err != nil {
			tb.Fatalf("Activate: %v", err)
		}
	}
	rt, err := NewRouter(reg, policy.NewGateway(clock.NewFake(t0)), opts...)
	if err != nil {
		tb.Fatal(err)
	}
	return rt, reg
}

func mustRoute(tb testing.TB, rt *Router, req Request) Result {
	tb.Helper()
	res, err := rt.Route(context.Background(), req)
	if err != nil {
		tb.Fatalf("Route(%q): %v", req.Utterance, err)
	}
	return res
}

func (r Result) has(id string) bool {
	for _, c := range r.Candidates {
		if c.CapabilityID == id {
			return true
		}
	}
	return false
}

func (r Result) candidate(id string) (Candidate, bool) {
	for _, c := range r.Candidates {
		if c.CapabilityID == id {
			return c, true
		}
	}
	return Candidate{}, false
}

func (c Candidate) signal(kind SignalKind) (Signal, bool) {
	for _, s := range c.Signals {
		if s.Kind == kind {
			return s, true
		}
	}
	return Signal{}, false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
