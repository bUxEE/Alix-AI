package cli

import (
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
)

// TestAuditChecksProbesAreRegisteredInTheBinary: sections 2, 6, 8, 9 and 10 of the
// integration audit have a check in the audit's default registry once this package is
// built in. Without audit_checks_probes.go they stay "not run" in production, and an
// activation passes with five warnings accepted instead of five runtime checks run.
func TestAuditChecksProbesAreRegisteredInTheBinary(t *testing.T) {
	have := map[int]string{}
	for _, c := range integrationaudit.Default.Checks() {
		if c.Target() == integrationaudit.TargetApplication {
			have[c.Section()] = c.ID()
		}
	}
	for _, section := range []int{
		integrationaudit.SectionDeterministic, integrationaudit.SectionScope, integrationaudit.SectionSafeExecution,
		integrationaudit.SectionDegradation, integrationaudit.SectionRuntimeSmoke,
	} {
		if have[section] == "" {
			t.Errorf("section %d has no runtime probe in the binary's default audit registry", section)
		}
	}
}

// auditChecksProbesBuiltinChecksOnly makes the audits of a test run with the
// built-in (static) checks only, for a test whose subject is not the runtime probes
// but what follows an audit: trusting, installing, accepting warnings, activating,
// quarantining and telling a running server.
//
// The binary also registers the runtime probes (M1-18). They are fail closed: an
// application that cannot be reached, or that offers no disposable tenant, leaves a
// consequential capability unprobed, and that is critical, so a package such as
// valid-minimal could never be activated by a test that gives the audit no adapter.
// The probes have their own tests in internal/integrationaudit/probes.
func auditChecksProbesBuiltinChecksOnly(t *testing.T) {
	t.Helper()
	reg := integrationaudit.NewRegistry()
	for _, c := range integrationaudit.Builtins() {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	previous := integrationaudit.Default
	integrationaudit.Default = reg
	t.Cleanup(func() { integrationaudit.Default = previous })
}
