package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	ragc "github.com/buxee/alix-ai/pkg/contracts/gen/rag"

	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// The lab is the core's own retrieval stack in a throwaway store: the real lexical index,
// the real query pipeline, the real answer step and the real citation resolver, over
// synthetic documents that the audit writes itself. It exists only for one check run, in a
// directory that is removed afterwards, and it never touches the application: nothing in
// it is a record, a file or a tenant of the application, and nothing reaches the
// application's adapter.
//
// What the lab stands in for, and what it does not:
//
//   - The documents are the audit's own (one short synthetic passage per source and scope,
//     each with a marker no other passage holds). The audit has no access to the
//     application's real documents, so what it proves is how AlixAI's retrieval and
//     citation machinery treats the sources the package declares, not what the
//     application's own documents say.
//   - They are written into the index through the index's own generation protocol
//     (begin, put, validate, activate), as ingestion does, not through the ingestion
//     queue: no job, no pool and no parser worker process is started.
//   - The source catalog is the audit's own small stand-in for the deployment's
//     deterministic authority on sources (the composition root's registry and policy). It
//     admits a source to the scope it is registered in and to no other, and it can revoke
//     and remove sources on request. What the probes exercise is the index's scope
//     enforcement, the query pipeline, the answer step and the resolver beneath it.
//   - The proposer of claims is the extractive stand-in (standin.go), not a model.

// labSource is a declared source the lab can host.
type labSource struct {
	id   string
	decl ap.RAGSource
}

// labDoc is the one synthetic document of a registration.
type labDoc struct {
	id      string
	text    string
	marker  string
	version string
}

// labPair is a tenant and workspace of the suite's fixtures: one index scope.
type labPair struct {
	tenantRef, workspaceRef string
	scope                   contracts.Scope // the scope index writes are made under
}

func (p *labPair) label() string { return p.tenantRef + "/" + p.workspaceRef }

// labPrincipal is a fixture principal as the lab knows it.
type labPrincipal struct {
	ref   string
	scope contracts.Scope
	pair  *labPair
}

// labReg is one declared source registered in one scope, with its document.
type labReg struct {
	source   string // the declared slug
	title    string // the display name the catalog gives
	pair     *labPair
	srcID    string // the core-minted source id: never reported
	def      source.Definition
	doc      labDoc
	genID    string
	children []string // chunk ids of the document: never reported
}

func (r *labReg) scopeKey() string { return keyOf(r.pair.scope) }

func keyOf(s contracts.Scope) string {
	return s.ApplicationID + "\x00" + s.TenantID + "\x00" + s.WorkspaceID
}

// lab is one throwaway RAG stack over the fixtures of one test suite.
type lab struct {
	opts Options
	dir  string
	db   *store.DB
	clk  *clock.Fake

	ix   *index.Index
	chk  *chunk.Chunker
	cat  *labCatalog
	gen  *extractive
	res  *answer.Resolver
	cfg  answer.Config
	retr Retriever
	answ Answerer
	resv Resolver

	suite      string
	app        string
	suffix     string
	principals map[string]*labPrincipal
	pairs      []*labPair
	regs       []*labReg
	// unhosted holds, by source slug, why the core would not build the source.
	unhosted map[string]string

	closeOnce sync.Once
	closeErr  error
}

var labCounter atomic.Uint64

// newSuffix keeps the identifiers of two labs made in the same instant apart. It never
// appears in a report.
func newSuffix(app string, clk clock.Clock) string {
	n := labCounter.Add(1)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", app, clk.Now().UnixNano(), n)))
	return hex.EncodeToString(sum[:])[:10]
}

func slug(ref string) string { return strings.ReplaceAll(ref, "-", "_") }

// letters renders bytes as lower-case letters only, so a marker is a plain word of the
// full-text index (no digit, so it is not read as an identifier either).
func letters(b []byte, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n && i < len(b); i++ {
		out = append(out, 'a'+b[i]%26)
	}
	return string(out)
}

// markerOf is the marker of a registration's passage: unique to the source and scope in
// this lab, and unlike anything in another lab.
func markerOf(suffix, source string, pair *labPair) string {
	sum := sha256.Sum256([]byte("marker|" + suffix + "|" + source + "|" + pair.tenantRef + "|" + pair.workspaceRef))
	return "mk" + letters(sum[:], 10)
}

// synthetic builds the passage of a registration. Its first sentence is what the
// extractive stand-in proposes as a claim; none of the wording is an instruction.
func synthetic(marker string) labDoc {
	text := "# Synthetic audit passage\n\n" +
		"Passage " + marker + " is a synthetic document that the integration audit wrote for this scope. " +
		"The audit retrieves it to prove that retrieval keeps every scope apart. " +
		"Every citation of it must resolve to this original document.\n"
	sum := sha256.Sum256([]byte(text))
	return labDoc{id: "audit-probe.md", text: text, marker: marker, version: "sha256:" + hex.EncodeToString(sum[:])}
}

// hostableTypes are the source types whose declarations the lab can host: those a folder
// provider serves. Their declared root is never touched; the lab replaces it.
var hostableTypes = map[ap.RAGSourceType]bool{
	ap.RAGSourceTypeLocalFolder:     true,
	ap.RAGSourceTypeMountedVolume:   true,
	ap.RAGSourceTypeExportedDataset: true,
}

// labSources splits the declared sources into those the lab can host and, for the rest, the
// reason it cannot.
func labSources(pkg *apppkg.Package) (hosted []labSource, reasons map[string]string) {
	reasons = map[string]string{}
	for _, id := range sortedKeys(pkg.Sources) {
		decl := pkg.Sources[id]
		switch {
		case hostableTypes[decl.Type] && (decl.Scope.Tenant.Mode == ap.RAGSourceScopeBindingModeFixed || decl.Scope.Workspace.Mode == ap.RAGSourceScopeBindingModeFixed):
			reasons[id] = "the source is bound to a fixed tenant or workspace of the deployment, which a disposable tenant cannot be"
		case hostableTypes[decl.Type]:
			hosted = append(hosted, labSource{id: id, decl: decl})
		case decl.Type == ap.RAGSourceTypeDocumentationPackage:
			reasons[id] = "the documents of a source shipped inside the package are not available to the audit, and no provider that serves them is part of this build"
		default:
			reasons[id] = "no provider for sources read through an API is part of this build"
		}
	}
	return hosted, reasons
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// labDefinition builds the definition of a declared source for one scope, the way the
// registration of a source does (a request-bound level takes the scope's tenant or
// workspace, a shared level none, a fixed level the declaration's own), over a root that
// belongs to the lab. The type is read as a folder source because the lab's root is one;
// nothing else of the declaration is changed.
func labDefinition(decl ap.RAGSource, tenantID, workspaceID, root string) (source.Definition, error) {
	d := decl
	d.Type = ap.RAGSourceTypeLocalFolder
	d.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}}
	b := source.Binding{SourceID: ids.New("src")}
	if decl.Scope.Tenant.Mode == ap.RAGSourceScopeBindingModeRequest {
		b.TenantID = tenantID
	}
	if decl.Scope.Workspace.Mode == ap.RAGSourceScopeBindingModeRequest {
		b.WorkspaceID = workspaceID
	}
	return source.FromDeclaration(d, b)
}

// newLab builds the lab for one test suite: the fixture principals' scopes, one
// registration of every hostable source in every scope, and its passage in the index.
func newLab(ctx context.Context, o Options, in *integrationaudit.Input, suite ap.PackageTests, srcs []labSource) (*lab, error) {
	var err error
	l := &lab{
		opts: o, suite: string(suite.Suite), app: in.Scope.ApplicationID,
		principals: map[string]*labPrincipal{}, unhosted: map[string]string{},
	}
	built := false
	defer func() {
		// A lab that did not come up is not left behind.
		if !built {
			_ = l.close()
		}
	}()
	if l.dir, err = os.MkdirTemp(o.ScratchDir, "alixai-audit-rag-*"); err != nil {
		return nil, errors.New("the throwaway directory could not be created")
	}
	start := in.Clock.Now()
	l.clk = clock.NewFake(start)
	l.suffix = newSuffix(l.app, l.clk)

	reg := store.NewRegistry()
	if err = reg.Register(index.Module, index.Migrations()); err != nil {
		return nil, errors.New("the index migrations could not be registered")
	}
	if l.db, err = store.Open(ctx, store.Options{Path: filepath.Join(l.dir, "rag.db"), Migrations: reg, Clock: l.clk}); err != nil {
		return nil, errors.New("the throwaway store could not be opened")
	}
	if l.ix, err = index.New(l.db, l.clk, index.Config{}); err != nil {
		return nil, errors.New("the throwaway index could not be built")
	}
	if l.chk, err = chunk.New(chunk.DefaultConfig()); err != nil {
		return nil, errors.New("the chunker could not be built")
	}
	l.cat = newLabCatalog()
	pipe, err := query.New(query.Deps{Lexical: l.ix, Catalog: l.cat, Hits: l.ix, Clock: l.clk}, query.DefaultConfig())
	if err != nil {
		return nil, errors.New("the query pipeline could not be built")
	}
	l.cfg = answer.DefaultConfig()
	if l.res, err = answer.NewResolver(l.cat, l.clk, l.cfg); err != nil {
		return nil, errors.New("the citation resolver could not be built")
	}
	l.gen = &extractive{}
	ans, err := answer.New(answer.Deps{Generator: l.gen, Catalog: l.cat, Resolver: l.res, Clock: l.clk}, l.cfg)
	if err != nil {
		return nil, errors.New("the answer step could not be built")
	}
	l.retr, l.answ, l.resv = pipe, ans, l.res
	if h := o.Hooks.Retrieve; h != nil {
		l.retr = h(l.retr)
	}
	if h := o.Hooks.Answer; h != nil {
		l.answ = h(l.answ)
	}
	if h := o.Hooks.Resolve; h != nil {
		l.resv = h(l.resv)
	}

	l.buildWorld(suite)
	if err = l.register(ctx, srcs); err != nil {
		return nil, err
	}
	built = true
	return l, nil
}

// buildWorld derives the scopes of the suite's principals, with identifiers generated for
// this lab: the logical references of the suite are what a report names.
func (l *lab) buildWorld(suite ap.PackageTests) {
	tenants := map[string]string{}
	workspaces := map[string]string{}
	for _, t := range suite.Fixtures.Tenants {
		tenants[string(t.Ref)] = "tnt_" + slug(string(t.Ref)) + "_" + l.suffix
		for _, w := range t.Workspaces {
			workspaces[string(t.Ref)+"/"+string(w)] = "ws_" + slug(string(w)) + "_" + l.suffix
		}
	}
	pairByLabel := map[string]*labPair{}
	n := 0
	principals := append([]ap.PackageTestsPrincipal(nil), suite.Fixtures.Principals...)
	sort.Slice(principals, func(i, j int) bool { return principals[i].Ref < principals[j].Ref })
	for _, p := range principals {
		n++
		roles := make([]string, len(p.Roles))
		for i, r := range p.Roles {
			roles[i] = string(r)
		}
		sc := contracts.Scope{
			DeploymentID:     integrationaudit.DefaultDeploymentID,
			ApplicationID:    l.app,
			TenantID:         tenants[string(p.Tenant)],
			WorkspaceID:      workspaces[string(p.Tenant)+"/"+string(p.Workspace)],
			PrincipalID:      "usr_" + slug(string(p.Ref)) + "_" + l.suffix,
			Roles:            roles,
			CapabilityScopes: []string{"rag.query"},
			SessionID:        fmt.Sprintf("ses_%s_%03d", l.suffix, n),
		}
		label := string(p.Tenant) + "/" + string(p.Workspace)
		pair := pairByLabel[label]
		if pair == nil {
			pair = &labPair{tenantRef: string(p.Tenant), workspaceRef: string(p.Workspace), scope: sc}
			pairByLabel[label] = pair
			l.pairs = append(l.pairs, pair)
		}
		l.principals[string(p.Ref)] = &labPrincipal{ref: string(p.Ref), scope: sc, pair: pair}
	}
	sort.Slice(l.pairs, func(i, j int) bool { return l.pairs[i].label() < l.pairs[j].label() })
}

// register registers every hostable source in every scope and writes its passage. A source
// the core would not build or register is recorded in unhosted, with the reason.
func (l *lab) register(ctx context.Context, srcs []labSource) error {
	for _, s := range srcs {
		for _, pair := range l.pairs {
			root := filepath.Join(l.dir, "roots", s.id)
			def, err := labDefinition(s.decl, pair.scope.TenantID, pair.scope.WorkspaceID, root)
			if err != nil {
				l.unhosted[s.id] = "the core refuses to build a source from the declaration (" + codeString(err) + ")"
				break
			}
			if err := def.CheckScope(pair.scope); err != nil {
				l.unhosted[s.id] = "the core refuses to register the source in the audit's scope (" + codeString(err) + ")"
				break
			}
			title := def.Title
			if title == "" {
				title = s.id
			}
			r := &labReg{
				source: s.id, title: title, pair: pair, srcID: def.SourceID, def: def,
				doc: synthetic(markerOf(l.suffix, s.id, pair)),
			}
			if err := l.index(ctx, r); err != nil {
				return fmt.Errorf("the synthetic passage of source %s could not be indexed (%s)", quote(s.id), codeString(err))
			}
			l.regs = append(l.regs, r)
			l.cat.add(r)
		}
	}
	return nil
}

// index writes a registration's passage the way ingestion does: a generation is begun,
// the document is put into it, the generation is validated and then activated.
func (l *lab) index(ctx context.Context, r *labReg) error {
	res, err := l.chk.Chunk(ctx, chunk.FromMarkdown(r.doc.id, r.doc.text))
	if err != nil {
		return err
	}
	sc := r.pair.scope
	comps := index.Components{
		Parser:  ragc.ComponentVersion{Name: "audit-lab", Version: "1"},
		Chunker: l.chk.Component(),
	}
	g, err := l.ix.BeginGeneration(ctx, sc, r.srcID, comps)
	if err != nil {
		return err
	}
	if _, err := l.ix.PutDocument(ctx, sc, string(g.GenerationID), index.Document{ID: r.doc.id, Version: r.doc.version, Chunks: res}); err != nil {
		return err
	}
	rep, err := l.ix.ValidateGeneration(ctx, sc, string(g.GenerationID), index.Expectation{})
	if err != nil {
		return err
	}
	if !rep.OK() {
		return contracts.New(contracts.ErrInternal, "the throwaway generation did not validate")
	}
	if err := l.ix.ActivateGeneration(ctx, sc, string(g.GenerationID)); err != nil {
		return err
	}
	r.genID = string(g.GenerationID)
	r.children = r.children[:0]
	for _, c := range res.Children {
		r.children = append(r.children, c.ID)
	}
	return nil
}

// replace puts a new text for a registration's document into its serving generation, the
// way an incremental refresh of a changed item does: the old text keeps serving until the
// new is in, and then only the new does.
func (l *lab) replace(ctx context.Context, r *labReg, doc labDoc) error {
	res, err := l.chk.Chunk(ctx, chunk.FromMarkdown(doc.id, doc.text))
	if err != nil {
		return err
	}
	if _, err := l.ix.PutDocument(ctx, r.pair.scope, r.genID, index.Document{ID: doc.id, Version: doc.version, Chunks: res}); err != nil {
		return err
	}
	r.doc = doc
	r.children = r.children[:0]
	for _, c := range res.Children {
		r.children = append(r.children, c.ID)
	}
	return nil
}

// removeSource removes a registration's source: its content is hidden in every generation
// and the catalog reports it removed.
func (l *lab) removeSource(ctx context.Context, r *labReg) error {
	if _, err := l.ix.DeleteSource(ctx, r.pair.scope, r.srcID); err != nil {
		return err
	}
	l.cat.remove(r)
	return nil
}

// close removes the throwaway store. It closes the database first (a file that is open
// cannot be removed on Windows) and is safe to call twice. It reports an error only when
// the directory could not be removed.
func (l *lab) close() error {
	l.closeOnce.Do(func() {
		if l.db != nil {
			_ = l.db.Close()
		}
		if l.dir != "" {
			if err := os.RemoveAll(l.dir); err != nil {
				l.closeErr = err
			}
		}
	})
	return l.closeErr
}

// regsOf lists the registrations of a scope pair, in source order.
func (l *lab) regsOf(pair *labPair) []*labReg {
	var out []*labReg
	for _, r := range l.regs {
		if r.pair == pair {
			out = append(out, r)
		}
	}
	return out
}

// regFor returns the registration of a source in a pair.
func (l *lab) regFor(pair *labPair, source string) *labReg {
	for _, r := range l.regs {
		if r.pair == pair && r.source == source {
			return r
		}
	}
	return nil
}

// foreign lists the registrations that are not in the scope of the principal: those of
// another tenant, and those of another workspace of the same tenant.
func (l *lab) foreign(p *labPrincipal) []*labReg {
	var out []*labReg
	for _, r := range l.regs {
		if r.pair != p.pair {
			out = append(out, r)
		}
	}
	return out
}

// principalRefs lists the fixture principals' references, in order.
func (l *lab) principalRefs() []string { return sortedKeys(l.principals) }

// ask retrieves for a principal. Only a typed error leaves this function; what the
// deployment's own wording says is not kept.
func (l *lab) ask(ctx context.Context, p *labPrincipal, text string, sourceIDs ...string) (query.Result, error) {
	return l.retr.Query(ctx, p.scope, query.Request{Text: text, SourceIDs: sourceIDs})
}

// leaks reports whether an evidence package mentions anything that belongs to a
// registration other than the allowed ones: its marker, its source, its generation or one
// of its chunks. It scans the package's whole JSON form, so a leak in any member shows;
// the question is blanked first, because a probe asks about another scope's marker on
// purpose and the package repeats the question.
func leaks(pkg ragc.EvidencePackage, notAllowed []*labReg) (bool, error) {
	pkg.Query = ""
	raw, err := jsonOf(pkg)
	if err != nil {
		return false, err
	}
	hay := strings.ToLower(raw)
	for _, r := range notAllowed {
		needles := append([]string{r.doc.marker, r.srcID, r.genID}, r.children...)
		for _, n := range needles {
			if n != "" && strings.Contains(hay, strings.ToLower(n)) {
				return true, nil
			}
		}
	}
	return false, nil
}

// labCatalog is the lab's stand-in for the deployment's authority on sources: a source is
// readable in the scope it was registered in and nowhere else, unless it was revoked
// there, and it is reported removed once removed.
type labCatalog struct {
	mu      sync.Mutex
	regs    map[string]*labReg // scope key and source id
	revoked map[string]bool
	removed map[string]bool
}

func newLabCatalog() *labCatalog {
	return &labCatalog{regs: map[string]*labReg{}, revoked: map[string]bool{}, removed: map[string]bool{}}
}

func catKey(scopeKey, srcID string) string { return scopeKey + "\x00" + srcID }

func (c *labCatalog) add(r *labReg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regs[catKey(r.scopeKey(), r.srcID)] = r
}

func (c *labCatalog) revoke(r *labReg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked[catKey(r.scopeKey(), r.srcID)] = true
}

func (c *labCatalog) restore(r *labReg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.revoked, catKey(r.scopeKey(), r.srcID))
}

func (c *labCatalog) remove(r *labReg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed[catKey(r.scopeKey(), r.srcID)] = true
}

// Sources implements query.SourceCatalog.
func (c *labCatalog) Sources(_ context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		k := catKey(keyOf(scope), id)
		r, ok := c.regs[k]
		if !ok || c.revoked[k] {
			continue
		}
		st := query.SourceState{DisplayName: r.title, Authorised: true, Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent}
		if c.removed[k] {
			st.Availability = conversation.CitationAvailabilityRemoved
		}
		out[id] = st
	}
	return out, nil
}

var _ query.SourceCatalog = (*labCatalog)(nil)

// codeString renders the typed code of an error, or "untyped_error". What a deployment or
// a library said in prose is never kept: it may carry a path or a value.
func codeString(err error) string {
	if c := contracts.CodeOf(err); c != "" {
		return string(c)
	}
	return "untyped_error"
}

// uniqueStrings returns the distinct strings of in, sorted.
func uniqueStrings(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}
