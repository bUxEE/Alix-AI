package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func rdyScaffoldLoad(t *testing.T, dir string) *apppkg.Result {
	t.Helper()
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReadinessScanScaffoldWritesAnUnsignedDraftTheLoaderRefuses(t *testing.T) {
	b := rdyCLINewBox(t, "")
	out := filepath.Join(b.root, "draft")
	before := rdyCLITree(t, b.project)

	for _, form := range [][]string{{"-scaffold", out}, {"--scaffold", out}, {"-scaffold=" + out}} {
		_ = os.RemoveAll(out)
		args := append([]string{"readiness", "scan", b.project, "-no-model", "-format", "json", "-app", "clinic-booking"}, form...)
		r := b.mustRun(args...)
		if !strings.Contains(r.stderr, "UNSIGNED DRAFT package") || !strings.Contains(r.stderr, "DRAFT-TODO.md") {
			t.Fatalf("%v: stderr does not say the draft is unsigned:\n%s", form, r.stderr)
		}
		if !strings.Contains(r.stdout, `"advisory": true`) {
			t.Fatalf("%v: the report was not printed:\n%s", form, r.stdout)
		}
	}
	if !slices.Equal(slices.Sorted(mapKeys(before)), slices.Sorted(mapKeys(rdyCLITree(t, b.project)))) || !sameTree(before, rdyCLITree(t, b.project)) {
		t.Fatal("the scanned directory changed")
	}

	tree := rdyCLITree(t, out)
	for _, want := range []string{"alixai.yaml", "DRAFT-TODO.md", "documentation/index.yaml", "capabilities/cap-001.yaml"} {
		if _, ok := tree[want]; !ok {
			t.Errorf("the draft lacks %s: %v", want, slices.Sorted(mapKeys(tree)))
		}
	}
	for name := range tree {
		if strings.Contains(name, "signature") {
			t.Errorf("the draft carries %s", name)
		}
	}
	if m, _ := os.ReadFile(filepath.Join(out, "alixai.yaml")); !strings.Contains(string(m), "DRAFT") || !strings.Contains(string(m), "id: clinic-booking") {
		t.Fatalf("alixai.yaml is not a marked draft for the named application:\n%s", m)
	}

	res := rdyScaffoldLoad(t, out)
	refused := false
	for _, f := range res.Findings {
		if f.CheckID == "signature.missing" && f.Severity == contracts.SeverityCritical {
			refused = true
		}
	}
	if !refused || res.Package != nil || res.OK() {
		t.Fatalf("the loader does not refuse the draft as unsigned: %+v", res.Findings)
	}
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func sameTree(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestReadinessScanScaffoldRefusesBeforeAnyWork(t *testing.T) {
	b := rdyCLINewBox(t, "")
	existing := filepath.Join(b.root, "taken")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := rdyCLITree(t, b.project)

	for name, args := range map[string][]string{
		"an existing directory":      {"-scaffold", existing},
		"a place inside the project": {"-scaffold", filepath.Join(b.project, "draft")},
		"no directory":               {"-scaffold"},
		"an empty directory name":    {"-scaffold", ""},
	} {
		r := b.run(append([]string{"readiness", "scan", b.project, "-no-model"}, args...)...)
		if r.code != ExitUsage {
			t.Errorf("%s: exit %d, want %d\nstderr:\n%s", name, r.code, ExitUsage, r.stderr)
		}
		if strings.Contains(r.stderr, "stored report") {
			t.Errorf("%s: a scan was made for a draft that cannot be written", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(existing, "keep.txt")); string(b) != "keep" {
		t.Fatal("the existing directory was changed")
	}
	if !sameTree(before, rdyCLITree(t, b.project)) {
		t.Fatal("the scanned directory changed")
	}
	if l := b.list(b.project); len(l.Reports) != 0 {
		t.Fatalf("a report was stored: %v", l.Reports)
	}
}

func TestReadinessScanWithoutScaffoldIsUnchangedAndHelpMentionsIt(t *testing.T) {
	b := rdyCLINewBox(t, "")
	r := b.mustRun("readiness", "scan", b.project, "-no-model", "-format", "json")
	if strings.Contains(r.stderr, "DRAFT") {
		t.Fatalf("a draft was written without -scaffold:\n%s", r.stderr)
	}
	h := b.run("readiness", "scan", "-h")
	if h.code != ExitOK || !strings.Contains(h.stdout, "-scaffold") {
		t.Fatalf("help does not mention -scaffold (exit %d):\n%s", h.code, h.stdout)
	}
}
