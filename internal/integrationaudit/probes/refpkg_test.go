package probes

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

const (
	clinicDir       = "../../../examples/reference-apps/clinic-booking/package"
	validMinimalDir = "../../../examples/packages/valid-minimal"
)

func loadRef(t *testing.T, dir string) *apppkg.Package {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Package == nil {
		t.Fatalf("package %s did not load: %v", dir, res.Findings)
	}
	return res.Package
}

// idProvisioner is a provisioner with no application behind it: it only hands out
// identifiers, so the world of a suite can be built and the requests the probes would
// send can be inspected.
type idProvisioner struct{}

func (idProvisioner) CreateTenant(context.Context, Tenant) error { return nil }
func (idProvisioner) Seed(_ context.Context, s Seed) (SeedResult, error) {
	res := SeedResult{Entities: map[string]string{}}
	for _, r := range s.Records {
		res.Entities[r.Ref] = "ent_" + slug(r.Ref)
	}
	return res, nil
}
func (idProvisioner) Cleanup(context.Context, string) (CleanupReport, error) {
	return CleanupReport{}, nil
}

// TestRequestsAreBuiltForEveryProbeOfTheReferencePackages holds the way probes build
// their requests to the two real, signed packages of the repository: every scope probe
// of every suite, every safety probe and every smoke path of the clinic and the demo
// library can be turned into a request from what the package declares and seeds, with
// no value invented.
func TestRequestsAreBuiltForEveryProbeOfTheReferencePackages(t *testing.T) {
	for _, dir := range []string{clinicDir, validMinimalDir} {
		pkg := loadRef(t, dir)
		for _, suite := range pkg.Tests {
			base := contracts.Scope{DeploymentID: "alixai-audit", ApplicationID: string(pkg.Manifest.Application.ID)}
			w, _, err := provision(context.Background(), idProvisioner{}, suite, base, pkg.SortedCapabilityIDs(), clock.NewFake(simT0))
			if err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			build := func(what, capID, principal, target string) map[string]any {
				c, ok := pkg.Capabilities[capID]
				if !ok {
					t.Errorf("%s: %s names unknown capability %s", dir, what, capID)
					return nil
				}
				p, ok := w.principals[principal]
				if !ok {
					t.Errorf("%s: %s names unknown principal %s", dir, what, principal)
					return nil
				}
				var rec *wRecord
				if target != "" {
					rec = w.records[target]
				}
				f, err := w.fieldsFor(c, p, rec)
				if err != nil {
					t.Errorf("%s: %s: %v", dir, what, err)
					return nil
				}
				return f
			}
			for _, p := range suite.ScopeProbes {
				if p.Operation == ap.PackageTestsScopeProbeOperationRetrieve {
					continue
				}
				target := ""
				if p.TargetRecord != nil {
					target = string(*p.TargetRecord)
				}
				build("scope probe "+string(p.ID), string(*p.CapabilityID), string(p.Principal), target)
			}
			for _, p := range suite.SafetyProbes {
				if f := build("safety probe "+string(p.ID), string(p.CapabilityID), string(p.Principal), ""); f != nil && len(f) == 0 {
					t.Errorf("%s: safety probe %s builds an empty request", dir, p.ID)
				}
			}
			for _, p := range suite.Smoke.Paths {
				if p.CapabilityID != nil {
					build("smoke path "+string(p.ID), string(*p.CapabilityID), string(p.Principal), "")
				}
			}
		}
	}
}
