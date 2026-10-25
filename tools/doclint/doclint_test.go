package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const qualified = "The decision layer is optional, bounded, non-generative and non-authoritative.\n\n"

func rules(fs []Finding) string {
	var ids []string
	for _, f := range fs {
		ids = append(ids, f.Rule)
	}
	return strings.Join(ids, ",")
}

func TestFlagsJEVAsRequired(t *testing.T) {
	for _, tc := range []struct{ name, path, text string }{
		{"other doc", "docs/architecture/x.md", "JEV is a required component of the scorer.\n"},
		{"lowercase variant", "docs/architecture/x.md", "Run Jev first.\n"},
		{"adr 0029 outside the note", "docs/decisions/0029-decision-layer-clm-8b.md", "JEV is required for scoring.\n"},
	} {
		if got := rules(Lint(tc.path, tc.text)); got != "JEV" {
			t.Errorf("%s: findings %q, want JEV", tc.name, got)
		}
	}
}

func TestAllowsJEVInHistoricalNoteOnly(t *testing.T) {
	adr := "docs/decisions/0029-decision-layer-clm-8b.md"
	note := qualified + "(JEV was only ever an optional proposed accelerator; it was never required, never the controller.)\n"
	if f := Lint(adr, note); len(f) != 0 {
		t.Errorf("historical note flagged: %v", f)
	}
	table := "| Fact | Source |\n| --- | --- |\n| up to 9x lower latency than Jev (vendor claim) | model card |\n"
	if f := Lint(adr, table); len(f) != 0 {
		t.Errorf("quoted facts-table row flagged: %v", f)
	}
	if f := Lint("docs/decisions/0030-x.md", note); rules(f) != "JEV" {
		t.Errorf("the note outside ADR 0029 must be flagged, got %v", f)
	}
}

func TestFlagsCLMDocumentMissingQualifiers(t *testing.T) {
	text := "CLM is a scorer.\n\nCLM runs beside vLLM.\n\nCLM has two heads.\n"
	f := Lint("docs/x.md", text)
	if rules(f) != "CLM-QUALIFIERS" || !strings.Contains(f[0].Msg, "non-authoritative") {
		t.Fatalf("findings %v", f)
	}
	if f := Lint("docs/x.md", qualified+text); len(f) != 0 {
		t.Errorf("qualified document flagged: %v", f)
	}
	if f := Lint("docs/x.md", "| CLM | not measured |\n"); len(f) != 0 {
		t.Errorf("a bare table mention is a pointer, not a description: %v", f)
	}
	if f := Lint("docs/x.md", "Nothing about the scorer here.\n"); len(f) != 0 {
		t.Errorf("document without CLM flagged: %v", f)
	}
}

func TestFlagsUnverifiedPerformanceClaim(t *testing.T) {
	for _, s := range []string{
		"CLM-8B is faster than the controller.",
		"CLM has lower latency in production.",
		"CLM-8B is safer and outperforms the rules.",
		"CLM reaches SOTA results.",
	} {
		if got := rules(Lint("docs/x.md", qualified+s+"\n")); got != "CLM-CLAIM" {
			t.Errorf("%q: findings %q, want CLM-CLAIM", s, got)
		}
	}
}

func TestAllowsLabelledVendorClaim(t *testing.T) {
	for _, s := range []string{
		"The vendor claim is that CLM is faster on a 4090; not evidence for AlixAI.",
		"Hypothesis: CLM-8B lowers latency; to be measured.",
		"CLM-8B is better only if the benchmark on the target host says so.",
		"Nobody says CLM is faster until the benchmark exists.",
	} {
		if f := Lint("docs/x.md", qualified+s+"\n"); len(f) != 0 {
			t.Errorf("%q flagged: %v", s, f)
		}
	}
	// a claim in one paragraph is not excused by a label in the next
	if got := rules(Lint("docs/x.md", qualified+"CLM is faster.\n\nThis is a vendor claim.\n")); got != "CLM-CLAIM" {
		t.Errorf("label in another paragraph excused the claim: %q", got)
	}
}

func TestFlagsUnconditionalCLMDefault(t *testing.T) {
	for _, s := range []string{
		"CLM-8B is enabled by default for every profile.",
		"CLM-8B is the default scorer.",
		"This is fine. CLM-8B is always on.",
	} {
		if got := rules(Lint("docs/x.md", qualified+s+"\n")); got != "CLM-DEFAULT" {
			t.Errorf("%q: findings %q, want CLM-DEFAULT", s, got)
		}
	}
	for _, s := range []string{
		"CLM-8B is enabled by default for a profile only if the gate passes.",
		"No profile enables CLM-8B by default.",
		"CLM-8B is off by default; the deterministic route is the default.",
		"CLM-8B is disabled unless the workload proves it fits.",
	} {
		if f := Lint("docs/x.md", qualified+s+"\n"); len(f) != 0 {
			t.Errorf("%q flagged: %v", s, f)
		}
	}
}

func TestExemptsOwnerPromptFilesAtRootOnly(t *testing.T) {
	root := t.TempDir()
	bad := "JEV is required. CLM-8B is the default.\n"
	for _, p := range []string{"Alix_X_Prompt.md", "docs/Alix_X_Prompt.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := LintPaths(root, []string{"Alix_X_Prompt.md", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range f {
		if x.Path != "docs/Alix_X_Prompt.md" {
			t.Errorf("unexpected finding %v", x)
		}
	}
	if len(f) == 0 {
		t.Error("nested prompt-named file must still be linted")
	}
}

func TestRunExitCodes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("JEV is required.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if c := run([]string{"a.md"}, root, &out, &errb); c != 1 || !strings.Contains(out.String(), "a.md:1: [JEV]") {
		t.Errorf("code %d out %q", c, out.String())
	}
	if c := run(nil, root, &out, &errb); c != 2 {
		t.Errorf("no args: code %d", c)
	}
	if c := run([]string{"missing.md"}, root, &out, &errb); c != 2 {
		t.Errorf("missing file: code %d", c)
	}
}

func TestPassesCurrentPlanningDocs(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("repository root not found")
	}
	paths := []string{"docs", "README.md"}
	// Planning documents kept outside the published tree are linted too when the checkout has them.
	for _, extra := range []string{"AlixAI_Standalone_Master_Plan.md", "AlixAI_External_Provider_Mode_Additive_Plan.md"} {
		if _, err := os.Stat(filepath.Join(root, extra)); err == nil {
			paths = append(paths, extra)
		}
	}
	f, err := LintPaths(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range f {
		t.Error(x)
	}
}
