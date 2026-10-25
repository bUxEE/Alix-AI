package providersuite

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func buildReport(t *testing.T, o FakeOptions) (pv.ConformanceReport, []byte) {
	t.Helper()
	sub := NewFake(o).Subject()
	start := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	rows := Run(context.Background(), fakeFactory(o))
	rep, raw, err := BuildReport(ReportInput{Rows: rows, Profile: sub.Profile, Mode: ModeFixtures, Started: start, Completed: start.Add(time.Minute), Clock: clock.NewFake(start)})
	if err != nil {
		t.Fatalf("BuildReport: %v", err)
	}
	return rep, raw
}

// TestReportValidatesAgainstSchema: the report of a run validates against the
// X-02 schema, decodes into the typed report the registry records, carries the
// profile's own provider reference and declaration, and its check set is the
// schema's, member for member.
func TestReportValidatesAgainstSchema(t *testing.T) {
	t.Run("a passing run", func(t *testing.T) {
		rep, raw := buildReport(t, FakeOptions{})
		if err := ValidateAgainst(SchemaConformanceReport, raw); err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != pv.ConformanceReportOutcomePassed {
			t.Fatalf("outcome = %s", rep.Outcome)
		}
		if rep.Provider.ModelID != "scripted" || rep.Declared.Image || rep.Declared.File || rep.Declared.Audio {
			t.Fatalf("provider/declared = %+v %+v", rep.Provider, rep.Declared)
		}
		if rep.Groups.Multimodal.ImageInputOutput.Result != pv.ConformanceReportConditionalCheckResultResultNotApplicable {
			t.Fatalf("an undeclared image check is %s", rep.Groups.Multimodal.ImageInputOutput.Result)
		}
		if rep.StartedAt != "2026-10-08T09:00:00Z" || rep.CompletedAt != "2026-10-08T09:01:00Z" {
			t.Fatalf("times = %s %s", rep.StartedAt, rep.CompletedAt)
		}
	})
	t.Run("declared media", func(t *testing.T) {
		rep, _ := buildReport(t, FakeOptions{Image: true, File: true, Audio: true})
		if !rep.Declared.Image || !rep.Declared.File || !rep.Declared.Audio {
			t.Fatalf("declared = %+v", rep.Declared)
		}
		if rep.Groups.Multimodal.AudioInputOutput.Result != pv.ConformanceReportConditionalCheckResultResultPassed {
			t.Fatalf("a declared audio check is %s", rep.Groups.Multimodal.AudioInputOutput.Result)
		}
	})
	t.Run("a failing run is a failed report with the reason code", func(t *testing.T) {
		b := BrokenProviders()[4] // usage_reporting
		sub := NewFake(b.Options).Subject()
		rows := Run(context.Background(), fakeFactory(b.Options))
		rep, raw, err := BuildReport(ReportInput{Rows: rows, Profile: sub.Profile, Mode: ModeFixtures, Started: time.Now(), Completed: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateAgainst(SchemaConformanceReport, raw); err != nil {
			t.Fatal(err)
		}
		r := rep.Groups.Inference.UsageReporting
		if rep.Outcome != pv.ConformanceReportOutcomeFailed || r.Result != pv.ConformanceReportCheckResultResultFailed || r.Reason == nil || string(*r.Reason) != "usage_missing_in_stream" {
			t.Fatalf("outcome %s, usage_reporting %+v", rep.Outcome, r)
		}
	})
	t.Run("an unknown mode is refused", func(t *testing.T) {
		if _, _, err := BuildReport(ReportInput{Mode: "recorded"}); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("the checks are the schema's members", func(t *testing.T) {
		raw, err := EmbeddedSchema("conformance-report.schema.json")
		if err != nil {
			t.Fatal(err)
		}
		var sch struct {
			Properties struct {
				Groups struct {
					Required   []string `json:"required"`
					Properties map[string]struct {
						Required []string `json:"required"`
					} `json:"properties"`
				} `json:"groups"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &sch); err != nil {
			t.Fatal(err)
		}
		want := map[string][]string{}
		for g, p := range sch.Properties.Groups.Properties {
			want[g] = p.Required
		}
		got := map[string][]string{}
		for _, c := range Checks() {
			got[c.Group] = append(got[c.Group], c.Name)
		}
		for g := range want {
			a, b := append([]string(nil), want[g]...), append([]string(nil), got[g]...)
			sort.Strings(a)
			sort.Strings(b)
			if len(a) != len(b) {
				t.Fatalf("group %s: schema has %v, suite has %v", g, a, b)
			}
			for i := range a {
				if a[i] != b[i] {
					t.Fatalf("group %s: schema has %v, suite has %v", g, a, b)
				}
			}
		}
		if len(got) != len(want) {
			t.Fatalf("groups: schema %d, suite %d", len(want), len(got))
		}
	})
}

// TestEmbeddedSchemasMatchPublished: the embedded copies are byte-identical to
// the published schemas.
func TestEmbeddedSchemasMatchPublished(t *testing.T) {
	for _, name := range EmbeddedSchemaNames() {
		copyBytes, err := EmbeddedSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		var orig []byte
		for _, area := range []string{"common", "providers"} {
			if b, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", area, name)); err == nil {
				orig = b
			}
		}
		if orig == nil {
			t.Fatalf("%s: no published schema of that name", name)
		}
		if !bytes.Equal(bytes.TrimSpace(orig), bytes.TrimSpace(copyBytes)) {
			t.Errorf("%s: the embedded copy differs from the published schema; copy it again", name)
		}
	}
}
