package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ---- a package whose declarations a test controls --------------------------------

// ragSourcesPackage is the active package as the registry reads it: the content
// hash it is at and the sources it declares, by package-level name. It is a
// ragReauditSource, so a registration is built from it the way a real package
// builds one (an id minted by the core, bound to the scope the declaration says).
type ragSourcesPackage struct {
	root func() string

	mu       sync.Mutex
	hash     string
	declared map[string][]func(*ap.RAGSource)
	reads    int
}

func ragSourcesNewPackage(hash string, root func() string) *ragSourcesPackage {
	return &ragSourcesPackage{root: root, hash: hash, declared: map[string][]func(*ap.RAGSource){}}
}

func (p *ragSourcesPackage) declare(name string, mutate ...func(*ap.RAGSource)) *ragSourcesPackage {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.declared[name] = mutate
	return p
}

func (p *ragSourcesPackage) drop(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.declared, name)
}

func (p *ragSourcesPackage) at(hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hash = hash
}

func (p *ragSourcesPackage) build(name string, b source.Binding) (source.Definition, bool, error) {
	p.mu.Lock()
	mutations, ok := p.declared[name]
	hash := p.hash
	p.mu.Unlock()
	if !ok {
		return source.Definition{}, false, nil
	}
	d := ragTestDecl(p.root())
	d.ID = ap.Slug(name)
	for _, m := range mutations {
		m(&d)
	}
	def, err := source.FromDeclaration(d, b)
	def.ManifestHash, def.PackageVersion = hash, "1.0.0"
	return def, true, err
}

// Active implements ragReauditSource.
func (p *ragSourcesPackage) Active(context.Context, string) (string, ragRebuild, error) {
	p.mu.Lock()
	p.reads++
	hash := p.hash
	p.mu.Unlock()
	return hash, func(old source.Definition) (source.Definition, bool, error) {
		return p.build(old.PackageSourceID, source.Binding{SourceID: old.SourceID, TenantID: old.Tenant.ID, WorkspaceID: old.Workspace.ID})
	}, nil
}

// Declare implements ragReauditSource.
func (p *ragSourcesPackage) Declare(_ context.Context, scope contracts.Scope, name, sourceID string) (source.Definition, bool, error) {
	return p.build(name, source.Binding{SourceID: sourceID, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID})
}

// ragSourcesRig is a composed stack with a package that declares "docs".
type ragSourcesRig struct {
	*ragFixture
	pkg *ragSourcesPackage
}

var ragSourcesOperatorEntries = []ragOperator{
	{ApplicationID: ragApp, PrincipalID: "ops-lead", Actions: []string{string(ragSourcesActionManage)}},
}

func ragSourcesOperator(tenant, workspace string) contracts.Scope {
	// The roles are what a host application asserts and grant nothing.
	return ragScope(tenant, workspace, "ops-lead", "member")
}

func ragSourcesNewRig(t *testing.T, over *ragSourcesRig, hash string, mutate ...func(*ragFixtureOpts)) *ragSourcesRig {
	t.Helper()
	var f *ragFixture
	pkg := ragSourcesNewPackage(hash, func() string { return f.root }).declare("docs")
	f = newRagFixture(t, func(o *ragFixtureOpts) {
		o.reaudit, o.operators = pkg, ragSourcesOperatorEntries
		if over != nil {
			o.over = over.ragFixture
		}
		for _, m := range mutate {
			m(o)
		}
	})
	f.pkgs.set(ragApp, hash)
	return &ragSourcesRig{ragFixture: f, pkg: pkg}
}

// replace is the package being replaced by another content, in the process.
func (r *ragSourcesRig) replace(hash string) {
	r.pkg.at(hash)
	r.pkgs.set(ragApp, hash)
}

func (r *ragSourcesRig) register(t *testing.T, scope contracts.Scope, name string) gateway.RAGSourcesRecord {
	t.Helper()
	rec, err := r.stack.api.Register(context.Background(), scope, gateway.RAGSourcesRegistration{PackageSourceID: name})
	if err != nil {
		t.Fatalf("Register %s: %v", name, err)
	}
	return rec
}

// served reports whether the catalogue serves the source to the scope.
func (r *ragSourcesRig) served(t *testing.T, scope contracts.Scope, id string) bool {
	t.Helper()
	got, err := r.stack.sources.Sources(context.Background(), scope, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	return len(got) == 1
}

func (r *ragSourcesRig) rows(t *testing.T) int {
	t.Helper()
	return r.countRows(`SELECT count(*) FROM ragsources_registration`)
}

func (r *ragSourcesRig) syncJobs(t *testing.T) int {
	t.Helper()
	return r.countRows(`SELECT count(*) FROM jobs WHERE type = ?`, ingest.JobSync)
}

func ragSourcesSyncJob(scope contracts.Scope, id string) jobs.Job {
	return jobs.Job{Scope: scope, Type: ingest.JobSync, Payload: json.RawMessage(fmt.Sprintf(`{"source_id":%q}`, id))}
}

// ---- the registry survives a restart --------------------------------------------

// A registration outlives the process. The index persists across a restart, and
// so must the knowledge of which sources exist: until it is read back, a query
// returns nothing (fail closed) and the jobs queued before the restart are
// denied as jobs of an unknown source. After it is read back and re-bound to the
// active package, the same query answers from the same index, and the job that
// was waiting runs to its end instead of failing permission_denied.
func TestRAGSourcesSurviveServeRestart(t *testing.T) {
	ctx := context.Background()
	op := ragSourcesOperator("t1", "w1")

	// The first process registers a source, ingests it, queues one more sync
	// and stops.
	one := ragSourcesNewRig(t, nil, ragHash)
	one.write("handbook.md", "# Handbook\n\nThe harbour dock schedule lists the tide tables.")
	rec := one.register(t, op, "docs")
	id := string(rec.Status.SourceID)
	one.drain()
	want := ragTexts(one.ask("harbour dock schedule"))
	if len(want) == 0 || !strings.Contains(strings.Join(want, " "), "harbour") {
		t.Fatalf("the first process indexed nothing it can answer from: %v", want)
	}
	waiting, err := one.stack.svc.RequestSync(ctx, one.scope, id, ingest.SyncOptions{Key: "left-waiting", ServiceIdentity: ragServiceIdentity})
	if err != nil {
		t.Fatal(err)
	}
	if one.rows(t) != 1 {
		t.Fatalf("%d registrations on record, want 1", one.rows(t))
	}

	// The second process starts over the same store, with nothing in memory.
	two := ragSourcesNewRig(t, one, ragHash)
	authorizer := ragAuthorizer{packages: two.pkgs, sources: two.stack.sources}
	if err := authorizer.AuthorizeService(ctx, ragServiceIdentity, ragSourcesSyncJob(two.scope, id)); err == nil {
		t.Error("before the registrations are read back the catalogue knows no source: the waiting job must not be authorised by anything else")
	}
	if got := ragTexts(two.ask("harbour dock schedule")); len(got) != 0 {
		t.Errorf("a query before the registrations are read back returned %v: the catalogue must fail closed", got)
	}

	if err := two.stack.sources.load(ctx); err != nil {
		t.Fatal(err)
	}
	// Registered: the waiting job is authorised under the service identity, for
	// this source's own scope only.
	if err := authorizer.AuthorizeService(ctx, ragServiceIdentity, ragSourcesSyncJob(two.scope, id)); err != nil {
		t.Errorf("the job waiting since the restart is denied: %v", err)
	}
	if err := authorizer.AuthorizeService(ctx, ragServiceIdentity, ragSourcesSyncJob(ragScope("t2", "w1", "usr-1"), id)); err == nil {
		t.Error("the source was authorised for another tenant's job")
	}
	// Not yet rebuilt from the package: nothing is served and nothing is lost.
	if two.served(t, two.scope, id) || len(ragTexts(two.ask("harbour dock schedule"))) != 0 {
		t.Error("a registration read back and not yet re-bound to the active package is served")
	}
	_, err = two.stack.sources.Resolve(ctx, two.scope, id)
	if code := contracts.CodeOf(err); err == nil || !code.Retryable() || errors.Is(err, ingest.ErrSourceGone) {
		t.Errorf("Resolve of a registration not re-bound yet = %v (%q): the job must be retried, never ended as if the source were gone", err, code)
	}
	if n := two.poll(); n != 1 {
		t.Fatalf("polled %d jobs, want the waiting one", n)
	}
	if j := two.jobState(waiting.Job.ID); j.State == jobs.StateFailed || j.State == jobs.StateSucceeded {
		t.Fatalf("the waiting job ended as %s (%s) before the registration was re-bound: it must be retried", j.State, j.ErrorCode)
	}

	// The re-bind reads the active package and binds the registration to it.
	if err := two.stack.sources.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !two.served(t, two.scope, id) {
		t.Fatal("the registration is not served after the re-bind")
	}
	two.drain()
	if j := two.jobState(waiting.Job.ID); j.State != jobs.StateSucceeded {
		t.Fatalf("the job that was waiting across the restart ended as %s (%s %s)\n%s", j.State, j.ErrorCode, j.ErrorMessage, two.dumpJobs())
	}
	if got := ragTexts(two.ask("harbour dock schedule")); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the query after the restart = %v, want what it answered before: %v", got, want)
	}
	if two.rows(t) != 1 {
		t.Errorf("%d registrations on record after the restart, want 1", two.rows(t))
	}

	// The status API reads the same registration back, serving and ready.
	recs, err := two.stack.api.Status(ctx, op, "")
	if err != nil || len(recs) != 1 || !recs[0].Serving || recs[0].Status.State != rag.SourceStatusStateReady ||
		recs[0].Status.Availability != conversation.CitationAvailabilityAvailable || recs[0].PackageSourceID != "docs" {
		t.Errorf("Status after the restart = %+v, %v", recs, err)
	}
}

// serve's own start re-binds before the pool leases its first job, so a job
// that waited across a restart runs once, on its first attempt.
func TestRAGSourcesStartRebindsBeforeTheFirstJob(t *testing.T) {
	ctx := context.Background()
	one := ragSourcesNewRig(t, nil, ragHash)
	one.write("handbook.md", "# Handbook\n\nThe harbour dock schedule lists the tide tables.")
	id := string(one.register(t, ragSourcesOperator("t1", "w1"), "docs").Status.SourceID)
	one.drain()
	waiting, err := one.stack.svc.RequestSync(ctx, one.scope, id, ingest.SyncOptions{Key: "left-waiting", ServiceIdentity: ragServiceIdentity})
	if err != nil {
		t.Fatal(err)
	}

	two := ragSourcesNewRig(t, one, ragHash)
	if err := two.stack.sources.load(ctx); err != nil {
		t.Fatal(err)
	}
	stop := two.stack.start(ctx)
	defer stop()
	pollUntil(t, 20*time.Second, "the waiting job to end", func() bool { return two.jobState(waiting.Job.ID).State.Terminal() })
	if j := two.jobState(waiting.Job.ID); j.State != jobs.StateSucceeded || j.Attempts != 1 {
		t.Errorf("the waiting job ended as %s after %d attempts (%s %s), want succeeded on the first", j.State, j.Attempts, j.ErrorCode, j.ErrorMessage)
	}
	if !two.served(t, two.scope, id) {
		t.Error("the source is not served after start")
	}
}

// The package may be replaced while the process is down. The registration is
// read back bound to the content it was made from, is not served, and is
// re-bound to the active one (the stored binding moves, and ingestion rescans
// when the rules changed). A restart after that finds it bound.
func TestRAGSourcesRebindAfterARestartIntoAReplacedPackage(t *testing.T) {
	ctx := context.Background()
	op := ragSourcesOperator("t1", "w1")
	one := ragSourcesNewRig(t, nil, ragHash)
	one.write("a.md", "# A\n\nAlpha dock.")
	id := string(one.register(t, op, "docs").Status.SourceID)
	one.drain()
	if got := one.syncJobs(t); got != 1 {
		t.Fatalf("%d syncs after the registration", got)
	}

	// The package was replaced, with other rules, while the process was down.
	two := ragSourcesNewRig(t, one, "pkg-hash-2")
	two.pkg.declare("docs", func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"drafts/**"} })
	if err := two.stack.sources.load(ctx); err != nil {
		t.Fatal(err)
	}
	if two.served(t, two.scope, id) {
		t.Fatal("a registration bound to the old package is served under the new one")
	}
	if got := two.stack.sources.lookup(two.scope, id); got == nil || got.def.ManifestHash != ragHash || got.bound {
		t.Fatalf("the registration read back = %+v, want the old content hash and not bound", got)
	}
	if err := two.stack.sources.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !two.served(t, two.scope, id) {
		t.Fatal("the registration is not served after the re-bind")
	}
	if got := two.syncJobs(t); got != 2 {
		t.Errorf("%d syncs after the rules changed, want a new one", got)
	}
	reg, ok, err := two.stack.sources.store.Get(ctx, two.scope, id)
	if err != nil || !ok || reg.ManifestHash != "pkg-hash-2" {
		t.Fatalf("the stored binding after the re-bind = %+v, %v, %v; want pkg-hash-2", reg, ok, err)
	}
	two.drain()

	// A restart with the package unchanged finds it bound already: nothing to
	// rescan.
	three := ragSourcesNewRig(t, two, "pkg-hash-2")
	three.pkg.declare("docs", func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"drafts/**"} })
	if err := three.stack.sources.load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := three.stack.sources.lookup(three.scope, id); got == nil || got.def.ManifestHash != "pkg-hash-2" {
		t.Fatalf("the registration read back after the re-bind = %+v", got)
	}
	if err := three.stack.sources.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !three.served(t, three.scope, id) {
		t.Error("the registration is not served after the second restart")
	}
	if got := three.syncJobs(t); got != 2 {
		t.Errorf("%d syncs after a restart with the package unchanged, want no new one", got)
	}
}

// A re-bind that fails (the package cannot be read) leaves the registration
// unserved and the stored binding as it was.
func TestRAGSourcesStayUnservedWhenTheReBindFails(t *testing.T) {
	ctx := context.Background()
	one := ragSourcesNewRig(t, nil, ragHash)
	one.write("a.md", "# A\n\nAlpha dock.")
	id := string(one.register(t, ragSourcesOperator("t1", "w1"), "docs").Status.SourceID)
	one.drain()

	broken := &ragFakeReaudit{}
	broken.set("pkg-hash-2", nil, contracts.New(contracts.ErrUnavailable, "the package cannot be read"))
	two := ragSourcesNewRig(t, one, "pkg-hash-2", func(o *ragFixtureOpts) { o.reaudit = broken })
	if err := two.stack.sources.load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := two.stack.sources.Reconcile(ctx); err == nil {
		t.Fatal("a re-bind that could not read the package reported success")
	}
	if two.served(t, two.scope, id) {
		t.Error("a registration that could not be re-bound is served")
	}
	if reg, ok, err := two.stack.sources.store.Get(ctx, two.scope, id); err != nil || !ok || reg.ManifestHash != ragHash {
		t.Errorf("the stored binding after a failed re-bind = %+v, %v, %v; it must stay %s", reg, ok, err, ragHash)
	}
	// Registered all the same: its jobs are retried, not denied.
	if err := (ragAuthorizer{packages: two.pkgs, sources: two.stack.sources}).AuthorizeService(ctx, ragServiceIdentity, ragSourcesSyncJob(two.scope, id)); err != nil {
		t.Errorf("a registration that is waiting for its re-bind is denied: %v", err)
	}
}

// A removal that began before the process stopped is not forgotten: the source
// is read back as being removed, never served and never authorised for a job,
// and repeating the removal finishes it and drops the record.
func TestRAGSourcesRemovalIsFinishedAfterARestart(t *testing.T) {
	ctx := context.Background()
	op := ragSourcesOperator("t1", "w1")
	one := ragSourcesNewRig(t, nil, ragHash)
	one.write("a.md", "# A\n\nAlpha dock.")
	id := string(one.register(t, op, "docs").Status.SourceID)
	one.drain()
	// The process stopped after the removal was recorded and before ingestion
	// deleted anything.
	if ok, err := one.stack.sources.store.MarkRemoving(ctx, one.scope, id); err != nil || !ok {
		t.Fatal(ok, err)
	}

	two := ragSourcesNewRig(t, one, ragHash)
	if err := two.stack.sources.load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := two.stack.sources.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if two.served(t, two.scope, id) || len(ragTexts(two.ask("alpha dock"))) != 0 {
		t.Error("a source whose removal began is served after the restart")
	}
	if err := (ragAuthorizer{packages: two.pkgs, sources: two.stack.sources}).AuthorizeService(ctx, ragServiceIdentity, ragSourcesSyncJob(two.scope, id)); err == nil {
		t.Error("a job of a source being removed was authorised")
	}
	recs, err := two.stack.api.Status(ctx, op, id)
	if err != nil || len(recs) != 1 || recs[0].Serving || recs[0].Status.State != rag.SourceStatusStateRemoving ||
		recs[0].Status.Availability != conversation.CitationAvailabilityRemoved {
		t.Fatalf("Status of a source being removed = %+v, %v", recs, err)
	}
	if _, err := two.stack.api.Register(ctx, op, gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("registering a declaration whose source is being removed = %v, want conflict", err)
	}

	if err := two.stack.api.Remove(ctx, op, id); err != nil {
		t.Fatalf("finishing the removal: %v", err)
	}
	if two.rows(t) != 0 || two.stack.sources.count() != 0 {
		t.Errorf("after the removal: %d records, %d in memory", two.rows(t), two.stack.sources.count())
	}
	if info, err := two.stack.svc.Status(ctx, two.scope, id); err != nil || info.Available || info.Reason != ingest.ReasonRemoved {
		t.Errorf("ingestion's view of the removed source: %+v, %v", info, err)
	}
	three := ragSourcesNewRig(t, two, ragHash)
	if err := three.stack.sources.load(ctx); err != nil || three.stack.sources.count() != 0 {
		t.Errorf("the removed source came back after another restart: %d, %v", three.stack.sources.count(), err)
	}
	// With the removal finished the declaration can be registered again: a new source.
	if again := three.register(t, op, "docs"); string(again.Status.SourceID) == id {
		t.Error("the declaration was registered again under the id of the removed source")
	}
}

// A registration the registry cannot read back is not a source the process can
// trust: it refuses to start (the provider returns the error).
func TestRAGSourcesLoadRefusesAnUnreadableRegistry(t *testing.T) {
	ctx := context.Background()
	one := ragSourcesNewRig(t, nil, ragHash)
	one.register(t, ragSourcesOperator("t1", "w1"), "docs")
	if _, err := one.db.ExecContext(ctx, `UPDATE ragsources_registration SET scope_json = '{"application_id":"demo-library","tenant_id":"t9","workspace_id":"w1"}'`); err != nil {
		t.Fatal(err)
	}
	two := ragSourcesNewRig(t, one, ragHash)
	err := two.stack.sources.load(ctx)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("load of a row whose scope disagrees = %v", err)
	}
	if two.stack.sources.count() != 0 {
		t.Error("an unreadable registry left registrations in the catalogue")
	}
}

// ---- the registry as gateway.RAGSources -----------------------------------------

// Registering, re-auditing and removing a source are operator acts, decided from
// the deployment's own list of operators and from nothing a token asserts: not a
// role, not a capability scope, not another application's operator, not an
// operator listed for other actions. Reading a status needs no operator, only the
// scope.
func TestRAGSourcesServiceNeedsOperatorAuthority(t *testing.T) {
	ctx := context.Background()
	rig := ragSourcesNewRig(t, nil, ragHash, func(o *ragFixtureOpts) {
		o.operators = []ragOperator{
			{ApplicationID: ragApp, PrincipalID: "ops-lead", Actions: []string{string(ragSourcesActionManage)}},
			{ApplicationID: ragApp, PrincipalID: "mass-deleter", Actions: []string{string(ingest.ActionConfirmMassDeletion)}},
			{ApplicationID: "another-app", PrincipalID: "elsewhere", Actions: []string{string(ragSourcesActionManage)}},
		}
	})
	rig.write("a.md", "# A\n\nAlpha dock.")
	op := ragSourcesOperator("t1", "w1")
	registered := rig.register(t, op, "docs")
	id := string(registered.Status.SourceID)

	// A principal that may not: every act is permission_denied and nothing changes.
	for name, scope := range map[string]contracts.Scope{
		"an ordinary member":                   ragScope("t1", "w1", "usr-1"),
		"a token asserting admin and operator": ragScope("t1", "w1", "usr-1", "admin", "operator", "rag-operator"),
		"an operator of confirmations only":    ragScope("t1", "w1", "mass-deleter"),
		"another application's operator":       ragScope("t1", "w1", "elsewhere"),
	} {
		scope.CapabilityScopes = []string{"rag.manage", "rag.admin", "rag.operator"}
		if _, err := rig.stack.api.Register(ctx, scope, gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Register = %v", name, err)
		}
		if _, err := rig.stack.api.Reaudit(ctx, scope); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Reaudit = %v", name, err)
		}
		if err := rig.stack.api.Remove(ctx, scope, id); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("%s: Remove = %v", name, err)
		}
	}
	if rig.rows(t) != 1 || !rig.served(t, rig.scope, id) {
		t.Fatalf("a refused call changed the registry: %d records, served %v", rig.rows(t), rig.served(t, rig.scope, id))
	}
	// A status is a read of the caller's own scope.
	if recs, err := rig.stack.api.Status(ctx, ragScope("t1", "w1", "usr-1"), ""); err != nil || len(recs) != 1 {
		t.Errorf("Status by an ordinary member = %+v, %v", recs, err)
	}
	// An incomplete scope is no scope.
	broken := op
	broken.TenantID = ""
	if _, err := rig.stack.api.Register(ctx, broken, gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); err == nil {
		t.Error("an incomplete scope registered a source")
	}

	// With no operator configured at all, nothing can be registered.
	bare := ragSourcesNewRig(t, nil, ragHash, func(o *ragFixtureOpts) { o.operators = nil })
	if _, err := bare.stack.api.Register(ctx, op, gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("Register with no operator configured = %v", err)
	}

	// The operator may.
	if err := rig.stack.api.Remove(ctx, op, id); err != nil {
		t.Errorf("the operator's Remove: %v", err)
	}
}

// A source is registered by the name its active package declares it under and by
// nothing else. The registration is idempotent per scope and declaration, bound to
// the scope's own tenant and workspace, and refused for an application with no
// active package and for a definition that is not the active package's.
func TestRAGSourcesServiceRegistersWhatThePackageDeclares(t *testing.T) {
	ctx := context.Background()
	rig := ragSourcesNewRig(t, nil, ragHash)
	rig.pkg.declare("faq", func(d *ap.RAGSource) { d.Include = []ap.GlobPattern{"faq/**"} })
	rig.write("a.md", "# A\n\nAlpha dock.")
	op1, op2 := ragSourcesOperator("t1", "w1"), ragSourcesOperator("t2", "w1")

	// A name the package does not declare.
	_, err := rig.stack.api.Register(ctx, op1, gateway.RAGSourcesRegistration{PackageSourceID: "secrets"})
	ragCodeOf(t, err, contracts.ErrValidationFailed)
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Details["reason"] != "not_declared" {
		t.Errorf("a name the package does not declare = %v, want details.reason not_declared", err)
	}
	if _, err := rig.stack.api.Register(ctx, op1, gateway.RAGSourcesRegistration{}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("no name = %v", err)
	}
	if rig.rows(t) != 0 {
		t.Fatal("a refused registration left a record")
	}

	first := rig.register(t, op1, "docs")
	if first.ApplicationID != ragApp || first.TenantID != "t1" || first.WorkspaceID != "w1" || first.PackageSourceID != "docs" ||
		first.PackageVersion != "1.0.0" || !first.Serving || !ids.Valid(string(first.Status.SourceID), source.IDPrefix) {
		t.Fatalf("the registration = %+v", first)
	}
	if first.Status.State != rag.SourceStatusStateRegistered || first.Status.Availability != conversation.CitationAvailabilityUnavailable ||
		first.Status.Error == nil || first.Status.LastSuccessAt != nil {
		t.Errorf("a source that was never scanned claims %+v: nothing is available that was not reached", first.Status)
	}
	// Idempotent: the same registration, not a second copy.
	again := rig.register(t, op1, "docs")
	if again.Status.SourceID != first.Status.SourceID || rig.rows(t) != 1 || rig.syncJobs(t) != 1 {
		t.Errorf("registering twice: ids %s and %s, %d records, %d syncs", first.Status.SourceID, again.Status.SourceID, rig.rows(t), rig.syncJobs(t))
	}
	// Another declaration is another source; another tenant has its own.
	faq := rig.register(t, op1, "faq")
	other := rig.register(t, op2, "docs")
	if faq.Status.SourceID == first.Status.SourceID || other.Status.SourceID == first.Status.SourceID {
		t.Error("two registrations share a source id")
	}
	if other.TenantID != "t2" {
		t.Errorf("the source registered for tenant t2 says %s", other.TenantID)
	}

	// Each scope sees its own sources and nothing else; another scope's id is the
	// answer for a source that does not exist.
	recs, err := rig.stack.api.Status(ctx, op1, "")
	if err != nil || len(recs) != 2 {
		t.Fatalf("Status of t1 = %d records, %v", len(recs), err)
	}
	if string(recs[0].Status.SourceID) > string(recs[1].Status.SourceID) {
		t.Error("Status is not ordered by source id")
	}
	if recs, err = rig.stack.api.Status(ctx, op2, ""); err != nil || len(recs) != 1 || recs[0].Status.SourceID != other.Status.SourceID {
		t.Errorf("Status of t2 = %+v, %v", recs, err)
	}
	_, err = rig.stack.api.Status(ctx, op2, string(first.Status.SourceID))
	if contracts.CodeOf(err) != contracts.ErrValidationFailed || !errors.As(err, &ce) || ce.Details["reason"] != "not_found" {
		t.Errorf("Status of another tenant's source = %v, want not_found", err)
	}
	if err := rig.stack.api.Remove(ctx, op2, string(first.Status.SourceID)); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("Remove of another tenant's source = %v, want not_found", err)
	}
	if rig.rows(t) != 3 {
		t.Errorf("%d records after the cross-tenant attempts, want 3", rig.rows(t))
	}

	// Ingested, the status reports what was observed.
	rig.drain()
	recs, err = rig.stack.api.Status(ctx, op1, string(first.Status.SourceID))
	if err != nil || len(recs) != 1 {
		t.Fatal(recs, err)
	}
	st := recs[0].Status
	if st.State != rag.SourceStatusStateReady || st.Availability != conversation.CitationAvailabilityAvailable || st.Counts.Indexed != 1 ||
		st.LastSuccessAt == nil || st.ActiveGenerationID == nil || st.Error != nil || st.DisplayName != "Handbook" {
		t.Errorf("Status after ingestion = %+v", st)
	}

	// A package replaced while the call runs: the definition is not the active
	// package's, and nothing is registered.
	rig.pkg.at("pkg-hash-other")
	if _, err := rig.stack.api.Register(ctx, ragSourcesOperator("t3", "w1"), gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Errorf("a definition not built from the active package = %v, want conflict", err)
	}
	rig.pkg.at(ragHash)
	// No active package: nothing is registered, nothing is served.
	rig.pkgs.deactivate(ragApp)
	if _, err := rig.stack.api.Register(ctx, ragSourcesOperator("t3", "w1"), gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("Register with no active package = %v, want quarantined", err)
	}
	if _, err := rig.stack.api.Reaudit(ctx, op1); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("Reaudit with no active package = %v, want quarantined", err)
	}
	recs, err = rig.stack.api.Status(ctx, op1, "")
	if err != nil || len(recs) != 2 || recs[0].Serving || recs[1].Serving {
		t.Errorf("Status with no active package = %+v, %v: nothing is served", recs, err)
	}
	rig.pkgs.set(ragApp, ragHash)
	if rig.rows(t) != 3 {
		t.Errorf("%d records, want 3", rig.rows(t))
	}

	// A registry with no package reader cannot register or re-audit.
	blind := ragSourcesNewRig(t, nil, ragHash, func(o *ragFixtureOpts) { o.reaudit = nil })
	if _, err := blind.stack.api.Register(ctx, op1, gateway.RAGSourcesRegistration{PackageSourceID: "docs"}); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("Register with no package reader = %v", err)
	}
	if _, err := blind.stack.api.Reaudit(ctx, op1); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("Reaudit with no package reader = %v", err)
	}
}

// A re-audit asked for by one scope re-binds that scope's registrations. It
// neither re-binds another tenant's nor, when the package dropped a declaration,
// purges another tenant's source: that is the process's own application-wide
// re-bind, which a request cannot trigger.
func TestRAGSourcesReauditIsLimitedToTheScope(t *testing.T) {
	ctx := context.Background()
	rig := ragSourcesNewRig(t, nil, ragHash)
	rig.write("a.md", "# A\n\nAlpha dock.")
	op1, op2 := ragSourcesOperator("t1", "w1"), ragSourcesOperator("t2", "w1")
	id1, id2 := string(rig.register(t, op1, "docs").Status.SourceID), string(rig.register(t, op2, "docs").Status.SourceID)
	rig.drain()
	if !rig.served(t, op1, id1) || !rig.served(t, op2, id2) {
		t.Fatal("both sources are served before the package changes")
	}

	// The package is replaced, still declaring docs. Tenant 1 asks for a re-audit.
	rig.replace("pkg-hash-2")
	if rig.served(t, op1, id1) || rig.served(t, op2, id2) {
		t.Fatal("a source bound to the old package is served under the new one")
	}
	rep, err := rig.stack.api.Reaudit(ctx, op1)
	if err != nil || rep != (gateway.RAGSourcesReaudit{Checked: 1, Rebound: 1}) {
		t.Fatalf("Reaudit of tenant 1 = %+v, %v", rep, err)
	}
	if !rig.served(t, op1, id1) {
		t.Error("tenant 1's source is not served after its re-audit")
	}
	if rig.served(t, op2, id2) {
		t.Error("tenant 1's re-audit re-bound tenant 2's source")
	}
	if reg, ok, _ := rig.stack.sources.store.Get(ctx, op2, id2); !ok || reg.ManifestHash != ragHash {
		t.Errorf("tenant 2's stored binding = %+v, %v: it must not have moved", reg, ok)
	}
	recs, err := rig.stack.api.Status(ctx, op2, "")
	if err != nil || len(recs) != 1 || recs[0].Serving {
		t.Errorf("Status of tenant 2 = %+v, %v: its source is registered and not served", recs, err)
	}

	// The package drops the declaration. Tenant 1's re-audit removes tenant 1's
	// source and only that.
	rig.pkg.drop("docs")
	rig.replace("pkg-hash-3")
	rep, err = rig.stack.api.Reaudit(ctx, op1)
	if err != nil || rep != (gateway.RAGSourcesReaudit{Checked: 1, Removed: 1}) {
		t.Fatalf("Reaudit of tenant 1 after the package dropped the declaration = %+v, %v", rep, err)
	}
	if rig.stack.sources.lookup(op2, id2) == nil {
		t.Fatal("tenant 1's re-audit removed tenant 2's source")
	}
	if info, err := rig.stack.svc.Status(ctx, op2, id2); err != nil || !info.Available {
		t.Errorf("tenant 2's data after tenant 1's re-audit: %+v, %v", info, err)
	}
	if rig.rows(t) != 1 {
		t.Errorf("%d records, want tenant 2's only", rig.rows(t))
	}
	// The process's own re-bind is the one that reaches every tenant.
	re := &ragFakeReaudit{}
	re.set("pkg-hash-3", func(source.Definition) (source.Definition, bool, error) { return source.Definition{}, false, nil }, nil)
	rig.stack.sources.reaudit = re
	if err := rig.stack.sources.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if rig.rows(t) != 0 || rig.stack.sources.count() != 0 {
		t.Errorf("after the application-wide re-bind: %d records, %d in memory", rig.rows(t), rig.stack.sources.count())
	}
}

// Registering the same declaration of one scope under a second id is a conflict
// in the registry, and a definition that does not name its declaration cannot be
// stored: it could not be rebuilt after a restart.
func TestRAGSourcesRegistryRefusals(t *testing.T) {
	ctx := context.Background()
	f := newRagFixture(t)
	f.register()
	second := ragTestDef(t, f.root, "t1", "w1", ragHash)
	ragCodeOf(t, f.stack.sources.Register(ctx, f.scope, second), contracts.ErrConflict)
	if f.stack.sources.lookup(f.scope, second.SourceID) != nil {
		t.Error("a registration the registry refused is served")
	}
	nameless := ragTestDef(t, f.root, "t1", "w1", ragHash, func(d *ap.RAGSource) { d.ID = "other" })
	nameless.PackageSourceID = ""
	ragCodeOf(t, f.stack.sources.Register(ctx, f.scope, nameless), contracts.ErrValidationFailed)
	if f.rows() != 1 {
		t.Errorf("%d records, want 1", f.rows())
	}
}

func (f *ragFixture) rows() int {
	f.t.Helper()
	return f.countRows(`SELECT count(*) FROM ragsources_registration`)
}

// ---- the configuration sections --------------------------------------------------

// serve reads six sections of the RAG composition. A registry other than
// config.Default (a test's or a tool's) used to refuse every one of them as an
// unknown section; it now accepts them, validates each against its own package's
// rules, and the loaded values reach the stack serve composes.
func TestRAGConfigSectionsAreAcceptedByTheRegistry(t *testing.T) {
	oldSection := ragSection
	t.Cleanup(func() { ragSection = oldSection })

	sections := []string{"jobs", "rag", "rag_answer", "rag_ingest", "rag_query", "rag_vector"}
	newRegistry := func() *config.Registry {
		reg := config.NewRegistry()
		registerServeSections(reg)
		return reg
	}
	have := newRegistry().Sections()
	for _, name := range sections {
		found := false
		for _, h := range have {
			found = found || h == name
		}
		if !found {
			t.Errorf("registerServeSections does not register %q (has %v)", name, have)
		}
	}
	// What a registry without them says, for the record: the sections are unknown.
	if err := config.NewRegistry().LoadFrom(ragSourcesWriteYAML(t, "rag_ingest:\n  enqueue_batch: 250\n"), nil); err == nil {
		t.Fatal("a registry with no RAG sections accepted a rag_ingest block")
	}

	doc := `rag:
  disk_floor_bytes: 2147483648
  operators:
    - application_id: demo-library
      principal_id: ops-lead
      actions: [manage_sources, confirm_mass_deletion]
rag_ingest:
  enqueue_batch: 250
  confirm_ttl: 30m
rag_query:
  rrf_k: 40
rag_vector:
  cache_mib: 32
rag_answer:
  max_claims: 8
jobs:
  max_attempts: 7
`
	reg := newRegistry()
	if err := reg.LoadFrom(ragSourcesWriteYAML(t, doc), nil); err != nil {
		t.Fatalf("a document with every RAG block was refused: %v", err)
	}
	got := ragSourcesSectionsOf(reg)
	switch {
	case ragSection.DiskFloorBytes != 2<<30 || len(ragSection.Operators) != 1 || ragSection.Operators[0].Actions[1] != "confirm_mass_deletion":
		t.Errorf("the rag section = %+v", ragSection)
	case got.ingest.EnqueueBatch != 250 || got.ingest.ConfirmTTL != 30*time.Minute:
		t.Errorf("the rag_ingest section = %+v", got.ingest)
	case got.query.RRFK != 40:
		t.Errorf("the rag_query section = %+v", got.query)
	case got.vector.CacheMiB != 32:
		t.Errorf("the rag_vector section = %+v", got.vector)
	case got.answer.MaxClaims != 8:
		t.Errorf("the rag_answer section = %+v", got.answer)
	case got.jobs.MaxAttempts != 7:
		t.Errorf("the jobs section = %+v", got.jobs)
	}

	// Each section is validated by its own package: a bad value is refused, and
	// the refusal names the section.
	for name, bad := range map[string]string{
		"rag":        "rag:\n  disk_floor_bytes: -1\n",
		"rag_ingest": "rag_ingest:\n  enqueue_batch: -5\n",
		"rag_query":  "rag_query:\n  rrf_k: -1\n",
		"rag_vector": "rag_vector:\n  cache_mib: -1\n",
		"rag_answer": "rag_answer:\n  max_claims: 1000\n",
		"jobs":       "jobs:\n  max_attempts: -1\n",
	} {
		err := newRegistry().LoadFrom(ragSourcesWriteYAML(t, bad), nil)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: an invalid block was accepted or the error does not name the section: %v", name, err)
		}
	}
	// An unknown key in a block is refused, as on config.Default.
	if err := newRegistry().LoadFrom(ragSourcesWriteYAML(t, "rag_ingest:\n  no_such_key: 1\n"), nil); err == nil {
		t.Error("an unknown key in rag_ingest was accepted")
	}
	// The sections are the registry's own: a second registry holds the defaults.
	if other := ragSourcesSectionsOf(newRegistry()); other.ingest.EnqueueBatch == 250 || other.jobs.MaxAttempts == 7 {
		t.Errorf("another registry sees the first one's values: %+v", other)
	}

	// The values reach the composed stack: the provider reads the sections of the
	// registry serve ran on.
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	oldParser, oldDisk, oldHook := ragNewParser, ragNewDiskPressure, ragOnCompose
	t.Cleanup(func() { ragNewParser, ragNewDiskPressure, ragOnCompose = oldParser, oldDisk, oldHook })
	ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return newRagStubParser() }
	var disk struct{ floor int64 }
	ragNewDiskPressure = func(_ string, floor int64) ingest.DiskPressure { disk.floor = floor; return &ragFakeDisk{} }
	var composed *ragStack
	ragOnCompose = func(s *ragStack) { composed = s }
	services := gateway.NewServices()
	if err := gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, DataDir: "the-data-dir", Audit: f.aud, Services: services, Config: reg}
	if err := ragServeProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	in := composed.input
	switch {
	case in.Ingest.EnqueueBatch != 250 || in.Query.RRFK != 40 || in.Vector.CacheMiB != 32 || in.Jobs.MaxAttempts != 7:
		t.Errorf("the stack was composed from ingest %d, query %d, vector %d, jobs %d: the loaded sections did not reach it",
			in.Ingest.EnqueueBatch, in.Query.RRFK, in.Vector.CacheMiB, in.Jobs.MaxAttempts)
	case disk.floor != 2<<30:
		t.Errorf("the disk floor is %d: the rag section did not reach the hook", disk.floor)
	case len(in.Config.Operators) != 1:
		t.Errorf("the operators of the stack: %+v", in.Config.Operators)
	}
	// A serve with no registry of its own (config.Default's) reads the packages'.
	if d := ragSourcesSectionsOf(nil); d.ingest.EnqueueBatch != ingest.FromConfig().EnqueueBatch || d.jobs.MaxAttempts != jobs.FromConfig().MaxAttempts {
		t.Errorf("the default registry's sections = %+v", d)
	}
}

func ragSourcesWriteYAML(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "alixai.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---- the active-package path of the re-audit trigger -----------------------------

// ragInstallerSource.Active opens the installed package (signature and content
// hash checked again) and rebuilds a registered source from it. This runs it on a
// real installed package: nothing installed and a quarantined package are not
// active, an activated one yields its content hash and a rebuild that binds the
// registered source to it, the package-declared registration of the same source
// agrees with that rebuild, and a package whose files changed on disk yields
// nothing.
func TestRAGActivePackagePathRebuildsFromInstalledPackage(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	dataDir := t.TempDir()
	db := storetest.Open(t, nil)
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{DB: db, Clock: clk, DataDir: dataDir, Verifier: trust.NewVerifier(devTrust{}, clk), CoreVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	src := ragInstallerSource{inst: inst}
	scope := ragScope("t1", "w1", "usr-1")

	// Nothing installed.
	if _, _, err := src.Active(ctx, "demo-library"); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("an application with nothing installed: %v", err)
	}
	// Installed is quarantined, which is not active.
	installed, err := inst.Install(ctx, apppkg.FromDir(filepath.Join(fixtures, "valid-minimal")))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.Active(ctx, "demo-library"); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("a quarantined package: %v", err)
	}
	if _, _, err := src.Declare(ctx, scope, "loan-policies", ids.New(source.IDPrefix)); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Fatalf("Declare on a quarantined package: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE apppkg_packages SET status = 'active', audit_report_id = 'rep_cli_01' WHERE application_id = 'demo-library'`); err != nil {
		t.Fatal(err)
	}

	hash, rebuild, err := src.Active(ctx, "demo-library")
	if err != nil || hash != installed.Record.ManifestHash || rebuild == nil {
		t.Fatalf("Active = %q, %v, %v; want the installed package's content hash %q", hash, rebuild != nil, err, installed.Record.ManifestHash)
	}
	// A registration read back from the registry names only what it keeps; the
	// rebuild gives it the rest from the package.
	stub := source.Definition{SourceID: ids.New(source.IDPrefix), PackageSourceID: "loan-policies", ApplicationID: "demo-library"}
	def, declared, err := rebuild(stub)
	if err != nil || !declared {
		t.Fatalf("rebuild of a declared source = %v, %v", declared, err)
	}
	if def.SourceID != stub.SourceID || def.PackageSourceID != "loan-policies" || def.ManifestHash != hash || def.PackageVersion == "" ||
		def.Type != ap.RAGSourceTypeDocumentationPackage || def.Root.Kind != source.RootPackage ||
		def.Tenant.Mode != ap.RAGSourceScopeBindingModeShared || def.Workspace.Mode != ap.RAGSourceScopeBindingModeShared {
		t.Errorf("the rebuilt definition = %+v", def)
	}
	if err := def.CheckScope(scope); err != nil {
		t.Errorf("a shared source of the package is not usable from a scope of the application: %v", err)
	}
	if _, declared, err := rebuild(source.Definition{SourceID: stub.SourceID, PackageSourceID: "dropped-source"}); err != nil || declared {
		t.Errorf("a source the package does not declare: declared %v, %v", declared, err)
	}
	// The registration of a declared source agrees with the rebuild of it.
	reg, ok, err := src.Declare(ctx, scope, "loan-policies", stub.SourceID)
	if err != nil || !ok || reg.ManifestHash != hash || reg.PackageSourceID != "loan-policies" || reg.Tenant.ID != "" || reg.Workspace.ID != "" {
		t.Errorf("Declare = %+v, %v, %v: a shared source names no tenant and no workspace", reg, ok, err)
	}
	if _, ok, err := src.Declare(ctx, scope, "dropped-source", stub.SourceID); err != nil || ok {
		t.Errorf("Declare of a source the package does not declare: %v, %v", ok, err)
	}
	// The record's own scope is the one the declaration is checked against.
	if _, _, err := src.Declare(ctx, ragScope("t1", "w1", "usr-1"), "loan-policies", "not-a-source-id"); err == nil {
		t.Error("a declaration was built for an id that is not a core-minted source id")
	}

	// A package whose files changed since it was installed is not read at all.
	dir := filepath.Join(dataDir, "packages", "demo-library", hash)
	if err := os.WriteFile(filepath.Join(dir, "rag-sources", "loan-policies.yaml"), []byte("id: loan-policies\ntype: local_folder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if h, rb, err := src.Active(ctx, "demo-library"); err == nil || h != "" || rb != nil {
		t.Errorf("a tampered package yielded %q, %v, %v", h, rb != nil, err)
	}
	if _, _, err := src.Declare(ctx, scope, "loan-policies", stub.SourceID); err == nil {
		t.Error("a tampered package declared a source")
	}
}

// ---- what serve provides ---------------------------------------------------------

// serve hands the gateway the source interface, wrapped in its scope guard, and
// refuses to start when the registrations cannot be read back.
func TestRAGSourcesServeProvidesTheInterface(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	oldParser, oldDisk := ragNewParser, ragNewDiskPressure
	t.Cleanup(func() { ragNewParser, ragNewDiskPressure = oldParser, oldDisk })
	ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return newRagStubParser() }
	ragNewDiskPressure = func(string, int64) ingest.DiskPressure { return &ragFakeDisk{} }
	services := gateway.NewServices()
	if err := gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	deps := serveDeps{Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, DataDir: "the-data-dir", Audit: f.aud, Services: services}
	if err := ragServeProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	svc, ok := gateway.Lookup[gateway.RAGSources](services)
	if !ok {
		t.Fatal("serve does not provide gateway.RAGSources")
	}
	// It is the guarded one: an incomplete scope is refused before anything is read.
	if _, err := svc.Status(context.Background(), contracts.Scope{}, ""); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Errorf("Status with no scope = %v", err)
	}
	if err := svc.Remove(context.Background(), f.scope, "not-an-id"); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("Remove of a malformed id = %v", err)
	}

	// An unreadable registry refuses the start.
	if _, err := f.db.ExecContext(context.Background(),
		`INSERT INTO ragsources_registration (application_id, tenant_id, workspace_id, source_id, package_source_id, manifest_hash, scope_json, state, created_ns, updated_ns)
		 VALUES ('demo-library', 't1', 'w1', ?, 'docs', 'h', 'not json', 'active', 1, 1)`, ids.New(source.IDPrefix)); err != nil {
		t.Fatal(err)
	}
	again := gateway.NewServices()
	if err := gateway.Provide[gateway.ApplicationService](again, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	deps.Services = again
	if err := ragServeProvide(context.Background(), deps); err == nil || !strings.Contains(err.Error(), "registered sources") {
		t.Errorf("serve over an unreadable registry = %v, want a refused start", err)
	}
}
