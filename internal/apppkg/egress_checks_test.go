package apppkg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const (
	egressSourceFile = "rag-sources/loan-policies.yaml"
	egressCapFile    = "capabilities/catalog-find-title.yaml"
	egressCtrFile    = "conversation-contracts/catalog-find-title.en.yaml"
	egressManifest   = "alixai.yaml"
)

// egressSource appends egress declarations to the valid-minimal source.
func egressSource(t testing.TB, p pkgFiles, decl string) pkgFiles {
	t.Helper()
	b, ok := p[egressSourceFile]
	if !ok {
		t.Fatalf("fixture has no file %s", egressSourceFile)
	}
	p[egressSourceFile] = append(append([]byte(nil), b...), []byte(decl)...)
	return p
}

// egressForbid appends external.forbid: true to the manifest.
func egressForbid(p pkgFiles) pkgFiles {
	p[egressManifest] = append(append([]byte(nil), p[egressManifest]...), []byte("external:\n  forbid: true\n")...)
	return p
}

// egressLoad signs the package with the TEST-ONLY key and loads it, as the
// other check tests do, so only the egress findings are in question.
func egressLoad(t testing.TB, p pkgFiles) *Result {
	t.Helper()
	return mustLoad(t, p.signed(t), loadOptions())
}

func egressChecksOf(res *Result) []string {
	var out []string
	for _, f := range res.Findings {
		if strings.HasPrefix(f.CheckID, "egress.") {
			out = append(out, f.CheckID)
		}
	}
	return out
}

func egressCount(fs []contracts.Finding, id string) int {
	n := 0
	for _, f := range fs {
		if f.CheckID == id {
			n++
		}
	}
	return n
}

func TestRAGSourceDefaultsToLocalOnly(t *testing.T) {
	res, err := Load(context.Background(), FromDir(validMinimalDir), loadOptions())
	if err != nil || res.Package == nil {
		t.Fatalf("Load: %v %v", err, res.Findings)
	}
	if len(res.Package.Sources) == 0 {
		t.Fatal("valid-minimal has no source")
	}
	for id, s := range res.Package.Sources {
		if s.EgressMode != nil || s.Classification != nil {
			t.Errorf("source %s declares egress members: valid-minimal must stay as it was", id)
		}
		if got := EffectiveSourceEgress(s); got != rag.SourceEgressLocalOnly {
			t.Errorf("source %s: effective egress mode %q, want local_only", id, got)
		}
		if got := EffectiveSourceClassification(s); got != rag.SourceClassificationInternal {
			t.Errorf("source %s: effective classification %q, want internal", id, got)
		}
	}
	for _, c := range res.Package.Capabilities {
		for _, f := range c.Fields {
			if FieldLocalOnly(f) || EffectiveFieldClassification(f) != rag.SourceClassificationInternal {
				t.Errorf("field %s: defaults must be local_only false and internal", f.Name)
			}
		}
	}
	if ForbidsExternal(res.Package.Manifest) {
		t.Error("a manifest with no external block must not forbid external")
	}
	if got := egressChecksOf(res); len(got) != 0 {
		t.Errorf("a package with no egress declarations has egress findings: %v", got)
	}
}

func TestRestrictedSourceCannotAllowExcerpt(t *testing.T) {
	for _, mode := range []string{"allow_excerpt", "allow_metadata"} {
		t.Run(mode, func(t *testing.T) {
			p := egressSource(t, valid(t), "egress_mode: "+mode+"\nclassification: restricted\n")
			res := egressLoad(t, p)
			requireCritical(t, res, "egress.restricted_source_exposed")
			f, _ := find(res.Findings, "egress.restricted_source_exposed")
			if f.Path != egressSourceFile || f.Remediation == "" || f.Severity != contracts.SeverityCritical {
				t.Errorf("finding = %+v", f)
			}
		})
	}
	for _, decl := range []string{
		"egress_mode: local_only\nclassification: restricted\n",
		"egress_mode: deny\nclassification: restricted\n",
		"classification: restricted\n",
		"egress_mode: allow_excerpt\nclassification: confidential\n",
	} {
		res := egressLoad(t, egressSource(t, valid(t), decl))
		if got := ids(res.Findings, contracts.SeverityCritical); len(got) != 0 {
			t.Errorf("%q: critical findings %v", decl, got)
		}
	}
}

func TestAllowExcerptNeedsExplicitClassification(t *testing.T) {
	res := egressLoad(t, egressSource(t, valid(t), "egress_mode: allow_excerpt\n"))
	requireCritical(t, res, "egress.excerpt_classification_missing")

	for _, decl := range []string{
		"egress_mode: allow_excerpt\nclassification: public\n",
		"egress_mode: allow_excerpt\nclassification: internal\n", // explicit, even if equal to the default
		"egress_mode: allow_metadata\n",                          // no content leaves
		"egress_mode: local_only\n",
	} {
		res := egressLoad(t, egressSource(t, valid(t), decl))
		if got := ids(res.Findings, contracts.SeverityCritical); len(got) != 0 {
			t.Errorf("%q: critical findings %v", decl, got)
		}
	}
	// the schema, not the loader, refuses a value outside the enums
	requireCritical(t, egressLoad(t, egressSource(t, valid(t), "egress_mode: send_everything\n")), "schema.rag_source")
	requireCritical(t, egressLoad(t, egressSource(t, valid(t), "classification: secret\n")), "schema.rag_source")
}

func TestForbidExternalRespected(t *testing.T) {
	// forbid alone is fine and is read as such
	res := egressLoad(t, egressForbid(valid(t)))
	if res.Package == nil || !ForbidsExternal(res.Package.Manifest) {
		t.Fatalf("external.forbid not read: %v", res.Findings)
	}
	// forbid with a source that stays local or is denied is fine
	for _, decl := range []string{"", "egress_mode: local_only\n", "egress_mode: deny\n"} {
		res := egressLoad(t, egressSource(t, egressForbid(valid(t)), decl))
		if got := ids(res.Findings, contracts.SeverityCritical); len(got) != 0 {
			t.Errorf("forbid + %q: critical findings %v", decl, got)
		}
	}
	// forbid contradicts a source that prepares to send
	for _, decl := range []string{
		"egress_mode: allow_excerpt\nclassification: public\n",
		"egress_mode: allow_metadata\n",
	} {
		requireCritical(t, egressLoad(t, egressSource(t, egressForbid(valid(t)), decl)), "egress.forbidden_by_package")
	}
	// without forbid the same source is fine
	if got := ids(egressLoad(t, egressSource(t, valid(t), "egress_mode: allow_metadata\n")).Findings, contracts.SeverityCritical); len(got) != 0 {
		t.Errorf("critical findings %v", got)
	}
	// forbid: false is the default
	p := valid(t)
	p[egressManifest] = append(append([]byte(nil), p[egressManifest]...), []byte("external:\n  forbid: false\n")...)
	if res := egressLoad(t, egressSource(t, p, "egress_mode: allow_metadata\n")); res.Critical() {
		t.Errorf("forbid false must not forbid: %v", res.Findings)
	}
}

func TestLocalOnlyFieldNotInPromptTemplate(t *testing.T) {
	mark := func(p pkgFiles, field string) pkgFiles {
		return p.edit(t, egressCapFile, "- name: "+field+"\n", "- name: "+field+"\n  local_only: true\n")
	}
	// the field is used by the templates of the contract: refused
	res := egressLoad(t, mark(valid(t), "query"))
	requireCritical(t, res, "egress.local_only_field_in_template")
	f, _ := find(res.Findings, "egress.local_only_field_in_template")
	if f.Capability != "catalog.find_title" || f.Path != egressCtrFile || !strings.Contains(f.Message, "{input.query}") {
		t.Errorf("finding = %+v", f)
	}
	// one finding per template slot
	if n := egressCount(res.Findings, "egress.local_only_field_in_template"); n < 3 {
		t.Errorf("only %d findings; the English contract places {input.query} in several templates", n)
	}

	// a local_only field that no template uses is fine
	res = egressLoad(t, mark(valid(t), "max_results"))
	if got := ids(res.Findings, contracts.SeverityCritical); len(got) != 0 {
		t.Errorf("critical findings %v", got)
	}
	if res.Package == nil || !FieldLocalOnly(res.Package.Capabilities["catalog.find_title"].Fields[2]) {
		t.Errorf("local_only not read")
	}
	// classification alone does not forbid a template
	p := valid(t).edit(t, egressCapFile, "- name: query\n", "- name: query\n  classification: restricted\n")
	if res := egressLoad(t, p); res.Critical() {
		t.Errorf("classification alone must not refuse the template: %v", res.Findings)
	}
}

// TestExistingPackagesStillValid loads every package the repository ships (the
// valid fixture and the three reference applications) and requires exactly
// what they gave before the egress members existed: no critical finding, no
// egress finding, and the safe defaults.
func TestExistingPackagesStillValid(t *testing.T) {
	dirs := []string{
		validMinimalDir,
		"../../examples/reference-apps/clinic-booking/package",
		"../../examples/reference-apps/facility-maintenance/package",
		"../../examples/reference-apps/ledger/package",
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(filepath.Dir(dir))+"/"+filepath.Base(dir), func(t *testing.T) {
			res, err := Load(context.Background(), FromDir(dir), Options{CoreVersion: DevCoreVersion})
			if err != nil || res.Package == nil {
				t.Fatalf("Load: %v\n%s", err, dump(res.Findings))
			}
			if res.Critical() {
				t.Fatalf("critical findings:\n%s", dump(res.Findings))
			}
			if got := egressChecksOf(res); len(got) != 0 {
				t.Errorf("egress findings %v", got)
			}
			for id, s := range res.Package.Sources {
				if EffectiveSourceEgress(s) != rag.SourceEgressLocalOnly || EffectiveSourceClassification(s) != rag.SourceClassificationInternal {
					t.Errorf("source %s: %s/%s, want local_only/internal", id, EffectiveSourceEgress(s), EffectiveSourceClassification(s))
				}
			}
			if ForbidsExternal(res.Package.Manifest) {
				t.Error("an existing package must not forbid external")
			}
		})
	}
}

// The committed egress fixtures: valid-minimal plus the declarations, signed
// with the TEST-ONLY key like the broken-* packages. Regenerate with
//
//	go test ./internal/apppkg -run TestEgressFixtures -update-fixtures
var egressFixtures = []brokenFixture{
	{
		dir: "egress-local-only",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			egressSource(t, p, "egress_mode: local_only\nclassification: restricted\n")
			p.edit(t, egressCapFile, "  description: How many titles to list.\n",
				"  description: How many titles to list.\n  classification: confidential\n  local_only: true\n")
		}),
	},
	{
		dir: "egress-allow-excerpt",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			egressSource(t, p, "egress_mode: allow_excerpt\nclassification: public\n")
		}),
	},
	{
		dir: "broken-restricted-excerpt",
		build: signedWith(func(t *testing.T, p pkgFiles) {
			egressSource(t, p, "egress_mode: allow_excerpt\nclassification: restricted\n")
		}),
		want: []string{"egress.restricted_source_exposed"},
	},
}

func TestEgressFixtures(t *testing.T) {
	for _, b := range egressFixtures {
		t.Run(b.dir, func(t *testing.T) {
			want := b.build(t, valid(t))
			dir := filepath.Join(fixturesRoot, b.dir)
			if *updateFixtures {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
				writeMapFS(t, dir, want)
			}
			have := map[string][]byte{}
			if _, err := os.Stat(dir); err == nil {
				for n, c := range readDirAll(t, dir) {
					have[n] = c
				}
			}
			if !sameFiles(have, egressMapFSFiles(want)) {
				t.Fatalf("examples/packages/%s is not what valid-minimal plus its declarations builds; run: go test ./internal/apppkg -run TestEgressFixtures -update-fixtures", b.dir)
			}
			res, err := Load(context.Background(), FromDir(dir), loadOptions())
			if err != nil {
				t.Fatal(err)
			}
			requireCritical(t, res, b.want...)
			if len(b.want) == 0 && (res.Package == nil || res.Authenticity != AuthVerified) {
				t.Fatalf("the fixture must load and verify: %v", res.Findings)
			}
		})
	}
}

func egressMapFSFiles(m fstest.MapFS) map[string][]byte {
	out := map[string][]byte{}
	for n, f := range m {
		out[n] = f.Data
	}
	return out
}

// The egress defects join the table of TestEveryDefectHasItsFinding and
// TestMutationsCoverTheRegistry (checks_test.go), so each check has a finding
// of the right severity, a complete record and a remediation.
func init() {
	mutations = append(mutations,
		mutation{name: "restricted source allows excerpts", edit: func(t *testing.T, p pkgFiles) {
			egressSource(t, p, "egress_mode: allow_excerpt\nclassification: restricted\n")
		}, crit: []string{"egress.restricted_source_exposed"}},
		mutation{name: "allow_excerpt without a classification", edit: func(t *testing.T, p pkgFiles) {
			egressSource(t, p, "egress_mode: allow_excerpt\n")
		}, crit: []string{"egress.excerpt_classification_missing"}},
		mutation{name: "external forbidden but a source allows metadata", edit: func(t *testing.T, p pkgFiles) {
			egressForbid(p)
			egressSource(t, p, "egress_mode: allow_metadata\n")
		}, crit: []string{"egress.forbidden_by_package"}},
		mutation{name: "local_only field in a template", edit: func(t *testing.T, p pkgFiles) {
			p.edit(t, egressCapFile, "- name: query\n", "- name: query\n  local_only: true\n")
		}, crit: []string{"egress.local_only_field_in_template"}},
	)
}
