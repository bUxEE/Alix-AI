package isolationsec

import (
	"strings"
	"testing"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// TestSecProbeGivenNamesAvoidHexAlphabet guards SEC-029 against the false positive where a
// tenant's own hex family canary substring-matches a short probe name such as "Ada" or "Bea".
func TestSecProbeGivenNamesAvoidHexAlphabet(t *testing.T) {
	for _, name := range []string{patientGivenA, patientGivenB} {
		if hexAlphabetOnly(name) {
			t.Errorf("probe given name %q uses only hex letters; a family canary can false-match it", name)
		}
	}
}

func hexAlphabetOnly(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) && (r < 'a' || r > 'f') {
			return false
		}
		if unicode.IsDigit(r) {
			continue
		}
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// TestSecForeignPatientHexCanaryIsOwnDataNotLeak forces the historical Ada/Bea failure mode:
// tenant B's family name contains the substring "ada", so patients.find("Ada") returns 1 from
// B's own patient. That is clinic substring matching on own data, not a cross-tenant read.
// With Amy/Zoe probes and the same colliding family, the foreign search stays at 0.
func TestSecForeignPatientHexCanaryIsOwnDataNotLeak(t *testing.T) {
	const (
		tenA, tenB = "ten-hex-a", "ten-hex-b"
		famA       = "famax001122334455"
		famCollide = "fambx00ada0bea000" // substring-matches both historical probes Ada and Bea
		idBea      = "pat_bea0000001"
		idAda      = "pat_ada0000001"
	)
	session := canary("ses-hex-")
	w := newWorld(t, []string{tenA, tenB})
	w.plant(tenA, idAda, "Ada", famA)
	w.plant(tenB, idBea, "Bea", famCollide)
	scA, scB := caller(tenA, session), caller(tenB, session)

	// Historical false positive: B asks for "Ada" and gets its own Bea via substring.
	res := w.say(scB, client{}, "Find the patient Ada")
	wantAct(t, res, "present_result")
	if n, _ := factOf(res, "patient_count"); n != "1" {
		t.Fatalf("forced hex collision: patient_count = %q, want 1 from own family substring", n)
	}
	if name, _ := factOf(res, "patient_1_name"); name != "Bea "+famCollide {
		t.Fatalf("forced hex collision returned %q, want B's own Bea (not A's Ada)", name)
	}

	// A's Ada is a different record on A's host; same query under A returns Ada, not Bea.
	resA := w.say(scA, client{}, "Find the patient Ada")
	wantAct(t, resA, "present_result")
	if name, _ := factOf(resA, "patient_1_name"); name != "Ada "+famA {
		t.Fatalf("tenant A got %q, want its own Ada", name)
	}

	// Safe probes: even with ada/bea in the family canary, foreign given names stay at zero.
	w2 := newWorld(t, []string{tenA, tenB})
	w2.plant(tenA, idAda, patientGivenA, famCollide)
	w2.plant(tenB, idBea, patientGivenB, famCollide)
	scA2, scB2 := caller(tenA, session+"2"), caller(tenB, session+"2")
	for _, c := range []struct {
		sc    contracts.Scope
		other string
	}{{scA2, patientGivenB}, {scB2, patientGivenA}} {
		res := w2.say(c.sc, client{}, "Find the patient "+c.other)
		wantAct(t, res, "present_result")
		if n, _ := factOf(res, "patient_count"); n != "0" {
			t.Fatalf("safe probe %q matched own data (%s) under colliding family canary", c.other, n)
		}
	}
}
