package adapters

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestInspectRequestAspects(t *testing.T) {
	if got := (InspectRequest{}).wanted(); len(got) != len(AllAspects()) {
		t.Fatalf("an empty request wants %v, want every aspect", got)
	}
	got := InspectRequest{Aspects: []Aspect{AspectNetwork, AspectVersions}}.wanted()
	if diff := cmp.Diff([]Aspect{AspectVersions, AspectNetwork}, got); diff != "" {
		t.Fatalf("wanted is not in canonical order (-want +got):\n%s", diff)
	}
	for _, bad := range []InspectRequest{
		{Aspects: []Aspect{"secrets"}},
		{Aspects: []Aspect{AspectHealth, AspectHealth}},
		{Aspects: []Aspect{""}},
	} {
		if err := bad.Validate(); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%v: got %v", bad, err)
		}
	}
}

const factText = `{"field":"application_version","value":{"kind":"text","value":"2.7.1"}}`

func TestNormaliseInspection(t *testing.T) {
	good := `{"reports":[
		{"aspect":"versions","state":"reported","facts":[` + factText + `]},
		{"aspect":"modules","state":"unsupported","facts":[]}]}`

	t.Run("silence is unsupported, and the report covers exactly what was asked", func(t *testing.T) {
		in, err := NormaliseInspection([]byte(good), InspectRequest{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(in.Reports) != len(AllAspects()) {
			t.Fatalf("%d reports for a request for every aspect", len(in.Reports))
		}
		for i, a := range AllAspects() {
			if in.Reports[i].Aspect != a {
				t.Fatalf("report %d is %s, want canonical order", i, in.Reports[i].Aspect)
			}
		}
		v, _ := in.Report(AspectVersions)
		if v.State != AspectReported || len(v.Facts) != 1 {
			t.Fatalf("versions = %+v", v)
		}
		n, _ := in.Report(AspectNetwork)
		if n.State != AspectUnsupported || n.Facts == nil || len(n.Facts) != 0 {
			t.Fatalf("an aspect the host did not mention must be recorded as unsupported with no facts: %+v", n)
		}
		if got := len(in.Unverified()); got != len(AllAspects())-1 {
			t.Fatalf("%d unverified aspects, want %d", got, len(AllAspects())-1)
		}
		if _, ok := in.Report("nonsense"); ok {
			t.Fatal("Report found an unknown aspect")
		}
	})

	t.Run("an empty list of reports means nothing is exposed", func(t *testing.T) {
		in, err := NormaliseInspection([]byte(`{"reports":[]}`), InspectRequest{}, 0)
		if err != nil || len(in.Unverified()) != len(AllAspects()) {
			t.Fatalf("%+v, %v; want every aspect unsupported", in, err)
		}
	})

	t.Run("a subset request returns that subset", func(t *testing.T) {
		in, err := NormaliseInspection([]byte(good), InspectRequest{Aspects: []Aspect{AspectVersions, AspectModules}}, 0)
		if err != nil || len(in.Reports) != 2 {
			t.Fatalf("%+v, %v", in, err)
		}
	})

	t.Run("an aspect that was not asked for is refused", func(t *testing.T) {
		if _, err := NormaliseInspection([]byte(good), InspectRequest{Aspects: []Aspect{AspectVersions}}, 0); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Fatalf("got %v, want schema_invalid", err)
		}
	})

	bad := map[string]string{
		"not json":               `nope`,
		"prose":                  `Everything is fine.`,
		"empty":                  ``,
		"array":                  `[]`,
		"unknown member":         `{"reports":[],"x":1}`,
		"unknown aspect":         `{"reports":[{"aspect":"secrets","state":"unsupported","facts":[]}]}`,
		"duplicate aspect":       `{"reports":[{"aspect":"health","state":"unsupported","facts":[]},{"aspect":"health","state":"unsupported","facts":[]}]}`,
		"unknown state":          `{"reports":[{"aspect":"health","state":"maybe","facts":[]}]}`,
		"reported with no fact":  `{"reports":[{"aspect":"health","state":"reported","facts":[]}]}`,
		"unsupported with fact":  `{"reports":[{"aspect":"health","state":"unsupported","facts":[` + factText + `]}]}`,
		"fact with bad field":    `{"reports":[{"aspect":"health","state":"reported","facts":[{"field":"Bad-Name","value":{"kind":"text","value":"x"}}]}]}`,
		"fact with bad decimal":  `{"reports":[{"aspect":"limits","state":"reported","facts":[{"field":"mem","value":{"kind":"decimal","value":"1,5"}}]}]}`,
		"fact with unknown kind": `{"reports":[{"aspect":"limits","state":"reported","facts":[{"field":"mem","value":{"kind":"blob","value":"x"}}]}]}`,
		"fact with null value":   `{"reports":[{"aspect":"limits","state":"reported","facts":[{"field":"mem","value":null}]}]}`,
		"two documents":          good + good,
	}
	for name, raw := range bad {
		if _, err := NormaliseInspection([]byte(raw), InspectRequest{}, 0); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Errorf("%s: got %v, want schema_invalid", name, err)
		}
	}

	t.Run("limits", func(t *testing.T) {
		if _, err := NormaliseInspection([]byte(good), InspectRequest{}, 10); reasonOf(err) != ReasonResponseTooLarge {
			t.Fatalf("over the byte limit: %v", err)
		}
		facts := make([]any, maxFactsPerAspect+1)
		for i := range facts {
			facts[i] = map[string]any{"field": "k", "value": map[string]any{"kind": "text", "value": "v"}}
		}
		raw, _ := json.Marshal(map[string]any{"reports": []any{map[string]any{"aspect": "limits", "state": "reported", "facts": facts}}})
		if _, err := NormaliseInspection(raw, InspectRequest{}, 0); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
			t.Fatalf("too many facts: %v", err)
		}
		if _, err := NormaliseInspection([]byte(good), InspectRequest{Aspects: []Aspect{"x"}}, 0); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Fatalf("a bad request is a validation error: %v", err)
		}
	})
}

func TestCheckInspectionRoundTrips(t *testing.T) {
	in, err := NormaliseInspection([]byte(`{"reports":[{"aspect":"versions","state":"reported","facts":[`+factText+`]}]}`), InspectRequest{Aspects: []Aspect{AspectVersions, AspectHealth}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CheckInspection(in, InspectRequest{Aspects: []Aspect{AspectVersions, AspectHealth}})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(in, again); diff != "" {
		t.Fatalf("CheckInspection changed a valid inspection (-want +got):\n%s", diff)
	}
	if _, err := CheckInspection(Inspection{Reports: []AspectReport{{Aspect: "nope", State: AspectUnsupported}}}, InspectRequest{}); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Fatalf("a hand-built bad inspection passed: %v", err)
	}
}
