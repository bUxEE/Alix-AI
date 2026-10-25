package contracts

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestFindingValidate(t *testing.T) {
	ok := Finding{CheckID: "docs.facet_missing", Severity: SeverityCritical, Message: "m", Remediation: "r"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("complete finding rejected: %v", err)
	}
	for name, f := range map[string]Finding{
		"no check id":      {Severity: SeverityInfo, Message: "m", Remediation: "r"},
		"unknown severity": {CheckID: "x", Severity: "fatal", Message: "m", Remediation: "r"},
		"no severity":      {CheckID: "x", Message: "m", Remediation: "r"},
		"no message":       {CheckID: "x", Severity: SeverityInfo, Remediation: "r"},
		"no remediation":   {CheckID: "x", Severity: SeverityInfo, Message: "m"},
	} {
		if err := f.Validate(); !HasCode(err, ErrValidationFailed) {
			t.Errorf("%s: Validate() = %v, want validation_failed", name, err)
		}
	}
}

// An unknown severity must block, never pass: it ranks and counts as critical.
func TestUnknownSeverityIsCritical(t *testing.T) {
	fs := []Finding{{CheckID: "a", Severity: "weird"}}
	if !HasCritical(fs) {
		t.Error("HasCritical ignored an unknown severity")
	}
	if c, w, i := CountSeverity(fs); c != 1 || w != 0 || i != 0 {
		t.Errorf("CountSeverity = %d/%d/%d, want 1/0/0", c, w, i)
	}
	if HasCritical([]Finding{{Severity: SeverityWarning}, {Severity: SeverityInfo}}) {
		t.Error("warnings and info are not critical")
	}
}

func TestSortFindingsIsDeterministic(t *testing.T) {
	in := []Finding{
		{CheckID: "b", Severity: SeverityInfo, Message: "1"},
		{CheckID: "z", Severity: SeverityCritical, Message: "2"},
		{CheckID: "a", Severity: SeverityWarning, Message: "3"},
		{CheckID: "a", Severity: SeverityCritical, Capability: "y", Message: "4"},
		{CheckID: "a", Severity: SeverityCritical, Capability: "x", Path: "p", Message: "5"},
		{CheckID: "a", Severity: SeverityCritical, Capability: "x", Path: "o", Message: "6"},
	}
	want := []string{"6", "5", "4", "2", "3", "1"}
	for i := 0; i < 3; i++ {
		got := append([]Finding(nil), in...)
		SortFindings(got)
		var msgs []string
		for _, f := range got {
			msgs = append(msgs, f.Message)
		}
		if diff := cmp.Diff(want, msgs); diff != "" {
			t.Fatalf("order differs (-want +got):\n%s", diff)
		}
		in[0], in[len(in)-1] = in[len(in)-1], in[0] // shuffle for the next round
	}
}

func TestFindingJSONShape(t *testing.T) {
	b, err := json.Marshal(Finding{CheckID: "c", Severity: SeverityWarning, Message: "m", Remediation: "r"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"check_id":"c","severity":"warning","message":"m","remediation":"r"}`
	if string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
	b, _ = json.Marshal(Finding{CheckID: "c", Severity: SeverityCritical, Capability: "k.v", Path: "a/b.yaml", Message: "m", Remediation: "r"})
	want = `{"check_id":"c","severity":"critical","capability":"k.v","path":"a/b.yaml","message":"m","remediation":"r"}`
	if string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
}
