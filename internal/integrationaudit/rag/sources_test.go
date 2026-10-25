package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/integrationaudit"
	"github.com/buxee/alix-ai/internal/rag/source"
	ragsources "github.com/buxee/alix-ai/internal/rag/sources"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// registryOf opens a real durable registry (internal/rag/sources) over a throwaway store.
func registryOf(t *testing.T) *ragsources.Registry {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(ragsources.Module, ragsources.Migrations()); err != nil {
		t.Fatal(err)
	}
	r, err := ragsources.New(storetest.Open(t, reg), clock.NewFake(testT0))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// register records a registration the way the deployment does.
func register(t *testing.T, r *ragsources.Registry, app, tenant, workspace, declaration, boundTenant, boundWorkspace string) {
	t.Helper()
	err := r.Put(context.Background(), ragsources.Registration{
		Scope: storetest.Scope(app, tenant, workspace), SourceID: ids.New("src"), PackageSourceID: declaration,
		PackageVersion: "1.0.0", ManifestHash: "sha256:test", BoundTenantID: boundTenant, BoundWorkspaceID: boundWorkspace,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestUndeclaredSourceReported: a source the deployment has registered for the application
// and the package does not declare is found and reported as critical; a source that is
// declared, and bound as declared, is not; another application's sources are not this
// application's business; and a deployment that cannot list its registrations leaves the
// question unverified, never answered "none".
func TestUndeclaredSourceReported(t *testing.T) {
	pkg := labPackage(t)
	app := string(pkg.Manifest.Application.ID)
	run := func(reg *ragsources.Registry) *integrationaudit.Outcome {
		d := folderDeployment(nil)
		if reg != nil {
			d.Registrations = FromRegistry(reg)
		}
		o := Options{Deployment: func(context.Context, *integrationaudit.Input) (*Deployment, error) { return d, nil }}
		return runCheck(t, checkOf(t, o, IDSources), auditInput(pkg))
	}

	t.Run("only declared sources, bound as declared", func(t *testing.T) {
		reg := registryOf(t)
		register(t, reg, app, "tnt-1", "ws-1", "loan-policies", "tnt-1", "ws-1")
		register(t, reg, app, "tnt-2", "ws-1", "loan-policies", "tnt-2", "ws-1")
		out := run(reg)
		if hasFinding(out, fdUndeclared.id) || hasFinding(out, fdBindingMismatch.id) {
			t.Fatalf("declared sources reported:\n%s", dump(out))
		}
		if hasUnverified(out, "rag_sources:registrations") {
			t.Errorf("the registrations were listed, yet they are recorded as unlisted")
		}
		if p := probeNamed(out, "registered sources against"); p == nil || !p.Passed {
			t.Errorf("no passing probe for the declared registrations: %+v", p)
		}
	})

	t.Run("a registered source the package does not declare", func(t *testing.T) {
		reg := registryOf(t)
		register(t, reg, app, "tnt-1", "ws-1", "loan-policies", "tnt-1", "ws-1")
		register(t, reg, app, "tnt-1", "ws-1", "shadow-docs", "tnt-1", "ws-1")
		register(t, reg, app, "tnt-2", "ws-1", "shadow-docs", "tnt-2", "ws-1")
		register(t, reg, "another-app", "tnt-1", "ws-1", "other-docs", "tnt-1", "ws-1")
		out := run(reg)
		var got []contracts.Finding
		for _, f := range out.Findings {
			if f.CheckID == fdUndeclared.id {
				got = append(got, f)
			}
		}
		if len(got) != 1 {
			t.Fatalf("want one undeclared finding (a declaration registered twice is one finding), got %d:\n%s", len(got), dump(out))
		}
		f := got[0]
		if f.Severity != contracts.SeverityCritical || !strings.Contains(f.Message, `"shadow-docs"`) || !strings.Contains(f.Message, "2 registration") {
			t.Errorf("finding = %v", f)
		}
		if strings.Contains(f.Message, "other-docs") {
			t.Errorf("another application's source was reported: %v", f)
		}
		if !hasCritical(out) {
			t.Errorf("an undeclared source must block activation")
		}
	})

	t.Run("a declared source bound against its declaration", func(t *testing.T) {
		reg := registryOf(t)
		// loan-policies is request-bound at both levels; this registration names no tenant.
		register(t, reg, app, "tnt-1", "ws-1", "loan-policies", "", "")
		out := run(reg)
		if !hasFinding(out, fdBindingMismatch.id) || !hasCritical(out) {
			t.Fatalf("a binding that contradicts the declaration was not reported:\n%s", dump(out))
		}
	})

	t.Run("a deployment that cannot list its registrations", func(t *testing.T) {
		out := run(nil)
		if hasFinding(out, fdUndeclared.id) {
			t.Fatalf("nothing could be listed, so nothing can be undeclared:\n%s", dump(out))
		}
		if !hasUnverified(out, "rag_sources:registrations") {
			t.Errorf("unverified = %v, want rag_sources:registrations", unverifiedSubjects(out))
		}
	})

	t.Run("a registry that fails is unverified, not clean", func(t *testing.T) {
		d := folderDeployment(nil)
		d.Registrations = func(context.Context, string) ([]Registration, error) {
			return nil, contracts.New(contracts.ErrUnavailable, "the store is busy at /var/secret/path")
		}
		o := Options{Deployment: func(context.Context, *integrationaudit.Input) (*Deployment, error) { return d, nil }}
		out := runCheck(t, checkOf(t, o, IDSources), auditInput(pkg))
		if !hasUnverified(out, "rag_sources:registrations") || hasFinding(out, fdUndeclared.id) {
			t.Fatalf("%s", dump(out))
		}
		for _, u := range out.Unverified {
			if strings.Contains(u.Reason, "/var/secret") {
				t.Errorf("what the deployment said leaked into the report: %q", u.Reason)
			}
		}
	})
}

// TestUnreachableSourceReported: a declared source whose root cannot be reached from the
// deployment is reported as unreachable (a warning, and unverified: nothing could be said
// about the rest of it), where the same source with its root in place is verified live; a
// mount the deployment has not registered is unreachable, and one it does not tell the audit
// about is unverified; a source type no provider serves is unverified, never reached.
func TestUnreachableSourceReported(t *testing.T) {
	deployment := func(d *Deployment) Options {
		return Options{Deployment: func(context.Context, *integrationaudit.Input) (*Deployment, error) { return d, nil }}
	}

	t.Run("reachable: verified live, every field", func(t *testing.T) {
		pkg := labPackage(t)
		out := runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), auditInput(pkg))
		if len(findingIDs(out, contracts.SeverityCritical))+len(findingIDs(out, contracts.SeverityWarning)) > 0 {
			t.Fatalf("a reachable, well declared source drew findings:\n%s", dump(out))
		}
		if len(out.Sources) != 1 || out.Sources[0].ID != "loan-policies" || out.Sources[0].Verified != "live" ||
			len(out.Sources[0].Declared) != len(ragFields) || len(out.Sources[0].Missing) != 0 {
			t.Fatalf("sources = %+v", out.Sources)
		}
		if hasUnverified(out, "rag_source:loan-policies") {
			t.Errorf("a source verified live is also unverified")
		}
		for _, f := range ragFields {
			if p := probeNamed(out, "loan-policies: "+f); p == nil || !p.Passed {
				t.Errorf("no passing probe for field %s: %+v", f, p)
			}
		}
		if len(failedProbes(out)) > 0 {
			t.Errorf("failed probes: %+v", failedProbes(out))
		}
	})

	t.Run("the root is gone", func(t *testing.T) {
		pkg := labPackage(t)
		if err := os.RemoveAll(folderRoot(pkg)); err != nil {
			t.Fatal(err)
		}
		out := runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), auditInput(pkg))
		got := findingIDs(out, contracts.SeverityWarning)
		if len(got) != 1 || got[0] != fdUnreachable.id {
			t.Fatalf("warnings = %v, want the unreachable source:\n%s", got, dump(out))
		}
		if hasCritical(out) {
			t.Errorf("an unreachable source is reported, and fails closed; it is no reason to block")
		}
		if !hasUnverified(out, "rag_source:loan-policies") {
			t.Errorf("unverified = %v: an unreachable source cannot be called verified", unverifiedSubjects(out))
		}
		if len(out.Sources) != 0 {
			t.Errorf("an unreachable source was recorded as verified: %+v", out.Sources)
		}
		for _, f := range out.Findings {
			if strings.Contains(f.Message, folderRoot(pkg)) || strings.Contains(f.Message, filepath.Base(filepath.Dir(folderRoot(pkg)))) {
				t.Errorf("the host path is in the finding: %v", f)
			}
		}
	})

	t.Run("mount roots", func(t *testing.T) {
		mountPkg := func() *integrationaudit.Input {
			pkg := labPackage(t)
			decl := pkg.Sources["loan-policies"]
			decl.Type = ap.RAGSourceTypeMountedVolume
			decl.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootMount{Kind: "mount", MountID: "nas-1"}}
			pkg.Sources["loan-policies"] = decl
			return auditInput(pkg)
		}
		// registered by the operator, but not under the id the package names
		d := folderDeployment(nil)
		d.Mounts = func(string) (string, bool) { return "", false }
		out := runCheck(t, checkOf(t, deployment(d), IDSources), mountPkg())
		if got := findingIDs(out, contracts.SeverityWarning); len(got) != 1 || got[0] != fdUnreachable.id {
			t.Fatalf("a mount the deployment has not registered must be unreachable:\n%s", dump(out))
		}
		// registered and present
		dir := t.TempDir()
		d = folderDeployment(nil)
		d.Mounts = func(id string) (string, bool) { return dir, id == "nas-1" }
		out = runCheck(t, checkOf(t, deployment(d), IDSources), mountPkg())
		if len(out.Findings) > 0 || len(out.Sources) != 1 || out.Sources[0].Verified != "live" {
			t.Fatalf("a registered, present mount must verify live:\n%s", dump(out))
		}
		// the audit is not told the mounts: unverified, not unreachable
		out = runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), mountPkg())
		if len(out.Findings) > 0 || !hasUnverified(out, "rag_source:loan-policies") {
			t.Fatalf("unknown mounts are unverified, not findings:\n%s", dump(out))
		}
	})

	t.Run("a source type no provider serves", func(t *testing.T) {
		pkg := loadRef(t, validMinimalDir) // loan-policies ships inside the package
		out := runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), auditInput(pkg))
		if len(out.Findings) > 0 {
			t.Fatalf("no provider is not a finding:\n%s", dump(out))
		}
		if !hasUnverified(out, "rag_source:loan-policies") || len(out.Sources) != 0 {
			t.Fatalf("a source nothing can reach was not left unverified:\n%s", dump(out))
		}
		found := false
		for _, u := range out.Unverified {
			found = found || strings.Contains(u.Reason, "no source provider")
		}
		if !found {
			t.Errorf("the reason does not say that no provider serves the type: %v", out.Unverified)
		}
	})

	t.Run("the credential reference must be one the application exposes", func(t *testing.T) {
		pkg := labPackage(t)
		decl := pkg.Sources["loan-policies"]
		ref := ap.CredentialRef("cred:library-docs")
		decl.CredentialRef = &ref
		pkg.Sources["loan-policies"] = decl
		// no inspection: unverified, never a pass
		out := runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), auditInput(pkg))
		if len(out.Findings) > 0 || !hasUnverified(out, "rag_source:loan-policies") || len(out.Sources) != 0 {
			t.Fatalf("a credential that could not be looked for is unverified:\n%s", dump(out))
		}
	})

	t.Run("include rules that admit nothing", func(t *testing.T) {
		pkg := labPackage(t)
		decl := pkg.Sources["loan-policies"]
		decl.Exclude = []ap.GlobPattern{"**"}
		pkg.Sources["loan-policies"] = decl
		out := runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), auditInput(pkg))
		if got := findingIDs(out, contracts.SeverityWarning); len(got) != 1 || got[0] != fdRulesEmpty.id {
			t.Fatalf("an exclude rule that removes everything went unreported:\n%s", dump(out))
		}
	})

	t.Run("a declaration the core refuses", func(t *testing.T) {
		pkg := labPackage(t)
		decl := pkg.Sources["loan-policies"]
		decl.Include = nil
		pkg.Sources["loan-policies"] = decl
		out := runCheck(t, checkOf(t, deployment(folderDeployment(nil)), IDSources), auditInput(pkg))
		if !hasFinding(out, fdDefinitionRefused.id) || !hasCritical(out) {
			t.Fatalf("a declaration the core cannot register went unreported:\n%s", dump(out))
		}
	})
}

// TestSourcesNeverReachIntoTheRoot: the live check asks the provider whether a root can be
// reached and nothing else: it never lists a directory and never reads a document of a real
// source.
func TestSourcesNeverReachIntoTheRoot(t *testing.T) {
	pkg := labPackage(t)
	root := folderRoot(pkg)
	canary := filepath.Join(root, "private.md")
	if err := os.WriteFile(canary, []byte("# private\n\nnot for the audit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &spyProvider{Provider: folderDeployment(nil).Providers[0]}
	d := &Deployment{Providers: []source.Provider{spy}}
	o := Options{Deployment: func(context.Context, *integrationaudit.Input) (*Deployment, error) { return d, nil }}
	out := runCheck(t, checkOf(t, o, IDSources), auditInput(pkg))
	if len(out.Sources) != 1 || out.Sources[0].Verified != "live" {
		t.Fatalf("%s", dump(out))
	}
	if spy.probes == 0 || spy.other != 0 {
		t.Errorf("provider calls: probe=%d other=%d, want Probe only", spy.probes, spy.other)
	}
}

type spyProvider struct {
	source.Provider
	probes, other int
}

func (s *spyProvider) Probe(ctx context.Context, sc contracts.Scope, d source.Definition) (source.Health, error) {
	s.probes++
	return s.Provider.Probe(ctx, sc, d)
}

func (s *spyProvider) Discover(ctx context.Context, sc contracts.Scope, d source.Definition, prev []source.Record) (source.ChangeSet, error) {
	s.other++
	return s.Provider.Discover(ctx, sc, d, prev)
}

func (s *spyProvider) DetectDeletions(ctx context.Context, sc contracts.Scope, d source.Definition, prev []source.Record) (source.ChangeSet, error) {
	s.other++
	return s.Provider.DetectDeletions(ctx, sc, d, prev)
}

func (s *spyProvider) Stat(ctx context.Context, sc contracts.Scope, d source.Definition, key string) (source.ItemInfo, error) {
	s.other++
	return s.Provider.Stat(ctx, sc, d, key)
}

func (s *spyProvider) FetchVersion(ctx context.Context, sc contracts.Scope, d source.Definition, key string) (source.Version, error) {
	s.other++
	return s.Provider.FetchVersion(ctx, sc, d, key)
}

func (s *spyProvider) Open(ctx context.Context, sc contracts.Scope, d source.Definition, key string) (source.Content, error) {
	s.other++
	return s.Provider.Open(ctx, sc, d, key)
}
