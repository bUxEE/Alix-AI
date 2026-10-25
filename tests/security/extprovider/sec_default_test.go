package extprovidersec

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// SEC-127
func TestSecExtExternalModeOffByDefaultNeedsEveryLevel(t *testing.T) {
	sc := receptionist()

	t.Run("control: all three mode levels and all four egress levels enabled sends the request", func(t *testing.T) {
		healthy(t, newWorld(t))
	})

	t.Run("a mode level that is not external keeps the turn local, and the external side is not even consulted", func(t *testing.T) {
		for _, lv := range []modes.Level{modes.LevelDeployment, modes.LevelApplication, modes.LevelTenant} {
			w := newWorld(t)
			if err := w.modes.Set(lv, sc, modes.ModeLocal); err != nil {
				t.Fatal(err)
			}
			w.proposeBook()
			w.localProposeBook()
			res := w.say(sc, sayBook)
			wantAct(t, res, "request_confirmation")
			w.noEgress("Local at the " + string(lv) + " level")
			if w.reg.checks != 0 {
				t.Fatalf("the profile registry was consulted %d times in Local mode", w.reg.checks)
			}
			if w.local.Calls() == 0 {
				t.Fatalf("%s: the local runtime did not serve", lv)
			}
		}
	})

	t.Run("a level that was never set counts as Local, not as unrestricted", func(t *testing.T) {
		w := newWorld(t)
		r, err := modes.NewResolver(modes.NewMemoryModes(), w.ks) // nothing set anywhere
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Resolve(ctxb(), sc)
		if err != nil || res.Mode != modes.ModeLocal {
			t.Fatalf("effective mode %v err %v with nothing set, want local", res.Mode, err)
		}
	})

	t.Run("a data-egress level that is missing or disabled stops the request, at every level", func(t *testing.T) {
		for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
			w := newWorld(t)
			w.putPolicyWith(sc, lv, "", func(in *dataegress.PolicyInput) { in.Enabled = false })
			w.refusedAndSilent("egress disabled at the " + string(lv) + " level")
		}
		w := newWorld(t, withoutEgressPolicy())
		w.refusedAndSilent("no egress policy at all")
	})

	t.Run("data that is not on the class list of every level does not leave: the most restrictive level wins", func(t *testing.T) {
		for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
			w := newWorld(t)
			w.putPolicyWith(sc, lv, "", func(in *dataegress.PolicyInput) {
				in.AllowedClasses = []pv.EgressEligibleClass{pv.EgressEligibleClassPublic}
			})
			w.refusedAndSilent("internal data not allowed at the " + string(lv) + " level")
		}
	})

	t.Run("enabling external processing is a privileged act: a principal without the capability cannot widen any level", func(t *testing.T) {
		w := newWorld(t, withoutEgressPolicy())
		plain := sc.Clone()
		plain.CapabilityScopes = []string{"availability.list"}
		for _, lv := range []pv.EgressLevel{pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
			if _, err := w.eg.PutPolicy(ctxb(), plain, baseInput(lv)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
				t.Fatalf("%s: err %v, a principal with no capability wrote a policy", lv, err)
			}
		}
		// Writing the level is not enough to let internal data leave: that needs the separate approval.
		writeOnly := sc.Clone()
		writeOnly.PrincipalID, writeOnly.SessionID = "usr_writer", "ses_writer"
		c, _ := dataegress.WriteCapability(pv.EgressLevelTenant)
		writeOnly.CapabilityScopes = []string{dataegress.CapabilityRead, c}
		if _, err := w.eg.PutPolicy(ctxb(), writeOnly, baseInput(pv.EgressLevelTenant)); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Fatalf("err %v: internal data was enabled without the approval capability", err)
		}
		w.refusedAndSilent("no policy was written")
	})

	t.Run("a binary built with the noexternal tag contains no external adapter: every importer outside the adapters and the dev tools is tagged out of it", func(t *testing.T) {
		root := secutil.RepoRoot(t)
		// importers lists the non-test Go files under base that import an external adapter, once each.
		importers := func(base string) []string {
			var out []string
			err := secutil.Walk(base, func(rel string) error {
				if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
					return nil
				}
				f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(base, filepath.FromSlash(rel)), nil, parser.ImportsOnly)
				if err != nil {
					return nil
				}
				for _, imp := range f.Imports {
					if p, _ := strconv.Unquote(imp.Path.Value); strings.HasPrefix(p, modulePath+"adapters/external") {
						out = append(out, rel)
						break
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		firstLine := func(base, rel string) string {
			b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := strings.Cut(string(b), "\n")
			return strings.TrimRight(line, "\r")
		}
		// untagged returns the importers outside the adapters and the dev tools whose first line is not the tag that
		// keeps them out of a noexternal build.
		untagged := func(base string) []string {
			var bad []string
			for _, rel := range importers(base) {
				if strings.HasPrefix(rel, "adapters/external/") || strings.HasPrefix(rel, "tools/") {
					continue
				}
				if firstLine(base, rel) != "//go:build !noexternal" {
					bad = append(bad, rel)
				}
			}
			return bad
		}
		// The checker, over a planted tree whose one importer carries no build tag.
		planted := plantedImports(t, modulePath+"adapters/external/openai")
		if got := untagged(planted); len(got) != 1 {
			t.Fatalf("control: the checker flagged %v in a planted, untagged importer", got)
		}
		if bad := untagged(root); len(bad) > 0 {
			t.Fatalf("these files import an external adapter without //go:build !noexternal, so a noexternal binary would carry it: %v", bad)
		}
		if n := len(importers(root)); n < 2 {
			t.Fatalf("the scan found %d importers: it is not looking at the composition root", n)
		}
		if firstLine(root, "internal/cli/providers_noexternal.go") != "//go:build noexternal" {
			t.Fatal("the noexternal stand-in of the composition root lost its build tag")
		}
		// The alixai binary never reaches the dev tools, which link the adapters untagged.
		for _, dir := range []string{"cmd/alixai", "internal/cli"} {
			if found := forbiddenImportsIn(t, filepath.Join(root, filepath.FromSlash(dir)), []string{"tools"}); len(found) > 0 {
				t.Fatalf("%s imports a dev tool: %v", dir, found)
			}
		}
	})
}
