package policy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// FuzzParseHostPolicy checks the loader never panics or hangs on arbitrary
// input, and that whatever it accepts is a policy that cannot lower a platform
// floor or grant what role_grants does not. M5-08 runs it.
func FuzzParseHostPolicy(f *testing.F) {
	for _, dir := range []string{"policy/examples/host-policy/valid", "policy/examples/host-policy/invalid",
		"application-package/examples/policy/valid", "application-package/examples/policy/invalid"} {
		files, _ := filepath.Glob(filepath.Join(schemaRoot, filepath.FromSlash(dir), "*.json"))
		for _, file := range files {
			if raw, err := os.ReadFile(file); err == nil {
				f.Add(raw)
			}
		}
	}
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"policy_id":"p"}`))
	f.Add([]byte(`null`))
	f.Add([]byte("\xef\xbb\xbf{}"))

	gw := NewGateway(clock.NewFake(t0))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHostPolicy(data)
		if err != nil {
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
				t.Fatalf("a rejected document must be schema_invalid, got %q", contracts.CodeOf(err))
			}
			return
		}
		// Accepted: it validates again, digests stably and compiles or is refused
		// with a typed error.
		if err := h.Validate(); err != nil {
			t.Fatalf("accepted document fails Validate: %v", err)
		}
		d1, err := h.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		d2, _ := h.Digest()
		if d1 != d2 {
			t.Fatal("digest not stable")
		}
		pol, err := CompilePolicies(h)
		if err != nil {
			if contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
				t.Fatalf("compile error code %q", contracts.CodeOf(err))
			}
			return
		}

		// Apply the policy to a package built from the document itself: every
		// capability it mentions, at every effect class, for every role it
		// grants. Whatever the policy says, the floor holds and an unlisted
		// role gets nothing.
		var capIDs []string
		seen := map[string]bool{}
		roles := map[string]bool{}
		for _, g := range h.RoleGrants {
			roles[g.Role] = true
			for _, c := range g.Capabilities {
				if !seen[c] {
					seen[c] = true
					capIDs = append(capIDs, c)
				}
			}
		}
		if len(capIDs) == 0 || len(capIDs) > 16 {
			return
		}
		var roleList []string
		for r := range roles {
			roleList = append(roleList, r)
		}
		for class, floor := range map[EffectClass]contracts.RiskLevel{
			EffectNone: 0, EffectReversible: 1, EffectExternal: 2, EffectPrivileged: 3,
		} {
			var decls []CapabilityDecl
			for _, c := range capIDs {
				decls = append(decls, CapabilityDecl{ID: c, Audience: AudienceUser, EffectClass: class, RiskLevel: floor, Confirmation: ConfirmNone})
			}
			pkg := &Package{
				applicationID: appID, version: "1", status: StatusActive,
				roles: roleList, roleSet: roles, caps: map[string]CapabilityDecl{}, policy: pol,
			}
			for _, d := range decls {
				pkg.caps[d.ID] = d
				pkg.capIDs = append(pkg.capIDs, d.ID)
			}
			for _, c := range capIDs {
				for _, role := range append([]string{"outsider"}, roleList[:min(len(roleList), 4)]...) {
					sc := scopeWith(capIDs, withRoles(role))
					for _, ph := range []Phase{PhaseExpose, PhasePrepare} {
						d := gw.Decide(context.Background(), Request{
							Scope: sc, Package: pkg, CapabilityID: c, Binding: BindingOf(sc), Phase: ph,
							Fields: map[string]any{"amount": 5, "weeks": "x"},
						})
						if d.Allowed && d.EffectiveRisk < floor {
							t.Fatalf("effective risk %d below the floor %d (class %s)", d.EffectiveRisk, floor, class)
						}
						if d.Allowed && role == "outsider" {
							t.Fatalf("a role no grant names was allowed %s", c)
						}
					}
				}
			}
		}
	})
}
