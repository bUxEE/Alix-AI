package contracts

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestRiskLevelBounds(t *testing.T) {
	for n := 0; n <= 3; n++ {
		lvl, err := ParseRiskLevel(n)
		if err != nil || int(lvl) != n || !lvl.Valid() || lvl.Validate() != nil {
			t.Errorf("level %d should be valid: %v %v", n, lvl, err)
		}
	}
	for _, n := range []int{-1, 4, 5, 255, 256, 1 << 20} {
		if _, err := ParseRiskLevel(n); !HasCode(err, ErrValidationFailed) {
			t.Errorf("ParseRiskLevel(%d) = %v, want validation_failed", n, err)
		}
	}
	for _, lvl := range []RiskLevel{4, 7, 255} {
		if lvl.Valid() || lvl.Validate() == nil {
			t.Errorf("RiskLevel(%d) must be invalid", lvl)
		}
		if _, err := json.Marshal(lvl); err == nil {
			t.Errorf("RiskLevel(%d) must refuse to marshal", lvl)
		}
	}

	// JSON round trip for the valid range.
	for n := 0; n <= 3; n++ {
		b, err := json.Marshal(RiskLevel(n))
		if err != nil || string(b) != strconv.Itoa(n) {
			t.Fatalf("marshal %d = %s, %v", n, b, err)
		}
		var back RiskLevel
		if err := json.Unmarshal(b, &back); err != nil || back != RiskLevel(n) {
			t.Fatalf("unmarshal %s = %v, %v", b, back, err)
		}
	}
	// Anything else is rejected, and a failed decode never changes the value.
	for _, in := range []string{"4", "-1", "2.5", "2.0", "1e0", `"2"`, "null", "true", "[]", "{}", `""`, "255"} {
		lvl := RiskConsequential
		if err := json.Unmarshal([]byte(in), &lvl); err == nil {
			t.Errorf("unmarshal %s should fail", in)
		}
		if lvl != RiskConsequential {
			t.Errorf("failed unmarshal of %s modified the value to %v", in, lvl)
		}
	}
	// Called directly, UnmarshalJSON must also be strict about number syntax.
	for _, in := range []string{"+2", "02", " 2x"} {
		var lvl RiskLevel
		if err := lvl.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("UnmarshalJSON(%q) should fail", in)
		}
	}
	// Decoding through a struct must also reject out-of-range values.
	var s struct {
		Risk RiskLevel `json:"risk"`
	}
	if err := json.Unmarshal([]byte(`{"risk": 9}`), &s); err == nil {
		t.Error("struct decode of risk 9 should fail")
	}
}

func TestRiskLevelSemantics(t *testing.T) {
	cases := []struct {
		lvl             RiskLevel
		confirm, stepUp bool
	}{
		{RiskRead, false, false},
		{RiskReversible, false, false},
		{RiskConsequential, true, false},
		{RiskPrivileged, true, true},
		{RiskLevel(9), true, true}, // invalid: fail closed
	}
	for _, c := range cases {
		if c.lvl.RequiresConfirmation() != c.confirm || c.lvl.RequiresStepUp() != c.stepUp {
			t.Errorf("%v: confirm=%t stepUp=%t", c.lvl, c.lvl.RequiresConfirmation(), c.lvl.RequiresStepUp())
		}
	}
	// The host may raise a level but never silently lower a platform minimum.
	if got := RiskConsequential.Escalate(RiskRead); got != RiskConsequential {
		t.Errorf("host lowered the platform minimum: %v", got)
	}
	if got := RiskReversible.Escalate(RiskPrivileged); got != RiskPrivileged {
		t.Errorf("host raise ignored: %v", got)
	}
	if got := RiskRead.Escalate(RiskLevel(9)); got != RiskPrivileged {
		t.Errorf("invalid input must escalate to the maximum, got %v", got)
	}
	if RiskReversible.String() != "risk_1" || RiskLevel(9).String() != "risk_invalid" {
		t.Error("String() mismatch")
	}
}
