package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
	ragaudit "github.com/buxee/alix-ai/internal/integrationaudit/rag"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/tools/devkeys"
)

// TestAuditChecksRAGAreRegisteredInTheBinary: the RAG checks of sections 3 (live half), 6 and
// 10 of the integration audit have a check in the audit's default registry once this package
// is built in. Without audit_checks_rag.go they stay "not run" in production, or, for
// sections 3, 6 and 10, are carried by the static check and the runtime probes alone, which
// both record the RAG half as unverified.
func TestAuditChecksRAGAreRegisteredInTheBinary(t *testing.T) {
	for id, section := range map[string]int{
		"application.rag_sources":   integrationaudit.SectionRAGInventory,
		"application.rag_citations": integrationaudit.SectionRAGInventory,
		"application.rag_scope":     integrationaudit.SectionScope,
		"application.rag_smoke":     integrationaudit.SectionRuntimeSmoke,
	} {
		found := false
		for _, c := range integrationaudit.Default.Checks() {
			if c.ID() == id {
				found = true
				if c.Section() != section {
					t.Errorf("%s is registered for section %d, want %d", id, c.Section(), section)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in the binary's default audit registry", id)
		}
	}
}

// auditChecksRAGWithoutRuntimeProbes makes the audits of a test run with the static checks and
// the RAG checks only: the runtime probes (M1-18) are fail closed and, with no adapter, raise
// criticals that would block the package, which this test is not about.
func auditChecksRAGWithoutRuntimeProbes(t *testing.T) {
	t.Helper()
	reg := integrationaudit.NewRegistry()
	for _, c := range append(integrationaudit.Builtins(), ragaudit.Checks(ragaudit.Options{})...) {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	previous := integrationaudit.Default
	integrationaudit.Default = reg
	t.Cleanup(func() { integrationaudit.Default = previous })
}

// TestAuditAccountsForTheRAGSectionsWithoutNewFindings runs a real audit of a real installed
// package (the demo library, whose source ships inside the package) the way the binary does,
// with no adapter and no deployment seam, and reads the signed report's summary: the RAG
// sections are run, and unverified, not "not run"; and the RAG checks raise no finding at all,
// so an operator is asked to accept nothing new for what the audit simply could not look at.
func TestAuditAccountsForTheRAGSectionsWithoutNewFindings(t *testing.T) {
	auditChecksRAGWithoutRuntimeProbes(t)
	transportsSwapDefault(t) // no transport: no adapter, whatever the binary registers
	e := newOpsEnv(t, "")
	signerPriv, signerPub, _ := e.genKey("audit-signer")
	e.mustRun("trust", "add-key", "-app", trust.PlatformApplicationID, "-key-id", "audit-signer", "-public-key-file", signerPub, "-operator", "root")
	e.mustRun("trust", "add-key", "-app", "demo-library", "-key-id", devkeys.KeyID, "-public-key", devkeys.PublicKeyBase64(), "-allow-test-keys", "-operator", "root")
	e.mustRun("package", "install", "-operator", "root", filepath.Join(fixtures, "valid-minimal"))

	var run struct {
		Outcome string `json:"outcome"`
		Summary struct {
			Critical int `json:"critical"`
			Warnings int `json:"warnings"`
		} `json:"summary"`
		Sections []struct {
			Number int    `json:"number"`
			Status string `json:"status"`
		} `json:"sections"`
		Findings []struct {
			CheckID string `json:"check_id"`
			Message string `json:"message"`
		} `json:"findings"`
	}
	decode(t, e.mustRun("audit", "run", "-signing-key", "audit-signer="+signerPriv, "-operator", "root", "-no-activate", "demo-library").stdout, &run)
	if run.Summary.Critical != 0 {
		t.Fatalf("run = %+v", run)
	}
	status := map[int]string{}
	for _, s := range run.Sections {
		status[s.Number] = s.Status
	}
	for _, n := range []int{integrationaudit.SectionRAGInventory, integrationaudit.SectionScope, integrationaudit.SectionRuntimeSmoke} {
		if status[n] != "unverified" {
			t.Errorf("section %d is %q, want unverified: the RAG checks ran and could not look at the sources", n, status[n])
		}
	}
	for _, f := range run.Findings {
		if strings.HasPrefix(f.CheckID, "ragprobe.") {
			t.Errorf("a RAG check raised %s for what it could not look at: %+v", f.CheckID, f)
		}
		if f.CheckID == "audit.section_not_run" {
			for _, n := range []string{"check 3 of", "check 6 of", "check 10 of"} {
				if strings.Contains(f.Message, n) {
					t.Errorf("a RAG section is still reported as not run: %s", f.Message)
				}
			}
		}
	}
}
