package dialogue

import (
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func TestMetaFromCapabilityFieldMapsClassTypeAndLocalOnly(t *testing.T) {
	conf := rag.SourceClassificationConfidential
	sens := true
	local := true
	cases := []struct {
		name string
		f    ap.CapabilityField
		want fieldMeta
	}{
		{"default internal string", ap.CapabilityField{Name: "n", Type: ap.CapabilityFieldTypeString}, fieldMeta{Type: an.FieldTypeString, Class: an.DataClassInternal}},
		{"public number", ap.CapabilityField{Name: "n", Type: ap.CapabilityFieldTypeNumber, Classification: ptrClass(rag.SourceClassificationPublic)}, fieldMeta{Type: an.FieldTypeNumber, Class: an.DataClassPublic}},
		{"sensitive elevates", ap.CapabilityField{Name: "n", Type: ap.CapabilityFieldTypeString, Sensitive: &sens}, fieldMeta{Type: an.FieldTypeString, Class: an.DataClassConfidential}},
		{"confidential declared", ap.CapabilityField{Name: "n", Type: ap.CapabilityFieldTypeInteger, Classification: &conf}, fieldMeta{Type: an.FieldTypeNumber, Class: an.DataClassConfidential}},
		{"local_only restricted", ap.CapabilityField{Name: "n", Type: ap.CapabilityFieldTypeString, LocalOnly: &local}, fieldMeta{Type: an.FieldTypeString, Class: an.DataClassRestricted, LocalOnly: true}},
		{"date", ap.CapabilityField{Name: "n", Type: ap.CapabilityFieldTypeDate}, fieldMeta{Type: an.FieldTypeDate, Class: an.DataClassInternal}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := metaFromCapabilityField(tc.f)
			if got != tc.want {
				t.Fatalf("got %+v want %+v (effective class %v local %v)", got, tc.want, apppkg.EffectiveFieldClassification(tc.f), apppkg.FieldLocalOnly(tc.f))
			}
		})
	}
}

func ptrClass(c rag.SourceClassification) *rag.SourceClassification { return &c }

func TestTypedCellNumbersAndDates(t *testing.T) {
	n := typedCell(act.Fact{Field: common.FieldName("x"), Value: act.CanonicalValue{Value: act.IntegerValue{Value: 42}}}, an.FieldTypeNumber)
	if n != float64(42) {
		t.Fatalf("integer cell = %v (%T)", n, n)
	}
	d := typedCell(act.Fact{Field: common.FieldName("d"), Value: act.CanonicalValue{Value: act.DateValue{Value: "2026-06-30"}}}, an.FieldTypeDate)
	if d != "2026-06-30" {
		t.Fatalf("date cell = %v", d)
	}
}

func TestVisibleFieldNamesProjection(t *testing.T) {
	if visibleFieldNames(nil) != nil {
		t.Fatal("nil names → no filter")
	}
	m := visibleFieldNames([]string{"a", "b"})
	if !m["a"] || m["c"] {
		t.Fatalf("%v", m)
	}
}
