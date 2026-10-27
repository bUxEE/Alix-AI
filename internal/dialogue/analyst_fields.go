package dialogue

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/internal/apppkg"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// fieldMeta is the package-declared type/class for one capability output field.
type fieldMeta struct {
	Type      an.FieldType
	Class     an.DataClass
	LocalOnly bool
}

func metaFromCapabilityField(f ap.CapabilityField) fieldMeta {
	m := fieldMeta{Type: mapCapabilityFieldType(f.Type), Class: an.DataClassInternal}
	switch apppkg.EffectiveFieldClassification(f) {
	case rag.SourceClassificationPublic:
		m.Class = an.DataClassPublic
	case rag.SourceClassificationConfidential:
		m.Class = an.DataClassConfidential
	case rag.SourceClassificationRestricted:
		m.Class = an.DataClassRestricted
	}
	if f.Sensitive != nil && *f.Sensitive && classRankData(m.Class) < classRankData(an.DataClassConfidential) {
		m.Class = an.DataClassConfidential
	}
	if apppkg.FieldLocalOnly(f) {
		m.Class = an.DataClassRestricted
		m.LocalOnly = true
	}
	return m
}

func classRankData(c an.DataClass) int {
	switch c {
	case an.DataClassPublic:
		return 0
	case an.DataClassInternal:
		return 1
	case an.DataClassConfidential:
		return 2
	}
	return 3
}

func mapCapabilityFieldType(t ap.CapabilityFieldType) an.FieldType {
	switch t {
	case ap.CapabilityFieldTypeInteger, ap.CapabilityFieldTypeNumber:
		return an.FieldTypeNumber
	case ap.CapabilityFieldTypeBoolean:
		return an.FieldTypeBoolean
	case ap.CapabilityFieldTypeDate:
		return an.FieldTypeDate
	case ap.CapabilityFieldTypeDatetime:
		return an.FieldTypeDatetime
	default:
		return an.FieldTypeString
	}
}

// typedCell converts a host fact into a dataset cell matching the field type.
func typedCell(f act.Fact, ft an.FieldType) an.DatasetCell {
	switch x := f.Value.Value.(type) {
	case act.IntegerValue:
		return float64(x.Value)
	case act.DecimalValue:
		n, err := strconv.ParseFloat(x.Value, 64)
		if err == nil {
			return n
		}
		return x.Value
	case act.BooleanValue:
		return x.Value
	case act.DateValue:
		return x.Value
	case act.DatetimeValue:
		return string(x.Value)
	case act.TimeValue:
		return x.Value
	case act.TextValue:
		if ft == an.FieldTypeNumber {
			if n, err := strconv.ParseFloat(strings.ReplaceAll(x.Value, ",", ""), 64); err == nil {
				return n
			}
		}
		return x.Value
	case act.IdentifierValue:
		return string(x.Value)
	}
	if v, ok := plainValue(f.Value); ok {
		switch n := v.(type) {
		case json.Number:
			if ft == an.FieldTypeNumber {
				if f64, err := n.Float64(); err == nil {
					return f64
				}
			}
			return n.String()
		default:
			return v
		}
	}
	return factText(f)
}

func fieldMetaMap(cap ap.Capability) map[string]fieldMeta {
	out := map[string]fieldMeta{}
	for _, f := range cap.Fields {
		out[string(f.Name)] = metaFromCapabilityField(f)
	}
	return out
}

func visibleFieldNames(names []string) map[string]bool {
	if len(names) == 0 {
		return nil // nil = no projection filter (all host-returned fields kept)
	}
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func datasetField(name string, meta fieldMeta) an.DatasetDatasetField {
	return an.DatasetDatasetField{
		Name: common.FieldName(name), Type: meta.Type, DataClass: meta.Class,
	}
}
