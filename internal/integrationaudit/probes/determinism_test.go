package probes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/integrationaudit"
)

func encode(t *testing.T, o *integrationaudit.Outcome) string {
	t.Helper()
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The report body carries no timestamp and no generated identifier: two audits of the
// same application are byte-identical, although every run generates tenants, principals,
// sessions and records of its own. Each check is run twice over equal applications, a
// correct one and a broken one, and what it reports must be the same.
func TestProbesAreDeterministic(t *testing.T) {
	for name, knob := range map[string]func(*simKnobs){
		"a correct host":               func(*simKnobs) {},
		"a host that leaks and breaks": func(k *simKnobs) { k.leakTenant, k.nonIdempotent, k.noExpiry, k.leftBehind = true, true, true, true },
	} {
		t.Run(name, func(t *testing.T) {
			for _, section := range []int{
				integrationaudit.SectionDeterministic, integrationaudit.SectionScope, integrationaudit.SectionSafeExecution,
				integrationaudit.SectionDegradation, integrationaudit.SectionRuntimeSmoke,
			} {
				a := encode(t, newEnv(t, knob).run(section))
				b := encode(t, newEnv(t, knob).run(section))
				if a != b {
					t.Errorf("section %d: two runs differ:\n%s\n%s", section, a, b)
				}
				for _, banned := range []string{"tnt_", "usr_", "ws_", "ses_", "note_", "idem_"} {
					if strings.Contains(a, banned) {
						t.Errorf("section %d: a generated identifier (%s...) is in the outcome", section, banned)
					}
				}
			}
		})
	}
}

// Importing the package is all the binary needs: the five checks register themselves with
// the audit's default registry, one for each section the static checks leave open.
func TestChecksRegisterThemselvesWithTheDefaultRegistry(t *testing.T) {
	sections := map[int]string{}
	for _, c := range integrationaudit.Default.Checks() {
		if strings.HasPrefix(c.ID(), "application.") {
			sections[c.Section()] = c.ID()
		}
	}
	want := map[int]string{
		integrationaudit.SectionDeterministic: IDResults, integrationaudit.SectionScope: IDScope,
		integrationaudit.SectionSafeExecution: IDSafety, integrationaudit.SectionDegradation: IDDegradation,
		integrationaudit.SectionRuntimeSmoke: IDSmoke,
	}
	for section, id := range want {
		if sections[section] != id {
			t.Errorf("section %d: registered %q, want %q", section, sections[section], id)
		}
	}
	for _, c := range Checks(Options{}) {
		if c.Target() != integrationaudit.TargetApplication {
			t.Errorf("%s audits the %s target, want application", c.ID(), c.Target())
		}
	}
}
